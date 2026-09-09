package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// ErrAlreadyRun is returned by a second call to Run on the same Scheduler.
var ErrAlreadyRun = errors.New("scheduler: Run called twice")

// runState is where the Scheduler is in its lifecycle. Reserve and Allocate
// read it to tell "not started yet" from "shutting down" (GL-070).
type runState int

const (
	// stateNew is before Run.
	stateNew runState = iota
	// stateRunning is while Run's context is live.
	stateRunning
	// stateStopping is from the cancellation of Run's context until Run
	// returns.
	stateStopping
	// stateStopped is after Run returned.
	stateStopped
)

// throttleWindow is the once-per-window period of the refusal (OB-003) and
// Pool exhaustion (OB-004) log lines.
const throttleWindow = time.Minute

// hostState is the mutable half of HostHealth.
type hostState struct {
	healthy     bool
	failures    int
	lastProbeAt time.Time
	info        *flintlock.HostInfo
	// infoLogged is what the Host reported the last time logHostInfo wrote a
	// line for it, so that an unchanging Host is logged once (HO-011).
	infoLogged string
}

//= docs/requirements/03-scheduler.md#capacity
//# The Scheduler SHALL be safe for concurrent use by the number of
//# workers the run loop starts.

// impl is the Scheduler. Every mutable field is guarded by mu, and no call to
// the Pool Manager, a Host or the Metrics sink is made while mu is held, so
// the run loop's workers can call Reserve, Allocate and Release concurrently
// without contending on a lock that is held across a network call.
type impl struct {
	deps    Deps
	set     Settings
	clk     clock.Clock
	backoff clock.Backoff
	log     *slog.Logger
	metrics Metrics

	ready     chan struct{}
	readyOnce sync.Once

	// bgCtx is the context of the heartbeat and release goroutines. It
	// outlives Run's context so that Leases are heartbeated during a
	// graceful shutdown (GL-071); Run cancels it last.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	// wg counts the heartbeat, release and retain goroutines. Run waits for
	// it before returning, so nothing outlives the Scheduler.
	wg sync.WaitGroup

	mu           sync.Mutex
	state        runState
	profiles     []*Profile
	reservations map[uint64]time.Time
	allocations  map[uint64]*handle
	// placements caches the Placement of a MicroVM by uid for the lifetime
	// of its Lease (SC-032).
	placements map[string]Placement
	// profileUse counts live Allocations per Profile name (SC-006).
	profileUse map[string]int
	// profileFree is closed and replaced whenever an Allocation ends, so a
	// caller waiting on a Profile's concurrency limit wakes at once.
	profileFree chan struct{}
	// waiting counts Allocations currently waiting on each Pool (OB-004).
	waiting map[poolmgr.PoolRef]int
	hosts   map[string]*hostState
	// drain is closed when the last Allocation ends, for the shutdown wait.
	drain    chan struct{}
	nextID   uint64
	bgClosed bool
	throttle map[string]time.Time
}

// Compile-time interface check.
var _ Scheduler = (*impl)(nil)

