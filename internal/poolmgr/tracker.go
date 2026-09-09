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
	// is unavailable and when a Pool starts being tracked (PL-051).
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
	// OnEvent and OnPoll, when set, are called after an event or a poll has
	// been applied; OnEvent is called for an event dropped because its Pool
	// is not tracked too, so that a caller sees every event that arrived.
	// They are the observation seam a test synchronises on and
	// a caller can use to react to Pool changes without a second
	// subscription; they run on the Tracker's own goroutines and must not
	// block.
	OnEvent func(*Event)
	OnPoll  func(PoolRef, PoolStatus)
}

// PoolTracker tracks how many warm MicroVMs each Pool has, from the Pool
// Manager's Events stream with GetPool as the fallback (PL-050 to PL-056).
// It is the input to the Scheduler's capacity computation (SC-002) and the
// wake-up source for a claim waiting on an exhausted Pool (SC-021). It is
// safe for concurrent use.
//
// Every one of a Pool's counts is kept by uid rather than as a running
// total, because the same event is seen more than once: a new subscription
// is replayed the recent events of every Pool, and those are exactly the
// events the poll that resynchronised the Pool has already accounted for.
// Naming a uid that is already in a count, or dropping one that is not in
// it, is a no-op, so a replay can neither inflate nor deflate it, and a
// poll landing beside the event it already counted cannot double it.
//
// The MicroVMs a poll reports whose uids no event has named are counted
// alongside the named ones, and the first event that names one moves it
// from the one group into the other -- the first, and only the first. The
// Pool Manager emits several events for one leased MicroVM, VM_CLAIMED then
// VM_RELEASED then VM_DELETED_ON_RELEASE, and if each of them took one
// MicroVM out of the anonymous count a Pool that a poll had counted
// anonymously would bleed three counts per Job and end up reporting zero
// while still holding warm MicroVMs.
//
// What the events cannot settle, a poll does. A Pool an event counts down
// to nothing is polled, because a wrong zero refuses Jobs and produces no
// event that could correct it. And a Pool short of its target where a
// MicroVM became available by taking one of the anonymous MicroVMs with it
// is polled, because the Tracker cannot tell a replayed availability event,
// where taking one is right, from a MicroVM created since the poll, where
// it leaves the Pool reading one short.
type PoolTracker struct {
	cfg TrackerConfig
	log *slog.Logger

	// kick asks the poll loop for an immediate poll, after a Pool starts
	// being tracked.
	kick chan struct{}
	// streamUp is closed and replaced by the subscription loop; the poll
	// loop reads it to decide whether the periodic poll is needed.
	streamMu sync.Mutex
	streamUp bool

	mu    sync.Mutex
	pools map[PoolRef]*trackedPool
}

// uidCount is one of a Pool's counts, kept by uid rather than as a running
// total. named holds the uid of every MicroVM an event has put in the
// count; unnamed is how many of the last poll's total no event has named;
// and accounted keeps one MicroVM from being taken out of unnamed twice,
// however many events the Pool Manager sends for it. It is dropped as soon
// as unnamed reaches zero, where there is nothing left to account for, so
// it stays the size of a Pool rather than growing with every MicroVM the
// Runner ever sees.
type uidCount struct {
	named     map[string]struct{}
	unnamed   int32
	accounted map[string]struct{}
}

func newUIDCount() uidCount {
	return uidCount{named: make(map[string]struct{}), accounted: make(map[string]struct{})}
}

// total is the count: the MicroVMs events have named plus the ones only a
// poll has counted.
func (c *uidCount) total() int32 {
	return int32(len(c.named)) + c.unnamed //nolint:gosec // pool sizes are small
}

// take moves one MicroVM out of the unnamed part of the count for uid, at
// most once for a given uid: the Pool Manager sends several events for one
// MicroVM and only the first of them says anything about how many of the
// poll's MicroVMs are still unaccounted for. It reports whether it did.
func (c *uidCount) take(uid string) bool {
	if uid == "" || c.unnamed <= 0 {
		return false
	}
	if _, done := c.accounted[uid]; done {
		return false
	}
	c.accounted[uid] = struct{}{}
	c.unnamed--
	if c.unnamed == 0 {
		clear(c.accounted)
	}
	return true
}

// add names a MicroVM. It reports whether it was not named before and
// whether naming it took one out of the unnamed part of the count, which is
// what happens when the MicroVM may be one the last poll counted without
// naming it.
func (c *uidCount) add(uid string) (added, tookUnnamed bool) {
	if uid == "" {
		return false, false
	}
	if _, ok := c.named[uid]; ok {
		return false, false
	}
	c.named[uid] = struct{}{}
	return true, c.take(uid)
}

