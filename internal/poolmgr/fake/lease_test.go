package fake

import (
	"errors"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc/codes"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL expire a Lease whose last heartbeat is older
//# than the Pool's heartbeat expiry threshold and SHALL delete the expired
//# Lease's MicroVM.

// TestLeaseExpiryDeletesTheMicroVM holds a Lease across the expiry threshold
// on the fake clock. A heartbeat inside the threshold keeps the Lease and
// its MicroVM; once the last heartbeat is older than the threshold the fake
// drops the Lease and deletes the MicroVM from the Host it was placed on.
func TestLeaseExpiryDeletesTheMicroVM(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	spec := h.spec("pool", 1, "host-a")
	spec.Replenishment = poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	host := h.stubs["host-a"]

	claim := h.claim("pool")
	if got := h.pm.Leases(); len(got) != 1 || !got[0].ExpiresAt.Equal(testEpoch.Add(30*time.Second)) {
		t.Fatalf("leases after the claim = %+v, want one expiring at the threshold", got)
	}

	// Well inside the threshold: a heartbeat moves the expiry out.
	h.advance(25 * time.Second)
	expires, err := h.client.Heartbeat(h.ctx, claim.LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if want := testEpoch.Add(55 * time.Second); !expires.Equal(want) {
		t.Fatalf("Heartbeat expiry = %s, want %s", expires, want)
	}
	h.advance(20 * time.Second)
	if got := h.pm.Leases(); len(got) != 1 {
		t.Fatalf("leases 45s in, 20s after a heartbeat = %+v, want the lease still held", got)
	}
	if _, deleted := host.counts(); deleted != 0 {
		t.Fatalf("%d microvms deleted while the lease was alive, want 0", deleted)
	}

	// Past the extended expiry with no further heartbeat.
	h.advance(20 * time.Second)
	expired := h.waitEvent(poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY)
	if expired.VMUID != claim.VMUID {
		t.Fatalf("VM_DELETED_DUE_TO_EXPIRY for %q, want the leased microvm %q", expired.VMUID, claim.VMUID)
	}
	if got := payloadOf(t, expired)["lease_id"]; got != claim.LeaseID {
		t.Fatalf("VM_DELETED_DUE_TO_EXPIRY lease_id = %v, want %q", got, claim.LeaseID)
	}
	if got := h.pm.Leases(); len(got) != 0 {
		t.Fatalf("leases after expiry = %+v, want none", got)
	}
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Fatalf("Heartbeat on the expired lease = %v, want ErrNotFound", err)
	}
	assertDeleted(t, host, claim.VMUID)
}

// assertDeleted fails unless uid was deleted on the Host and is gone from it.
func assertDeleted(t *testing.T, host *stubHost, uid string) {
	t.Helper()
	host.mu.Lock()
	defer host.mu.Unlock()
	if _, ok := host.vms[uid]; ok {
		t.Fatalf("microvm %s is still on host %s", uid, host.name)
	}
	for _, d := range host.deleted {
		if d == uid {
			return
		}
	}
	t.Fatalf("microvm %s was never deleted on host %s (deleted: %v)", uid, host.name, host.deleted)
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL return `RESOURCE_EXHAUSTED` from `ClaimVM`
//# when the Pool has no MicroVM in the `AVAILABLE` phase.

// TestClaimOnEmptyPoolIsResourceExhausted asserts the gRPC status code on the
// wire, because the Scheduler's exhaustion path (PL-032) keys on the code,
// and checks the two ways a Pool can be empty: never filled, and every
// MicroVM already leased. The typed client's mapping onto ErrExhausted is
// asserted alongside it.
func TestClaimOnEmptyPoolIsResourceExhausted(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	lease := h.rawLease()

	// A Pool with no MicroVM at all.
	empty := h.spec("empty", 0, "host-a")
	h.createPool(empty)
	_, err := lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("empty"))})
	if code := statusCode(t, err); code != codes.ResourceExhausted {
		t.Fatalf("ClaimVM on an empty pool: code %v, want RESOURCE_EXHAUSTED", code)
	}
	if _, err := h.client.ClaimVM(h.ctx, h.ref("empty")); !errors.Is(err, poolmgr.ErrExhausted) {
		t.Fatalf("ClaimVM through the client = %v, want ErrExhausted", err)
	}

	// A Pool whose only MicroVM is leased. REPLACE_ON_DELETE does not
	// replenish on a claim, so the Pool stays empty until the release.
	spec := h.spec("one", 1, "host-a")
	spec.Replenishment = poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	claim := h.claim("one")
	if st := h.pool("one").Status; st.Available != 0 || st.Leased != 1 {
		t.Fatalf("status after the claim = %+v, want nothing available", st)
	}
	_, err = lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("one"))})
	if code := statusCode(t, err); code != codes.ResourceExhausted {
		t.Fatalf("ClaimVM on a fully leased pool: code %v, want RESOURCE_EXHAUSTED", code)
	}

	// The replacement the release starts makes the Pool claimable again, so
	// the code really tracks the AVAILABLE phase and is not a constant.
	h.release(claim.LeaseID)
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	if _, err := lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("one"))}); err != nil {
		t.Fatalf("ClaimVM after the pool refilled: %v", err)
	}

	// An unknown Pool is NOT_FOUND, not RESOURCE_EXHAUSTED.
	_, err = lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("nope"))})
	if code := statusCode(t, err); code != codes.NotFound {
		t.Fatalf("ClaimVM on an unknown pool: code %v, want NOT_FOUND", code)
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL populate the Host name on `ClaimVMResponse`
//# when the proto carries a field for it and SHALL provide a switch that
//# omits it, so that both Placement paths of the Scheduler are tested.

// TestHostOnClaimResponse claims through the generated Lease client so that
// the host field is observed on the wire, and drives both settings of the
// switch: the Scheduler's direct path reads the name and address off the
// response, and its fan-out path (SC-031) needs the field to be genuinely
// absent.
func TestHostOnClaimResponse(t *testing.T) {
	tests := []struct {
		name     string
		omit     bool
		wantHost bool
	}{
		{name: "host reported", wantHost: true},
		{name: "host omitted", omit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, poolmgr.FakeConfig{OmitHostOnClaim: tt.omit}, "host-a")
			h.createPool(h.spec("pool", 1, "host-a"))

			resp, err := h.rawLease().ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("pool"))})
			if err != nil {
				t.Fatalf("ClaimVM: %v", err)
			}
			if !tt.wantHost {
				if resp.Host != nil {
					t.Fatalf("ClaimVMResponse.host = %v, want it unset", resp.GetHost())
				}
				return
			}
			if resp.GetHost().GetName() != "host-a" {
				t.Fatalf("ClaimVMResponse.host.name = %q, want %q", resp.GetHost().GetName(), "host-a")
			}
			if got, want := resp.GetHost().GetAddress(), "host-a:9090"; got != want {
				t.Fatalf("ClaimVMResponse.host.address = %q, want %q", got, want)
			}
			// The name is the Host the MicroVM was really placed on.
			for _, vm := range h.pm.VMs() {
				if vm.UID == resp.GetVmUid() && vm.Host != resp.GetHost().GetName() {
					t.Fatalf("microvm %s is on %q but the claim reported %q", vm.UID, vm.Host, resp.GetHost().GetName())
				}
			}
			// The typed client carries the same Host through to the
			// Scheduler. IMMEDIATE_ON_LEASE replaced the claimed MicroVM, so
			// there is one to claim again once it reports available.
			h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
			claim, err := h.client.ClaimVM(h.ctx, h.ref("pool"))
			if err != nil {
				t.Fatalf("ClaimVM through the client: %v", err)
			}
			if claim.Host.Name != "host-a" || claim.Host.Address != "host-a:9090" {
				t.Fatalf("Claim.Host = %+v, want host-a at host-a:9090", claim.Host)
			}
		})
	}
}
