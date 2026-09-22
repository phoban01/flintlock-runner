package kubelet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/kubelet/kubelettest"
)

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The Pod Provider SHALL be tested against a Kubernetes API
//# server test environment and the fake Host, with no kubelet, no KVM and no
//# AWS.

// The scenarios in this package run the real provider, Run and all, against
// a real kube-apiserver and etcd started by controller-runtime's envtest and
// against the fake Host. Nothing else exists: no kubelet, no scheduler, no
// controller manager, no KVM and no AWS. A test binds its pods by name,
// creates the Host's Node by hand and, where a scenario needs one, plays the
// Host's kubelet itself.
//
// The binaries come from `make envtest`, which `make test` runs and
// which sets KUBEBUILDER_ASSETS. Without them the scenarios are skipped, or
// fail when FLINTLOCK_RUNNER_REQUIRE_ENVTEST is set, as it is in CI.

const (
	agentNamespace = kubelettest.AgentNamespace
	rbacManifest   = "../../deploy/host-agent/rbac.yaml"
	// policyManifest is loaded as well, so that every scenario runs the
	// provider under the admission policy it ships with (KF-133).
	policyManifest = "../../deploy/host-agent/admission-policy.yaml"
	waitTimeout    = 30 * time.Second
	hostAddress    = "10.1.2.3"
	guestAddress   = "10.200.0.7"
)

// suite is the shared API server, nil when there is none. Its admin client
// plays the scheduler, the Runner, the Host's kubelet and the operator; each
// fixture's provider runs as suite.AgentFor its Host, holding exactly the
// shipped RBAC and subject to the shipped admission policy.
var suite *kubelettest.Environment

func TestMain(m *testing.M) {
	env, err := kubelettest.Start(rbacManifest, kubelettest.WithAdmissionPolicy(policyManifest))
	switch {
	case errors.Is(err, kubelettest.ErrNoAssets):
		// Every scenario skips.
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	default:
		suite = env
	}
	code := m.Run()
	if suite != nil {
		_ = suite.Stop()
	}
	os.Exit(code)
}

// fixture is one Host: its Node, its fake flintlockd and its provider.
type fixture struct {
	t         *testing.T
	hostNode  string
	vnode     string
	namespace string
	cfg       *Config
	fake      *hostfake.Host
	host      *gatedHost
	certs     *hostfake.TestCerts
	logs      *syncBuffer

	addr   string
	cancel context.CancelFunc
	done   chan error
}

var fixtureSeq atomic.Int64

// newFixture creates the Host's Node, a namespace for Pool pods and the fake
// Host. It does not start the provider.
func newFixture(t *testing.T, fakeCfg flintlock.FakeHostConfig) *fixture {
	t.Helper()
	if suite == nil {
		t.Skip("no API server test environment: run `make envtest` and set KUBEBUILDER_ASSETS")
	}
	ctx := context.Background()
	id := fixtureSeq.Add(1)
	f := &fixture{
		t:         t,
		hostNode:  fmt.Sprintf("host-%d", id),
		namespace: fmt.Sprintf("runners-%d", id),
		logs:      &syncBuffer{},
	}
	f.vnode = VirtualNodeName(f.hostNode)

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: f.hostNode,
		Labels: map[string]string{
			labelArch:                     "amd64",
			kubelabels.LabelHost:          "true",
			kubelabels.Prefix + "image":   "sha-0a1b2c",
			"example.com/not-this-one":    "x",
			"node.kubernetes.io/instance": "metal",
		},
	}}
	node, err := suite.Admin.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the host's node: %v", err)
	}
	node.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: hostAddress}}
	if _, err := suite.Admin.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("setting the host's node status: %v", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.namespace}}
	if _, err := suite.Admin.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the namespace: %v", err)
	}

	fakeCfg.Name, fakeCfg.SandboxRoot, fakeCfg.ExecEnabled = f.hostNode, t.TempDir(), true
	f.fake = hostfake.New(fakeCfg)
	t.Cleanup(func() { _ = f.fake.Close() })
	f.host = &gatedHost{PoolHostClient: f.fake.Client(), held: map[string]bool{}}

	if f.certs, err = hostfake.WriteTestCerts(t.TempDir()); err != nil {
		t.Fatalf("writing certificates: %v", err)
	}
	f.cfg = &Config{
		HostNode:            f.hostNode,
		MicroVMNamespace:    "flr-" + f.hostNode,
		HostReserve:         Reserve{CPU: "2", Memory: "4Gi"},
		MaxMicroVMs:         8,
		NotReadyDir:         filepath.Join(t.TempDir(), "not-ready.d"),
		Listen:              "127.0.0.1:0",
		TLS:                 ServerTLS{CertFile: f.certs.ServerCertFile, KeyFile: f.certs.ServerKeyFile, ClientCAFile: f.certs.CAFile},
		LeaseDuration:       time.Minute,
		DrainTimeout:        time.Hour,
		Guard:               Guard{Namespace: agentNamespace},
		SyncInterval:        50 * time.Millisecond,
		GuestAddressCommand: "echo " + guestAddress,
	}
	f.cfg.ApplyDefaults()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, line := range strings.Split(f.logs.String(), "\n") {
			if line != "" && !strings.Contains(line, "level=DEBUG") {
				t.Log(line)
			}
		}
	})
	t.Cleanup(f.stop)
	return f
}

