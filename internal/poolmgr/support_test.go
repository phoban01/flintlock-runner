package poolmgr_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	flintlockfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	pmfake "github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
)

// The tests in this package run against the fake Pool Manager
// (internal/poolmgr/fake) over a real gRPC connection, with the fake Host
// (internal/flintlock/fake) underneath it, because that is the only way to
// tell that the client, and the policy built on it, behave the way they
// will against battery. Only the units' own clocks are fake: everything the
// tests wait for is an event or a channel, never a sleep.

const (
	testNamespace = "runner-ns"
	testRunner    = "runner-1"
	// testTimeout bounds every test's context. Nothing is expected to take
	// anything like this long; it turns a hang into a failure.
	testTimeout = 30 * time.Second
)

// testEpoch is where every fake clock starts.
var testEpoch = time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)

// testLogger discards output unless the test fails, so a failing test still
// shows the warnings the units wrote.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testWriter sends log output to t.Log.
type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Logf("%s", p)
	return len(p), nil
}

// poolManager is a fake Pool Manager serving gRPC on a local port, with one
// fake Host per name, plus a client of this package dialled at it.
type poolManager struct {
	t     *testing.T
	ctx   context.Context
	pm    *pmfake.PoolManager
	hosts *pmfake.Hosts
	addr  string
	// hostByName is the fake Host behind each name, for tests that assert on
	// what the Pool Manager did to it.
	hostByName map[string]*flintlockfake.Host
}

// startPoolManager serves a fake Pool Manager with one fake Host per name
// and returns it. Serve and every Host are cleaned up with the test.
//
// The fake runs on the wall clock. Nothing in these tests depends on its
// internal pacing: a Pool fills because creating it kicks the fake's
// reconcile, and the tests wait for the resulting events rather than for
// time to pass. The units under test keep their own fake clocks.
func startPoolManager(t *testing.T, ctx context.Context, cfg poolmgr.FakeConfig, hostNames ...string) *poolManager {
	t.Helper()
	p := &poolManager{t: t, ctx: ctx, hosts: pmfake.NewHosts(), hostByName: make(map[string]*flintlockfake.Host)}
	for _, name := range hostNames {
		host := flintlockfake.New(flintlock.FakeHostConfig{
			Name:        name,
			SandboxRoot: t.TempDir(),
			ExecEnabled: true,
			Version:     "fake",
		})
		t.Cleanup(func() { _ = host.Close() })
		p.hostByName[name] = host
		p.hosts.Add(name, host.Client(), name+":9090")
	}
	if cfg.Hosts == nil {
		cfg.Hosts = p.hosts
	}
	p.pm = pmfake.New(cfg)

	serveCtx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- p.pm.Serve(serveCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("fake pool manager: Serve: %v", err)
			}
		case <-time.After(testTimeout):
			t.Error("fake pool manager: Serve did not return")
		}
	})
	select {
	case <-p.pm.Ready():
	case err := <-served:
		t.Fatalf("fake pool manager: Serve returned before it was ready: %v", err)
	case <-ctx.Done():
		t.Fatalf("fake pool manager: waiting for Serve: %v", ctx.Err())
	}
	p.addr = p.pm.Addr()
	return p
}

// withNetworkStatus decorates a fake Host so that every MicroVM it reports
// carries a network interface status. The fake Host has no networking of
// its own, and the Pool Manager copies the interfaces of the MicroVM it
// hands out from the Host's answer, so this is what gives the claim
// response something to carry (PL-031).
type withNetworkStatus struct {
	flintlock.PoolHostClient
}

// GetMicroVM implements flintlock.HostClient.
func (h withNetworkStatus) GetMicroVM(ctx context.Context, uid string) (*types.MicroVM, error) {
	vm, err := h.PoolHostClient.GetMicroVM(ctx, uid)
	if err != nil {
		return nil, err
	}
	if vm.GetStatus() != nil && len(vm.GetStatus().GetNetworkInterfaces()) == 0 {
		vm.Status.NetworkInterfaces = map[string]*types.NetworkInterfaceStatus{
			poolmgr.GuestDeviceID: {HostDeviceName: "tap-" + uid, Index: 0, MacAddress: "aa:bb:cc:dd:ee:ff"},
		}
	}
	return vm, nil
}

// decorateHosts replaces every Host of the Pool Manager with one that
// reports network interfaces.
func (p *poolManager) decorateHosts() {
	p.t.Helper()
	for name, host := range p.hostByName {
		p.hosts.Add(name, withNetworkStatus{host.Client()}, name+":9090")
	}
}

// host is the fake Host behind a name.
func (p *poolManager) host(name string) *flintlockfake.Host {
	p.t.Helper()
	host, ok := p.hostByName[name]
	if !ok {
		p.t.Fatalf("no fake host named %q", name)
	}
	return host
}

