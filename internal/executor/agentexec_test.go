package executor

import (
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/phoban01/battery-operator/pkg/claimclient"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/hostservices"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/testing/fakeexecagent"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// recordingAgents is an AgentHosts that records what it was asked for and
// hands out a stub client.
type recordingAgents struct {
	mu       sync.Mutex
	leases   []transport.AgentClaim
	released int
}

func (r *recordingAgents) Lease(_ context.Context, claim transport.AgentClaim) (flintlock.HostClient, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leases = append(r.leases, claim)
	return agentStub{}, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.released++
	}, nil
}

// agentStub is a Host client that is never called: the recording factory
// builds no real transport.
type agentStub struct{ flintlock.HostClient }

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# Where the claim backend is configured, the Executor SHALL run
//# each Stage through the Exec Agent of the Host named in the Job's claim,
//# with the Stage script on standard input.

// TestAgentExecThroughTheClaimsHost runs a Job whose Profile names ssh on
// an executor configured for agent-exec. The transport is built as
// agent-exec for the claimed MicroVM, with the client of the Exec Agent at
// the address the claim gives for its Host; the Host Registry is never
// asked, and the client is released when the Job ends.
func TestAgentExecThroughTheClaimsHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.deps.Hosts = tripwireRegistry{t: t}
	profile := testProfile()
	profile.Transport = config.Transport{Kind: config.TransportSSH, SSH: config.SSHTransport{PrivateKeyFile: "/no/such/key"}}
	f.sched.profile = profile
	f.sched.tune = func(a *scheduler.Allocation) {
		a.VMUID = "vm-7"
		a.Lease.ID = "lease-7"
		a.Placement.Host = "host-7"
		a.Host = poolmgr.HostRef{Name: "host-7", Address: "10.0.0.7:10270"}
	}
	agents := &recordingAgents{}
	f.opts = append(f.opts, WithAgentExec(agents))

	if _, _, err := f.runBuild(context.Background(), testJob()); err != nil {
		t.Fatalf("the job failed: %v", err)
	}
	f.factory.mu.Lock()
	defer f.factory.mu.Unlock()
	if len(f.factory.targets) != 1 {
		t.Fatalf("%d transports built, want 1", len(f.factory.targets))
	}
	target := f.factory.targets[0]
	if target.Kind != transport.KindAgentExec || target.VMUID != "vm-7" || target.Host == nil {
		t.Errorf("target = kind %q, vm %q; want agent-exec to vm-7 through the agent", target.Kind, target.VMUID)
	}
	agents.mu.Lock()
	defer agents.mu.Unlock()
	want := transport.AgentClaim{LeaseID: "lease-7", VMUID: "vm-7", Host: "host-7", Address: "10.0.0.7:10270"}
	if len(agents.leases) != 1 || agents.leases[0] != want {
		t.Errorf("agent leases = %v, want the claim's lease, host and agent address %v", agents.leases, want)
	}
	if agents.released != 1 {
		t.Errorf("the agent client was released %d times, want once", agents.released)
	}
}

