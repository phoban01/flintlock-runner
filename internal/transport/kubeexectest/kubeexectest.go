// Package kubeexectest is the test environment of the kube-exec Guest
// Transport (docs/requirements/12-cluster-fleet.md#kube-exec-transport): a
// real kube-apiserver and etcd from controller-runtime's envtest, configured
// the way a cluster's API server is to reach its kubelets, and on each fake
// Host the real Pod Provider (internal/kubelet) in front of the fake
// flintlockd (internal/flintlock/fake). An exec opened by the transport goes
// the whole way a production one does: the Runner's client to the API
// server, the API server's kubelet client to the provider's TLS kubelet
// endpoint, and the provider to MicroVMExec on the fake Host. The package is
// imported by tests only.
package kubeexectest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/kubelet"
)

const (
	// AssetsVar names the directory of kube-apiserver and etcd, which
	// `make envtest` downloads and prints.
	AssetsVar = "KUBEBUILDER_ASSETS"
	// RequireVar names the variable that, when set, makes a missing
	// environment a failure instead of a skip. CI sets it.
	RequireVar = "FLINTLOCK_RUNNER_REQUIRE_ENVTEST"
	// HostAddress is the internal address of every fake Host's Node, and so
	// of its Virtual Node: the address the API server dials the Pod
	// Provider's kubelet endpoint on, which listens there.
	HostAddress = "127.0.0.1"
	// WaitTimeout bounds every wait of the fixture.
	WaitTimeout = 30 * time.Second
	// agentNamespace holds the drain guard of every provider; nothing in
	// these tests drains, but the provider's configuration names one.
	agentNamespace = "flintlock-system"
	// labelPodUID is the MicroVM label the Pod Provider records its pod's
	// UID in (KF-021).
	labelPodUID = kubelabels.Prefix + "pod-uid"
)

// ErrNoAssets is returned by Start when AssetsVar is unset and RequireVar is
// not: the caller skips.
var ErrNoAssets = errors.New("kubeexectest: no API server test environment: run `make envtest` and set " + AssetsVar)

// Env is one running API server that can reach the Pod Providers the tests
// start.
type Env struct {
	env *envtest.Environment
	// Config is the administrator's configuration.
	Config *rest.Config
	// Admin is the administrator's client. It plays the scheduler that binds
	// pods and the operator; the Pod Providers use it too, because what they
	// may do is the provider's own tests' concern.
	Admin kubernetes.Interface

	certs *hostfake.TestCerts
	dir   string

	users sync.Mutex
	seq   atomic.Int64
}

// Start starts kube-apiserver and etcd, configured as a cluster's API server
// is to reach its kubelets: a client certificate to present to them, the
// authority their serving certificates are checked against, and the
// internal address of a Node as the only address it dials, because a
// Virtual Node's host name resolves nowhere. One certificate authority
// issues all three sides here: the API server's kubelet client certificate,
// which every Pod Provider requires (KF-031), and every provider's serving
// certificate, which names HostAddress.
func Start() (*Env, error) {
	dir := os.Getenv(AssetsVar)
	if dir == "" {
		if os.Getenv(RequireVar) != "" {
			return nil, fmt.Errorf("kubeexectest: %s is set and %s is not: run `make envtest`", RequireVar, AssetsVar)
		}
		return nil, ErrNoAssets
	}
	certDir, err := os.MkdirTemp("", "kubeexectest-")
	if err != nil {
		return nil, err
	}
	certs, err := hostfake.WriteTestCerts(certDir)
	if err != nil {
		_ = os.RemoveAll(certDir)
		return nil, err
	}

	env := &envtest.Environment{BinaryAssetsDirectory: dir}
	env.ControlPlane.GetAPIServer().Configure().
		Set("kubelet-client-certificate", certs.ClientCertFile).
		Set("kubelet-client-key", certs.ClientKeyFile).
		Set("kubelet-certificate-authority", certs.CAFile).
		Set("kubelet-preferred-address-types", string(corev1.NodeInternalIP))
	cfg, err := env.Start()
	if err != nil {
		_ = os.RemoveAll(certDir)
		return nil, fmt.Errorf("kubeexectest: starting the API server test environment: %w", err)
	}
	e := &Env{env: env, Config: cfg, certs: certs, dir: certDir}
	if e.Admin, err = kubernetes.NewForConfig(cfg); err == nil {
		_, err = e.Admin.CoreV1().Namespaces().Create(context.Background(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agentNamespace}}, metav1.CreateOptions{})
	}
	if err != nil {
		_ = e.Stop()
		return nil, err
	}
	return e, nil
}

// Stop stops the API server and etcd and removes the certificates.
func (e *Env) Stop() error {
	err := e.env.Stop()
	_ = os.RemoveAll(e.dir)
	return err
}

// Namespace creates a namespace of its own for a test, standing for the
// Runner's.
func (e *Env) Namespace(t testing.TB) string {
	t.Helper()
	name := fmt.Sprintf("runners-%d", e.seq.Add(1))
	_, err := e.Admin.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
	return name
}

