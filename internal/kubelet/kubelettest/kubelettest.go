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

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
)

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
	// more (KF-111).
	Agent kubernetes.Interface
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
func Start(rbacManifest string) (*Environment, error) {
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
	if e.Admin, err = kubernetes.NewForConfig(cfg); err == nil {
		err = applyRBAC(context.Background(), e.Admin, rbacManifest)
	}
	if err == nil {
		agentCfg := rest.CopyConfig(cfg)
		agentCfg.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:" + AgentNamespace + ":" + AgentServiceAccount,
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + AgentNamespace, "system:authenticated"},
		}
		e.Agent, err = kubernetes.NewForConfig(agentCfg)
	}
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	return e, nil
}

// Stop stops the API server and etcd.
func (e *Environment) Stop() error { return e.env.Stop() }

// applyRBAC creates the Host Agent's namespace and every object of the
// manifest, strictly decoded so that a misspelt field is an error.
func applyRBAC(ctx context.Context, admin kubernetes.Interface, manifest string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: AgentNamespace}}
	if _, err := admin.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		return err
	}
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
		default:
			err = fmt.Errorf("unexpected kind %q", meta.Kind)
		}
		if err != nil {
			return fmt.Errorf("%s: %s: %w", manifest, meta.Kind, err)
		}
	}
	return nil
}
