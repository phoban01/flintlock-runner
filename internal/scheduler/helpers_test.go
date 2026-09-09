package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	flfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	pmfake "github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
)

// Test support for the Scheduler: stubs for the poolmgr policy units that
// another work package implements, a Host Registry over the fake Host, and an
// env that wires them into a Scheduler on a fake clock.

const (
	testNamespace = "runner-ns"
	testRunner    = "runner-1"
	testTimeout   = 30 * time.Second
)

var testEpoch = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

// testContext is a context bounded by the test timeout.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------- Tracker

// stubTracker is a poolmgr.Tracker whose counts a test sets directly. The
// real one is built by the poolmgr work package against the same interface.
type stubTracker struct {
	mu        sync.Mutex
	order     []poolmgr.PoolRef
	pools     map[poolmgr.PoolRef]*poolmgr.PoolAvailability
	waits     map[poolmgr.PoolRef]chan struct{}
	exhausted map[poolmgr.PoolRef]int
}

func newStubTracker() *stubTracker {
	return &stubTracker{
		pools:     make(map[poolmgr.PoolRef]*poolmgr.PoolAvailability),
		waits:     make(map[poolmgr.PoolRef]chan struct{}),
		exhausted: make(map[poolmgr.PoolRef]int),
	}
}

func (t *stubTracker) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (t *stubTracker) Track(ref poolmgr.PoolRef, declared bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entryLocked(ref).Declared = declared
}

func (t *stubTracker) Untrack(ref poolmgr.PoolRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pools, ref)
	for i, r := range t.order {
		if r == ref {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

func (t *stubTracker) Available(ref poolmgr.PoolRef) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.entryLocked(ref).Status.Available
}

func (t *stubTracker) MarkExhausted(ref poolmgr.PoolRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entryLocked(ref).Status.Available = 0
	t.exhausted[ref]++
}

func (t *stubTracker) Wait(ref poolmgr.PoolRef) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch, ok := t.waits[ref]
	if !ok {
		ch = make(chan struct{})
		t.waits[ref] = ch
	}
	return ch
}

func (t *stubTracker) Pools() []poolmgr.PoolAvailability {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]poolmgr.PoolAvailability, 0, len(t.pools))
	for _, ref := range t.order {
		if pa, ok := t.pools[ref]; ok {
			out = append(out, *pa)
		}
	}
	return out
}

// entryLocked returns the Pool's entry, creating it.
func (t *stubTracker) entryLocked(ref poolmgr.PoolRef) *poolmgr.PoolAvailability {
	pa, ok := t.pools[ref]
	if !ok {
		pa = &poolmgr.PoolAvailability{Pool: ref, Declared: true}
		t.pools[ref] = pa
		t.order = append(t.order, ref)
	}
	return pa
}

// setAvailable sets a Pool's available count without waking waiters.
func (t *stubTracker) setAvailable(ref poolmgr.PoolRef, n int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entryLocked(ref).Status.Available = n
}

// setStatus sets a Pool's whole status.
func (t *stubTracker) setStatus(ref poolmgr.PoolRef, status poolmgr.PoolStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entryLocked(ref).Status = status
}

// wake is the event that reports a MicroVM in the Pool becoming available.
func (t *stubTracker) wake(ref poolmgr.PoolRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ch, ok := t.waits[ref]; ok {
		close(ch)
		delete(t.waits, ref)
	}
}

// exhaustedCount is how often MarkExhausted was called for a Pool.
func (t *stubTracker) exhaustedCount(ref poolmgr.PoolRef) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exhausted[ref]
}

// ----------------------------------------------------------------- Health

// stubHealth is a poolmgr.Health a test drives directly.
type stubHealth struct {
	mu          sync.Mutex
	healthy     bool
	unavailable int
	contacted   chan struct{}
	once        sync.Once
}

func newStubHealth() *stubHealth {
	return &stubHealth{healthy: true, contacted: make(chan struct{})}
}

func (h *stubHealth) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (h *stubHealth) Contacted() <-chan struct{} { return h.contacted }

