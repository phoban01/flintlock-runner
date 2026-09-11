package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urfave/cli"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/network"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/config/runnercfg"
	"github.com/phoban01/flintlock-runner/internal/executor"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/inventory"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// runnerConfigFile is the name of the gitlab-runner configuration the `run`
// subcommand writes into the state directory and hands to the run loop.
// gitlab-runner's run command reads its configuration from a file and
// nothing else, so the translation of our configuration (runnercfg.Build)
// is written there, owner-only because it carries the runner token.
const runnerConfigFile = "gitlab-runner.toml"

// systemIDFile is where the persistent system identifier lives, next to the
// run loop's configuration, which is the file gitlab-runner's own
// configuration loader reads it from, so both agree on it (GL-012, GL-013).
const systemIDFile = ".runner_system_id"

// exitVerifyFailed is the exit status when GitLab rejects the runner token
// at startup (GL-015).
const exitVerifyFailed = 4

// runRunner is the action of `run`: it loads and validates the
// configuration, builds every dependency of the Executor, verifies the
// runner token with GitLab, and hands off to gitlab-runner's run loop with
// the flintlock executor in its provider registry. It returns when the run
// loop has shut down, which it does on SIGTERM, SIGINT or SIGQUIT after the
// running Jobs have finished or been cancelled and every Lease is released.
func runRunner(c *cli.Context) error {
	path := c.GlobalString("config")
	cfg, err := loadConfig(c)
	if err != nil {
		return err
	}
	log := newLogger(c.App.ErrWriter, cfg.Observability)
	slog.SetDefault(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return cli.NewExitError(fmt.Sprintf("creating state directory: %v", err), 1)
	}
	systemID, err := loadSystemID(cfg.StateDir)
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	r, err := newRunner(ctx, cfg, log, func(s executor.Stopper) { takeOverStopSignals(ctx, s, log) })
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}
	defer r.close()

	//= docs/requirements/01-gitlab-protocol.md#authentication
	//# The Runner SHALL authenticate to GitLab with a runner
	//# authentication token (a token beginning with `glrt-`) supplied by
	//# configuration.
	//
	// The token is gitlab.token (or its environment override), carried into
	// the RunnerCredentials the network client authenticates every request
	// with; nothing registers a runner to obtain one.
	rcfg, err := runnercfg.Build(cfg, systemID)
	if err != nil {
		return cli.NewExitError(err.Error(), exitInvalidConfig)
	}

	//= docs/requirements/01-gitlab-protocol.md#authentication
	//# The Runner SHALL send its system identifier as `system_id` on
	//# every runner-scoped request to GitLab.
	//
	// The run loop's configuration goes into the state directory, next to
	// the identifier file, which is where gitlab-runner's loader reads the
	// identifier from; it then sets it on the RunnerConfig that every
	// runner-scoped request of the network client carries.
	runnerPath := filepath.Join(cfg.StateDir, runnerConfigFile)
	if err := rcfg.SaveConfig(runnerPath); err != nil {
		return cli.NewExitError(fmt.Sprintf("writing %s: %v", runnerPath, err), 1)
	}

	//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
	//# The Runner SHALL advertise the executor name `flintlock` in the
	//# `info.executor` field of every job request.
	//
	// The one RunnerConfig names the flintlock executor (runnercfg), the
	// registry holds the provider under that name, and the network client
	// reads the executor name and its features from that RunnerConfig and
	// registry for the info of every request.
	providers := map[string]common.ExecutorProvider{}
	executor.Register(providers, r.provider)
	cmd, client := newRunLoopCommand(providers)

	if err := verifyRunner(ctx, client, rcfg.Runners[0], systemID, log); err != nil {
		return err
	}

	// SIGHUP reloads the Profiles and the Inventory (CF-007). The run loop
	// also sees SIGHUP and re-reads its own file, which has not changed.
	reloader := config.NewReloader(cfg, path, config.WithLogger(log))
	reloader.Subscribe(func(ctx context.Context, next *config.Config) error {
		r.inventory.set(next.Inventory.Hosts)
		return r.sched.Reload(ctx, next.Profiles, next.Inventory.Hosts)
	})
	go func() { _ = reloader.ServeSIGHUP(ctx) }()

	app := cli.NewApp()
	app.Name = c.App.Name
	app.Writer, app.ErrWriter = c.App.Writer, c.App.ErrWriter
	app.Commands = []cli.Command{cmd}
	return app.Run([]string{c.App.Name, "run", "--config", runnerPath})
}

