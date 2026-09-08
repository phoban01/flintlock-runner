package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

func TestSkeleton(t *testing.T) {
	t.Parallel()
	pm := New(poolmgr.FakeConfig{})
	if pm.Config().Placement != poolmgr.PlacementLeastVMs {
		t.Errorf("default placement = %q, want least_vms", pm.Config().Placement)
	}
	pm.SetFaults(poolmgr.Faults{RefuseHeartbeats: true})
	if !pm.Faults().RefuseHeartbeats {
		t.Error("faults not stored")
	}
	if pm.Leases() != nil || pm.VMs() != nil {
		t.Error("skeleton should hold no records")
	}
	c := pm.Client()
	if _, err := c.ClaimVM(context.Background(), poolmgr.PoolRef{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("ClaimVM error = %v, want ErrUnsupported", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close error = %v", err)
	}
}