func (h *stubHealth) Healthy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy
}

func (h *stubHealth) MarkUnavailable() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unavailable++
	h.healthy = false
}

// contact is the first successful Pool Manager call.
func (h *stubHealth) contact() { h.once.Do(func() { close(h.contacted) }) }

func (h *stubHealth) setHealthy(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.healthy = v
}

func (h *stubHealth) unavailableCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unavailable
}

// --------------------------------------------------- Specs, hosts, declarer

// stubSpecs is a poolmgr.SpecBuilder that builds the smallest valid spec.
type stubSpecs struct {
	err error
}

func (b stubSpecs) Build(p config.Profile, in poolmgr.SpecInput) (poolmgr.PoolSpec, error) {
	if b.err != nil {
		return poolmgr.PoolSpec{}, b.err
	}
	return poolmgr.PoolSpec{
		Template: &types.MicroVMSpec{
			Namespace:  in.Namespace,
			Vcpu:       int32(p.VCPU),
			MemoryInMb: int32(p.MemoryMB),
		},
		Size:           int32(p.Pool.Size),
		FlintlockHosts: in.Hosts,
		Replenishment: poolmgr.ReplenishmentStrategy{
			Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE,
		},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatInterval:        p.Pool.HeartbeatInterval,
		HeartbeatExpiryThreshold: p.Pool.HeartbeatExpiry,
	}, nil
}

// stubSelector is a poolmgr.HostSelector that keeps every Inventory Host of
// the Profile's architecture.
type stubSelector struct{}

func (stubSelector) Select(p config.Profile, inventory []config.HostEntry) []string {
	out := make([]string, 0, len(inventory))
	for _, h := range inventory {
		if p.Arch != "" && h.Arch != "" && p.Arch != h.Arch {
			continue
		}
		out = append(out, h.Name)
	}
	return out
}

// stubDeclarer declares Pools at a poolmgr.PoolAdmin, which is either the
// fake Pool Manager or nothing at all, in which case it answers from the spec.
type stubDeclarer struct {
	admin poolmgr.PoolAdmin

	mu    sync.Mutex
	err   error
	specs []poolmgr.PoolSpec
}

func (d *stubDeclarer) Declare(ctx context.Context, spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	d.mu.Lock()
	d.specs = append(d.specs, spec)
	err := d.err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if d.admin == nil {
		return &poolmgr.Pool{Spec: spec}, nil
	}
	pool, createErr := d.admin.CreatePool(ctx, spec)
	if errors.Is(createErr, poolmgr.ErrAlreadyExists) {
		return d.admin.UpdatePool(ctx, spec)
	}
	if createErr != nil {
		return nil, createErr
	}
	return pool, nil
}

func (d *stubDeclarer) declared() []poolmgr.PoolSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]poolmgr.PoolSpec(nil), d.specs...)
}

func (d *stubDeclarer) setErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

// --------------------------------------------------------------- Registry

// testRegistry is a flintlock.Registry over clients a test supplies. The real
// one is built by the hosts work package against the same interface.
type testRegistry struct {
	mu      sync.Mutex
	clients map[string]flintlock.HostClient
	eps     map[string]flintlock.Endpoint
	names   []string
}

func newTestRegistry() *testRegistry {
	return &testRegistry{
		clients: make(map[string]flintlock.HostClient),
		eps:     make(map[string]flintlock.Endpoint),
	}
}

// add registers a Host under a name and endpoint address.
func (r *testRegistry) add(name, address string, client flintlock.HostClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[name] = client
	r.eps[name] = flintlock.Endpoint{Name: name, Address: address}
	r.names = append(r.names, name)
	sort.Strings(r.names)
}

func (r *testRegistry) Get(name string) (flintlock.HostClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.clients[name]
	if !ok {
		return nil, fmt.Errorf("%q: %w", name, flintlock.ErrUnknownHost)
	}
	return c, nil
}

func (r *testRegistry) Endpoint(name string) (flintlock.Endpoint, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, ok := r.eps[name]
	return ep, ok
}