// New builds a Scheduler from deps and settings. Deps.Clock, Deps.Backoff,
// Deps.Metrics and Deps.Logger take documented defaults when nil; every other
// dependency is required. The returned Scheduler grants nothing until Run has
// started it and the Pool Manager has answered (SC-008).
func New(deps Deps, settings Settings) (Scheduler, error) {
	if err := checkDeps(deps); err != nil {
		return nil, err
	}
	if settings.Slots <= 0 {
		return nil, fmt.Errorf("scheduler: settings: slots is %d, want at least 1", settings.Slots)
	}
	s := &impl{
		deps:         deps,
		set:          settings,
		clk:          deps.Clock,
		backoff:      deps.Backoff,
		log:          deps.Logger,
		metrics:      deps.Metrics,
		ready:        make(chan struct{}),
		reservations: make(map[uint64]time.Time),
		allocations:  make(map[uint64]*handle),
		placements:   make(map[string]Placement),
		profileUse:   make(map[string]int),
		profileFree:  make(chan struct{}),
		waiting:      make(map[poolmgr.PoolRef]int),
		hosts:        make(map[string]*hostState),
		throttle:     make(map[string]time.Time),
	}
	if s.clk == nil {
		s.clk = clock.Real{}
	}
	if s.backoff == nil {
		s.backoff = clock.Exponential{Base: 500 * time.Millisecond, Max: 30 * time.Second}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.log = s.log.With("component", "scheduler")
	if s.metrics == nil {
		s.metrics = nopMetrics{}
	}
	s.profiles = resolveProfiles(settings.Profiles, settings.Namespace)
	s.resetHosts(settings.Inventory)
	return s, nil
}

// checkDeps reports the first required dependency that is missing.
func checkDeps(d Deps) error {
	missing := ""
	switch {
	case d.PoolManager == nil:
		missing = "PoolManager"
	case d.Specs == nil:
		missing = "Specs"
	case d.HostSelector == nil:
		missing = "HostSelector"
	case d.Declarer == nil:
		missing = "Declarer"
	case d.Tracker == nil:
		missing = "Tracker"
	case d.Health == nil:
		missing = "Health"
	case d.Hosts == nil:
		missing = "Hosts"
	}
	if missing != "" {
		return fmt.Errorf("scheduler: deps: %s is required", missing)
	}
	return nil
}

// resolveProfiles pairs each configured Profile with its PoolRef, applying
// the CF-028 defaults so that a Settings built as a literal in a test
// resolves the same Pool as one that went through config.ApplyDefaults.
func resolveProfiles(profiles []config.Profile, namespace string) []*Profile {
	out := make([]*Profile, 0, len(profiles))
	for _, p := range profiles {
		ref := poolmgr.PoolRef{Name: p.Pool.Name, Namespace: p.Pool.Namespace}
		if ref.Name == "" {
			ref.Name = p.Name
		}
		if ref.Namespace == "" {
			ref.Namespace = namespace
		}
		out = append(out, &Profile{Profile: p, PoolRef: ref})
	}
	return out
}

// resetHosts replaces the Host health table with one entry per Inventory
// Host, keeping the state of Hosts that are in both. A new Host starts
// healthy so that a Job placed on it before its first probe is not aborted.
func (s *impl) resetHosts(inventory []config.HostEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]*hostState, len(inventory))
	for _, h := range inventory {
		if old, ok := s.hosts[h.Name]; ok {
			next[h.Name] = old
			continue
		}
		next[h.Name] = &hostState{healthy: true}
	}
	s.hosts = next
}

// endpoints converts an Inventory into the Endpoints the Host Registry is
// dialled with (HO-010).
func endpoints(inventory []config.HostEntry) []flintlock.Endpoint {
	out := make([]flintlock.Endpoint, 0, len(inventory))
	for _, h := range inventory {
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

//= docs/requirements/03-scheduler.md#release
//# When starting, the Scheduler SHALL NOT attempt to find or clean up
//# MicroVMs from a previous incarnation, because Leases it no longer
//# heartbeats expire and the Pool Manager deletes them.

// Run declares one Pool per Profile, compares each Pool's Host list with the
// Inventory (HO-015), starts the Tracker, the Pool Manager health probe and
// the Host health probe, and blocks until ctx is cancelled. Startup lists no
// MicroVM and releases no Lease: a Lease held by a previous incarnation of
// this Runner is not heartbeated by this one, so the Pool Manager expires it
// and deletes its MicroVM without the Runner doing anything.
//
// On cancellation it releases every unconverted Reservation (GL-070), keeps
// heartbeating held Leases until they are released or the shutdown timeout
// elapses (GL-071), aborts whatever is left with ErrShutdown and releases
// their Leases (GL-072), and then returns.
func (s *impl) Run(ctx context.Context) error {
	if err := s.start(ctx); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.deps.Hosts.Apply(runCtx, endpoints(s.set.Inventory)); err != nil {
		s.log.Warn("host registry could not be built from the inventory", "error", err)
	}
	s.declareAll(runCtx)

	var loops sync.WaitGroup
	s.startLoop(&loops, func() { s.runTracker(runCtx) })
	s.startLoop(&loops, func() { s.runHealth(runCtx) })
	s.startLoop(&loops, func() { s.awaitContact(runCtx) })
	s.startLoop(&loops, func() { s.probeLoop(runCtx) })
	s.startLoop(&loops, func() { s.declareLoop(runCtx) })

	<-ctx.Done()
	s.setState(stateStopping)
	s.shutdown()

	cancel()
	loops.Wait()

	s.bgCancel()
	s.wg.Wait()
	s.setState(stateStopped)
	return nil
}

// start claims the run and builds the background context. It returns
// ErrAlreadyRun if the Scheduler has already been run.
func (s *impl) start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != stateNew {
		return ErrAlreadyRun
	}
	s.state = stateRunning
	s.bgCtx, s.bgCancel = context.WithCancel(context.WithoutCancel(ctx))
	return nil
}

// setState records a lifecycle transition.
func (s *impl) setState(st runState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
}

// startLoop runs fn in a goroutine counted by wg.
func (s *impl) startLoop(wg *sync.WaitGroup, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn()
	}()
}

