package provision

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// call is one recorded Remote.Run.
type call struct {
	Instance fleet.Instance
	Script   fleet.Script
	Stdin    string
}

// recordingRemote records every script and answers from a table keyed by
// step. Nothing is executed.
type recordingRemote struct {
	mu      sync.Mutex
	calls   []call
	answers map[fleet.Step]fleet.RunResult
	// seq answers the first runs of a step in order before answers.
	seq map[fleet.Step][]fleet.RunResult
}

func (r *recordingRemote) Run(_ context.Context, inst fleet.Instance, s fleet.Script, out io.Writer) (*fleet.RunResult, error) {
	var stdin string
	if s.Stdin != nil {
		b, err := io.ReadAll(s.Stdin)
		if err != nil {
			return nil, err
		}
		stdin = string(b)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{Instance: inst, Script: s, Stdin: stdin})
	res := r.answers[fleet.Step(s.Name)]
	if q := r.seq[fleet.Step(s.Name)]; len(q) > 0 {
		res, r.seq[fleet.Step(s.Name)] = q[0], q[1:]
	}
	_, _ = io.WriteString(out, res.Stdout)
	return &res, nil
}

func (r *recordingRemote) steps() []fleet.Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []fleet.Step
	for _, c := range r.calls {
		out = append(out, fleet.Step(c.Script.Name))
	}
	return out
}

func (r *recordingRemote) call(step fleet.Step) (call, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.Script.Name == string(step) {
			return c, true
		}
	}
	return call{}, false
}

const (
	pinnedDetect = "::version:: flintlock v0.14.0\n::version:: firecracker v1.10.1\n" +
		"::version:: cloud_hypervisor v41.0\n::version:: containerd v1.7.22\n::thinpool:: present\n"
	freshDetect = "::version:: flintlock none\n::version:: firecracker none\n" +
		"::version:: cloud_hypervisor none\n::version:: containerd none\n::thinpool:: absent\n"
)

func testFleet() config.Fleet {
	return config.Fleet{
		Versions:       config.PinnedVersions{Flintlock: "v0.14.0", Firecracker: "v1.10.1", CloudHypervisor: "v41.0", Containerd: "v1.7.22", PoolManager: "v0.1.0"},
		ThinPoolDevice: "/dev/nvme1n1",
		GuestSubnet:    "172.31.0.0/16",
		Flintlockd:     config.Flintlockd{Port: 9090, Token: "s3cret-token"},
		HostReserve:    config.HostReserve{VCPU: 2, MemoryMB: 8192},
	}
}

func testServices() config.HostServices {
	on := true
	return config.HostServices{
		Buildkit:       config.Buildkit{Service: config.Service{Enabled: &on, Port: 1234}},
		GoProxy:        config.GoProxy{Service: config.Service{Enabled: &on, Port: 3000}, PrivatePatterns: []string{"gitlab.example.com/g/*"}, PrivateVCSHost: "gitlab.example.com", CredentialParameter: "/p/go"},
		RegistryMirror: config.RegistryMirror{Service: config.Service{Enabled: &on, Port: 5000}, Upstreams: []config.RegistryUpstream{{URL: "https://registry-1.docker.io"}, {URL: "https://ghcr.io", CredentialParameter: "/p/ghcr"}}},
		HTTPCache:      config.HTTPCache{Service: config.Service{Enabled: &on, Port: 3128}, Upstreams: []config.HTTPCacheUpstream{{Name: "npm", URL: "https://registry.npmjs.org"}}},
		CacheVolume:    config.CacheVolume{Directory: "/var/lib/flintlock-runner/cache", SizeCap: 10 << 30},
	}
}

var testInstance = fleet.Instance{ID: "i-0aaa", Type: "m7g.metal", Arch: config.ArchARM64, PrivateIP: "10.0.1.10", State: "running", VCPU: 64, MemoryMB: 262144, Tags: map[string]string{"tier": "general"}}

type fakeParams map[string]string

func (f fakeParams) GetParameter(_ context.Context, name string) (string, error) {
	v, ok := f[name]
	if !ok {
		return "", errors.New("no such parameter")
	}
	return v, nil
}

