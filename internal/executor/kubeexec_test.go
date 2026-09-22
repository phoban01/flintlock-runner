package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
	"github.com/phoban01/flintlock-runner/internal/transport/kubeexectest"
)

// tripwireRegistry is a Host Registry that fails the test whenever a Host
// client is asked of it: with kube-exec the Runner holds no connection to
// any Host (KF-063), so nothing may borrow or lease one.
type tripwireRegistry struct{ t *testing.T }

func (r tripwireRegistry) Get(name string) (flintlock.HostClient, error) {
	r.t.Errorf("the executor asked the host registry for host %s", name)
	return nil, errors.New("tripwire")
}

func (r tripwireRegistry) Lease(name string) (flintlock.HostClient, func(), error) {
	r.t.Errorf("the executor leased host %s from the host registry", name)
	return nil, nil, errors.New("tripwire")
}

func (tripwireRegistry) Endpoint(string) (flintlock.Endpoint, bool) {
	return flintlock.Endpoint{}, false
}
func (tripwireRegistry) Names() []string                                   { return nil }
func (tripwireRegistry) Apply(context.Context, []flintlock.Endpoint) error { return nil }
func (tripwireRegistry) Close() error                                      { return nil }

//= docs/requirements/12-cluster-fleet.md#kube-allocation
//= type=test
//# Where the Kubernetes pool backend is configured, the Executor
//# SHALL use the `kube-exec` Guest Transport for every Profile

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# Where the `kube-exec` Guest Transport is configured, the Runner
//# SHALL NOT open any connection to a Host.

// TestKubeExecForEveryProfile runs a Job whose Profile names ssh, with no
// key to read, on an executor told to use kube-exec for every Profile. The
// transport is built as kube-exec for the claimed pod, which is the Lease
// id, with no Host client, and the Host Registry is never asked for one:
// ssh with its missing key would have failed Prepare, and a lease would
// have tripped the registry.
func TestKubeExecForEveryProfile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.deps.Hosts = tripwireRegistry{t: t}
	profile := testProfile()
	profile.Transport = config.Transport{Kind: config.TransportSSH, SSH: config.SSHTransport{PrivateKeyFile: "/no/such/key"}}
	f.sched.profile = profile
	f.sched.tune = func(a *scheduler.Allocation) {
		a.Lease.ID = "pool-builders-x7k2q"
		a.Placement.Host = "host-7-microvms"
	}
	f.opts = append(f.opts, WithGuestTransport(transport.KindKubeExec))

	if _, _, err := f.runBuild(context.Background(), testJob()); err != nil {
		t.Fatalf("the job failed: %v", err)
	}
	f.factory.mu.Lock()
	defer f.factory.mu.Unlock()
	if len(f.factory.targets) != 1 {
		t.Fatalf("%d transports built, want 1", len(f.factory.targets))
	}
	target := f.factory.targets[0]
	if target.Kind != transport.KindKubeExec || target.VMUID != "pool-builders-x7k2q" || target.Host != nil {
		t.Errorf("target = kind %q, vm %q, host %v; want kube-exec for pod pool-builders-x7k2q with no host client",
			target.Kind, target.VMUID, target.Host)
	}
}

