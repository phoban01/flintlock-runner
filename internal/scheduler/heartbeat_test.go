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

//= docs/requirements/03-scheduler.md#lease-keep-alive
//= type=test
//# While an Allocation holds a Lease, the Scheduler SHALL send a
//# heartbeat to the Pool Manager at an interval no longer than half the time
//# remaining until the Lease expiry it last received.

func TestHeartbeatDelayIsNeverMoreThanHalfTheRemainingLease(t *testing.T) {
	t.Parallel()
	now := testEpoch
	tests := []struct {
		name     string
		expiry   time.Duration
		interval time.Duration
		attempt  int
		want     time.Duration
	}{
		{name: "half the remaining lease", expiry: time.Minute, interval: time.Hour, want: 30 * time.Second},
		{name: "the pool interval when it is shorter", expiry: time.Minute, interval: 10 * time.Second, want: 10 * time.Second},
		{name: "half wins over a longer interval", expiry: 10 * time.Second, interval: 10 * time.Second, want: 5 * time.Second},
		{name: "a failure retries within the backoff", expiry: time.Minute, interval: time.Hour, attempt: 1, want: time.Second},
		{name: "an almost expired lease is retried at once", expiry: time.Nanosecond, interval: time.Hour, want: minHeartbeatDelay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lease := Lease{ExpiresAt: now.Add(tt.expiry)}
			got := heartbeatDelay(lease, now, tt.interval, clock.Exponential{Base: time.Second}, tt.attempt)
			if got != tt.want {
				t.Fatalf("heartbeatDelay = %v, want %v", got, tt.want)
			}
			if remaining := tt.expiry; got > remaining/2 && remaining > 2*minHeartbeatDelay {
				t.Fatalf("heartbeatDelay = %v, more than half of the remaining %v", got, remaining)
			}
		})
	}
}

func TestHeartbeatKeepsTheLeaseAliveWithinHalfTheExpiry(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	e := newEnv(t, envConfig{client: client})
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(e.clk, 30*time.Second)
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	h := e.allocate(ctx, JobInfo{ID: 1}, "default")
	defer e.sched.Release(h)

	for round := 0; round < 3; round++ {
		lease := h.Allocation().Lease
		if err := e.clk.BlockUntil(ctx, 1); err != nil {
			t.Fatalf("waiting for the heartbeat timer: %v", err)
		}
		deadlines := e.clk.Deadlines()
		if len(deadlines) != 1 {
			t.Fatalf("pending timers = %d, want the heartbeat's alone", len(deadlines))
		}
		half := e.clk.Now().Add(lease.ExpiresAt.Sub(e.clk.Now()) / 2)
		if deadlines[0].After(half) {
			t.Fatalf("round %d: heartbeat armed for %v, later than half the remaining lease at %v",
				round, deadlines[0], half)
		}
		before := client.beatCount()
		e.clk.Advance(deadlines[0].Sub(e.clk.Now()))
		waitFor(t, ctx, func() bool { return client.beatCount() > before })
	}

	// Every heartbeat pushed the expiry out.
	if got := h.Allocation().Lease.ExpiresAt; !got.After(testEpoch.Add(30 * time.Second)) {
		t.Fatalf("lease expiry = %v, want it extended past the first one", got)
	}
	if h.Err() != nil {
		t.Fatalf("handle failed with %v while heartbeats succeeded", h.Err())
	}
}

//= docs/requirements/03-scheduler.md#lease-keep-alive
//= type=test
//# If a heartbeat reports that the Lease no longer exists, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and drop the Allocation without a release call.

func TestLeaseGoneAtTheHeartbeatAbortsTheJobWithoutReleasing(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	host := newFakeHost(t, "host-1")
	pm, raw := newFakePoolManager(t, ctx, map[string]flintlock.PoolHostClient{"host-1": host.Client()})
	client := newCountingClient(raw)

	e := newEnv(t, envConfig{
		client: client,
		hosts:  map[string]flintlock.HostClient{"host-1": runnerClient(t, host)},
	})
	e.declarer.admin = raw
	e.startBare(ctx)
	e.sched.declareAll(ctx)
	pool := e.poolOf("default")
	waitForClaimable(t, ctx, raw, pool)
	e.tracker.setAvailable(pool, 1)

	h := e.allocate(ctx, JobInfo{ID: 21}, "default")

	// The Lease disappears at the Pool Manager, which is what a lease that
	// expired during a Job looks like to the next heartbeat.
	pm.SetFaults(poolmgr.Faults{RefuseHeartbeats: true})
	e.advance(ctx, 1, 20*time.Second)

	select {
	case <-h.Done():
	case <-ctx.Done():
		t.Fatal("the handle was not failed after the lease went away")
	}
	if !errors.Is(h.Err(), ErrLeaseLost) {
		t.Fatalf("handle error = %v, want ErrLeaseLost", h.Err())
	}
	// The Slot is returned without a release call: the lease the Runner would
	// name is the one that no longer exists.
	waitFor(t, ctx, func() bool { return e.sched.Snapshot().SlotsInUse == 0 })
	if got := client.releaseCount(); got != 0 {
		t.Fatalf("release calls = %d, want 0", got)
	}

	// The Executor's Cleanup still calls Release; it must stay a no-op.
	e.sched.Release(h)
	if got := client.releaseCount(); got != 0 {
		t.Fatalf("release calls after Release on a lost lease = %d, want 0", got)
	}
}

