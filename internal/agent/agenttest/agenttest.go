// Package agenttest is the test environment of the Exec Agent and of the
// `agent-exec` Guest Transport (KF-190): a real kube-apiserver and etcd,
// started by controller-runtime's envtest, serving the provisional test
// definition of the claim resource, with the agent's shipped RBAC and
// admission policy applied; and, per Host, a Node, a fake flintlockd
// (internal/flintlock/fake) served over gRPC on loopback and a real Exec
// Agent in front of it, authenticating with a real bound ServiceAccount
// token of a Host Agent pod on that Node. There is no kubelet, no KVM and
// no battery: a test plays battery by writing claims itself.
package agenttest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/phoban01/flintlock-runner/internal/agent"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

const (
	// AssetsVar names the directory of kube-apiserver and etcd, which
	// `make envtest` downloads and prints.
	AssetsVar = "KUBEBUILDER_ASSETS"
	// RequireVar names the variable that, when set, makes a missing
	// environment a failure instead of a skip. CI sets it.
	RequireVar = "FLINTLOCK_RUNNER_REQUIRE_ENVTEST"
	// AgentNamespace is the namespace of the subject of
	// deploy/agent/rbac.yaml, where the guard pods live.
	AgentNamespace = "flintlock-system"
	// AgentServiceAccount is the ServiceAccount the manifest binds: the
	// Host Agent's, because the Exec Agent is a container of that pod.
	AgentServiceAccount = "flintlock-host-agent"
	// AgentUser is the user name of AgentServiceAccount.
	AgentUser = "system:serviceaccount:" + AgentNamespace + ":" + AgentServiceAccount
	// HostAddress is the internal address of every Host's Node here, which
	// is where each Exec Agent listens and what the fake Host's test
	// certificate names.
	HostAddress = "127.0.0.1"
	// WaitTimeout bounds every wait of the environment.
	WaitTimeout = 30 * time.Second
)

// ErrNoAssets is returned by Start when AssetsVar is unset and RequireVar is
// not: the caller skips.
var ErrNoAssets = errors.New("agenttest: no API server test environment: run `make envtest` and set " + AssetsVar)

// Env is one running API server.
type Env struct {
	env *envtest.Environment
	// Config is the administrator's configuration.
	Config *rest.Config
	// Admin is the test's own client: it plays battery, the Host's kubelet
	// and the operator.
	Admin kubernetes.Interface
	// Dynamic is the administrator's client of the claim resource.
	Dynamic dynamic.Interface
	// Certs are the serving certificates of every Exec Agent, for
	// HostAddress, and their certificate authority.
	Certs *hostfake.TestCerts

	dir string
	seq atomic.Int64
}

// ModuleRoot is the directory of go.mod: this file is three directories
// below it.
func ModuleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//# The Exec Agent and the `agent-exec` Guest Transport SHALL be
//# tested against a Kubernetes API server test environment serving the
//# claim resources from a test definition, and the fake Host, with no KVM
//# and no battery.

// Start starts kube-apiserver and etcd with the claim resource's test
// definition installed, deploy/agent/rbac.yaml and
// deploy/agent/admission-policy.yaml applied unchanged, and waits until the
// API server enforces the policy.
func Start() (*Env, error) {
	if os.Getenv(AssetsVar) == "" {
		if os.Getenv(RequireVar) != "" {
			return nil, fmt.Errorf("agenttest: %s is set and %s is not: run `make envtest`", RequireVar, AssetsVar)
		}
		return nil, ErrNoAssets
	}
	root := ModuleRoot()
	dir, err := os.MkdirTemp("", "agenttest-")
	if err != nil {
		return nil, err
	}
	certs, err := hostfake.WriteTestCerts(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "internal", "agent", "testdata", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("agenttest: starting the API server test environment: %w", err)
	}
	e := &Env{env: env, Config: cfg, Certs: certs, dir: dir}
	ctx := context.Background()
	if e.Admin, err = kubernetes.NewForConfig(cfg); err == nil {
		e.Dynamic, err = dynamic.NewForConfig(cfg)
	}
	if err == nil {
		_, err = e.Admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: AgentNamespace}}, metav1.CreateOptions{})
	}
	if err == nil {
		_, err = e.Admin.CoreV1().ServiceAccounts(AgentNamespace).Create(ctx,
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: AgentServiceAccount}}, metav1.CreateOptions{})
	}
	for _, manifest := range []string{"rbac.yaml", "admission-policy.yaml"} {
		if err == nil {
			err = e.Apply(ctx, filepath.Join(root, "deploy", "agent", manifest))
		}
	}
	if err == nil {
		err = e.awaitPolicy(ctx)
	}
	if err != nil {
		_ = env.Stop()
		_ = os.RemoveAll(dir)
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

// Apply creates every object of a manifest as the administrator.
func (e *Env) Apply(ctx context.Context, manifest string) error {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(e.Config)
	if err != nil {
		return err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))
	for _, doc := range strings.Split(string(data), "\n---") {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			return fmt.Errorf("%s: %w", manifest, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", manifest, gvk.Kind, err)
		}
		var client dynamic.ResourceInterface = e.Dynamic.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			client = e.Dynamic.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		}
		if _, err := client.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("%s: %s %s: %w", manifest, gvk.Kind, obj.GetName(), err)
		}
	}
	return nil
}

