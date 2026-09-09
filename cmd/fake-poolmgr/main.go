// Command fake-poolmgr runs the fake Pool Manager
// (docs/requirements/10-test-doubles.md#fake-pool-manager) as a standalone
// binary, so that a fleet can run on it while battery has no release
// (TD-009). It is the same pool manager the tests run in process: it serves
// the poolmgr.v1alpha1 PoolAdmin, Lease and Events services over gRPC
// (TD-001) and creates, places and deletes MicroVMs through flintlock's
// MicroVM service on the Hosts of its inventory (TD-002), which here are
// real flintlockd endpoints rather than fake Hosts.
//
// Usage:
//
//	fake-poolmgr -listen :9440 -inventory /etc/flintlock-runner/inventory.yaml
//	fake-poolmgr -host host-a=10.0.0.1:9090 -host host-b=10.0.0.2:9090 -insecure
//
// The inventory file is the Fleet Controller's Inventory (FL-060): a `hosts`
// list of entries with a name, a flintlockd endpoint, optional TLS material
// and a token, of which fake-poolmgr reads the fields it needs and ignores
// the rest. -host is the shorthand for an endpoint with no TLS material of
// its own; it verifies TLS unless -insecure is given, because plaintext has
// to be explicit (SE-021).
//
// State is in memory: stopping the binary deletes every MicroVM it created
// and forgets every Pool. It is a test double, not a pool manager to run a
// production fleet on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
)

// Defaults for the flags. The listen address is battery's own default port
// so that a Runner configured for battery needs no change to talk to the
// fake, and the reconcile interval matches the fake's.
const (
	defaultListen            = ":9440"
	defaultReconcileInterval = time.Second
	defaultReadyTimeout      = 60 * time.Second
	defaultEventReplay       = 100
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fake-poolmgr: %v\n", err)
		os.Exit(1)
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL be runnable as a standalone binary as well as
//# in process, so that a fleet without battery can run on it.

// run is the body of the binary: it parses args, dials the Hosts of the
// inventory, serves the fake Pool Manager on the configured address and
// returns when ctx is cancelled, which main wires to SIGINT and SIGTERM. The
// bound address is written to out as soon as the listener is up, which is
// what a supervisor or a test waits for when the port is :0. Shutdown is
// clean: every MicroVM the fake created is deleted from its Host and every
// connection is closed before it returns.
func run(ctx context.Context, args []string, out io.Writer) error {
	opts, err := parseFlags(args, out)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: opts.logLevel})))

	hosts, err := fake.HostsFromDialer(ctx, fake.AdminDialer{}, opts.endpoints)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	defer func() {
		if err := hosts.Close(); err != nil {
			slog.Warn("closing host connections", "error", err)
		}
	}()

	cfg := opts.cfg
	cfg.Hosts = hosts
	pm := fake.New(cfg)

	served := make(chan error, 1)
	go func() { served <- pm.Serve(ctx) }()
	select {
	case err := <-served:
		if err == nil {
			err = errors.New("stopped before it was listening")
		}
		return err
	case <-pm.Ready():
	}
	if _, err := fmt.Fprintf(out, "fake-poolmgr listening on %s\n", pm.Addr()); err != nil {
		slog.Warn("reporting the listen address", "error", err)
	}
	slog.Info("serving", "address", pm.Addr(), "hosts", hosts.Names(), "namespace", cfg.Namespace)
	return <-served
}

// options is the parsed command line.
type options struct {
	cfg       poolmgr.FakeConfig
	endpoints []flintlock.Endpoint
	logLevel  slog.Level
}