func (r *testRegistry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

func (r *testRegistry) Apply(_ context.Context, eps []flintlock.Endpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(eps))
	for _, ep := range eps {
		if _, ok := r.clients[ep.Name]; !ok {
			continue
		}
		r.eps[ep.Name] = ep
		names = append(names, ep.Name)
	}
	sort.Strings(names)
	r.names = names
	return nil
}

func (r *testRegistry) Close() error { return nil }

// countingHost counts the calls the Placement fan-out and the health probe
// make on a Host.
type countingHost struct {
	flintlock.HostClient

	mu         sync.Mutex
	getCalls   int
	listCalls  int
	infoCalls  int
	getErr     error
	infoErr    error
	listErr    error
	microvm    *types.MicroVM
	serverInfo *flintlock.HostInfo
}

func newCountingHost(name string) *countingHost {
	return &countingHost{
		serverInfo: &flintlock.HostInfo{
			Name: name, VersionKnown: true, Version: "test",
			Exec: flintlock.GuestService{Enabled: true},
		},
		getErr: fmt.Errorf("%w: microvm", flintlock.ErrNotFound),
	}
}

func (c *countingHost) Name() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo.Name
}

func (c *countingHost) ServerInfo(context.Context) (*flintlock.HostInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.infoCalls++
	if c.infoErr != nil {
		return nil, c.infoErr
	}
	return c.serverInfo, nil
}

func (c *countingHost) GetMicroVM(_ context.Context, _ string) (*types.MicroVM, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCalls++
	if c.microvm != nil {
		return c.microvm, nil
	}
	return nil, c.getErr
}

func (c *countingHost) ListMicroVMs(context.Context, string) ([]*types.MicroVM, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCalls++
	if c.listErr != nil {
		return nil, c.listErr
	}
	return nil, nil
}

func (c *countingHost) Close() error { return nil }

// set mutates the Host under its lock.
func (c *countingHost) set(fn func(*countingHost)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// counts reports the calls made so far.
func (c *countingHost) counts() (get, list, info int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getCalls, c.listCalls, c.infoCalls
}

// holds makes the Host answer GetMicroVM with a MicroVM of that uid.
func (c *countingHost) holds(uid string) {
	c.set(func(h *countingHost) { h.microvm = &types.MicroVM{Spec: &types.MicroVMSpec{Uid: &uid}} })
}

// ----------------------------------------------------------- Pool Manager

// stubClient is a poolmgr.Client whose every call a test scripts. Only the
// calls the Scheduler makes are implemented.
type stubClient struct {
	mu         sync.Mutex
	claimFn    func(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error)
	beatFn     func(ctx context.Context, leaseID string) (time.Time, error)
	releaseFn  func(ctx context.Context, leaseID string) error
	getPoolFn  func(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Pool, error)
	claims     int
	beats      int
	releases   []string
	getPools   int
	subscribed int
	// releasedCh announces every release, so a test waits for one instead of
	// polling.
	releasedCh chan string
}

// newStubClient is a scripted Pool Manager client.
func newStubClient() *stubClient {
	return &stubClient{releasedCh: make(chan string, 64)}
}

func (c *stubClient) CreatePool(context.Context, poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	return &poolmgr.Pool{}, nil
}

func (c *stubClient) UpdatePool(context.Context, poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	return &poolmgr.Pool{}, nil
}

func (c *stubClient) DeletePool(context.Context, poolmgr.PoolRef) error { return nil }

func (c *stubClient) GetPool(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Pool, error) {
	c.mu.Lock()
	c.getPools++
	fn := c.getPoolFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, ref)
	}
	return &poolmgr.Pool{Spec: poolmgr.PoolSpec{Ref: ref}}, nil
}

func (c *stubClient) ListPools(context.Context, string) ([]*poolmgr.Pool, error) { return nil, nil }

func (c *stubClient) ClaimVM(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
	c.mu.Lock()
	c.claims++
	fn := c.claimFn
	c.mu.Unlock()
	if fn == nil {
		return nil, poolmgr.ErrExhausted
	}
	return fn(ctx, ref)
}

