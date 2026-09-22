package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// kubernetesBackend selects the Kubernetes pool backend in a test env.
func kubernetesBackend(s *Settings) {
	s.PoolManager.Backend = config.PoolBackendKubernetes
}

//= docs/requirements/12-cluster-fleet.md#kube-allocation
//= type=test
//# Where the Kubernetes pool backend is configured, the Scheduler
//# SHALL record the claimed pod's Virtual Node as the Placement without
//# looking it up in the Inventory, and SC-034 SHALL NOT apply.

// TestVirtualNodeIsThePlacementWithoutAnInventory is the claim of
// TestPlacementOnAHostOutsideTheInventoryReleasesTheLease on the Kubernetes
// pool backend, with no Inventory at all: the Virtual Node the claim names
// becomes the Placement, and nothing is released.
func TestVirtualNodeIsThePlacementWithoutAnInventory(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = func(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
			return &poolmgr.Claim{
				LeaseID: "pool-default-abcde",
				VMUID:   "pod-uid-1",
				Host:    poolmgr.HostRef{Name: "node-7-microvms"},
			}, nil
		}
	})
	e := newEnv(t, envConfig{client: client, inventory: []config.HostEntry{}, tune: kubernetesBackend})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)

	r := e.reserve(ctx)
	h, err := e.sched.Allocate(ctx, r, JobInfo{ID: 12}, e.profile("default"))
	if err != nil {
		t.Fatalf("Allocate = %v, want the Virtual Node accepted as the Placement", err)
	}
	pl := h.Allocation().Placement
	if pl.Host != "node-7-microvms" || pl.Source != PlacementFromClaim {
		t.Errorf("Placement = %+v, want node-7-microvms from the claim", pl)
	}
	if got := client.releasedLeases(); len(got) != 0 {
		t.Errorf("released leases = %v, want none", got)
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-allocation
//= type=test
//# When claiming for a Job, the Scheduler SHALL pass the Job's own
//# timeout to the Kubernetes pool backend

// TestClaimCarriesTheJobTimeout checks that the Job's timeout reaches the
// claim, and that a Job without one leaves the backend to its default.
func TestClaimCarriesTheJobTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		wantOK  bool
	}{
		{name: "job timeout", timeout: 3 * time.Hour, wantOK: true},
		{name: "no job timeout", timeout: 0, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := testContext(t)

			var (
				mu     sync.Mutex
				got    time.Duration
				gotOK  bool
				called bool
			)
			client := newStubClient()
			client.script(func(c *stubClient) {
				c.claimFn = func(ctx context.Context, _ poolmgr.PoolRef) (*poolmgr.Claim, error) {
					mu.Lock()
					defer mu.Unlock()
					got, gotOK = poolmgr.JobTimeoutFrom(ctx)
					called = true
					return &poolmgr.Claim{LeaseID: "lease-1", VMUID: "vm-1", Host: poolmgr.HostRef{Name: "node-1-microvms"}}, nil
				}
			})
			e := newEnv(t, envConfig{client: client, inventory: []config.HostEntry{}, tune: kubernetesBackend})
			e.startBare(ctx)
			e.tracker.setAvailable(e.poolOf("default"), 1)

			r := e.reserve(ctx)
			if _, err := e.sched.Allocate(ctx, r, JobInfo{ID: 13, Timeout: tc.timeout}, e.profile("default")); err != nil {
				t.Fatalf("Allocate: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !called {
				t.Fatal("the claim was never made")
			}
			if gotOK != tc.wantOK || (tc.wantOK && got != tc.timeout) {
				t.Errorf("claim context carries (%s, %t), want (%s, %t)", got, gotOK, tc.timeout, tc.wantOK)
			}
		})
	}
}
