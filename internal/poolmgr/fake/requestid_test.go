package fake

import (
	"errors"
	"strings"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL answer a `ClaimVM` whose `request_id` names
//# an existing Lease with that Lease, without extending its expiry; with
//# `INVALID_ARGUMENT` when that Lease is from another Pool; and with
//# `ABORTED` while that Lease's MicroVM is being deleted.

// TestClaimReplaysByRequestID claims with a request id, then repeats the
// call: the repeat gets the same Lease and MicroVM, the Pool has lost one
// MicroVM, not two, and the Lease's expiry is where the first call left it.
// A claim with no request id is never replayed, and an id longer than
// battery allows is refused.
func TestClaimReplaysByRequestID(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	h.createPool(h.spec("pool", 3, "host-a"))

	first, err := h.client.ClaimVM(h.ctx, h.ref("pool"), "req-1")
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)
	h.advance(10 * time.Second)

	again, err := h.client.ClaimVM(h.ctx, h.ref("pool"), "req-1")
	if err != nil {
		t.Fatalf("ClaimVM with the same request id: %v", err)
	}
	if again.LeaseID != first.LeaseID || again.VMUID != first.VMUID {
		t.Fatalf("replay = lease %s on %s, want the first call's lease %s on %s", again.LeaseID, again.VMUID, first.LeaseID, first.VMUID)
	}
	if again.Host != first.Host {
		t.Errorf("replay host = %+v, want %+v", again.Host, first.Host)
	}
	leases := h.pm.Leases()
	if len(leases) != 1 {
		t.Fatalf("leases after the replay = %+v, want one", leases)
	}
	if leases[0].RequestID != "req-1" {
		t.Errorf("lease request id = %q, want req-1", leases[0].RequestID)
	}
	if want := testEpoch.Add(30 * time.Second); !leases[0].ExpiresAt.Equal(want) {
		t.Errorf("lease expiry after the replay = %s, want %s: a replay is not a heartbeat", leases[0].ExpiresAt, want)
	}
	if st := h.pool("pool").Status; st.Leased != 1 {
		t.Errorf("pool status after the replay = %+v, want one leased", st)
	}

	// No request id: every call is a new claim.
	for i := 0; i < 2; i++ {
		if _, err := h.client.ClaimVM(h.ctx, h.ref("pool"), ""); err != nil {
			t.Fatalf("ClaimVM with no request id (%d): %v", i, err)
		}
		h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)
	}
	if got := h.pm.Leases(); len(got) != 3 {
		t.Fatalf("leases after two claims with no request id = %d, want 3", len(got))
	}

	_, err = h.client.ClaimVM(h.ctx, h.ref("pool"), strings.Repeat("x", 256))
	if !errors.Is(err, poolmgr.ErrInvalid) {
		t.Fatalf("ClaimVM with a 256-byte request id = %v, want ErrInvalid", err)
	}
}