// RunnerConfig is the client configuration of a new user bound to the
// Runner's shipped RBAC (deploy/runner/role.yaml) in namespace and to
// nothing else, so that a call the kube-exec transport or the Executor makes
// outside it is refused by the API server and fails the test (KF-110).
func (e *Env) RunnerConfig(t testing.TB, namespace string) *rest.Config {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("%s-runner-%d", namespace, e.seq.Add(1))
	e.users.Lock()
	user, err := e.env.AddUser(envtest.User{Name: name}, nil)
	e.users.Unlock()
	if err != nil {
		t.Fatalf("adding user %s: %v", name, err)
	}
	subject := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}}

	role, clusterRole := runnerRoles(t)
	role.Namespace = namespace
	if _, err := e.Admin.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	if _, err := e.Admin.RbacV1().ClusterRoles().Create(ctx, clusterRole, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	if _, err := e.Admin.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   subject,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   subject,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole.Name},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The authoriser learns of a binding through its own watch, so the
	// first moments after creating one can still be refused.
	cfg := user.Config()
	client := kubernetes.NewForConfigOrDie(cfg)
	eventually(t, "the runner's role binding takes effect", func() bool {
		_, podsErr := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{Limit: 1})
		_, nodesErr := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
		return podsErr == nil && nodesErr == nil
	}, nil)
	return cfg
}

// runnerRoles reads the Role and the ClusterRole of deploy/runner/role.yaml.
func runnerRoles(t testing.TB) (*rbacv1.Role, *rbacv1.ClusterRole) {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "deploy", "runner", "role.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var (
		role        *rbacv1.Role
		clusterRole *rbacv1.ClusterRole
	)
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		// A Role is a ClusterRole without an aggregation rule, so one type
		// reads both documents.
		var raw rbacv1.ClusterRole
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		switch raw.Kind {
		case "Role":
			role = &rbacv1.Role{ObjectMeta: raw.ObjectMeta, Rules: raw.Rules}
		case "ClusterRole":
			clusterRole = &raw
		case "":
		default:
			t.Fatalf("%s: unexpected kind %q", path, raw.Kind)
		}
	}
	if role == nil || clusterRole == nil {
		t.Fatalf("%s: want a Role and a ClusterRole", path)
	}
	return role, clusterRole
}

// moduleRoot is the directory of go.mod, found from this file.
func moduleRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("kubeexectest: cannot locate the source tree")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("kubeexectest: no go.mod above " + file)
		}
		dir = parent
	}
}

// HostOptions shape one fake Host.
type HostOptions struct {
	// HostServices are the Host Services the provider publishes on its
	// Virtual Node (KF-017), by name, each listening on HostAddress at the
	// given port. The provider reports its node ready only while each one
	// accepts connections (KF-015), so the fixture listens on each.
	HostServices map[string]int
	// BootDelay is how long each MicroVM stays PENDING.
	BootDelay time.Duration
}

// Host is one fake Host: its Node, its fake flintlockd and the Pod Provider
// that registered its Virtual Node.
type Host struct {
	env *Env
	t   testing.TB
	// Node is the Host's own Node, VirtualNode the provider's.
	Node        string
	VirtualNode string
	// Fake is the fake flintlockd.
	Fake *hostfake.Host
	logs *syncBuffer
}

// NewHost creates a Host's Node at HostAddress, a fake flintlockd behind it
// and a Pod Provider serving its kubelet endpoint on HostAddress, and waits
// until the provider has registered the Virtual Node. Everything stops when
// the test ends.
func (e *Env) NewHost(t testing.TB, opts HostOptions) *Host {
	t.Helper()
	ctx := context.Background()
	id := e.seq.Add(1)
	h := &Host{env: e, t: t, Node: fmt.Sprintf("host-%d", id), logs: &syncBuffer{}}
	h.VirtualNode = kubelet.VirtualNodeName(h.Node)

	node, err := e.Admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   h.Node,
		Labels: map[string]string{kubelabels.LabelHost: "true", corev1.LabelArchStable: runtime.GOARCH},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the host's node: %v", err)
	}
	node.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: HostAddress}}
	if _, err := e.Admin.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("setting the host's node status: %v", err)
	}

	h.Fake = hostfake.New(flintlock.FakeHostConfig{
		Name: h.Node, SandboxRoot: t.TempDir(), ExecEnabled: true, BootDelay: opts.BootDelay,
	})
	t.Cleanup(func() { _ = h.Fake.Close() })

	services := map[string]kubelet.HostService{}
	for name, port := range opts.HostServices {
		services[name] = kubelet.HostService{Enabled: true, Port: port}
	}
	cfg := &kubelet.Config{
		HostNode:            h.Node,
		MicroVMNamespace:    "flr-" + h.Node,
		HostReserve:         kubelet.Reserve{CPU: "2", Memory: "4Gi"},
		MaxMicroVMs:         8,
		BridgeGateway:       HostAddress,
		HostServices:        services,
		NotReadyDir:         filepath.Join(t.TempDir(), "not-ready.d"),
		Listen:              net.JoinHostPort(HostAddress, "0"),
		TLS:                 kubelet.ServerTLS{CertFile: e.certs.ServerCertFile, KeyFile: e.certs.ServerKeyFile, ClientCAFile: e.certs.CAFile},
		LeaseDuration:       time.Hour,
		DrainTimeout:        time.Hour,
		Guard:               kubelet.Guard{Namespace: agentNamespace},
		SyncInterval:        50 * time.Millisecond,
		GuestAddressCommand: "echo 10.200.0.7",
	}
	cfg.ApplyDefaults()

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- kubelet.Run(runCtx, kubelet.Options{
			Config: cfg, Kube: e.Admin, Host: h.Fake.Client(), Version: "test", Listener: listener, Ready: ready,
			Logger: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(WaitTimeout):
			t.Errorf("the pod provider of %s did not stop in %s", h.Node, WaitTimeout)
		}
		if t.Failed() {
			t.Logf("pod provider log of %s:\n%s", h.Node, h.logs.String())
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the pod provider stopped before it was ready: %v\n%s", err, h.logs.String())
	case <-time.After(WaitTimeout):
		t.Fatalf("the pod provider was not ready in %s\n%s", WaitTimeout, h.logs.String())
	}
	return h
}