// awaitPolicy waits until the admission policy refuses what it has to: an
// Exec Agent labelling a Node, tried as a dry run.
func (e *Env) awaitPolicy(ctx context.Context) error {
	cfg := rest.CopyConfig(e.Config)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: AgentUser,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + AgentNamespace, "system:authenticated"},
		Extra:    map[string][]string{agent.NodeNameExtra: {"agenttest-policy-probe"}},
	}
	probe, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	if _, err := e.Admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "agenttest-policy-probe"}}, metav1.CreateOptions{}); err != nil {
		return err
	}
	deadline := time.Now().Add(WaitTimeout)
	for {
		_, err := probe.CoreV1().Nodes().Patch(ctx, "agenttest-policy-probe", k8stypes.MergePatchType,
			[]byte(`{"metadata":{"labels":{"probe":"x"}}}`), metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
		if IsPolicyDenial(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agenttest: the admission policy was not enforced within %s: the probe got %w", WaitTimeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// IsPolicyDenial reports whether err is a refusal by a
// ValidatingAdmissionPolicy, as opposed to one by RBAC or anything else.
func IsPolicyDenial(err error) bool {
	return err != nil && apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy")
}

// Namespace creates a namespace of its own for a test, standing for a
// Runner's.
func (e *Env) Namespace(t testing.TB) string {
	t.Helper()
	name := fmt.Sprintf("runners-%d", e.seq.Add(1))
	if _, err := e.Admin.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
	return name
}

// Identity is a ServiceAccount with a real token of its own.
type Identity struct {
	// User is the user name the API server authenticates the token as.
	User string
	// Token is the token.
	Token string
}

// ServiceAccountToken creates a ServiceAccount and requests a token for
// it, standing for a Runner's projected token. The token is bound to no
// object; a TokenReview authenticates it all the same.
func (e *Env) ServiceAccountToken(t testing.TB, namespace, name string) Identity {
	t.Helper()
	ctx := context.Background()
	_, err := e.Admin.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating service account %s/%s: %v", namespace, name, err)
	}
	return Identity{User: "system:serviceaccount:" + namespace + ":" + name, Token: e.token(t, namespace, name, nil)}
}

// token requests a token of a ServiceAccount, bound to the object when one
// is given.
func (e *Env) token(t testing.TB, namespace, name string, bound *authenticationv1.BoundObjectReference) string {
	t.Helper()
	expiry := int64(3600)
	tok, err := e.Admin.CoreV1().ServiceAccounts(namespace).CreateToken(context.Background(), name,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry, BoundObjectRef: bound}},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("requesting a token of %s/%s: %v", namespace, name, err)
	}
	return tok.Status.Token
}

// AgentConfig is the client configuration of the Exec Agent of the Host
// whose Node is hostNode: the bound ServiceAccount token of a Host Agent pod
// on that Node, which is what the kubelet projects into the pod and what
// `flr agent` authenticates with in-cluster, and nothing else.
func (e *Env) AgentConfig(t testing.TB, hostNode string) *rest.Config {
	t.Helper()
	ctx := context.Background()
	pod, err := e.Admin.CoreV1().Pods(AgentNamespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "host-agent-", Namespace: AgentNamespace},
		Spec: corev1.PodSpec{
			NodeName:           hostNode,
			ServiceAccountName: AgentServiceAccount,
			Containers:         []corev1.Container{{Name: "exec-agent", Image: "flr"}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the host agent pod on %s: %v", hostNode, err)
	}
	token := e.token(t, AgentNamespace, AgentServiceAccount,
		&authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID})
	cfg := rest.AnonymousClientConfig(e.Config)
	cfg.BearerToken = token
	return cfg
}

// ClaimStatus is what battery would write into a claim's status.
type ClaimStatus struct {
	Phase     agent.ClaimPhase
	VMUID     string
	HostNode  string
	ExpiresAt time.Time
}

