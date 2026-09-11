package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"gitlab.com/gitlab-org/gitlab-runner/executors"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
)

// RunnerUnsupported is the failure reason for a Job the Executor cannot run
// at all: an image no Profile matches (EX-011), services (GL-024) or
// `cache:` without a Distributed cache (CF-083). gitlab-runner has no
// constant for it; GitLab maps it to "the runner does not support this job".
const RunnerUnsupported spec.JobFailureReason = "runner_unsupported"

// Option configures the Provider beyond its Deps.
type Option func(*provider)

// WithLifecycle makes the Provider's Init start lc.Run and its Shutdown
// cancel it and wait for it to return, so that the run loop's own start and
// stop hooks own the Scheduler (GL-070 to GL-073). Without it the caller
// runs the Scheduler itself.
func WithLifecycle(lc scheduler.Lifecycle) Option {
	return func(p *provider) { p.lifecycle = lc }
}

// WithClock sets the clock the readiness wait and the prepare report are
// timed against (EX-014, EX-019). The default is the wall clock.
func WithClock(clk clock.Clock) Option {
	return func(p *provider) {
		if clk != nil {
			p.clk = clk
		}
	}
}

// WithLogger sets the process logger. The default is slog.Default.
func WithLogger(log *slog.Logger) Option {
	return func(p *provider) {
		if log != nil {
			p.log = log
		}
	}
}

// WithHTTPCacheUpstreams names the configured HTTP cache upstreams, whose
// env_var names join the closed list of variables a Host Service resolver
// may add to a Job (EX-060, EX-066).
func WithHTTPCacheUpstreams(upstreams []config.HTTPCacheUpstream) Option {
	return func(p *provider) {
		for _, u := range upstreams {
			if u.EnvVar != "" {
				p.httpCacheVars[u.EnvVar] = true
			}
		}
	}
}

// provider is the flintlock ExecutorProvider (EX-001, EX-002).
type provider struct {
	deps      Deps
	clk       clock.Clock
	log       *slog.Logger
	lifecycle scheduler.Lifecycle
	// httpCacheVars are the configured HTTP cache variable names.
	httpCacheVars map[string]bool

	mu        sync.Mutex
	runCancel context.CancelFunc
	runDone   chan struct{}
	initHook  func()

	// abort is closed by AbortAll (GL-073).
	abort     chan struct{}
	abortOnce sync.Once
}

//= docs/requirements/02-executor.md#interface
//# The Executor SHALL implement the `ExecutorProvider` and
//# `Executor` interfaces of the gitlab-runner `common` package.

// Compile-time checks: the provider is a common.ExecutorProvider and a
// common.ManagedExecutorProvider, and the executor it creates is a
// common.Executor.
var (
	_ Provider = (*provider)(nil)
	_ Executor = (*executor)(nil)
)

// NewProvider builds the flintlock ExecutorProvider. Scheduler, Transports
// and Hosts are required; Env and Inventory may be nil, in which case no
// Host Service variables are added.
func NewProvider(deps Deps, opts ...Option) (Provider, error) {
	switch {
	case deps.Scheduler == nil:
		return nil, errors.New("executor: deps: Scheduler is required")
	case deps.Transports == nil:
		return nil, errors.New("executor: deps: Transports is required")
	case deps.Hosts == nil:
		return nil, errors.New("executor: deps: Hosts is required")
	}
	p := &provider{
		deps: deps, clk: clock.Real{}, log: slog.Default(),
		httpCacheVars: map[string]bool{}, abort: make(chan struct{}),
	}
	for _, o := range opts {
		o(p)
	}
	p.log = p.log.With("component", "executor")
	return p, nil
}

//= docs/requirements/02-executor.md#interface
//# The Executor SHALL be registered in the provider registry under
//# the executor name `flintlock`.

// Register adds the provider to a provider registry map under Name, which is
// what the run loop looks the RunnerConfig's executor up by.
func Register(providers map[string]common.ExecutorProvider, p Provider) {
	providers[Name] = p
}

// CanCreate implements common.ExecutorProvider.
func (p *provider) CanCreate() bool { return true }

//= docs/requirements/01-gitlab-protocol.md#job-execution
//# The Runner SHALL generate each Stage script with the `bash`
//# shell implementation of the gitlab-runner `shells` package.

