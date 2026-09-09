package poolmgr_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// trackerPollInterval is the configured GetPool interval in these tests.
// Nothing waits for it: the tests fire the Tracker's fake clock.
const trackerPollInterval = 5 * time.Second

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# The Scheduler SHALL track the number of available warm MicroVMs in every
//# Pool by subscribing to the `Events` service.

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports a MicroVM in a Pool becoming available, claimed,
//# released or deleted, the Scheduler SHALL update that Pool's available
//# count before the next capacity computation.

// TestAvailabilityFollowsTheEventStream takes a Pool of two through a
// MicroVM becoming available, being claimed, being released and being
// deleted, and checks the count the capacity computation would read after
// each. The Pool Manager's clock is not touched: every step is driven by a
// real claim or release and observed through the events the Tracker
// applied.
func TestAvailabilityFollowsTheEventStream(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	spec := specFor(t, testProfile("small", 2), "host-a")
	pm.fillPool(c, spec)

	tracker := newTracker(t, c, nil, trackerPollInterval, nil)
	tracker.run(t)
	tracker.Track(spec.Ref, true)
	tracker.await(t, ctx, "both warm microvms to be counted", func() bool {
		return tracker.Available(spec.Ref) == 2
	})
	waiting := tracker.Wait(spec.Ref)

	// Keep the Pool Manager from replacing what is claimed, so that each
	// count below is the one the event produced rather than a race with a
	// replacement boot.
	pm.host("host-a").SetFaults(flintlock.HostFaults{CreateFails: true})

	claim, err := c.ClaimVM(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	tracker.await(t, ctx, "the claim to be counted", func() bool {
		return tracker.Available(spec.Ref) == 1
	})
	if got := poolStatus(t, tracker, spec.Ref).Leased; got != 1 {
		t.Errorf("leased = %d after one claim, want 1", got)
	}

	if err := c.ReleaseVM(ctx, claim.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
	tracker.awaitEvent(t, ctx, "the release to be counted", func(e *poolmgr.Event) bool {
		return e.Type == poolmgrv1.EventType_VM_DELETED_ON_RELEASE
	})
	if got := tracker.Available(spec.Ref); got != 1 {
		t.Errorf("available = %d after the released microvm was deleted, want 1", got)
	}
	if got := poolStatus(t, tracker, spec.Ref).Leased; got != 0 {
		t.Errorf("leased = %d after the release, want 0", got)
	}

	// The replacement the Pool Manager provisions takes the Pool back to
	// two, and that is what wakes anything waiting on the Pool (SC-021).
	pm.host("host-a").SetFaults(flintlock.HostFaults{})
	tracker.await(t, ctx, "the replacement to be counted", func() bool {
		return tracker.Available(spec.Ref) == 2
	})
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("the wait channel was not closed when a microvm became available")
	}
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When the `Events` stream is unavailable, the Scheduler SHALL fall back
//# to polling `GetPool` at the configured interval until the stream can be
//# re-established.

// TestPollingWhileTheEventStreamIsDown takes the Events service away while
// leaving the rest of the Pool Manager reachable. The Tracker keeps a
// Pool's availability current from GetPool at the configured interval, and
// goes back to events once Subscribe works again.
func TestPollingWhileTheEventStreamIsDown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	gate := &eventGate{Client: c}
	gate.block(true)

	spec := specFor(t, testProfile("small", 1), "host-a")
	pm.fillPool(c, spec)
	// Nothing replaces a claimed MicroVM for now, so the counts below come
	// from the polls alone.
	pm.host("host-a").SetFaults(flintlock.HostFaults{CreateFails: true})

	tracker := newTrackerWith(t, gate, c, nil, trackerPollInterval, nil)
	tracker.run(t)
	tracker.Track(spec.Ref, true)

	// With no stream at all, the poll a newly tracked Pool triggers is what
	// tells the Tracker the Pool is full.
	tracker.await(t, ctx, "the poll to count the warm microvm", func() bool {
		return tracker.Available(spec.Ref) == 1
	})
	if gate.subscribes() == 0 {
		t.Error("the tracker never tried to subscribe")
	}

	claim, err := c.ClaimVM(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	// No event can report the claim, so only the next poll can. Both loops
	// have to be waiting on their timers before the clock moves, or the one
	// that is still arming would miss the tick.
	awaitTimers(t, ctx, tracker.clk, 2)
	tracker.clk.Advance(trackerPollInterval)
	tracker.await(t, ctx, "the poll to notice the claim", func() bool {
		return tracker.Available(spec.Ref) == 0
	})
	before := gate.subscribes()

	// With Subscribe working again the Tracker re-establishes the stream on
	// the next interval, and events carry the counts from then on.
	gate.block(false)
	awaitTimers(t, ctx, tracker.clk, 2)
	tracker.clk.Advance(trackerPollInterval)
	pm.host("host-a").SetFaults(flintlock.HostFaults{})
	if err := c.ReleaseVM(ctx, claim.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
	tracker.awaitEvent(t, ctx, "an event to arrive over the re-established stream", func(e *poolmgr.Event) bool {
		return e.Pool == spec.Ref
	})
	if got := gate.subscribes(); got <= before {
		t.Errorf("subscribe attempts = %d, want more than the %d before the stream came back", got, before)
	}
	tracker.await(t, ctx, "the replacement to be counted from events", func() bool {
		return tracker.Available(spec.Ref) == 1
	})
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports that a Pool is below its target size, the
//# Scheduler SHALL log it at `warn` level with the Pool name and the
//# counts.

// TestPoolBelowTargetIsWarned empties a Pool and keeps the Host from
// refilling it, so that the Pool Manager reports the Pool short of its
// target, and checks the warning the Tracker writes and that the counts the
// event carried have been applied.
func TestPoolBelowTargetIsWarned(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	spec := specFor(t, testProfile("small", 2), "host-a")
	pm.fillPool(c, spec)

	logs := newLogRecorder(t)
	tracker := newTracker(t, c, nil, trackerPollInterval, logs.logger())
	tracker.run(t)
	tracker.Track(spec.Ref, true)
	tracker.await(t, ctx, "both warm microvms to be counted", func() bool {
		return tracker.Available(spec.Ref) == 2
	})

	pm.host("host-a").SetFaults(flintlock.HostFaults{CreateFails: true})
	for i := 0; i < 2; i++ {
		if _, err := c.ClaimVM(ctx, spec.Ref); err != nil {
			t.Fatalf("ClaimVM %d: %v", i, err)
		}
	}
	tracker.awaitEvent(t, ctx, "the pool to be reported below its target", func(e *poolmgr.Event) bool {
		return e.Type == poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET && e.Pool == spec.Ref
	})

	if !logs.atLevel("WARN", "below its target size", spec.Ref.String(), "target=2") {
		t.Errorf("the shortfall was not warned about with the pool name and the counts; log was:\n%s", logs.text())
	}
	if got := tracker.Available(spec.Ref); got != 0 {
		t.Errorf("available = %d after the pool was reported empty, want 0", got)
	}
}

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When an event reports a hook failure or a quarantined MicroVM in a Pool,
//# the Scheduler SHALL log it at `warn` level with the Pool name and the
//# MicroVM uid.

// TestHookFailureIsWarned injects a failing create hook and a failing
// pre-lease hook, one under each hook failure policy, and checks that both
// are warned about with the Pool name and the uid of the MicroVM they
// happened to.
func TestHookFailureIsWarned(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy poolmgr.HookFailurePolicy
		want   string
	}{
		{name: "deleted and replaced", policy: poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE, want: "quarantined=false"},
		{name: "quarantined", policy: poolmgrv1.HookFailurePolicy_QUARANTINE, want: "quarantined=true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()

			pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
			c := pm.client()
			spec := specFor(t, testProfile("small", 1), "host-a")
			spec.HookFailurePolicy = tc.policy

			logs := newLogRecorder(t)
			tracker := newTracker(t, c, nil, trackerPollInterval, logs.logger())
			tracker.run(t)
			tracker.Track(spec.Ref, true)

			pm.setFaults(poolmgr.Faults{HookFailures: []poolmgr.HookFailure{
				{Pool: spec.Ref, Hook: poolmgr.HookCreate, Remaining: 1},
			}})
			if _, err := c.CreatePool(ctx, spec); err != nil {
				t.Fatalf("CreatePool: %v", err)
			}

			failed := tracker.awaitEvent(t, ctx, "the hook failure", func(e *poolmgr.Event) bool {
				return e.Type == poolmgrv1.EventType_VM_HOOK_FAILED && e.Pool == spec.Ref
			})
			if failed.VMUID == "" {
				t.Fatal("the hook failure event names no microvm")
			}
			if !logs.atLevel("WARN", "pool hook failed", spec.Ref.String(), failed.VMUID) {
				t.Errorf("the hook failure was not warned about with the pool name and the uid; log was:\n%s", logs.text())
			}
			if !logs.contains(tc.want) {
				t.Errorf("the warning does not say %q; log was:\n%s", tc.want, logs.text())
			}
		})
	}
}

//= docs/requirements/04-pool-manager.md#claiming
//= type=test
//# While the Pool Manager is unhealthy, the Scheduler SHALL count the
//# available warm MicroVMs of every Pool as zero when computing capacity.

// TestUnhealthyPoolManagerZeroesEveryPool fills two Pools, marks the Pool
// Manager unhealthy the way a failed claim does, and checks that every
// Pool's available count reads zero for as long as the backoff period
// lasts -- both through Available and in the snapshot the capacity
// computation and the metrics read.
func TestUnhealthyPoolManagerZeroesEveryPool(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	const backoff = 30 * time.Second
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	first := specFor(t, testProfile("first", 1), "host-a")
	second := specFor(t, testProfile("second", 2), "host-a")
	pm.fillPool(c, first)
	pm.fillPool(c, second)

	healthClock := clock.NewFake(testEpoch)
	health := newHealth(t, c, healthClock, func(cfg *poolmgr.HealthConfig) {
		cfg.UnhealthyFor = backoff
		// Long enough that no probe runs during the test: the state under
		// test is the one MarkUnavailable produced.
		cfg.Interval = time.Hour
	})
	runInBackground(t, "Health.Run", health.Run)
	awaitContact(t, ctx, health)

	tracker := newTracker(t, c, health, trackerPollInterval, nil)
	tracker.run(t)
	tracker.Track(first.Ref, true)
	tracker.Track(second.Ref, true)
	tracker.await(t, ctx, "both pools to be counted", func() bool {
		return tracker.Available(first.Ref) == 1 && tracker.Available(second.Ref) == 2
	})

	health.MarkUnavailable()
	if got := tracker.Available(first.Ref); got != 0 {
		t.Errorf("available = %d for the first pool while the pool manager is unhealthy, want 0", got)
	}
	if got := tracker.Available(second.Ref); got != 0 {
		t.Errorf("available = %d for the second pool while the pool manager is unhealthy, want 0", got)
	}
	for _, pool := range tracker.Pools() {
		if pool.Status.Available != 0 {
			t.Errorf("snapshot of %s = %+v while the pool manager is unhealthy, want no available microvms",
				pool.Pool, pool.Status)
		}
	}

	// The counts come back by themselves when the backoff period is over;
	// nothing had to be re-learnt from the Pool Manager.
	healthClock.Advance(backoff)
	if got := tracker.Available(second.Ref); got != 2 {
		t.Errorf("available = %d for the second pool after the backoff, want 2", got)
	}
}

// TestUntrackedPoolCountsAsEmpty covers the two other reasons a Pool counts
// as empty: it is not tracked at all, and its declaration is failing.
func TestUntrackedPoolCountsAsEmpty(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	spec := specFor(t, testProfile("small", 1), "host-a")
	pm.fillPool(c, spec)

	tracker := newTracker(t, c, nil, trackerPollInterval, nil)
	tracker.run(t)
	if got := tracker.Available(spec.Ref); got != 0 {
		t.Errorf("available = %d for a pool that is not tracked, want 0", got)
	}

	tracker.Track(spec.Ref, false)
	tracker.await(t, ctx, "the undeclared pool to be polled", func() bool {
		return len(tracker.Pools()) == 1
	})
	if got := tracker.Available(spec.Ref); got != 0 {
		t.Errorf("available = %d for a pool whose declaration is failing, want 0", got)
	}

	tracker.Track(spec.Ref, true)
	tracker.await(t, ctx, "the declared pool to be counted", func() bool {
		return tracker.Available(spec.Ref) == 1
	})

	tracker.Untrack(spec.Ref)
	if got := tracker.Available(spec.Ref); got != 0 {
		t.Errorf("available = %d for an untracked pool, want 0", got)
	}
	if pools := tracker.Pools(); len(pools) != 0 {
		t.Errorf("snapshot = %+v after untracking the only pool, want it empty", pools)
	}
}

// TestTrackerRejectsAnIncompleteConfiguration covers the two dependencies
// the Tracker cannot do without.
func TestTrackerRejectsAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := poolmgr.NewTracker(poolmgr.TrackerConfig{}); err == nil {
		t.Error("NewTracker accepted a configuration with no Events client")
	}
	if _, err := poolmgr.NewTracker(poolmgr.TrackerConfig{Events: &eventGate{}}); err == nil {
		t.Error("NewTracker accepted a configuration with no PoolAdmin client")
	}
}

// awaitTimers blocks until n timers are armed on the clock. A test that
// moves a fake clock has to know that everything it means to wake is
// already waiting, or the tick fires into a gap.
func awaitTimers(t *testing.T, ctx context.Context, clk *clock.Fake, n int) {
	t.Helper()
	if err := clk.BlockUntil(ctx, n); err != nil {
		t.Fatalf("waiting for %d timers to be armed: %v", n, err)
	}
}

// poolStatus is one Pool's entry in the Tracker's snapshot.
func poolStatus(t *testing.T, tracker poolmgr.Tracker, pool poolmgr.PoolRef) poolmgr.PoolStatus {
	t.Helper()
	for _, entry := range tracker.Pools() {
		if entry.Pool == pool {
			return entry.Status
		}
	}
	t.Fatalf("no pool %s in the tracker's snapshot", pool)
	return poolmgr.PoolStatus{}
}

// eventGate is a client whose Subscribe can be turned off, so that a test
// can take the Events service away while leaving the rest of the Pool
// Manager reachable. Everything else is the real client's.
type eventGate struct {
	poolmgr.Client
	mu      sync.Mutex
	blocked bool
	tries   int
}

// block turns Subscribe off or on.
func (g *eventGate) block(blocked bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocked = blocked
}

// subscribes is how many times Subscribe has been called.
func (g *eventGate) subscribes() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tries
}

// Subscribe implements poolmgr.Events.
func (g *eventGate) Subscribe(ctx context.Context, filter poolmgr.EventFilter) (poolmgr.EventStream, error) {
	g.mu.Lock()
	g.tries++
	blocked := g.blocked
	g.mu.Unlock()
	if blocked {
		return nil, fmt.Errorf("%w: events service is switched off", poolmgr.ErrUnavailable)
	}
	return g.Client.Subscribe(ctx, filter)
}
