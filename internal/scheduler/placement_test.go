package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/03-scheduler.md#placement
//= type=test
//# When a claim succeeds and the response carries a `host` with a
//# name, the Scheduler SHALL record that name as the Placement, and if the
//# `host.address` differs from the Inventory entry of that name, then the
//# Scheduler SHALL log a warning with both values, count the mismatch and
//# continue using the Inventory endpoint.

func TestPlacementFromTheClaimedHostName(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	hosts := []config.HostEntry{testHostEntry("host-1"), testHostEntry("host-2")}
	e := newEnv(t, envConfig{inventory: hosts})
	e.startBare(ctx)

	claim := &poolmgr.Claim{
		LeaseID: "lease-1",
		VMUID:   "vm-1",
		Host:    poolmgr.HostRef{Name: "host-2", Address: "host-2:9090"},
	}
	got, err := e.sched.ResolvePlacement(ctx, e.profile("default"), claim)
	if err != nil {
		t.Fatalf("ResolvePlacement: %v", err)
	}
	if got.Host != "host-2" {
		t.Fatalf("placement host = %q, want host-2", got.Host)
	}
	if got.Source != PlacementFromClaim {
		t.Fatalf("placement source = %q, want %q", got.Source, PlacementFromClaim)
	}
	if got.ResolvedAt != testEpoch {
		t.Fatalf("resolved at = %v, want %v", got.ResolvedAt, testEpoch)
	}
	// The claim named the Host, so no Host was asked for the MicroVM.
	for _, name := range []string{"host-1", "host-2"} {
		client, err := e.registry.Get(name)
		if err != nil {
			t.Fatalf("registry.Get(%q): %v", name, err)
		}
		if get, _, _ := client.(*countingHost).counts(); get != 0 {
			t.Fatalf("%s was asked for the microvm %d times, want 0", name, get)
		}
	}
	if got := e.metrics.failure(FailureAddressMismatch); got != 0 {
		t.Fatalf("address mismatches counted = %d, want 0", got)
	}
}

func TestPlacementWarnsOnAddressMismatchAndKeepsTheInventoryEndpoint(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	entry := testHostEntry("host-1")
	entry.Endpoint = "10.0.0.7:9090"
	e := newEnv(t, envConfig{inventory: []config.HostEntry{entry}})
	e.startBare(ctx)

	claim := &poolmgr.Claim{
		LeaseID: "lease-1",
		VMUID:   "vm-1",
		Host:    poolmgr.HostRef{Name: "host-1", Address: "172.16.4.7:9090"},
	}
	got, err := e.sched.ResolvePlacement(ctx, e.profile("default"), claim)
	if err != nil {
		t.Fatalf("ResolvePlacement: %v", err)
	}
	if got.Host != "host-1" || got.Source != PlacementFromClaim {
		t.Fatalf("placement = %+v, want host-1 from the claim", got)
	}
	if n := e.metrics.failure(FailureAddressMismatch); n != 1 {
		t.Fatalf("address mismatches counted = %d, want 1", n)
	}

	warnings := e.logs.find("address differs")
	if len(warnings) != 1 {
		t.Fatalf("mismatch warnings = %d, want 1", len(warnings))
	}
	attrs := warnings[0].attrs
	if attrs["claimed_address"] != "172.16.4.7:9090" || attrs["inventory_endpoint"] != "10.0.0.7:9090" {
		t.Fatalf("warning attributes = %v, want both addresses", attrs)
	}

	// The Runner keeps reaching the Host at the Inventory endpoint.
	ep, ok := e.registry.Endpoint("host-1")
	if !ok || ep.Address != "10.0.0.7:9090" {
		t.Fatalf("endpoint used = %+v, want the inventory endpoint", ep)
	}
}

//= docs/requirements/03-scheduler.md#placement
//= type=test
//# When a claim succeeds and the response does not name the Host,
//# the Scheduler SHALL resolve the Placement by calling `GetMicroVM` with the
//# MicroVM's uid on each Host the Pool Manager reports for that Pool until
//# one returns it.

