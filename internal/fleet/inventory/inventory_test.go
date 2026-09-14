package inventory

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

var testNow = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

// fleetInput is a Fleet Controller input: a whole Runner configuration with
// a fleet section, an inline Host so that it validates, and a Profile.
const fleetInput = `gitlab:
  url: https://gitlab.example.com
  token: glrt-fleet
pool_manager:
  endpoint: 10.0.0.5:9091
inventory:
  hosts:
    - name: host-a
      endpoint: 10.0.1.10:9090
      arch: arm64
      vcpu: 62
      memory_mb: 253952
profiles:
  - name: small
    arch: arm64
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    pool:
      size: 2
  - name: large
    arch: arm64
    vcpu: 8
    memory_mb: 16384
    images: [large]
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    pool:
      size: 1
fleet:
  region: eu-west-1
  discovery:
    tag_key: flintlock
    tag_value: "true"
  versions:
    flintlock: v0.9.0
    firecracker: v1.10.0
    containerd: v1.7.0
    pool_manager: v0.1.0
  thin_pool_device: /dev/nvme1n1
  host_reserve:
    vcpu: 2
    memory_mb: 4096
`

func parseInput(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(fleetInput), t.TempDir(), config.WithEnv(func(string) (string, bool) { return "", false }))
	if err != nil {
		t.Fatalf("parse fleet input: %v", err)
	}
	return cfg
}

