package poolmgr

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// defaultEventsPollInterval is how often GetPool is polled, and a new
// subscription attempted, while the Events stream is unavailable (PL-051).
const defaultEventsPollInterval = 5 * time.Second

// TrackerConfig is what NewTracker needs. Events and Admin are required.
type TrackerConfig struct {
	// Events is the Pool Manager's Events service, the primary source of
	// every Pool's available count (PL-050).
	Events Events
	// Admin is the PoolAdmin service, used to poll GetPool while the stream
	// is unavailable and to resynchronise after a re-subscription (PL-051).
	Admin PoolAdmin
	// Health, when set, zeroes every Pool's available count while the Pool
	// Manager is unhealthy (PL-035).
	Health Health
	// Clock paces the polling.
	Clock clock.Clock
	// PollInterval is the GetPool poll interval while the stream is down
	// (PL-051).
	PollInterval time.Duration
	// Log receives the Pool warnings of PL-055 and PL-056.
	Log *slog.Logger
	// Observer, when set, is called with every event after it has been
	// applied. It is the seam a test synchronises on and a caller can use
	// to react to Pool events without a second subscription; it runs on the
	// Tracker's own goroutine, so it must not block.
	Observer func(*Event)
}

// PoolTracker tracks how many warm MicroVMs each Pool has, from the Pool
// Manager's Events stream with GetPool as the fallback (PL-050 to PL-056).
// It is the input to the Scheduler's capacity computation (SC-002) and the
// wake-up source for a claim waiting on an exhausted Pool (SC-021). It is
// safe for concurrent use.
type PoolTracker struct {
	cfg TrackerConfig
	log *slog.Logger

	// kick asks the run loop for an immediate poll, after a Pool is
	// tracked.
	kick chan struct{}

	mu    sync.Mutex
	pools map[PoolRef]*trackedPool
}

// trackedPool is one Pool's tracked state.
type trackedPool struct {
	status PoolStatus
	// declared is false while the Pool's declaration is failing (PL-016).
	declared bool
	// exhausted records a RESOURCE_EXHAUSTED claim and holds the available
	// count at zero until the next event or poll (PL-053).
	exhausted bool
	// waiters is closed when a MicroVM in this Pool becomes available
	// (SC-021); waiting counts the callers currently waiting on it.
	waiters chan struct{}
	waiting int
}

// NewTracker builds the Tracker.
func NewTracker(cfg TrackerConfig) (*PoolTracker, error) {
	switch {
	case cfg.Events == nil:
		return nil, errors.New("poolmgr: tracker needs an Events client")
	case cfg.Admin == nil:
		return nil, errors.New("poolmgr: tracker needs a PoolAdmin client")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultEventsPollInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &PoolTracker{
		cfg:   cfg,
		log:   cfg.Log.With("component", "poolmgr-tracker"),
		kick:  make(chan struct{}, 1),
		pools: make(map[PoolRef]*trackedPool),
	}, nil
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//# The Scheduler SHALL track the number of available warm MicroVMs in every
//# Pool by subscribing to the `Events` service.

//= docs/requirements/04-pool-manager.md#capacity-tracking
//# When the `Events` stream is unavailable, the Scheduler SHALL fall back
//# to polling `GetPool` at the configured interval until the stream can be
//# re-established.

// Run subscribes to the Events service and keeps every tracked Pool's
// counts up to date until ctx is cancelled. While no subscription can be
// held -- the Pool Manager refused Subscribe, or the stream was dropped --
// it polls GetPool for every tracked Pool at the configured interval and
// tries to subscribe again on each of those ticks, so a Pool's availability
// keeps being tracked across a Pool Manager restart rather than freezing at
// the last event. Every subscription, including a re-subscription, starts
// with a poll, because the events that arrived while the stream was down
// are lost and the counts have to be resynchronised from the Pool Manager.
func (t *PoolTracker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		stream, err := t.cfg.Events.Subscribe(ctx, EventFilter{})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			t.log.Warn("events stream unavailable; polling GetPool", "interval", t.cfg.PollInterval, "error", err)
			t.pollAll(ctx)
			if werr := t.wait(ctx, t.cfg.PollInterval); werr != nil {
				return nil
			}
			continue
		}
		t.pollAll(ctx)
		t.readStream(ctx, stream)
	}
	return nil
}

