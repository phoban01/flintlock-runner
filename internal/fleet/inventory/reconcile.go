package inventory

import (
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// LabelInstanceID is the label that ties an Inventory entry to the instance
// it was provisioned from. The Host name can come from a tag and the
// endpoint from an override, so neither is a stable key; the instance id is.
const LabelInstanceID = "fleet.flintlock-runner.dev/instance-id"

// InstanceID is the instance an Inventory entry was provisioned from: its
// LabelInstanceID label, or its name for an entry without one (the static
// provider uses the configured name as the instance id).
func InstanceID(h *config.HostEntry) string {
	if id := h.Labels[LabelInstanceID]; id != "" {
		return id
	}
	return h.Name
}

// Plan is what a re-run of `fleet provision` has to do to bring the
// Inventory in line with discovery.
type Plan struct {
	// New are discovered instances that no Inventory entry names; they are
	// the only ones provisioned (FL-063).
	New []fleet.Instance
	// Known are discovered instances the Inventory already lists.
	Known []fleet.Instance
	// Removed are Inventory entries whose instance discovery no longer
	// returns (FL-064).
	Removed []config.HostEntry
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//# When run again after new instances match the discovery filter,
//# the Fleet Controller SHALL provision only the new instances and SHALL merge
//# them into the existing Inventory.

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//# When run again after an instance in the Inventory no longer
//# exists, the Fleet Controller SHALL remove it from the Inventory and report
//# the removal.

// Diff compares the current Inventory with a discovery result. discovered
// has to be the complete result of a successful discovery: an instance
// missing from it is taken to no longer exist. Order follows discovered for
// New and Known and the Inventory for Removed.
func Diff(inv *fleet.Inventory, discovered []fleet.Instance) Plan {
	listed := map[string]bool{}
	if inv != nil {
		for i := range inv.Hosts {
			listed[InstanceID(&inv.Hosts[i])] = true
		}
	}
	present := map[string]bool{}
	var p Plan
	for _, inst := range discovered {
		present[inst.ID] = true
		if listed[inst.ID] {
			p.Known = append(p.Known, inst)
		} else {
			p.New = append(p.New, inst)
		}
	}
	if inv != nil {
		for _, h := range inv.Hosts {
			if !present[InstanceID(&h)] {
				p.Removed = append(p.Removed, h)
			}
		}
	}
	return p
}

// Merge returns the Inventory after a provisioning run: the existing
// entries in their order, without the ones plan removes, followed by added
// in the order given. An added entry for an instance the Inventory already
// lists replaces it in place. The input is not modified.
func Merge(inv *fleet.Inventory, plan Plan, added []config.HostEntry, now time.Time) *fleet.Inventory {
	removed := map[string]bool{}
	for i := range plan.Removed {
		removed[InstanceID(&plan.Removed[i])] = true
	}
	byID := map[string]config.HostEntry{}
	var order []string
	for _, h := range added {
		id := InstanceID(&h)
		if _, dup := byID[id]; !dup {
			order = append(order, id)
		}
		byID[id] = h
	}
	out := &fleet.Inventory{GeneratedAt: now}
	if inv != nil {
		for _, h := range inv.Hosts {
			id := InstanceID(&h)
			if removed[id] {
				continue
			}
			if repl, ok := byID[id]; ok {
				h = repl
				delete(byID, id)
			}
			out.Hosts = append(out.Hosts, h)
		}
	}
	for _, id := range order {
		if h, ok := byID[id]; ok {
			out.Hosts = append(out.Hosts, h)
		}
	}
	return out
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//# The Fleet Controller SHALL compute each Host's capacity as the
//# instance's vCPU and memory minus the configured Host reserve.

// Capacity is the capacity an instance offers MicroVMs: its vCPU and memory
// minus the Host reserve. An instance the reserve leaves nothing of is an
// error rather than a Host with zero or negative capacity.
func Capacity(inst fleet.Instance, reserve config.HostReserve) (vcpu, memoryMB int, err error) {
	vcpu = inst.VCPU - reserve.VCPU
	memoryMB = inst.MemoryMB - reserve.MemoryMB
	if vcpu <= 0 || memoryMB <= 0 {
		return 0, 0, fmt.Errorf("instance %s has %d vCPU and %d MB; the host reserve of %d vCPU and %d MB leaves no capacity for MicroVMs",
			inst.ID, inst.VCPU, inst.MemoryMB, reserve.VCPU, reserve.MemoryMB)
	}
	return vcpu, memoryMB, nil
}

// Complete finishes the Inventory entry the Provisioner produced for inst:
// it sets the capacity from the instance and the Host reserve (FL-061),
// labels the entry with its instance id, and fills the name, architecture
// and endpoint when the Provisioner left them empty (the instance id, the
// instance architecture, and the private address or its override on the
// flintlockd port). Everything else the Provisioner recorded, the Host
// Service addresses and installed versions included, is kept.
func Complete(entry config.HostEntry, inst fleet.Instance, f *config.Fleet) (config.HostEntry, error) {
	var reserve config.HostReserve
	if f != nil {
		reserve = f.HostReserve
	}
	vcpu, mem, err := Capacity(inst, reserve)
	if err != nil {
		return entry, err
	}
	entry.VCPU, entry.MemoryMB = vcpu, mem
	if entry.Name == "" {
		entry.Name = inst.ID
	}
	if entry.Arch == "" {
		entry.Arch = inst.Arch
	}
	if entry.Endpoint == "" && f != nil {
		addr := inst.PrivateIP
		if o := f.EndpointOverrides[inst.ID]; o != "" {
			addr = o
		}
		entry.Endpoint = net.JoinHostPort(addr, strconv.Itoa(f.Flintlockd.Port))
	}
	labels := maps.Clone(entry.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelInstanceID] = inst.ID
	entry.Labels = labels
	return entry, nil
}

// InstanceOf reconstructs the instance an Inventory entry was provisioned
// from, for the commands that run scripts on Hosts already in the Inventory
// (drain and teardown): the instance id, the address of the endpoint and the
// architecture.
func InstanceOf(h *config.HostEntry) fleet.Instance {
	addr, _, err := net.SplitHostPort(h.Endpoint)
	if err != nil {
		addr = h.Endpoint
	}
	return fleet.Instance{
		ID:        InstanceID(h),
		Arch:      h.Arch,
		PrivateIP: addr,
		State:     "running",
		Tags:      maps.Clone(h.Labels),
	}
}

// RunnerConfig is the Runner configuration generated from the Fleet
// Controller's input (FL-062): the input as given, with the Inventory
// section replaced by a reference to the Inventory file and without the
// Fleet section, which only the fleet subcommands read. The Pool Manager
// section and the Profiles are carried over unchanged.
func RunnerConfig(in *config.Config, inventoryPath string) *config.Config {
	out := *in
	out.Inventory = config.Inventory{File: inventoryPath}
	out.Fleet = nil
	out.Profiles = slices.Clone(in.Profiles)
	return &out
}