// PutClaim creates or replaces a claim of the provisional resource,
// recording creator as the identity that created it, and writes its
// status as battery would.
func (e *Env) PutClaim(t testing.TB, namespace, name, creator string, st ClaimStatus) {
	t.Helper()
	ctx := context.Background()
	res := agent.ProvisionalClaimResource
	client := e.Dynamic.Resource(res.GroupVersionResource()).Namespace(namespace)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": res.Group + "/" + res.Version,
		"kind":       "MicroVMClaim",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{"poolRef": map[string]any{"name": "pool"}},
	}}
	if creator != "" {
		obj.SetAnnotations(map[string]string{res.CreatorAnnotation: creator})
	}
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		existing, err = client.Create(ctx, obj, metav1.CreateOptions{})
	case err == nil:
		obj.SetResourceVersion(existing.GetResourceVersion())
		existing, err = client.Update(ctx, obj, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("writing claim %s/%s: %v", namespace, name, err)
	}
	status := map[string]any{}
	if st.Phase != "" {
		status["phase"] = string(st.Phase)
	}
	if st.VMUID != "" {
		status["microVM"] = map[string]any{"uid": st.VMUID}
	}
	if st.HostNode != "" {
		status["host"] = map[string]any{"nodeName": st.HostNode}
	}
	if !st.ExpiresAt.IsZero() {
		status["leaseExpiresAt"] = st.ExpiresAt.UTC().Format(time.RFC3339)
	}
	existing.Object["status"] = status
	if _, err := client.UpdateStatus(ctx, existing, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("writing the status of claim %s/%s: %v", namespace, name, err)
	}
}

// DeleteClaim deletes a claim, as its Runner does when the Job ends.
func (e *Env) DeleteClaim(t testing.TB, namespace, name string) {
	t.Helper()
	err := e.Dynamic.Resource(agent.ProvisionalClaimResource.GroupVersionResource()).Namespace(namespace).
		Delete(context.Background(), name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting claim %s/%s: %v", namespace, name, err)
	}
}

// HostOptions shape one Host.
type HostOptions struct {
	// HostServices are the Host Services the agent publishes (KF-179) by
	// name, each listening on HostAddress at the given port.
	HostServices map[string]int
	// ExecOpenTimeout is the agent's deadline for flintlockd to open an
	// exec stream (KF-177); zero is two seconds.
	ExecOpenTimeout time.Duration
	// DrainTimeout is the agent's drain timeout (KF-181); zero is an hour.
	DrainTimeout time.Duration
	// ExecDisabled makes the fake flintlockd report exec disabled.
	ExecDisabled bool
}

// Host is one Host: its Node, its fake flintlockd, one CREATED MicroVM on
// it, and its Exec Agent.
type Host struct {
	env  *Env
	t    testing.TB
	opts HostOptions
	// Node is the name of the Host's Node.
	Node string
	// Fake is the fake flintlockd.
	Fake *hostfake.Host
	// VMUID is the uid of the MicroVM created on the Host.
	VMUID string
	// NotReadyDir is the agent's not-ready-reason directory.
	NotReadyDir string
	// Address is where the Exec Agent serves, HostAddress and its port.
	Address string

	logs *syncBuffer

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
}