// runner holds what `run` builds and has to close on the way out.
type runner struct {
	client    poolmgr.Client
	hosts     flintlock.Registry
	sched     scheduler.Scheduler
	provider  executor.Provider
	inventory *inventoryView
}

// newRunner builds the Pool Manager client and the policy units the
// Scheduler runs on, the Host Registry over the Inventory, the Guest
// Transport factory, the Scheduler and the flintlock executor provider. The
// provider owns the Scheduler's lifetime: the run loop's Init starts it and
// its Shutdown stops it once every worker has stopped.
func newRunner(ctx context.Context, cfg *config.Config, log *slog.Logger, onInit func(executor.Stopper)) (*runner, error) {
	pm := cfg.PoolManager
	client, err := poolmgr.NewClient(poolmgr.ClientConfig{
		Endpoint: pm.Endpoint,
		TLS:      pm.TLS,
		Deadline: pm.Deadline,
	})
	if err != nil {
		return nil, fmt.Errorf("pool manager client: %w", err)
	}
	health, err := poolmgr.NewHealth(poolmgr.HealthConfig{
		Admin:            client,
		Namespace:        cfg.Scheduler.Namespace,
		Interval:         pm.HealthInterval,
		FailureThreshold: pm.HealthFailureThreshold,
		UnhealthyFor:     pm.HealthBackoff,
		Log:              log,
	})
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	tracker, err := poolmgr.NewTracker(poolmgr.TrackerConfig{
		Events:       client,
		Admin:        client,
		Health:       health,
		PollInterval: pm.EventsPollInterval,
		Log:          log,
	})
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	dialer := flintlock.NewDialer(flintlock.WithCallDeadline(cfg.Scheduler.HostCallDeadline))
	hosts, err := flintlock.NewRegistry(ctx, dialer, inventory.Endpoints(cfg.Inventory.Hosts),
		flintlock.WithRegistryLogger(log))
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("host registry: %w", err)
	}

	//= docs/requirements/01-gitlab-protocol.md#job-acquisition
	//# The Runner SHALL run no more Jobs concurrently than the
	//# configured concurrency limit.
	//
	// The limit is enforced twice: the run loop's concurrent and limit
	// (runnercfg) and the Scheduler's Slots below, so a Job is not even
	// requested once every Slot is taken (SC-001, SC-004).
	sched, err := scheduler.New(scheduler.Deps{
		PoolManager:  client,
		Specs:        poolmgr.NewSpecBuilder(),
		HostSelector: poolmgr.NewHostSelector(),
		Declarer:     poolmgr.NewDeclarer(client),
		Tracker:      tracker,
		Health:       health,
		Hosts:        hosts,
		Logger:       log,
	}, scheduler.Settings{
		RunnerName:      cfg.GitLab.Name,
		Namespace:       cfg.Scheduler.Namespace,
		Slots:           cfg.GitLab.Concurrent,
		ShutdownTimeout: cfg.GitLab.ShutdownTimeout,
		Scheduler:       cfg.Scheduler,
		PoolManager:     cfg.PoolManager,
		Profiles:        cfg.Profiles,
		Inventory:       cfg.Inventory.Hosts,
	})
	if err != nil {
		_ = hosts.Close()
		_ = client.Close()
		return nil, err
	}

	inv := newInventoryView(cfg.Inventory.Hosts)
	var provider executor.Provider
	initHook := func() {
		if s, ok := provider.(executor.Stopper); ok && onInit != nil {
			onInit(s)
		}
	}
	provider, err = executor.NewProvider(executor.Deps{
		Scheduler:  sched,
		Transports: transport.NewFactory(transport.WithLogger(log)),
		Env:        executor.NewHostServiceEnv(cfg.HostServices),
		Inventory:  inv,
		Timeouts: executor.Timeouts{
			Prepare:      cfg.Executor.PrepareTimeout,
			GracefulKill: cfg.Executor.GracefulKillTimeout,
			Transport:    cfg.Executor.TransportDeadline,
		},
		KeepOnFailure:   cfg.Scheduler.KeepOnFailure,
		CacheConfigured: cfg.DistributedCache != nil,
		Hosts:           hosts,
	},
		executor.WithLifecycle(sched),
		executor.WithLogger(log),
		executor.WithHTTPCacheUpstreams(cfg.HostServices.HTTPCache.Upstreams),
		executor.WithInitHook(initHook),
	)
	if err != nil {
		_ = hosts.Close()
		_ = client.Close()
		return nil, err
	}
	return &runner{client: client, hosts: hosts, sched: sched, provider: provider, inventory: inv}, nil
}

