package poolmgr_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// The tests in this file drive the Tracker from a scripted Events service
// and a stub PoolAdmin rather than from the fake Pool Manager. They are
// about bookkeeping the fake cannot produce on demand: the several events
// battery sends for one leased MicroVM, a replay whose backlog no longer
// holds the availability events of a full Pool, and another Runner's Pool
// arriving on a subscription that carries every Pool.

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports a MicroVM in a Pool becoming available, claimed,
//# released or deleted, the Scheduler SHALL update that Pool's available
//# count before the next capacity computation.

// TestOneMicroVMLeavesThePolledCountOnce takes a Pool of three that only a
// poll has counted through the three events battery sends for one leased
// MicroVM: claimed, released, then deleted on release. Only the claim says
// a warm MicroVM has gone, so the Pool has to read two afterwards. Counting
// each of them instead reads zero for a Pool still holding two warm
// MicroVMs, and no poll corrects that while the stream is up.
func TestOneMicroVMLeavesThePolledCountOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(3, poolmgr.PoolStatus{Available: 3})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll to count three warm microvms", func() bool {
		return tracker.Available(pool) == 3
	})

	// No poll may answer from here on: the count below has to be the one
	// the events produced rather than one a poll repaired.
	admin.failWith(errors.New("no more polls in this test"))

	for i, typ := range []poolmgr.EventType{
		poolmgrv1.EventType_VM_CLAIMED,
		poolmgrv1.EventType_VM_RELEASED,
		poolmgrv1.EventType_VM_DELETED_ON_RELEASE,
	} {
		events.send(vmEvent(pool, typ, "vm-1", int64(i+1)))
		tracker.awaitEvent(t, ctx, "the event for the leased microvm", func(e *poolmgr.Event) bool {
			return e.Type == typ && e.VMUID == "vm-1"
		})
	}

	if got := tracker.Available(pool); got != 2 {
		t.Errorf("available = %d after one microvm was claimed, released and deleted, want 2", got)
	}
}

// TestAPoolCountedDownToNothingIsPolled replays the backlog a Runner that
// has just restarted is handed: claims for MicroVMs whose availability
// events have fallen out of the Pool Manager's outbox window, so the
// Tracker has never named them. They count the Pool down to nothing while
// it is in fact full. A Pool that reads empty is never claimed from and so
// produces no further event, and the Runner would refuse every Job with no
// path back, so the Tracker asks the Pool Manager for its own figures.
func TestAPoolCountedDownToNothingIsPolled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(3, poolmgr.PoolStatus{Available: 3})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll to count three warm microvms", func() bool {
		return tracker.Available(pool) == 3
	})

	polls := admin.calls()
	for i, uid := range []string{"vm-a", "vm-b", "vm-c"} {
		events.send(vmEvent(pool, poolmgrv1.EventType_VM_CLAIMED, uid, int64(i+1)))
	}
	tracker.awaitEvent(t, ctx, "the last of the replayed claims", func(e *poolmgr.Event) bool {
		return e.VMUID == "vm-c"
	})

	// The clock never moves here, so nothing but the Pool reading empty
	// can produce the poll that puts the count back.
	tracker.await(t, ctx, "the pool manager's own count to be asked for", func() bool {
		return admin.calls() > polls && tracker.Available(pool) == 3
	})
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports a MicroVM in a Pool becoming available, claimed,
//# released or deleted, the Scheduler SHALL update that Pool's available
//# count before the next capacity computation.

