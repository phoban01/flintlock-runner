package hostservices_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/phoban01/flintlock-runner/internal/hostservices"
)

const (
	// hostAgentNamespace and hostAgentServiceAccount are the identity
	// deploy/host-agent/host-services-rbac.yaml binds.
	hostAgentNamespace      = "flintlock-system"
	hostAgentServiceAccount = "flintlock-host-agent"
	// nodeNameExtra is where the API server puts the node of the pod a
	// bound token was issued to.
	nodeNameExtra = "authentication.kubernetes.io/node-name"
)

// startAPIServer starts kube-apiserver and etcd, or skips the test when
// their binaries are not there, unless CI requires them.
func startAPIServer(t *testing.T) (*rest.Config, kubernetes.Interface) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("FLINTLOCK_RUNNER_REQUIRE_ENVTEST") != "" {
			t.Fatal("FLINTLOCK_RUNNER_REQUIRE_ENVTEST is set and KUBEBUILDER_ASSETS is not: run `make envtest`")
		}
		t.Skip("no API server test environment: run `make envtest` and set KUBEBUILDER_ASSETS")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting the API server test environment: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, admin
}

// apply creates every object of a manifest under deploy/host-agent,
// unchanged, as the administrator.
func apply(t *testing.T, admin kubernetes.Interface, name string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "host-agent", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, doc := range strings.Split(string(data), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode([]byte(doc), nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		switch o := obj.(type) {
		case *rbacv1.ClusterRole:
			_, err = admin.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.ClusterRoleBinding:
			_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
		case *admissionregistrationv1.ValidatingAdmissionPolicy:
			_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, o, metav1.CreateOptions{})
		case *admissionregistrationv1.ValidatingAdmissionPolicyBinding:
			_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, o, metav1.CreateOptions{})
		default:
			t.Fatalf("%s: unexpected %T", name, obj)
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// isPolicyDenial reports whether err is a refusal by a
// ValidatingAdmissionPolicy, as opposed to one by RBAC or anything else.
func isPolicyDenial(err error) bool {
	return err != nil && apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy")
}

// hostAgentClient is the client of the Host Agent's pod on node: the bound
// ServiceAccount token that the kubelet projects into the pod, and nothing
// else.
func hostAgentClient(t *testing.T, cfg *rest.Config, admin kubernetes.Interface, node string) kubernetes.Interface {
	t.Helper()
	ctx := context.Background()
	pod, err := admin.CoreV1().Pods(hostAgentNamespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "host-agent-", Namespace: hostAgentNamespace},
		Spec: corev1.PodSpec{
			NodeName:           node,
			ServiceAccountName: hostAgentServiceAccount,
			Containers:         []corev1.Container{{Name: "host-services", Image: "flr"}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return tokenClient(t, cfg, admin, &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID})
}

// tokenClient is a client with a token of the Host Agent's ServiceAccount,
// bound to the object when one is given.
func tokenClient(t *testing.T, cfg *rest.Config, admin kubernetes.Interface, bound *authenticationv1.BoundObjectReference) kubernetes.Interface {
	t.Helper()
	expiry := int64(3600)
	tok, err := admin.CoreV1().ServiceAccounts(hostAgentNamespace).CreateToken(context.Background(), hostAgentServiceAccount,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry, BoundObjectRef: bound}},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := rest.AnonymousClientConfig(cfg)
	c.BearerToken = tok.Status.Token
	client, err := kubernetes.NewForConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

//= docs/requirements/12-cluster-fleet.md#cluster-host-agent
//= type=test
//# The Host Agent SHALL publish the address and port of each
//# enabled Host Service on the guest bridge gateway, under the names
//# `buildkit`, `go_proxy`, `registry_mirror` and `http_cache`, as the
//# annotations `host-service.gitlab-runner.flintlock.dev/<name>` on its Host's
//# Node.

// TestPublishUnderTheShippedPolicy runs the publisher as the Host Agent of
// host-a, with the bound token of its pod, against an API server with
// deploy/host-agent's RBAC and admission policy applied unchanged. It
// publishes its Host Services on host-a. It cannot annotate host-b, change
// host-a's labels or any other annotation of host-a; and a token that
// names no node changes nothing.
func TestPublishUnderTheShippedPolicy(t *testing.T) {
	t.Parallel()
	cfg, admin := startAPIServer(t)
	ctx := context.Background()
	if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: hostAgentNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CoreV1().ServiceAccounts(hostAgentNamespace).Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: hostAgentServiceAccount}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	apply(t, admin, "host-services-rbac.yaml")
	apply(t, admin, "host-services-admission-policy.yaml")
	for _, name := range []string{"host-a", "host-b"} {
		if _, err := admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{"gitlab-runner.flintlock.dev/host": "true"},
			Annotations: map[string]string{"example.com/unrelated": "kept"},
		}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// The policy takes a moment to be enforced; wait until it refuses a
	// label change, tried as a dry run.
	probe := rest.CopyConfig(cfg)
	probe.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + hostAgentNamespace + ":" + hostAgentServiceAccount,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + hostAgentNamespace, "system:authenticated"},
		Extra:    map[string][]string{nodeNameExtra: {"host-a"}},
	}
	prober, err := kubernetes.NewForConfig(probe)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := prober.CoreV1().Nodes().Patch(ctx, "host-a", k8stypes.MergePatchType,
			[]byte(`{"metadata":{"labels":{"probe":"x"}}}`), metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
		if isPolicyDenial(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the admission policy was not enforced: the probe got %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	agent := hostAgentClient(t, cfg, admin, "host-a")
	services := &hostservices.Config{HostNode: "host-a", BridgeGateway: "10.200.0.1", HostServices: map[string]hostservices.HostService{
		"buildkit": {Enabled: true, Port: 1234},
		"go_proxy": {Enabled: true, Port: 3000},
	}}
	if err := hostservices.Publish(ctx, agent, "host-a", services.Annotations()); err != nil {
		t.Fatalf("publishing on its own Node: %v", err)
	}
	node, err := admin.CoreV1().Nodes().Get(ctx, "host-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if node.Annotations["host-service.gitlab-runner.flintlock.dev/buildkit"] != "10.200.0.1:1234" ||
		node.Annotations["host-service.gitlab-runner.flintlock.dev/go_proxy"] != "10.200.0.1:3000" ||
		node.Annotations["example.com/unrelated"] != "kept" {
		t.Errorf("host-a's annotations = %v", node.Annotations)
	}

	if err := hostservices.Publish(ctx, agent, "host-b", services.Annotations()); !isPolicyDenial(err) {
		t.Errorf("publishing on another Host's Node = %v, want a policy denial", err)
	}
	for what, patch := range map[string]string{
		"a label":            `{"metadata":{"labels":{"gitlab-runner.flintlock.dev/host":"false"}}}`,
		"another annotation": `{"metadata":{"annotations":{"example.com/unrelated":"changed"}}}`,
		"a new annotation":   `{"metadata":{"annotations":{"gitlab-runner.flintlock.dev/other":"x"}}}`,
		"its spec":           `{"spec":{"unschedulable":true}}`,
	} {
		_, err := agent.CoreV1().Nodes().Patch(ctx, "host-a", k8stypes.MergePatchType, []byte(patch), metav1.PatchOptions{})
		if !isPolicyDenial(err) {
			t.Errorf("changing %s of its own Node = %v, want a policy denial", what, err)
		}
	}

	unbound := tokenClient(t, cfg, admin, nil)
	if err := hostservices.Publish(ctx, unbound, "host-a", services.Annotations()); !isPolicyDenial(err) {
		t.Errorf("publishing with a token that names no node = %v, want a policy denial", err)
	}
}
