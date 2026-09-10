package inventory_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/inventory"
)

//= docs/requirements/05-hosts.md#inventory
//= type=test
//# The Runner SHALL load the set of Hosts from the Inventory
//# section of its configuration, keyed by the Host names the Pool Manager
//# uses in `flintlock_hosts`.

// TestEndpointsComeFromTheInventorySection loads the Inventory section of a
// configuration and checks that every Host in it becomes an Endpoint with
// its address, token and TLS material, keyed by the Inventory name. The
// names are the ones the Runner declares to the Pool Manager as a Pool's
// flintlock_hosts, which is why the same list is built here with
// config.SelectHosts and compared: a Placement the Pool Manager reports has
// to resolve to one of these Endpoints or it cannot be reached.
func TestEndpointsComeFromTheInventorySection(t *testing.T) {
	t.Parallel()
	hosts := []config.HostEntry{
		{
			Name:     "metal-a",
			Endpoint: "10.0.0.1:9090",
			Arch:     config.ArchARM64,
			Token:    config.Secret("token-a"),
			TLS:      config.ClientTLS{CAFile: "/etc/ca.pem", CertFile: "/etc/client.pem", KeyFile: "/etc/client-key.pem"},
			Labels:   map[string]string{"zone": "a"},
		},
		{
			Name:     "metal-b",
			Endpoint: "10.0.0.2:9090",
			Arch:     config.ArchARM64,
			TLS:      config.ClientTLS{Insecure: true},
			Labels:   map[string]string{"zone": "b"},
		},
	}

	got := inventory.Endpoints(hosts)
	want := []flintlock.Endpoint{
		{
			Name:    "metal-a",
			Address: "10.0.0.1:9090",
			Token:   "token-a",
			TLS: flintlock.TLSOptions{
				CAFile:   "/etc/ca.pem",
				CertFile: "/etc/client.pem",
				KeyFile:  "/etc/client-key.pem",
			},
		},
		{
			Name:    "metal-b",
			Address: "10.0.0.2:9090",
			TLS:     flintlock.TLSOptions{Insecure: true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Endpoints returned\n%+v\nwant\n%+v", got, want)
	}

	// The keys are the flintlock_hosts names, which is what makes a
	// Placement resolvable.
	profile := &config.Profile{Arch: config.ArchARM64}
	byName := make(map[string]flintlock.Endpoint, len(got))
	for _, ep := range got {
		byName[ep.Name] = ep
	}
	for _, name := range config.SelectHosts(profile, hosts) {
		if _, ok := byName[name]; !ok {
			t.Errorf("pool host %s has no endpoint; a microvm placed there could not be reached", name)
		}
	}

	if inventory.Endpoints(nil) != nil {
		t.Error("an empty inventory produced endpoints")
	}
}

// TestApplyReloadsTheRegistry checks the reload path: the Inventory of a
// reloaded configuration reaches the Registry through one translation
// (HO-014).
func TestApplyReloadsTheRegistry(t *testing.T) {
	t.Parallel()
	reg := &recordingRegistry{}
	hosts := []config.HostEntry{{Name: "metal-a", Endpoint: "10.0.0.1:9090", TLS: config.ClientTLS{Insecure: true}}}
	if err := inventory.Apply(context.Background(), reg, hosts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []flintlock.Endpoint{{Name: "metal-a", Address: "10.0.0.1:9090", TLS: flintlock.TLSOptions{Insecure: true}}}
	if !reflect.DeepEqual(reg.applied, want) {
		t.Errorf("Apply passed %+v to the registry, want %+v", reg.applied, want)
	}
}

// recordingRegistry records what Apply was given.
type recordingRegistry struct {
	flintlock.Registry
	applied []flintlock.Endpoint
}

// Apply implements flintlock.Registry.
func (r *recordingRegistry) Apply(_ context.Context, endpoints []flintlock.Endpoint) error {
	r.applied = endpoints
	return nil
}