// TestClaimReplayFromAnotherPoolIsInvalid reuses a request id against a
// different Pool: the same id names one Lease, and that Lease is not in the
// Pool asked for, so the call is refused rather than answered or claimed.
func TestClaimReplayFromAnotherPoolIsInvalid(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	h.createPool(h.spec("one", 1, "host-a"))
	h.createPool(h.spec("two", 1, "host-a"))

	if _, err := h.client.ClaimVM(h.ctx, h.ref("one"), "req-1"); err != nil {
		t.Fatalf("ClaimVM(one): %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)

	_, err := h.client.ClaimVM(h.ctx, h.ref("two"), "req-1")
	if !errors.Is(err, poolmgr.ErrInvalid) {
		t.Fatalf("ClaimVM(two) with pool one's request id = %v, want ErrInvalid", err)
	}
	if st := h.pool("two").Status; st.Leased != 0 || st.Available != 1 {
		t.Errorf("pool two after the refused claim = %+v, want untouched", st)
	}
}

// TestClaimReplayWhileReleasingIsAborted holds the released MicroVM's
// deletion on the Host and repeats the claim meanwhile: the Lease still
// exists, but its MicroVM is going, so the answer is ABORTED. Once the
// deletion completes the request id is free, and the same call claims a
// new MicroVM.
func TestClaimReplayWhileReleasingIsAborted(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	host := h.stubs["host-a"]
	spec := h.spec("pool", 2, "host-a")
	spec.Replenishment = poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)

	first, err := h.client.ClaimVM(h.ctx, h.ref("pool"), "req-1")
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)

	gate := make(chan struct{})
	host.set(func(s *stubHost) { s.deleteGate = gate })
	released := make(chan error, 1)
	go func() { released <- h.client.ReleaseVM(h.ctx, first.LeaseID) }()
	h.waitDeleteCall(host)

	_, err = h.client.ClaimVM(h.ctx, h.ref("pool"), "req-1")
	if !errors.Is(err, poolmgr.ErrAborted) {
		t.Fatalf("ClaimVM while the lease's microvm is being deleted = %v, want ErrAborted", err)
	}

	host.set(func(s *stubHost) { s.deleteGate = nil })
	close(gate)
	if err := <-released; err != nil {
		t.Fatalf("ReleaseVM once the host answered: %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)

	second, err := h.client.ClaimVM(h.ctx, h.ref("pool"), "req-1")
	if err != nil {
		t.Fatalf("ClaimVM once the lease is gone: %v", err)
	}
	if second.LeaseID == first.LeaseID || second.VMUID == first.VMUID {
		t.Fatalf("claim after the release = lease %s on %s, want a new lease on a new microvm, not %s on %s",
			second.LeaseID, second.VMUID, first.LeaseID, first.VMUID)
	}
	if got := h.pm.Leases(); len(got) != 1 || got[0].RequestID != "req-1" {
		t.Fatalf("leases = %+v, want the new lease alone, with the request id", got)
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL support fault injection that claims a
//# MicroVM and answers `UNAVAILABLE` instead of the Lease, for a set number
//# of claims.

// TestFaultLoseClaimResponse injects one lost response: the claim is made
// and the caller gets UNAVAILABLE, as it does from a call that timed out
// after battery claimed. The retry with the same request id gets the Lease
// the lost call made; the fault is consumed, so a claim with another id is
// a new one.
func TestFaultLoseClaimResponse(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	h.createPool(h.spec("pool", 2, "host-a"))
	h.pm.SetFaults(poolmgr.Faults{LoseClaimResponses: 1})

	_, err := h.client.ClaimVM(h.ctx, h.ref("pool"), "req-1")
	if !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Fatalf("ClaimVM with its response lost = %v, want ErrUnavailable", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)
	leases := h.pm.Leases()
	if len(leases) != 1 || leases[0].RequestID != "req-1" {
		t.Fatalf("leases after the lost response = %+v, want the one the lost call made", leases)
	}
	if f := h.pm.Faults(); f.LoseClaimResponses != 0 {
		t.Errorf("LoseClaimResponses after one lost response = %d, want 0", f.LoseClaimResponses)
	}

	retry, err := h.client.ClaimVM(h.ctx, h.ref("pool"), "req-1")
	if err != nil {
		t.Fatalf("retry with the same request id: %v", err)
	}
	if retry.LeaseID != leases[0].LeaseID || retry.VMUID != leases[0].VMUID {
		t.Fatalf("retry = lease %s on %s, want the lost call's %s on %s", retry.LeaseID, retry.VMUID, leases[0].LeaseID, leases[0].VMUID)
	}

	other, err := h.client.ClaimVM(h.ctx, h.ref("pool"), "req-2")
	if err != nil {
		t.Fatalf("claim with another request id after the fault was consumed: %v", err)
	}
	if other.LeaseID == retry.LeaseID {
		t.Fatal("a claim with another request id got the replayed lease")
	}
	if got := h.pm.Leases(); len(got) != 2 {
		t.Fatalf("leases = %d, want 2", len(got))
	}
}