// fullEntry is an Inventory entry with every field FL-060 lists set.
func fullEntry(name, addr string) config.HostEntry {
	return config.HostEntry{
		Name:     name,
		Endpoint: addr + ":9090",
		Arch:     config.ArchARM64,
		VCPU:     62,
		MemoryMB: 253952,
		Labels:   map[string]string{"zone": "a", LabelInstanceID: "i-" + name},
		TLS:      config.ClientTLS{CAFile: "/etc/flintlock-runner/tls/ca.pem"},
		Services: config.HostServiceAddresses{
			Buildkit:       "tcp://172.31.0.1:1234",
			GoProxy:        "http://172.31.0.1:3000",
			RegistryMirror: "http://172.31.0.1:5000",
		},
		Versions: config.InstalledVersions{
			Flintlock:   "v0.9.0",
			Firecracker: "v1.10.0",
			Containerd:  "v1.7.0",
		},
	}
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//= type=test
//# When provisioning completes, the Fleet Controller SHALL write an
//# Inventory file listing every Host with its name, `flintlockd` endpoint,
//# architecture, vCPU and memory capacity, labels, Host Service addresses and
//# installed versions.

func TestSaveWritesEveryHostWithEveryField(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	inv := &fleet.Inventory{
		Hosts:       []config.HostEntry{fullEntry("host-a", "10.0.1.10"), fullEntry("host-b", "10.0.1.11")},
		GeneratedAt: testNow,
	}
	if err := (Store{}).Save(context.Background(), path, inv); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The Runner reads the file with config.LoadInventoryFile's schema; what
	// it reads has to be what was saved, field for field.
	got, err := config.LoadInventoryFile(path)
	if err != nil {
		t.Fatalf("LoadInventoryFile: %v", err)
	}
	if !reflect.DeepEqual(got.Hosts, inv.Hosts) {
		t.Errorf("the Runner reads back\n%#v\nwant\n%#v", got.Hosts, inv.Hosts)
	}
	if !got.GeneratedAt.Equal(testNow) {
		t.Errorf("generated_at = %v, want %v", got.GeneratedAt, testNow)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("inventory mode = %v, want 0600 because it can carry Host tokens", fi.Mode().Perm())
	}

	loaded, err := (Store{}).Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(loaded.Hosts, inv.Hosts) {
		t.Errorf("Store.Load = %#v, want %#v", loaded.Hosts, inv.Hosts)
	}
}

// TestSaveKeepsAGroupReadGrant rewrites an Inventory whose group was given
// read access, as `fleet up --install-runner` gives the Runner's user, and
// checks that the rewrite keeps the group and its read permission but drops
// anything wider.
func TestSaveKeepsAGroupReadGrant(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	inv := &fleet.Inventory{Hosts: []config.HostEntry{fullEntry("host-a", "10.0.1.10")}}
	if err := (Store{}).Save(context.Background(), path, inv); err != nil {
		t.Fatal(err)
	}
	// A supplementary group of this process, when it has one, stands in
	// for the Runner's group.
	gid := os.Getgid()
	if groups, err := os.Getgroups(); err == nil {
		for _, g := range groups {
			if g != gid {
				gid = g
				break
			}
		}
	}
	if err := os.Chown(path, -1, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Store{}).Save(context.Background(), path, inv); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("rewritten mode = %v, want 0640: the group read kept, the world read dropped", fi.Mode().Perm())
	}
	if got := int(fi.Sys().(*syscall.Stat_t).Gid); got != gid {
		t.Errorf("rewritten group = %d, want %d", got, gid)
	}

	// Without a group grant the file stays owner-only.
	if err := os.Chmod(path, 0o604); err != nil {
		t.Fatal(err)
	}
	if err := (Store{}).Save(context.Background(), path, inv); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("rewritten mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

func TestLoadMissingFileIsEmptyInventory(t *testing.T) {
	t.Parallel()
	inv, err := (Store{}).Load(context.Background(), filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(inv.Hosts) != 0 {
		t.Errorf("hosts = %v, want none", inv.Hosts)
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(path, []byte("hosts: {not: a list}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{}).Load(context.Background(), path); err == nil {
		t.Fatal("Load accepted a corrupt Inventory; a re-run would then discard every Host")
	}
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//= type=test
//# The Fleet Controller SHALL generate a Runner configuration file
//# that references the Inventory, names the Pool Manager endpoint and carries
//# the Profiles from its input.

func TestSaveRunnerConfigReferencesInventoryAndCarriesProfiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	invPath := filepath.Join(dir, "inventory.yaml")
	cfgPath := filepath.Join(dir, "config.yaml")
	in := parseInput(t)

	inv := &fleet.Inventory{Hosts: []config.HostEntry{fullEntry("host-a", "10.0.1.10")}, GeneratedAt: testNow}
	if err := (Store{}).Save(context.Background(), invPath, inv); err != nil {
		t.Fatal(err)
	}
	if err := (Store{}).SaveRunnerConfig(context.Background(), cfgPath, RunnerConfig(in, invPath)); err != nil {
		t.Fatalf("SaveRunnerConfig: %v", err)
	}

	// The written file is what the Runner will start from: it has to load
	// and validate on its own, with nothing from the environment.
	got, err := config.Load(cfgPath, config.WithEnv(func(string) (string, bool) { return "", false }))
	if err != nil {
		t.Fatalf("the generated Runner configuration does not load: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "file: "+invPath) {
		t.Errorf("the Runner configuration does not reference the Inventory file %s:\n%s", invPath, raw)
	}
	if !reflect.DeepEqual(got.Inventory.Hosts, inv.Hosts) {
		t.Errorf("hosts resolved through the reference = %#v, want %#v", got.Inventory.Hosts, inv.Hosts)
	}
	if got.PoolManager.Endpoint != in.PoolManager.Endpoint {
		t.Errorf("pool_manager.endpoint = %q, want %q", got.PoolManager.Endpoint, in.PoolManager.Endpoint)
	}
	if !reflect.DeepEqual(got.Profiles, in.Profiles) {
		t.Errorf("profiles = %#v\nwant %#v", got.Profiles, in.Profiles)
	}
	if got.Fleet == nil || got.Fleet.Region != in.Fleet.Region {
		t.Errorf("the Runner configuration dropped the fleet section; the next fleet run reads it: %#v", got.Fleet)
	}
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600 because it carries the runner token", fi.Mode().Perm())
	}
}

func TestSaveRunnerConfigRefusesAnInvalidConfiguration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	in := parseInput(t)
	// The Inventory it references does not exist, so the Runner could not
	// start from it.
	err := (Store{}).SaveRunnerConfig(context.Background(), cfgPath, RunnerConfig(in, filepath.Join(dir, "missing.yaml")))
	if err == nil {
		t.Fatal("SaveRunnerConfig wrote a configuration the Runner cannot load")
	}
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		t.Error("an invalid Runner configuration was written")
	}
}

func instance(id string) fleet.Instance {
	return fleet.Instance{ID: id, Type: "c7g.metal", Arch: config.ArchARM64, PrivateIP: "10.0.1." + id[len(id)-1:], State: "running", VCPU: 64, MemoryMB: 131072}
}

func entryFor(id string) config.HostEntry {
	e := fullEntry("host-"+id, "10.0.1.1")
	e.Labels = map[string]string{LabelInstanceID: id}
	return e
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//= type=test
//# When run again after new instances match the discovery filter,
//# the Fleet Controller SHALL provision only the new instances and SHALL merge
//# them into the existing Inventory.

func TestDiffSelectsOnlyNewInstancesAndMergeKeepsExisting(t *testing.T) {
	t.Parallel()
	a, b := entryFor("i-1"), entryFor("i-2")
	a.Endpoint, b.Endpoint = "10.0.1.1:9090", "10.0.1.2:9090"
	existing := &fleet.Inventory{Hosts: []config.HostEntry{a, b}}

	plan := Diff(existing, []fleet.Instance{instance("i-1"), instance("i-2"), instance("i-3")})
	if got := ids(plan.New); !reflect.DeepEqual(got, []string{"i-3"}) {
		t.Fatalf("instances to provision = %v, want only the new one [i-3]", got)
	}
	if got := ids(plan.Known); !reflect.DeepEqual(got, []string{"i-1", "i-2"}) {
		t.Errorf("known = %v, want [i-1 i-2]", got)
	}
	if len(plan.Removed) != 0 {
		t.Errorf("removed = %v, want none", plan.Removed)
	}

	c := entryFor("i-3")
	c.Endpoint = "10.0.1.3:9090"
	merged := Merge(existing, plan, []config.HostEntry{c}, testNow)
	if !reflect.DeepEqual(merged.Hosts, []config.HostEntry{a, b, c}) {
		t.Errorf("merged = %#v, want the existing entries unchanged followed by the new one", merged.Hosts)
	}
	if !merged.GeneratedAt.Equal(testNow) {
		t.Errorf("generated_at = %v", merged.GeneratedAt)
	}
	if len(existing.Hosts) != 2 {
		t.Error("Merge modified its input")
	}
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//= type=test
//# When run again after an instance in the Inventory no longer
//# exists, the Fleet Controller SHALL remove it from the Inventory and report
//# the removal.

func TestDiffReportsVanishedInstancesAndMergeDropsThem(t *testing.T) {
	t.Parallel()
	a, b := entryFor("i-1"), entryFor("i-2")
	a.Endpoint, b.Endpoint = "10.0.1.1:9090", "10.0.1.2:9090"
	existing := &fleet.Inventory{Hosts: []config.HostEntry{a, b}}

	plan := Diff(existing, []fleet.Instance{instance("i-2")})
	if len(plan.Removed) != 1 || plan.Removed[0].Name != a.Name {
		t.Fatalf("removed = %v, want [%s]", plan.Removed, a.Name)
	}
	if len(plan.New) != 0 {
		t.Errorf("new = %v, want none", ids(plan.New))
	}
	merged := Merge(existing, plan, nil, testNow)
	if !reflect.DeepEqual(merged.Hosts, []config.HostEntry{b}) {
		t.Errorf("merged = %#v, want only %s", merged.Hosts, b.Name)
	}
}

func TestInstanceIDFallsBackToName(t *testing.T) {
	t.Parallel()
	// A static Host is discovered under its configured name.
	existing := &fleet.Inventory{Hosts: []config.HostEntry{{Name: "box-1", Endpoint: "192.0.2.1:9090"}}}
	plan := Diff(existing, []fleet.Instance{{ID: "box-1"}})
	if len(plan.New) != 0 || len(plan.Removed) != 0 {
		t.Errorf("plan = %+v, want box-1 recognised as known", plan)
	}
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//= type=test
//# The Fleet Controller SHALL compute each Host's capacity as the
//# instance's vCPU and memory minus the configured Host reserve.

func TestCompleteSetsCapacityFromInstanceMinusReserve(t *testing.T) {
	t.Parallel()
	f := &config.Fleet{HostReserve: config.HostReserve{VCPU: 2, MemoryMB: 4096}, Flintlockd: config.Flintlockd{Port: 9090}}
	inst := fleet.Instance{ID: "i-9", Arch: config.ArchAMD64, PrivateIP: "10.0.2.9", VCPU: 96, MemoryMB: 196608}

	// The Provisioner's entry carries the raw figures; Complete replaces
	// them, whatever they were.
	got, err := Complete(config.HostEntry{VCPU: 96, MemoryMB: 196608, Versions: config.InstalledVersions{Flintlock: "v0.9.0"}}, inst, f)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.VCPU != 94 || got.MemoryMB != 192512 {
		t.Errorf("capacity = %d vCPU %d MB, want 94 vCPU 192512 MB", got.VCPU, got.MemoryMB)
	}
	if got.Name != "i-9" || got.Arch != config.ArchAMD64 || got.Endpoint != "10.0.2.9:9090" {
		t.Errorf("defaults = %q %q %q", got.Name, got.Arch, got.Endpoint)
	}
	if got.Labels[LabelInstanceID] != "i-9" {
		t.Errorf("labels = %v, want the instance id", got.Labels)
	}
	if got.Versions.Flintlock != "v0.9.0" {
		t.Error("Complete dropped what the Provisioner recorded")
	}

	f.EndpointOverrides = map[string]string{"i-9": "host-9.internal"}
	got, err = Complete(config.HostEntry{}, inst, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "host-9.internal:9090" {
		t.Errorf("endpoint with override = %q", got.Endpoint)
	}

	if _, err := Complete(config.HostEntry{}, fleet.Instance{ID: "tiny", VCPU: 2, MemoryMB: 8192}, f); err == nil {
		t.Error("an instance the reserve leaves no vCPU for was accepted")
	}
}

func TestInstanceOfUsesEndpointAddress(t *testing.T) {
	t.Parallel()
	h := entryFor("i-7")
	h.Endpoint = "10.0.1.7:9090"
	inst := InstanceOf(&h)
	if inst.ID != "i-7" || inst.PrivateIP != "10.0.1.7" || inst.Arch != config.ArchARM64 {
		t.Errorf("InstanceOf = %+v", inst)
	}
}

func TestRemoveIgnoresMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := Remove(path); err != nil {
		t.Fatalf("Remove of a missing file: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file still there")
	}
}

func ids(insts []fleet.Instance) []string {
	var out []string
	for _, i := range insts {
		out = append(out, i.ID)
	}
	return out
}
