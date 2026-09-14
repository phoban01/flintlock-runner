package main

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/opstest"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// These tests drive fleet provision --dry-run and fleet up through the
// production wiring with the fleetFixture's fakes: the opstest Discovery,
// Provisioner, Remote, Scripts and Control Node, and for verification the
// opstest Stack's fake Hosts and Pool Manager. Nothing reaches AWS and no
// script runs on this machine.

// contacts counts the calls into the seams through which a fleet command
// reaches the Pool Manager and the Hosts outside the Provisioner.
type contacts struct {
	poolManager atomic.Int32
	hosts       atomic.Int32
}

// counted wraps s so that c counts every Pool Manager client and Host
// dialer it hands out.
func (c *contacts) counted(s fleetSeams) fleetSeams {
	pm, hosts := s.PoolManager, s.Hosts
	s.PoolManager = func(cfg *config.Config) (poolmgr.Client, error) {
		c.poolManager.Add(1)
		return pm(cfg)
	}
	s.Hosts = func() flintlock.Dialer {
		c.hosts.Add(1)
		return hosts()
	}
	return s
}

// upFixture is a fleet fixture around a running opstest Stack whose
// Inventory is empty: its Provisioner turns the discovered instances
// host-1, host-2, ... into the Stack's Hosts, so that provisioning writes
// an Inventory that verification can exercise.
func upFixture(t *testing.T, opts opstest.Options) *fleetFixture {
	t.Helper()
	stack := opstest.Start(t, opts)
	f := newFleetFixture(t, stack.PoolManager.Addr(), true)
	f.stack = stack
	var insts []fleet.Instance
	for i, h := range stack.Inventory {
		insts = append(insts, metal(h.Name, fmt.Sprintf("10.0.1.%d", i+1)))
	}
	f.discovery.SetInstances(insts...)
	f.provisioner.Entry = func(inst fleet.Instance) config.HostEntry {
		h, ok := config.HostByName(stack.Inventory, inst.ID)
		if !ok {
			t.Errorf("provisioned %s, which is not a Stack Host", inst.ID)
			return config.HostEntry{Name: inst.ID}
		}
		return *h
	}
	return f
}

