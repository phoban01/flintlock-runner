package main

import (
	"context"
	"log/slog"
	"maps"
	"reflect"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/discovery"
	fleetinventory "github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
)

// firstRefreshWait bounds how long `run` waits at startup for the first
// Inventory refresh, so that the Scheduler starts with the Hosts tag
// discovery finds. A refresh that fails or takes longer is retried at the
// configured interval while the Runner runs on its configured Inventory.
const firstRefreshWait = 15 * time.Second

// applyInventory hands the Runner a new Inventory: the executor's view and
// the Scheduler's, which reconciles the Host Registry and redeclares the
// Pools. It is what a SIGHUP reload does (CF-007).
type applyInventory func(ctx context.Context, profiles []config.Profile, hosts []config.HostEntry) error

// liveInventory is the Runner's Inventory. It is the configured one, which
// SIGHUP reloads, and in launch template mode the instances tag discovery
// finds are merged into it at every refresh (FL-092): a discovered
// instance the configuration does not list joins with an entry built the
// way `fleet provision` builds one, and a listed Host whose instance
// discovery no longer returns leaves, as `fleet provision` merges its
// Inventory (FL-063, FL-064). Both paths end in the same applyInventory.
type liveInventory struct {
	log *slog.Logger

	mu sync.Mutex
	// cfg is the current configuration: its Profiles, its Inventory and
	// its fleet section, from which discovered Hosts' entries are built.
	cfg *config.Config
	// discovered is the last refresh's instances, once refreshed is set.
	discovered []fleet.Instance
	refreshed  bool
	firstOnce  sync.Once
	first      chan struct{}
	// apply is nil until the Runner is built; applied is the Inventory it
	// was last given.
	apply   applyInventory
	applied []config.HostEntry
}

func newLiveInventory(cfg *config.Config, log *slog.Logger) *liveInventory {
	return &liveInventory{cfg: cfg, log: log, first: make(chan struct{})}
}

// hosts is the Inventory the Runner should have now.
func (l *liveInventory) hosts() []config.HostEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hostsLocked()
}

func (l *liveInventory) hostsLocked() []config.HostEntry {
	configured := &fleet.Inventory{Hosts: l.cfg.Inventory.Hosts}
	if !l.refreshed {
		return configured.Hosts
	}
	plan := fleetinventory.Diff(configured, l.discovered)
	var added []config.HostEntry
	for _, inst := range plan.New {
		entry, err := discoveredEntry(l.cfg, inst)
		if err != nil {
			l.log.Warn("inventory refresh: leaving out a discovered instance", "instance", inst.ID, "error", err)
			continue
		}
		added = append(added, entry)
	}
	return fleetinventory.Merge(configured, plan, added, time.Time{}).Hosts
}

// discoveredEntry is the Inventory entry of an instance that provisioned
// itself from the launch template's user-data: named by its instance id
// (FL-052) and labelled with it and its tags, at its flintlockd endpoint,
// with the Host Service addresses every Host serves and the fleet's token
// and TLS settings, as `fleet provision` records a Host. Its capacity is
// what EC2 reports less the Host reserve: the vCPU, and no memory, which
// EC2 does not report and the Host measures only for the Fleet Controller
// (FL-061). The Runner does not place by Host capacity; the Pool Manager
// does.
func discoveredEntry(cfg *config.Config, inst fleet.Instance) (config.HostEntry, error) {
	svc, err := scripts.ServiceAddresses(cfg.HostServices, cfg.Fleet.GuestSubnet)
	if err != nil {
		return config.HostEntry{}, err
	}
	labels := maps.Clone(inst.Tags)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[fleetinventory.LabelInstanceID] = inst.ID
	reserve := cfg.Fleet.HostReserve
	entry := config.HostEntry{
		Name:     inst.ID,
		Endpoint: discovery.Endpoint(inst, cfg.Fleet),
		Arch:     inst.Arch,
		VCPU:     max(inst.VCPU-reserve.VCPU, 0),
		MemoryMB: max(inst.MemoryMB-reserve.MemoryMB, 0),
		Labels:   labels,
		Services: svc,
	}
	return withFleetAccess(cfg, entry), nil
}

// refresh is the Refresher's OnChange: insts are the supported instances
// carrying the discovery tag.
func (l *liveInventory) refresh(ctx context.Context, insts []fleet.Instance) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.discovered, l.refreshed = insts, true
	l.firstOnce.Do(func() { close(l.first) })
	return l.applyLocked(ctx, false)
}

// reload is the SIGHUP subscriber: the new configuration's Profiles and
// Inventory, with the last refresh's instances merged in again.
func (l *liveInventory) reload(ctx context.Context, next *config.Config) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = next
	return l.applyLocked(ctx, true)
}

// awaitFirst waits for the first refresh, for at most d.
func (l *liveInventory) awaitFirst(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.first:
	case <-t.C:
		l.log.Warn("inventory refresh: no answer from tag discovery yet; starting with the configured inventory", "waited", d)
	case <-ctx.Done():
	}
}

// attach starts applying changes to the Runner, which was built with the
// Inventory built. A refresh that arrived since is applied now.
func (l *liveInventory) attach(ctx context.Context, apply applyInventory, built []config.HostEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.apply, l.applied = apply, built
	return l.applyLocked(ctx, false)
}

// applyLocked hands the Runner the current Inventory. A refresh that
// changes nothing is not applied; a reload always is, because its Profiles
// may have changed.
func (l *liveInventory) applyLocked(ctx context.Context, always bool) error {
	if l.apply == nil {
		return nil
	}
	hosts := l.hostsLocked()
	if !always && reflect.DeepEqual(hosts, l.applied) {
		return nil
	}
	if err := l.apply(ctx, l.cfg.Profiles, hosts); err != nil {
		return err
	}
	if !always {
		l.log.Info("inventory refreshed from tag discovery", "hosts", hostNamesOf(hosts))
	}
	l.applied = hosts
	return nil
}

func hostNamesOf(hosts []config.HostEntry) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name)
	}
	return out
}