// parseFlags parses args into options. Usage and flag errors are written to
// out; flag.ErrHelp is returned for -h, which is not a failure.
func parseFlags(args []string, out io.Writer) (*options, error) {
	fs := flag.NewFlagSet("fake-poolmgr", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		listen     = fs.String("listen", defaultListen, "gRPC listen address; :0 picks a free port")
		inventory  = fs.String("inventory", "", "path of a YAML inventory of flintlockd hosts")
		insecure   = fs.Bool("insecure", false, "dial the -host endpoints in plaintext instead of TLS")
		namespace  = fs.String("namespace", "", "serve only Pools in this namespace; empty serves every namespace")
		placement  = fs.String("placement", string(poolmgr.PlacementLeastVMs), "placement strategy: least_vms or round_robin")
		reconcile  = fs.Duration("reconcile-interval", defaultReconcileInterval, "how often pools are topped up and expired leases swept")
		ready      = fs.Duration("ready-timeout", defaultReadyTimeout, "how long a microvm has to reach CREATED and run its create hooks")
		replay     = fs.Int("event-replay", defaultEventReplay, "how many recent events per pool a new subscriber is replayed")
		omitHost = fs.Bool("omit-host-on-claim", false, "leave the host field of ClaimVMResponse unset, as a pool manager that predates it does")
		logLevel = fs.String("log-level", "info", "log level: debug, info, warn or error")
	)
	var hostFlags hostList
	fs.Var(&hostFlags, "host", "a flintlockd host as name=address; repeat for more")
	const usageExtra = "\nThe inventory file is a YAML document with a `hosts` list of entries\n" +
		"carrying a name, an endpoint, and optionally tls and token or token_file.\n"
	fs.Usage = func() {
		fmt.Fprintf(out, "usage: fake-poolmgr [flags]\n\n")
		fs.PrintDefaults()
		fmt.Fprint(out, usageExtra)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	opts := &options{}
	level, err := parseLevel(*logLevel)
	if err != nil {
		return nil, err
	}
	opts.logLevel = level

	strategy := poolmgr.PlacementStrategy(*placement)
	switch strategy {
	case poolmgr.PlacementLeastVMs, poolmgr.PlacementRoundRobin:
	default:
		return nil, fmt.Errorf("-placement %q: want %q or %q", *placement, poolmgr.PlacementLeastVMs, poolmgr.PlacementRoundRobin)
	}
	if *reconcile <= 0 {
		return nil, fmt.Errorf("-reconcile-interval %s: want a positive duration", *reconcile)
	}
	if *ready <= 0 {
		return nil, fmt.Errorf("-ready-timeout %s: want a positive duration", *ready)
	}
	if *replay <= 0 {
		return nil, fmt.Errorf("-event-replay %d: want a positive count", *replay)
	}
	opts.cfg = poolmgr.FakeConfig{
		Listen:            *listen,
		Placement:         strategy,
		ReconcileInterval: *reconcile,
		ReadyTimeout:      *ready,
		EventReplay:       *replay,
		OmitHostOnClaim:   *omitHost,
		Namespace:         *namespace,
	}

	if *inventory != "" {
		endpoints, err := loadInventory(*inventory)
		if err != nil {
			return nil, err
		}
		opts.endpoints = endpoints
	}
	for _, h := range hostFlags {
		h.TLS.Insecure = *insecure
		opts.endpoints = append(opts.endpoints, h)
	}
	if err := checkUnique(opts.endpoints); err != nil {
		return nil, err
	}
	return opts, nil
}

// parseLevel maps a -log-level value onto a slog.Level.
func parseLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("-log-level %q: want debug, info, warn or error", s)
	}
	return level, nil
}

// checkUnique rejects duplicate Host names and endpoints, which the Runner's
// Inventory also forbids (CF-031) and which would make placement ambiguous.
func checkUnique(endpoints []flintlock.Endpoint) error {
	names := make(map[string]bool, len(endpoints))
	addresses := make(map[string]bool, len(endpoints))
	for _, ep := range endpoints {
		if names[ep.Name] {
			return fmt.Errorf("host %q is declared twice", ep.Name)
		}
		if addresses[ep.Address] {
			return fmt.Errorf("address %q is declared twice", ep.Address)
		}
		names[ep.Name] = true
		addresses[ep.Address] = true
	}
	return nil
}

// hostList collects repeated -host name=address flags.
type hostList []flintlock.Endpoint

// String implements flag.Value.
func (l *hostList) String() string {
	names := make([]string, 0, len(*l))
	for _, ep := range *l {
		names = append(names, ep.Name+"="+ep.Address)
	}
	return strings.Join(names, ",")
}

// Set implements flag.Value.
func (l *hostList) Set(v string) error {
	name, address, ok := strings.Cut(v, "=")
	if !ok || name == "" || address == "" {
		return fmt.Errorf("host %q: want name=address", v)
	}
	*l = append(*l, flintlock.Endpoint{Name: name, Address: address})
	return nil
}