// snapshot is every file under dir with its mode, time and content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		var content []byte
		if !d.IsDir() {
			if content, err = os.ReadFile(p); err != nil {
				return err
			}
		}
		out[p] = fmt.Sprintf("%v %v %q", fi.Mode(), fi.ModTime().UnixNano(), content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

//= docs/requirements/06-fleet.md#one-shot-operation
//= type=test
//# Where the dry-run flag is passed, the provision command SHALL
//# report each instance it would provision, each Inventory Host it would leave
//# unchanged and each Host it would remove from the Inventory, and SHALL NOT
//# contact any Host, write the Inventory or the Runner configuration, or change
//# the Pool Manager.

// TestFleetProvisionDryRunReportsThePlanAndChangesNothing provisions i-1
// and i-2, then, with i-2 gone and i-3 new, runs a dry run. It reports i-3
// to provision, i-1 to leave alone and i-2 to remove, and afterwards no
// Host was contacted, no script was rendered or run, no file under the
// fixture changed or appeared and the Pool Manager was not reached. A real
// run from the same discovery then does exactly what the dry run said.
func TestFleetProvisionDryRunReportsThePlanAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newFleetFixture(t, "127.0.0.1:9440", false)
	f.discovery.SetInstances(metal("i-1", "10.0.1.1"), metal("i-2", "10.0.1.2"))
	if out, err := f.run(t, "provision"); err != nil {
		t.Fatalf("first provision: %v\n%s", err, out)
	}
	f.discovery.SetInstances(metal("i-1", "10.0.1.1"), metal("i-3", "10.0.1.3"))

	before := snapshot(t, f.dir)
	seen, remoteCalls, renders := len(f.provisioner.Seen()), len(f.remote.Calls()), len(f.scripts.Calls())
	installs, reloads := f.control.calls()
	var c contacts
	s := c.counted(f.seams())
	control := &scriptRecorder{}
	s.ControlRemote = control
	s.Dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("the dry run dialled a Host")
		return nil, fmt.Errorf("no dialling in a dry run")
	}

	out, err := runFleetCommand(t, s, f.configPath, "provision", "--dry-run")
	if err != nil {
		t.Fatalf("fleet provision --dry-run = %v, want exit 0 once the plan is computed\n%s", err, out)
	}
	for _, want := range []string{
		"would provision i-3 (c7g.metal, " + runtime.GOARCH + ", 10.0.1.3)",
		"would leave i-1 alone: it is already in the Inventory",
		"would remove i-2 (instance i-2) from the Inventory: the instance no longer exists",
		"plan: 1 to provision, 1 already in the Inventory, 1 to remove",
		"a dry run cannot tell which of the instances to provision would be excluded for want of it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}

	// No Host, script or Pool Manager was reached, and nothing was written.
	if got := f.provisioner.Seen(); len(got) != seen {
		t.Errorf("the dry run provisioned %v", got[seen:])
	}
	if got := f.remote.Calls(); len(got) != remoteCalls {
		t.Errorf("the dry run ran scripts on Hosts: %+v", got[remoteCalls:])
	}
	if got := f.scripts.Calls(); len(got) != renders {
		t.Errorf("the dry run rendered scripts: %+v", got[renders:])
	}
	if i, r := f.control.calls(); len(i) != len(installs) || len(r) != len(reloads) {
		t.Errorf("the dry run installed or reloaded the Pool Manager daemon: installs %v, reloads %v", i, r)
	}
	if n := c.poolManager.Load(); n != 0 {
		t.Errorf("the dry run built %d Pool Manager client(s)", n)
	}
	if n := c.hosts.Load(); n != 0 {
		t.Errorf("the dry run built %d Host dialer(s)", n)
	}
	if runs := control.scripts(); len(runs) != 0 {
		t.Errorf("the dry run ran scripts on the Control Node: %+v", runs)
	}
	after := snapshot(t, f.dir)
	for p, st := range after {
		if before[p] != st {
			t.Errorf("the dry run wrote %s", p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("the dry run removed %s", p)
		}
	}

	// The real run does what the dry run reported.
	if out, err := f.run(t, "provision"); err != nil {
		t.Fatalf("provision after the dry run: %v\n%s", err, out)
	}
	if got := f.provisioner.Seen()[seen:]; !slices.Equal(got, []string{"i-3"}) {
		t.Errorf("the real run provisioned %v, the dry run said [i-3]", got)
	}
	inv, err := config.LoadInventoryFile(f.invPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostNames(inv.Hosts); !slices.Equal(got, []string{"i-1", "i-3"}) {
		t.Errorf("Inventory after the real run = %v, the dry run said i-1 stays, i-2 goes and i-3 comes", got)
	}
}

//= docs/requirements/06-fleet.md#one-shot-operation
//= type=test
//# The Fleet Controller SHALL provide an up command that runs the
//# provision command, then runs the verification command with the Profiles'
//# Pools declared against the Runner configuration that provisioning wrote,
//# and reports the outcome of both in one summary.

//= docs/requirements/06-fleet.md#one-shot-operation
//= type=test
//# Where the install-runner flag is not passed, the up command
//# SHALL print the command that starts the Runner with the generated Runner
//# configuration.

// TestFleetUpProvisionsThenVerifiesAndPrintsTheRunCommand runs fleet up on
// two discovered instances. It provisions both, writes the Inventory and
// the Runner configuration, then declares the Pool and exercises a MicroVM
// on each Host, and ends with one summary that reports both steps and the
// command that starts the Runner with the generated configuration. Nothing
// is installed on the Control Node without --install-runner.
func TestFleetUpProvisionsThenVerifiesAndPrintsTheRunCommand(t *testing.T) {
	t.Parallel()
	f := upFixture(t, opstest.Options{Hosts: 2})
	// The input references an Inventory file that provisioning does not
	// write, so that only the Runner configuration provisioning wrote leads
	// verification to the provisioned Hosts.
	input, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	stale := "inventory:\n  file: " + f.invPath + "\n"
	if !strings.Contains(string(input), stale) {
		t.Fatalf("the fixture input no longer has %q", stale)
	}
	input = []byte(strings.Replace(string(input), stale, "inventory:\n  file: "+filepath.Join(f.dir, "elsewhere.yaml")+"\n", 1))
	if err := os.WriteFile(f.configPath, input, 0o600); err != nil {
		t.Fatal(err)
	}
	s := f.seams()
	control := &scriptRecorder{}
	s.ControlRemote = control

	out, err := runFleetCommand(t, s, f.configPath, "up")
	if err != nil {
		t.Fatalf("fleet up: %v\n%s", err, out)
	}
	if got := f.provisioner.Seen(); !slices.Equal(sortedCopy(got), []string{"host-1", "host-2"}) {
		t.Errorf("provisioned %v, want host-1 and host-2", got)
	}
	provisioned := strings.Index(out, "wrote the Runner configuration "+f.runnerPath)
	declared := strings.Index(out, "declared pool ")
	if provisioned < 0 || declared < 0 || declared < provisioned {
		t.Errorf("want provisioning to write the Runner configuration and then verification to declare the Pool:\n%s", out)
	}
	for _, want := range []string{"verifying the fleet against " + f.runnerPath, "host-1: claim to ready", "host-2: claim to ready", "verification passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	summary := out[strings.LastIndex(out, "fleet up summary:"):]
	for _, want := range []string{
		"provision  ok: the Inventory " + f.invPath + " lists 2 Host(s); 2 provisioned, 0 already there, 0 removed",
		"verify     ok: 2 Host(s) and 1 Pool(s) passed",
		"runner     not installed; start it with: flr --config " + f.runnerPath + " run",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary lacks %q:\n%s", want, summary)
		}
	}
	if strings.Count(out, "fleet up summary:") != 1 || strings.Index(out, "fleet up summary:") < strings.Index(out, "verification passed") {
		t.Errorf("want exactly one summary, after both steps:\n%s", out)
	}
	if runs := control.scripts(); len(runs) != 0 {
		t.Errorf("fleet up without --install-runner ran %+v on the Control Node", runs)
	}
}

//= docs/requirements/06-fleet.md#one-shot-operation
//= type=test
//# If provisioning fails, then the up command SHALL NOT run
//# verification and SHALL exit with a non-zero status.

// TestFleetUpStopsBeforeVerificationWhenProvisioningFails fails host-2's
// provisioning. host-1 provisions and would verify, but fleet up exits
// non-zero without building a Host dialer or declaring a Pool, and its
// summary says verification was not run.
func TestFleetUpStopsBeforeVerificationWhenProvisioningFails(t *testing.T) {
	t.Parallel()
	f := upFixture(t, opstest.Options{Hosts: 2})
	f.provisioner.Fail = map[string]bool{"host-2": true}
	var c contacts
	s := c.counted(f.seams())
	s.ControlRemote = &scriptRecorder{}

	out, err := runFleetCommand(t, s, f.configPath, "up", "--install-runner")
	if exitCode(err) != exitFleetFailure {
		t.Fatalf("fleet up = %v (exit %d), want exit %d because host-2 failed to provision\n%s", err, exitCode(err), exitFleetFailure, out)
	}
	for _, want := range []string{"fleet up summary:", "provision  failed: fleet provision: 1 failure(s)", "host-2: step flintlock",
		"verify     not run, because provisioning failed", "runner     not installed, because provisioning failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("exit message lacks %q:\n%s", want, err)
		}
	}
	if n := c.hosts.Load(); n != 0 {
		t.Errorf("verification ran after provisioning failed: %d Host dialer(s) built", n)
	}
	if strings.Contains(out, "declared pool") || strings.Contains(out, "verifying the fleet") {
		t.Errorf("verification ran after provisioning failed:\n%s", out)
	}
	if _, err := f.stack.Client.GetPool(context.Background(), f.stack.Ref()); err == nil {
		t.Error("the Pool was declared although provisioning failed")
	}
}

//= docs/requirements/06-fleet.md#one-shot-operation
//= type=test
//# If verification fails, then the up command SHALL exit with a
//# non-zero status.

// TestFleetUpExitsNonZeroWhenVerificationFails provisions two Hosts, one
// of which has its exec service disabled, so that verification fails there
// after provisioning succeeded.
func TestFleetUpExitsNonZeroWhenVerificationFails(t *testing.T) {
	t.Parallel()
	f := upFixture(t, opstest.Options{Hosts: 2, ExecDisabled: map[string]bool{"host-2": true}})
	s := f.seams()
	control := &scriptRecorder{}
	s.ControlRemote = control

	out, err := runFleetCommand(t, s, f.configPath, "up", "--timeout", "2s", "--install-runner")
	if exitCode(err) != exitFleetFailure {
		t.Fatalf("fleet up = %v (exit %d), want exit %d because host-2 fails verification\n%s", err, exitCode(err), exitFleetFailure, out)
	}
	for _, want := range []string{"provision  ok: the Inventory", "verify     failed:", "host-2: step exec_service",
		"runner     not installed, because verification failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("exit message lacks %q:\n%s", want, err)
		}
	}
	if runs := control.scripts(); len(runs) != 0 {
		t.Errorf("the Runner was installed on a fleet that failed verification: %+v", runs)
	}
}