// client dials the fake Pool Manager with the package's own client. The
// tests that are about the connection itself build their own instead.
func (p *poolManager) client() poolmgr.Client {
	p.t.Helper()
	c, err := poolmgr.NewClient(poolmgr.ClientConfig{
		Endpoint:      p.addr,
		TLS:           config.ClientTLS{Insecure: true},
		Deadline:      testTimeout,
		ReconnectBase: 10 * time.Millisecond,
		ReconnectMax:  50 * time.Millisecond,
	})
	if err != nil {
		p.t.Fatalf("NewClient: %v", err)
	}
	p.t.Cleanup(func() { _ = c.Close() })
	return c
}

// ref is a PoolRef in the test namespace.
func ref(name string) poolmgr.PoolRef {
	return poolmgr.PoolRef{Name: name, Namespace: testNamespace}
}

// testProfile is a Profile with everything a Pool needs and nothing more,
// with the configuration defaults applied to it the way loading a file
// would (CF-005), because that is the shape the Scheduler hands to the
// SpecBuilder. Callers change what they are testing.
func testProfile(name string, size int) config.Profile {
	return withDefaults(config.Profile{
		Name:     name,
		Arch:     config.ArchARM64,
		VCPU:     2,
		MemoryMB: 2048,
		Kernel:   config.Kernel{Image: "ghcr.io/example/kernel:6.6"},
		RootFS:   "ghcr.io/example/rootfs:v1",
		Pool:     config.PoolSettings{Size: size},
	})
}

// withDefaults applies the configuration defaults to one Profile.
func withDefaults(p config.Profile) config.Profile {
	cfg := &config.Config{
		Scheduler: config.Scheduler{Namespace: testNamespace},
		Profiles:  []config.Profile{p},
	}
	config.ApplyDefaults(cfg)
	return cfg.Profiles[0]
}

// specFor builds the PoolSpec of a Profile the way the Scheduler will, on
// the given Hosts.
func specFor(t *testing.T, p config.Profile, hosts ...string) poolmgr.PoolSpec {
	t.Helper()
	spec, err := poolmgr.NewSpecBuilder().Build(p, poolmgr.SpecInput{
		RunnerName: testRunner,
		Namespace:  testNamespace,
		Hosts:      hosts,
	})
	if err != nil {
		t.Fatalf("building spec for profile %q: %v", p.Name, err)
	}
	return spec
}

// events subscribes to every Pool and closes the stream with the test.
func (p *poolManager) events(c poolmgr.Client) poolmgr.EventStream {
	p.t.Helper()
	stream, err := c.Subscribe(p.ctx, poolmgr.EventFilter{})
	if err != nil {
		p.t.Fatalf("Subscribe: %v", err)
	}
	p.t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// awaitEvent reads from stream until an event of type typ arrives, and
// returns it.
func awaitEvent(t *testing.T, ctx context.Context, stream poolmgr.EventStream, typ poolmgr.EventType) *poolmgr.Event {
	t.Helper()
	for {
		event, err := stream.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if event.Type == typ {
			return event
		}
	}
}

// fillPool creates a Pool and waits until every one of its MicroVMs is
// available, so a following claim finds warm capacity.
func (p *poolManager) fillPool(c poolmgr.Client, spec poolmgr.PoolSpec) {
	p.t.Helper()
	stream := p.events(c)
	if _, err := c.CreatePool(p.ctx, spec); err != nil {
		p.t.Fatalf("CreatePool(%s): %v", spec.Ref, err)
	}
	for i := int32(0); i < spec.Size; i++ {
		awaitEvent(p.t, p.ctx, stream, poolmgrv1.EventType_VM_AVAILABLE)
	}
}

// vms returns the fake Pool Manager's own record of its MicroVMs.
func (p *poolManager) vms() []poolmgr.VMRecord { return p.pm.VMs() }

// leases returns the fake Pool Manager's own record of its Leases.
func (p *poolManager) leases() []poolmgr.LeaseRecord { return p.pm.Leases() }

// setFaults injects faults and clears them again with the test.
func (p *poolManager) setFaults(f poolmgr.Faults) {
	p.t.Helper()
	p.pm.SetFaults(f)
}

// gateAdmin wraps a PoolAdmin and fails every call with ErrUnavailable
// while it is closed, so that a test can take the Pool Manager away from a
// unit without stopping the fake and without moving any clock.
type gateAdmin struct {
	poolmgr.PoolAdmin
	// closed is read atomically through the channel returned by state.
	state chan bool
	// calls counts the calls that reached the wrapper.
	calls chan struct{}
}

func newGateAdmin(admin poolmgr.PoolAdmin, closed bool) *gateAdmin {
	g := &gateAdmin{PoolAdmin: admin, state: make(chan bool, 1), calls: make(chan struct{}, 1024)}
	g.state <- closed
	return g
}

// set opens or closes the gate.
func (g *gateAdmin) set(closed bool) {
	<-g.state
	g.state <- closed
}

// blocked reports the gate's state.
func (g *gateAdmin) blocked() bool {
	v := <-g.state
	g.state <- v
	return v
}

// ListPools implements poolmgr.PoolAdmin.
func (g *gateAdmin) ListPools(ctx context.Context, namespace string) ([]*poolmgr.Pool, error) {
	select {
	case g.calls <- struct{}{}:
	default:
	}
	if g.blocked() {
		return nil, fmt.Errorf("%w: gate closed", poolmgr.ErrUnavailable)
	}
	return g.PoolAdmin.ListPools(ctx, namespace)
}

// GetPool implements poolmgr.PoolAdmin.
func (g *gateAdmin) GetPool(ctx context.Context, r poolmgr.PoolRef) (*poolmgr.Pool, error) {
	if g.blocked() {
		return nil, fmt.Errorf("%w: gate closed", poolmgr.ErrUnavailable)
	}
	return g.PoolAdmin.GetPool(ctx, r)
}

// awaitCall waits for one call to reach the gate.
func (g *gateAdmin) awaitCall(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-g.calls:
	case <-ctx.Done():
		t.Fatalf("no call reached the pool manager: %v", ctx.Err())
	}
}