// TestARepairPollThatFailsIsAskedForAgain takes the previous test one step
// further: the poll a Pool counted down to nothing asks for fails, as a
// transient error on a Pool Manager that is about to be healthy again. The
// Pool has to keep owing that resynchronisation, so that the next interval
// asks for it again -- even though the Events stream never dropped, which
// is what would otherwise put the poll loop back to work. Asking exactly
// once strands the Pool on a count of zero for ever: the Runner refuses
// every Job for it, no Job means no claim, and no claim means no event that
// could correct it.
func TestARepairPollThatFailsIsAskedForAgain(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(3, poolmgr.PoolStatus{Available: 3})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll to count three warm microvms", func() bool {
		return tracker.Available(pool) == 3
	})

	// The repair the replayed backlog below asks for is refused. A poll
	// that failed produces no OnPoll, so the stub itself is what says the
	// repair was attempted and refused.
	admin.failWith(errors.New("the pool manager is briefly unreachable"))
	polls := admin.calls()
	for i, uid := range []string{"vm-a", "vm-b", "vm-c"} {
		events.send(vmEvent(pool, poolmgrv1.EventType_VM_CLAIMED, uid, int64(i+1)))
	}
	admin.awaitGet(t, ctx, polls+1)
	if got := tracker.Available(pool); got != 0 {
		t.Fatalf("available = %d after the replayed claims, want the wrong zero this test repairs", got)
	}

	// The Pool Manager is healthy again, and the stream never dropped, so
	// only a Pool that remembers it owes a poll can ask for one.
	admin.answer(3, poolmgr.PoolStatus{Available: 3})
	awaitPollInterval(t, ctx, tracker, pool, 3)
}

// awaitPollInterval moves the clock an interval at a time until the Pool's
// available count reads want. It moves it more than once on purpose: a poll
// loop that is between a poll and re-arming its timer has the tick that has
// just been delivered drained by Reset, so one interval can be swallowed.
// Another interval costs nothing, and a Tracker that has forgotten a Pool
// owes a poll never recovers however many of them pass.
func awaitPollInterval(t *testing.T, ctx context.Context, tracker *trackerHarness, pool poolmgr.PoolRef, want int32) {
	t.Helper()
	for range 100 {
		if got := tracker.Available(pool); got == want {
			return
		}
		awaitTimers(t, ctx, tracker.clk, 1)
		tracker.clk.Advance(trackerPollInterval)
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("timed out waiting for the poll interval to repair the pool: %v", ctx.Err())
		}
	}
	t.Errorf("available = %d after a hundred poll intervals with the pool manager healthy again, want %d",
		tracker.Available(pool), want)
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports a MicroVM in a Pool becoming available, claimed,
//# released or deleted, the Scheduler SHALL update that Pool's available
//# count before the next capacity computation.

// TestAPollAnsweredBeforeAnEventIsNotFoldedAfterIt delivers a GetPool
// answer out of order with an event: the Pool Manager answers while a Pool
// of two holds two warm MicroVMs, one of them is claimed, and the answer
// reaches the Tracker only after the claim has been applied. Folding it
// would put the claimed MicroVM back, because a poll's total can raise a
// count, and nothing would take it out again; the Runner would count a warm
// MicroVM the Pool Manager no longer has. The answer has to be discarded,
// and the Pool has to go on owing the poll it did not get an answer to.
func TestAPollAnsweredBeforeAnEventIsNotFoldedAfterIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(2, poolmgr.PoolStatus{Available: 2})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll to count two warm microvms", func() bool {
		return tracker.Available(pool) == 2
	})

	// Re-declaring the Pool asks for a poll. Its answer is taken now, while
	// the Pool Manager still holds two warm MicroVMs, and held back.
	release := admin.holdNext(t)
	polls := admin.calls()
	tracker.Track(pool, true)
	admin.awaitGet(t, ctx, polls+1)

	// One of them is claimed while the question is out.
	events.send(vmEvent(pool, poolmgrv1.EventType_VM_CLAIMED, "vm-1", 1))
	tracker.awaitEvent(t, ctx, "the claim", func(e *poolmgr.Event) bool {
		return e.ID == 1
	})
	if got := tracker.Available(pool); got != 1 {
		t.Fatalf("available = %d after the claim, want 1", got)
	}

	// The answer from before the claim arrives now. The poll loop works one
	// round at a time, so the next GetPool, which fails, is what says the
	// held answer has been dealt with; re-declaring the Pool again makes
	// sure there is a next one.
	admin.failWith(errors.New("the pool manager is briefly unreachable"))
	release()
	tracker.Track(pool, true)
	admin.awaitGet(t, ctx, polls+2)
	if got := tracker.Available(pool); got != 1 {
		t.Fatalf("available = %d after a poll answered before the claim arrived after it, want 1", got)
	}

	// The replacement for the claimed MicroVM has booted, and no event says
	// so. The discarded answer and the failed one after it left the Pool
	// owing a poll, so the interval asks again with the stream up.
	admin.answer(2, poolmgr.PoolStatus{Available: 2, Leased: 1})
	awaitPollInterval(t, ctx, tracker, pool, 2)
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports a MicroVM in a Pool becoming available, claimed,
//# released or deleted, the Scheduler SHALL update that Pool's available
//# count before the next capacity computation.