// okDial is a Dial that always connects.
func okDial(context.Context, string, string) (net.Conn, error) {
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func newTestProvisioner(t *testing.T, r fleet.Remote, mut func(*Options)) *Provisioner {
	t.Helper()
	sc, err := scripts.New()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"/pki/ca.pem": "CA PEM", "/pki/i-0aaa.pem": "CERT PEM", "/pki/i-0aaa.key": "KEY PEM"}
	o := Options{
		Scripts: sc, Remote: r, Fleet: testFleet(), HostServices: testServices(),
		Profiles:   []config.Profile{{Name: "p", Arch: config.ArchARM64, Kernel: config.Kernel{Image: "k:1"}, RootFS: "r:1"}},
		Certs:      &fleet.CertBundle{CAFile: "/pki/ca.pem", Hosts: map[string]fleet.HostCert{"i-0aaa": {CertFile: "/pki/i-0aaa.pem", KeyFile: "/pki/i-0aaa.key"}}},
		Parameters: fakeParams{"/p/go": "go-cred", "/p/ghcr": "user:ghcr-pass"},
		Dial:       okDial,
		ReadFile: func(name string) ([]byte, error) {
			v, ok := files[name]
			if !ok {
				return nil, errors.New("no such file")
			}
			return []byte(v), nil
		},
	}
	if mut != nil {
		mut(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProvisionRunsEveryStepInOrder(t *testing.T) {
	t.Parallel()
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{
		fleet.StepDetect:    {Stdout: freshDetect},
		fleet.StepThinPool:  {Stdout: "::changed:: thin pool flintlock/thinpool on /dev/nvme1n1\n"},
		fleet.StepFlintlock: {Stdout: "::changed:: flintlock v0.14.0\n"},
	}}
	res := newTestProvisioner(t, r, nil).Provision(context.Background(), testInstance)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	got := r.steps()
	// detect runs again after the provisioner changed the Host.
	want := append(HostSteps(), fleet.StepDetect)
	if strings.Join(stepNames(got), ",") != strings.Join(stepNames(want), ",") {
		t.Errorf("ran %v, want %v", got, want)
	}
	if res.UpToDate {
		t.Error("a fresh Host reported up to date")
	}
	if res.Steps[len(res.Steps)-1].Step != StepReachability {
		t.Errorf("last step = %s, want the reachability check", res.Steps[len(res.Steps)-1].Step)
	}
}

func stepNames(s []fleet.Step) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = string(v)
	}
	return out
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# If the configured thin pool already exists on an instance, then
//# the Fleet Controller SHALL NOT recreate it or wipe its device.

func TestProvisionSkipsThinPoolWhenPresent(t *testing.T) {
	t.Parallel()
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{
		fleet.StepDetect: {Stdout: strings.Replace(freshDetect, "absent", "present", 1)},
	}}
	res := newTestProvisioner(t, r, nil).Provision(context.Background(), testInstance)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if _, ran := r.call(fleet.StepThinPool); ran {
		t.Error("the thin pool script was sent to a Host that has the pool")
	}
	if !res.Steps[1].Skipped || res.Steps[1].Step != fleet.StepThinPool {
		t.Errorf("thin pool step = %+v, want skipped", res.Steps[1])
	}
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# When run against an instance that is already provisioned at the
//# pinned versions, the Fleet Controller SHALL make no changes to that
//# instance and report it as up to date.

func TestProvisionReportsUpToDateAndChangesNothing(t *testing.T) {
	t.Parallel()
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{fleet.StepDetect: {Stdout: pinnedDetect}}}
	res := newTestProvisioner(t, r, nil).Provision(context.Background(), testInstance)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.UpToDate {
		t.Error("a provisioned Host was not reported up to date")
	}
	for _, s := range []fleet.Step{fleet.StepThinPool, fleet.StepFlintlock} {
		if _, ran := r.call(s); ran {
			t.Errorf("%s ran on a Host at the pinned versions", s)
		}
	}

	// One pinned version off: the provisioner runs.
	r2 := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{
		fleet.StepDetect:    {Stdout: strings.Replace(pinnedDetect, "containerd v1.7.22", "containerd v1.7.20", 1)},
		fleet.StepFlintlock: {Stdout: "::changed:: flintlock v0.14.0\n"},
	}}
	res2 := newTestProvisioner(t, r2, nil).Provision(context.Background(), testInstance)
	if _, ran := r2.call(fleet.StepFlintlock); !ran || res2.UpToDate {
		t.Errorf("flintlock ran %v, up to date %v; want it run and not up to date", ran, res2.UpToDate)
	}

	// Any step that reports a change makes the Host not up to date.
	r3 := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{
		fleet.StepDetect:     {Stdout: pinnedDetect},
		fleet.StepNetworking: {Stdout: "::changed:: /etc/flintlock-runner/guest-firewall.nft\n"},
	}}
	if res3 := newTestProvisioner(t, r3, nil).Provision(context.Background(), testInstance); res3.UpToDate {
		t.Error("a Host whose firewall changed was reported up to date")
	}
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL record the installed versions of
//# flintlock, Firecracker, Cloud Hypervisor and containerd in the Inventory.