func (c *stubClient) Heartbeat(ctx context.Context, leaseID string) (time.Time, error) {
	c.mu.Lock()
	c.beats++
	fn := c.beatFn
	c.mu.Unlock()
	if fn == nil {
		return time.Time{}, poolmgr.ErrNotFound
	}
	return fn(ctx, leaseID)
}

func (c *stubClient) ReleaseVM(ctx context.Context, leaseID string) error {
	c.mu.Lock()
	c.releases = append(c.releases, leaseID)
	fn := c.releaseFn
	ch := c.releasedCh
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- leaseID:
		default:
		}
	}
	if fn == nil {
		return nil
	}
	return fn(ctx, leaseID)
}

func (c *stubClient) Subscribe(context.Context, poolmgr.EventFilter) (poolmgr.EventStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscribed++
	return nil, poolmgr.ErrUnavailable
}

func (c *stubClient) Close() error { return nil }

func (c *stubClient) claimCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims
}

func (c *stubClient) beatCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.beats
}

func (c *stubClient) releaseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.releases)
}

func (c *stubClient) releasedLeases() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.releases...)
}

func (c *stubClient) script(fn func(*stubClient)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// countingClient counts the calls the Scheduler makes on a real Pool Manager
// client, which is how a test proves a call was not made.
type countingClient struct {
	poolmgr.Client

	mu         sync.Mutex
	releases   []string
	claims     int
	releasedCh chan string
}

// newCountingClient wraps a real Pool Manager client with call counters.
func newCountingClient(c poolmgr.Client) *countingClient {
	return &countingClient{Client: c, releasedCh: make(chan string, 64)}
}

// awaitRelease blocks until the client has made a release call.
func (c *countingClient) awaitRelease(t *testing.T, ctx context.Context) string {
	t.Helper()
	select {
	case id := <-c.releasedCh:
		return id
	case <-ctx.Done():
		t.Fatal("waiting for a release call")
		return ""
	}
}

func (c *countingClient) ClaimVM(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
	c.mu.Lock()
	c.claims++
	c.mu.Unlock()
	return c.Client.ClaimVM(ctx, ref)
}

func (c *countingClient) ReleaseVM(ctx context.Context, leaseID string) error {
	c.mu.Lock()
	c.releases = append(c.releases, leaseID)
	ch := c.releasedCh
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- leaseID:
		default:
		}
	}
	return c.Client.ReleaseVM(ctx, leaseID)
}

func (c *countingClient) releaseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.releases)
}

func (c *countingClient) claimCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims
}

// ---------------------------------------------------------------- Metrics

// recorder is a Metrics that keeps what it was told.
type recorder struct {
	mu          sync.Mutex
	refusals    map[RefusalReason]int
	failures    map[FailureKind]int
	allocations []time.Duration
	waited      map[poolmgr.PoolRef][]time.Duration
	poolGauges  map[poolmgr.PoolRef]poolmgr.PoolStatus
	hostGauges  map[string]bool
}

func newRecorder() *recorder {
	return &recorder{
		refusals:   make(map[RefusalReason]int),
		failures:   make(map[FailureKind]int),
		waited:     make(map[poolmgr.PoolRef][]time.Duration),
		poolGauges: make(map[poolmgr.PoolRef]poolmgr.PoolStatus),
		hostGauges: make(map[string]bool),
	}
}

func (r *recorder) ReservationRefused(reason RefusalReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusals[reason]++
}

func (r *recorder) AllocationObserved(_ string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allocations = append(r.allocations, d)
}

func (r *recorder) PoolWaited(pool poolmgr.PoolRef, waited time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waited[pool] = append(r.waited[pool], waited)
}

func (r *recorder) PoolGauges(pool poolmgr.PoolRef, status poolmgr.PoolStatus, _ int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolGauges[pool] = status
}

func (r *recorder) HostGauges(host string, healthy bool, _ int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hostGauges[host] = healthy
}

func (r *recorder) FailureCounted(kind FailureKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[kind]++
}

func (r *recorder) refusal(reason RefusalReason) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusals[reason]
}

