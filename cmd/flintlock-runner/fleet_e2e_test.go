package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsfake"
	"github.com/phoban01/flintlock-runner/internal/fleet/ca"
	"github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/fleet/launchtemplate"
	"github.com/phoban01/flintlock-runner/internal/fleet/opstest"
	"github.com/phoban01/flintlock-runner/internal/fleet/remote"
	"github.com/phoban01/flintlock-runner/internal/fleet/remote/sshtest"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
)

// These tests drive the fleet subcommands through the production wiring
// (productionSeams) with fakes only at the edges: awsfake for EC2, Systems
// Manager and its parameters, the in-process SSH server, the opstest fake
// Hosts and Pool Manager, and a Remote that records the Control Node's
// script. No test reaches AWS, and no provisioning script runs on this
// machine: over Systems Manager the fake records every script and answers
// for it, and over SSH every script is rendered for real and recorded, but
// what the SSH server runs in its place is a stand-in that only prints the
// report lines the real step would.

// recordedScript is one script a Remote was asked to run.
type recordedScript struct {
	Instance string
	Name     string
	Content  string
	Stdin    string
}

// scriptRecorder is a fleet.Remote that records every script, reads its
// standard input, and succeeds without running anything.
type scriptRecorder struct {
	mu   sync.Mutex
	runs []recordedScript
}

func (r *scriptRecorder) Run(_ context.Context, inst fleet.Instance, s fleet.Script, _ io.Writer) (*fleet.RunResult, error) {
	var stdin []byte
	if s.Stdin != nil {
		var err error
		if stdin, err = io.ReadAll(s.Stdin); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, recordedScript{Instance: inst.ID, Name: s.Name, Content: s.Content, Stdin: string(stdin)})
	return &fleet.RunResult{}, nil
}

func (r *scriptRecorder) scripts() []recordedScript {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs)
}

// standInScripts renders every script with the real templates and records
// it, then hands out in its place a stand-in that reads its standard input,
// prints what report reports for the step and exits with status 3 on the
// instances fail names for that step. The real script never leaves the
// process.
type standInScripts struct {
	real   fleet.Scripts
	report func(step fleet.Step) string
	fail   map[string]fleet.Step

	mu       sync.Mutex
	rendered []recordedScript
}

func (s *standInScripts) Render(step fleet.Step, in fleet.RenderInput) (fleet.Script, error) {
	sc, err := s.real.Render(step, in)
	if err != nil {
		return sc, err
	}
	s.mu.Lock()
	s.rendered = append(s.rendered, recordedScript{Instance: in.Instance.ID, Name: sc.Name, Content: sc.Content})
	s.mu.Unlock()
	code := 0
	if st, ok := s.fail[in.Instance.ID]; ok && st == step {
		code = 3
	}
	var out string
	if s.report != nil {
		out = s.report(step)
	}
	sc.Content = "#!/usr/bin/env bash\n" +
		"# Stands in for the rendered " + string(step) + " script, which was recorded and is not run.\n" +
		"while read -r _; do :; done\n" +
		"printf '%s' " + shellQuote(out) + "\n" +
		"if [ " + strconv.Itoa(code) + " != 0 ]; then echo 'stand-in failure in " + string(step) + "' >&2; fi\n" +
		"exit " + strconv.Itoa(code) + "\n"
	return sc, nil
}

func (s *standInScripts) All() []fleet.Step { return s.real.All() }