func TestProvisionRecordsInstalledVersions(t *testing.T) {
	t.Parallel()
	// Already provisioned: the versions detect found.
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{fleet.StepDetect: {Stdout: pinnedDetect}}}
	res := newTestProvisioner(t, r, nil).Provision(context.Background(), testInstance)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	want := config.InstalledVersions{Flintlock: "v0.14.0", Firecracker: "v1.10.1", CloudHypervisor: "v41.0", Containerd: "v1.7.22"}
	if res.Entry.Versions != want {
		t.Errorf("versions = %+v, want %+v", res.Entry.Versions, want)
	}

	// Freshly provisioned: detect runs again and its findings, not the
	// pins, are recorded.
	after := strings.Replace(pinnedDetect, "cloud_hypervisor v41.0", "cloud_hypervisor v41.0.1", 1)
	r2 := &recordingRemote{seq: map[fleet.Step][]fleet.RunResult{fleet.StepDetect: {{Stdout: freshDetect}, {Stdout: after}}}}
	res2 := newTestProvisioner(t, r2, nil).Provision(context.Background(), testInstance)
	if res2.Err != nil {
		t.Fatal(res2.Err)
	}
	want.CloudHypervisor = "v41.0.1"
	if res2.Entry.Versions != want {
		t.Errorf("versions = %+v, want %+v", res2.Entry.Versions, want)
	}
	if n := strings.Count(strings.Join(stepNames(r2.steps()), ","), string(fleet.StepDetect)); n != 2 {
		t.Errorf("detect ran %d times, want 2", n)
	}
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL verify TCP reachability from the
//# Control Node to each Host's `flintlockd` port after provisioning and SHALL
//# report any Host that is unreachable together with the security group
//# rule that would be needed.

func TestProvisionChecksFlintlockdReachability(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	inst := testInstance
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{fleet.StepDetect: {Stdout: pinnedDetect}}}
	p := newTestProvisioner(t, r, func(o *Options) {
		o.Dial = nil
		o.Fleet.EndpointOverrides = map[string]string{inst.ID: ln.Addr().String()}
	})
	if res := p.Provision(context.Background(), inst); res.Err != nil || res.Entry == nil {
		t.Fatalf("reachable Host failed: %v", res.Err)
	}

	// A closed port: the Host fails with the rule it needs.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().(*net.TCPAddr)
	_ = closed.Close()
	p = newTestProvisioner(t, r, func(o *Options) {
		o.Dial = nil
		o.DialTimeout = 2 * time.Second
		o.Fleet.EndpointOverrides = map[string]string{inst.ID: addr.String()}
	})
	res := p.Provision(context.Background(), inst)
	if res.Err == nil || res.Entry != nil {
		t.Fatalf("unreachable Host provisioned: entry %+v", res.Entry)
	}
	msg := res.Err.Error()
	for _, want := range []string{"unreachable from the Control Node", "security group", "inbound rule allowing TCP port " + strconv.Itoa(addr.Port), "127.0.0.1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if last := res.Steps[len(res.Steps)-1]; last.Step != StepReachability || last.Err == nil {
		t.Errorf("last step = %+v", last)
	}
}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# The Fleet Controller SHALL verify that each installed service is
//# active before reporting an instance as provisioned.

