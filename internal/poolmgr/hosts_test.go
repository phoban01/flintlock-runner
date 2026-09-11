package poolmgr_test

import (
	"context"
	"slices"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// testInventory is a mixed Inventory: two arm64 Hosts with different
// labels and one amd64 Host.
func testInventory() []config.HostEntry {
	return []config.HostEntry{
		{Name: "arm-fast", Endpoint: "10.0.0.1:9090", Arch: config.ArchARM64, Labels: map[string]string{"disk": "nvme", "zone": "a"}},
		{Name: "arm-slow", Endpoint: "10.0.0.2:9090", Arch: config.ArchARM64, Labels: map[string]string{"disk": "hdd", "zone": "b"}},
		{Name: "amd-fast", Endpoint: "10.0.0.3:9090", Arch: config.ArchAMD64, Labels: map[string]string{"disk": "nvme", "zone": "a"}},
	}
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//= type=test
//# The Scheduler SHALL set each Pool's `flintlock_hosts` to the Hosts in
//# the Inventory that satisfy the Profile's Host selector and architecture.

// TestHostsAreSelectedByArchitectureAndSelector covers architecture on its
// own, a selector on its own, the two together and a selector nothing
// satisfies, and then checks that the selection is what reaches the Pool's
// flintlock_hosts.
func TestHostsAreSelectedByArchitectureAndSelector(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		arch     config.Architecture
		selector map[string]string
		want     []string
	}{
		{
			name: "architecture only",
			arch: config.ArchARM64,
			want: []string{"arm-fast", "arm-slow"},
		},
		{
			name: "the other architecture",
			arch: config.ArchAMD64,
			want: []string{"amd-fast"},
		},
		{
			name:     "architecture and one label",
			arch:     config.ArchARM64,
			selector: map[string]string{"disk": "nvme"},
			want:     []string{"arm-fast"},
		},
		{
			name:     "every label has to match",
			arch:     config.ArchARM64,
			selector: map[string]string{"disk": "nvme", "zone": "b"},
			want:     nil,
		},
		{
			name:     "a label no host carries",
			arch:     config.ArchARM64,
			selector: map[string]string{"gpu": "yes"},
			want:     nil,
		},
		{
			name:     "a label whose value differs",
			arch:     config.ArchAMD64,
			selector: map[string]string{"disk": "hdd"},
			want:     nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			profile := testProfile("p", 1)
			profile.Arch = tc.arch
			profile.HostSelector = tc.selector

			got := poolmgr.NewHostSelector().Select(profile, testInventory())
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Select = %v, want %v", got, tc.want)
			}

			spec, err := poolmgr.NewSpecBuilder().Build(profile, poolmgr.SpecInput{
				RunnerName: testRunner,
				Namespace:  testNamespace,
				Hosts:      got,
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !slices.Equal(spec.FlintlockHosts, tc.want) {
				t.Errorf("flintlock_hosts = %v, want %v", spec.FlintlockHosts, tc.want)
			}
		})
	}
}

// TestSelectedHostsAreWhereTheMicroVMsLand checks the selection end to end:
// a Pool declared with one of two Hosts places every MicroVM on that Host.
func TestSelectedHostsAreWhereTheMicroVMsLand(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "arm-fast", "arm-slow")
	c := pm.client()

	profile := testProfile("nvme-only", 2)
	profile.HostSelector = map[string]string{"disk": "nvme"}
	hosts := poolmgr.NewHostSelector().Select(profile, testInventory())
	spec := specFor(t, profile, hosts...)
	pm.fillPool(c, spec)

	for _, vm := range pm.vms() {
		if vm.Host != "arm-fast" {
			t.Errorf("microvm %s is on %q, want the only selected host arm-fast", vm.UID, vm.Host)
		}
	}
	sandboxes, err := pm.host("arm-slow").Sandboxes()
	if err != nil {
		t.Fatalf("listing the sandboxes of the host that was not selected: %v", err)
	}
	if len(sandboxes) != 0 {
		t.Errorf("the host the selector left out holds %v, want nothing", sandboxes)
	}
}
