// Package fake is the fake Host (docs/requirements/10-test-doubles.md#fake-host):
// a flintlock MicroVM and MicroVMExec server (TD-020) that represents each
// MicroVM as a sandbox directory and runs ExecCommand as a local process
// (TD-021). It also provides an in-memory flintlock.PoolHostClient bound to a
// Host directly, plus a Dialer/AdminDialer over a set of fake Hosts, so that
// Scheduler and transport tests run without gRPC.
//
// One Host has one lifetime. It starts at New and ends at Close, or when the
// context given to Serve is cancelled; the two are the same thing, because a
// Host whose listener is gone has nothing left to keep its processes alive
// for. Closing kills every child process and stops every pending boot, and
// removes no sandbox directory: a sandbox is deleted only by DeleteMicroVM,
// so whatever is left under SandboxRoot afterwards is a MicroVM somebody
// forgot to delete (TD-054). Sandboxes reports them.
//
// Behaviour is modelled on flintlockd where the fake can observe it: the
// same status codes, the same first-message rule on ExecCommand, the same
// error-then-exit_code framing, the same basic auth header. Where the fake
// is more permissive it says so in the method comment.
package fake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// errClosed is the internal cause when the Host has been closed.
var errClosed = errors.New("fake host closed")

// Option configures a Host beyond its FakeHostConfig.
type Option func(*Host)

// WithClock makes the Host schedule boot delays (TD-022) and exec timeouts
// (TD-023) against clk instead of the wall clock, so that tests advance a
// fake clock rather than sleep.
func WithClock(clk clock.Clock) Option {
	return func(h *Host) { h.clk = clk }
}

// Host is one fake Host. Zero values of its methods are not usable; build it
// with New.
type Host struct {
	cfg     flintlock.FakeHostConfig
	clk     clock.Clock
	started time.Time

	// ctx is the Host's lifetime. cancel ends it; wg counts the goroutines
	// (boot timers, in-process exec handlers) that have to finish before
	// Close returns.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mu guards everything below it.
	mu          sync.Mutex
	sandboxRoot string
	vms         map[string]*microVM
	addr        string
	ready       chan struct{}
	served      bool
	closed      bool

	// faultsMu guards faults and faultsCh. faultsCh is closed and replaced
	// on every SetFaults so that a goroutine blocked by the Unresponsive
	// fault wakes up when a test clears it.
	faultsMu sync.Mutex
	faults   flintlock.HostFaults
	faultsCh chan struct{}
}

// New builds a fake Host from cfg. The Host is usable in process through
// Client at once; Serve adds the gRPC listener.
func New(cfg flintlock.FakeHostConfig, opts ...Option) *Host {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Host{
		cfg:         cfg,
		clk:         clock.Real{},
		ctx:         ctx,
		cancel:      cancel,
		sandboxRoot: cfg.SandboxRoot,
		vms:         make(map[string]*microVM),
		ready:       make(chan struct{}),
		faultsCh:    make(chan struct{}),
	}
	for _, o := range opts {
		o(h)
	}
	h.started = h.clk.Now()
	return h
}

// Config returns the configuration the Host was built with.
func (h *Host) Config() flintlock.FakeHostConfig { return h.cfg }

// Addr is the bound listen address once Serve is running, empty before.
func (h *Host) Addr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addr
}

// Ready is closed once Serve is listening, at which point Addr is set. It
// never closes if Serve fails before binding; wait on it together with
// Serve's error.
func (h *Host) Ready() <-chan struct{} { return h.ready }

// Client returns an in-memory PoolHostClient bound to this Host, for tests
// that need no gRPC. It carries the Host's own token, so it is always
// authorised.
func (h *Host) Client() flintlock.PoolHostClient { return h.newClient(h.cfg.Token) }

// Close ends the Host: it kills every running exec process, stops every
// pending boot and waits for them. Sandbox directories are left where they
// are (TD-054). Close is idempotent and is also what Serve does on its way
// out.
func (h *Host) Close() error {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
	return nil
}

//= docs/requirements/10-test-doubles.md#fake-host
//# The fake Host SHALL support fault injection for a create that ends in
//# `FAILED`, an exec stream dropped before the exit code, and a Host that
//# stops answering.

// SetFaults implements flintlock.HostFaultInjector. Faults are read at the
// point where they act: CreateFails when the boot timer fires,
// DropExecBeforeExit just before the exit code would be sent, Unresponsive
// on every request and before every response chunk. Clearing Unresponsive
// wakes whatever it was blocking.
func (h *Host) SetFaults(f flintlock.HostFaults) {
	h.faultsMu.Lock()
	defer h.faultsMu.Unlock()
	h.faults = f
	close(h.faultsCh)
	h.faultsCh = make(chan struct{})
}

// Faults implements flintlock.HostFaultInjector.
func (h *Host) Faults() flintlock.HostFaults {
	h.faultsMu.Lock()
	defer h.faultsMu.Unlock()
	return h.faults
}

// faultsAndSignal returns the current faults and a channel that is closed
// the next time they change.
func (h *Host) faultsAndSignal() (flintlock.HostFaults, <-chan struct{}) {
	h.faultsMu.Lock()
	defer h.faultsMu.Unlock()
	return h.faults, h.faultsCh
}

// awaitResponsive blocks while the Unresponsive fault is set (TD-025). It
// returns nil once the Host answers again, ctx.Err() when the caller gives
// up first, and errClosed when the Host is closed meanwhile. This is what
// makes the fault a timeout at the caller rather than a refused connection:
// the listener stays open and the request is simply never answered.
func (h *Host) awaitResponsive(ctx context.Context) error {
	for {
		faults, changed := h.faultsAndSignal()
		if !faults.Unresponsive {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-h.ctx.Done():
			return errClosed
		}
	}
}