// NewHost creates a Host's Node at HostAddress, a fake flintlockd served on
// loopback with one CREATED MicroVM, and starts the Exec Agent in front of
// it with its own bound identity, a real claim lookup and the shipped RBAC
// and admission policy. Everything stops when the test ends.
func (e *Env) NewHost(t testing.TB, opts HostOptions) *Host {
	t.Helper()
	ctx := context.Background()
	id := e.seq.Add(1)
	h := &Host{env: e, t: t, opts: opts, Node: fmt.Sprintf("host-%d", id), logs: &syncBuffer{}}

	node, err := e.Admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: h.Node}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the host's node: %v", err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: HostAddress}}
	if _, err := e.Admin.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("setting the host's node status: %v", err)
	}

	h.Fake = hostfake.New(flintlock.FakeHostConfig{
		Name: h.Node, Listen: net.JoinHostPort(HostAddress, "0"), SandboxRoot: t.TempDir(),
		ExecEnabled: !opts.ExecDisabled, Version: "test",
	})
	serveCtx, stopServe := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- h.Fake.Serve(serveCtx) }()
	t.Cleanup(func() {
		stopServe()
		<-served
	})
	select {
	case <-h.Fake.Ready():
	case err := <-served:
		t.Fatalf("the fake flintlockd did not start: %v", err)
	}
	vm, err := h.Fake.Client().CreateMicroVM(ctx, &types.MicroVMSpec{Id: "job", Namespace: "agenttest"})
	if err != nil {
		t.Fatalf("creating a microvm: %v", err)
	}
	h.VMUID = vm.GetSpec().GetUid()
	h.NotReadyDir = filepath.Join(t.TempDir(), "not-ready.d")

	listener, err := net.Listen("tcp", net.JoinHostPort(HostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	h.Address = listener.Addr().String()
	h.start(listener)
	t.Cleanup(func() {
		h.StopAgent()
		if t.Failed() {
			t.Logf("exec agent log of %s:\n%s", h.Node, h.logs.String())
		}
	})
	return h
}

// Sandbox is the directory the fake flintlockd runs the MicroVM's commands
// in, which is the guest's root as the fake plays it.
func (h *Host) Sandbox() string {
	dir, ok := h.Fake.SandboxPath(h.VMUID)
	if !ok {
		h.t.Fatalf("the microvm %s has no sandbox", h.VMUID)
	}
	return dir
}

// Log is what the Exec Agent has logged.
func (h *Host) Log() string { return h.logs.String() }

// config is the agent's configuration on this Host.
func (h *Host) config() *agent.Config {
	services := map[string]agent.HostService{}
	for name, port := range h.opts.HostServices {
		services[name] = agent.HostService{Enabled: true, Port: port}
	}
	open := h.opts.ExecOpenTimeout
	if open == 0 {
		open = 2 * time.Second
	}
	cfg := &agent.Config{
		HostNode:         h.Node,
		Flintlockd:       h.Fake.Addr(),
		FlintlockdUserID: -1,
		TLS:              agent.ServerTLS{CertFile: h.env.Certs.ServerCertFile, KeyFile: h.env.Certs.ServerKeyFile},
		ExecOpenTimeout:  open,
		CallTimeout:      5 * time.Second,
		BridgeGateway:    HostAddress,
		HostServices:     services,
		NotReadyDir:      h.NotReadyDir,
		DrainTimeout:     h.opts.DrainTimeout,
		Guard:            agent.Guard{Namespace: AgentNamespace},
		SyncInterval:     50 * time.Millisecond,
	}
	cfg.ApplyDefaults()
	return cfg
}

// start runs the Exec Agent on listener, as its Host's Host Agent: the
// shipped RBAC, under the shipped admission policy, with an identity that
// names its Host.
func (h *Host) start(listener net.Listener) {
	h.t.Helper()
	cfg := h.config()
	restConfig := h.env.AgentConfig(h.t, h.Node)
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		h.t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		h.t.Fatal(err)
	}
	fl, err := agent.DialFlintlockd(cfg.Flintlockd)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	claims := agent.NewDynamicClaims(dyn, agent.ProvisionalClaimResource, time.Minute)
	go claims.Run(ctx)

	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer func() { _ = fl.Close() }()
		done <- agent.Run(ctx, agent.Options{
			Config: cfg, Kube: kube, Claims: claims, Flintlockd: fl, Listener: listener, Ready: ready,
			Logger: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		h.t.Fatalf("the exec agent stopped before it was ready: %v\n%s", err, h.logs.String())
	case <-time.After(WaitTimeout):
		cancel()
		h.t.Fatalf("the exec agent was not ready in %s\n%s", WaitTimeout, h.logs.String())
	}
	deadline := time.Now().Add(WaitTimeout)
	for !claims.HasSynced() {
		if time.Now().After(deadline) {
			cancel()
			h.t.Fatalf("the exec agent's claims were not listed in %s", WaitTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	h.cancel, h.done = cancel, done
	h.mu.Unlock()
}

// StopAgent stops the Exec Agent, as a restart of its container does. Its
// in-flight streams are cut after the agent's stop grace.
func (h *Host) StopAgent() {
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(WaitTimeout):
		h.t.Errorf("the exec agent of %s did not stop in %s", h.Node, WaitTimeout)
	}
}

// StartAgent starts the Exec Agent again on the same address, as the
// restarted container does.
func (h *Host) StartAgent() {
	h.t.Helper()
	var listener net.Listener
	var err error
	deadline := time.Now().Add(WaitTimeout)
	for {
		if listener, err = net.Listen("tcp", h.Address); err == nil {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("listening on %s again: %v", h.Address, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.start(listener)
}

// ReadNode reads the Host's Node.
func (h *Host) ReadNode() *corev1.Node {
	h.t.Helper()
	node, err := h.env.Admin.CoreV1().Nodes().Get(context.Background(), h.Node, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("reading the host's node: %v", err)
	}
	return node
}

// Eventually polls ok until it holds or WaitTimeout passes.
func Eventually(t testing.TB, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(WaitTimeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", WaitTimeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ListenHostService listens on a free port of HostAddress and accepts
// connections until the test ends, standing in for a Host Service the agent
// probes (KF-178). It returns the port and a function that stops it.
func ListenHostService(t testing.TB) (int, func()) {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(HostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = l.Close() }) }
	t.Cleanup(stop)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, stop
}

// syncBuffer is a log buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
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
