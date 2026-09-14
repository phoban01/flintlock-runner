package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	fleetinventory "github.com/phoban01/flintlock-runner/internal/fleet/inventory"
)

// TestLiveInventoryMergesDiscoveredHosts checks what a refresh hands the
// Runner: the configured Hosts that tag discovery still returns, and an
// entry for each instance it newly finds; that an unchanged refresh
// applies nothing; and that a SIGHUP reload keeps the discovered Hosts.
func TestLiveInventoryMergesDiscoveredHosts(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Inventory: config.Inventory{Hosts: []config.HostEntry{
			{Name: "host-a", Endpoint: "10.0.1.1:9090", VCPU: 8, MemoryMB: 16384},
			{Name: "host-gone", Endpoint: "10.0.1.9:9090", VCPU: 8, MemoryMB: 16384},
		}},
		Profiles: []config.Profile{{Name: "small"}},
		Fleet: &config.Fleet{
			GuestSubnet:   "172.31.0.0/16",
			InventoryPath: "/etc/flintlock-runner/inventory.yaml",
			Flintlockd:    config.Flintlockd{Port: 9090, Token: "fleet-token", TLS: config.ServerTLSFiles{CAFile: "/etc/flintlock-runner/fleet-ca.pem"}},
			HostReserve:   config.HostReserve{VCPU: 2, MemoryMB: 4096},
		},
	}
	live := newLiveInventory(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var applied [][]config.HostEntry
	apply := func(_ context.Context, profiles []config.Profile, hosts []config.HostEntry) error {
		if len(profiles) != 1 {
			t.Errorf("applied with %d Profiles", len(profiles))
		}
		applied = append(applied, hosts)
		return nil
	}
	ctx := context.Background()
	if err := live.attach(ctx, apply, cfg.Inventory.Hosts); err != nil || len(applied) != 0 {
		t.Fatalf("attach before any refresh applied %v (%v)", applied, err)
	}

	insts := []fleet.Instance{
		{ID: "host-a", PrivateIP: "10.0.1.1", Arch: config.ArchARM64},
		{ID: "i-new", PrivateIP: "10.0.2.7", Arch: config.ArchARM64, VCPU: 64, Tags: map[string]string{"tier": "general"}},
	}
	if err := live.refresh(ctx, insts); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 {
		t.Fatalf("a changing refresh applied %d times", len(applied))
	}
	got := applied[0]
	if names := hostNamesOf(got); !slices.Equal(names, []string{"host-a", "i-new"}) {
		t.Fatalf("applied Hosts = %v, want host-a kept, host-gone removed and i-new joined", names)
	}
	if got[0].VCPU != 8 || got[0].Endpoint != "10.0.1.1:9090" {
		t.Errorf("the configured entry changed: %+v", got[0])
	}
	n := got[1]
	if n.Endpoint != "10.0.2.7:9090" || n.VCPU != 62 || n.MemoryMB != 0 || n.Token != "fleet-token" ||
		n.TLS.CAFile != "/etc/flintlock-runner/fleet-ca.pem" || n.Labels["tier"] != "general" ||
		n.Labels[fleetinventory.LabelInstanceID] != "i-new" || n.Services.Buildkit == "" {
		t.Errorf("discovered entry = %+v", n)
	}

	if err := live.refresh(ctx, insts); err != nil || len(applied) != 1 {
		t.Errorf("an unchanged refresh applied again (%d, %v)", len(applied), err)
	}

	next := *cfg
	next.Profiles = []config.Profile{{Name: "big"}}
	if err := live.reload(ctx, &next); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || !slices.Equal(hostNamesOf(applied[1]), []string{"host-a", "i-new"}) {
		t.Errorf("a reload applied %v, want the discovered Hosts kept", applied)
	}
}