// readStream applies events until the stream ends. It always closes the
// stream, so the client's reader goroutine goes away with it.
func (t *PoolTracker) readStream(ctx context.Context, stream EventStream) {
	defer func() { _ = stream.Close() }()
	for {
		event, err := stream.Recv(ctx)
		if err != nil {
			if ctx.Err() == nil {
				t.log.Warn("events stream ended; falling back to polling", "error", err)
			}
			return
		}
		t.apply(event)
	}
}

// wait blocks for d on the clock, returning early when the Tracker is
// kicked. It returns the context's error when ctx ends first.
func (t *PoolTracker) wait(ctx context.Context, d time.Duration) error {
	timer := t.cfg.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.kick:
		return nil
	case <-timer.C():
		return nil
	}
}

// pollAll refreshes every tracked Pool from GetPool. A Pool the Pool
// Manager does not know is marked undeclared, so that it counts as empty
// until the declaration is retried (PL-016).
func (t *PoolTracker) pollAll(ctx context.Context) {
	for _, ref := range t.refs() {
		if ctx.Err() != nil {
			return
		}
		pool, err := t.cfg.Admin.GetPool(ctx, ref)
		switch {
		case err == nil:
			t.setStatus(ref, pool.Status)
		case errors.Is(err, ErrNotFound):
			t.log.Warn("pool is not known to the pool manager; counting it as empty", "pool", ref.String())
			t.Track(ref, false)
		default:
			if ctx.Err() == nil {
				t.log.Warn("polling pool failed", "pool", ref.String(), "error", err)
			}
		}
	}
}

// Track implements Tracker. Tracking a Pool that is already tracked only
// updates whether it is declared, so a re-declaration does not lose the
// Pool's counts or its waiters.
func (t *PoolTracker) Track(ref PoolRef, declared bool) {
	t.mu.Lock()
	p := t.poolLocked(ref)
	p.declared = declared
	t.mu.Unlock()
	t.requestPoll()
}

// Untrack implements Tracker. Anything waiting on the Pool is woken, since
// no event for it will be applied again.
func (t *PoolTracker) Untrack(ref PoolRef) {
	t.mu.Lock()
	if p, ok := t.pools[ref]; ok {
		delete(t.pools, ref)
		closeWaiters(p)
	}
	t.mu.Unlock()
}

//= docs/requirements/04-pool-manager.md#claiming
//# While the Pool Manager is unhealthy, the Scheduler SHALL count the
//# available warm MicroVMs of every Pool as zero when computing capacity.