func TestPlacementFallsBackToAskingEachPoolHost(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	// A Pool Manager that never names the Host on a claim, as one older than
	// battery PR #45 does, over two fake Hosts.
	first := newFakeHost(t, "host-1")
	second := newFakeHost(t, "host-2")
	source := map[string]flintlock.PoolHostClient{
		"host-1": first.Client(),
		"host-2": second.Client(),
	}
	pm, client := newFakePoolManagerWith(t, ctx, source, func(cfg *poolmgr.FakeConfig) {
		cfg.OmitHostOnClaim = true
	})
	_ = pm

	e := newEnv(t, envConfig{
		client:    client,
		inventory: []config.HostEntry{testHostEntry("host-1"), testHostEntry("host-2")},
		hosts: map[string]flintlock.HostClient{
			"host-1": runnerClient(t, first),
			"host-2": runnerClient(t, second),
		},
	})
	e.declarer.admin = client
	e.startBare(ctx)
	e.sched.declareAll(ctx)

	pool := e.poolOf("default")
	waitForClaimable(t, ctx, client, pool)
	e.tracker.setAvailable(pool, 1)

	h := e.allocate(ctx, JobInfo{ID: 42}, "default")
	defer e.sched.Release(h)

	alloc := h.Allocation()
	if alloc.Host.Name != "" {
		t.Fatalf("the claim named host %q; the fallback path was not exercised", alloc.Host.Name)
	}
	if alloc.Placement.Source != PlacementFromLookup {
		t.Fatalf("placement source = %q, want %q", alloc.Placement.Source, PlacementFromLookup)
	}
	if alloc.Placement.Host != "host-1" && alloc.Placement.Host != "host-2" {
		t.Fatalf("placement host = %q, want one of the pool's hosts", alloc.Placement.Host)
	}

	// The Host the fan-out named is the one that really has the MicroVM.
	owner, err := e.registry.Get(alloc.Placement.Host)
	if err != nil {
		t.Fatalf("registry.Get(%q): %v", alloc.Placement.Host, err)
	}
	if _, err := owner.GetMicroVM(ctx, alloc.VMUID); err != nil {
		t.Fatalf("the placed host does not have microvm %s: %v", alloc.VMUID, err)
	}
}

func TestPlacementLookupSkipsHostsThatDoNotHaveTheMicroVM(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	// Three Hosts; only the last has the MicroVM, so the fan-out asks each in
	// turn until one answers.
	first, second, third := newCountingHost("host-1"), newCountingHost("host-2"), newCountingHost("host-3")
	third.holds("vm-9")
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.getPoolFn = func(_ context.Context, ref poolmgr.PoolRef) (*poolmgr.Pool, error) {
			return &poolmgr.Pool{Spec: poolmgr.PoolSpec{
				Ref:            ref,
				FlintlockHosts: []string{"host-1", "host-2", "host-3"},
			}}, nil
		}
	})
	e := newEnv(t, envConfig{
		client: client,
		inventory: []config.HostEntry{
			testHostEntry("host-1"), testHostEntry("host-2"), testHostEntry("host-3"),
		},
		hosts: map[string]flintlock.HostClient{
			"host-1": first, "host-2": second, "host-3": third,
		},
	})
	e.startBare(ctx)

	got, err := e.sched.ResolvePlacement(ctx, e.profile("default"), &poolmgr.Claim{LeaseID: "l", VMUID: "vm-9"})
	if err != nil {
		t.Fatalf("ResolvePlacement: %v", err)
	}
	if got.Host != "host-3" || got.Source != PlacementFromLookup {
		t.Fatalf("placement = %+v, want host-3 from the lookup", got)
	}
	for _, h := range []*countingHost{first, second, third} {
		if get, _, _ := h.counts(); get != 1 {
			t.Fatalf("%s asked %d times, want 1", h.Name(), get)
		}
	}
}

//= docs/requirements/03-scheduler.md#placement
//= type=test
//# The Scheduler SHALL cache the Placement of a MicroVM for the
//# lifetime of its Lease.