// Create implements common.ExecutorProvider: one executor per Job, holding
// nothing until Prepare. Its shell is gitlab-runner's bash implementation,
// registered by the shells package the binary imports, which the Build
// generates every Stage script with.
func (p *provider) Create() common.Executor {
	return &executor{
		AbstractExecutor: executors.AbstractExecutor{
			ExecutorOptions: executors.ExecutorOptions{
				DefaultCustomBuildsDirEnabled: false,
				DefaultSafeDirectoryCheckout:  false,
				// A MicroVM runs exactly one Job, so builds are not
				// shared between concurrent Jobs in one directory tree.
				SharedBuildsDir: false,
				Shell: common.ShellScriptInfo{
					Shell: DefaultShell,
					// A normal shell: the guest's login profile is not part
					// of the contract between a Profile and a Job.
					Type: common.NormalShell,
				},
			},
		},
		p: p,
	}
}

//= docs/requirements/02-executor.md#interface
//# When the run loop calls `Acquire`, the Executor SHALL request a
//# Reservation from the Scheduler and return it as the executor data.

//= docs/requirements/02-executor.md#interface
//# If the Scheduler refuses a Reservation, then the Executor SHALL
//# return an error that the run loop treats as no free executor, so that no
//# Job is requested from GitLab.

//= docs/requirements/01-gitlab-protocol.md#job-acquisition
//# The Runner SHALL request a Job from GitLab only after the
//# Scheduler has granted a Reservation for it.

// Acquire implements common.ExecutorProvider. The run loop calls it before
// it asks GitLab for a Job; a Reservation comes back as *Data, and anything
// else, a refusal above all, comes back as a NoFreeExecutorError, which the
// loop treats as "no capacity" and so makes no job request.
func (p *provider) Acquire(*common.RunnerConfig) (common.ExecutorData, error) {
	r, err := p.deps.Scheduler.Reserve(context.Background())
	if err != nil {
		return nil, &common.NoFreeExecutorError{Message: fmt.Sprintf("flintlock: %v", err)}
	}
	return &Data{Reservation: r}, nil
}

//= docs/requirements/02-executor.md#interface
//# When the run loop calls `Release`, the Executor SHALL release the
//# Reservation held in the executor data.

//= docs/requirements/01-gitlab-protocol.md#job-acquisition
//# When GitLab returns no job, the Runner SHALL release the
//# Reservation before the next request is made.

// Release implements common.ExecutorProvider. The run loop calls it when it
// is done with the executor data: straight after an empty job request, or
// after the Build. Releasing a Reservation that Prepare converted into an
// Allocation is a no-op in the Scheduler, so this is always safe.
func (p *provider) Release(_ *common.RunnerConfig, data common.ExecutorData) {
	d, ok := data.(*Data)
	if !ok || d == nil || d.Reservation == nil {
		return
	}
	p.deps.Scheduler.ReleaseReservation(d.Reservation)
}

//= docs/requirements/02-executor.md#interface
//# The Executor SHALL report the shell name `bash` and the feature
//# set listed in the GitLab protocol document from `GetFeatures` and
//# `GetDefaultShell`.

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//# The Runner SHALL advertise the features `variables`, `image`,
//# `refspecs`, `masking`, `raw_variables`, `artifacts`, `artifacts_exclude`,
//# `upload_multiple_artifacts`, `upload_raw_artifacts`, `cache`,
//# `fallback_cache_keys`, `multi_build_steps`, `return_exit_code`,
//# `trace_reset`, `trace_checksum`, `trace_size`, `cancelable` and
//# `cancel_gracefully`.

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//# The Runner SHALL NOT advertise the `services`, `session`,
//# `terminal`, `proxy` or `shared` features.