// start runs the provider and waits until the Virtual Node is registered.
func (f *fixture) start() {
	f.t.Helper()
	listener, err := net.Listen("tcp", f.cfg.Listen)
	if err != nil {
		f.t.Fatal(err)
	}
	f.addr = listener.Addr().String()
	agent, err := suite.AgentFor(f.hostNode)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	f.cancel, f.done = cancel, make(chan error, 1)
	go func() {
		f.done <- Run(ctx, Options{
			Config: f.cfg, Kube: agent, Host: f.host, Version: "test", Listener: listener, Ready: ready,
			Logger: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
	}()
	select {
	case <-ready:
	case err := <-f.done:
		f.done <- err
		f.t.Fatalf("the provider stopped before it was ready: %v\n%s", err, f.logs.String())
	case <-time.After(waitTimeout):
		f.t.Fatalf("the provider was not ready in %s\n%s", waitTimeout, f.logs.String())
	}
}

// stop ends the provider, as a restart or a crash would, and waits for it.
func (f *fixture) stop() {
	if f.cancel == nil {
		return
	}
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(waitTimeout):
		f.t.Errorf("the provider did not stop in %s", waitTimeout)
	}
	f.cancel = nil
}

// podOption changes a pod before it is created.
type podOption func(*corev1.Pod)

func withState(state string) podOption {
	return func(p *corev1.Pod) { p.Labels[kubelabels.LabelState] = state }
}

func withLease(at time.Time) podOption {
	return func(p *corev1.Pod) { p.Annotations[kubelabels.AnnotationLease] = kubelabels.FormatLease(at) }
}

// createPod creates a MicroVM pod bound by name to the Virtual Node, which
// is what the scheduler would have done.
func (f *fixture) createPod(name string, opts ...podOption) *corev1.Pod {
	f.t.Helper()
	pod := microVMPodForTest()
	pod.ObjectMeta = metav1.ObjectMeta{
		Name: name, Namespace: f.namespace,
		Labels:      map[string]string{},
		Annotations: pod.Annotations,
	}
	grace := int64(1)
	pod.Spec.NodeName = f.vnode
	pod.Spec.TerminationGracePeriodSeconds = &grace
	pod.Spec.Tolerations = []corev1.Toleration{{Key: kubelabels.TaintMicroVM, Operator: corev1.TolerationOpExists}}
	for _, o := range opts {
		o(pod)
	}
	created, err := suite.Admin.CoreV1().Pods(f.namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("creating pod %s: %v", name, err)
	}
	return created
}

func (f *fixture) getPod(name string) (*corev1.Pod, error) {
	return suite.Admin.CoreV1().Pods(f.namespace).Get(context.Background(), name, metav1.GetOptions{})
}

// waitPod waits for a pod to satisfy a condition.
func (f *fixture) waitPod(name, what string, ok func(*corev1.Pod) bool) *corev1.Pod {
	f.t.Helper()
	var last *corev1.Pod
	f.eventually(fmt.Sprintf("pod %s: %s", name, what), func() bool {
		pod, err := f.getPod(name)
		if err != nil {
			return false
		}
		last = pod
		return ok(pod)
	})
	return last
}

func (f *fixture) waitRunning(name string) *corev1.Pod {
	f.t.Helper()
	return f.waitPod(name, "running and ready", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodRunning && podReady(p) })
}