func TestPlacementIsCachedForTheLifetimeOfTheLease(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	host := newCountingHost("host-1")
	host.holds("vm-1")
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
			return &poolmgr.Claim{LeaseID: "lease-1", VMUID: "vm-1"}, nil
		}
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
		c.getPoolFn = func(_ context.Context, ref poolmgr.PoolRef) (*poolmgr.Pool, error) {
			return &poolmgr.Pool{Spec: poolmgr.PoolSpec{Ref: ref, FlintlockHosts: []string{"host-1"}}}, nil
		}
	})
	e := newEnv(t, envConfig{client: client, hosts: map[string]flintlock.HostClient{"host-1": host}})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 2)

	h := e.allocate(ctx, JobInfo{ID: 1}, "default")
	get, _, _ := host.counts()
	if get != 1 {
		t.Fatalf("GetMicroVM calls for the first resolution = %d, want 1", get)
	}

	// While the Lease is held the Placement is answered from the cache.
	again, err := e.sched.ResolvePlacement(ctx, e.profile("default"), &poolmgr.Claim{LeaseID: "lease-1", VMUID: "vm-1"})
	if err != nil {
		t.Fatalf("second ResolvePlacement: %v", err)
	}
	if again != h.Allocation().Placement {
		t.Fatalf("cached placement = %+v, want %+v", again, h.Allocation().Placement)
	}
	if get, _, _ := host.counts(); get != 1 {
		t.Fatalf("GetMicroVM calls while the lease is held = %d, want 1", get)
	}

	// Once the Lease is handed back the cache entry is gone.
	e.sched.Release(h)
	if _, err := e.sched.ResolvePlacement(ctx, e.profile("default"), &poolmgr.Claim{LeaseID: "lease-1", VMUID: "vm-1"}); err != nil {
		t.Fatalf("ResolvePlacement after release: %v", err)
	}
	if get, _, _ := host.counts(); get != 2 {
		t.Fatalf("GetMicroVM calls after the lease was released = %d, want 2", get)
	}
}

//= docs/requirements/03-scheduler.md#placement
//= type=test
//# If Placement cannot be resolved for a MicroVM, then the Scheduler
//# SHALL release its Lease and report an allocation error.

func TestUnresolvedPlacementReleasesTheLease(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	// No Host has the MicroVM, and the claim did not name one.
	host := newCountingHost("host-1")
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
			return &poolmgr.Claim{LeaseID: "lease-7", VMUID: "vm-7"}, nil
		}
		c.getPoolFn = func(_ context.Context, ref poolmgr.PoolRef) (*poolmgr.Pool, error) {
			return &poolmgr.Pool{Spec: poolmgr.PoolSpec{Ref: ref, FlintlockHosts: []string{"host-1"}}}, nil
		}
	})
	e := newEnv(t, envConfig{client: client, hosts: map[string]flintlock.HostClient{"host-1": host}})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	h, err := e.sched.Allocate(ctx, r, JobInfo{ID: 5}, e.profile("default"))
	if h != nil {
		t.Fatalf("Allocate returned a handle %+v, want an error", h.Allocation())
	}
	if !errors.Is(err, ErrPlacementUnresolved) {
		t.Fatalf("Allocate error = %v, want ErrPlacementUnresolved", err)
	}
	var aerr *AllocationError
	if !errors.As(err, &aerr) || aerr.Pool != e.poolOf("default") || aerr.Profile != "default" {
		t.Fatalf("error = %v, want an AllocationError naming the profile and pool", err)
	}

	_ = waitForRelease(t, ctx, client)
	if got := client.releasedLeases(); got[0] != "lease-7" {
		t.Fatalf("released leases = %v, want lease-7", got)
	}
	// The Slot is the caller's Reservation, still held for it to release.
	if got := e.sched.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("slots in use = %d, want the caller's reservation still held", got)
	}
}

//= docs/requirements/03-scheduler.md#placement
//= type=test
//# If the Host named in a Placement is not in the Inventory, then
//# the Scheduler SHALL release the Lease and report an allocation error
//# naming the Host.

func TestPlacementOnAHostOutsideTheInventoryReleasesTheLease(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
			return &poolmgr.Claim{
				LeaseID: "lease-3",
				VMUID:   "vm-3",
				Host:    poolmgr.HostRef{Name: "host-99", Address: "host-99:9090"},
			}, nil
		}
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	_, err := e.sched.Allocate(ctx, r, JobInfo{ID: 11}, e.profile("default"))
	if !errors.Is(err, ErrHostNotInInventory) {
		t.Fatalf("Allocate error = %v, want ErrHostNotInInventory", err)
	}
	var aerr *AllocationError
	if !errors.As(err, &aerr) || aerr.Host != "host-99" {
		t.Fatalf("error = %v, want an AllocationError naming host-99", err)
	}

	_ = waitForRelease(t, ctx, client)
	if got := client.releasedLeases(); got[0] != "lease-3" {
		t.Fatalf("released leases = %v, want lease-3", got)
	}
}

// waitForRelease blocks until the client has made a release call and returns
// the lease id it named.
func waitForRelease(t *testing.T, ctx context.Context, client *stubClient) string {
	t.Helper()
	select {
	case id := <-client.releasedCh:
		return id
	case <-ctx.Done():
		t.Fatal("waiting for a release call")
		return ""
	}
}