// TestAReplacementBootedSinceThePollIsCounted covers the other half of the
// guess a VM_AVAILABLE for an unnamed MicroVM forces. A Pool of two has one
// warm MicroVM and one leased, so the poll leaves one MicroVM counted but
// unnamed. The replacement for the leased one then finishes booting and its
// VM_AVAILABLE arrives. Naming it takes the unnamed one with it, on the
// assumption that the event is the replay of the one the poll already
// counted -- which is wrong here, and leaves the Pool reading one where it
// holds two. The Pool is short of its target, so only the Pool Manager can
// say which case this is, and the Tracker has to ask.
func TestAReplacementBootedSinceThePollIsCounted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(2, poolmgr.PoolStatus{Available: 1, Leased: 1})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll to count one warm microvm and one leased", func() bool {
		return tracker.Available(pool) == 1
	})

	// The replacement has booted, so the Pool Manager now holds two.
	admin.answer(2, poolmgr.PoolStatus{Available: 2, Leased: 1})
	events.send(vmEvent(pool, poolmgrv1.EventType_VM_AVAILABLE, "vm-new", 1))

	// The clock never moves, so the only poll that can happen is the one
	// the Pool asks for because it may be reading short.
	tracker.await(t, ctx, "the replacement to be counted", func() bool {
		return tracker.Available(pool) == 2
	})
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports a MicroVM in a Pool becoming available, claimed,
//# released or deleted, the Scheduler SHALL update that Pool's available
//# count before the next capacity computation.

// TestAReplayedLifecycleOnAFullPoolIsPolled replays one complete lifecycle
// for a MicroVM that was created, claimed and gone before the poll: the
// availability event and the claim that follows it. The availability event
// lands while the Pool still reads three of three, so nothing looks wrong
// at that moment; it is the claim after it that takes the count to two on a
// Pool that in fact holds three. The doubt is introduced by one event and
// only becomes visible on the next, so it has to outlive the event that
// raised it.
func TestAReplayedLifecycleOnAFullPoolIsPolled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(3, poolmgr.PoolStatus{Available: 3})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll to count three warm microvms", func() bool {
		return tracker.Available(pool) == 3
	})

	polls := admin.calls()
	events.send(vmEvent(pool, poolmgrv1.EventType_VM_AVAILABLE, "vm-gone", 1))
	events.send(vmEvent(pool, poolmgrv1.EventType_VM_CLAIMED, "vm-gone", 2))
	tracker.awaitEvent(t, ctx, "the replayed claim", func(e *poolmgr.Event) bool {
		return e.ID == 2
	})

	// The clock never moves, so the count can only come back through the
	// poll the Pool asks for once it reads below its target.
	tracker.await(t, ctx, "the pool manager's own count to be asked for", func() bool {
		return admin.calls() > polls && tracker.Available(pool) == 3
	})
}

