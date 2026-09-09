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

//= docs/requirements/03-scheduler.md#allocation
//= type=test
//# When an Allocation is requested for a Profile, the Scheduler
//# SHALL claim a warm MicroVM from the Profile's Pool.

func TestAllocateClaimsFromTheProfilesPool(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	host := newFakeHost(t, "host-1")
	pm, client := newFakePoolManager(t, ctx, map[string]flintlock.PoolHostClient{"host-1": host.Client()})

	e := newEnv(t, envConfig{
		client:   client,
		profiles: profileFixtures(),
		hosts:    map[string]flintlock.HostClient{"host-1": runnerClient(t, host)},
	})
	e.declarer.admin = client
	e.startBare(ctx)
	e.sched.declareAll(ctx)

	pool := e.poolOf("rust")
	waitForClaimable(t, ctx, client, pool)
	e.tracker.setAvailable(pool, 1)

	h := e.allocate(ctx, JobInfo{ID: 77, Image: "rust:1.90"}, "rust")
	defer e.sched.Release(h)

	alloc := h.Allocation()
	if alloc.Lease.Pool != pool {
		t.Fatalf("claimed from %s, want the profile's pool %s", alloc.Lease.Pool, pool)
	}

	leases := pm.Leases()
	if len(leases) != 1 {
		t.Fatalf("leases at the pool manager = %d, want 1", len(leases))
	}
	if leases[0].LeaseID != alloc.Lease.ID || leases[0].VMUID != alloc.VMUID {
		t.Fatalf("lease at the pool manager = %+v, want the allocation's %s/%s",
			leases[0], alloc.Lease.ID, alloc.VMUID)
	}
	if leases[0].Pool != pool {
		t.Fatalf("lease pool = %s, want %s", leases[0].Pool, pool)
	}

	// The MicroVM the Pool Manager leased is one of that Pool's warm MicroVMs.
	var found bool
	for _, vm := range pm.VMs() {
		if vm.UID == alloc.VMUID {
			found = vm.Pool == pool
		}
	}
	if !found {
		t.Fatalf("microvm %s is not one of pool %s", alloc.VMUID, pool)
	}
}

//= docs/requirements/03-scheduler.md#allocation
//= type=test
//# If the Pool has no warm MicroVM available, then the Scheduler
//# SHALL retry the claim with exponential backoff, waking early on a Pool
//# Manager event that reports a MicroVM in that Pool becoming available,
//# until the allocation timeout elapses.

func TestAllocateRetriesAndWakesOnAnAvailabilityEvent(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	// The Pool is exhausted until the test says otherwise. The backoff is an
	// hour and the clock never moves, so the only thing that can produce the
	// second attempt is the Tracker's wake-up.
	exhausted := make(chan struct{})
	client := newStubClient()
	claims := claimsFrom("host-1")
	client.script(func(c *stubClient) {
		c.claimFn = func(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
			select {
			case <-exhausted:
				return claims(ctx, ref)
			default:
				return nil, poolmgr.ErrExhausted
			}
		}
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client, backoff: clock.Exponential{Base: time.Hour}})
	e.startBare(ctx)
	pool := e.poolOf("default")
	e.tracker.setAvailable(pool, 1)

	r := e.reserve(ctx)
	type result struct {
		h   Handle
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := e.sched.Allocate(ctx, r, JobInfo{ID: 8}, e.profile("default"))
		done <- result{h, err}
	}()

	// The first claim is refused, the Pool is marked exhausted and the
	// allocation waits on the backoff timer and the wake-up channel.
	if err := e.clk.BlockUntil(ctx, 2); err != nil {
		t.Fatalf("waiting for the allocation and backoff timers: %v", err)
	}
	if got := e.tracker.exhaustedCount(pool); got == 0 {
		t.Fatal("the pool was not marked exhausted after RESOURCE_EXHAUSTED")
	}
	select {
	case got := <-done:
		t.Fatalf("Allocate finished with %v, %v while the pool was exhausted", got.h, got.err)
	default:
	}

	// An event reports a MicroVM becoming available: the wait ends early,
	// without the backoff having elapsed.
	close(exhausted)
	e.tracker.wake(pool)

	got := <-done
	if got.err != nil {
		t.Fatalf("Allocate after the wake-up: %v", got.err)
	}
	if e.clk.Now() != testEpoch {
		t.Fatalf("the clock moved to %v; the retry did not come from the wake-up", e.clk.Now())
	}
	if n := client.claimCount(); n != 2 {
		t.Fatalf("claim attempts = %d, want 2", n)
	}
	if waits := e.metrics.waitedOn(pool); len(waits) != 1 {
		t.Fatalf("pool waits recorded = %v, want one", waits)
	}
	warnings := e.logs.find("pool is exhausted")
	if len(warnings) != 1 || warnings[0].attrs["waiting"] != "1" {
		t.Fatalf("exhaustion warnings = %+v, want one naming the number waiting", warnings)
	}
	e.sched.Release(got.h)
}