func TestProvisionFailsWhenAServiceIsInactive(t *testing.T) {
	t.Parallel()
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{
		fleet.StepDetect:       {Stdout: pinnedDetect},
		fleet.StepVerifyActive: {ExitCode: 1, Stdout: "::active:: containerd\n::inactive:: flintlock-runner-zot failed\n", Stderr: "verify_active: error: some services are not active\n"},
	}}
	res := newTestProvisioner(t, r, nil).Provision(context.Background(), testInstance)
	if res.Err == nil || res.Entry != nil {
		t.Fatalf("instance with an inactive service reported provisioned: %+v", res)
	}
	if !strings.Contains(res.Err.Error(), "flintlock-runner-zot") || !strings.Contains(res.Err.Error(), string(fleet.StepVerifyActive)) {
		t.Errorf("error %q does not name the step and the unit", res.Err)
	}
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL record the address and port of each
//# Host Service in the Host's Inventory entry.

func TestProvisionEntry(t *testing.T) {
	t.Parallel()
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{fleet.StepDetect: {Stdout: pinnedDetect}}}
	res := newTestProvisioner(t, r, nil).Provision(context.Background(), testInstance)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	e := res.Entry
	if e.Name != "i-0aaa" || e.Endpoint != "10.0.1.10:9090" || e.Arch != config.ArchARM64 || e.VCPU != 62 || e.MemoryMB != 262144-8192 {
		t.Errorf("entry = %+v", e)
	}
	if e.Services.Buildkit != "tcp://172.31.0.1:1234" || e.Services.GoProxy != "http://172.31.0.1:3000" ||
		e.Services.RegistryMirror != "http://172.31.0.1:5000" || e.Services.HTTPCache["npm"] != "http://172.31.0.1:3128/npm" {
		t.Errorf("services = %+v", e.Services)
	}
	if e.TLS.CAFile != "/pki/ca.pem" || e.Labels["tier"] != "general" || e.Token != "" {
		t.Errorf("tls %+v labels %v token %q", e.TLS, e.Labels, e.Token)
	}
}

// TestProvisionPassesSecretsOnStdin checks that the token, the TLS
// material and the Host Service credentials reach the Host on standard
// input and never in a script (SE-014, SE-015).
func TestProvisionPassesSecretsOnStdin(t *testing.T) {
	t.Parallel()
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{fleet.StepDetect: {Stdout: freshDetect}}}
	res := newTestProvisioner(t, r, nil).Provision(context.Background(), testInstance)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	for _, step := range []fleet.Step{fleet.StepFlintlock, fleet.StepFlintlockd} {
		c, _ := r.call(step)
		for _, want := range []string{"flintlockd_token czNjcmV0LXRva2Vu", "tls_ca Q0EgUEVN", "tls_cert Q0VSVCBQRU0=", "tls_key S0VZIFBFTQ=="} {
			if !strings.Contains(c.Stdin, want) {
				t.Errorf("%s stdin lacks %q", step, want)
			}
		}
	}
	hs, _ := r.call(fleet.StepHostServices)
	if !strings.Contains(hs.Stdin, "go_proxy_credential Z28tY3JlZA==") || !strings.Contains(hs.Stdin, "registry_credential_1 dXNlcjpnaGNyLXBhc3M=") {
		t.Errorf("host services stdin = %q", hs.Stdin)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		for _, secret := range []string{"s3cret-token", "KEY PEM", "go-cred", "ghcr-pass"} {
			if strings.Contains(c.Script.Content, secret) {
				t.Errorf("%s script contains a secret", c.Script.Name)
			}
		}
		if c.Instance.ID != testInstance.ID {
			t.Errorf("ran on %s", c.Instance.ID)
		}
	}
}

func TestProvisionStopsAtFailedStep(t *testing.T) {
	t.Parallel()
	r := &recordingRemote{answers: map[fleet.Step]fleet.RunResult{
		fleet.StepDetect:     {Stdout: pinnedDetect},
		fleet.StepNetworking: {ExitCode: 1, Stderr: "networking: error: no IPv4 default route\n"},
	}}
	res := newTestProvisioner(t, r, nil).Provision(context.Background(), testInstance)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no IPv4 default route") {
		t.Fatalf("err = %v", res.Err)
	}
	if _, ran := r.call(fleet.StepFlintlockd); ran {
		t.Error("ran a step after a failed one")
	}
}

func TestOptionsFrom(t *testing.T) {
	t.Parallel()
	f := testFleet()
	cfg := &config.Config{Fleet: &f, HostServices: testServices(), Profiles: []config.Profile{{Name: "p"}}}
	r := &recordingRemote{}
	o := OptionsFrom(cfg, fleet.Deps{Remote: r, Parameters: fakeParams{}}, &fleet.CertBundle{CAFile: "/ca"}, nil)
	if o.Remote != r || o.Fleet.ThinPoolDevice != f.ThinPoolDevice || o.Certs.CAFile != "/ca" || len(o.Profiles) != 1 || o.Parameters == nil {
		t.Errorf("options = %+v", o)
	}
}

