package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// Test support: an in-memory flintlock.PoolHostClient, a harness that runs
// the fake on a fake clock, and event helpers. The fake Host in
// internal/flintlock/fake is built by another work package; the stub here
// is the minimum a pool manager needs from a Host.

const (
	testNamespace = "runner-ns"
	testInterval  = time.Second
	testTimeout   = 10 * time.Second
)

var testEpoch = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

// stubHost is an instant, in-memory Host. Knobs make it misbehave.
type stubHost struct {
	name string

	mu      sync.Mutex
	next    int
	vms     map[string]*types.MicroVM
	created []string
	deleted []string
	execs   []string

	// stayPending keeps every MicroVM PENDING; failCreate reports FAILED.
	stayPending bool
	failCreate  bool
	createErr   error
	// deleteErr is returned by DeleteMicroVM while set.
	deleteErr error
	// execExit is the exit code every hook command gets; execErr fails the
	// stream instead.
	execExit int32
	execErr  error
}

func newStubHost(name string) *stubHost {
	return &stubHost{name: name, vms: make(map[string]*types.MicroVM)}
}

func (h *stubHost) Name() string { return h.name }

func (h *stubHost) ServerInfo(context.Context) (*flintlock.HostInfo, error) {
	return &flintlock.HostInfo{Name: h.name, VersionKnown: true, Version: "stub", Exec: flintlock.GuestService{Enabled: true}}, nil
}

func (h *stubHost) GetMicroVM(_ context.Context, uid string) (*types.MicroVM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	vm, ok := h.vms[uid]
	if !ok {
		return nil, fmt.Errorf("%w: %s", flintlock.ErrNotFound, uid)
	}
	return vm, nil
}

func (h *stubHost) ListMicroVMs(context.Context, string) ([]*types.MicroVM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*types.MicroVM, 0, len(h.vms))
	for _, vm := range h.vms {
		out = append(out, vm)
	}
	return out, nil
}

func (h *stubHost) Exec(context.Context) (flintlock.ExecStream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.execErr != nil {
		return nil, h.execErr
	}
	return &stubExecStream{host: h, exit: h.execExit}, nil
}

func (h *stubHost) SSHProxy(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, errors.ErrUnsupported
}

func (h *stubHost) Close() error { return nil }

func (h *stubHost) CreateMicroVM(_ context.Context, spec *types.MicroVMSpec) (*types.MicroVM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.createErr != nil {
		return nil, h.createErr
	}
	h.next++
	uid := fmt.Sprintf("%s-vm%d", h.name, h.next)
	state := types.MicroVMStatus_CREATED
	switch {
	case h.stayPending:
		state = types.MicroVMStatus_PENDING
	case h.failCreate:
		state = types.MicroVMStatus_FAILED
	}
	spec.Uid = &uid
	vm := &types.MicroVM{Spec: spec, Status: &types.MicroVMStatus{
		State:             state,
		NetworkInterfaces: map[string]*types.NetworkInterfaceStatus{"net0": {HostDeviceName: "tap-" + uid, MacAddress: "aa:bb"}},
	}}
	h.vms[uid] = vm
	h.created = append(h.created, uid)
	return vm, nil
}

func (h *stubHost) DeleteMicroVM(_ context.Context, uid string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.deleteErr != nil {
		return h.deleteErr
	}
	if _, ok := h.vms[uid]; !ok {
		return fmt.Errorf("%w: %s", flintlock.ErrNotFound, uid)
	}
	delete(h.vms, uid)
	h.deleted = append(h.deleted, uid)
	return nil
}

// live returns the uids of the MicroVMs currently on the Host.
func (h *stubHost) live() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.vms)
}

func (h *stubHost) counts() (created, deleted int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.created), len(h.deleted)
}

func (h *stubHost) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.execs...)
}