//= docs/requirements/03-scheduler.md#allocation
//= type=test
//# If the allocation timeout elapses before a MicroVM is claimed,
//# then the Scheduler SHALL return an allocation error naming the Profile and
//# the Pool.

func TestAllocateTimesOutNamingTheProfileAndPool(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient() // every claim is refused as exhausted
	e := newEnv(t, envConfig{
		client:  client,
		backoff: clock.Exponential{Base: time.Second, Max: time.Second},
		tune:    func(s *Settings) { s.Scheduler.AllocationTimeout = 30 * time.Second },
	})
	e.startBare(ctx)
	pool := e.poolOf("default")
	e.tracker.setAvailable(pool, 1)

	r := e.reserve(ctx)
	errCh := make(chan error, 1)
	go func() {
		_, err := e.sched.Allocate(ctx, r, JobInfo{ID: 4}, e.profile("default"))
		errCh <- err
	}()

	// Two timers are armed: the allocation timeout and the backoff. Moving
	// past the timeout ends the allocation.
	e.advance(ctx, 2, 31*time.Second)

	err := <-errCh
	if !errors.Is(err, ErrAllocationTimeout) {
		t.Fatalf("Allocate error = %v, want ErrAllocationTimeout", err)
	}
	var aerr *AllocationError
	if !errors.As(err, &aerr) {
		t.Fatalf("error %v is not an *AllocationError", err)
	}
	if aerr.Profile != "default" || aerr.Pool != pool {
		t.Fatalf("error names profile %q and pool %s, want default and %s", aerr.Profile, aerr.Pool, pool)
	}
	if !contains(err.Error(), "default") || !contains(err.Error(), pool.String()) {
		t.Fatalf("error message %q names neither the profile nor the pool", err.Error())
	}
	// The Slot is still the caller's Reservation.
	if got := e.sched.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("slots in use = %d, want the reservation still held", got)
	}
}

//= docs/requirements/03-scheduler.md#allocation
//= type=test
//# When the Job's context is cancelled during allocation, the
//# Scheduler SHALL stop the allocation and release any Lease that was
//# obtained.

func TestCancelledJobContextReleasesTheLeaseObtained(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	jobCtx, cancelJob := context.WithCancel(ctx)
	client := newStubClient()
	client.script(func(c *stubClient) {
		// The claim succeeds, but the Job is cancelled while it is in flight,
		// which is the race the requirement is about.
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
			cancelJob()
			return &poolmgr.Claim{
				LeaseID: "lease-cancel",
				VMUID:   "vm-cancel",
				Host:    poolmgr.HostRef{Name: "host-1", Address: "host-1:9090"},
			}, nil
		}
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	h, err := e.sched.Allocate(jobCtx, r, JobInfo{ID: 12}, e.profile("default"))
	if h != nil {
		t.Fatalf("Allocate returned a handle, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Allocate error = %v, want context.Canceled", err)
	}
	if got := waitForRelease(t, ctx, client); got != "lease-cancel" {
		t.Fatalf("released leases = %v, want lease-cancel", got)
	}
	if got := e.sched.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("slots in use = %d, want the reservation still held for the caller", got)
	}
}

// TestCancelledAllocationDoesNotWaitForTheAbandonedRelease holds the "stop
// the allocation" half of SC-023. Allocate holds the caller's Reservation
// until it returns, so a release made on the calling goroutine keeps the
// worker and its Slot for the whole retry ladder: up to ReleaseRetryLimit
// attempts, each bounded by the Pool Manager deadline, with backoff between
// them, and none of it interruptible by the Job's own context, because the
// release call deliberately survives cancellation.
func TestCancelledAllocationDoesNotWaitForTheAbandonedRelease(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()

	inRelease := make(chan struct{})
	holdRelease := make(chan struct{})
	// The release is let go before the test's cleanup waits for the
	// background goroutines.
	defer close(holdRelease)

	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
			cancelJob()
			return &poolmgr.Claim{
				LeaseID: "lease-abandoned",
				VMUID:   "vm-abandoned",
				Host:    poolmgr.HostRef{Name: "host-1", Address: "host-1:9090"},
			}, nil
		}
		// A Pool Manager that never answers the release, which is what the
		// retry ladder is for.
		c.releaseFn = func(context.Context, string) error {
			close(inRelease)
			<-holdRelease
			return nil
		}
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := e.sched.Allocate(jobCtx, r, JobInfo{ID: 23}, e.profile("default"))
		done <- err
	}()

	// The abandoned Lease is being released; if that release were inline,
	// Allocate would be sitting inside it.
	select {
	case <-inRelease:
	case <-ctx.Done():
		t.Fatal("the abandoned lease was never released")
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Allocate error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Allocate is still blocked while the abandoned lease is being released")
	}

	// The Slot is the caller's to give back the moment Allocate returned.
	if got := e.sched.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("slots in use = %d, want the reservation still held for the caller", got)
	}
	e.sched.ReleaseReservation(r)
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use after releasing the reservation = %d, want 0", got)
	}
}

