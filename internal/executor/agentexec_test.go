package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/phoban01/flintlock-runner/internal/agent"
	"github.com/phoban01/flintlock-runner/internal/agent/agenttest"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// recordingAgents is an AgentHosts that records what it was asked for and
// hands out a stub client.
type recordingAgents struct {
	mu       sync.Mutex
	leases   []poolmgr.HostRef
	released int
}

func (r *recordingAgents) Lease(host, address string) (flintlock.HostClient, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leases = append(r.leases, poolmgr.HostRef{Name: host, Address: address})
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
	if len(agents.leases) != 1 || agents.leases[0] != (poolmgr.HostRef{Name: "host-7", Address: "10.0.0.7:10270"}) {
		t.Errorf("agent leases = %v, want the claim's host and agent address", agents.leases)
	}
	if agents.released != 1 {
		t.Errorf("the agent client was released %d times, want once", agents.released)
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The Executor SHALL read the Host Service addresses for a Job
//# from the annotations of KF-179 on the Node of the Job's Host.

// TestHostServicesFromTheHostsNode reads a Host's Node carrying the
// annotations the Exec Agent publishes and gets its Host Services back, and
// gets none for a Node that cannot be read.
func TestHostServicesFromTheHostsNode(t *testing.T) {
	t.Parallel()
	nodes := kubefake.NewClientset(virtualNode("host-7", map[string]string{
		ServiceBuildkit: "10.200.0.1:1234",
		ServiceGoProxy:  "10.200.0.1:3000",
	})).CoreV1().Nodes()
	inv := NewHostNodeInventory(nodes, nil, nil)
	entry, ok := inv.Host("host-7")
	if !ok {
		t.Fatal("the host's node was not read")
	}
	if entry.Services.Buildkit != "tcp://10.200.0.1:1234" || entry.Services.GoProxy != "http://10.200.0.1:3000" {
		t.Errorf("services = %+v", entry.Services)
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
//# from the annotations of KF-179 on the Node of the Job's Host.

// TestJobOverAgentExec runs a whole Job through gitlab-runner's Build with
// the production transport factory and client pool, against a real API
// server and a real Exec Agent in front of a fake Host (agenttest). The
// Runner holds its ServiceAccount token and the agents' certificate
// authority and nothing else; its Host Registry fails the test if asked.
// The claim is a Bound claim of the Runner's, and the Host Service
// variables come from the annotations the agent published on the Host's
// Node.
func TestJobOverAgentExec(t *testing.T) {
	t.Parallel()
	env, err := agenttest.Start()
	if errors.Is(err, agenttest.ErrNoAssets) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	buildkitPort, _ := agenttest.ListenHostService(t)
	host := env.NewHost(t, agenttest.HostOptions{HostServices: map[string]int{ServiceBuildkit: buildkitPort}})
	ns := env.Namespace(t)
	runner := env.ServiceAccountToken(t, ns, "runner")
	env.PutClaim(t, ns, "job", runner.User, agenttest.ClaimStatus{
		Phase: agent.ClaimBound, VMUID: host.VMUID, HostNode: host.Node, ExpiresAt: time.Now().Add(time.Hour),
	})
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(runner.Token), 0o600); err != nil {
		t.Fatal(err)
	}
	agents, err := transport.NewAgentHosts(transport.AgentExecConfig{CAFile: env.Certs.CAFile, TokenFile: tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agents.Close() })
	admin, err := kubernetes.NewForConfig(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	agenttest.Eventually(t, "the host service annotations", func() bool {
		return host.ReadNode().Annotations["host-service.gitlab-runner.flintlock.dev/buildkit"] != ""
	})

	f := newFixture(t)
	f.deps.Hosts = tripwireRegistry{t: t}
	f.deps.Transports = transport.NewFactory()
	f.deps.Env = NewHostServiceEnv(config.HostServices{})
	f.deps.Inventory = NewHostNodeInventory(admin.CoreV1().Nodes(), nil, nil)
	f.opts = append(f.opts, WithAgentExec(agents))
	profile := testProfile()
	root := t.TempDir()
	profile.Shell, profile.User = "bash", "root"
	profile.BuildsDir, profile.CacheDir = root+"/builds", root+"/cache"
	f.sched.profile = profile
	f.sched.tune = func(a *scheduler.Allocation) {
		a.VMUID, a.Placement.Host = host.VMUID, host.Node
		a.Host = poolmgr.HostRef{Name: host.Node, Address: host.Address}
	}

	job := testJob()
	job.Steps[0].Script = spec.StepScript{`echo "$JOB_SECRET_VALUE"`, `echo "buildkit at $BUILDKIT_HOST"`, `pwd`}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	trace, _, err := f.runBuild(ctx, job)
	if err != nil {
		t.Fatalf("the job failed: %v\n%s\n%s", err, trace.String(), host.Log())
	}
	log := trace.String()
	for _, want := range []string{
		"only-in-the-script",
		fmt.Sprintf("buildkit at tcp://%s:%d", agenttest.HostAddress, buildkitPort),
		root + "/builds/",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the job log lacks %q:\n%s", want, log)
		}
	}
}