// trackerHarness is a PoolTracker with the two observation channels its
// configuration offers, so a test can wait for the Tracker to have applied
// something instead of polling its state.
type trackerHarness struct {
	*poolmgr.PoolTracker
	clk    *clock.Fake
	events chan *poolmgr.Event
	polls  chan poolmgr.PoolRef
}

// newTracker builds a Tracker over one client with a fake clock. It does
// not start it; call run.
func newTracker(t *testing.T, c poolmgr.Client, health poolmgr.Health, pollInterval time.Duration, log *slog.Logger) *trackerHarness {
	t.Helper()
	return newTrackerWith(t, c, c, health, pollInterval, log)
}

// newTrackerWith builds a Tracker whose Events and PoolAdmin come from
// different places, which is how a test takes the Events stream away
// without taking the Pool Manager away (PL-051).
func newTrackerWith(t *testing.T, events poolmgr.Events, admin poolmgr.PoolAdmin, health poolmgr.Health, pollInterval time.Duration, log *slog.Logger) *trackerHarness {
	t.Helper()
	h := &trackerHarness{
		clk:    clock.NewFake(testEpoch),
		events: make(chan *poolmgr.Event, 1024),
		polls:  make(chan poolmgr.PoolRef, 1024),
	}
	if log == nil {
		log = testLogger(t)
	}
	tracker, err := poolmgr.NewTracker(poolmgr.TrackerConfig{
		Events:       events,
		Admin:        admin,
		Health:       health,
		Clock:        h.clk,
		PollInterval: pollInterval,
		Log:          log,
		OnEvent: func(e *poolmgr.Event) {
			select {
			case h.events <- e:
			default:
			}
		},
		OnPoll: func(ref poolmgr.PoolRef, _ poolmgr.PoolStatus) {
			select {
			case h.polls <- ref:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	h.PoolTracker = tracker
	return h
}

// run starts the Tracker and stops it with the test.
func (h *trackerHarness) run(t *testing.T) {
	t.Helper()
	runInBackground(t, "Tracker.Run", h.Run)
}

// await blocks until cond holds, waking on every event and poll the Tracker
// applies. It fails the test when ctx ends first.
func (h *trackerHarness) await(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for {
		if cond() {
			return
		}
		select {
		case <-h.events:
		case <-h.polls:
		case <-ctx.Done():
			if cond() {
				return
			}
			t.Fatalf("timed out waiting for %s: %v", what, ctx.Err())
		}
	}
}

// awaitEvent blocks until an event the Tracker applied satisfies match.
func (h *trackerHarness) awaitEvent(t *testing.T, ctx context.Context, what string, match func(*poolmgr.Event) bool) *poolmgr.Event {
	t.Helper()
	for {
		select {
		case e := <-h.events:
			if match(e) {
				return e
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", what, ctx.Err())
		}
	}
}

// newHealth builds a HealthMonitor over a PoolAdmin with a fake clock and
// test-sized settings. fn adjusts the configuration.
func newHealth(t *testing.T, admin poolmgr.PoolAdmin, clk clock.Clock, fn func(*poolmgr.HealthConfig)) *poolmgr.HealthMonitor {
	t.Helper()
	cfg := poolmgr.HealthConfig{
		Admin:            admin,
		Namespace:        testNamespace,
		Clock:            clk,
		Backoff:          clock.Exponential{Base: time.Second, Max: 8 * time.Second},
		Interval:         healthInterval,
		FailureThreshold: 3,
		UnhealthyFor:     30 * time.Second,
		Log:              testLogger(t),
	}
	if fn != nil {
		fn(&cfg)
	}
	health, err := poolmgr.NewHealth(cfg)
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	return health
}

// healthInterval is the probe interval the tests configure. Nothing waits
// for it: the tests fire the health monitor's fake clock.
const healthInterval = 10 * time.Second

// awaitContact blocks until the health monitor has reached the Pool
// Manager once.
func awaitContact(t *testing.T, ctx context.Context, health poolmgr.Health) {
	t.Helper()
	select {
	case <-health.Contacted():
	case <-ctx.Done():
		t.Fatalf("the pool manager was never contacted: %v", ctx.Err())
	}
}

// recordingTracker is a Tracker that records what it was told, for the
// units whose contract is what they report to it.
type recordingTracker struct {
	mu        sync.Mutex
	tracked   map[poolmgr.PoolRef]bool
	dropped   []poolmgr.PoolRef
	exhausted []poolmgr.PoolRef
}

func newRecordingTracker() *recordingTracker {
	return &recordingTracker{tracked: make(map[poolmgr.PoolRef]bool)}
}

func (r *recordingTracker) Run(ctx context.Context) error { <-ctx.Done(); return nil }

func (r *recordingTracker) Track(pool poolmgr.PoolRef, declared bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tracked[pool] = declared
}

func (r *recordingTracker) Untrack(pool poolmgr.PoolRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tracked, pool)
	r.dropped = append(r.dropped, pool)
}

func (r *recordingTracker) Available(pool poolmgr.PoolRef) int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tracked[pool] {
		return 1
	}
	return 0
}

func (r *recordingTracker) MarkExhausted(pool poolmgr.PoolRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exhausted = append(r.exhausted, pool)
}

func (r *recordingTracker) Wait(poolmgr.PoolRef) <-chan struct{} { return make(chan struct{}) }

func (r *recordingTracker) Pools() []poolmgr.PoolAvailability { return nil }

// state reports what the Tracker was last told about a Pool.
func (r *recordingTracker) state(pool poolmgr.PoolRef) (declared, tracked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	declared, tracked = r.tracked[pool]
	return declared, tracked
}

// untracked is every Pool the Tracker was told to forget.
func (r *recordingTracker) untracked() []poolmgr.PoolRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]poolmgr.PoolRef(nil), r.dropped...)
}