// TestOneMicroVMLeavesThePolledLeasedCountOnce is the at-most-once guard on
// its own, on a count no other part of the bookkeeping touches. A Pool of
// two has both its MicroVMs leased and neither named, so the poll's figure
// of two is entirely anonymous. One of them is then released and, its
// replacement having failed, expires: two events about one MicroVM, and
// only the first says anything about how many of the poll's leased MicroVMs
// are still unaccounted for. Taking one per event instead reads zero for a
// Pool that still has one MicroVM out on lease.
func TestOneMicroVMLeavesThePolledLeasedCountOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(2, poolmgr.PoolStatus{Leased: 2})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll to count two leased microvms", func() bool {
		return poolStatus(t, tracker, pool).Leased == 2
	})

	// No poll may answer from here on: the count below has to be the one
	// the events produced rather than one a poll repaired.
	admin.failWith(errors.New("no more polls in this test"))

	for i, typ := range []poolmgr.EventType{
		poolmgrv1.EventType_VM_RELEASED,
		poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY,
	} {
		events.send(vmEvent(pool, typ, "vm-1", int64(i+1)))
		tracker.awaitEvent(t, ctx, "the event for the leased microvm", func(e *poolmgr.Event) bool {
			return e.Type == typ && e.VMUID == "vm-1"
		})
	}

	if got := poolStatus(t, tracker, pool).Leased; got != 1 {
		t.Errorf("leased = %d after one microvm was released and then expired, want 1", got)
	}
}

// TestOtherRunnersPoolsAreNotTracked sends an event for a Pool in another
// Runner's namespace, which a subscription to every Pool carries on a
// shared Pool Manager. The Tracker has to drop it: adopting it would grow
// the Tracker with every foreign Pool, publish a metric series for each and
// poll them all.
func TestOtherRunnersPoolsAreNotTracked(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	ours := ref("small")
	theirs := poolmgr.PoolRef{Name: "small", Namespace: "another-runner"}
	events := newScriptedEvents()
	admin := newStubAdmin(1, poolmgr.PoolStatus{Available: 1})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(ours, true)
	tracker.await(t, ctx, "our pool to be counted", func() bool {
		return tracker.Available(ours) == 1
	})

	events.send(vmEvent(theirs, poolmgrv1.EventType_VM_AVAILABLE, "vm-x", 1))
	tracker.awaitEvent(t, ctx, "the foreign pool's event", func(e *poolmgr.Event) bool {
		return e.Pool == theirs
	})

	if got := tracker.Available(theirs); got != 0 {
		t.Errorf("available = %d for another runner's pool, want 0", got)
	}
	pools := tracker.Pools()
	if len(pools) != 1 || pools[0].Pool != ours {
		t.Errorf("snapshot = %+v, want this runner's pool alone", pools)
	}
	if polled := admin.polled(); slices.Contains(polled, theirs) {
		t.Errorf("polled pools = %v, want another runner's pool never polled", polled)
	}
}

// TestReplayedEventsDoNotInflateTheOtherCounts replays the events a new
// subscription is handed for MicroVMs the poll had already counted: the
// claim of a leased MicroVM and the creation of a provisioning one. Leased,
// provisioning and quarantined are what PoolAvailability.Status reports for
// OB-016 and OB-019, and folding them as running totals counts every
// replayed event again, on every re-subscription, with nothing to repair it
// while the stream stays up.
func TestReplayedEventsDoNotInflateTheOtherCounts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(3, poolmgr.PoolStatus{Leased: 1, Provisioning: 1})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll to count one leased and one provisioning microvm", func() bool {
		status := poolStatus(t, tracker, pool)
		return status.Leased == 1 && status.Provisioning == 1
	})

	// The events behind the counts the poll reported, delivered twice, as
	// two subscriptions replaying the same window would.
	for round := int64(1); round <= 2; round++ {
		events.send(vmEvent(pool, poolmgrv1.EventType_VM_CLAIMED, "vm-leased", round*10))
		events.send(vmEvent(pool, poolmgrv1.EventType_VM_PROVISIONED, "vm-booting", round*10+1))
		tracker.awaitEvent(t, ctx, "the replayed provisioning event", func(e *poolmgr.Event) bool {
			return e.ID == round*10+1
		})
		status := poolStatus(t, tracker, pool)
		if status.Leased != 1 {
			t.Errorf("leased = %d after replay %d, want the pool manager's 1", status.Leased, round)
		}
		if status.Provisioning != 1 {
			t.Errorf("provisioning = %d after replay %d, want the pool manager's 1", status.Provisioning, round)
		}
	}
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports a MicroVM in a Pool becoming available, claimed,
//# released or deleted, the Scheduler SHALL update that Pool's available
//# count before the next capacity computation.