func (s *standInScripts) renderedFor(step fleet.Step) []recordedScript {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedScript
	for _, r := range s.rendered {
		if r.Name == string(step) {
			out = append(out, r)
		}
	}
	return out
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// detectReport is what the detect step reports for a Host with a usable
// /dev/kvm, 48 vCPU and 96 GiB of memory, the pinned flintlock components
// and its thin pool.
const detectReport = "::kvm:: ok\n::capacity:: vcpu 48\n::capacity:: memory_mb 98304\n" +
	"::version:: flintlock v0.9.0\n::version:: firecracker v1.10.0\n" +
	"::version:: cloud_hypervisor none\n::version:: containerd v1.7.0\n::thinpool:: present\n"

func reportFor(step fleet.Step) string {
	if step == fleet.StepDetect {
		return detectReport
	}
	return "::changed:: " + string(step) + "\n"
}

// runFleetCommand runs one fleet subcommand with s and returns its output.
func runFleetCommand(t *testing.T, s fleetSeams, configPath string, args ...string) (string, error) {
	t.Helper()
	app := newApp()
	for i := range app.Commands {
		if app.Commands[i].Name == "fleet" {
			app.Commands[i].Subcommands = fleetCommands(s)
		}
	}
	var out syncBuffer
	app.Writer = &out
	app.ErrWriter = &out
	err := app.Run(append([]string{"flintlock-runner", "--config", configPath, "fleet"}, args...))
	return out.String(), err
}

// e2eSeams are the production seams with the Control Node's scripts
// recorded instead of run, and AWS failing the test unless aws is given.
func e2eSeams(t *testing.T, aws *awsClients, control fleet.Remote) fleetSeams {
	t.Helper()
	s := productionSeams()
	s.AWS = func(context.Context, string) (*awsClients, error) {
		if aws == nil {
			t.Error("the fleet command reached for AWS")
			return nil, errors.New("no AWS in this test")
		}
		return aws, nil
	}
	s.ControlRemote = control
	s.ControlReadyTimeout = 10 * time.Second
	s.RemoteOptions = []remote.Option{remote.WithPollInterval(time.Millisecond)}
	s.Now = func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }
	return s
}