// runTracker runs the capacity Tracker until ctx is cancelled (PL-050).
func (s *impl) runTracker(ctx context.Context) {
	if err := s.deps.Tracker.Run(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("capacity tracker stopped", "error", err)
	}
}

// runHealth runs the Pool Manager health probe until ctx is cancelled
// (PL-004, PL-005).
func (s *impl) runHealth(ctx context.Context) {
	if err := s.deps.Health.Run(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("pool manager health probe stopped", "error", err)
	}
}

// awaitContact closes Ready once the Pool Manager has answered (SC-008).
func (s *impl) awaitContact(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-s.deps.Health.Contacted():
		s.readyOnce.Do(func() { close(s.ready) })
		s.log.Info("pool manager contacted, reservations may now be granted")
	}
}

// Ready implements Lifecycle.
func (s *impl) Ready() <-chan struct{} { return s.ready }

//= docs/requirements/03-scheduler.md#capacity
//# When starting, the Scheduler SHALL NOT grant any Reservation until
//# it has successfully contacted the Pool Manager at least once.

// contacted reports whether the Pool Manager has answered at least once. It
// is the first thing Reserve checks, so no Reservation is granted before the
// Runner knows a Pool Manager is there at all.
func (s *impl) contacted() bool {
	select {
	case <-s.ready:
		return true
	default:
	}
	select {
	case <-s.deps.Health.Contacted():
		s.readyOnce.Do(func() { close(s.ready) })
		return true
	default:
		return false
	}
}

// Reload implements Lifecycle (CF-007).
func (s *impl) Reload(ctx context.Context, profiles []config.Profile, inventory []config.HostEntry) error {
	next := resolveProfiles(profiles, s.set.Namespace)

	s.mu.Lock()
	previous := s.profiles
	s.profiles = next
	s.set.Profiles = profiles
	s.set.Inventory = inventory
	s.mu.Unlock()

	for _, p := range previous {
		if findProfileByPool(next, p.PoolRef) != nil {
			continue
		}
		//= docs/requirements/04-pool-manager.md#pool-declaration
		//# When a Profile is removed by a configuration reload, the
		//# Scheduler SHALL NOT delete its Pool and SHALL log that the Pool is no
		//# longer referenced.
		s.log.Info("pool is no longer referenced by any profile and is left in place",
			"pool", p.PoolRef.String(), "profile", p.Name)
		s.deps.Tracker.Untrack(p.PoolRef)
	}

	s.resetHosts(inventory)
	if err := s.deps.Hosts.Apply(ctx, endpoints(inventory)); err != nil {
		return fmt.Errorf("scheduler: reload host registry: %w", err)
	}
	s.declareAll(ctx)
	return nil
}

// findProfileByPool returns the Profile bound to ref, or nil.
func findProfileByPool(profiles []*Profile, ref poolmgr.PoolRef) *Profile {
	for _, p := range profiles {
		if p.PoolRef == ref {
			return p
		}
	}
	return nil
}

// profileSnapshot copies the current Profile list.
func (s *impl) profileSnapshot() []*Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Profile(nil), s.profiles...)
}

// profileByName returns the resolved Profile of that name, or nil.
func (s *impl) profileByName(name string) *Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.profiles {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// inventorySnapshot copies the current Inventory.
func (s *impl) inventorySnapshot() []config.HostEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]config.HostEntry(nil), s.set.Inventory...)
}

// declareAll creates or updates the Pool of every Profile and tracks it
// (PL-010). A Pool that could not be declared is tracked as undeclared, so it
// counts as empty until a retry succeeds (PL-016).
func (s *impl) declareAll(ctx context.Context) {
	inventory := s.inventorySnapshot()
	for _, p := range s.profileSnapshot() {
		s.declare(ctx, p, inventory)
	}
}

// declare declares one Pool. A Pool that could not be declared is tracked as
// undeclared, which is what makes it count as empty until a retry succeeds.
func (s *impl) declare(ctx context.Context, p *Profile, inventory []config.HostEntry) {
	hosts := s.deps.HostSelector.Select(p.Profile, inventory)
	spec, err := s.deps.Specs.Build(p.Profile, poolmgr.SpecInput{
		RunnerName: s.set.RunnerName,
		Namespace:  s.set.Namespace,
		Hosts:      hosts,
	})
	if err != nil {
		s.log.Error("pool spec could not be built from the profile",
			"profile", p.Name, "pool", p.PoolRef.String(), "error", err)
		s.deps.Tracker.Track(p.PoolRef, false)
		return
	}
	spec.Ref = p.PoolRef
	pool, err := s.deps.Declarer.Declare(ctx, spec)
	if err != nil {
		//= docs/requirements/04-pool-manager.md#pool-declaration
		//# If `CreatePool` or `UpdatePool` fails for a Profile, then the
		//# Scheduler SHALL log the error, treat that Pool as empty and retry the
		//# declaration at the configured interval.
		s.log.Error("pool declaration failed, treating the pool as empty until the retry succeeds",
			"profile", p.Name, "pool", p.PoolRef.String(), "error", err)
		s.deps.Tracker.Track(p.PoolRef, false)
		return
	}
	s.deps.Tracker.Track(p.PoolRef, true)
	s.compareHosts(p, pool)

}

