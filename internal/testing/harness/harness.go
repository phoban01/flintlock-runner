package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	pmfake "github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// Environment variables the harness reads through OptionsFromEnv and
// nowhere else.
const (
	// EnvHardwareInventory names an Inventory file (the `hosts:` list the
	// Fleet Controller writes) of real flintlockd Hosts. When it is set the
	// scenarios run against those Hosts instead of fake Hosts (TD-052).
	EnvHardwareInventory = "FLINTLOCK_RUNNER_E2E_INVENTORY"
	// EnvPoolManager names a real Pool Manager endpoint, host:port. When it
	// is set the harness uses it instead of the fake Pool Manager (TD-053).
	// The harness dials it in plaintext.
	EnvPoolManager = "FLINTLOCK_RUNNER_E2E_POOL_MANAGER"
	// EnvKernelImage and EnvRootFSImage are the Profile images used on the
	// hardware tier, where they have to be real OCI images with a guest
	// agent (EX-052). The fake Hosts never read them.
	EnvKernelImage = "FLINTLOCK_RUNNER_E2E_KERNEL_IMAGE"
	EnvRootFSImage = "FLINTLOCK_RUNNER_E2E_ROOTFS_IMAGE"
	// EnvRunnerBinary names a prebuilt flintlock-runner binary, so that a
	// test run does not build one. `make e2e` sets it.
	EnvRunnerBinary = "FLINTLOCK_RUNNER_E2E_BINARY"
)

// Fixed names the harness gives the Runner and its Pool.
const (
	// Namespace is the Runner namespace (CF-050) and the only namespace the
	// fake Pool Manager serves, which proves the Runner declares its Pools
	// there (PL-017).
	Namespace = "harness"
	// ProfileName is the name of the single, Default Profile and so of its
	// Pool (CF-028).
	ProfileName = "default"
	// RunnerName is the runner name reported to GitLab.
	RunnerName = "flintlock-harness"
	// RunnerToken is the runner authentication token the fake GitLab
	// requires (GL-010, TD-033).
	RunnerToken = "glrt-flintlock-harness"
	// HostToken is the basic auth token every fake Host requires (TD-024).
	HostToken = "harness-host-token"
)

// Defaults for zero Options fields.
const (
	DefaultHosts     = 2
	DefaultPoolSize  = 1
	DefaultBootDelay = 200 * time.Millisecond
	// fakeHostVCPU and fakeHostMemoryMB are the capacity every fake Host
	// declares in the Inventory.
	fakeHostVCPU     = 8
	fakeHostMemoryMB = 16384
)

// Options configure a Stack. The zero value is a working stack of two fake
// Hosts, one warm MicroVM and the fake Pool Manager.
type Options struct {
	// Hosts is the number of fake Hosts; zero means DefaultHosts. Ignored
	// on the hardware tier.
	Hosts int
	// PoolSize is the Profile's Pool size; zero means DefaultPoolSize.
	PoolSize int
	// BootDelay is how long a fake MicroVM stays PENDING (TD-022); zero
	// means DefaultBootDelay.
	BootDelay time.Duration
	// RunnerBinary is a prebuilt flintlock-runner. Empty means build one
	// into the Stack's root with `go build`, which has to run inside this
	// module.
	RunnerBinary string
	// Root is the directory the Stack owns: the configuration, the Runner's
	// log and state, the builds and cache directories and every sandbox
	// live under it. Empty means a new temporary directory. Shutdown
	// removes it unless KeepRoot is set.
	Root     string
	KeepRoot bool
	// RunnerOutput, when set, receives a copy of the Runner's standard
	// output and standard error. They are always written to RunnerLog.
	RunnerOutput io.Writer
	// Logf, when set, receives one line per step the harness takes.
	Logf func(format string, args ...any)
	// HardwareInventory is the value of EnvHardwareInventory (TD-052).
	HardwareInventory string
	// PoolManagerEndpoint is the value of EnvPoolManager (TD-053).
	PoolManagerEndpoint string
	// KernelImage and RootFSImage override the Profile images. The fake
	// tier uses placeholders; the hardware tier requires both.
	KernelImage string
	RootFSImage string
	// Configure, when set, may change the generated configuration before
	// it is written, for scenarios that need a different setting.
	Configure func(*config.Config)
}

