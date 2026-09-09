package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# The Scheduler SHALL maintain a count of held Reservations and
//# active Allocations and SHALL treat their sum as the number of Slots in use.

func TestSlotsInUseIsReservationsPlusAllocations(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = func(context.Context, string) (time.Time, error) { return testEpoch.Add(time.Hour), nil }
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 4)

	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use before any reservation = %d, want 0", got)
	}

	first := e.reserve(ctx)
	second := e.reserve(ctx)
	if got := e.sched.Snapshot().SlotsInUse; got != 2 {
		t.Fatalf("slots in use with two reservations = %d, want 2", got)
	}

	h, err := e.sched.Allocate(ctx, first, JobInfo{ID: 1}, e.profile("default"))
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got := e.sched.Snapshot().SlotsInUse; got != 2 {
		t.Fatalf("slots in use with one reservation and one allocation = %d, want 2", got)
	}

	e.sched.ReleaseReservation(second)
	if got := e.sched.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("slots in use after releasing the reservation = %d, want 1", got)
	}
	e.sched.Release(h)
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use after releasing the allocation = %d, want 0", got)
	}
}

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# The Scheduler SHALL compute available capacity as the lesser of
//# the number of unused Slots and the sum of the number of available warm
//# MicroVMs across all Pools.

func TestAvailableCapacityIsTheLesserOfSlotsAndWarmMicroVMs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		slots      int
		inUse      int
		warm       int
		want       int
		wantReason RefusalReason
	}{
		{name: "warm is the limit", slots: 8, inUse: 0, warm: 3, want: 3},
		{name: "slots are the limit", slots: 4, inUse: 1, warm: 9, want: 3},
		{name: "equal", slots: 5, inUse: 0, warm: 5, want: 5},
		{name: "no slot left", slots: 2, inUse: 2, warm: 9, want: 0, wantReason: RefusalNoFreeSlot},
		{name: "no warm microvm", slots: 4, inUse: 0, warm: 0, want: 0, wantReason: RefusalNoWarmMicroVM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := testContext(t)
			e := newEnv(t, envConfig{tune: func(s *Settings) { s.Slots = tt.slots }})
			e.startBare(ctx)
			e.tracker.setAvailable(e.poolOf("default"), int32(tt.warm))
			for i := 0; i < tt.inUse; i++ {
				e.reserve(ctx)
			}

			warm := e.sched.warmAvailable()
			e.sched.mu.Lock()
			got, reason := e.sched.availableCapacityLocked(warm)
			e.sched.mu.Unlock()
			if got != tt.want || reason != tt.wantReason {
				t.Fatalf("availableCapacity = (%d, %q), want (%d, %q)", got, reason, tt.want, tt.wantReason)
			}
		})
	}
}

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# The Scheduler SHALL count a Pool's MicroVMs that are being
//# provisioned or running create hooks as unavailable when computing
//# capacity.

func TestProvisioningMicroVMsDoNotCountAsCapacity(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{})
	e.startBare(ctx)
	pool := e.poolOf("default")

	// Every MicroVM of the Pool is booting or running its create hooks, and
	// one is quarantined; none of them can be claimed.
	e.tracker.setStatus(pool, poolmgr.PoolStatus{Available: 0, Provisioning: 4, Quarantined: 1, Leased: 2})

	if _, err := e.sched.Reserve(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("Reserve with only provisioning microvms = %v, want a refusal", err)
	}
	var refusal *Refusal
	_, err := e.sched.Reserve(ctx)
	if !errors.As(err, &refusal) || refusal.Reason != RefusalNoWarmMicroVM {
		t.Fatalf("refusal reason = %v, want %q", err, RefusalNoWarmMicroVM)
	}

	// The same Pool with one MicroVM finished provisioning grants at once.
	e.tracker.setStatus(pool, poolmgr.PoolStatus{Available: 1, Provisioning: 3, Quarantined: 1, Leased: 2})
	if _, err := e.sched.Reserve(ctx); err != nil {
		t.Fatalf("Reserve with one available microvm: %v", err)
	}
}

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# When a Reservation is requested and available capacity is
//# greater than zero, the Scheduler SHALL grant a Reservation and count it as
//# a Slot in use.

