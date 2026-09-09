// Package fake is the fake Pool Manager
// (docs/requirements/10-test-doubles.md#fake-pool-manager): a minimal but
// real pool manager on the battery protos. It serves the poolmgr.v1alpha1
// PoolAdmin, Lease and Events services over gRPC with the generated server
// stubs (TD-001), creates, places and deletes MicroVMs only through
// flintlock.PoolHostClient values obtained from a poolmgr.HostSource
// (TD-002), and is usable in process through PoolManager.Client, which is a
// loopback gRPC client over the same handlers so that Scheduler tests
// exercise the wire path without a network.
//
// State is held in memory and is lost on shutdown; because nothing persists,
// Run deletes every MicroVM it created before returning, so a stopped fake
// leaves no MicroVMs on its Hosts. The control loop (Run or Serve) drives
// replenishment (TD-003), lease expiry (TD-004) and deferred deletions on
// the configured clock; RPCs work before Run starts, but provisioning waits
// for it.
//
// Behaviour follows battery main as of 2026-09-07 where the two overlap:
// least-VM-count placement per Pool (TD-007), RESOURCE_EXHAUSTED on an empty
// Pool (TD-005), the host field on ClaimVMResponse (TD-008), the three
// replenishment strategies, hook failure policies and the event types. Where
// the fake is deliberately different it says so in the code: the tick tops a
// Pool up to its size for every strategy (battery relies on the strategy
// alone), event payloads carry JSON the Runner reads for PL-055, and
// DeletePool deletes the Pool's idle MicroVMs instead of refusing while any
// exist.
package fake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// Defaults for zero FakeConfig fields and internal pacing.
const (
	defaultReconcileInterval = time.Second
	defaultReadyTimeout      = 60 * time.Second
	defaultEventReplay       = 100
	// createPollInterval paces GetMicroVM while a MicroVM boots and the
	// guest-agent readiness probe before create hooks. The first check is
	// immediate, so an instant Host needs no clock movement.
	createPollInterval = 250 * time.Millisecond
	// shutdownTimeout bounds the deletion of every MicroVM when Run stops.
	shutdownTimeout = 30 * time.Second
)

// ErrAlreadyRunning is returned by Run and Serve when the fake has already
// been started; a PoolManager runs once.
var ErrAlreadyRunning = errors.New("fake poolmgr: already running or stopped")

// ErrStopped is what every call on a Client taken after the fake has shut
// down returns. The fake keeps nothing across a shutdown, so such a client
// could only fail; it says so plainly rather than pointing at a stopped
// server. A client taken before the shutdown fails with
// poolmgr.ErrUnavailable from then on, as a client of a real Pool Manager
// that went away does.
var ErrStopped = errors.New("fake poolmgr: shut down")

// PoolManager is the fake Pool Manager. It is constructed from a
// poolmgr.FakeConfig and served with Serve, or run in process with Run and
// used through Client. It implements poolmgr.FaultInjector and
// poolmgr.Inspector.
type PoolManager struct {
	cfg poolmgr.FakeConfig
	log *slog.Logger

	// mu guards every field below it up to events. Host calls are never made
	// while it is held.
	mu               sync.Mutex
	faults           poolmgr.Faults
	unavailableUntil time.Time
	pools            map[poolKey]*poolState
	vms              map[int64]*vmState
	vmByUID          map[string]*vmState
	leases           map[string]*leaseState
	nextVM           int64
	// runCtx is the control loop's context: nil before Run, done after it.
	runCtx context.Context
	// started is set by Run, stopped once it has begun shutting down. Both
	// are read under mu, so a caller that holds it and finds the fake
	// running can start background work knowing stop has not begun waiting
	// for it.
	started bool
	stopped bool
	// serving is claimed by Serve before it listens, so that the single
	// listener and the single close of ready belong to one call.
	serving bool
	wg      sync.WaitGroup

	events *eventBus
	// kick wakes the control loop for an immediate reconcile.
	kick chan struct{}

	addrMu sync.Mutex
	addr   string
	// ready is closed once Serve is listening.
	ready chan struct{}

	// loop is the in-memory server behind Client. New builds it, before any
	// goroutine can see the PoolManager, and nothing writes it afterwards.
	loop *loopback
}