// ListenHostService listens on a free port of HostAddress and accepts
// connections until the test ends, standing in for a Host Service that the
// Pod Provider probes before it reports its node ready (KF-015). It returns
// the port.
func ListenHostService(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(HostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, portText, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portText)
	return port
}

// Pod creates a claimed MicroVM pod with CreatePod and waits until the
// provider reports it running and ready, which it does only once a command
// has run in the guest (KF-024).
func (h *Host) Pod(namespace, name string) *corev1.Pod {
	h.t.Helper()
	created := h.CreatePod(namespace, name)
	var last *corev1.Pod
	eventually(h.t, "pod "+name+" running and ready", func() bool {
		p, err := h.env.Admin.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		last = p
		return p.Status.Phase == corev1.PodRunning && podReady(p)
	}, h.logs)
	if last == nil {
		return created
	}
	return last
}

// CreatePod creates a MicroVM pod in namespace bound by name to the Virtual
// Node, as the scheduler would have bound a Pool pod, and marks it claimed,
// as a claim would have. It does not wait for the pod to run.
func (h *Host) CreatePod(namespace, name string) *corev1.Pod {
	h.t.Helper()
	noToken := false
	grace := int64(1)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{kubelabels.LabelState: kubelabels.StateClaimed},
			Annotations: map[string]string{
				kubelabels.AnnotationKernelImage: "ghcr.io/example/kernel:6.1",
				kubelabels.AnnotationHypervisor:  "firecracker",
				kubelabels.AnnotationLease:       kubelabels.FormatLease(time.Now()),
			},
		},
		Spec: corev1.PodSpec{
			NodeName:                      h.VirtualNode,
			AutomountServiceAccountToken:  &noToken,
			TerminationGracePeriodSeconds: &grace,
			Tolerations:                   []corev1.Toleration{{Key: kubelabels.TaintMicroVM, Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:  "microvm",
				Image: "ghcr.io/example/rootfs:ubuntu",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				}},
			}},
		},
	}
	created, err := h.env.Admin.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		h.t.Fatalf("creating pod %s: %v", name, err)
	}
	return created
}

// Sandbox is the fake Host's sandbox directory of the pod's MicroVM: the
// guest's root, as the fake Host plays it.
func (h *Host) Sandbox(pod *corev1.Pod) string {
	h.t.Helper()
	vms, err := h.Fake.Client().ListMicroVMs(context.Background(), "")
	if err != nil {
		h.t.Fatalf("listing microvms: %v", err)
	}
	for _, vm := range vms {
		if vm.GetSpec().GetLabels()[labelPodUID] != string(pod.UID) {
			continue
		}
		path, ok := h.Fake.SandboxPath(vm.GetSpec().GetUid())
		if !ok {
			h.t.Fatalf("microvm %s of pod %s has no sandbox", vm.GetSpec().GetUid(), pod.Name)
		}
		return path
	}
	h.t.Fatalf("no microvm is labelled with pod %s", pod.Name)
	return ""
}

// Log is what the Pod Provider has logged so far.
func (h *Host) Log() string { return h.logs.String() }

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// eventually polls ok until it holds or WaitTimeout passes.
func eventually(t testing.TB, what string, ok func() bool, logs *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(WaitTimeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	if logs != nil {
		t.Fatalf("timed out after %s waiting for: %s\npod provider log:\n%s", WaitTimeout, what, logs.String())
	}
	t.Fatalf("timed out after %s waiting for: %s", WaitTimeout, what)
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
