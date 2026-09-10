// Package inventory turns the Inventory section of the configuration into
// the Endpoints the Host Registry is built from (docs/requirements/05-hosts.md#inventory).
//
// It is a package of its own because internal/flintlock deliberately does
// not depend on the configuration schema: the Host client and the fake Host
// are dialled from Endpoint literals. This is the one place that knows both
// types, so the Runner's wiring and its reload path (HO-014) share one
// translation instead of writing it twice.
package inventory

import (
	"context"
	"fmt"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

//= docs/requirements/05-hosts.md#inventory
//# The Runner SHALL load the set of Hosts from the Inventory
//# section of its configuration, keyed by the Host names the Pool Manager
//# uses in `flintlock_hosts`.

// Endpoints translates the Inventory section into one Endpoint per Host, in
// Inventory order. The Endpoint's Name is the Inventory name, which is the
// name the Runner declares to the Pool Manager in a Pool's `flintlock_hosts`
// (config.SelectHosts) and the name the Pool Manager reports back as a
// Placement, so the Registry that keys on it can resolve a Placement to a
// connection without a second lookup table.
//
// The token is resolved from the configuration's Secret, and the TLS
// material is carried across as it stands; a Host marked insecure is the
// only one dialled in plaintext (SE-021).
func Endpoints(hosts []config.HostEntry) []flintlock.Endpoint {
	if len(hosts) == 0 {
		return nil
	}
	endpoints := make([]flintlock.Endpoint, 0, len(hosts))
	for i := range hosts {
		endpoints = append(endpoints, Endpoint(&hosts[i]))
	}
	return endpoints
}

// Endpoint translates one Inventory entry.
func Endpoint(h *config.HostEntry) flintlock.Endpoint {
	return flintlock.Endpoint{
		Name:    h.Name,
		Address: h.Endpoint,
		Token:   string(h.Token),
		TLS: flintlock.TLSOptions{
			CAFile:   h.TLS.CAFile,
			CertFile: h.TLS.CertFile,
			KeyFile:  h.TLS.KeyFile,
			Insecure: h.TLS.Insecure,
		},
	}
}

// Apply reconciles a Registry with the Inventory of a reloaded
// configuration (HO-014): it is Registry.Apply over the translation above,
// so that the Runner's reload path does not have to know either type.
func Apply(ctx context.Context, reg flintlock.Registry, hosts []config.HostEntry) error {
	if err := reg.Apply(ctx, Endpoints(hosts)); err != nil {
		return fmt.Errorf("applying inventory: %w", err)
	}
	return nil
}
