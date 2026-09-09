package fake

import (
	"errors"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// Tests for the deletion side of the control loop: one deletion of a MicroVM
// is in flight at a time, however slow or unwilling its Host is. A second
// attempt started while the first is still on the Host would both pile
// goroutines and outstanding Host calls up at the reconcile interval, and
// race the first attempt on the MicroVM's state.

// TestDeletionRetryWaitsForTheOneInFlight holds the Host inside
// DeleteMicroVM, which is what a slow or hung flintlockd looks like, and
// checks that the reconcile passes that follow start no second deletion of
// the same MicroVM. Without the in-flight guard every tick spawns another
// retry, so the Host sees one call per interval and the fake leaks a
// goroutine per interval with it.
func TestDeletionRetryWaitsForTheOneInFlight(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	host := h.stubs["host-a"]
	spec := h.spec("pool", 1, "host-a")
	spec.Replenishment = poolmgr.ReplenishmentStrategy{
		Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
		MinSize: minSize(1),
	}
	h.createPool(spec)
	claim := h.claim("pool")

	gate := make(chan struct{})
	host.set(func(s *stubHost) { s.deleteGate = gate })

	released := make(chan error, 1)
	go func() { released <- h.client.ReleaseVM(h.ctx, claim.LeaseID) }()
	// The release's deletion is now on the Host and will not come back
	// until the gate opens.
	h.waitDeleteCall(host)

	// Several reconcile intervals pass with the Host still holding it.
	for i := 0; i < 5; i++ {
		h.tick()
	}
	h.waitTimers(1)

	host.set(func(s *stubHost) { s.deleteGate = nil })
	close(gate)
	if err := <-released; err != nil {
		t.Fatalf("ReleaseVM once the host answered: %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
	assertDeleted(t, host, claim.VMUID)

	// Run's return waits for every goroutine the ticks spawned, so any
	// extra deletion has reached the Host by now.
	h.stop()
	if n := host.deleteAttempts(claim.VMUID); n != 1 {
		t.Fatalf("the host saw %d delete calls for %s, want the single one the release started: a tick must not retry a deletion that is still in flight", n, claim.VMUID)
	}
}

// TestDeletionRetriesDoNotRaceOnVMState drives reconcile passes back to back
// against a Host that refuses the deletion, with releases of the same Lease
// running alongside, and is meant to be run under -race: it is the
// arrangement in which one retry writes the MicroVM's pending delete event
// while another reads it. The guard that keeps a second attempt from
// starting is what removes the race.
func TestDeletionRetriesDoNotRaceOnVMState(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	host := h.stubs["host-a"]
	spec := h.spec("pool", 1, "host-a")
	spec.Replenishment = poolmgr.ReplenishmentStrategy{
		Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
		MinSize: minSize(1),
	}
	h.createPool(spec)
	claim := h.claim("pool")

	host.set(func(s *stubHost) { s.deleteErr = errors.New("host busy") })
	if err := h.client.ReleaseVM(h.ctx, claim.LeaseID); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Fatalf("ReleaseVM while the host refuses = %v, want ErrUnavailable", err)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				h.pm.kickReconcile()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			// The MicroVM is still there, so this keeps deferring too.
			_ = h.client.ReleaseVM(h.ctx, claim.LeaseID)
		}
	}()
	wg.Wait()

	// Let the Host take it, so the fake shuts down with nothing left.
	host.set(func(s *stubHost) { s.deleteErr = nil })
	h.advance(testInterval)
	h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
	assertDeleted(t, host, claim.VMUID)
}

// TestExpiryWarningIsForgottenWithItsLease releases a Lease the control loop
// has already warned about and checks that the warning goes with it. The
// record is per lease id, and lease ids are never reused, so one left behind
// for a Lease that ended by release rather than by expiry grows the map for
// as long as the Pool exists, which in the standalone binary is the life of
// the process.
func TestExpiryWarningIsForgottenWithItsLease(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	h.createPool(h.spec("pool", 1, "host-a"))
	claim := h.claim("pool")

	// Within one heartbeat interval of the expiry, the loop warns.
	h.advance(21 * time.Second)
	warning := h.waitEvent(poolmgrv1.EventType_VM_EXPIRING_SOON)
	if warning.VMUID != claim.VMUID {
		t.Fatalf("VM_EXPIRING_SOON names %q, want the leased microvm %q", warning.VMUID, claim.VMUID)
	}
	if n := h.warned("pool"); n != 1 {
		t.Fatalf("%d warned leases after the warning, want 1", n)
	}

	h.release(claim.LeaseID)
	if leases := h.pm.Leases(); len(leases) != 0 {
		t.Fatalf("leases after the release = %+v, want none", leases)
	}
	if n := h.warned("pool"); n != 0 {
		t.Fatalf("%d warned leases after the release, want none: the record must not outlive the lease", n)
	}
}
