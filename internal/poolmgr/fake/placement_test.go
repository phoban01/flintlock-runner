package fake

import (
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL place MicroVMs across a Pool's
//# `flintlock_hosts` by least MicroVM count, matching battery's design.

// TestPlacementByLeastVMCount fills Pools of several sizes over several
// Hosts and checks where each MicroVM landed, on the fake's own record and
// on the Hosts themselves. Placement is over the Pool's flintlock_hosts, so
// a Host the fake knows but the Pool does not name gets nothing, and ties
// are broken by the order of flintlock_hosts as battery's PickHost does.
func TestPlacementByLeastVMCount(t *testing.T) {
	tests := []struct {
		name  string
		known []string
		pool  []string
		size  int32
		want  map[string]int
	}{
		{
			name:  "one host takes everything",
			known: []string{"host-a"},
			pool:  []string{"host-a"},
			size:  3,
			want:  map[string]int{"host-a": 3},
		},
		{
			name:  "an exact multiple spreads evenly",
			known: []string{"host-a", "host-b", "host-c"},
			pool:  []string{"host-a", "host-b", "host-c"},
			size:  6,
			want:  map[string]int{"host-a": 2, "host-b": 2, "host-c": 2},
		},
		{
			name:  "the remainder goes to the first hosts",
			known: []string{"host-a", "host-b", "host-c"},
			pool:  []string{"host-a", "host-b", "host-c"},
			size:  4,
			want:  map[string]int{"host-a": 2, "host-b": 1, "host-c": 1},
		},
		{
			name:  "hosts outside flintlock_hosts are not placed on",
			known: []string{"host-a", "host-b", "host-c"},
			pool:  []string{"host-b", "host-c"},
			size:  4,
			want:  map[string]int{"host-b": 2, "host-c": 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, poolmgr.FakeConfig{}, tt.known...)
			h.createPool(h.spec("pool", tt.size, tt.pool...))

			if got := h.vmHosts(); !sameCounts(got, tt.want) {
				t.Fatalf("placement = %v, want %v", got, tt.want)
			}
			for _, name := range tt.known {
				created, _ := h.stubs[name].counts()
				if created != tt.want[name] {
					t.Fatalf("host %s created %d microvms, want %d", name, created, tt.want[name])
				}
			}
		})
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL place MicroVMs across a Pool's
//# `flintlock_hosts` by least MicroVM count, matching battery's design.

// TestPlacementFillsTheEmptiestHost makes the second Host of a balanced Pool
// lose a MicroVM and asserts that the replacement goes back to it, because
// it is the Host with the fewest. Round-robin placement, the fake's other
// mode, would hand out the first Host instead, and the test asserts that
// difference so that a least-VM-count placement cannot be mistaken for a
// cycle that happens to look balanced.
func TestPlacementFillsTheEmptiestHost(t *testing.T) {
	tests := []struct {
		placement poolmgr.PlacementStrategy
		want      map[string]int
	}{
		{placement: poolmgr.PlacementLeastVMs, want: map[string]int{"host-a": 2, "host-b": 2}},
		{placement: poolmgr.PlacementRoundRobin, want: map[string]int{"host-a": 3, "host-b": 1}},
	}
	for _, tt := range tests {
		t.Run(string(tt.placement), func(t *testing.T) {
			h := newHarness(t, poolmgr.FakeConfig{Placement: tt.placement}, "host-a", "host-b")
			spec := h.spec("pool", 4, "host-a", "host-b")
			spec.Replenishment = poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
			h.createPool(spec)
			if got := h.vmHosts(); !sameCounts(got, map[string]int{"host-a": 2, "host-b": 2}) {
				t.Fatalf("initial placement = %v, want two on each host", got)
			}

			// Claims go to the longest-available MicroVM, so the first is the
			// one on host-a and the second the one on host-b. Releasing only
			// the second leaves host-b one short: a leased MicroVM still
			// counts towards its Host.
			first := h.claim("pool")
			second := h.claim("pool")
			if host := hostOfUID(t, h.pm.VMs(), first.VMUID); host != "host-a" {
				t.Fatalf("first claim is on %q, want host-a", host)
			}
			if host := hostOfUID(t, h.pm.VMs(), second.VMUID); host != "host-b" {
				t.Fatalf("second claim is on %q, want host-b", host)
			}
			h.release(second.LeaseID)
			h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)

			if got := h.vmHosts(); !sameCounts(got, tt.want) {
				t.Fatalf("placement after the replacement = %v, want %v", got, tt.want)
			}
		})
	}
}

// hostOfUID returns the Host a MicroVM is placed on.
func hostOfUID(t *testing.T, records []poolmgr.VMRecord, uid string) string {
	t.Helper()
	for _, r := range records {
		if r.UID == uid {
			return r.Host
		}
	}
	t.Fatalf("no microvm %q among %d records", uid, len(records))
	return ""
}

// sameCounts compares two placement maps, treating a missing key as zero.
func sameCounts(got, want map[string]int) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	for k, v := range got {
		if want[k] != v {
			return false
		}
	}
	return true
}