// remove drops a MicroVM from the count, reporting whether it was named. A
// MicroVM that was not named can only be one the last poll counted, which
// is what the caller takes out of the unnamed part instead.
func (c *uidCount) remove(uid string) bool {
	if uid == "" {
		return false
	}
	if _, ok := c.named[uid]; ok {
		delete(c.named, uid)
		return true
	}
	return false
}

// rebaseline folds an authoritative total -- a GetPool answer or the counts
// a POOL_SIZE_BELOW_TARGET event carries -- into the count. The MicroVMs it
// reports that no event has named become the new unnamed part; the ones
// events have named are already in the total, so they are marked accounted
// and can never be taken out of the unnamed part later.
func (c *uidCount) rebaseline(total int32) {
	named := int32(len(c.named)) //nolint:gosec // pool sizes are small
	c.unnamed = total - named
	if c.unnamed < 0 {
		c.unnamed = 0
	}
	clear(c.accounted)
	if c.unnamed == 0 {
		return
	}
	for uid := range c.named {
		c.accounted[uid] = struct{}{}
	}
}

// reset empties the count.
func (c *uidCount) reset() {
	clear(c.named)
	clear(c.accounted)
	c.unnamed = 0
}

// trackedPool is one Pool's tracked state. Every count is uid-keyed: a
// running total folded from a stream that replays its recent events to
// every new subscription double counts them, and a running total folded
// alongside a poll double counts anything the poll's answer already
// included (OB-017, OB-019).
type trackedPool struct {
	available    uidCount
	leased       uidCount
	provisioning uidCount
	quarantined  uidCount
	// size is the Pool's target size from the last event that carried it.
	size int32
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

// availableCount is the Pool's available warm MicroVMs: the ones events
// have named plus the ones only a poll has counted.
func (p *trackedPool) availableCount() int32 {
	if !p.declared || p.exhausted {
		return 0
	}
	return p.available.total()
}

// status is the Pool's counts as a PoolStatus.
func (p *trackedPool) status() PoolStatus {
	return PoolStatus{
		Available:    p.availableCount(),
		Leased:       p.leased.total(),
		Provisioning: p.provisioning.total(),
		Quarantined:  p.quarantined.total(),
	}
}

// rebaseline folds the Pool Manager's own figures -- a GetPool answer or
// the counts a POOL_SIZE_BELOW_TARGET event carries -- into every count.
func (p *trackedPool) rebaseline(status PoolStatus) {
	p.available.rebaseline(status.Available)
	p.leased.rebaseline(status.Leased)
	p.provisioning.rebaseline(status.Provisioning)
	p.quarantined.rebaseline(status.Quarantined)
}

// leaves takes a MicroVM out of one count, whether an event had named it or
// only a poll had counted it.
func leaves(c *uidCount, uid string) {
	if !c.remove(uid) {
		c.take(uid)
	}
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

// Run tracks every Pool until ctx is cancelled. It holds a subscription to
// the Events service, applying each event to the Pool it names as it
// arrives, and runs a poll loop beside it that asks GetPool for a Pool's
// counts when the Pool starts being tracked -- a Pool the Runner has just
// declared, or one it has just reloaded, is usually not empty and its
// MicroVMs became available before the Runner was listening -- and, while
// the stream is unavailable, at the configured interval.
//
// Both loops stop with ctx, and Run returns only when both have.
func (t *PoolTracker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t.pollLoop(ctx)
	}()
	t.subscribeLoop(ctx)
	wg.Wait()
	return nil
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//# When the `Events` stream is unavailable, the Scheduler SHALL fall back
//# to polling `GetPool` at the configured interval until the stream can be
//# re-established.

// subscribeLoop keeps a subscription to the Events service. When Subscribe
// fails, or the stream is dropped, it marks the stream down -- which is
// what puts the poll loop beside it onto its interval -- and tries again
// after that same interval, so the Pool Manager is asked for a new
// subscription as often as it is polled, and the Tracker goes back to
// events as soon as one is granted.
//
// The recent events every new subscription is replayed need no poll of
// their own. Each of a Pool's counts is uid-keyed, so folding an event that
// has already been folded changes nothing, and the interval the stream
// spends down before a re-subscription is one the poll loop beside this one
// is polling through.
func (t *PoolTracker) subscribeLoop(ctx context.Context) {
	for ctx.Err() == nil {
		stream, err := t.cfg.Events.Subscribe(ctx, EventFilter{})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			t.setStreamUp(false)
			t.log.Warn("events stream unavailable; polling GetPool instead",
				"interval", t.cfg.PollInterval, "error", err)
			if werr := t.sleep(ctx, t.cfg.PollInterval); werr != nil {
				return
			}
			continue
		}
		t.setStreamUp(true)
		t.readStream(ctx, stream)
		t.setStreamUp(false)
		if ctx.Err() != nil {
			return
		}
		if werr := t.sleep(ctx, t.cfg.PollInterval); werr != nil {
			return
		}
	}
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

// pollLoop polls every tracked Pool when a Pool starts being tracked and,
// while the Events stream is down, at the configured interval. It shares
// the interval with the re-subscription: while the stream is up the timer
// only re-arms.
//
// The kick path re-arms the timer without draining it first, which is safe
// here rather than an oversight: Reset drops a tick the timer has already
// delivered, both on clock.Fake, which drains the channel itself, and on
// the real timer at this module's Go version, where a stopped or reset
// timer can no longer hand out a stale value.
func (t *PoolTracker) pollLoop(ctx context.Context) {
	timer := t.cfg.Clock.NewTimer(t.cfg.PollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.kick:
		case <-timer.C():
			if t.streamIsUp() {
				timer.Reset(t.cfg.PollInterval)
				continue
			}
		}
		t.pollAll(ctx)
		timer.Reset(t.cfg.PollInterval)
	}
}

// sleep waits for d on the clock. It returns the context's error when ctx
// ends first.
func (t *PoolTracker) sleep(ctx context.Context, d time.Duration) error {
	timer := t.cfg.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

// setStreamUp records whether a subscription is held.
func (t *PoolTracker) setStreamUp(up bool) {
	t.streamMu.Lock()
	defer t.streamMu.Unlock()
	t.streamUp = up
}

// streamIsUp reports whether a subscription is held.
func (t *PoolTracker) streamIsUp() bool {
	t.streamMu.Lock()
	defer t.streamMu.Unlock()
	return t.streamUp
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
			t.applyPoll(ref, pool.Spec.Size, pool.Status)
		case errors.Is(err, ErrNotFound):
			t.log.Warn("pool is not known to the pool manager; counting it as empty", "pool", ref.String())
			t.setDeclared(ref, false)
		default:
			if ctx.Err() == nil {
				t.log.Warn("polling pool failed", "pool", ref.String(), "error", err)
			}
		}
	}
}