func TestReserveGrantsAndCountsTheSlot(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{tune: func(s *Settings) { s.Slots = 2 }})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 5)

	r, err := e.sched.Reserve(ctx)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if r.ID == 0 || r.GrantedAt != testEpoch {
		t.Fatalf("Reservation = %+v, want an id and the current time", r)
	}
	if got := e.sched.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("slots in use = %d, want 1", got)
	}
	second, err := e.sched.Reserve(ctx)
	if err != nil {
		t.Fatalf("second Reserve: %v", err)
	}
	if second.ID == r.ID {
		t.Fatal("two reservations share an id")
	}
}

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# When a Reservation is requested and available capacity is zero,
//# the Scheduler SHALL refuse the Reservation.

func TestReserveRefusesWhenCapacityIsZero(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		slots      int
		warm       int32
		wantReason RefusalReason
	}{
		{name: "every slot in use", slots: 1, warm: 5, wantReason: RefusalNoFreeSlot},
		{name: "no warm microvm", slots: 4, warm: 0, wantReason: RefusalNoWarmMicroVM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := testContext(t)
			e := newEnv(t, envConfig{tune: func(s *Settings) { s.Slots = tt.slots }})
			e.startBare(ctx)
			e.tracker.setAvailable(e.poolOf("default"), tt.warm)
			if tt.wantReason == RefusalNoFreeSlot {
				e.reserve(ctx)
			}

			r, err := e.sched.Reserve(ctx)
			if r != nil {
				t.Fatalf("Reserve granted %+v, want a refusal", r)
			}
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("Reserve error = %v, want ErrRefused", err)
			}
			var refusal *Refusal
			if !errors.As(err, &refusal) || refusal.Reason != tt.wantReason {
				t.Fatalf("refusal = %v, want reason %q", err, tt.wantReason)
			}
			if got := e.metrics.refusal(tt.wantReason); got != 1 {
				t.Fatalf("refusal metric for %q = %d, want 1", tt.wantReason, got)
			}
		})
	}
}

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# When a Reservation is released without an Allocation, the
//# Scheduler SHALL return its Slot immediately.

func TestReleaseReservationReturnsTheSlotImmediately(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{tune: func(s *Settings) { s.Slots = 1 }})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 3)

	r := e.reserve(ctx)
	if _, err := e.sched.Reserve(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("second Reserve = %v, want a refusal while the only slot is held", err)
	}

	e.sched.ReleaseReservation(r)
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use after release = %d, want 0", got)
	}
	if _, err := e.sched.Reserve(ctx); err != nil {
		t.Fatalf("Reserve after the slot was returned: %v", err)
	}

	// Releasing twice is a no-op rather than a second free Slot.
	e.sched.ReleaseReservation(r)
	if got := e.sched.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("slots in use after a double release = %d, want 1", got)
	}
}

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# Where a Profile has a configured maximum concurrency, the
//# Scheduler SHALL NOT hold more Allocations for that Profile than the
//# maximum.

func TestProfileMaximumConcurrencyIsNotExceeded(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	profile := testProfile("default", 4)
	profile.MaxConcurrency = 1
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client, profiles: []config.Profile{profile}})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 4)

	first := e.allocate(ctx, JobInfo{ID: 1}, "default")

	// The second allocation cannot start while the first holds the only unit.
	second := e.reserve(ctx)
	blocked := make(chan struct {
		h   Handle
		err error
	}, 1)
	go func() {
		h, err := e.sched.Allocate(ctx, second, JobInfo{ID: 2}, e.profile("default"))
		blocked <- struct {
			h   Handle
			err error
		}{h, err}
	}()

	// Give the blocked allocation the chance to claim: it must not, so no
	// second claim reaches the Pool Manager while the limit is reached.
	if err := e.clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the allocation timeout timer: %v", err)
	}
	if got := client.claimCount(); got != 1 {
		t.Fatalf("claims while the profile limit is reached = %d, want 1", got)
	}
	select {
	case got := <-blocked:
		t.Fatalf("the second allocation completed with %v, %v while the limit was reached", got.h, got.err)
	default:
	}

	// Releasing the first Allocation frees the unit and the second proceeds.
	e.sched.Release(first)
	got := <-blocked
	if got.err != nil {
		t.Fatalf("the second allocation after the limit was freed: %v", got.err)
	}
	if got := client.claimCount(); got != 2 {
		t.Fatalf("claims after the limit was freed = %d, want 2", got)
	}
	e.sched.Release(got.h)
}