func TestCancelledJobContextStopsAWaitingAllocation(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	jobCtx, cancelJob := context.WithCancel(ctx)
	client := newStubClient() // always exhausted
	e := newEnv(t, envConfig{client: client, backoff: clock.Exponential{Base: time.Hour}})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	errCh := make(chan error, 1)
	go func() {
		_, err := e.sched.Allocate(jobCtx, r, JobInfo{ID: 13}, e.profile("default"))
		errCh <- err
	}()
	if err := e.clk.BlockUntil(ctx, 2); err != nil {
		t.Fatalf("waiting for the allocation timers: %v", err)
	}
	cancelJob()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Allocate error = %v, want context.Canceled", err)
	}
	if client.releaseCount() != 0 {
		t.Fatalf("release calls = %d, want none: no lease was obtained", client.releaseCount())
	}
}

//= docs/requirements/03-scheduler.md#allocation
//= type=test
//# The Scheduler SHALL record for every Allocation the Job id, the
//# Profile name, the MicroVM uid, the Lease id and the Placement.

func TestAllocationRecordsEveryIdentifier(t *testing.T) {
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

	h := e.allocate(ctx, JobInfo{ID: 4711, Image: "whatever"}, "default")
	defer e.sched.Release(h)

	got := h.Allocation()
	if got.JobID != 4711 {
		t.Errorf("job id = %d, want 4711", got.JobID)
	}
	if got.Profile != "default" {
		t.Errorf("profile = %q, want default", got.Profile)
	}
	if got.VMUID != "vm-1" {
		t.Errorf("microvm uid = %q, want vm-1", got.VMUID)
	}
	if got.Lease.ID != "lease-1" {
		t.Errorf("lease id = %q, want lease-1", got.Lease.ID)
	}
	if got.Placement.Host != "host-1" {
		t.Errorf("placement host = %q, want host-1", got.Placement.Host)
	}
	if got.ClaimedAt != testEpoch {
		t.Errorf("claimed at = %v, want %v", got.ClaimedAt, testEpoch)
	}
	if got.Lease.ExpiresAt != testEpoch.Add(30*time.Second) {
		t.Errorf("lease expiry = %v, want the pool's expiry threshold after the claim", got.Lease.ExpiresAt)
	}
	if got.Host.Name != "host-1" || got.Host.Address != "host-1:9090" {
		t.Errorf("claimed host = %+v, want host-1 with its address", got.Host)
	}
}

//= docs/requirements/03-scheduler.md#allocation
//= type=test
//# The Scheduler SHALL log every allocation with the Job id,
//# Profile, Host name, MicroVM uid and elapsed time from claim to Placement.

func TestAllocationIsLoggedWithJobProfileHostAndElapsed(t *testing.T) {
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

	h := e.allocate(ctx, JobInfo{ID: 99}, "default")
	defer e.sched.Release(h)

	records := e.logs.find("microvm allocated")
	if len(records) != 1 {
		t.Fatalf("allocation log records = %d, want 1", len(records))
	}
	attrs := records[0].attrs
	for _, want := range []struct{ key, value string }{
		{"job", "99"},
		{"profile", "default"},
		{"host", "host-1"},
		{"vm", "vm-1"},
		{"elapsed", "0s"},
	} {
		if attrs[want.key] != want.value {
			t.Errorf("log attribute %q = %q, want %q (record: %v)", want.key, attrs[want.key], want.value, attrs)
		}
	}
	if len(e.metrics.allocations) != 1 {
		t.Fatalf("allocation durations observed = %d, want 1", len(e.metrics.allocations))
	}
}

//= docs/requirements/03-scheduler.md#allocation
//= type=test
//# The Scheduler SHALL convert a Reservation into an Allocation
//# without releasing the Slot in between.