// Track implements Tracker. Tracking a Pool that is already tracked only
// updates whether it is declared, so a re-declaration does not lose the
// Pool's counts or its waiters. A newly tracked Pool is polled, because its
// warm MicroVMs may have become available before the Tracker was listening.
func (t *PoolTracker) Track(ref PoolRef, declared bool) {
	t.mu.Lock()
	_, known := t.pools[ref]
	p := t.poolLocked(ref)
	p.declared = declared
	t.mu.Unlock()
	if !known || declared {
		t.requestPoll()
	}
}

// setDeclared records whether a Pool is declared without asking for a poll,
// which is what the poll loop itself needs.
func (t *PoolTracker) setDeclared(ref PoolRef, declared bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.poolLocked(ref).declared = declared
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
	return p.availableCount()
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
	p.available.reset()
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
		status := p.status()
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

// applyPoll folds the counts of one GetPool answer into a Pool. The
// MicroVMs the answer says are available that no event has named are
// counted as unnamed; the ones events have named are already counted, so
// the Pool Manager's total is not double counted. The other three counts
// are running totals between polls, so the answer replaces them: this is
// what the poll every re-subscription asks for undoes the replay's double
// counting with.
func (t *PoolTracker) applyPoll(ref PoolRef, size int32, status PoolStatus) {
	t.mu.Lock()
	p := t.poolLocked(ref)
	p.rebaseline(status)
	p.size = size
	p.exhausted = false
	woken := p.availableCount() > 0
	if woken {
		closeWaiters(p)
	}
	applied := p.status()
	t.mu.Unlock()

	if woken {
		t.log.Debug("poll found warm microvms", "pool", ref.String(), "available", applied.Available)
	}
	if t.cfg.OnPoll != nil {
		t.cfg.OnPoll(ref, applied)
	}
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

// apply folds one event into the Pool it names and wakes anything waiting
// on that Pool. A MicroVM becoming available is added to the Pool's
// available set, and one that is claimed, released, deleted or quarantined
// is taken out of it, so the count is right before apply returns and a
// capacity computation that runs after the event has been delivered sees
// it. A POOL_SIZE_BELOW_TARGET event carries the Pool Manager's own counts,
// so it resynchronises the Pool as well as producing the warning of
// PL-055, and a hook failure produces the warning of PL-056.
//
// Two things ask for a poll, which is the only way to settle a count the
// events alone cannot. An event that counts a Pool down to nothing: a count
// that is wrongly zero is the one error the Tracker cannot recover from on
// its own, because an empty Pool is never claimed from and a Pool that is
// never claimed from produces no further event to correct it. And a MicroVM
// becoming available that took one of the MicroVMs the last poll left
// unnamed, since that is either the replay of an event the poll already
// counted or a MicroVM created since, and the Pool reads one short if it is
// the second. Both are self-limiting: a Pool whose MicroVMs are all named
// asks for neither.
//
// An event for a Pool this Runner does not track is dropped. The
// subscription carries every Pool on the Pool Manager, and on a shared one
// most of them belong to other Runners: adopting them would grow the
// Tracker without bound, publish a metric series per foreign Pool (PL-054)
// and poll each of them. Nothing is lost by dropping them, because Track
// polls a Pool the moment the Runner starts tracking it, so a Pool the
// Runner does declare picks up whatever it missed.
func (t *PoolTracker) apply(event *Event) {
	t.mu.Lock()
	p, tracked := t.pools[event.Pool]
	if !tracked {
		t.mu.Unlock()
		if t.cfg.OnEvent != nil {
			t.cfg.OnEvent(event)
		}
		return
	}
	woken, resync := false, false
	before := p.availableCount()
	switch event.Type {
	case poolmgrv1.EventType_VM_PROVISIONED:
		p.exhausted = false
		p.provisioning.add(event.VMUID)

	case poolmgrv1.EventType_VM_AVAILABLE:
		p.exhausted = false
		leaves(&p.provisioning, event.VMUID)
		added, tookUnnamed := p.available.add(event.VMUID)
		if added {
			closeWaiters(p)
			woken = true
		}
		// Naming this MicroVM took one of the MicroVMs the last poll
		// counted without naming it, which is right if the event is the
		// replay of the one that made it available and wrong if it is a
		// MicroVM created since the poll, in which case the Pool now reads
		// one short. Only the Pool Manager can tell the two apart, and only
		// a Pool that is short of its target can be reading short at all.
		resync = tookUnnamed && (p.size <= 0 || p.availableCount() < p.size)

	case poolmgrv1.EventType_VM_CLAIMED:
		// A claimed MicroVM was available a moment before, so one the
		// Tracker never named has to be one the last poll counted without
		// naming it. It is the only event that says so: the release and
		// the deletion that follow it are about a MicroVM that was already
		// leased, and a MicroVM whose hook failed while it was being
		// created was never available at all.
		leaves(&p.available, event.VMUID)
		p.leased.add(event.VMUID)

	case poolmgrv1.EventType_VM_RELEASED:
		p.exhausted = false
		p.available.remove(event.VMUID)
		leaves(&p.leased, event.VMUID)

	case poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY:
		p.exhausted = false
		p.available.remove(event.VMUID)
		leaves(&p.leased, event.VMUID)

	case poolmgrv1.EventType_VM_DELETED_ON_RELEASE:
		p.exhausted = false
		p.available.remove(event.VMUID)
		p.leased.remove(event.VMUID)

	case poolmgrv1.EventType_VM_HOOK_FAILED:
		p.available.remove(event.VMUID)
		leaves(&p.provisioning, event.VMUID)
		if hookFailureQuarantined(event) {
			p.quarantined.add(event.VMUID)
		}

	case poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET:
		if counts, ok := belowTargetCounts(event); ok {
			p.exhausted = false
			p.size = counts.Target
			p.rebaseline(PoolStatus{
				Available:    counts.Available,
				Leased:       counts.Leased,
				Provisioning: counts.Provisioning,
				Quarantined:  counts.Quarantined,
			})
		}
	}
	emptied := before > 0 && p.availableCount() == 0
	t.mu.Unlock()

	if emptied || resync {
		t.requestPoll()
	}

	switch event.Type {
	case poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET:
		t.logBelowTarget(event)
	case poolmgrv1.EventType_VM_HOOK_FAILED:
		t.logHookFailure(event)
	}
	if woken {
		t.log.Debug("microvm available", "pool", event.Pool.String(), "uid", event.VMUID)
	}
	if t.cfg.OnEvent != nil {
		t.cfg.OnEvent(event)
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

// poolLocked returns the tracked Pool, creating it if this is the first
// time it is named. t.mu has to be held.
func (t *PoolTracker) poolLocked(ref PoolRef) *trackedPool {
	p, ok := t.pools[ref]
	if !ok {
		p = &trackedPool{
			available:    newUIDCount(),
			leased:       newUIDCount(),
			provisioning: newUIDCount(),
			quarantined:  newUIDCount(),
			waiters:      make(chan struct{}),
		}
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

// requestPoll wakes the poll loop without blocking.
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

// Compile-time interface check.
var _ Tracker = (*PoolTracker)(nil)