// GetFeatures implements common.ExecutorProvider. It sets every feature of
// GL-022 itself rather than relying on the bash shell and the network client
// to add theirs, so that the advertised set does not change if either
// library stops setting one; and it clears the five GL-023 forbids, which
// neither of them sets.
func (p *provider) GetFeatures(f *common.FeaturesInfo) error {
	f.Variables = true
	f.Image = true
	f.Refspecs = true
	f.Masking = true
	f.RawVariables = true
	f.Artifacts = true
	f.ArtifactsExclude = true
	f.UploadMultipleArtifacts = true
	f.UploadRawArtifacts = true
	f.Cache = true
	f.FallbackCacheKeys = true
	f.MultiBuildSteps = true
	f.ReturnExitCode = true
	f.TraceReset = true
	f.TraceChecksum = true
	f.TraceSize = true
	f.Cancelable = true
	f.CancelGracefully = true

	f.Services = false
	f.Session = false
	f.Terminal = false
	f.Proxy = false
	f.Shared = false
	return nil
}

// GetConfigInfo implements common.ExecutorProvider; there is nothing to
// report.
func (p *provider) GetConfigInfo(*common.RunnerConfig, *common.ConfigInfo) {}

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//# The Runner SHALL advertise `bash` as its default shell.

// GetDefaultShell implements common.ExecutorProvider.
func (p *provider) GetDefaultShell() string { return DefaultShell }

// Init implements common.ManagedExecutorProvider. It starts the Scheduler
// when one was given with WithLifecycle, then runs the WithInitHook hook;
// it does not block. The run loop calls Init after it has subscribed to the
// process's stop signals and before it starts its workers.
func (p *provider) Init() {
	p.mu.Lock()
	if p.lifecycle != nil && p.runCancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		p.runCancel, p.runDone = cancel, done
		go func() {
			defer close(done)
			if err := p.lifecycle.Run(ctx); err != nil {
				p.log.Error("scheduler stopped with an error", "error", err)
			}
		}()
	}
	hook := p.initHook
	p.initHook = nil
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
}

// Shutdown implements common.ManagedExecutorProvider. The run loop calls it
// once every worker has stopped, so every Job has been cleaned up and its
// Lease handed back; cancelling the Scheduler's Run releases any Reservation
// still held and waits for the background releases to finish. It blocks
// until Run has returned or ctx is done.
func (p *provider) Shutdown(ctx context.Context, _ *common.Config) {
	p.BeginShutdown()
	p.mu.Lock()
	done := p.runDone
	p.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
		p.log.Warn("scheduler did not stop within the shutdown timeout")
	}
}

// Stopper is the shutdown control of the provider NewProvider returns. `run`
// drives it from the process's stop signals, because the run loop's own
// handling of SIGTERM does not match GL-070 to GL-073 (see
// cmd/flintlock-runner).
type Stopper interface {
	// BeginShutdown cancels the Scheduler's Run: every unconverted
	// Reservation is released and no more are granted (GL-070); running
	// Jobs keep their Leases for the shutdown timeout (GL-071), after which
	// the Scheduler aborts them as runner_system_failure and releases their
	// Leases (GL-072). It returns at once and is safe to call more than once.
	BeginShutdown()
	// AbortAll cancels every running Job at once as runner_system_failure,
	// for a second termination signal (GL-073). Their MicroVMs are released
	// by Cleanup as for any other Job.
	AbortAll()
}

var _ Stopper = (*provider)(nil)

// WithInitHook runs hook at the end of Init. `run` installs its stop signal
// handling there, because that is the first point at which the run loop has
// subscribed to the signals it has to take over.
func WithInitHook(hook func()) Option {
	return func(p *provider) { p.initHook = hook }
}

// ErrAborted is the cause of a Job cancelled by AbortAll.
var ErrAborted = errors.New("executor: runner is shutting down; job aborted")

//= docs/requirements/01-gitlab-protocol.md#shutdown
//# When the Runner receives `SIGTERM` or `SIGQUIT`, the Runner SHALL
//# stop requesting Jobs and release every unconverted Reservation.

// BeginShutdown implements Stopper.
func (p *provider) BeginShutdown() {
	p.mu.Lock()
	cancel := p.runCancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

//= docs/requirements/01-gitlab-protocol.md#shutdown
//# When the Runner receives a second termination signal during a
//# graceful shutdown, the Runner SHALL cancel all running Jobs immediately.

// AbortAll implements Stopper. Every Stage running now or started later
// sees the abort and stops as runner_system_failure, and so does a Prepare
// still waiting for a MicroVM.
func (p *provider) AbortAll() {
	p.abortOnce.Do(func() { close(p.abort) })
}
