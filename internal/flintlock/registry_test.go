package flintlock_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// newRegistry builds a Registry over endpoints with the production Dialer
// and closes it when the test ends.
func newRegistry(t *testing.T, endpoints []flintlock.Endpoint) flintlock.Registry {
	t.Helper()
	reg, err := flintlock.NewRegistry(context.Background(), flintlock.NewDialer(), endpoints)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

//= docs/requirements/05-hosts.md#inventory
//= type=test
//# When the configuration is reloaded, the Runner SHALL add new
//# Hosts, stop probing removed Hosts and keep serving Jobs on removed Hosts
//# until they finish.

// TestApplyAddsAndRetiresHosts reloads a Registry that holds two Hosts with
// an Inventory that drops one and adds another. The new Host has to be
// reachable, the removed one has to disappear from the Registry so that
// nothing probes it again, and the client a Job already holds for it has to
// go on working, because a Job in the middle of a Stage cannot be told that
// its Host has left the configuration.
func TestApplyAddsAndRetiresHosts(t *testing.T) {
	t.Parallel()
	h1 := startHost(t, flintlock.FakeHostConfig{Name: "h1", Version: "v1", ExecEnabled: true})
	h2 := startHost(t, flintlock.FakeHostConfig{Name: "h2", Version: "v1", ExecEnabled: true})
	h3 := startHost(t, flintlock.FakeHostConfig{Name: "h3", Version: "v1", ExecEnabled: true})

	endpoint := func(name, addr string) flintlock.Endpoint {
		return flintlock.Endpoint{Name: name, Address: addr, TLS: flintlock.TLSOptions{Insecure: true}}
	}
	reg := newRegistry(t, []flintlock.Endpoint{
		endpoint("h1", h1.Addr()),
		endpoint("h2", h2.Addr()),
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// A Job is running on h2 and holds its client.
	running, release, err := reg.Lease("h2")
	if err != nil {
		t.Fatalf("Lease(h2): %v", err)
	}
	defer release()

	if err := reg.Apply(ctx, []flintlock.Endpoint{
		endpoint("h1", h1.Addr()),
		endpoint("h3", h3.Addr()),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got, want := reg.Names(), []string{"h1", "h3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the reload the registry holds %v, want %v", got, want)
	}
	if _, err := reg.Get("h2"); !errors.Is(err, flintlock.ErrUnknownHost) {
		t.Errorf("Get(h2) after it was removed returned %v, want ErrUnknownHost", err)
	}
	if _, ok := reg.Endpoint("h2"); ok {
		t.Error("the removed host still has an endpoint in the registry")
	}
	if _, err := reg.Get("h3"); err != nil {
		t.Errorf("Get(h3) after it was added: %v", err)
	}
	if ep, ok := reg.Endpoint("h3"); !ok || ep.Address != h3.Addr() {
		t.Errorf("Endpoint(h3) returned (%+v, %v), want the new address", ep, ok)
	}

	// The Job on the removed Host keeps running.
	if _, err := running.ServerInfo(ctx); err != nil {
		t.Errorf("the client of a removed host stopped working: %v", err)
	}

	// Closing the Registry releases it, so nothing is left behind.
	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := running.ServerInfo(ctx); err == nil {
		t.Error("the client of a removed host survived Close")
	}
}

//= docs/requirements/05-hosts.md#inventory
//= type=test
//# When the configuration is reloaded, the Runner SHALL add new
//# Hosts, stop probing removed Hosts and keep serving Jobs on removed Hosts
//# until they finish.

// TestReloadReleasesTheClientsItRetires is the far side of "until they
// finish". A removed Host's connection has to outlive the reload for the
// Job that is still running there, and no longer than that: each
// connection is dialled with keepalive permitted without streams, so one
// that is never released goes on pinging a machine that has left the
// Inventory for the life of the process, and a Runner whose Inventory is
// regenerated would collect one per reload.
//
// So the three cases are checked apart: a Host removed while a Job holds a
// lease keeps its connection until the Job releases it, a Host removed with
// nothing holding it loses its connection there and then, and releasing a
// lease on a Host that is still in the Inventory releases nothing.
func TestReloadReleasesTheClientsItRetires(t *testing.T) {
	t.Parallel()
	h1 := startHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	h2 := startHost(t, flintlock.FakeHostConfig{Name: "h2", ExecEnabled: true})
	h3 := startHost(t, flintlock.FakeHostConfig{Name: "h3", ExecEnabled: true})

	endpoint := func(name, addr string) flintlock.Endpoint {
		return flintlock.Endpoint{Name: name, Address: addr, TLS: flintlock.TLSOptions{Insecure: true}}
	}
	reg := newRegistry(t, []flintlock.Endpoint{endpoint("h1", h1.Addr()), endpoint("h2", h2.Addr())})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// A Job is running on h2 when the reload drops it.
	running, release, err := reg.Lease("h2")
	if err != nil {
		t.Fatalf("Lease(h2): %v", err)
	}
	if err := reg.Apply(ctx, []flintlock.Endpoint{endpoint("h1", h1.Addr())}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := running.ServerInfo(ctx); err != nil {
		t.Fatalf("the leased client of a removed host stopped working: %v", err)
	}

	// The Job finishes: nothing holds the removed Host any more, so its
	// connection goes rather than being kept until the process exits.
	release()
	if _, err := running.ServerInfo(ctx); err == nil {
		t.Error("the connection to a removed host outlived the last lease on it")
	}

	// A Host removed with nothing holding it is released by the reload
	// itself.
	if err := reg.Apply(ctx, []flintlock.Endpoint{endpoint("h1", h1.Addr()), endpoint("h3", h3.Addr())}); err != nil {
		t.Fatalf("Apply adding h3: %v", err)
	}
	unheld, err := reg.Get("h3")
	if err != nil {
		t.Fatalf("Get(h3): %v", err)
	}
	if _, err := unheld.ServerInfo(ctx); err != nil {
		t.Fatalf("ServerInfo on the newly added host: %v", err)
	}
	if err := reg.Apply(ctx, []flintlock.Endpoint{endpoint("h1", h1.Addr())}); err != nil {
		t.Fatalf("Apply removing h3: %v", err)
	}
	if _, err := unheld.ServerInfo(ctx); err == nil {
		t.Error("the connection to a host removed with no lease on it was kept open")
	}

	// Releasing a lease on a Host that is still in the Inventory releases
	// nothing: h1 was never removed.
	kept, releaseKept, err := reg.Lease("h1")
	if err != nil {
		t.Fatalf("Lease(h1): %v", err)
	}
	releaseKept()
	if _, err := kept.ServerInfo(ctx); err != nil {
		t.Errorf("releasing a lease closed the client of a host that is still in the inventory: %v", err)
	}
}

// TestApplyRedialsAChangedEndpoint checks the other half of a reload: an
// entry that keeps its name but changes its address is dialled again, so
// that the Registry does not go on using a connection to the machine the
// Host used to be.
func TestApplyRedialsAChangedEndpoint(t *testing.T) {
	t.Parallel()
	before := startHost(t, flintlock.FakeHostConfig{Name: "h1", Version: "before", ExecEnabled: true})
	after := startHost(t, flintlock.FakeHostConfig{Name: "h1", Version: "after", ExecEnabled: true})

	reg := newRegistry(t, []flintlock.Endpoint{
		{Name: "h1", Address: before.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// The Job that is running holds a lease, which is what keeps the
	// connection to the old process open across the reload (HO-014).
	client, release, err := reg.Lease("h1")
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	defer release()
	info, err := client.ServerInfo(ctx)
	if err != nil || info.Version != "before" {
		t.Fatalf("ServerInfo returned (%v, %v), want version before", info, err)
	}

	if err := reg.Apply(ctx, []flintlock.Endpoint{
		{Name: "h1", Address: after.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	updated, err := reg.Get("h1")
	if err != nil {
		t.Fatalf("Get after the reload: %v", err)
	}
	info, err = updated.ServerInfo(ctx)
	if err != nil || info.Version != "after" {
		t.Fatalf("after the reload ServerInfo returned (%v, %v), want version after", info, err)
	}
	// The Job that was already running still speaks to the old process.
	if info, err := client.ServerInfo(ctx); err != nil || info.Version != "before" {
		t.Errorf("the client held by a running job returned (%v, %v), want version before", info, err)
	}
}

// TestApplyKeepsUnchangedHosts checks that a reload that changes nothing
// costs nothing: the Registry hands back the same client, so no Host is
// reconnected because a Profile elsewhere in the file changed.
func TestApplyKeepsUnchangedHosts(t *testing.T) {
	t.Parallel()
	h1 := startHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	endpoints := []flintlock.Endpoint{
		{Name: "h1", Address: h1.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
	}
	reg := newRegistry(t, endpoints)

	first, err := reg.Get("h1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := reg.Apply(context.Background(), endpoints); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	second, err := reg.Get("h1")
	if err != nil {
		t.Fatalf("Get after the reload: %v", err)
	}
	if first != second {
		t.Error("an unchanged host was dialled again on reload")
	}
}

// TestRegistryIsSafeForConcurrentUse hammers a Registry from several
// goroutines while it is being reloaded, because the Scheduler probes Hosts
// while the configuration is reloaded under it.
func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	h1 := startHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	h2 := startHost(t, flintlock.FakeHostConfig{Name: "h2", ExecEnabled: true})
	first := []flintlock.Endpoint{
		{Name: "h1", Address: h1.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
	}
	second := []flintlock.Endpoint{
		{Name: "h1", Address: h1.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
		{Name: "h2", Address: h2.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
	}
	reg := newRegistry(t, first)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = reg.Names()
				if client, err := reg.Get("h1"); err == nil {
					_ = client.Name()
				}
				// h2 comes and goes with every other reload, so leasing it
				// races the reload that retires it and the release races
				// the close that retirement leads to.
				if client, release, err := reg.Lease("h2"); err == nil {
					_ = client.Name()
					release()
				}
				_, _ = reg.Endpoint("h2")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			endpoints := first
			if j%2 == 0 {
				endpoints = second
			}
			if err := reg.Apply(ctx, endpoints); err != nil {
				t.Errorf("Apply: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

// TestRegistryRejectsADuplicateInventory checks that two entries claiming
// the same Host name are refused rather than one silently winning.
func TestRegistryRejectsADuplicateInventory(t *testing.T) {
	t.Parallel()
	h1 := startHost(t, flintlock.FakeHostConfig{Name: "h1"})
	_, err := flintlock.NewRegistry(context.Background(), flintlock.NewDialer(), []flintlock.Endpoint{
		{Name: "h1", Address: h1.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
		{Name: "h1", Address: h1.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
	})
	if err == nil {
		t.Fatal("NewRegistry accepted two entries for one host name")
	}
}