func (r *recorder) failure(kind FailureKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures[kind]
}

func (r *recorder) waitedOn(pool poolmgr.PoolRef) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.waited[pool]...)
}

// ------------------------------------------------------------------- Logs

// logRecorder is a slog.Handler that keeps every record.
type logRecorder struct {
	mu      sync.Mutex
	records []logRecord
}

// logRecord is one captured record, attributes flattened.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: make(map[string]string)}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, rec)
	return nil
}

func (l *logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &prefixHandler{parent: l, attrs: attrs}
}

func (l *logRecorder) WithGroup(string) slog.Handler { return l }

// prefixHandler carries the attributes of a With-derived logger.
type prefixHandler struct {
	parent *logRecorder
	attrs  []slog.Attr
}

func (h *prefixHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *prefixHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(h.attrs...)
	return h.parent.Handle(ctx, r)
}

func (h *prefixHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &prefixHandler{parent: h.parent, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *prefixHandler) WithGroup(string) slog.Handler { return h }

// find returns the records whose message contains sub.
func (l *logRecorder) find(sub string) []logRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []logRecord
	for _, r := range l.records {
		if contains(r.msg, sub) {
			out = append(out, r)
		}
	}
	return out
}

// contains is strings.Contains without the import in every file.
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------- env

// envConfig shapes the Scheduler an env builds.
type envConfig struct {
	client    poolmgr.Client
	profiles  []config.Profile
	inventory []config.HostEntry
	hosts     map[string]flintlock.HostClient
	backoff   clock.Backoff
	tune      func(*Settings)
	specsErr  error
}

// env is a Scheduler with every dependency a test can reach.
type env struct {
	t        *testing.T
	clk      *clock.Fake
	tracker  *stubTracker
	health   *stubHealth
	registry *testRegistry
	declarer *stubDeclarer
	metrics  *recorder
	logs     *logRecorder
	client   poolmgr.Client
	sched    *impl

	runDone chan error
	cancel  context.CancelFunc
}

// newEnv builds a Scheduler from cfg, with one Profile and one Host by
// default. Nothing is started; startBare or run does that.
func newEnv(t *testing.T, cfg envConfig) *env {
	t.Helper()
	if cfg.profiles == nil {
		cfg.profiles = []config.Profile{testProfile("default", 1)}
	}
	if cfg.inventory == nil {
		cfg.inventory = []config.HostEntry{testHostEntry("host-1")}
	}
	if cfg.client == nil {
		cfg.client = newStubClient()
	}
	if cfg.backoff == nil {
		cfg.backoff = clock.Exponential{Base: time.Minute}
	}

	e := &env{
		t:        t,
		clk:      clock.NewFake(testEpoch),
		tracker:  newStubTracker(),
		health:   newStubHealth(),
		registry: newTestRegistry(),
		metrics:  newRecorder(),
		logs:     &logRecorder{},
		client:   cfg.client,
	}
	e.declarer = &stubDeclarer{}
	for name, client := range cfg.hosts {
		e.registry.add(name, name+":9090", client)
	}
	for _, h := range cfg.inventory {
		if _, ok := cfg.hosts[h.Name]; ok {
			continue
		}
		e.registry.add(h.Name, h.Endpoint, newCountingHost(h.Name))
	}

	settings := Settings{
		RunnerName:      testRunner,
		Namespace:       testNamespace,
		Slots:           4,
		ShutdownTimeout: 30 * time.Second,
		Scheduler: config.Scheduler{
			Namespace:              testNamespace,
			AllocationTimeout:      time.Minute,
			HostHealthInterval:     10 * time.Second,
			HostUnhealthyThreshold: 2,
			HostCallDeadline:       time.Second,
		},
		PoolManager: config.PoolManager{
			Deadline:             time.Second,
			ReleaseRetryLimit:    2,
			DeclareRetryInterval: 30 * time.Second,
		},
		Profiles:  cfg.profiles,
		Inventory: cfg.inventory,
	}
	if cfg.tune != nil {
		cfg.tune(&settings)
	}

	sched, err := New(Deps{
		PoolManager:  cfg.client,
		Specs:        stubSpecs{err: cfg.specsErr},
		HostSelector: stubSelector{},
		Declarer:     e.declarer,
		Tracker:      e.tracker,
		Health:       e.health,
		Hosts:        e.registry,
		Clock:        e.clk,
		Backoff:      cfg.backoff,
		Metrics:      e.metrics,
		Logger:       slog.New(e.logs),
	}, settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.sched = sched.(*impl)
	for _, p := range e.sched.profiles {
		e.tracker.Track(p.PoolRef, true)
	}
	return e
}

// startBare puts the Scheduler in the running state and marks the Pool
// Manager contacted without starting Run's loops, so that the only timers on
// the fake clock are the ones the test is about.
func (e *env) startBare(ctx context.Context) {
	e.t.Helper()
	if err := e.sched.start(ctx); err != nil {
		e.t.Fatalf("start: %v", err)
	}
	e.health.contact()
	e.t.Cleanup(func() {
		e.sched.closeBackground()
		e.sched.bgCancel()
		e.sched.wg.Wait()
	})
}

// run starts the whole Scheduler and returns a function that shuts it down
// and waits for Run to return.
func (e *env) run(ctx context.Context) func() {
	e.t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.runDone = make(chan error, 1)
	go func() { e.runDone <- e.sched.Run(runCtx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-e.runDone:
				if err != nil {
					e.t.Errorf("Run: %v", err)
				}
			case <-ctx.Done():
				e.t.Error("Run did not return")
			}
		})
	}
	e.t.Cleanup(stop)
	waitFor(e.t, ctx, func() bool { return e.sched.runningState() == stateRunning })
	return stop
}