// e2eConfig is a fleet input around the opstest Stack's Pool Manager and
// Profile, with fleetSection as its fleet section.
func e2eConfig(t *testing.T, dir string, stack *opstest.Stack, fleetSection string) string {
	t.Helper()
	cfg := fmt.Sprintf(`gitlab:
  url: https://gitlab.example.com
  token: glrt-fleet-test
  name: %s
pool_manager:
  endpoint: %s
  tls:
    insecure: true
scheduler:
  namespace: %s
inventory:
  file: %s/inventory.yaml
profiles:
  - name: small
    arch: %s
    shell: %s
    kernel:
      image: ghcr.io/example/kernel:test
    rootfs: ghcr.io/example/rootfs:test
    pool:
      size: 1
fleet:
  versions:
    flintlock: v0.9.0
    firecracker: v1.10.0
    containerd: v1.7.0
    pool_manager: v0.1.0
  thin_pool_device: /dev/nvme1n1
  host_reserve:
    vcpu: 2
    memory_mb: 4096
  inventory_path: %[4]s/inventory.yaml
  runner_config_path: %[4]s/runner.yaml
  drain_timeout: 20s
%[7]s`, opstest.RunnerName, stack.PoolManager.Addr(), opstest.Namespace, dir, runtime.GOARCH, stack.Profiles[0].Shell, fleetSection)
	path := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

//= docs/requirements/06-fleet.md#remote-execution
//= type=test
//# If provisioning fails on one instance, then the Fleet Controller
//# SHALL continue provisioning the others and SHALL exit with a non-zero
//# status that summarises every failure.

// TestFleetProvisionOverSSMContinuesPastFailuresAndExitsNonZero provisions
// a tag-discovered fleet of three instances over Systems Manager, one at a
// time, where the first instance's networking step and the third's
// flintlockd step fail. The second is still provisioned and alone written
// to the Inventory and the Pool Manager's host list, the third is still
// tried after the first failed, and the command exits non-zero with a
// summary naming both failures and their steps.
func TestFleetProvisionOverSSMContinuesPastFailuresAndExitsNonZero(t *testing.T) {
	t.Parallel()
	stack := opstest.Start(t, opstest.Options{Hosts: 1})
	dir := t.TempDir()

	// The TLS material the fleet supplies, and the Hosts read from the
	// Systems Manager parameters named below.
	supplied, err := (ca.Authority{}).Ensure(context.Background(), filepath.Join(dir, "supplied"), []fleet.Instance{{ID: "fleet", PrivateIP: "10.0.1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	path := e2eConfig(t, dir, stack, fmt.Sprintf(`  region: eu-west-1
  discovery:
    tag_key: flintlock-runner
    tag_value: ci
  parallelism: 1
  flintlockd:
    token: fleet-secret-token
    tls:
      ca_file: %s
      cert_file: %s
      key_file: %s
  launch_template:
    parameters:
      host_token: /flr/host-token
      tls_ca: /flr/tls-ca
      tls_cert: /flr/tls-cert
      tls_key: /flr/tls-key
`, supplied.CAFile, supplied.Hosts["fleet"].CertFile, supplied.Hosts["fleet"].KeyFile))

	tags := map[string]string{"flintlock-runner": "ci"}
	instance := func(id, ip string) fleet.Instance {
		return fleet.Instance{ID: id, Type: "c7g.metal", Arch: config.Architecture(runtime.GOARCH), PrivateIP: ip, Tags: tags, VCPU: 64, MemoryMB: 131072}
	}
	ec2 := awsfake.NewEC2(instance("i-1", "10.0.1.1"), instance("i-2", "10.0.1.2"), instance("i-3", "10.0.1.3"),
		fleet.Instance{ID: "i-other", Type: "c7g.metal", Arch: config.ArchARM64, PrivateIP: "10.0.9.9", Tags: map[string]string{"flintlock-runner": "other"}})
	ssm := awsfake.NewSSM()
	failAt := map[string]fleet.Step{"i-1": fleet.StepNetworking, "i-3": fleet.StepFlintlockd}
	ssm.SetResponder(func(in fleet.SendCommandInput, id string) awsfake.Reply {
		step := fleet.Step(in.Comment)
		if failAt[id] == step {
			return awsfake.Reply{ExitCode: 1, Stderr: "stand-in failure in " + string(step) + "\n"}
		}
		return awsfake.Reply{Stdout: reportFor(step)}
	})
	params := awsfake.NewParameters(nil)
	control := &scriptRecorder{}
	s := e2eSeams(t, &awsClients{EC2: ec2, SSM: ssm, Parameters: params}, control)
	var dialed []string
	var dialMu sync.Mutex
	s.Dial = func(_ context.Context, _, address string) (net.Conn, error) {
		dialMu.Lock()
		dialed = append(dialed, address)
		dialMu.Unlock()
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}

	out, err := runFleetCommand(t, s, path, "provision")
	if exitCode(err) != exitFleetFailure {
		t.Fatalf("fleet provision = %v (exit %d), want exit %d because two instances failed\n%s", err, exitCode(err), exitFleetFailure, out)
	}
	msg := err.Error()
	for _, want := range []string{"2 failure(s)", "i-1: step networking", "stand-in failure in networking", "i-3: step flintlockd", "stand-in failure in flintlockd"} {
		if !strings.Contains(msg, want) {
			t.Errorf("exit message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "i-2") {
		t.Errorf("exit message names i-2, which provisioned:\n%s", msg)
	}

	// Every instance was worked on, in order, the third after the first
	// had failed.
	var order []string
	for _, c := range ssm.SentCommands() {
		if len(c.InstanceIDs) != 1 {
			t.Fatalf("SendCommand to %v, want one instance at a time", c.InstanceIDs)
		}
		if id := c.InstanceIDs[0]; len(order) == 0 || order[len(order)-1] != id {
			order = append(order, id)
		}
		// Nothing secret travels in a script (SE-015): over Systems Manager
		// the Hosts read it from the parameters.
		for _, secret := range []string{"fleet-secret-token", "PRIVATE KEY", "BEGIN CERTIFICATE"} {
			if strings.Contains(c.Script, secret) {
				t.Errorf("script %s to %s carries %q", c.Comment, c.InstanceIDs[0], secret)
			}
		}
	}
	if !slices.Equal(order, []string{"i-1", "i-2", "i-3"}) {
		t.Errorf("instances were provisioned in the order %v, want i-1, i-2 and then i-3 despite i-1 failing", order)
	}
	if describes := ec2.DescribeCalls(); len(describes) != 1 || describes[0].TagKey != "flintlock-runner" || describes[0].TagValue != "ci" {
		t.Errorf("DescribeInstances calls = %+v, want one by the tag", describes)
	}
	if len(ec2.TerminateCalls()) != 0 {
		t.Error("provisioning terminated an instance")
	}

	// The instance that provisioned is the Inventory, with its capacity
	// less the Host reserve once, and it trusts the supplied CA.
	inv, err := config.LoadInventoryFile(filepath.Join(dir, "inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hostNames(inv.Hosts); !slices.Equal(got, []string{"i-2"}) {
		t.Fatalf("Inventory = %v, want only i-2", got)
	}
	h := inv.Hosts[0]
	if h.Endpoint != "10.0.1.2:9090" || h.VCPU != 48-2 || h.MemoryMB != 98304-4096 {
		t.Errorf("i-2 entry = endpoint %s, %d vCPU, %d MB; want 10.0.1.2:9090 with the measured 48-2 vCPU and 98304-4096 MB", h.Endpoint, h.VCPU, h.MemoryMB)
	}
	if h.TLS.CAFile != supplied.CAFile || h.Versions.Flintlock != "v0.9.0" || h.Labels[inventory.LabelInstanceID] != "i-2" {
		t.Errorf("i-2 entry = %+v, want the supplied CA, the detected versions and its instance id", h)
	}
	if !slices.Contains(dialed, "10.0.1.2:9090") {
		t.Errorf("reachability dialed %v, want i-2's flintlockd endpoint (FL-047)", dialed)
	}

	// The Pool Manager daemon was installed with the one Host, verifying
	// it with the supplied CA, and its token went on standard input.
	runs := control.scripts()
	if len(runs) != 1 || runs[0].Name != string(fleet.StepControlNode) || runs[0].Instance != controlNodeName {
		t.Fatalf("Control Node scripts = %+v, want one control_node run", runs)
	}
	for _, want := range []string{"'i-2'", "'10.0.1.2:9090'", supplied.CAFile, stack.PoolManager.Addr()} {
		if !strings.Contains(runs[0].Content, want) {
			t.Errorf("the control_node script lacks %q", want)
		}
	}
	if strings.Contains(runs[0].Content, "'i-1'") || strings.Contains(runs[0].Content, "'i-3'") {
		t.Error("the Pool Manager host list names a Host that failed to provision")
	}
	if !strings.Contains(runs[0].Stdin, scripts.SecretFlintlockdToken+" ") || strings.Contains(runs[0].Content, "fleet-secret-token") {
		t.Errorf("the control_node script's token: stdin %q", runs[0].Stdin)
	}
}

// TestFleetProvisionOverSSMNeedsParameterSecrets checks that a fleet
// provisioned over Systems Manager, which has no standard input, is refused
// before anything runs when its Hosts have nowhere to read their secrets
// from.
func TestFleetProvisionOverSSMNeedsParameterSecrets(t *testing.T) {
	t.Parallel()
	stack := opstest.Start(t, opstest.Options{Hosts: 1})
	dir := t.TempDir()
	path := e2eConfig(t, dir, stack, `  region: eu-west-1
  discovery:
    tag_key: flintlock-runner
    tag_value: ci
`)
	ssm := awsfake.NewSSM()
	s := e2eSeams(t, &awsClients{EC2: awsfake.NewEC2(), SSM: ssm, Parameters: awsfake.NewParameters(nil)}, &scriptRecorder{})
	out, err := runFleetCommand(t, s, path, "provision")
	if exitCode(err) != exitInvalidConfig || !strings.Contains(err.Error(), "fleet.launch_template.parameters") {
		t.Fatalf("fleet provision = %v, want a configuration error naming the parameters\n%s", err, out)
	}
	if len(ssm.SentCommands()) != 0 {
		t.Error("scripts were sent although the Hosts could not get their secrets")
	}
}

// TestFleetOverSSHStaticNeedsNoAWSAndTearsDown provisions a static fleet of
// one machine over SSH through the in-process SSH server, then tears it
// down with and without --purge. Nothing reaches for AWS; every Host step
// and the teardown script are rendered from the real templates for the
// machine and delivered over SSH, where a stand-in runs in their place.
func TestFleetOverSSHStaticNeedsNoAWSAndTearsDown(t *testing.T) {
	t.Parallel()
	stack := opstest.Start(t, opstest.Options{Hosts: 1, PoolSize: 1})
	server := sshtest.New(t)
	_, port, err := net.SplitHostPort(stack.Inventory[0].Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := e2eConfig(t, dir, stack, fmt.Sprintf(`  discovery:
    static:
      - name: host-1
        address: 127.0.0.1
        arch: %s
        vcpu: 64
        memory_mb: 131072
  remote:
    mode: ssh
    ssh:
      user: fleet
      key_file: %s
      port: %d
      known_hosts_file: %s
  flintlockd:
    port: %s
    token: %s
    insecure: true
`, runtime.GOARCH, server.ClientKeyFile, server.Port(), server.KnownHostsFile, port, opstest.Token))

	std := &standInScripts{report: reportFor}
	real, err := scripts.New()
	if err != nil {
		t.Fatal(err)
	}
	std.real = real
	control := &scriptRecorder{}
	s := e2eSeams(t, nil, control)
	s.Scripts = func(*config.Config) (fleet.Scripts, error) { return std, nil }

	out, err := runFleetCommand(t, s, path, "provision")
	if err != nil {
		t.Fatalf("fleet provision: %v\n%s", err, out)
	}
	invPath := filepath.Join(dir, "inventory.yaml")
	inv, err := config.LoadInventoryFile(invPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 1 {
		t.Fatalf("Inventory = %v, want host-1", hostNames(inv.Hosts))
	}
	h := inv.Hosts[0]
	if h.Name != "host-1" || h.Endpoint != stack.Inventory[0].Endpoint || h.VCPU != 48-2 || h.MemoryMB != 98304-4096 || !h.TLS.Insecure {
		t.Errorf("host-1 entry = %+v, want its flintlockd endpoint and the measured capacity less the reserve", h)
	}
	// Each Host step that detect did not rule out went over SSH: an upload
	// and a run each, as the configured user.
	want := []fleet.Step{fleet.StepDetect, fleet.StepNetworking, fleet.StepFlintlockd,
		fleet.StepHostServices, fleet.StepPrepull, fleet.StepPrewarm, fleet.StepVerifyActive}
	var steps []fleet.Step
	for _, r := range std.rendered {
		if r.Name == string(fleet.StepControlNode) {
			continue
		}
		if r.Instance != "host-1" {
			t.Errorf("rendered %s for %q", r.Name, r.Instance)
		}
		steps = append(steps, fleet.Step(r.Name))
	}
	if !slices.Equal(steps, want) {
		t.Errorf("rendered steps = %v, want %v", steps, want)
	}
	execs := server.Execs()
	if len(execs) != 2*len(want) {
		t.Errorf("the SSH server ran %d commands, want an upload and a run for each of %d steps", len(execs), len(want))
	}
	for _, e := range execs {
		if e.User != "fleet" {
			t.Errorf("SSH as %q, want the configured user", e.User)
		}
	}
	if fl := std.renderedFor(fleet.StepFlintlockd); len(fl) != 1 || !strings.Contains(fl[0].Content, "127.0.0.1") {
		t.Errorf("the flintlockd step was not rendered for the machine's address")
	}
	// The Pool Manager daemon was installed on the Control Node, never over
	// SSH, with host-1 in its host list.
	cn := std.renderedFor(fleet.StepControlNode)
	if runs := control.scripts(); len(runs) != 1 || len(cn) != 1 || !strings.Contains(cn[0].Content, "'host-1'") {
		t.Errorf("Control Node scripts = %d, rendered %d, want the Pool Manager installed once with host-1", len(runs), len(cn))
	}

	// Teardown without --purge keeps the thin pool; with it, removes it.
	// Both disable every installed unit on host-1.
	for _, purge := range []bool{false, true} {
		if err := (inventory.Store{}).Save(context.Background(), invPath, &fleet.Inventory{Hosts: inv.Hosts}); err != nil {
			t.Fatal(err)
		}
		stack.Declare(t)
		args := []string{"teardown"}
		if purge {
			args = append(args, "--purge")
		}
		out, err := runFleetCommand(t, s, path, args...)
		if err != nil {
			t.Fatalf("fleet %v: %v\n%s", args, err, out)
		}
		if _, err := os.Stat(invPath); !os.IsNotExist(err) {
			t.Errorf("fleet %v left the Inventory: %v", args, err)
		}
		td := std.renderedFor(fleet.StepTeardown)
		last := td[len(td)-1]
		if len(td) != map[bool]int{false: 1, true: 2}[purge] || last.Instance != "host-1" {
			t.Fatalf("teardown scripts = %d, last for %q", len(td), last.Instance)
		}
		for _, u := range []string{"flintlockd", "containerd", "dnsmasq", "flintlock-runner-network", "nginx"} {
			if !strings.Contains(last.Content, " "+u+" ") && !strings.Contains(last.Content, " "+u+";") {
				t.Errorf("fleet %v: the teardown script does not disable %s", args, u)
			}
		}
		if got := strings.Contains(last.Content, "vgremove"); got != purge {
			t.Errorf("fleet %v: the teardown script removes the thin pool = %v, want %v", args, got, purge)
		}
	}
}

//= docs/requirements/06-fleet.md#launch-template-mode
//= type=test
//# Where launch template mode is selected, the Fleet Controller
//# SHALL emit a cloud-init user-data script that performs the Host
//# provisioning steps unattended at first boot so that instances launched by
//# an auto scaling group self-provision.

// TestFleetEmitUserDataFitsEC2Limit emits the user-data for the complete
// reference configuration, every Host Service on, and checks that what the
// command writes, the gzip-compressed script, is within EC2's 16384-byte
// user-data limit that an auto scaling group enforces, and decompresses to
// the script --plain prints.
func TestFleetEmitUserDataFitsEC2Limit(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "internal", "config", "testdata", "full.yaml")
	params := awsfake.NewParameters(map[string]string{
		"/flintlock-runner/host-token": "host-token-value",
		"/flintlock-runner/tls-ca":     "ca-value",
		"/flintlock-runner/tls-cert":   "cert-value",
		"/flintlock-runner/tls-key":    "key-value",
	})
	s := e2eSeams(t, &awsClients{EC2: awsfake.NewEC2(), SSM: awsfake.NewSSM(), Parameters: params}, &scriptRecorder{})

	emitted, err := runFleetCommand(t, s, path, "emit-userdata")
	if err != nil {
		t.Fatalf("fleet emit-userdata: %v\n%s", err, emitted)
	}
	if n := len(emitted); n > launchtemplate.MaxUserDataBytes || n > 16384 {
		t.Errorf("the emitted user-data is %d bytes; EC2 rejects more than 16384", n)
	}
	zr, err := gzip.NewReader(strings.NewReader(emitted))
	if err != nil {
		t.Fatalf("the emitted user-data is not gzip, which cloud-init needs to decompress it: %v", err)
	}
	script, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := runFleetCommand(t, s, path, "emit-userdata", "--plain")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(script, []byte(plain)) || !strings.HasPrefix(plain, "#!/bin/bash\n") {
		t.Errorf("the gzip user-data does not decompress to the --plain script")
	}
	// The script itself embeds every Host step and is over the limit,
	// which is why it has to be compressed.
	t.Logf("user-data: %d bytes plain, %d gzip-compressed", len(plain), len(emitted))
	if !strings.Contains(plain, "flintlock-runner-buildkitd") || strings.Contains(plain, "host-token-value") {
		t.Error("the plain user-data lacks the Host Service steps or embeds a secret")
	}
}