// TestADuplicateAvailableEventChangesNothing replays the availability event
// of a MicroVM the Tracker has already counted, which is exactly what a new
// subscription is handed. It must change nothing: the count stays where it
// was, nothing waiting on the Pool is woken, and the number of callers
// waiting on the Pool that OB-004 and OB-021 report is not cleared by an
// event that told the Tracker nothing new.
func TestADuplicateAvailableEventChangesNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pool := ref("small")
	events := newScriptedEvents()
	admin := newStubAdmin(1, poolmgr.PoolStatus{})
	tracker := newTrackerWith(t, events, admin, nil, trackerPollInterval, nil)
	tracker.run(t)
	events.awaitSubscribed(t, ctx)
	tracker.Track(pool, true)
	tracker.await(t, ctx, "the poll a newly tracked pool triggers", func() bool {
		return admin.calls() > 0
	})
	// A poll that finds warm MicroVMs wakes the waiters too, so no poll may
	// answer once there is something waiting: the wake under test has to be
	// the event's.
	admin.failWith(errors.New("no more polls in this test"))

	events.send(vmEvent(pool, poolmgrv1.EventType_VM_AVAILABLE, "vm-1", 1))
	tracker.await(t, ctx, "the microvm to be counted", func() bool {
		return tracker.Available(pool) == 1
	})

	waiting := tracker.Wait(pool)
	events.send(vmEvent(pool, poolmgrv1.EventType_VM_AVAILABLE, "vm-1", 2))
	tracker.awaitEvent(t, ctx, "the duplicate availability event", func(e *poolmgr.Event) bool {
		return e.ID == 2
	})

	select {
	case <-waiting:
		t.Error("a duplicate availability event woke everything waiting on the pool")
	default:
	}
	if got := tracker.Available(pool); got != 1 {
		t.Errorf("available = %d after a duplicate availability event, want 1", got)
	}
	if got := poolWaiting(t, tracker, pool); got != 1 {
		t.Errorf("waiting = %d after a duplicate availability event, want the one caller still waiting", got)
	}
}

// poolWaiting is how many callers the Tracker reports waiting on a Pool.
func poolWaiting(t *testing.T, tracker poolmgr.Tracker, pool poolmgr.PoolRef) int {
	t.Helper()
	for _, entry := range tracker.Pools() {
		if entry.Pool == pool {
			return entry.Waiting
		}
	}
	t.Fatalf("no pool %s in the tracker's snapshot", pool)
	return 0
}

// vmEvent is one Pool Manager event about a MicroVM.
func vmEvent(pool poolmgr.PoolRef, typ poolmgr.EventType, uid string, id int64) *poolmgr.Event {
	return &poolmgr.Event{ID: id, Pool: pool, VMUID: uid, Type: typ, At: testEpoch}
}

// scriptedEvents is an Events service a test drives by hand: Subscribe
// hands out one stream at a time and send pushes onto it the events the
// Pool Manager would have sent.
type scriptedEvents struct {
	mu      sync.Mutex
	current *scriptedStream
	// granted carries one token per subscription handed out, so a test can
	// wait for the Tracker to be listening before it sends anything.
	granted chan struct{}
}

func newScriptedEvents() *scriptedEvents {
	return &scriptedEvents{granted: make(chan struct{}, 16)}
}

// awaitSubscribed blocks until the next subscription has been handed out.
// Nothing may be sent before that: an event pushed while the Tracker is
// between subscriptions has no stream to arrive on.
func (e *scriptedEvents) awaitSubscribed(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-e.granted:
	case <-ctx.Done():
		t.Fatalf("the tracker never took out a subscription: %v", ctx.Err())
	}
}