// OptionsFromEnv returns opts with the tier switches filled from the
// environment (TD-052, TD-053) and the Runner binary from
// EnvRunnerBinary, where opts leaves them empty.
func OptionsFromEnv(opts Options) Options {
	return optionsFromLookup(opts, os.LookupEnv)
}

func optionsFromLookup(opts Options, lookup func(string) (string, bool)) Options {
	fill := func(dst *string, key string) {
		if *dst != "" {
			return
		}
		if v, ok := lookup(key); ok {
			*dst = v
		}
	}
	fill(&opts.HardwareInventory, EnvHardwareInventory)
	fill(&opts.PoolManagerEndpoint, EnvPoolManager)
	fill(&opts.KernelImage, EnvKernelImage)
	fill(&opts.RootFSImage, EnvRootFSImage)
	fill(&opts.RunnerBinary, EnvRunnerBinary)
	return opts
}

// Hardware reports whether the options select the hardware tier (TD-052).
func (o Options) Hardware() bool { return o.HardwareInventory != "" }

// Stack is a running set of fakes and, after StartRunner, the Runner. Its
// methods are safe for concurrent use.
type Stack struct {
	opts Options

	// Root is the directory the Stack owns (Options.Root).
	Root string
	// ConfigPath is the generated runner configuration file.
	ConfigPath string
	// RunnerLog is where the Runner's output is written.
	RunnerLog string
	// Config is the configuration written to ConfigPath, defaults applied.
	Config *config.Config

	// GitLab is the fake GitLab.
	GitLab *fakegitlab.Server
	// PoolManager is the fake Pool Manager; nil when a real one is used
	// (TD-053).
	PoolManager *pmfake.PoolManager
	// Hosts are the fake Hosts; nil on the hardware tier (TD-052).
	Hosts []*hostfake.Host
	// Inventory is what the Runner's Inventory lists: the fake Hosts, or
	// the hardware Inventory.
	Inventory []config.HostEntry

	ownRoot    bool
	pmAddr     string
	pmHosts    *pmfake.Hosts
	pmCancel   context.CancelFunc
	pmDone     chan error
	hostCancel context.CancelFunc
	hostDone   []chan error

	mu        sync.Mutex
	runner    *runnerProc
	nextJobID int64
	down      bool
}

// logf reports one harness step.
func (s *Stack) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// PoolManagerAddr is the Pool Manager endpoint the Runner is configured
// with: the fake's listen address or EnvPoolManager.
func (s *Stack) PoolManagerAddr() string { return s.pmAddr }

// Pool is the Pool the Runner declares for the Default Profile.
func (s *Stack) Pool() poolmgr.PoolRef {
	return poolmgr.PoolRef{Namespace: Namespace, Name: ProfileName}
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//# The project SHALL provide an end-to-end harness that starts the
//# fake GitLab, the fake Pool Manager, a configurable number of fake Hosts
//# and the real `flintlock-runner` binary, submits Jobs and asserts on the
//# recorded trace and final state, and SHALL run in continuous integration
//# on a machine without KVM.

// Start brings up the fake GitLab, the Hosts and the Pool Manager, and
// writes and validates the runner configuration. It does not start the
// Runner; StartRunner does. On error everything started so far is stopped
// and the root is removed.
func Start(ctx context.Context, opts Options) (s *Stack, err error) {
	if opts.Hosts <= 0 {
		opts.Hosts = DefaultHosts
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = DefaultPoolSize
	}
	if opts.BootDelay <= 0 {
		opts.BootDelay = DefaultBootDelay
	}
	s = &Stack{opts: opts, nextJobID: 1}
	defer func() {
		if err != nil {
			s.teardown()
			s = nil
		}
	}()

	if err := s.makeRoot(); err != nil {
		return s, err
	}
	s.logf("working directory %s", s.Root)

	s.GitLab = fakegitlab.New(fakegitlab.Options{
		RunnerToken: RunnerToken,
		// Short enough that a Runner asking for a Job during shutdown is
		// not held for long, long enough that an idle Runner is not
		// spinning; a Job enqueued meanwhile wakes the poll at once.
		LongPollTimeout: 2 * time.Second,
		// One second is the smallest interval the network client honours
		// (GL-051), so the trace arrives while the Job runs.
		TraceUpdateInterval: time.Second,
	})
	if err := s.GitLab.Start(); err != nil {
		return s, err
	}
	s.logf("fake GitLab listening on %s", s.GitLab.URL())

	if opts.Hardware() {
		err = s.useHardwareHosts()
	} else {
		err = s.startFakeHosts(ctx)
	}
	if err != nil {
		return s, err
	}

	if err := s.startPoolManager(ctx); err != nil {
		return s, err
	}

	if err := s.writeConfig(); err != nil {
		return s, err
	}
	s.logf("runner configuration written to %s", s.ConfigPath)
	return s, nil
}

// makeRoot creates the Stack's directory tree.
func (s *Stack) makeRoot() error {
	root := s.opts.Root
	if root == "" {
		dir, err := os.MkdirTemp("", "flintlock-harness-")
		if err != nil {
			return fmt.Errorf("harness: creating root: %w", err)
		}
		root = dir
		s.ownRoot = true
	} else {
		abs, err := filepath.Abs(root)
		if err != nil {
			return fmt.Errorf("harness: root %s: %w", root, err)
		}
		root = abs
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("harness: creating root: %w", err)
		}
	}
	s.Root = root
	s.ConfigPath = filepath.Join(root, "config.yaml")
	s.RunnerLog = filepath.Join(root, "runner.log")
	for _, d := range []string{s.buildsDir(), s.cacheDir(), s.stateDir(), s.homeDir(), filepath.Join(root, "hosts")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("harness: creating %s: %w", d, err)
		}
	}
	return nil
}

