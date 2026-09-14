package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/drain"
	"github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/fleet/opstest"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// fleetFixture is a fleet input configuration in a temporary directory,
// with the fakes every seam hands out.
type fleetFixture struct {
	dir         string
	configPath  string
	invPath     string
	runnerPath  string
	discovery   *opstest.Discovery
	provisioner *opstest.Provisioner
	remote      *opstest.Remote
	scripts     *opstest.Scripts
	ec2         *opstest.EC2
	stack       *opstest.Stack
}

// fleetConfigYAML renders a fleet input. pmEndpoint is the Pool Manager;
// insecure starts flintlockd without TLS.
func fleetConfigYAML(t *testing.T, dir, pmEndpoint string, insecure bool) string {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`gitlab:
  url: https://gitlab.example.com
  token: glrt-fleet-test
  name: %[5]s
pool_manager:
  endpoint: %[2]s
  tls:
    insecure: true
scheduler:
  namespace: %[6]s
inventory:
  file: %[1]s/inventory.yaml
profiles:
  - name: small
    arch: %[4]s
    shell: %[7]s
    kernel:
      image: ghcr.io/example/kernel:test
    rootfs: ghcr.io/example/rootfs:test
    pool:
      size: 2
fleet:
  discovery:
    static:
      - name: i-1
        address: 10.0.1.1
        arch: %[4]s
        vcpu: 64
        memory_mb: 131072
  remote:
    mode: ssh
    ssh:
      user: ubuntu
      key_file: /home/ubuntu/.ssh/id_ed25519
  versions:
    flintlock: v0.9.0
    firecracker: v1.10.0
    containerd: v1.7.0
    pool_manager: v0.1.0
  thin_pool_device: /dev/nvme1n1
  flintlockd:
    insecure: %[3]t
  host_reserve:
    vcpu: 2
    memory_mb: 4096
  inventory_path: %[1]s/inventory.yaml
  runner_config_path: %[1]s/runner.yaml
  drain_timeout: 5s
  verification_timeout: 20s
`, dir, pmEndpoint, insecure, runtime.GOARCH, opstest.RunnerName, opstest.Namespace, shell)
}

func newFleetFixture(t *testing.T, pmEndpoint string, insecure bool) *fleetFixture {
	t.Helper()
	dir := t.TempDir()
	f := &fleetFixture{
		dir:         dir,
		configPath:  filepath.Join(dir, "fleet.yaml"),
		invPath:     filepath.Join(dir, "inventory.yaml"),
		runnerPath:  filepath.Join(dir, "runner.yaml"),
		discovery:   &opstest.Discovery{},
		provisioner: &opstest.Provisioner{},
		remote:      &opstest.Remote{},
		scripts:     &opstest.Scripts{},
		ec2:         &opstest.EC2{},
	}
	if err := os.WriteFile(f.configPath, []byte(fleetConfigYAML(t, dir, pmEndpoint, insecure)), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fleetFixture) seams() fleetSeams {
	s := productionSeams()
	s.Discovery = func(*config.Config) (fleet.Discovery, error) { return f.discovery, nil }
	s.Remote = func(*config.Config, io.Writer) (fleet.Remote, error) { return f.remote, nil }
	s.Scripts = func(*config.Config) (fleet.Scripts, error) { return f.scripts, nil }
	s.Provisioner = func(*config.Config, fleet.Deps) (fleet.Provisioner, error) { return f.provisioner, nil }
	s.EC2 = func(*config.Config) (fleet.EC2, error) { return f.ec2, nil }
	if f.stack != nil {
		s.Leases = func(poolmgr.Client) drain.LeaseReporter { return drain.InspectorLeases{Inspector: f.stack.PoolManager} }
	}
	s.Now = func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }
	return s
}

// run runs one fleet subcommand and returns its output and error.
func (f *fleetFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	app := newApp()
	for i := range app.Commands {
		if app.Commands[i].Name == "fleet" {
			app.Commands[i].Subcommands = fleetCommands(f.seams())
		}
	}
	var out syncBuffer
	app.Writer = &out
	app.ErrWriter = &out
	err := app.Run(append([]string{"flintlock-runner", "--config", f.configPath, "fleet"}, args...))
	return out.String(), err
}

func metal(id, ip string) fleet.Instance {
	return fleet.Instance{ID: id, Type: "c7g.metal", Arch: config.Architecture(runtime.GOARCH), PrivateIP: ip, State: "running", VCPU: 64, MemoryMB: 131072}
}