// New builds a fake Pool Manager from cfg. Zero fields take the defaults
// documented on poolmgr.FakeConfig; a nil Hosts knows no Host, so Pools
// declared against it never fill.
func New(cfg poolmgr.FakeConfig) *PoolManager {
	if cfg.Placement == "" {
		cfg.Placement = poolmgr.PlacementLeastVMs
	}
	if cfg.Listen == "" {
		cfg.Listen = ":0"
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = defaultReconcileInterval
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}
	if cfg.EventReplay <= 0 {
		cfg.EventReplay = defaultEventReplay
	}
	if cfg.Hosts == nil {
		cfg.Hosts = NewHosts()
	}
	p := &PoolManager{
		cfg:     cfg,
		log:     slog.Default().With("component", "fake-poolmgr"),
		pools:   make(map[poolKey]*poolState),
		vms:     make(map[int64]*vmState),
		vmByUID: make(map[string]*vmState),
		leases:  make(map[string]*leaseState),
		events:  newEventBus(cfg.EventReplay),
		kick:    make(chan struct{}, 1),
		ready:   make(chan struct{}),
	}
	p.loop = p.newLoopback()
	return p
}

// Config returns the configuration the fake was built with, defaults
// applied.
func (p *PoolManager) Config() poolmgr.FakeConfig { return p.cfg }

// Serve listens on cfg.Listen, runs the control loop and serves the three
// gRPC services until ctx is cancelled (TD-001). It returns nil after a
// clean shutdown, during which every MicroVM the fake created is deleted
// from its Host. A PoolManager serves once. cmd/fake-poolmgr is the
// standalone binary over it (TD-009).
func (p *PoolManager) Serve(ctx context.Context) error {
	if err := p.claimServe(); err != nil {
		return err
	}
	lis, err := net.Listen("tcp", p.cfg.Listen)
	if err != nil {
		return fmt.Errorf("fake poolmgr: listen %s: %w", p.cfg.Listen, err)
	}
	p.addrMu.Lock()
	p.addr = lis.Addr().String()
	p.addrMu.Unlock()
	close(p.ready)

	srv := p.newGRPCServer()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(runCtx) }()

	select {
	case <-ctx.Done():
		srv.Stop()
		<-serveErr
		return <-runErr
	case err := <-serveErr:
		cancel()
		<-runErr
		return fmt.Errorf("fake poolmgr: serve: %w", err)
	case err := <-runErr:
		srv.Stop()
		<-serveErr
		if err != nil {
			return err
		}
		return errors.New("fake poolmgr: control loop stopped while serving")
	}
}

// claimServe reserves this PoolManager's single serve. Serve calls it
// before it listens, so that a second Serve returns ErrAlreadyRunning
// instead of binding a second listener and closing the already closed ready
// channel, which panicked on the caller's goroutine.
func (p *PoolManager) claimServe() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.serving || p.started || p.stopped {
		return ErrAlreadyRunning
	}
	p.serving = true
	return nil
}

// Addr is the bound listen address once Serve is running, empty before.
func (p *PoolManager) Addr() string {
	p.addrMu.Lock()
	defer p.addrMu.Unlock()
	return p.addr
}

// Ready is closed once Serve is listening, so a caller that started Serve on
// another goroutine can wait for Addr without polling. It never closes if
// Serve fails to listen; select on Serve's result as well.
func (p *PoolManager) Ready() <-chan struct{} { return p.ready }

// Run drives the control loop without a network listener: it tops Pools up
// per their replenishment strategy (TD-003), expires Leases (TD-004) and
// retries deferred deletions every cfg.ReconcileInterval on cfg.Clock, and
// immediately after a Pool is created or updated. It blocks until ctx is
// cancelled, then waits for in-flight provisioning, deletes every MicroVM
// it created and returns nil. Serve calls it; call it directly for in-process
// use with Client.
func (p *PoolManager) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return ErrAlreadyRunning
	}
	p.started = true
	p.runCtx = runCtx
	p.mu.Unlock()
	defer p.stop(cancel)

	timer := p.cfg.Clock.NewTimer(p.cfg.ReconcileInterval)
	defer timer.Stop()
	p.tick(runCtx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C():
			p.tick(runCtx)
			timer.Reset(p.cfg.ReconcileInterval)
		case <-p.kick:
			p.tick(runCtx)
		}
	}
}