// close releases the connections. The Scheduler has stopped by then: the
// run loop's shutdown stopped it through the provider.
func (r *runner) close() {
	_ = r.hosts.Close()
	_ = r.client.Close()
}

// inventoryView is the Executor's view of the Inventory (InventoryLookup),
// swapped whole on reload.
type inventoryView struct {
	hosts atomic.Pointer[[]config.HostEntry]
}

func newInventoryView(hosts []config.HostEntry) *inventoryView {
	v := &inventoryView{}
	v.set(hosts)
	return v
}

// set replaces the Inventory.
func (v *inventoryView) set(hosts []config.HostEntry) {
	cp := append([]config.HostEntry(nil), hosts...)
	v.hosts.Store(&cp)
}

// Host implements executor.InventoryLookup.
func (v *inventoryView) Host(name string) (*config.HostEntry, bool) {
	return config.HostByName(*v.hosts.Load(), name)
}

//= docs/requirements/01-gitlab-protocol.md#shutdown
//# While a graceful shutdown is in progress, the Runner SHALL allow
//# running Jobs to finish for up to the configured shutdown timeout.

//= docs/requirements/01-gitlab-protocol.md#shutdown
//# If running Jobs have not finished when the shutdown timeout
//# elapses, then the Runner SHALL cancel them, report them as failed with the
//# reason `runner_system_failure` and release their MicroVMs.

// takeOverStopSignals makes SIGTERM and SIGINT stop the Runner gracefully.
//
// gitlab-runner's run loop, at the pinned commit, treats SIGQUIT as a
// graceful stop that waits for running builds without a limit, and SIGTERM
// and SIGINT as a forceful one that aborts them at once as
// runner_interrupted. GL-070 to GL-073 want every termination signal to be
// graceful, bounded by the shutdown timeout, and a second one to cancel.
// The run loop subscribes to all three signals before it calls the
// provider's Init, which is where this runs: signal.Ignore takes SIGTERM
// and SIGINT away from every subscriber, the run loop's included, and this
// function subscribes to them again alone. On the first signal of any of
// the three it starts the Scheduler's shutdown (no new Reservations,
// unconverted ones released; running Jobs keep their Leases for the
// shutdown timeout and are then aborted as runner_system_failure with their
// Leases released) and, unless the signal was SIGQUIT already, sends the
// process a SIGQUIT so that the run loop stops requesting Jobs and waits for
// the running ones. Every later signal cancels all running Jobs at once.
func takeOverStopSignals(ctx context.Context, s executor.Stopper, log *slog.Logger) {
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		defer signal.Stop(ch)
		relayed, received := 0, 0
		for {
			var sig os.Signal
			select {
			case <-ctx.Done():
				return
			case sig = <-ch:
			}
			if sig == syscall.SIGQUIT && relayed > 0 {
				relayed--
				continue
			}
			received++
			if received > 1 {
				log.Warn("second stop signal: cancelling every running job", "signal", sig.String())
				s.AbortAll()
				continue
			}
			log.Warn("stop signal: requesting no more jobs; running jobs have the shutdown timeout to finish", "signal", sig.String())
			s.BeginShutdown()
			if sig != syscall.SIGQUIT {
				relayed++
				if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
					log.Error("could not start the run loop's graceful shutdown", "error", err)
				}
			}
		}
	}()
}

