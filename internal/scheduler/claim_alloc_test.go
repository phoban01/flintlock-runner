package scheduler

import (
	"context"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// claimBackend selects the claim pool backend in a test env.
func claimBackend(s *Settings) {
	s.PoolManager.Backend = config.PoolBackendClaim
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# and from nothing else.

//= docs/requirements/12-cluster-fleet.md#kube-allocation
//= type=test
//# Where the claim backend is configured, the Scheduler SHALL
//# record the Host that the Bound claim names as the Placement without
//# looking it up in the Inventory, and SC-034 SHALL NOT apply.

// TestClaimedHostIsThePlacementWithoutAnInventory is the claim of
// TestPlacementOnAHostOutsideTheInventoryReleasesTheLease on the claim
// backend, with no Inventory at all: the Host the claim's status names
// becomes the Placement, SC-034 does not apply, and nothing is released.
func TestClaimedHostIsThePlacementWithoutAnInventory(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
			return &poolmgr.Claim{
				LeaseID: "default-x7k2p",
				VMUID:   "microvm-1",
				Host:    poolmgr.HostRef{Name: "node-7", Address: "10.0.0.7:9443"},
			}, nil
		}
	})
	e := newEnv(t, envConfig{client: client, inventory: []config.HostEntry{}, tune: claimBackend})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	h, err := e.sched.Allocate(ctx, r, JobInfo{ID: 14}, e.profile("default"))
	if err != nil {
		t.Fatalf("Allocate = %v, want the claimed Host accepted as the Placement", err)
	}
	pl := h.Allocation().Placement
	if pl.Host != "node-7" || pl.Source != PlacementFromClaim {
		t.Errorf("Placement = %+v, want node-7 from the claim", pl)
	}
	if got := h.Allocation().Host; got.Address != "10.0.0.7:9443" {
		t.Errorf("Allocation Host = %+v, want the claim's agent address kept", got)
	}
	if got := client.releasedLeases(); len(got) != 0 {
		t.Errorf("released leases = %v, want none", got)
	}
}