// Available implements Tracker: the Pool's available count as the capacity
// computation should see it. It is zero for a Pool that is not tracked or
// whose declaration is failing (PL-016), zero from a RESOURCE_EXHAUSTED
// claim until the next event or poll (PL-053), and zero for every Pool
// while the Pool Manager is unhealthy, because a Pool Manager that cannot
// be reached cannot hand out a warm MicroVM however many it holds (PL-035).
func (t *PoolTracker) Available(ref PoolRef) int32 {
	if t.unhealthy() {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pools[ref]
	if !ok {
		return 0
	}
	return availableOf(p)
}

// availableOf is the available count of one tracked Pool, ignoring Pool
// Manager health.
func availableOf(p *trackedPool) int32 {
	if !p.declared || p.exhausted {
		return 0
	}
	return p.status.Available
}

// unhealthy reports whether the Pool Manager is currently unreachable.
func (t *PoolTracker) unhealthy() bool {
	return t.cfg.Health != nil && !t.cfg.Health.Healthy()
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//# When a `ClaimVM` call returns `RESOURCE_EXHAUSTED`, the Scheduler SHALL
//# set that Pool's available count to zero until the next event or poll
//# says otherwise.

// MarkExhausted implements Tracker. The Pool's available count reads zero
// from here on, whatever the last event said, until an event or a poll
// reports its state again: the claim that failed is better evidence than a
// count that another Runner, or another worker of this one, has already
// consumed.
func (t *PoolTracker) MarkExhausted(ref PoolRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.poolLocked(ref)
	p.exhausted = true
	p.status.Available = 0
}

// Wait implements Tracker: a channel closed the next time a MicroVM in the
// Pool becomes available. The caller counts as waiting on the Pool until
// then (OB-004, OB-021); the count is cleared when the waiters are woken.
func (t *PoolTracker) Wait(ref PoolRef) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.poolLocked(ref)
	p.waiting++
	return p.waiters
}

// Pools implements Tracker: a snapshot of every tracked Pool, in a stable
// order. The available count is the one Available reports, so a caller
// summing this snapshot computes the same capacity.
func (t *PoolTracker) Pools() []PoolAvailability {
	unhealthy := t.unhealthy()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]PoolAvailability, 0, len(t.pools))
	for ref, p := range t.pools {
		status := p.status
		status.Available = availableOf(p)
		if unhealthy {
			status.Available = 0
		}
		out = append(out, PoolAvailability{
			Pool:     ref,
			Status:   status,
			Declared: p.declared,
			Waiting:  p.waiting,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pool.String() < out[j].Pool.String() })
	return out
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//# When an event reports a MicroVM in a Pool becoming available, claimed,
//# released or deleted, the Scheduler SHALL update that Pool's available
//# count before the next capacity computation.

//= docs/requirements/04-pool-manager.md#capacity-tracking
//# When an event reports that a Pool is below its target size, the
//# Scheduler SHALL log it at `warn` level with the Pool name and the
//# counts.

//= docs/requirements/04-pool-manager.md#capacity-tracking
//# When an event reports a hook failure or a quarantined MicroVM in a Pool,
//# the Scheduler SHALL log it at `warn` level with the Pool name and the
//# MicroVM uid.

// apply folds one event into the Pool's counts and wakes anything waiting
// on the Pool. The counts move by the event's own meaning -- a MicroVM
// becoming available, claimed, released or deleted each changes exactly one
// or two of them -- and the update happens before apply returns, so a
// capacity computation that runs after the event has been delivered sees
// it. A POOL_SIZE_BELOW_TARGET event carries the Pool Manager's own counts,
// which are authoritative, so it resynchronises the Pool as well as
// producing the warning of PL-055.
func (t *PoolTracker) apply(event *Event) {
	t.mu.Lock()
	p := t.poolLocked(event.Pool)
	woken := false
	switch event.Type {
	case poolmgrv1.EventType_VM_PROVISIONED:
		p.exhausted = false
		p.status.Provisioning++

	case poolmgrv1.EventType_VM_AVAILABLE:
		p.exhausted = false
		p.status.Available++
		p.status.Provisioning = decrement(p.status.Provisioning)
		closeWaiters(p)
		woken = true

	case poolmgrv1.EventType_VM_CLAIMED:
		p.status.Available = decrement(p.status.Available)
		p.status.Leased++

	case poolmgrv1.EventType_VM_RELEASED:
		p.exhausted = false
		p.status.Leased = decrement(p.status.Leased)

	case poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY:
		p.exhausted = false
		p.status.Leased = decrement(p.status.Leased)

	case poolmgrv1.EventType_VM_DELETED_ON_RELEASE:
		p.exhausted = false

	case poolmgrv1.EventType_VM_HOOK_FAILED:
		p.status.Provisioning = decrement(p.status.Provisioning)
		if hookFailureQuarantined(event) {
			p.status.Quarantined++
		}

	case poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET:
		if counts, ok := belowTargetCounts(event); ok {
			p.exhausted = false
			p.status = counts.status()
		}
	}
	t.mu.Unlock()

	switch event.Type {
	case poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET:
		t.logBelowTarget(event)
	case poolmgrv1.EventType_VM_HOOK_FAILED:
		t.logHookFailure(event)
	}
	if woken {
		t.log.Debug("microvm available", "pool", event.Pool.String(), "uid", event.VMUID)
	}
	if t.cfg.Observer != nil {
		t.cfg.Observer(event)
	}
}

// logBelowTarget writes the PL-055 warning: the Pool name and the counts
// the Pool Manager reported with the event.
func (t *PoolTracker) logBelowTarget(event *Event) {
	counts, ok := belowTargetCounts(event)
	if !ok {
		t.log.Warn("pool is below its target size", "pool", event.Pool.String())
		return
	}
	t.log.Warn("pool is below its target size",
		"pool", event.Pool.String(), "namespace", event.Pool.Namespace,
		"target", counts.Target, "available", counts.Available, "leased", counts.Leased,
		"provisioning", counts.Provisioning, "quarantined", counts.Quarantined)
}

// logHookFailure writes the PL-056 warning: the Pool name and the uid of
// the MicroVM whose hook failed, and whether it was quarantined rather than
// deleted and replaced.
func (t *PoolTracker) logHookFailure(event *Event) {
	payload := hookFailurePayload(event)
	t.log.Warn("pool hook failed",
		"pool", event.Pool.String(), "namespace", event.Pool.Namespace, "uid", event.VMUID,
		"hook", payload.Hook, "policy", payload.Policy, "quarantined", payload.quarantined(),
		"error", payload.Error)
}

// belowTargetPayload is the payload of a POOL_SIZE_BELOW_TARGET event.
type belowTargetPayload struct {
	Target       int32 `json:"target"`
	Available    int32 `json:"available"`
	Leased       int32 `json:"leased"`
	Provisioning int32 `json:"provisioning"`
	Quarantined  int32 `json:"quarantined"`
}

// status is the payload's counts as a PoolStatus.
func (p belowTargetPayload) status() PoolStatus {
	return PoolStatus{
		Available:    p.Available,
		Leased:       p.Leased,
		Provisioning: p.Provisioning,
		Quarantined:  p.Quarantined,
	}
}

// belowTargetCounts decodes the counts an event carries, reporting whether
// it had any.
func belowTargetCounts(event *Event) (belowTargetPayload, bool) {
	var out belowTargetPayload
	if len(event.Payload) == 0 {
		return out, false
	}
	if err := json.Unmarshal(event.Payload, &out); err != nil {
		return belowTargetPayload{}, false
	}
	return out, true
}

// hookFailureBody is the payload of a VM_HOOK_FAILED event.
type hookFailureBody struct {
	Hook   string `json:"hook"`
	Error  string `json:"error"`
	Policy string `json:"policy"`
}

// quarantined reports whether the MicroVM was kept for inspection rather
// than deleted and replaced.
func (b hookFailureBody) quarantined() bool {
	return b.Policy == poolmgrv1.HookFailurePolicy_QUARANTINE.String()
}

// hookFailurePayload decodes a hook failure payload, tolerating one that is
// missing or unreadable.
func hookFailurePayload(event *Event) hookFailureBody {
	var out hookFailureBody
	if len(event.Payload) == 0 {
		return out
	}
	if err := json.Unmarshal(event.Payload, &out); err != nil {
		return hookFailureBody{}
	}
	return out
}

// hookFailureQuarantined reports whether a VM_HOOK_FAILED event says the
// MicroVM was quarantined.
func hookFailureQuarantined(event *Event) bool { return hookFailurePayload(event).quarantined() }

// setStatus replaces a Pool's counts with the ones a poll returned, which
// also clears an exhausted mark (PL-053).
func (t *PoolTracker) setStatus(ref PoolRef, status PoolStatus) {
	t.mu.Lock()
	p := t.poolLocked(ref)
	p.status = status
	p.exhausted = false
	woken := false
	if status.Available > 0 {
		closeWaiters(p)
		woken = true
	}
	t.mu.Unlock()
	if woken {
		t.log.Debug("poll found the pool has warm microvms", "pool", ref.String(), "available", status.Available)
	}
}

// poolLocked returns the tracked Pool, creating it if this is the first
// time it is named. t.mu has to be held.
func (t *PoolTracker) poolLocked(ref PoolRef) *trackedPool {
	p, ok := t.pools[ref]
	if !ok {
		p = &trackedPool{waiters: make(chan struct{})}
		t.pools[ref] = p
	}
	return p
}

// closeWaiters wakes everything waiting on the Pool and arms a fresh
// channel for the next waiter. The caller has to hold t.mu.
func closeWaiters(p *trackedPool) {
	close(p.waiters)
	p.waiters = make(chan struct{})
	p.waiting = 0
}

// requestPoll wakes the run loop without blocking.
func (t *PoolTracker) requestPoll() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// refs lists the tracked Pools in a stable order.
func (t *PoolTracker) refs() []PoolRef {
	t.mu.Lock()
	defer t.mu.Unlock()
	refs := make([]PoolRef, 0, len(t.pools))
	for ref := range t.pools {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	return refs
}

// decrement subtracts one without going below zero, because the counts are
// folded from a stream that may have started mid-life.
func decrement(n int32) int32 {
	if n <= 0 {
		return 0
	}
	return n - 1
}

// Compile-time interface check.
var _ Tracker = (*PoolTracker)(nil)