// publishHostServices publishes services, by name and port on gateway, on
// the Node node as the Host Agent does (hostservices), with every other
// Host Service switched off.
func publishHostServices(t *testing.T, kube *kubefake.Clientset, node, gateway string, services map[string]int) {
	t.Helper()
	cfg := &hostservices.Config{HostNode: node, BridgeGateway: gateway, HostServices: map[string]hostservices.HostService{}}
	for name, port := range services {
		cfg.HostServices[name] = hostservices.HostService{Enabled: true, Port: port}
	}
	if err := hostservices.Publish(context.Background(), kube, node, cfg.Annotations()); err != nil {
		t.Fatal(err)
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The Executor SHALL read the Host Service addresses for a Job
//# from the annotations of KF-194 that the Host Agent publishes on the Node
//# of the Job's Host.

// TestHostServicesFromTheHostsNode reads a Host's Node carrying the
// annotations the Host Agent publishes and gets its Host Services back,
// and gets none for a Node that cannot be read.
func TestHostServicesFromTheHostsNode(t *testing.T) {
	t.Parallel()
	kube := kubefake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "host-7"}})
	publishHostServices(t, kube, "host-7", "10.200.0.1", map[string]int{ServiceBuildkit: 1234, ServiceGoProxy: 3000})
	inv := NewHostNodeInventory(kube.CoreV1().Nodes(), nil, nil)
	entry, ok := inv.Host("host-7")
	if !ok {
		t.Fatal("the host's node was not read")
	}
	if entry.Services.Buildkit != "tcp://10.200.0.1:1234" || entry.Services.GoProxy != "http://10.200.0.1:3000" {
		t.Errorf("services = %+v", entry.Services)
	}
	if entry.Services.RegistryMirror != "" || entry.Services.HTTPCache != nil {
		t.Errorf("services that are not published were read: %+v", entry.Services)
	}
	if _, ok := inv.Host("no-such-node"); ok {
		t.Error("a node that cannot be read gave host services")
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# Where the claim backend is configured, the Executor SHALL run
//# each Stage through the Exec Agent of the Host named in the Job's claim,
//# with the Stage script on standard input.

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The Executor SHALL read the Host Service addresses for a Job
//# from the annotations of KF-194 that the Host Agent publishes on the Node
//# of the Job's Host.

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//= type=test
//# The `agent-exec` Guest Transport SHALL be tested with
//# battery-operator's Client Library against a test double of
//# battery-operator's Exec Agent in front of the fake Host, with no KVM and
//# no battery.

// TestJobOverAgentExec runs a whole Job through gitlab-runner's Build with
// the production transport factory and client pool. The claim is held by
// battery-operator's Client Library, and the Stages reach the test double
// of battery-operator's Exec Agent in front of a fake Host with that
// claim's token. The Host Registry fails the test if asked. The Host
// Service variables come from the annotations the Host Agent published on
// the Host's Node.
func TestJobOverAgentExec(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	host := fake.New(flintlock.FakeHostConfig{Name: "host-7", ExecEnabled: true, SandboxRoot: t.TempDir()})
	t.Cleanup(func() { _ = host.Close() })
	vm, err := host.Client().CreateMicroVM(ctx, &types.MicroVMSpec{Id: "job", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	certs, err := fake.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	world, err := fakeexecagent.NewWorld("ci", "builders")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := fakeexecagent.Start(fakeexecagent.Config{
		Node: "host-7", CertFile: certs.ServerCertFile, KeyFile: certs.ServerKeyFile,
		Upstream: host.Client(), Authorizer: world,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	pem, err := os.ReadFile(certs.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	servingCA := x509.NewCertPool()
	servingCA.AppendCertsFromPEM(pem)
	claims, err := claimclient.New(claimclient.Config{Client: world.Kube, Namespace: "ci", ServingCA: servingCA})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := world.Claim(ctx, claims, claimclient.Request{Pool: "builders", ServiceAccountName: "runner", Name: "job"}, vm.GetSpec().GetUid(), agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = claim.Release(context.Background()) })
	// The claim backend's dialler, by lease id.
	dial := func(ctx context.Context, leaseID string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
		if leaseID != claim.Name() {
			return nil, fmt.Errorf("no claim holds lease %s", leaseID)
		}
		return claim.Dial(ctx, opts...)
	}
	agents, err := transport.NewAgentHosts(dial, transport.AgentExecConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agents.Close() })

	kube := kubefake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "host-7"}})
	publishHostServices(t, kube, "host-7", "10.200.0.1", map[string]int{ServiceBuildkit: 1234})

	f := newFixture(t)
	f.deps.Hosts = tripwireRegistry{t: t}
	f.deps.Transports = transport.NewFactory()
	f.deps.Env = NewHostServiceEnv(config.HostServices{})
	f.deps.Inventory = NewHostNodeInventory(kube.CoreV1().Nodes(), nil, nil)
	f.opts = append(f.opts, WithAgentExec(agents))
	profile := testProfile()
	root := t.TempDir()
	profile.Shell, profile.User = "bash", "root"
	profile.BuildsDir, profile.CacheDir = root+"/builds", root+"/cache"
	f.sched.profile = profile
	f.sched.tune = func(a *scheduler.Allocation) {
		a.VMUID, a.Placement.Host = vm.GetSpec().GetUid(), claim.NodeName()
		a.Lease.ID = claim.Name()
		a.Host = poolmgr.HostRef{Name: claim.NodeName(), Address: claim.AgentAddress()}
	}

	job := testJob()
	job.Steps[0].Script = spec.StepScript{`echo "$JOB_SECRET_VALUE"`, `echo "buildkit at $BUILDKIT_HOST"`, `pwd`}
	trace, _, err := f.runBuild(ctx, job)
	if err != nil {
		t.Fatalf("the job failed: %v\n%s", err, trace.String())
	}
	log := trace.String()
	for _, want := range []string{"only-in-the-script", "buildkit at tcp://10.200.0.1:1234", root + "/builds/"} {
		if !strings.Contains(log, want) {
			t.Errorf("the job log lacks %q:\n%s", want, log)
		}
	}
	for _, c := range agent.Calls() {
		if c.Method == "ExecCommand" && c.Token != world.Tokens("job")[0] {
			t.Errorf("a Stage reached the agent with %q, not the claim's token", c.Token)
		}
	}
}