//= docs/requirements/06-fleet.md#one-shot-operation
//= type=test
//# Where the install-runner flag is passed, the up command SHALL
//# install the Runner as a systemd service on the Control Node that uses the
//# generated Runner configuration, and SHALL enable and start it and verify
//# that it is active.

// TestFleetUpInstallsTheRunnerService runs fleet up --install-runner with
// the real script templates. After verification it renders the runner step
// for the Control Node and runs it there: a unit that starts this binary,
// installed, with the generated Runner configuration, enabled, started and
// checked active, readable files for its user limited to the Runner's own.
// When the step fails on the Control Node, fleet up exits non-zero.
func TestFleetUpInstallsTheRunnerService(t *testing.T) {
	t.Parallel()
	f := upFixture(t, opstest.Options{Hosts: 2})
	real, err := scripts.New()
	if err != nil {
		t.Fatal(err)
	}
	std := &standInScripts{real: real}
	bin := filepath.Join(t.TempDir(), "flr")
	if err := os.WriteFile(bin, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := f.seams()
	s.Scripts = func(*config.Config) (fleet.Scripts, error) { return std, nil }
	s.Executable = func() (string, error) { return bin, nil }
	control := &scriptRecorder{}
	s.ControlRemote = control

	out, err := runFleetCommand(t, s, f.configPath, "up", "--install-runner")
	if err != nil {
		t.Fatalf("fleet up --install-runner: %v\n%s", err, out)
	}
	runs := control.scripts()
	if len(runs) != 1 || runs[0].Name != string(fleet.StepRunner) || runs[0].Instance != controlNodeName {
		t.Fatalf("Control Node scripts = %+v, want the runner step once", runs)
	}
	if passed, installing := strings.Index(out, "verification passed"), strings.Index(out, "installing the Runner"); passed < 0 || installing < passed {
		t.Errorf("want the Runner installed after verification passed:\n%s", out)
	}
	rendered := std.renderedFor(fleet.StepRunner)
	if len(rendered) != 1 {
		t.Fatalf("the runner step was rendered %d times", len(rendered))
	}
	// fleet up installs the binary's resolved path. On macOS the temp
	// directory is under /var, a symlink to /private/var.
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	unit := rendered[0].Content
	for _, want := range []string{
		"config='" + f.runnerPath + "'",
		"install -D -m 0755 '" + resolved + "' \"$bin\"",
		"User=$user",
		"ExecStart=$bin --config $config run",
		`ensure_service "$unit"`,
		`status=$(systemctl is-active "$unit" || true)` + "\n" +
			`if [ "$status" != active ] || [ "$(systemctl show -p NRestarts --value "$unit")" != "$restarts" ]; then`,
		"'" + f.runnerPath + "' '" + f.invPath + "'",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the runner step lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "ca-key.pem") {
		t.Error("the runner step gives the Runner's user the CA key")
	}
	if !strings.Contains(out, "runner     ok: the systemd service flr is active, running flr --config "+f.runnerPath+" run") {
		t.Errorf("the summary does not report the Runner active:\n%s", out)
	}

	// The step failing on the Control Node fails fleet up.
	s.ControlRemote = &opstest.Remote{Fail: map[string]int{controlNodeName: 1}}
	out, err = runFleetCommand(t, s, f.configPath, "up", "--install-runner")
	if exitCode(err) != exitFleetFailure || !strings.Contains(err.Error(), "runner     failed: runner script exited 1") {
		t.Errorf("fleet up with a failing runner step = %v, want a non-zero exit naming it\n%s", err, out)
	}
}

//= docs/requirements/06-fleet.md#one-shot-operation
//= type=test
//# Where the dry-run flag is passed, the up command SHALL report
//# the provision plan and the steps it would run after provisioning, and
//# SHALL NOT provision, verify or install anything.

// TestFleetUpDryRunChangesNothing runs fleet up --dry-run --install-runner
// and checks that it prints the plan and the steps up would take, and that
// nothing was provisioned, written, declared, verified or installed.
func TestFleetUpDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	f := upFixture(t, opstest.Options{Hosts: 2})
	var c contacts
	s := c.counted(f.seams())
	control := &scriptRecorder{}
	s.ControlRemote = control
	before := snapshot(t, f.dir)

	out, err := runFleetCommand(t, s, f.configPath, "up", "--dry-run", "--install-runner")
	if err != nil {
		t.Fatalf("fleet up --dry-run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"would provision host-1", "would provision host-2", "plan: 2 to provision, 0 already in the Inventory, 0 to remove",
		"then fleet up would:",
		"write the Inventory " + f.invPath + " and the Runner configuration " + f.runnerPath,
		"install the Pool Manager daemon on the Control Node",
		"run fleet verify --declare against " + f.runnerPath,
		`install "flr --config ` + f.runnerPath + ` run" as the systemd service flr on the Control Node`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := f.provisioner.Seen(); len(got) != 0 {
		t.Errorf("the dry run provisioned %v", got)
	}
	if n, h := c.poolManager.Load(), c.hosts.Load(); n != 0 || h != 0 {
		t.Errorf("the dry run built %d Pool Manager client(s) and %d Host dialer(s)", n, h)
	}
	if runs := control.scripts(); len(runs) != 0 {
		t.Errorf("the dry run ran %+v on the Control Node", runs)
	}
	if i, r := f.control.calls(); len(i)+len(r) != 0 {
		t.Errorf("the dry run touched the Pool Manager daemon: %v %v", i, r)
	}
	if after := snapshot(t, f.dir); len(after) != len(before) {
		t.Errorf("the dry run wrote files: before %d, after %d", len(before), len(after))
	}
	if _, err := f.stack.Client.GetPool(context.Background(), f.stack.Ref()); err == nil {
		t.Error("the dry run declared the Pool")
	}
}

// TestFleetTeardownStopsTheRunnerService tears down a fleet whose Control
// Node has the Runner's service: the runner step runs there in its remove
// mode, and not at all where the service is not installed.
func TestFleetTeardownStopsTheRunnerService(t *testing.T) {
	t.Parallel()
	for _, installed := range []bool{true, false} {
		f := stackFixture(t, opstest.Options{Hosts: 1, PoolSize: 1})
		f.stack.Declare(t)
		real, err := scripts.New()
		if err != nil {
			t.Fatal(err)
		}
		std := &standInScripts{real: real}
		s := f.seams()
		s.Scripts = func(*config.Config) (fleet.Scripts, error) { return std, nil }
		s.RunnerInstalled = func() bool { return installed }
		control := &scriptRecorder{}
		s.ControlRemote = control
		if out, err := runFleetCommand(t, s, f.configPath, "teardown"); err != nil {
			t.Fatalf("fleet teardown: %v\n%s", err, out)
		}
		runs := control.scripts()
		if !installed {
			if len(runs) != 0 {
				t.Errorf("teardown ran %+v on a Control Node without the Runner's service", runs)
			}
			continue
		}
		if len(runs) != 1 || runs[0].Name != string(fleet.StepRunner) {
			t.Fatalf("Control Node scripts = %+v, want the runner step", runs)
		}
		rendered := std.renderedFor(fleet.StepRunner)
		if len(rendered) != 1 || !strings.Contains(rendered[0].Content, `systemctl disable --now --quiet "$unit"`) ||
			strings.Contains(rendered[0].Content, `ensure_service "$unit"`) {
			t.Errorf("the runner step teardown runs does not only stop and disable the service")
		}
	}
}
