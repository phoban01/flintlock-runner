package poolmgr_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// claimer builds a Claimer over a client with the given policy units.
func claimer(t *testing.T, c poolmgr.Client, tracker poolmgr.Tracker, health poolmgr.Health, redeclarer poolmgr.Redeclarer, log *logRecorder) *poolmgr.Claimer {
	t.Helper()
	logger := testLogger(t)
	if log != nil {
		logger = log.logger()
	}
	cl, err := poolmgr.NewClaimer(poolmgr.ClaimerConfig{
		Lease:      c,
		Tracker:    tracker,
		Health:     health,
		Redeclarer: redeclarer,
		Log:        logger,
	})
	if err != nil {
		t.Fatalf("NewClaimer: %v", err)
	}
	return cl
}

//= docs/requirements/04-pool-manager.md#claiming
//= type=test
//# When a warm MicroVM is needed for a Profile, the Scheduler SHALL call
//# `ClaimVM` with the `PoolRef` of that Profile's Pool.

// TestClaimTakesFromThePoolOfThatProfile declares two Profiles and claims
// for each of them, then checks against the fake Pool Manager's own lease
// records that each claim came out of that Profile's Pool.
func TestClaimTakesFromThePoolOfThatProfile(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client(nil)
	small := specFor(t, testProfile("small", 1), "host-a")
	large := specFor(t, testProfile("large", 1), "host-a")
	pm.fillPool(c, small)
	pm.fillPool(c, large)

	cl := claimer(t, c, nil, nil, nil, nil)
	for _, spec := range []poolmgr.PoolSpec{small, large} {
		claim, err := cl.Claim(ctx, spec.Ref)
		if err != nil {
			t.Fatalf("Claim from %s: %v", spec.Ref, err)
		}
		var found bool
		for _, lease := range pm.leases() {
			if lease.LeaseID != claim.LeaseID {
				continue
			}
			found = true
			if lease.Pool != spec.Ref {
				t.Errorf("lease %s is on pool %s, want %s", lease.LeaseID, lease.Pool, spec.Ref)
			}
		}
		if !found {
			t.Errorf("the pool manager has no lease %s", claim.LeaseID)
		}
	}
}

//= docs/requirements/04-pool-manager.md#claiming
//= type=test
//# If `ClaimVM` returns `RESOURCE_EXHAUSTED`, then the Scheduler SHALL
//# treat the Pool as having no warm MicroVM available.

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# When a `ClaimVM` call returns `RESOURCE_EXHAUSTED`, the Scheduler SHALL
//# set that Pool's available count to zero until the next event or poll
//# says otherwise.

// TestExhaustedPoolCountsAsEmpty claims one MicroVM more than the Pool
// holds. The refusal comes back as ErrExhausted and the Pool's available
// count reads zero from then on, even though the Tracker had just been told
// by an event that a MicroVM was available -- and it goes back up when the
// replacement the Pool Manager provisions arrives.
func TestExhaustedPoolCountsAsEmpty(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client(nil)
	spec := specFor(t, testProfile("small", 1), "host-a")
	pm.fillPool(c, spec)

	tracker := newTracker(t, c, nil, time.Minute, nil)
	tracker.run(t)
	tracker.Track(spec.Ref, true)
	tracker.await(t, ctx, "the warm microvm to be counted", func() bool {
		return tracker.Available(spec.Ref) > 0
	})

	// Keep the Host from producing the replacement, so that the Pool is
	// certainly empty for the second claim rather than racing the boot.
	pm.host("host-a").SetFaults(flintlock.HostFaults{CreateFails: true})

	cl := claimer(t, c, tracker, nil, nil, nil)
	if _, err := cl.Claim(ctx, spec.Ref); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if _, err := cl.Claim(ctx, spec.Ref); !errors.Is(err, poolmgr.ErrExhausted) {
		t.Fatalf("second Claim = %v, want ErrExhausted", err)
	}
	if got := tracker.Available(spec.Ref); got != 0 {
		t.Errorf("available = %d after an exhausted claim, want 0", got)
	}

	// With the Host healthy again, replenishment produces a MicroVM and the
	// event that reports it lifts the Pool's count off zero.
	pm.host("host-a").SetFaults(flintlock.HostFaults{})
	tracker.await(t, ctx, "the replacement microvm to be counted", func() bool {
		return tracker.Available(spec.Ref) > 0
	})
}

