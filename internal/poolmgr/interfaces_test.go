package poolmgr

import (
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
)

// TestEnumAliasesTrackProto pins the alias types to the generated enums: the
// assignments below compile only while each alias is identical to its proto
// type, so a proto bump that renames a value fails here rather than at
// runtime.
func TestEnumAliasesTrackProto(t *testing.T) {
	t.Parallel()
	var got struct {
		Strategy ReplenishmentStrategyType
		Phase    VMPhase
		Event    EventType
		Policy   HookFailurePolicy
	}
	got.Strategy = poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE
	got.Phase = poolmgrv1.VMPhase_AVAILABLE
	got.Event = poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET
	got.Policy = poolmgrv1.HookFailurePolicy_QUARANTINE

	want := map[string]string{
		got.Strategy.String(): "IMMEDIATE_ON_LEASE",
		got.Phase.String():    "AVAILABLE",
		got.Event.String():    "POOL_SIZE_BELOW_TARGET",
		got.Policy.String():   "QUARANTINE",
	}
	for have, expect := range want {
		if have != expect {
			t.Errorf("enum renders as %q, want %q", have, expect)
		}
	}
}

func TestClaimCarriesHostFromProto(t *testing.T) {
	t.Parallel()
	resp := &poolmgrv1.ClaimVMResponse{LeaseId: "l1", VmUid: "vm1", Host: &poolmgrv1.HostInfo{Name: "h1", Address: "10.0.0.1:9090"}}
	c := Claim{LeaseID: resp.GetLeaseId(), VMUID: resp.GetVmUid(), Host: HostRef{Name: resp.GetHost().GetName(), Address: resp.GetHost().GetAddress()}}
	if c.Host != (HostRef{Name: "h1", Address: "10.0.0.1:9090"}) {
		t.Errorf("Host = %+v", c.Host)
	}
	if got := (PoolRef{Name: "p", Namespace: "ns"}).String(); got != "ns/p" {
		t.Errorf("PoolRef.String = %q", got)
	}
}