// SandboxRoot is the directory the sandboxes live under: the configured one,
// or a temporary directory created on first use when none was configured.
func (h *Host) SandboxRoot() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sandboxRoot
}

// SandboxPath returns the sandbox directory of a MicroVM the Host currently
// has, so that tests can seed files for a command to find.
func (h *Host) SandboxPath(uid string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	vm, ok := h.vms[uid]
	if !ok {
		return "", false
	}
	return vm.sandbox, true
}

// Sandboxes lists the sandbox directories that exist under SandboxRoot, by
// uid, sorted. It reads the filesystem rather than the Host's memory so that
// it is meaningful after Close, which is when the harness asks whether a
// scenario left a MicroVM behind (TD-054). It returns nil when the root was
// never created.
func (h *Host) Sandboxes() ([]string, error) {
	root := h.SandboxRoot()
	if root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing sandboxes under %s: %w", root, err)
	}
	var uids []string
	for _, e := range entries {
		if e.IsDir() {
			uids = append(uids, e.Name())
		}
	}
	sort.Strings(uids)
	return uids, nil
}

// ensureSandboxRoot creates the sandbox root, choosing a temporary one when
// the configuration left it empty. Callers hold h.mu.
func (h *Host) ensureSandboxRootLocked() (string, error) {
	if h.sandboxRoot == "" {
		dir, err := os.MkdirTemp("", "fakehost-"+sanitizeName(h.cfg.Name)+"-")
		if err != nil {
			return "", fmt.Errorf("creating sandbox root: %w", err)
		}
		h.sandboxRoot = dir
		return dir, nil
	}
	if err := os.MkdirAll(h.sandboxRoot, 0o755); err != nil {
		return "", fmt.Errorf("creating sandbox root %s: %w", h.sandboxRoot, err)
	}
	return h.sandboxRoot, nil
}

// sanitizeName makes a Host name safe for use in a directory name.
func sanitizeName(name string) string {
	if name == "" {
		return "host"
	}
	return filepath.Base(name)
}

// newUID returns a fresh MicroVM uid. flintlockd uses ULIDs; any unique
// token does for the fake.
func newUID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating uid: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// listenAddress is the address Serve binds; an empty Listen means a free
// loopback port, which is what every test wants.
func (h *Host) listenAddress() string {
	if h.cfg.Listen == "" {
		return "127.0.0.1:0"
	}
	return h.cfg.Listen
}

// listen binds the listener and publishes Addr. It is separate from Serve so
// that Serve's error path is easy to read.
func (h *Host) listen() (net.Listener, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errClosed
	}
	if h.served {
		return nil, errors.New("fake host: Serve called twice")
	}
	lis, err := net.Listen("tcp", h.listenAddress())
	if err != nil {
		return nil, fmt.Errorf("fake host %s: listen: %w", h.cfg.Name, err)
	}
	h.served = true
	h.addr = lis.Addr().String()
	close(h.ready)
	return lis, nil
}

// Dialer hands out in-memory clients for a set of fake Hosts keyed by name.
// It implements both flintlock.Dialer and flintlock.AdminDialer; the
// Scheduler is given it as a Dialer and the fake Pool Manager as an
// AdminDialer, which keeps the HO-007 split intact in tests.
//
// The Endpoint's Token is checked against the Host's configured token on
// every call, so a wrong token fails with flintlock.ErrUnauthenticated in
// memory as it does over gRPC (TD-024).
type Dialer struct {
	mu    sync.Mutex
	hosts map[string]*Host
}

// NewDialer builds a Dialer over hosts.
func NewDialer(hosts ...*Host) *Dialer {
	d := &Dialer{hosts: make(map[string]*Host, len(hosts))}
	for _, h := range hosts {
		d.hosts[h.cfg.Name] = h
	}
	return d
}

// Add registers another Host after construction, for reload tests (HO-014).
func (d *Dialer) Add(h *Host) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hosts[h.cfg.Name] = h
}

// Dial implements flintlock.Dialer. It matches on ep.Name, returns
// flintlock.ErrUnknownHost for a Host it does not have, and hides the admin
// methods of the underlying client.
func (d *Dialer) Dial(ctx context.Context, ep flintlock.Endpoint) (flintlock.HostClient, error) {
	c, err := d.DialAdmin(ctx, ep)
	if err != nil {
		return nil, err
	}
	// Wrap so that a type assertion to flintlock.HostAdminClient fails in
	// tests exactly as it would against a real Runner-side client (HO-007).
	return struct{ flintlock.HostClient }{c}, nil
}

// DialAdmin implements flintlock.AdminDialer.
func (d *Dialer) DialAdmin(_ context.Context, ep flintlock.Endpoint) (flintlock.PoolHostClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.hosts[ep.Name]
	if !ok {
		return nil, fmt.Errorf("fake dialer: %q: %w", ep.Name, flintlock.ErrUnknownHost)
	}
	return h.newClient(ep.Token), nil
}

// cloneMicroVM returns a deep copy so that callers never share the Host's
// record.
func cloneMicroVM(vm *types.MicroVM) *types.MicroVM {
	return cloneProto(vm)
}

// Compile-time interface checks.
var (
	_ flintlock.PoolHostClient    = (*Client)(nil)
	_ flintlock.HostFaultInjector = (*Host)(nil)
	_ flintlock.Dialer            = (*Dialer)(nil)
	_ flintlock.AdminDialer       = (*Dialer)(nil)
)