// exhaustedPools is every Pool a claim reported exhausted.
func (r *recordingTracker) exhaustedPools() []poolmgr.PoolRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]poolmgr.PoolRef(nil), r.exhausted...)
}

// logRecorder captures what a unit logged, for the requirements whose
// response is a log line.
type logRecorder struct {
	t  *testing.T
	mu sync.Mutex
	// lines is every record written, as text.
	lines []string
}

func newLogRecorder(t *testing.T) *logRecorder { return &logRecorder{t: t} }

// logger is the slog.Logger to hand to the unit under test.
func (r *logRecorder) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Write implements io.Writer.
func (r *logRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.lines = append(r.lines, string(p))
	r.mu.Unlock()
	r.t.Logf("%s", p)
	return len(p), nil
}

// contains reports whether any record contains sub.
func (r *logRecorder) contains(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// containsAll reports whether one record contains every one of subs.
func (r *logRecorder) containsAll(subs ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.lines {
		matched := true
		for _, sub := range subs {
			if !strings.Contains(line, sub) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// atLevel reports whether one record at the given level contains every one
// of subs.
func (r *logRecorder) atLevel(level string, subs ...string) bool {
	return r.containsAll(append([]string{"level=" + level}, subs...)...)
}

// text is everything that was logged, for a failure message.
func (r *logRecorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "")
}

// runInBackground runs fn until the test ends, failing the test if it
// returns an error or does not stop when its context is cancelled.
func runInBackground(t *testing.T, name string, fn func(context.Context) error) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("%s returned %v", name, err)
			}
		case <-time.After(testTimeout):
			t.Errorf("%s did not stop", name)
		}
	}
	t.Cleanup(stop)
	return cancel
}

// templateOf is the MicroVM template of a spec, for assertions.
func templateOf(t *testing.T, spec poolmgr.PoolSpec) *types.MicroVMSpec {
	t.Helper()
	if spec.Template == nil {
		t.Fatal("spec has no template")
	}
	return spec.Template
}

// discard is an io.Writer that throws output away.
var _ io.Writer = (*testWriter)(nil)
