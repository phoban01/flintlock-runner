package config

//= docs/requirements/07-configuration.md#profiles-section
//# A Profile's Host selector SHALL be expressed as label
//# requirements matched against Inventory labels and SHALL determine the
//# Pool's host list.

// MatchesHost reports whether h can carry p's Pool: the architectures agree
// and every label requirement of the Host selector is present on h with the
// same value. A Profile without a selector matches every Host of its
// architecture.
func MatchesHost(p *Profile, h *HostEntry) bool {
	if p.Arch != h.Arch {
		return false
	}
	for k, want := range p.HostSelector {
		if got, ok := h.Labels[k]; !ok || got != want {
			return false
		}
	}
	return true
}

// SelectHosts returns, in Inventory order, the names of the Hosts that
// MatchesHost accepts for p. It is the Pool's `flintlock_hosts` list
// (CF-024, PL-011).
func SelectHosts(p *Profile, inventory []HostEntry) []string {
	var names []string
	for i := range inventory {
		if MatchesHost(p, &inventory[i]) {
			names = append(names, inventory[i].Name)
		}
	}
	return names
}

//= docs/requirements/07-configuration.md#inventory-section
//# The Host name in an Inventory entry SHALL be the name the Pool
//# Manager uses for that Host in `flintlock_hosts`.

// HostByName finds the Inventory entry for the Host name a Placement
// reports. The Pool Manager identifies Hosts by the very names the Runner
// declares in `flintlock_hosts` (SelectHosts), so the join is by exact name;
// a Placement naming a Host that is not in the Inventory yields false.
func HostByName(inventory []HostEntry, name string) (*HostEntry, bool) {
	for i := range inventory {
		if inventory[i].Name == name {
			return &inventory[i], true
		}
	}
	return nil, false
}
