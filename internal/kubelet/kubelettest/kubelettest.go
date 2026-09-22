// Package kubelettest is the test environment of the Pod Provider: a real
// kube-apiserver and etcd, started by controller-runtime's envtest, with the
// provider's shipped RBAC applied and a client that holds exactly it. The
// fake Host (internal/flintlock/fake) is the other half; together they test
// the provider with no kubelet, no KVM and no AWS. It is a package of its
// own so that the cluster stack of the end-to-end harness can start the same
// environment.
package kubelettest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const (
	// AssetsVar names the directory of kube-apiserver and etcd, which
	// `make envtest` downloads and prints.
	AssetsVar = "KUBEBUILDER_ASSETS"
	// RequireVar names the variable that, when set, makes a missing
	// environment a failure instead of a skip. CI sets it.
	RequireVar = "FLINTLOCK_RUNNER_REQUIRE_ENVTEST"
	// AgentNamespace is the namespace of the subject of
	// deploy/host-agent/rbac.yaml.
	AgentNamespace = "flintlock-system"
	// AgentServiceAccount is the ServiceAccount the manifest binds.
	AgentServiceAccount = "flintlock-host-agent"
	// AgentUser is the user name of AgentServiceAccount.
	AgentUser = "system:serviceaccount:" + AgentNamespace + ":" + AgentServiceAccount
	// NodeNameExtra is the user information extra in which the API server
	// records the node of the pod a bound ServiceAccount token belongs to.
	NodeNameExtra = "authentication.kubernetes.io/node-name"
	// KubeletClientUser is the user of the client certificate that the fake
	// Host's WriteTestCerts issues. Start binds it to the bootstrap
	// ClusterRole system:kubelet-api-admin, as kubeadm binds the API
	// server's kubelet client, so that it may use the Pod Provider's kubelet
	// API (KF-130).
	KubeletClientUser = "flintlock-runner"
)

// policyActiveTimeout bounds the wait for an admission policy to take
// effect: the API server compiles and loads a new policy asynchronously.
const policyActiveTimeout = 30 * time.Second

// ErrNoAssets is returned by Start when AssetsVar is unset and RequireVar is
// not: the caller skips.
var ErrNoAssets = errors.New("kubelettest: no API server test environment: run `make envtest` and set " + AssetsVar)

// Environment is a running API server.
type Environment struct {
	env *envtest.Environment
	// Config is the administrator's configuration.
	Config *rest.Config
	// Admin is the test's own client: it plays the scheduler, the Runner,
	// the Host's kubelet and the operator.
	Admin kubernetes.Interface
	// Agent is the provider's client. It impersonates the Host Agent's
	// ServiceAccount, so it can do what the shipped RBAC allows and nothing
	// more (KF-111). Its identity names no node, so where the admission
	// policy is loaded it may change nothing; AgentFor is the client of one
	// Host's provider.
	Agent kubernetes.Interface
}

// Option configures Start.
type Option func(*options)

type options struct {
	admissionPolicy string
}

// WithAdmissionPolicy loads the manifest at path, which is
// deploy/host-agent/admission-policy.yaml, after the RBAC manifest, and makes
// Start wait until the API server enforces it.
func WithAdmissionPolicy(path string) Option {
	return func(o *options) { o.admissionPolicy = path }
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The Pod Provider SHALL be tested against a Kubernetes API
//# server test environment and the fake Host, with no kubelet, no KVM and no
//# AWS.

// Start starts kube-apiserver and etcd and nothing else: no kubelet, no
// scheduler and no controller manager run, so a test binds its pods by name
// and creates its Nodes by hand. rbacManifest is the path of
// deploy/host-agent/rbac.yaml, which is applied unchanged; the API server
// enforces RBAC, so the Agent client fails wherever the provider would
// overstep the manifest.
func Start(rbacManifest string, opts ...Option) (*Environment, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if os.Getenv(AssetsVar) == "" {
		if os.Getenv(RequireVar) != "" {
			return nil, fmt.Errorf("kubelettest: %s is set and %s is not: run `make envtest`", RequireVar, AssetsVar)
		}
		return nil, ErrNoAssets
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		return nil, fmt.Errorf("kubelettest: starting the API server test environment: %w", err)
	}
	e := &Environment{env: env, Config: cfg}
	ctx := context.Background()
	if e.Admin, err = kubernetes.NewForConfig(cfg); err == nil {
		err = applyRBAC(ctx, e.Admin, rbacManifest)
	}
	if err == nil {
		err = bindKubeletClient(ctx, e.Admin)
	}
	if err == nil {
		e.Agent, err = e.agent(nil)
	}
	if err == nil && o.admissionPolicy != "" {
		if err = applyManifest(ctx, e.Admin, o.admissionPolicy); err == nil {
			err = e.awaitPolicy(ctx)
		}
	}
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	return e, nil
}

// Stop stops the API server and etcd.
func (e *Environment) Stop() error { return e.env.Stop() }

// AgentFor is the client of the Pod Provider of the Host whose Node is
// hostNode: the Host Agent's ServiceAccount, impersonated with the node
// extra that the bound token of a Host Agent pod on that Node carries, so
// that the admission policy sees what it would see from that token. The
// test of the policy itself (KF-137) uses real bound tokens.
func (e *Environment) AgentFor(hostNode string) (kubernetes.Interface, error) {
	return e.agent(map[string][]string{NodeNameExtra: {hostNode}})
}

func (e *Environment) agent(extra map[string][]string) (kubernetes.Interface, error) {
	cfg := rest.CopyConfig(e.Config)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: AgentUser,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + AgentNamespace, "system:authenticated"},
		Extra:    extra,
	}
	return kubernetes.NewForConfig(cfg)
}