// Subscribe implements poolmgr.Events.
func (e *scriptedEvents) Subscribe(_ context.Context, _ poolmgr.EventFilter) (poolmgr.EventStream, error) {
	s := newScriptedStream()
	e.mu.Lock()
	e.current = s
	e.mu.Unlock()
	select {
	case e.granted <- struct{}{}:
	default:
	}
	return s, nil
}

// send pushes one event onto the current stream.
func (e *scriptedEvents) send(event *poolmgr.Event) {
	e.mu.Lock()
	s := e.current
	e.mu.Unlock()
	if s != nil {
		s.events <- event
	}
}

// scriptedStream is one subscription of a scriptedEvents.
type scriptedStream struct {
	events chan *poolmgr.Event
	closed chan struct{}
	once   sync.Once
}

func newScriptedStream() *scriptedStream {
	return &scriptedStream{events: make(chan *poolmgr.Event, 256), closed: make(chan struct{})}
}

// Recv implements poolmgr.EventStream.
func (s *scriptedStream) Recv(ctx context.Context) (*poolmgr.Event, error) {
	select {
	case event := <-s.events:
		return event, nil
	case <-s.closed:
		return nil, fmt.Errorf("%w: the pool manager dropped the stream", poolmgr.ErrUnavailable)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close implements poolmgr.EventStream.
func (s *scriptedStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// stubAdmin is a PoolAdmin whose GetPool answers with the status the test
// sets and records what it was asked for. The Tracker calls nothing else on
// the interface, so the rest is left to the embedded nil.
type stubAdmin struct {
	poolmgr.PoolAdmin

	mu     sync.Mutex
	size   int32
	status poolmgr.PoolStatus
	err    error
	gets   int
	refs   []poolmgr.PoolRef
	// held, when set, is what the next GetPool waits on after it has taken
	// its answer and before it returns it.
	held chan struct{}
	// pinged carries one token per GetPool, so a test can wait for a poll
	// that answered with an error, which produces nothing observable on
	// the Tracker itself.
	pinged chan struct{}
}

func newStubAdmin(size int32, status poolmgr.PoolStatus) *stubAdmin {
	return &stubAdmin{size: size, status: status, pinged: make(chan struct{}, 256)}
}

// awaitGet blocks until GetPool has been called at least n times.
func (a *stubAdmin) awaitGet(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	for a.calls() < n {
		select {
		case <-a.pinged:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %d GetPool calls; saw %d: %v", n, a.calls(), ctx.Err())
		}
	}
}

// failWith makes every later GetPool fail, so that a test can be sure no
// poll repaired what it is measuring.
func (a *stubAdmin) failWith(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

// answer changes what GetPool reports from now on and clears any failure,
// which is how a test moves the Pool Manager on or lets it come back.
func (a *stubAdmin) answer(size int32, status poolmgr.PoolStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.size, a.status, a.err = size, status, nil
}

// calls is how many times GetPool has been called.
func (a *stubAdmin) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gets
}

// polled is every Pool GetPool has been asked for.
func (a *stubAdmin) polled() []poolmgr.PoolRef {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]poolmgr.PoolRef(nil), a.refs...)
}

// holdNext makes the next GetPool take its answer when it is called and
// then wait for release before returning it, which is how a test delivers
// an answer the Pool Manager gave before an event after the Tracker has
// applied that event.
func (a *stubAdmin) holdNext(t *testing.T) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	a.mu.Lock()
	a.held = gate
	a.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

// GetPool implements poolmgr.PoolAdmin. The answer is taken when GetPool
// is called, so a held call answers with the status as it was then.
func (a *stubAdmin) GetPool(ctx context.Context, poolRef poolmgr.PoolRef) (*poolmgr.Pool, error) {
	a.mu.Lock()
	a.gets++
	a.refs = append(a.refs, poolRef)
	held := a.held
	a.held = nil
	err := a.err
	pool := &poolmgr.Pool{
		Spec:   poolmgr.PoolSpec{Ref: poolRef, Size: a.size},
		Status: a.status,
	}
	a.mu.Unlock()
	select {
	case a.pinged <- struct{}{}:
	default:
	}
	if held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return pool, nil
}