func exitCode(err error) int {
	var ec cli.ExitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func hostNames(hosts []config.HostEntry) []string {
	var out []string
	for _, h := range hosts {
		out = append(out, h.Name)
	}
	return out
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//= type=test
//# When run again after new instances match the discovery filter,
//# the Fleet Controller SHALL provision only the new instances and SHALL merge
//# them into the existing Inventory.

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//= type=test
//# When run again after an instance in the Inventory no longer
//# exists, the Fleet Controller SHALL remove it from the Inventory and report
//# the removal.

func TestFleetProvisionMergesNewAndRemovesVanishedInstances(t *testing.T) {
	t.Parallel()
	f := newFleetFixture(t, "127.0.0.1:9440", false)

	f.discovery.SetInstances(metal("i-1", "10.0.1.1"), metal("i-2", "10.0.1.2"), fleet.Instance{ID: "i-small", Type: "c7g.large"})
	out, err := f.run(t, "provision")
	if err != nil {
		t.Fatalf("first provision: %v\n%s", err, out)
	}
	if got := f.provisioner.Seen(); !slices.Equal(sortedCopy(got), []string{"i-1", "i-2"}) {
		t.Errorf("first run provisioned %v, want i-1 and i-2 and not the non-metal instance", got)
	}
	inv, err := config.LoadInventoryFile(f.invPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostNames(inv.Hosts); !slices.Equal(got, []string{"i-1", "i-2"}) {
		t.Fatalf("Inventory = %v, want [i-1 i-2]", got)
	}
	first := inv.Hosts[0]
	if first.VCPU != 62 || first.MemoryMB != 131072-4096 || first.Endpoint != "10.0.1.1:9090" {
		t.Errorf("entry = %+v, want capacity minus the reserve at the private address", first)
	}
	// TLS was generated (SE-023) and the entry trusts the generated CA.
	if first.TLS.CAFile != filepath.Join(f.dir, "tls", "ca.pem") {
		t.Errorf("entry TLS = %+v, want the generated CA", first.TLS)
	}
	if fi, err := os.Stat(filepath.Join(f.dir, "tls", "ca-key.pem")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("generated CA key: %v %v", fi, err)
	}
	// The Runner configuration was written and loads on its own.
	runner, err := config.Load(f.runnerPath, config.WithEnv(func(string) (string, bool) { return "", false }))
	if err != nil {
		t.Fatalf("the generated Runner configuration does not load: %v", err)
	}
	if runner.Inventory.File != f.invPath || runner.PoolManager.Endpoint != "127.0.0.1:9440" || runner.Profiles[0].Name != "small" {
		t.Errorf("Runner configuration = inventory %q, pool manager %q, profiles %v", runner.Inventory.File, runner.PoolManager.Endpoint, runner.Profiles)
	}

	// Second run: i-2 has gone, i-3 is new.
	f.discovery.SetInstances(metal("i-1", "10.0.1.1"), metal("i-3", "10.0.1.3"))
	out, err = f.run(t, "provision")
	if err != nil {
		t.Fatalf("second provision: %v\n%s", err, out)
	}
	if got := f.provisioner.Seen()[2:]; !slices.Equal(got, []string{"i-3"}) {
		t.Errorf("second run provisioned %v, want only the new instance i-3", got)
	}
	if !strings.Contains(out, "removed i-2 (instance i-2) from the Inventory") {
		t.Errorf("the removal of i-2 was not reported:\n%s", out)
	}
	inv, err = config.LoadInventoryFile(f.invPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostNames(inv.Hosts); !slices.Equal(got, []string{"i-1", "i-3"}) {
		t.Errorf("Inventory = %v, want i-1 kept, i-2 removed and i-3 merged in", got)
	}
	if inv.Hosts[0].Endpoint != first.Endpoint || inv.Hosts[0].VCPU != first.VCPU {
		t.Errorf("the existing entry changed: %+v", inv.Hosts[0])
	}
}

func TestFleetProvisionReportsFailuresAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	f := newFleetFixture(t, "127.0.0.1:9440", true)
	f.provisioner.Fail = map[string]bool{"i-2": true}
	f.discovery.SetInstances(metal("i-1", "10.0.1.1"), metal("i-2", "10.0.1.2"))
	out, err := f.run(t, "provision")
	if exitCode(err) != exitFleetFailure || !strings.Contains(err.Error(), "i-2: step flintlock") {
		t.Fatalf("provision = %v, want a non-zero exit naming i-2 and its step\n%s", err, out)
	}
	inv, lerr := config.LoadInventoryFile(f.invPath)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got := hostNames(inv.Hosts); !slices.Equal(got, []string{"i-1"}) {
		t.Errorf("Inventory = %v, want the Host that did provision", got)
	}
	if !inv.Hosts[0].TLS.Insecure {
		t.Errorf("insecure fleet entry TLS = %+v, want insecure", inv.Hosts[0].TLS)
	}
}

func TestFleetProvisionLeavesInventoryWhenDiscoveryFails(t *testing.T) {
	t.Parallel()
	f := newFleetFixture(t, "127.0.0.1:9440", true)
	f.discovery.SetInstances(metal("i-1", "10.0.1.1"))
	if out, err := f.run(t, "provision"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	f.discovery.SetErr(errors.New("DescribeInstances: throttled"))
	if _, err := f.run(t, "provision"); exitCode(err) != exitFleetFailure {
		t.Fatalf("provision with failing discovery = %v, want a failure", err)
	}
	inv, err := config.LoadInventoryFile(f.invPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 1 {
		t.Errorf("a failed discovery removed Hosts: %v", hostNames(inv.Hosts))
	}
}

func TestFleetSeamsNameTheirWorkPackage(t *testing.T) {
	t.Parallel()
	f := newFleetFixture(t, "127.0.0.1:9440", true)
	app := newApp()
	app.Writer, app.ErrWriter = io.Discard, io.Discard
	err := app.Run([]string{"flintlock-runner", "--config", f.configPath, "fleet", "provision"})
	if exitCode(err) != exitNotImplemented || !strings.Contains(err.Error(), "fleet-access") {
		t.Errorf("provision without the access work package = %v, want exit %d naming it", err, exitNotImplemented)
	}
}

// stackFixture is a fleet fixture whose Inventory is a running opstest
// Stack's.
func stackFixture(t *testing.T, opts opstest.Options) *fleetFixture {
	t.Helper()
	s := opstest.Start(t, opts)
	f := newFleetFixture(t, s.PoolManager.Addr(), true)
	f.stack = s
	if err := (inventory.Store{}).Save(context.Background(), f.invPath, &fleet.Inventory{Hosts: s.Inventory}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFleetVerifyPassesOnAHealthyFleet(t *testing.T) {
	t.Parallel()
	f := stackFixture(t, opstest.Options{Hosts: 2})
	out, err := f.run(t, "verify", "--declare")
	if err != nil {
		t.Fatalf("fleet verify: %v\n%s", err, out)
	}
	for _, want := range []string{"host-1: claim to ready", "host-2: claim to ready", "verification passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

//= docs/requirements/06-fleet.md#verification
//= type=test
//# If verification fails on any Host or service, then the Fleet
//# Controller SHALL exit with a non-zero status naming each failure and the
//# step that failed.

func TestFleetVerifyExitsNonZeroNamingEachFailure(t *testing.T) {
	t.Parallel()
	f := stackFixture(t, opstest.Options{Hosts: 2, ExecDisabled: map[string]bool{"host-2": true}})
	out, err := f.run(t, "verify", "--declare", "--timeout", "2s")
	if exitCode(err) != exitFleetFailure {
		t.Fatalf("fleet verify = %v (exit %d), want exit %d\n%s", err, exitCode(err), exitFleetFailure, out)
	}
	for _, want := range []string{"host-2: step exec_service", "host-2: step exercise"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("exit message %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "host-1:") {
		t.Errorf("exit message names the healthy host-1: %v", err)
	}
}

func TestFleetDrainRemovesHostFromPools(t *testing.T) {
	t.Parallel()
	f := stackFixture(t, opstest.Options{Hosts: 2})
	f.stack.Declare(t)
	out, err := f.run(t, "drain", "--stop", "host-1")
	if err != nil {
		t.Fatalf("fleet drain: %v\n%s", err, out)
	}
	pool, err := f.stack.Client.GetPool(context.Background(), f.stack.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pool.Spec.FlintlockHosts, "host-1") {
		t.Errorf("flintlock_hosts = %v after draining host-1", pool.Spec.FlintlockHosts)
	}
	calls := f.remote.Calls()
	if len(calls) != 1 || calls[0].Script != string(fleet.StepDrain) || calls[0].Instance.ID != "host-1" {
		t.Errorf("remote calls = %+v, want the drain script on host-1", calls)
	}
	if _, err := f.run(t, "drain", "host-9"); exitCode(err) != exitFleetFailure {
		t.Errorf("draining a Host not in the Inventory = %v, want a failure", err)
	}
}

func TestFleetTeardownRemovesInventory(t *testing.T) {
	t.Parallel()
	f := stackFixture(t, opstest.Options{Hosts: 1, PoolSize: 1})
	f.stack.Declare(t)
	out, err := f.run(t, "teardown")
	if err != nil {
		t.Fatalf("fleet teardown: %v\n%s", err, out)
	}
	if _, err := os.Stat(f.invPath); !os.IsNotExist(err) {
		t.Errorf("Inventory still present: %v", err)
	}
	if len(f.ec2.TerminateCalls()) != 0 {
		t.Error("teardown terminated instances without --terminate")
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