// virtualNode is a Virtual Node as the Pod Provider publishes it (KF-017):
// one annotation per enabled Host Service, `address:port` on the bridge
// gateway.
func virtualNode(name string, services map[string]string) *corev1.Node {
	annotations := map[string]string{"unrelated.example.com/note": "x"}
	for service, addr := range services {
		annotations[kubelabels.HostServiceAnnotation(service)] = addr
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# The Executor SHALL read the Host Service addresses for a Job
//# from the annotations of the Virtual Node named in the Placement.

// TestHostServicesFromTheVirtualNode runs Prepare on a cluster fleet, whose
// Runner has no Inventory: the Host Service variables come from the
// annotations of the Virtual Node the Placement names, not from any other
// node, in the forms a provisioned Host's Inventory entry has. A service
// whose annotation is missing or malformed gets no variables, and neither
// does a Job placed on a Virtual Node that cannot be read (EX-062).
func TestHostServicesFromTheVirtualNode(t *testing.T) {
	t.Parallel()
	cfg := hostServicesConfig()
	nodes := kubefake.NewClientset(
		virtualNode("host-7-microvms", map[string]string{
			ServiceBuildkit:       "10.200.0.1:1234",
			ServiceGoProxy:        "10.200.0.1:3000",
			ServiceRegistryMirror: "not an address",
			ServiceHTTPCache:      "10.200.0.1:3128",
		}),
		virtualNode("host-8-microvms", map[string]string{ServiceBuildkit: "10.200.0.9:1234"}),
	).CoreV1().Nodes()

	run := func(placement string) (map[string]string, string) {
		f := newFixture(t)
		f.deps.Hosts = tripwireRegistry{t: t}
		f.deps.Env = NewHostServiceEnv(cfg)
		f.deps.Inventory = NewVirtualNodeInventory(nodes, cfg.HTTPCache.Upstreams, nil)
		f.opts = append(f.opts, WithHTTPCacheUpstreams(cfg.HTTPCache.Upstreams), WithGuestTransport(transport.KindKubeExec))
		f.sched.tune = func(a *scheduler.Allocation) { a.Placement.Host = placement }
		e, trace, err := f.prepareOnly(context.Background(), testJob())
		if err != nil {
			t.Fatalf("Prepare on %s: %v", placement, err)
		}
		defer e.Cleanup()
		vars := map[string]string{}
		for _, v := range e.Build.Variables {
			vars[v.Key] = v.Value
		}
		return vars, trace.String()
	}

	vars, log := run("host-7-microvms")
	want := map[string]string{
		"BUILDKIT_HOST": "tcp://10.200.0.1:1234",
		"GOPROXY":       "http://10.200.0.1:3000",
		"GOFLAGS":       "-modcacherw",
		"NODEJS_MIRROR": "http://10.200.0.1:3128/nodejs",
		"PIP_INDEX_URL": "http://10.200.0.1:3128/pypi",
	}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %q, want %q from the virtual node's annotation", k, vars[k], v)
		}
	}
	if v, ok := vars["CI_REGISTRY_MIRROR"]; ok {
		t.Errorf("CI_REGISTRY_MIRROR = %q from a malformed annotation, want it left out", v)
	}
	if !strings.Contains(log, "buildkit") || strings.Contains(log, "registry_mirror") {
		t.Errorf("the prepare section does not name exactly the services made available:\n%s", log)
	}

	vars, _ = run("host-8-microvms")
	if vars["BUILDKIT_HOST"] != "tcp://10.200.0.9:1234" || vars["GOPROXY"] != "" {
		t.Errorf("on host-8: BUILDKIT_HOST = %q, GOPROXY = %q; want only host-8's buildkit", vars["BUILDKIT_HOST"], vars["GOPROXY"])
	}

	vars, _ = run("no-such-node-microvms")
	for _, k := range HostServiceVarNames {
		if v, ok := vars[k]; ok {
			t.Errorf("%s = %q on a virtual node that cannot be read, want no host service variables", k, v)
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# Where the `kube-exec` Guest Transport is configured, the
//# Executor SHALL run each Stage by opening the exec subresource of the
//# claimed pod through the Kubernetes API, with the Stage script on standard
//# input.

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# The Executor SHALL read the Host Service addresses for a Job
//# from the annotations of the Virtual Node named in the Placement.

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# Where the `kube-exec` Guest Transport is configured, the Runner
//# SHALL NOT open any connection to a Host.

// TestJobOverKubeExec runs a whole Job through gitlab-runner's Build with
// the production transport factory, against a real API server and the real
// Pod Provider in front of a fake Host (kubeexectest). The Runner's side
// holds the Runner's shipped RBAC and the API server's address and nothing
// else: its Host Registry fails the test if a Host is asked of it. Every
// Stage the Build generates is run over the claimed pod's exec
// subresource, the Host Service variables come from the annotations the
// provider published on its Virtual Node, and the Job's output reaches the
// Job log.
func TestJobOverKubeExec(t *testing.T) {
	t.Parallel()
	env, err := kubeexectest.Start()
	if errors.Is(err, kubeexectest.ErrNoAssets) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	buildkitPort := kubeexectest.ListenHostService(t)
	host := env.NewHost(t, kubeexectest.HostOptions{HostServices: map[string]int{ServiceBuildkit: buildkitPort}})
	ns := env.Namespace(t)
	pod := host.Pod(ns, "pool-builders-x7k2q")
	runner := env.RunnerConfig(t, ns)
	client, err := kubernetes.NewForConfig(runner)
	if err != nil {
		t.Fatal(err)
	}

	f := newFixture(t)
	f.deps.Hosts = tripwireRegistry{t: t}
	f.deps.Transports = transport.NewFactory(transport.WithKubeExec(runner, ns))
	f.deps.Env = NewHostServiceEnv(config.HostServices{})
	f.deps.Inventory = NewVirtualNodeInventory(client.CoreV1().Nodes(), nil, nil)
	f.opts = append(f.opts, WithGuestTransport(transport.KindKubeExec))
	// The generated scripts name the builds directory absolutely and run on
	// this machine, so on the fake Host it is a directory of the test's, as
	// the harness's fake tier has it.
	profile := testProfile()
	root := t.TempDir()
	profile.Shell, profile.User = "bash", "root"
	profile.BuildsDir, profile.CacheDir = root+"/builds", root+"/cache"
	f.sched.profile = profile
	f.sched.tune = func(a *scheduler.Allocation) {
		a.VMUID, a.Lease.ID, a.Placement.Host = string(pod.UID), pod.Name, host.VirtualNode
	}

	job := testJob()
	job.Steps[0].Script = spec.StepScript{`echo "$JOB_SECRET_VALUE"`, `echo "buildkit at $BUILDKIT_HOST"`, `pwd`}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	trace, _, err := f.runBuild(ctx, job)
	if err != nil {
		t.Fatalf("the job failed: %v\n%s", err, trace.String())
	}
	log := trace.String()
	for _, want := range []string{
		"only-in-the-script",
		fmt.Sprintf("buildkit at tcp://%s:%d", kubeexectest.HostAddress, buildkitPort),
		root + "/builds/",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the job log lacks %q:\n%s", want, log)
		}
	}
}