// poolOf is the PoolRef of a named Profile.
func (e *env) poolOf(profile string) poolmgr.PoolRef {
	e.t.Helper()
	p := e.sched.profileByName(profile)
	if p == nil {
		e.t.Fatalf("no profile %q", profile)
	}
	return p.PoolRef
}

// profile is the resolved Profile of that name.
func (e *env) profile(name string) *Profile {
	e.t.Helper()
	p := e.sched.profileByName(name)
	if p == nil {
		e.t.Fatalf("no profile %q", name)
	}
	return p
}

// reserve grants a Reservation or fails the test.
func (e *env) reserve(ctx context.Context) *Reservation {
	e.t.Helper()
	r, err := e.sched.Reserve(ctx)
	if err != nil {
		e.t.Fatalf("Reserve: %v", err)
	}
	return r
}

// allocate reserves and allocates, or fails the test.
func (e *env) allocate(ctx context.Context, job JobInfo, profile string) Handle {
	e.t.Helper()
	r := e.reserve(ctx)
	h, err := e.sched.Allocate(ctx, r, job, e.profile(profile))
	if err != nil {
		e.t.Fatalf("Allocate: %v", err)
	}
	return h
}

// advance moves the fake clock after waiting for n timers to be armed, so a
// test never fires a timer that has not been set yet.
func (e *env) advance(ctx context.Context, n int, d time.Duration) {
	e.t.Helper()
	if err := e.clk.BlockUntil(ctx, n); err != nil {
		e.t.Fatalf("waiting for %d timers: %v", n, err)
	}
	e.clk.Advance(d)
}

// waitFor blocks until cond is true or the test's context ends. It is how a
// test waits for a background goroutine to have made a change that has no
// channel of its own; the timing is bounded by the context, not by a sleep.
func waitFor(t *testing.T, ctx context.Context, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatal("the condition was not met before the test timeout")
		case <-time.After(time.Millisecond):
		}
	}
}

// ------------------------------------------------------------- fixtures

// testProfile is a Profile with a Pool of that size.
func testProfile(name string, size int) config.Profile {
	return config.Profile{
		Name:     name,
		Arch:     config.ArchARM64,
		VCPU:     2,
		MemoryMB: 1024,
		Default:  name == "default",
		Pool: config.PoolSettings{
			Size:              size,
			Name:              name,
			Namespace:         testNamespace,
			HeartbeatInterval: 10 * time.Second,
			HeartbeatExpiry:   30 * time.Second,
		},
	}
}