func (f *fixture) waitGone(name string) {
	f.t.Helper()
	f.eventually("pod "+name+" is gone", func() bool {
		_, err := f.getPod(name)
		return apierrors.IsNotFound(err)
	})
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (f *fixture) eventually(what string, ok func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	f.t.Fatalf("timed out after %s waiting for: %s\nprovider log:\n%s", waitTimeout, what, f.logs.String())
}

// consistently asserts that ok holds for the whole of d.
func (f *fixture) consistently(what string, d time.Duration, ok func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !ok() {
			f.t.Fatalf("stopped holding: %s\nprovider log:\n%s", what, f.logs.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (f *fixture) virtualNode() *corev1.Node {
	f.t.Helper()
	node, err := suite.Admin.CoreV1().Nodes().Get(context.Background(), f.vnode, metav1.GetOptions{})
	if err != nil {
		f.t.Fatalf("reading the virtual node: %v", err)
	}
	return node
}

// microVMs lists the fake Host's MicroVMs in the provider's namespace.
func (f *fixture) microVMs() []*types.MicroVM {
	f.t.Helper()
	vms, err := f.fake.Client().ListMicroVMs(context.Background(), f.cfg.MicroVMNamespace)
	if err != nil {
		f.t.Fatalf("listing microvms: %v", err)
	}
	return vms
}

// microVMOf returns the MicroVM labelled with the pod's UID, or nil.
func (f *fixture) microVMOf(pod *corev1.Pod) *types.MicroVM {
	f.t.Helper()
	for _, vm := range f.microVMs() {
		if vm.GetSpec().GetLabels()[vmLabelPodUID] == string(pod.UID) {
			return vm
		}
	}
	return nil
}

// gatedHost is the fake Host's client with three switches a scenario needs
// and the fake does not have: exec refused for a while, deletes that take a
// while, as flintlockd's do, and a count of creates.
type gatedHost struct {
	flintlock.PoolHostClient

	mu         sync.Mutex
	execDown   bool
	holdDelete bool
	held       map[string]bool
	creates    int
}

func (g *gatedHost) set(change func(*gatedHost)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

func (g *gatedHost) created() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.creates
}

func (g *gatedHost) Exec(ctx context.Context) (flintlock.ExecStream, error) {
	g.mu.Lock()
	down := g.execDown
	g.mu.Unlock()
	if down {
		return nil, fmt.Errorf("%w: the guest agent does not answer yet", flintlock.ErrUnavailable)
	}
	return g.PoolHostClient.Exec(ctx)
}

func (g *gatedHost) CreateMicroVM(ctx context.Context, spec *types.MicroVMSpec) (*types.MicroVM, error) {
	g.mu.Lock()
	g.creates++
	g.mu.Unlock()
	return g.PoolHostClient.CreateMicroVM(ctx, spec)
}

// DeleteMicroVM accepts the delete and, while deletes are held, leaves the
// MicroVM listed, which is what flintlockd does while it tears one down.
func (g *gatedHost) DeleteMicroVM(ctx context.Context, uid string) error {
	g.mu.Lock()
	if g.holdDelete {
		g.held[uid] = true
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()
	return g.PoolHostClient.DeleteMicroVM(ctx, uid)
}

// releaseDeletes finishes every held delete.
func (g *gatedHost) releaseDeletes(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	g.holdDelete = false
	held := g.held
	g.held = map[string]bool{}
	g.mu.Unlock()
	for uid := range held {
		if err := g.PoolHostClient.DeleteMicroVM(context.Background(), uid); err != nil {
			t.Errorf("finishing the delete of %s: %v", uid, err)
		}
	}
}

// syncBuffer is a log sink safe to read while the provider writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