func TestNewRequiresTLSUnlessInsecure(t *testing.T) {
	t.Parallel()
	sc, err := scripts.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Scripts: sc, Remote: &recordingRemote{}, Fleet: testFleet()}); err == nil {
		t.Error("New accepted a secure fleet without certificates")
	}
	f := testFleet()
	f.Flintlockd.Insecure = true
	if _, err := New(Options{Scripts: sc, Remote: &recordingRemote{}, Fleet: f}); err != nil {
		t.Error(err)
	}
}

// fakeAdmin answers ListPools after failing a number of times.
type fakeAdmin struct {
	poolmgr.PoolAdmin
	mu    sync.Mutex
	fails int
	calls int
}

func (f *fakeAdmin) ListPools(context.Context, string) ([]*poolmgr.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.fails {
		return nil, poolmgr.ErrUnavailable
	}
	return nil, nil
}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# The Fleet Controller SHALL verify after installation that the
//# Pool Manager daemon answers `ListPools`

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# The Fleet Controller SHALL install the pinned Pool Manager daemon
//# on the Control Node with a host list generated from the Inventory that
//# names every Host with its `flintlockd` endpoint, token and TLS settings.

func TestControlNodeInstallsAndWaitsForListPools(t *testing.T) {
	t.Parallel()
	sc, err := scripts.New()
	if err != nil {
		t.Fatal(err)
	}
	r := &recordingRemote{}
	admin := &fakeAdmin{fails: 1}
	self := fleet.Instance{ID: "control-node", Arch: config.ArchARM64, PrivateIP: "10.0.0.5"}
	cn, err := NewControlNode(ControlNodeOptions{
		Scripts: sc, Remote: r, Self: self, Fleet: testFleet(), PoolManager: admin, ReadyTimeout: 5 * time.Second,
		Options: map[string]string{scripts.OptionPoolManagerListen: "10.0.0.5:9443"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inv := &fleet.Inventory{Hosts: []config.HostEntry{{Name: "i-0aaa", Endpoint: "10.0.1.10:9090"}, {Name: "i-0bbb", Endpoint: "10.0.1.11:9090"}}}
	if err := cn.InstallPoolManager(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	c, ok := r.call(fleet.StepControlNode)
	if !ok || c.Instance.ID != "control-node" {
		t.Fatalf("control node script not run on the Control Node: %+v", c.Instance)
	}
	for _, h := range inv.Hosts {
		if !strings.Contains(c.Script.Content, "'"+h.Name+"'") || !strings.Contains(c.Script.Content, "'"+h.Endpoint+"'") {
			t.Errorf("host list lacks %s", h.Name)
		}
	}
	if strings.Contains(c.Script.Content, "s3cret-token") || !strings.Contains(c.Stdin, "flintlockd_token ") {
		t.Error("token not passed on stdin only")
	}
	if admin.calls < 2 {
		t.Errorf("ListPools called %d times, want a retry until it answered", admin.calls)
	}

	// A daemon that never answers fails the install.
	cn, err = NewControlNode(ControlNodeOptions{Scripts: sc, Remote: r, Self: self, Fleet: testFleet(), PoolManager: &fakeAdmin{fails: 1 << 30}, ReadyTimeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := cn.ReloadPoolManager(context.Background(), inv); err == nil || !strings.Contains(err.Error(), "ListPools") {
		t.Errorf("err = %v", err)
	}
}

func TestLocalRemoteRunsWithStdinAndPrefix(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	res, err := LocalRemote{}.Run(context.Background(), fleet.Instance{ID: "cn"}, fleet.Script{
		Name: "t", Content: "read -r x; echo \"got $x\"; echo oops >&2; exit 3\n", Stdin: strings.NewReader("hello\n"), Timeout: 10 * time.Second,
	}, &syncWriter{w: &out})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "got hello\n" || res.Stderr != "oops\n" {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(out.String(), "cn: got hello\n") || !strings.Contains(out.String(), "cn: oops\n") {
		t.Errorf("streamed %q", out.String())
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}