func (h *stubHost) set(fn func(*stubHost)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

// stubExecStream answers one ExecStart with an exit code.
type stubExecStream struct {
	host *stubHost
	exit int32
	sent bool
}

func (s *stubExecStream) Send(req *execv1.ExecCommandRequest) error {
	if start := req.GetStart(); start != nil {
		s.host.mu.Lock()
		s.host.execs = append(s.host.execs, start.GetCmd())
		s.host.mu.Unlock()
	}
	return nil
}

func (s *stubExecStream) Recv() (*execv1.ExecCommandResponse, error) {
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	return &execv1.ExecCommandResponse{Payload: &execv1.ExecCommandResponse_ExitCode{ExitCode: s.exit}}, nil
}

func (s *stubExecStream) CloseSend() error { return nil }

// harness runs one fake Pool Manager on a fake clock with stub Hosts, an
// in-process client and a subscription to every event.
type harness struct {
	t      *testing.T
	ctx    context.Context
	clk    *clock.Fake
	hosts  *Hosts
	stubs  map[string]*stubHost
	pm     *PoolManager
	client poolmgr.Client
	events poolmgr.EventStream
	// seen counts every event type waitEvent has read.
	seen map[poolmgr.EventType]int
}

// newHarness builds and starts the fake. Zero cfg fields get test defaults:
// the fake clock, a one second reconcile interval and hosts as the
// HostSource with one stub per name.
func newHarness(t *testing.T, cfg poolmgr.FakeConfig, hostNames ...string) *harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	h := &harness{t: t, ctx: ctx, clk: clock.NewFake(testEpoch), hosts: NewHosts(), stubs: make(map[string]*stubHost), seen: make(map[poolmgr.EventType]int)}
	for _, name := range hostNames {
		s := newStubHost(name)
		h.stubs[name] = s
		h.hosts.Add(name, s, name+":9090")
	}
	if cfg.Clock == nil {
		cfg.Clock = h.clk
	}
	if cfg.Hosts == nil {
		cfg.Hosts = h.hosts
	}
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = testInterval
	}
	h.pm = New(cfg)
	h.client = h.pm.Client()
	events, err := h.client.Subscribe(ctx, poolmgr.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	h.events = events

	runDone := make(chan error, 1)
	go func() { runDone <- h.pm.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-runDone; err != nil {
			t.Errorf("Run returned %v", err)
		}
		_ = events.Close()
		_ = h.client.Close()
	})
	return h
}

func (h *harness) ref(name string) poolmgr.PoolRef {
	return poolmgr.PoolRef{Name: name, Namespace: testNamespace}
}

// spec is a valid IMMEDIATE_ON_LEASE PoolSpec on the given Hosts.
func (h *harness) spec(name string, size int32, hosts ...string) poolmgr.PoolSpec {
	return poolmgr.PoolSpec{
		Ref:                      h.ref(name),
		Template:                 &types.MicroVMSpec{Namespace: testNamespace, Vcpu: 1, MemoryInMb: 256},
		Size:                     size,
		FlintlockHosts:           hosts,
		Replenishment:            poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatInterval:        10 * time.Second,
		HeartbeatExpiryThreshold: 30 * time.Second,
	}
}

// createPool declares a Pool and waits until it is full.
func (h *harness) createPool(spec poolmgr.PoolSpec) {
	h.t.Helper()
	if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
		h.t.Fatalf("CreatePool(%s): %v", spec.Ref, err)
	}
	for i := int32(0); i < spec.Size; i++ {
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	}
}

// tick waits for the control loop to arm its timer, then fires it.
func (h *harness) tick() {
	h.t.Helper()
	if err := h.clk.BlockUntil(h.ctx, 1); err != nil {
		h.t.Fatalf("waiting for the reconcile timer: %v", err)
	}
	h.clk.Advance(testInterval)
}

// advance moves the clock by d and then fires one tick at the new time.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	h.clk.Advance(d)
	h.tick()
}

// waitEvent reads events until one of type typ arrives and returns it.
func (h *harness) waitEvent(typ poolmgr.EventType) *poolmgr.Event {
	h.t.Helper()
	return h.collectUntil(typ)[0]
}

// collectUntil reads events until one of type typ arrives and returns the
// events read, the match last. Events of one Pool arrive in order, so the
// slice is proof of what did and did not happen before the match.
func (h *harness) collectUntil(typ poolmgr.EventType) []*poolmgr.Event {
	h.t.Helper()
	var got []*poolmgr.Event
	for {
		e, err := h.events.Recv(h.ctx)
		if err != nil {
			h.t.Fatalf("waiting for %s after %d events: %v", typ, len(got), err)
		}
		h.seen[e.Type]++
		got = append(got, e)
		if e.Type == typ {
			// Put the match first so waitEvent can return got[0].
			got[0], got[len(got)-1] = got[len(got)-1], got[0]
			return got
		}
	}
}

// count returns how many events of type typ are in events.
func count(events []*poolmgr.Event, typ poolmgr.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// pool fetches a Pool's current view.
func (h *harness) pool(name string) *poolmgr.Pool {
	h.t.Helper()
	p, err := h.client.GetPool(h.ctx, h.ref(name))
	if err != nil {
		h.t.Fatalf("GetPool(%s): %v", name, err)
	}
	return p
}

// claim claims from a Pool and waits for VM_CLAIMED so that the state the
// claim changed under the lock is visible to the next GetPool.
func (h *harness) claim(name string) *poolmgr.Claim {
	h.t.Helper()
	c, err := h.client.ClaimVM(h.ctx, h.ref(name))
	if err != nil {
		h.t.Fatalf("ClaimVM(%s): %v", name, err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)
	return c
}

// release releases a Lease and waits for the deletion to complete.
func (h *harness) release(leaseID string) {
	h.t.Helper()
	if err := h.client.ReleaseVM(h.ctx, leaseID); err != nil {
		h.t.Fatalf("ReleaseVM(%s): %v", leaseID, err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
}

func hostOf(records []poolmgr.VMRecord) map[string]int {
	out := make(map[string]int)
	for _, r := range records {
		out[r.Host]++
	}
	return out
}