func TestProfileLimitTimesOut(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	profile := testProfile("default", 4)
	profile.MaxConcurrency = 1
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client, profiles: []config.Profile{profile}})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 4)

	held := e.allocate(ctx, JobInfo{ID: 1}, "default")
	defer e.sched.Release(held)

	r := e.reserve(ctx)
	errCh := make(chan error, 1)
	go func() {
		_, err := e.sched.Allocate(ctx, r, JobInfo{ID: 2}, e.profile("default"))
		errCh <- err
	}()
	e.advance(ctx, 1, 2*time.Minute)

	err := <-errCh
	if !errors.Is(err, ErrProfileLimit) {
		t.Fatalf("Allocate error = %v, want ErrProfileLimit", err)
	}
}

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# The Scheduler SHALL be safe for concurrent use by the number of
//# workers the run loop starts.

func TestConcurrentReserveAllocateRelease(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = func(context.Context, string) (time.Time, error) { return testEpoch.Add(time.Hour), nil }
	})
	e := newEnv(t, envConfig{client: client, tune: func(s *Settings) { s.Slots = 8 }})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 100)

	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for round := 0; round < 8; round++ {
				r, err := e.sched.Reserve(ctx)
				if err != nil {
					if !errors.Is(err, ErrRefused) {
						t.Errorf("Reserve: %v", err)
					}
					continue
				}
				if round%2 == 0 {
					e.sched.ReleaseReservation(r)
					continue
				}
				h, err := e.sched.Allocate(ctx, r, JobInfo{ID: int64(id)}, e.profile("default"))
				if err != nil {
					t.Errorf("Allocate: %v", err)
					e.sched.ReleaseReservation(r)
					continue
				}
				_ = e.sched.Snapshot()
				e.sched.Release(h)
			}
		}(i)
	}
	wg.Wait()

	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use after every worker finished = %d, want 0", got)
	}
}

//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# When starting, the Scheduler SHALL NOT grant any Reservation until
//# it has successfully contacted the Pool Manager at least once.

func TestReserveRefusesUntilThePoolManagerHasAnswered(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{})
	// Plenty of capacity, but the Pool Manager has not answered yet.
	if err := e.sched.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		e.sched.closeBackground()
		e.sched.bgCancel()
		e.sched.wg.Wait()
	})
	e.tracker.setAvailable(e.poolOf("default"), 10)

	r, err := e.sched.Reserve(ctx)
	if r != nil {
		t.Fatalf("Reserve granted %+v before the pool manager answered", r)
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Reason != RefusalPoolManagerNotContacted {
		t.Fatalf("refusal = %v, want reason %q", err, RefusalPoolManagerNotContacted)
	}
	select {
	case <-e.sched.Ready():
		t.Fatal("Ready is closed before the pool manager answered")
	default:
	}

	e.health.contact()
	if _, err := e.sched.Reserve(ctx); err != nil {
		t.Fatalf("Reserve after the pool manager answered: %v", err)
	}
	select {
	case <-e.sched.Ready():
	default:
		t.Fatal("Ready is not closed after the pool manager answered")
	}
}

func TestReserveBeforeRunAndWhileShuttingDown(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{})
	e.tracker.setAvailable(e.poolOf("default"), 5)

	if _, err := e.sched.Reserve(ctx); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Reserve before Run = %v, want ErrNotRunning", err)
	}

	e.startBare(ctx)
	e.sched.setState(stateStopping)
	var refusal *Refusal
	_, err := e.sched.Reserve(ctx)
	if !errors.As(err, &refusal) || refusal.Reason != RefusalShuttingDown {
		t.Fatalf("Reserve while shutting down = %v, want a shutting-down refusal", err)
	}
}
