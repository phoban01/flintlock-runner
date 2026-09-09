package fake

import (
	"errors"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL emit every `Event` type defined in the proto
//# at the corresponding phase transition.

// TestEveryEventTypeIsEmittedAtItsTransition walks one Pool through every
// transition the proto has an Event for and checks each event as it arrives:
// the type, the MicroVM it names and the payload the Runner reads. It then
// asserts that every EventType the proto declares was covered, so that a new
// type upstream fails this test rather than going unemitted, and that event
// ids are monotonic per Pool as the proto requires.
func TestEveryEventTypeIsEmittedAtItsTransition(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	host := h.stubs["host-a"]
	spec := h.spec("pool", 1, "host-a")
	var all []*poolmgr.Event
	// collect reads up to the next event of type typ and keeps every event
	// read, so that all is the whole stream in delivery order.
	collect := func(typ poolmgr.EventType) []*poolmgr.Event {
		t.Helper()
		got := h.collectUntil(typ)
		all = append(all, got...)
		return got
	}

	// Declaring the Pool: provisioning starts, the Host creates the MicroVM,
	// the create hooks run and it becomes available.
	if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	got := collect(poolmgrv1.EventType_VM_AVAILABLE)
	replenishing := eventOfType(t, got, poolmgrv1.EventType_POOL_REPLENISHING)
	if replenishing.VMUID != "" {
		t.Fatalf("POOL_REPLENISHING names microvm %q, want a pool-level event", replenishing.VMUID)
	}
	provisioned := eventOfType(t, got, poolmgrv1.EventType_VM_PROVISIONED)
	available := eventOfType(t, got, poolmgrv1.EventType_VM_AVAILABLE)
	if provisioned.VMUID == "" || provisioned.VMUID != available.VMUID {
		t.Fatalf("VM_PROVISIONED %q and VM_AVAILABLE %q, want both for the same microvm", provisioned.VMUID, available.VMUID)
	}
	if provisioned.ID >= available.ID {
		t.Fatalf("VM_PROVISIONED id %d, VM_AVAILABLE id %d: provisioning comes first", provisioned.ID, available.ID)
	}
	if h := payloadOf(t, available)["host"]; h != "host-a" {
		t.Fatalf("VM_AVAILABLE host = %v, want host-a", h)
	}
	if !available.At.Equal(testEpoch) {
		t.Fatalf("VM_AVAILABLE created_at = %s, want the fake clock's %s", available.At, testEpoch)
	}

	// Claiming: the MicroVM is leased, and the strategy replaces it.
	claim, err := h.client.ClaimVM(h.ctx, h.ref("pool"))
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	claimed := eventOfType(t, collect(poolmgrv1.EventType_VM_AVAILABLE), poolmgrv1.EventType_VM_CLAIMED)
	if claimed.VMUID != claim.VMUID {
		t.Fatalf("VM_CLAIMED names %q, want the claimed microvm %q", claimed.VMUID, claim.VMUID)
	}
	if got := payloadOf(t, claimed)["lease_id"]; got != claim.LeaseID {
		t.Fatalf("VM_CLAIMED lease_id = %v, want %q", got, claim.LeaseID)
	}

	// Releasing: the Lease ends and the MicroVM is deleted on release.
	if err := h.client.ReleaseVM(h.ctx, claim.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
	got = collect(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
	released := eventOfType(t, got, poolmgrv1.EventType_VM_RELEASED)
	deleted := eventOfType(t, got, poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
	if released.VMUID != claim.VMUID || deleted.VMUID != claim.VMUID {
		t.Fatalf("VM_RELEASED %q and VM_DELETED_ON_RELEASE %q, want the released microvm %q", released.VMUID, deleted.VMUID, claim.VMUID)
	}
	if released.ID >= deleted.ID {
		t.Fatalf("VM_RELEASED id %d, VM_DELETED_ON_RELEASE id %d: the release comes first", released.ID, deleted.ID)
	}
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Fatalf("Heartbeat after VM_RELEASED = %v, want the lease gone", err)
	}

	// A second claim held past the warning window and then past expiry.
	second, err := h.client.ClaimVM(h.ctx, h.ref("pool"))
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	collect(poolmgrv1.EventType_VM_CLAIMED)
	h.advance(25 * time.Second)
	expiring := eventOfType(t, collect(poolmgrv1.EventType_VM_EXPIRING_SOON), poolmgrv1.EventType_VM_EXPIRING_SOON)
	if expiring.VMUID != second.VMUID {
		t.Fatalf("VM_EXPIRING_SOON names %q, want the leased microvm %q", expiring.VMUID, second.VMUID)
	}
	if got := payloadOf(t, expiring)["lease_id"]; got != second.LeaseID {
		t.Fatalf("VM_EXPIRING_SOON lease_id = %v, want %q", got, second.LeaseID)
	}
	h.advance(10 * time.Second)
	expired := eventOfType(t, collect(poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY), poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY)
	if expired.VMUID != second.VMUID {
		t.Fatalf("VM_DELETED_DUE_TO_EXPIRY names %q, want the expired microvm %q", expired.VMUID, second.VMUID)
	}

	// A create hook that fails on the MicroVM the next claim replaces: it
	// never becomes available, and the Pool is left under its target, which
	// the next tick reports.
	h.pm.SetFaults(poolmgr.Faults{HookFailures: []poolmgr.HookFailure{{Hook: poolmgr.HookCreate, Remaining: 1}}})
	before, _ := host.counts()
	if _, err := h.client.ClaimVM(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	failed := eventOfType(t, collect(poolmgrv1.EventType_VM_HOOK_FAILED), poolmgrv1.EventType_VM_HOOK_FAILED)
	if p := payloadOf(t, failed); p["hook"] != string(poolmgr.HookCreate) || p["policy"] != poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE.String() {
		t.Fatalf("VM_HOOK_FAILED payload = %v, want the create hook and the pool's policy", p)
	}
	if after, _ := host.counts(); after != before+1 {
		t.Fatalf("host created %d microvms, want one more than the %d before the failed hook", after, before)
	}
	// The policy is DELETE_AND_REPLACE, so the MicroVM whose hook failed goes
	// back to the Host. That deletion has no Event of its own.
	h.waitDeletion(host, failed.VMUID)
	assertDeleted(t, host, failed.VMUID)

	// Nothing is available now, so the tick reports the shortfall before it
	// replenishes.
	h.advance(testInterval)
	shortfall := eventOfType(t, collect(poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET), poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET)
	if p := payloadOf(t, shortfall); p["target"] != float64(1) || p["available"] != float64(0) {
		t.Fatalf("POOL_SIZE_BELOW_TARGET payload = %v, want target 1 and nothing available", p)
	}

	// Every type the proto declares was seen at one of those transitions.
	for value, name := range poolmgrv1.EventType_name {
		typ := poolmgrv1.EventType(value)
		if typ == poolmgrv1.EventType_EVENT_TYPE_UNSPECIFIED {
			continue
		}
		if h.seen[typ] == 0 {
			t.Errorf("no %s event was emitted", name)
		}
	}
	assertMonotonic(t, all)
}

// assertMonotonic fails unless event ids increase strictly in the order the
// events were delivered, which is what the proto promises per Pool.
func assertMonotonic(t *testing.T, events []*poolmgr.Event) {
	t.Helper()
	last := make(map[poolmgr.PoolRef]int64)
	for _, e := range events {
		if prev, ok := last[e.Pool]; ok && e.ID <= prev {
			t.Fatalf("event %s of %s has id %d after id %d", e.Type, e.Pool, e.ID, prev)
		}
		last[e.Pool] = e.ID
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL emit every `Event` type defined in the proto
//# at the corresponding phase transition.

// TestSubscribeFilterAndReplay checks the delivery around those events: a
// subscription filtered to one Pool sees only that Pool's events, and a
// subscriber that arrives late is replayed the recent ones, which is how the
// Tracker recovers the state it missed (PL-051).
func TestSubscribeFilterAndReplay(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	h.createPool(h.spec("one", 1, "host-a"))
	h.createPool(h.spec("two", 1, "host-a"))

	ref := h.ref("one")
	stream, err := h.client.Subscribe(h.ctx, poolmgr.EventFilter{Pool: &ref})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// The replay covers everything that happened to the Pool before the
	// subscription, and nothing from the other Pool.
	var types []poolmgr.EventType
	for i := 0; i < 3; i++ {
		e, err := stream.Recv(h.ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if e.Pool != ref {
			t.Fatalf("event from %s on a stream filtered to %s", e.Pool, ref)
		}
		types = append(types, e.Type)
	}
	want := []poolmgr.EventType{
		poolmgrv1.EventType_POOL_REPLENISHING,
		poolmgrv1.EventType_VM_PROVISIONED,
		poolmgrv1.EventType_VM_AVAILABLE,
	}
	for i, w := range want {
		if types[i] != w {
			t.Fatalf("replayed events = %v, want %v", types, want)
		}
	}

	// Live delivery is filtered the same way: the claim on the other Pool is
	// not delivered, the claim on this one is.
	if _, err := h.client.ClaimVM(h.ctx, h.ref("two")); err != nil {
		t.Fatalf("ClaimVM(two): %v", err)
	}
	claim, err := h.client.ClaimVM(h.ctx, ref)
	if err != nil {
		t.Fatalf("ClaimVM(one): %v", err)
	}
	e, err := stream.Recv(h.ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if e.Type != poolmgrv1.EventType_VM_CLAIMED || e.VMUID != claim.VMUID {
		t.Fatalf("next filtered event = %s for %q, want VM_CLAIMED for %q", e.Type, e.VMUID, claim.VMUID)
	}
}