// bindKubeletClient lets KubeletClientUser use the kubelet API of every
// node, which is what system:kubelet-api-admin is for.
func bindKubeletClient(ctx context.Context, admin kubernetes.Interface) error {
	_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kubelettest-kubelet-api-client"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:kubelet-api-admin"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: KubeletClientUser}},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("kubelettest: binding the kubelet API client: %w", err)
	}
	return nil
}

// awaitPolicy waits until the admission policy refuses what it has to: a
// Pod Provider creating a Node that is not its Virtual Node, tried as a dry
// run so that nothing is created meanwhile.
func (e *Environment) awaitPolicy(ctx context.Context) error {
	probe, err := e.AgentFor("kubelettest-policy-probe")
	if err != nil {
		return err
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "kubelettest-not-a-virtual-node"}}
	deadline := time.Now().Add(policyActiveTimeout)
	for {
		_, err := probe.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if IsPolicyDenial(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kubelettest: the admission policy was not enforced within %s: the probe got %v", policyActiveTimeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// IsPolicyDenial reports whether err is a refusal by a
// ValidatingAdmissionPolicy, as opposed to one by RBAC or anything else.
func IsPolicyDenial(err error) bool {
	return err != nil && apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy")
}

// applyRBAC creates the Host Agent's namespace and every object of the
// manifest.
func applyRBAC(ctx context.Context, admin kubernetes.Interface, manifest string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: AgentNamespace}}
	if _, err := admin.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		return err
	}
	return applyManifest(ctx, admin, manifest)
}

// applyManifest creates every object of a manifest, strictly decoded so that
// a misspelt field is an error.
func applyManifest(ctx context.Context, admin kubernetes.Interface, manifest string) error {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	for _, doc := range strings.Split(string(data), "\n---") {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			return fmt.Errorf("%s: %w", manifest, err)
		}
		switch meta.Kind {
		case "":
			continue
		case "ClusterRole":
			obj := &rbacv1.ClusterRole{}
			if err = yaml.UnmarshalStrict([]byte(doc), obj); err == nil {
				_, err = admin.RbacV1().ClusterRoles().Create(ctx, obj, metav1.CreateOptions{})
			}
		case "ClusterRoleBinding":
			obj := &rbacv1.ClusterRoleBinding{}
			if err = yaml.UnmarshalStrict([]byte(doc), obj); err == nil {
				_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, obj, metav1.CreateOptions{})
			}
		case "Role":
			obj := &rbacv1.Role{}
			if err = yaml.UnmarshalStrict([]byte(doc), obj); err == nil {
				_, err = admin.RbacV1().Roles(obj.Namespace).Create(ctx, obj, metav1.CreateOptions{})
			}
		case "RoleBinding":
			obj := &rbacv1.RoleBinding{}
			if err = yaml.UnmarshalStrict([]byte(doc), obj); err == nil {
				_, err = admin.RbacV1().RoleBindings(obj.Namespace).Create(ctx, obj, metav1.CreateOptions{})
			}
		case "ValidatingAdmissionPolicy":
			obj := &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err = yaml.UnmarshalStrict([]byte(doc), obj); err == nil {
				_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, obj, metav1.CreateOptions{})
			}
		case "ValidatingAdmissionPolicyBinding":
			obj := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err = yaml.UnmarshalStrict([]byte(doc), obj); err == nil {
				_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, obj, metav1.CreateOptions{})
			}
		default:
			err = fmt.Errorf("unexpected kind %q", meta.Kind)
		}
		if err != nil {
			return fmt.Errorf("%s: %s: %w", manifest, meta.Kind, err)
		}
	}
	return nil
}