// stop ends the control loop: it cancels background work, waits for it,
// deletes every MicroVM on its Host and shuts the loopback server down.
// Marking the fake stopped under p.mu before the wait is what keeps a
// handler from starting work between the cancellation and the wait: a
// handler that reaches provisionNLocked either holds the lock first, and so
// adds to the WaitGroup before stop takes it, or finds the fake stopped.
func (p *PoolManager) stop(cancel context.CancelFunc) {
	cancel()
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()

	p.wg.Wait()
	p.cleanupAll()
	p.loop.srv.Stop()
}

// cleanupAll deletes every MicroVM the fake still knows about. The fake has
// no persistence, so a MicroVM left behind would be an orphan on its Host.
func (p *PoolManager) cleanupAll() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	p.mu.Lock()
	vms := make([]*vmState, 0, len(p.vms))
	for _, vm := range p.vms {
		vms = append(vms, vm)
	}
	p.mu.Unlock()
	for _, vm := range vms {
		if err := p.deleteVM(ctx, vm, 0); err != nil {
			p.log.Warn("shutdown: microvm not deleted", "uid", vm.uid, "host", vm.host, "error", err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ls := range p.leases {
		p.log.Warn("shutdown: lease dropped", "lease_id", id, "vm_uid", ls.rec.VMUID)
		p.dropLeaseLocked(id)
	}
}

// kickReconcile wakes the control loop without blocking.
func (p *PoolManager) kickReconcile() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Client returns an in-process poolmgr.Client bound to this fake. It is a
// gRPC client over an in-memory connection to the same handlers Serve
// exposes, so it behaves exactly like the Runner's client against the
// standalone binary. Each call returns an independent client; Close closes
// only that client's connection.
//
// The lifecycle is the fake's, not the client's. Taken before Run, the
// client works: every RPC is served, and only provisioning waits for the
// control loop. Taken after Run has returned, every call fails with
// ErrStopped rather than pointing at a server that is not there; a client
// taken earlier and kept fails with poolmgr.ErrUnavailable from the
// shutdown on.
func (p *PoolManager) Client() poolmgr.Client {
	conn, err := p.loopbackConn()
	switch {
	case errors.Is(err, ErrStopped):
		return &Client{err: err}
	case err != nil:
		return &Client{err: fmt.Errorf("fake poolmgr: loopback: %w", err)}
	}
	return newClient(conn)
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL support fault injection for claim latency, a
//# period of `UNAVAILABLE`, hook failure and a dropped `Events` stream.

// SetFaults implements poolmgr.FaultInjector. UnavailableFor starts counting
// on the fake's clock when it is set; DropEventsStream ends every open
// Subscribe stream and clears itself; the other switches are read on each
// request.
func (p *PoolManager) SetFaults(f poolmgr.Faults) {
	f.HookFailures = append([]poolmgr.HookFailure(nil), f.HookFailures...)
	drop := f.DropEventsStream
	f.DropEventsStream = false

	p.mu.Lock()
	p.faults = f
	if f.UnavailableFor > 0 {
		p.unavailableUntil = p.cfg.Clock.Now().Add(f.UnavailableFor)
	} else {
		p.unavailableUntil = time.Time{}
	}
	p.mu.Unlock()

	if drop {
		p.events.dropAll()
	}
}

// Faults implements poolmgr.FaultInjector.
func (p *PoolManager) Faults() poolmgr.Faults {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.faults
	f.HookFailures = append([]poolmgr.HookFailure(nil), f.HookFailures...)
	return f
}

// Leases implements poolmgr.Inspector (TD-054). Records are sorted by claim
// time, then lease id.
func (p *PoolManager) Leases() []poolmgr.LeaseRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]poolmgr.LeaseRecord, 0, len(p.leases))
	for _, ls := range p.leases {
		out = append(out, ls.rec)
	}
	sortLeases(out)
	return out
}

// VMs implements poolmgr.Inspector, which is how a test sees the placement
// TD-007 asks for. Records are in creation order;
// a MicroVM whose CreateMicroVM call has not returned yet has an empty UID.
func (p *PoolManager) VMs() []poolmgr.VMRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.vmRecordsLocked()
}

// Compile-time interface checks.
var (
	_ poolmgr.FaultInjector = (*PoolManager)(nil)
	_ poolmgr.Inspector     = (*PoolManager)(nil)
)
