package poolmgr

import (
	"sort"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// hostSelector is the HostSelector implementation. It holds no state.
type hostSelector struct{}

// NewHostSelector returns the HostSelector that picks a Profile's Pool
// Hosts out of the Inventory.
func NewHostSelector() HostSelector { return hostSelector{} }

//= docs/requirements/04-pool-manager.md#pool-declaration
//# The Scheduler SHALL set each Pool's `flintlock_hosts` to the Hosts in
//# the Inventory that satisfy the Profile's Host selector and architecture.

// Select implements HostSelector. A Host qualifies when its architecture is
// the Profile's and every label requirement of the Profile's Host selector
// is present on the Host with the same value (CF-024); a Profile with no
// selector takes every Host of its architecture. The result is sorted, so
// that a Pool's flintlock_hosts do not churn between declarations because
// the Inventory was ordered differently.
func (hostSelector) Select(p config.Profile, inventory []config.HostEntry) []string {
	var names []string
	for _, host := range inventory {
		if p.Arch != "" && host.Arch != p.Arch {
			continue
		}
		if !matchesSelector(p.HostSelector, host.Labels) {
			continue
		}
		names = append(names, host.Name)
	}
	sort.Strings(names)
	return names
}

// matchesSelector reports whether labels satisfies every requirement, by
// equality.
func matchesSelector(selector, labels map[string]string) bool {
	for key, want := range selector {
		if got, ok := labels[key]; !ok || got != want {
			return false
		}
	}
	return true
}

// Compile-time interface check.
var _ HostSelector = hostSelector{}
