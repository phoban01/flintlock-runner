package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/03-scheduler.md#release
//= type=test
//# When a Job finishes, the Scheduler SHALL release its Lease with
//# the Pool Manager and SHALL NOT delete the MicroVM itself.

func TestReleaseHandsTheLeaseBackAndDeletesNothing(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	host := newFakeHost(t, "host-1")
	pm, raw := newFakePoolManager(t, ctx, map[string]flintlock.PoolHostClient{"host-1": host.Client()})
	client := newCountingClient(raw)

	e := newEnv(t, envConfig{client: client, hosts: map[string]flintlock.HostClient{"host-1": runnerClient(t, host)}})
	e.declarer.admin = raw
	e.startBare(ctx)
	e.sched.declareAll(ctx)
	pool := e.poolOf("default")
	waitForClaimable(t, ctx, raw, pool)
	e.tracker.setAvailable(pool, 1)

	h := e.allocate(ctx, JobInfo{ID: 31}, "default")
	alloc := h.Allocation()
	if len(pm.Leases()) != 1 {
		t.Fatalf("leases held = %d, want 1", len(pm.Leases()))
	}

	e.sched.Release(h)
	if got := client.awaitRelease(t, ctx); got != alloc.Lease.ID {
		t.Fatalf("released lease %q, want %q", got, alloc.Lease.ID)
	}
	waitFor(t, ctx, func() bool { return len(pm.Leases()) == 0 })

	// The Runner never deletes a MicroVM; the Pool Manager does that when it
	// reconciles the released one.
	if _, ok := e.registry.clients["host-1"].(flintlock.HostAdminClient); ok {
		t.Fatal("the runner-side host client exposes the admin methods")
	}
}

//= docs/requirements/03-scheduler.md#release
//= type=test
//# The Scheduler SHALL return the Slot of a released MicroVM as
//# soon as the release has been requested rather than when it completes.

func TestReleaseReturnsTheSlotBeforeTheCallCompletes(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	inRelease := make(chan struct{})
	finish := make(chan struct{})
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
		c.releaseFn = func(context.Context, string) error {
			close(inRelease)
			<-finish
			return nil
		}
	})
	e := newEnv(t, envConfig{client: client, tune: func(s *Settings) { s.Slots = 1 }})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 5)

	h := e.allocate(ctx, JobInfo{ID: 32}, "default")
	e.sched.Release(h)

	// Release has returned; the ReleaseVM call has not.
	<-inRelease
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use while the release is still in flight = %d, want 0", got)
	}
	if _, err := e.sched.Reserve(ctx); err != nil {
		t.Fatalf("Reserve while the release is in flight: %v", err)
	}
	close(finish)
}

//= docs/requirements/03-scheduler.md#release
//= type=test
//# If a release fails, then the Scheduler SHALL retry it with
//# exponential backoff up to the configured retry limit and SHALL then log
//# the Lease id and rely on Lease expiry.