//= docs/requirements/01-gitlab-protocol.md#authentication
//# When starting, the Runner SHALL load a persistent system
//# identifier from its state directory or generate and store one if none
//# exists.

// loadSystemID reads the system identifier from the state directory, or
// generates one in gitlab-runner's format and stores it owner-only. The file
// is the one gitlab-runner's configuration loader reads from the directory
// of the configuration file `run` writes, so the run loop reports the same
// identifier.
func loadSystemID(stateDir string) (string, error) {
	path := filepath.Join(stateDir, systemIDFile)
	data, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(data)); systemIDPattern.MatchString(id) {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading system id: %w", err)
	}
	id, err := newSystemID()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("storing system id: %w", err)
	}
	return id, nil
}

// systemIDPattern is gitlab-runner's system identifier format.
var systemIDPattern = regexp.MustCompile(`^[sr]_[0-9a-zA-Z]{12}$`)

// newSystemID generates a random identifier in gitlab-runner's format.
func newSystemID() (string, error) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, 12)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", fmt.Errorf("generating system id: %w", err)
		}
		b[i] = alphabet[n.Int64()]
	}
	return "s_" + string(b), nil
}

//= docs/requirements/01-gitlab-protocol.md#authentication
//# When starting, the Runner SHALL call `POST /api/v4/runners/verify`
//# with the full `info` payload before it requests any Job.

//= docs/requirements/01-gitlab-protocol.md#authentication
//# If token verification returns a forbidden response, then the
//# Runner SHALL log the failure and exit with a non-zero status.

//= docs/requirements/01-gitlab-protocol.md#authentication
//# The Runner SHALL NOT call the runner registration endpoint
//# `POST /api/v4/runners`.

//= docs/requirements/01-gitlab-protocol.md#authentication
//= type=todo
//= tracking-issue=23
//# Where GitLab reports a token expiry time, the Runner SHALL rotate
//# the token through `POST /api/v4/runners/reset_authentication_token` before
//# that time is reached.

// verifyRunner calls runners/verify through the network client, whose
// request carries the full info payload with the flintlock executor's
// features, before the run loop requests any Job. It verifies; it never
// registers, because the token comes from the configuration (GL-010). A
// forbidden answer ends
// the process with a non-zero status; a GitLab that cannot be reached is
// retried a few times first, because the Runner commonly starts alongside
// it.
func verifyRunner(ctx context.Context, client common.Network, rc *common.RunnerConfig, systemID string, log *slog.Logger) error {
	const attempts = 10
	var lastErr error
	for i := range attempts {
		resp, err := client.VerifyRunner(ctx, *rc, systemID)
		switch {
		case err == nil && resp != nil:
			return nil
		case err == nil:
			log.Error("gitlab rejected the runner token", "url", rc.URL)
			return cli.NewExitError("flintlock-runner: GitLab rejected the runner authentication token (403 Forbidden)", exitVerifyFailed)
		}
		lastErr = err
		log.Warn("could not verify the runner with gitlab; retrying", "attempt", i+1, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * time.Second):
		}
	}
	return cli.NewExitError(fmt.Sprintf("flintlock-runner: verifying the runner with GitLab: %v", lastErr), exitVerifyFailed)
}

//= docs/requirements/08-observability.md#logging
//= type=todo
//= tracking-issue=7
//# The Runner SHALL write structured logs in JSON or text format as
//# configured, using the standard library `slog` package.

// newLogger builds the process logger from the observability section.
func newLogger(w io.Writer, o config.Observability) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(o.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if o.LogFormat == config.LogText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// The network client satisfies the interface verifyRunner takes.
var _ common.Network = (*network.GitLabClient)(nil)
