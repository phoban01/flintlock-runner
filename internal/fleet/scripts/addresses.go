package scripts

import (
	"fmt"
	"net/netip"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// Gateway returns the guest bridge gateway address for a guest subnet: the
// first address in it (FL-040). Every Host Service binds it (FL-101).
func Gateway(subnet string) (string, error) {
	if subnet == "" {
		subnet = config.DefaultGuestSubnet
	}
	p, err := netip.ParsePrefix(subnet)
	if err != nil || !p.Addr().Is4() || p.Bits() > 29 {
		return "", fmt.Errorf("guest subnet %q: need an IPv4 CIDR of /29 or larger", subnet)
	}
	return p.Masked().Addr().Next().String(), nil
}

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL record the address and port of each
//# Host Service in the Host's Inventory entry.

// ServiceAddresses are the Host Service addresses a guest on a Host
// reaches, for the Host's Inventory entry: the bridge gateway and each
// enabled service's port. A disabled service has no address, so the
// Executor injects nothing for it (FL-111, EX-062).
func ServiceAddresses(hs config.HostServices, subnet string) (config.HostServiceAddresses, error) {
	gw, err := Gateway(subnet)
	if err != nil {
		return config.HostServiceAddresses{}, err
	}
	var a config.HostServiceAddresses
	if hs.Buildkit.IsEnabled() {
		a.Buildkit = fmt.Sprintf("tcp://%s:%d", gw, hs.Buildkit.Port)
	}
	if hs.GoProxy.IsEnabled() {
		a.GoProxy = fmt.Sprintf("http://%s:%d", gw, hs.GoProxy.Port)
	}
	if hs.RegistryMirror.IsEnabled() {
		a.RegistryMirror = fmt.Sprintf("http://%s:%d", gw, hs.RegistryMirror.Port)
	}
	if hs.HTTPCache.IsEnabled() && len(hs.HTTPCache.Upstreams) > 0 {
		a.HTTPCache = make(map[string]string, len(hs.HTTPCache.Upstreams))
		for _, u := range hs.HTTPCache.Upstreams {
			a.HTTPCache[u.Name] = fmt.Sprintf("http://%s:%d/%s", gw, hs.HTTPCache.Port, u.Name)
		}
	}
	return a, nil
}