func TestReleaseRetriesThenReliesOnLeaseExpiry(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
		c.releaseFn = func(context.Context, string) error { return errors.New("pool manager said no") }
	})
	e := newEnv(t, envConfig{
		client:  client,
		backoff: clock.Exponential{Base: time.Second, Max: 4 * time.Second},
		tune:    func(s *Settings) { s.PoolManager.ReleaseRetryLimit = 2 },
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	h := e.allocate(ctx, JobInfo{ID: 33}, "default")
	lease := h.Allocation().Lease.ID
	e.sched.Release(h)

	// One attempt, then a backoff, a second attempt, another backoff and a
	// third: the limit is the number of retries after the first call.
	waitFor(t, ctx, func() bool { return client.releaseCount() == 1 })
	for attempt := 1; attempt <= 2; attempt++ {
		e.advance(ctx, 1, 10*time.Second)
		want := attempt + 1
		waitFor(t, ctx, func() bool { return client.releaseCount() == want })
	}

	waitFor(t, ctx, func() bool { return len(e.logs.find("relying on lease expiry")) == 1 })
	if got := client.releaseCount(); got != 3 {
		t.Fatalf("release attempts = %d, want 3", got)
	}
	if got := e.metrics.failure(FailureRelease); got != 3 {
		t.Fatalf("release failures counted = %d, want 3", got)
	}
	record := e.logs.find("relying on lease expiry")[0]
	if record.attrs["lease"] != lease {
		t.Fatalf("the give-up log names lease %q, want %q", record.attrs["lease"], lease)
	}
}

func TestReleaseTreatsAnUnknownLeaseAsReleased(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
		c.releaseFn = func(context.Context, string) error { return poolmgr.ErrNotFound }
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	h := e.allocate(ctx, JobInfo{ID: 34}, "default")
	e.sched.Release(h)
	waitFor(t, ctx, func() bool { return client.releaseCount() == 1 })

	// Nothing is retried and nothing is counted as a failure.
	if got := e.metrics.failure(FailureRelease); got != 0 {
		t.Fatalf("release failures counted = %d, want 0", got)
	}
}

//= docs/requirements/03-scheduler.md#release
//= type=test
//# When starting, the Scheduler SHALL NOT attempt to find or clean up
//# MicroVMs from a previous incarnation, because Leases it no longer
//# heartbeats expire and the Pool Manager deletes them.

func TestStartupLeavesAPreviousIncarnationsLeaseAlone(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	host := newFakeHost(t, "host-1")
	pm, raw := newFakePoolManager(t, ctx, map[string]flintlock.PoolHostClient{"host-1": host.Client()})
	client := newCountingClient(raw)

	e := newEnv(t, envConfig{client: client, hosts: map[string]flintlock.HostClient{"host-1": runnerClient(t, host)}})
	e.declarer.admin = raw

	// A previous incarnation of this Runner declared the Pool and holds a
	// Lease on one of its MicroVMs.
	e.sched.declareAll(ctx)
	pool := e.poolOf("default")
	waitForClaimable(t, ctx, raw, pool)
	previous, err := raw.ClaimVM(ctx, pool)
	if err != nil {
		t.Fatalf("the previous incarnation could not claim: %v", err)
	}

	// This incarnation starts.
	e.health.contact()
	e.run(ctx)
	waitFor(t, ctx, func() bool { return len(e.declarer.declared()) > 1 })

	// The old Lease is untouched: nothing was released and nothing was
	// deleted, and the Runner never asked a Host what it holds.
	leases := pm.Leases()
	if len(leases) != 1 || leases[0].LeaseID != previous.LeaseID {
		t.Fatalf("leases at the pool manager = %+v, want the previous incarnation's %s", leases, previous.LeaseID)
	}
	if got := client.releaseCount(); got != 0 {
		t.Fatalf("release calls at startup = %d, want 0", got)
	}
	if _, err := host.Client().GetMicroVM(ctx, previous.VMUID); err != nil {
		t.Fatalf("the previous incarnation's microvm is gone: %v", err)
	}
}

//= docs/requirements/03-scheduler.md#release
//= type=test
//# Where the keep-on-failure debug option is enabled, the Scheduler
//# SHALL keep heartbeating the Lease of a failed Job for the configured keep
//# duration before releasing it, and SHALL log the MicroVM uid and Host name.

func TestRetainKeepsHeartbeatingThenReleases(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	e := newEnv(t, envConfig{
		client: client,
		tune: func(s *Settings) {
			s.Scheduler.KeepOnFailure = true
			s.Scheduler.KeepDuration = 10 * time.Minute
			s.Slots = 1
		},
	})
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(e.clk, 30*time.Second)
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 5)

	h := e.allocate(ctx, JobInfo{ID: 35}, "default")
	alloc := h.Allocation()
	e.sched.Retain(h)

	// The Slot comes back at once, so the next Job can start while the failed
	// Job's MicroVM is kept.
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use after Retain = %d, want 0", got)
	}
	if _, err := e.sched.Reserve(ctx); err != nil {
		t.Fatalf("Reserve after Retain: %v", err)
	}

	records := e.logs.find("keeping the microvm")
	if len(records) != 1 {
		t.Fatalf("keep log records = %d, want 1", len(records))
	}
	if records[0].attrs["vm"] != alloc.VMUID || records[0].attrs["host"] != alloc.Placement.Host {
		t.Fatalf("keep log = %v, want the microvm uid and host name", records[0].attrs)
	}

	// The Lease is kept alive while it is retained: the heartbeat and the
	// keep timers are both armed, and heartbeats keep being answered.
	beats := client.beatCount()
	e.advance(ctx, 2, 20*time.Second)
	waitFor(t, ctx, func() bool { return client.beatCount() > beats })
	if client.releaseCount() != 0 {
		t.Fatalf("release calls during the keep duration = %d, want 0", client.releaseCount())
	}

	// When the keep duration elapses the Lease is released.
	e.clk.Advance(10 * time.Minute)
	waitFor(t, ctx, func() bool { return client.releaseCount() == 1 })
	if got := client.releasedLeases(); got[0] != alloc.Lease.ID {
		t.Fatalf("released lease = %q, want %q", got[0], alloc.Lease.ID)
	}
}

func TestRetainWithoutKeepOnFailureReleasesAtOnce(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	h := e.allocate(ctx, JobInfo{ID: 36}, "default")
	e.sched.Retain(h)
	waitFor(t, ctx, func() bool { return client.releaseCount() == 1 })
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use = %d, want 0", got)
	}
}

func TestReleaseTwiceMakesOneCall(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	h := e.allocate(ctx, JobInfo{ID: 37}, "default")
	e.sched.Release(h)
	e.sched.Release(h)
	waitFor(t, ctx, func() bool { return client.releaseCount() == 1 })
	if got := client.releaseCount(); got != 1 {
		t.Fatalf("release calls = %d, want 1", got)
	}
}