//= docs/requirements/04-pool-manager.md#claiming
//= type=test
//# If `ClaimVM` returns `NOT_FOUND`, then the Scheduler SHALL re-declare
//# the Pool from its Profile and treat the Pool as having no warm MicroVM
//# available.

// TestClaimOnAnUnknownPoolRedeclaresIt deletes a declared Pool behind the
// Runner's back, claims, and checks both halves: the Pool is declared again
// from the Profile the Runner still holds, and the claim is answered as an
// exhausted Pool rather than as a hard failure.
func TestClaimOnAnUnknownPoolRedeclaresIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "arm-fast", "arm-slow")
	c := pm.client(nil)
	d := declaration(t, c, clock.NewFake(testEpoch), nil)
	profile := testProfile("small", 1)
	if err := d.Sync(ctx, []config.Profile{profile}, testInventory()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := c.DeletePool(ctx, ref("small")); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}

	tracker := newRecordingTracker()
	cl := claimer(t, c, tracker, nil, d, nil)
	_, err := cl.Claim(ctx, ref("small"))
	if !errors.Is(err, poolmgr.ErrExhausted) {
		t.Errorf("Claim on a deleted pool = %v, want it treated as an exhausted pool", err)
	}
	if !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("Claim error = %v, want it to still say the pool was not found", err)
	}
	if !slices.Contains(tracker.exhaustedPools(), ref("small")) {
		t.Errorf("pools marked exhausted = %v, want the deleted pool", tracker.exhaustedPools())
	}
	pool, err := c.GetPool(ctx, ref("small"))
	if err != nil {
		t.Fatalf("the pool was not re-declared: %v", err)
	}
	if pool.Spec.Size != int32(profile.Pool.Size) {
		t.Errorf("re-declared pool size = %d, want the profile's %d", pool.Spec.Size, profile.Pool.Size)
	}
}

//= docs/requirements/04-pool-manager.md#claiming
//= type=test
//# If `ClaimVM` fails with `UNAVAILABLE` or a connection error, then the
//# Scheduler SHALL mark the Pool Manager unhealthy for the configured
//# backoff period.

// TestUnavailableClaimMarksThePoolManagerUnhealthy takes the Pool Manager
// away for the duration of a claim and checks that the health unit is
// marked unhealthy at once, stays unhealthy for the configured backoff
// period measured on its own clock, and recovers when the period is over.
func TestUnavailableClaimMarksThePoolManagerUnhealthy(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	const backoff = 45 * time.Second
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client(nil)
	clk := clock.NewFake(testEpoch)
	health := newHealth(t, c, clk, func(cfg *poolmgr.HealthConfig) {
		cfg.UnhealthyFor = backoff
	})
	runInBackground(t, "Health.Run", health.Run)
	awaitContact(t, ctx, health)

	pm.setFaults(poolmgr.Faults{UnavailableFor: time.Hour})
	cl := claimer(t, c, nil, health, nil, nil)
	if _, err := cl.Claim(ctx, ref("small")); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Fatalf("Claim while the pool manager is away = %v, want ErrUnavailable", err)
	}
	if health.Healthy() {
		t.Error("the pool manager is still healthy after a claim could not reach it")
	}

	// Not yet: the backoff period has not elapsed.
	clk.Advance(backoff - time.Second)
	if health.Healthy() {
		t.Errorf("the pool manager is healthy again %s into a %s backoff", backoff-time.Second, backoff)
	}

	// With the fault cleared and the period over, health comes back.
	pm.setFaults(poolmgr.Faults{})
	clk.Advance(2 * time.Second)
	if !health.Healthy() {
		t.Error("the pool manager is still unhealthy after the backoff period")
	}
}

// TestClaimWithoutPolicyUnitsStillWorks checks that a Claimer built with
// only a Lease, which is what a Scheduler test does, does not panic on any
// of the branches.
func TestClaimWithoutPolicyUnitsStillWorks(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client(nil)
	logs := newLogRecorder(t)
	cl := claimer(t, c, nil, nil, nil, logs)

	if _, err := cl.Claim(ctx, ref("no-such-pool")); !errors.Is(err, poolmgr.ErrExhausted) {
		t.Errorf("Claim on an unknown pool = %v, want ErrExhausted", err)
	}
	if !logs.contains("nothing can re-declare it") {
		t.Errorf("the missing re-declarer was not logged; log was:\n%s", logs.text())
	}
	if _, err := poolmgr.NewClaimer(poolmgr.ClaimerConfig{}); err == nil {
		t.Error("NewClaimer accepted a configuration with no Lease")
	}
}