func TestConversionNeverFreesTheSlot(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	// The Runner has exactly one Slot. While the only Reservation is being
	// converted, a second worker asking for one has to be refused: if the
	// conversion released the Slot first, this Reserve would succeed and the
	// Runner would take a Job it cannot place.
	inClaim := make(chan struct{})
	proceed := make(chan struct{})
	client := newStubClient()
	claims := claimsFrom("host-1")
	client.script(func(c *stubClient) {
		c.claimFn = func(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
			close(inClaim)
			<-proceed
			return claims(ctx, ref)
		}
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client, tune: func(s *Settings) { s.Slots = 1 }})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 5)

	r := e.reserve(ctx)
	done := make(chan Handle, 1)
	go func() {
		h, err := e.sched.Allocate(ctx, r, JobInfo{ID: 1}, e.profile("default"))
		if err != nil {
			t.Errorf("Allocate: %v", err)
			done <- nil
			return
		}
		done <- h
	}()

	<-inClaim
	if _, err := e.sched.Reserve(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("Reserve during the conversion = %v, want a refusal: the slot is held", err)
	}
	close(proceed)

	h := <-done
	if h == nil {
		t.Fatal("no handle")
	}
	if got := e.sched.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("slots in use after the conversion = %d, want 1", got)
	}
	if _, err := e.sched.Reserve(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("Reserve after the conversion = %v, want a refusal", err)
	}
	if h.Allocation().ReservationID != r.ID {
		t.Fatalf("allocation reservation id = %d, want %d", h.Allocation().ReservationID, r.ID)
	}
	e.sched.Release(h)
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use after release = %d, want 0", got)
	}
}

func TestAllocateRedeclaresAnUnknownPool(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	var attempts int
	client := newStubClient()
	claims := claimsFrom("host-1")
	client.script(func(c *stubClient) {
		c.claimFn = func(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
			attempts++
			if attempts == 1 {
				return nil, poolmgr.ErrNotFound
			}
			return claims(ctx, ref)
		}
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client, backoff: clock.Exponential{Base: time.Second}})
	e.startBare(ctx)
	pool := e.poolOf("default")
	e.tracker.setAvailable(pool, 1)

	r := e.reserve(ctx)
	done := make(chan Handle, 1)
	go func() {
		h, err := e.sched.Allocate(ctx, r, JobInfo{ID: 3}, e.profile("default"))
		if err != nil {
			t.Errorf("Allocate: %v", err)
		}
		done <- h
	}()
	e.advance(ctx, 2, 2*time.Second)

	h := <-done
	if h == nil {
		t.Fatal("no handle")
	}
	defer e.sched.Release(h)
	if got := e.declarer.declared(); len(got) != 1 || got[0].Ref != pool {
		t.Fatalf("declarations after NOT_FOUND = %+v, want one for %s", got, pool)
	}
}

func TestAllocateMarksThePoolManagerUnavailable(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
			return nil, poolmgr.ErrUnavailable
		}
	})
	e := newEnv(t, envConfig{
		client:  client,
		backoff: clock.Exponential{Base: time.Second, Max: time.Second},
		tune:    func(s *Settings) { s.Scheduler.AllocationTimeout = 10 * time.Second },
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	errCh := make(chan error, 1)
	go func() {
		_, err := e.sched.Allocate(ctx, r, JobInfo{ID: 6}, e.profile("default"))
		errCh <- err
	}()
	e.advance(ctx, 2, 11*time.Second)

	if err := <-errCh; !errors.Is(err, ErrAllocationTimeout) {
		t.Fatalf("Allocate error = %v, want ErrAllocationTimeout", err)
	}
	if got := e.health.unavailableCount(); got == 0 {
		t.Fatal("the pool manager was not marked unavailable after an UNAVAILABLE claim")
	}
}

func TestAllocateReturnsAnUnexpectedClaimErrorAtOnce(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	boom := errors.New("boom")
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) { return nil, boom }
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	_, err := e.sched.Allocate(ctx, r, JobInfo{ID: 2}, e.profile("default"))
	if !errors.Is(err, boom) {
		t.Fatalf("Allocate error = %v, want the claim error", err)
	}
	if n := client.claimCount(); n != 1 {
		t.Fatalf("claim attempts = %d, want 1: an unexpected error is not retried", n)
	}
}

func TestAllocateBeforeRun(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{})
	r := &Reservation{ID: 1}
	if _, err := e.sched.Allocate(ctx, r, JobInfo{ID: 1}, e.profile("default")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Allocate before Run = %v, want ErrNotRunning", err)
	}
}