// compareHosts warns about a Pool Host that is not in the Inventory (HO-015).
func (s *impl) compareHosts(p *Profile, pool *poolmgr.Pool) {
	if pool == nil {
		return
	}
	for _, name := range pool.Spec.FlintlockHosts {
		if _, ok := s.deps.Hosts.Endpoint(name); ok {
			continue
		}
		//= docs/requirements/05-hosts.md#inventory
		//# When starting, the Runner SHALL compare the Inventory with the
		//# Hosts the Pool Manager reports for each Pool and SHALL log a warning for
		//# any Pool Host missing from the Inventory, because a MicroVM placed there
		//# could not be reached.
		s.log.Warn("pool host is not in the inventory; a microvm placed there could not be reached",
			"pool", p.PoolRef.String(), "host", name)
	}
}

// declareLoop retries the declaration of undeclared Pools at the configured
// interval (PL-016).
func (s *impl) declareLoop(ctx context.Context) {
	interval := s.set.PoolManager.DeclareRetryInterval
	if interval <= 0 {
		interval = config.DefaultPoolManagerDeclareRetry
	}
	timer := s.clk.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		}
		inventory := s.inventorySnapshot()
		for _, p := range s.profileSnapshot() {
			if s.poolDeclared(p.PoolRef) {
				continue
			}
			s.declare(ctx, p, inventory)
		}
		timer.Reset(interval)
	}
}

// poolDeclared reports whether the Tracker holds ref as declared.
func (s *impl) poolDeclared(ref poolmgr.PoolRef) bool {
	for _, pa := range s.deps.Tracker.Pools() {
		if pa.Pool == ref {
			return pa.Declared
		}
	}
	return false
}

// Snapshot implements Status (OB-031, OB-032).
func (s *impl) Snapshot() Snapshot {
	pools := s.deps.Tracker.Pools()
	healthy := s.deps.Health.Healthy()
	contacted := s.contacted()

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range pools {
		pools[i].Waiting = s.waiting[pools[i].Pool]
	}
	hosts := make([]HostHealth, 0, len(s.hosts))
	for name, st := range s.hosts {
		hosts = append(hosts, HostHealth{
			Name:                name,
			Healthy:             st.healthy,
			ConsecutiveFailures: st.failures,
			LastProbeAt:         st.lastProbeAt,
			Info:                st.info,
			Leased:              s.leasedOnLocked(name),
		})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	return Snapshot{
		PoolManagerContacted: contacted,
		PoolManagerHealthy:   healthy,
		Slots:                s.set.Slots,
		SlotsInUse:           s.slotsInUseLocked(),
		Pools:                pools,
		Hosts:                hosts,
	}
}

// leasedOnLocked counts the Allocations placed on a Host (OB-018).
func (s *impl) leasedOnLocked(host string) int {
	n := 0
	for _, h := range s.allocations {
		if h.placementHost() == host {
			n++
		}
	}
	return n
}

// throttled reports whether key has been logged within the throttle window,
// and records the time when it has not (OB-003, OB-004).
func (s *impl) throttled(key string) bool {
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.throttle[key]; ok && now.Sub(last) < throttleWindow {
		return true
	}
	s.throttle[key] = now
	return false
}

// unthrottle forgets a throttle key, so that the next line under it is
// logged whatever is left of the window.
func (s *impl) unthrottle(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.throttle, key)
}

// nopMetrics is the Metrics sink used when Deps.Metrics is nil.
type nopMetrics struct{}

// ReservationRefused implements Metrics.
func (nopMetrics) ReservationRefused(RefusalReason) {}

// AllocationObserved implements Metrics.
func (nopMetrics) AllocationObserved(string, time.Duration) {}

// PoolWaited implements Metrics.
func (nopMetrics) PoolWaited(poolmgr.PoolRef, time.Duration) {}

// PoolGauges implements Metrics.
func (nopMetrics) PoolGauges(poolmgr.PoolRef, poolmgr.PoolStatus, int32) {}

// HostGauges implements Metrics.
func (nopMetrics) HostGauges(string, bool, int) {}

// FailureCounted implements Metrics.
func (nopMetrics) FailureCounted(FailureKind) {}