// testHostEntry is an Inventory entry for a Host.
func testHostEntry(name string) config.HostEntry {
	return config.HostEntry{
		Name:     name,
		Endpoint: name + ":9090",
		Arch:     config.ArchARM64,
		VCPU:     32,
		MemoryMB: 65536,
	}
}

// newFakeHost is a fake flintlock Host with an in-memory client.
func newFakeHost(t *testing.T, name string) *flfake.Host {
	t.Helper()
	h := flfake.New(flintlock.FakeHostConfig{
		Name:        name,
		SandboxRoot: t.TempDir(),
		Version:     "v0.0.0-test",
		ExecEnabled: true,
	})
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// runnerClient is the Runner-side client of a fake Host. It comes from the
// fake Dialer, which hides the admin methods exactly as the real Runner-side
// client does, so a test that holds one cannot create or delete a MicroVM
// (HO-007).
func runnerClient(t *testing.T, h *flfake.Host) flintlock.HostClient {
	t.Helper()
	c, err := flfake.NewDialer(h).Dial(context.Background(), flintlock.Endpoint{Name: h.Config().Name})
	if err != nil {
		t.Fatalf("dialling the fake host %q: %v", h.Config().Name, err)
	}
	return c
}

// newFakePoolManager starts a fake Pool Manager over fake Hosts and returns
// it with a client. Its clock is its own, so advancing the Scheduler's clock
// does not move the Pool Manager's expiry sweeper.
func newFakePoolManager(t *testing.T, ctx context.Context, hosts map[string]flintlock.PoolHostClient) (*pmfake.PoolManager, poolmgr.Client) {
	t.Helper()
	return newFakePoolManagerWith(t, ctx, hosts, nil)
}

// newFakePoolManagerWith is newFakePoolManager with the fake's configuration
// adjusted, for the SC-031 path where the claim response names no Host.
func newFakePoolManagerWith(
	t *testing.T,
	ctx context.Context,
	hosts map[string]flintlock.PoolHostClient,
	tweak func(*poolmgr.FakeConfig),
) (*pmfake.PoolManager, poolmgr.Client) {
	t.Helper()
	source := pmfake.NewHosts()
	for name, client := range hosts {
		source.Add(name, client, name+":9090")
	}
	cfg := poolmgr.FakeConfig{
		Hosts:             source,
		Clock:             clock.NewFake(testEpoch),
		ReconcileInterval: time.Second,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	pm := pmfake.New(cfg)
	done := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	go func() { done <- pm.Run(runCtx) }()
	client := pm.Client()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = client.Close()
	})
	return pm, client
}

// claimsFrom is a claim function handing out unique lease and MicroVM ids on
// a named Host.
func claimsFrom(host string) func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
	var n atomic.Int64
	return func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
		i := n.Add(1)
		return &poolmgr.Claim{
			LeaseID: fmt.Sprintf("lease-%d", i),
			VMUID:   fmt.Sprintf("vm-%d", i),
			Host:    poolmgr.HostRef{Name: host, Address: host + ":9090"},
		}, nil
	}
}

// beatsFor is a heartbeat function that extends the Lease by d on clk.
func beatsFor(clk *clock.Fake, d time.Duration) func(context.Context, string) (time.Time, error) {
	return func(context.Context, string) (time.Time, error) { return clk.Now().Add(d), nil }
}

// waitForClaimable blocks until the Pool has an available warm MicroVM.
func waitForClaimable(t *testing.T, ctx context.Context, client poolmgr.Client, ref poolmgr.PoolRef) {
	t.Helper()
	events, err := client.Subscribe(ctx, poolmgr.EventFilter{Pool: &ref})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = events.Close() }()
	for {
		pool, err := client.GetPool(ctx, ref)
		if err == nil && pool.Status.Available > 0 {
			return
		}
		if _, err := events.Recv(ctx); err != nil {
			t.Fatalf("waiting for an available microvm in %s: %v", ref, err)
		}
	}
}