func (s *Stack) buildsDir() string { return filepath.Join(s.Root, "builds") }
func (s *Stack) cacheDir() string  { return filepath.Join(s.Root, "cache") }
func (s *Stack) stateDir() string  { return filepath.Join(s.Root, "state") }
func (s *Stack) homeDir() string   { return filepath.Join(s.Root, "home") }

// guestDirs are the builds and cache directories as the Profile names them.
// On the fake tier they are absolute paths under the root, because the
// generated scripts run on this machine; on the hardware tier they are the
// guest's own.
func (s *Stack) guestDirs() (builds, cache string) {
	if s.opts.Hardware() {
		return config.DefaultBuildsDir, config.DefaultCacheDir
	}
	return s.buildsDir(), s.cacheDir()
}

// startFakeHosts serves opts.Hosts fake Hosts on loopback ports, each with
// its sandboxes under the root, basic auth on and the exec service
// reported enabled.
func (s *Stack) startFakeHosts(ctx context.Context) error {
	hostCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.hostCancel = cancel
	for i := 1; i <= s.opts.Hosts; i++ {
		name := fmt.Sprintf("host-%d", i)
		h := hostfake.New(flintlock.FakeHostConfig{
			Name:        name,
			Listen:      "127.0.0.1:0",
			SandboxRoot: filepath.Join(s.Root, "hosts", name),
			BootDelay:   s.opts.BootDelay,
			Version:     "v0.0.0-harness",
			ExecEnabled: true,
			Token:       HostToken,
		})
		done := make(chan error, 1)
		go func() { done <- h.Serve(hostCtx) }()
		select {
		case <-h.Ready():
		case err := <-done:
			if err == nil {
				err = errors.New("stopped before it was listening")
			}
			return fmt.Errorf("harness: fake host %s: %w", name, err)
		}
		s.Hosts = append(s.Hosts, h)
		s.hostDone = append(s.hostDone, done)
		s.Inventory = append(s.Inventory, config.HostEntry{
			Name:     name,
			Endpoint: h.Addr(),
			Arch:     config.Architecture(runtime.GOARCH),
			VCPU:     fakeHostVCPU,
			MemoryMB: fakeHostMemoryMB,
			Token:    HostToken,
			TLS:      config.ClientTLS{Insecure: true},
		})
		s.logf("fake Host %s listening on %s (sandboxes under %s)", name, h.Addr(), h.SandboxRoot())
	}
	return nil
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//# Where the environment variable naming a hardware Inventory is
//# set, the harness SHALL run the same scenarios against the real Hosts it
//# lists instead of fake Hosts, and SHALL be skipped otherwise.

// useHardwareHosts reads the hardware Inventory (TD-052). No fake Host is
// started: the Runner's Inventory is the file's hosts, and the fake Pool
// Manager, when there is one, dials them over gRPC as it would any
// flintlockd.
func (s *Stack) useHardwareHosts() error {
	if s.opts.KernelImage == "" || s.opts.RootFSImage == "" {
		return fmt.Errorf("harness: the hardware tier (%s) needs a real kernel and root filesystem image: set %s and %s",
			EnvHardwareInventory, EnvKernelImage, EnvRootFSImage)
	}
	inv, err := config.LoadInventoryFile(s.opts.HardwareInventory)
	if err != nil {
		return fmt.Errorf("harness: hardware inventory: %w", err)
	}
	if len(inv.Hosts) == 0 {
		return fmt.Errorf("harness: hardware inventory %s lists no hosts", s.opts.HardwareInventory)
	}
	s.Inventory = append([]config.HostEntry(nil), inv.Hosts...)
	for _, h := range s.Inventory {
		s.logf("hardware Host %s at %s", h.Name, h.Endpoint)
	}
	return nil
}

// endpoints converts the Inventory to what a flintlock dialer takes.
func (s *Stack) endpoints() []flintlock.Endpoint {
	out := make([]flintlock.Endpoint, 0, len(s.Inventory))
	for _, h := range s.Inventory {
		out = append(out, flintlock.Endpoint{
			Name:    h.Name,
			Address: h.Endpoint,
			Token:   string(h.Token),
			TLS: flintlock.TLSOptions{
				CAFile:   h.TLS.CAFile,
				CertFile: h.TLS.CertFile,
				KeyFile:  h.TLS.KeyFile,
				Insecure: h.TLS.Insecure,
			},
		})
	}
	return out
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//# Where the environment variable naming a real Pool Manager
//# endpoint is set, the harness SHALL use it instead of the fake Pool
//# Manager.

// startPoolManager serves the fake Pool Manager over the Inventory's Hosts,
// or, when a real Pool Manager endpoint is given, starts nothing and points
// the Runner at it (TD-053).
func (s *Stack) startPoolManager(ctx context.Context) error {
	if s.opts.PoolManagerEndpoint != "" {
		s.pmAddr = s.opts.PoolManagerEndpoint
		s.logf("using the Pool Manager at %s; no fake Pool Manager started", s.pmAddr)
		return nil
	}
	// The fake reaches every Host over gRPC at its Inventory endpoint, as it
	// reaches flintlockd, under its Inventory name, which is what makes a
	// claim's Placement resolvable by the Runner (CF-032).
	hosts, err := pmfake.HostsFromDialer(ctx, pmfake.AdminDialer{}, s.endpoints())
	if err != nil {
		return fmt.Errorf("harness: dialing hosts for the fake pool manager: %w", err)
	}
	s.pmHosts = hosts
	pm := pmfake.New(poolmgr.FakeConfig{
		Listen:            "127.0.0.1:0",
		Hosts:             hosts,
		ReconcileInterval: 250 * time.Millisecond,
		Namespace:         Namespace,
	})
	pmCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.pmCancel = cancel
	s.pmDone = make(chan error, 1)
	go func() { s.pmDone <- pm.Serve(pmCtx) }()
	select {
	case <-pm.Ready():
	case err := <-s.pmDone:
		s.pmDone <- err
		if err == nil {
			err = errors.New("stopped before it was listening")
		}
		return fmt.Errorf("harness: fake pool manager: %w", err)
	}
	s.PoolManager = pm
	s.pmAddr = pm.Addr()
	s.logf("fake Pool Manager listening on %s (hosts %v)", s.pmAddr, hosts.Names())
	return nil
}

// RunnerRunning reports whether the Runner process is running.
func (s *Stack) RunnerRunning() bool {
	s.mu.Lock()
	r := s.runner
	s.mu.Unlock()
	return r != nil && !r.exited()
}

// Up is Start followed by StartRunner.
func Up(ctx context.Context, opts Options) (*Stack, error) {
	s, err := Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := s.StartRunner(ctx); err != nil {
		return nil, errors.Join(err, s.Shutdown(context.WithoutCancel(ctx)))
	}
	return s, nil
}

// lookBash finds the bash the Profile's shell points at. The fake Host runs
// the shell as a local process, so it has to be this machine's bash, which
// is not always /bin/bash.
func lookBash() (string, error) {
	path, err := exec.LookPath("bash")
	if err != nil {
		return "", fmt.Errorf("harness: the fake Hosts run Stages with this machine's bash: %w", err)
	}
	return filepath.Abs(path)
}