//= docs/requirements/03-scheduler.md#lease-keep-alive
//= type=test
//# If heartbeats fail for longer than the Lease expiry, then the
//# Scheduler SHALL treat the Lease as lost and abort the Job.

func TestHeartbeatsFailingPastTheExpiryLoseTheLease(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	// The Pool Manager answers every heartbeat with an error that is not
	// NOT_FOUND, so the Lease is only lost by running out of time.
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = func(context.Context, string) (time.Time, error) {
			return time.Time{}, errors.New("connection reset")
		}
	})
	e := newEnv(t, envConfig{client: client, backoff: clock.Exponential{Base: 5 * time.Second}})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	h := e.allocate(ctx, JobInfo{ID: 22}, "default")
	expiry := h.Allocation().Lease.ExpiresAt

	// Walk the clock past the expiry, letting every heartbeat fail.
	for e.clk.Now().Before(expiry) && h.Err() == nil {
		if err := e.clk.BlockUntil(ctx, 1); err != nil {
			t.Fatalf("waiting for the heartbeat timer: %v", err)
		}
		e.clk.Advance(10 * time.Second)
	}

	select {
	case <-h.Done():
	case <-ctx.Done():
		t.Fatal("the handle was not failed after heartbeats failed past the expiry")
	}
	if !errors.Is(h.Err(), ErrLeaseLost) {
		t.Fatalf("handle error = %v, want ErrLeaseLost", h.Err())
	}
	if got := e.metrics.failure(FailureHeartbeat); got == 0 {
		t.Fatal("no heartbeat failure was counted")
	}
	waitFor(t, ctx, func() bool { return e.sched.Snapshot().SlotsInUse == 0 })
	if got := client.releaseCount(); got != 0 {
		t.Fatalf("release calls for an expired lease = %d, want 0", got)
	}
}

// TestReleaseDuringAnInFlightHeartbeatDoesNotLoseTheLease pins the SC-061
// boundary: the requirement is about a heartbeat finding the Lease gone under
// a live Allocation, not about one that races the Runner's own release. The
// keep-alive call outlives its cancellation on purpose, so Release runs
// stopHeartbeat and ReleaseVM while a heartbeat is still in flight; the
// NOT_FOUND that comes back is the Runner's own release, and treating it as a
// lost Lease would fail the Handle of a Job that finished cleanly.
func TestReleaseDuringAnInFlightHeartbeatDoesNotLoseTheLease(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	inFlight := make(chan struct{})
	proceed := make(chan struct{})
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		// The first heartbeat blocks until the test has released the Lease,
		// and then answers as the Pool Manager does for a Lease that has
		// just been released.
		c.beatFn = func(context.Context, string) (time.Time, error) {
			close(inFlight)
			<-proceed
			return time.Time{}, poolmgr.ErrNotFound
		}
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	h := e.allocate(ctx, JobInfo{ID: 61}, "default")

	// Fire the keep-alive timer and wait for the call to be in flight.
	e.advance(ctx, 1, 10*time.Second)
	select {
	case <-inFlight:
	case <-ctx.Done():
		t.Fatal("the heartbeat call was never made")
	}

	// The Job finished: Release returns the Slot and hands the Lease back
	// while that heartbeat is still waiting for an answer.
	e.sched.Release(h)
	close(proceed)

	// Let the keep-alive loop and the background release finish.
	e.sched.closeBackground()
	e.sched.bgCancel()
	e.sched.wg.Wait()

	select {
	case <-h.Done():
		t.Fatalf("Done was closed with %v after a normal Release", h.Err())
	default:
	}
	if err := h.Err(); err != nil {
		t.Fatalf("handle error after a normal Release = %v, want nil", err)
	}
	if got := len(e.logs.find("lease lost, aborting the job")); got != 0 {
		t.Fatalf("%d lease-lost error lines after a normal Release, want 0", got)
	}
}
