package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/kubelet/kubelettest"
)

// ErrNoCluster is returned by StartCluster when the API server test
// environment's binaries are missing and FLINTLOCK_RUNNER_REQUIRE_ENVTEST is
// not set: the caller skips the Kubernetes stack.
var ErrNoCluster = kubelettest.ErrNoAssets

// Cluster is the API server test environment a Kubernetes stack runs
// against: a real kube-apiserver and etcd from envtest with the Host Agent's
// shipped RBAC and admission policy applied (kubelettest), configured as a
// cluster's API server is to reach its kubelets, so that a pods/exec request
// is proxied to the Pod Provider of the pod's Virtual Node. One Cluster is
// shared by many Stacks, each of which works in a namespace and on Hosts of
// its own; starting one takes seconds, a Stack on it a fraction of that.
type Cluster struct {
	env   *kubelettest.Environment
	certs *hostfake.TestCerts
	dir   string
	root  string
	seq   atomic.Int64
}

// StartCluster starts the API server test environment. It returns
// ErrNoCluster when the envtest binaries are not there to start it with.
func StartCluster() (*Cluster, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "flintlock-harness-cluster-")
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	// One certificate authority issues the API server's kubelet client
	// certificate and every Pod Provider's serving certificate, which names
	// the loopback address the Virtual Nodes report.
	certs, err := hostfake.WriteTestCerts(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("harness: %w", err)
	}
	deploy := filepath.Join(root, "deploy", "host-agent")
	env, err := kubelettest.Start(filepath.Join(deploy, "rbac.yaml"),
		kubelettest.WithAdmissionPolicy(filepath.Join(deploy, "admission-policy.yaml")),
		kubelettest.WithKubeletClient(kubelettest.KubeletClient{
			CertFile: certs.ClientCertFile, KeyFile: certs.ClientKeyFile, CAFile: certs.CAFile,
		}))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &Cluster{env: env, certs: certs, dir: dir, root: root}, nil
}

// Stop stops the API server and etcd and removes the certificates.
func (c *Cluster) Stop() error {
	err := c.env.Stop()
	if rmErr := os.RemoveAll(c.dir); rmErr != nil {
		err = errors.Join(err, rmErr)
	}
	return err
}

// Admin is the administrator's client. The harness uses it for what the
// cluster's own components do (the scheduler, the ReplicaSet controller, a
// Host's kubelet) and for what an operator does (cordon, clean up).
func (c *Cluster) Admin() kubernetes.Interface { return c.env.Admin }

// next returns a number no other Stack on the Cluster has had, for the
// names of namespaces, Nodes and users.
func (c *Cluster) next() int64 { return c.seq.Add(1) }

// runnerUser creates a user bound to the Runner's shipped RBAC
// (deploy/runner/role.yaml) in namespace and to nothing else, so that
// whatever the Runner does outside it is refused by the API server
// (KF-110), and waits until the bindings take effect.
func (c *Cluster) runnerUser(ctx context.Context, namespace string) (*rest.Config, error) {
	name := fmt.Sprintf("%s-runner", namespace)
	cfg, err := c.env.AddUser(name)
	if err != nil {
		return nil, fmt.Errorf("harness: adding user %s: %w", name, err)
	}
	role, clusterRole, err := runnerRoles(filepath.Join(c.root, "deploy", "runner", "role.yaml"))
	if err != nil {
		return nil, err
	}
	admin := c.env.Admin
	subject := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}}
	role.Namespace = namespace
	if _, err := admin.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("harness: creating the runner's role: %w", err)
	}
	if _, err := admin.RbacV1().ClusterRoles().Create(ctx, clusterRole, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("harness: creating the runner's cluster role: %w", err)
	}
	if _, err := admin.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   subject,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
	}, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("harness: binding the runner's role: %w", err)
	}
	if _, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   subject,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole.Name},
	}, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("harness: binding the runner's cluster role: %w", err)
	}

	// The authoriser learns of a binding through its own watch, so the
	// first moments after creating one can still be refused.
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	err = poll(ctx, 30*time.Second, func() bool {
		_, podsErr := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{Limit: 1})
		_, setsErr := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{Limit: 1})
		_, nodesErr := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
		return podsErr == nil && setsErr == nil && nodesErr == nil
	})
	if err != nil {
		return nil, fmt.Errorf("harness: the runner's role bindings did not take effect: %w", err)
	}
	return cfg, nil
}

// removeRunnerUser deletes the bindings runnerUser made outside the
// namespace. The namespace's own go with the Stack's pods.
func (c *Cluster) removeRunnerUser(ctx context.Context, namespace string) error {
	err := c.env.Admin.RbacV1().ClusterRoleBindings().Delete(ctx, namespace+"-runner", metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// runnerRoles reads the Role and the ClusterRole of deploy/runner/role.yaml.
func runnerRoles(path string) (*rbacv1.Role, *rbacv1.ClusterRole, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("harness: %w", err)
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
			return nil, nil, fmt.Errorf("harness: %s: %w", path, err)
		}
		switch raw.Kind {
		case "Role":
			role = &rbacv1.Role{ObjectMeta: raw.ObjectMeta, Rules: raw.Rules}
		case "ClusterRole":
			clusterRole = &raw
		case "":
		default:
			return nil, nil, fmt.Errorf("harness: %s: unexpected kind %q", path, raw.Kind)
		}
	}
	if role == nil || clusterRole == nil {
		return nil, nil, fmt.Errorf("harness: %s: want a Role and a ClusterRole", path)
	}
	return role, clusterRole, nil
}

// writeKubeconfig writes cfg, a client certificate identity, as the
// kubeconfig file the Runner reads (pool_manager.kubernetes.kubeconfig),
// owner-only because it holds the private key.
func writeKubeconfig(path string, cfg *rest.Config) error {
	const name = "harness"
	file := clientcmdapi.NewConfig()
	file.Clusters[name] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	file.AuthInfos[name] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	file.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	file.CurrentContext = name
	if err := clientcmd.WriteToFile(*file, path); err != nil {
		return fmt.Errorf("harness: writing the runner's kubeconfig: %w", err)
	}
	return os.Chmod(path, 0o600)
}

// createNamespace creates a namespace.
func (c *Cluster) createNamespace(ctx context.Context, name string) error {
	_, err := c.env.Admin.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("harness: creating namespace %s: %w", name, err)
	}
	return nil
}

// poll calls ok every 25ms until it holds, ctx ends or timeout passes.
func poll(ctx context.Context, timeout time.Duration, ok func() bool) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ok() {
			return nil
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
}
