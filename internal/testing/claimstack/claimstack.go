// Package claimstack is the claim design in a test process: an API server
// from controller-runtime's envtest with battery-operator's CRDs, the
// identities and permissions of deploy/runner applied unchanged, a fake
// flintlockd for each Host with the test double of battery-operator's Exec
// Agent in front of it, and the fake battery binding claims on those Hosts
// (docs/requirements/12-cluster-fleet.md#claim-test-doubles). It writes a
// kubeconfig for the Runner's own ServiceAccount, so that the flr binary
// runs the claim backend with the permissions the Fleet Manifests grant
// and no others.
//
// The test of `flr run` on the claim backend (cmd/flr) and the end-to-end
// harness (internal/testing/harness) both start it. It needs the envtest
// binaries; Start reports when they are missing, as claimtest.Start does.
package claimstack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim/claimtest"
	"github.com/phoban01/flintlock-runner/internal/testing/fakeexecagent"
)

// The names deploy/runner gives the Runner's namespace, its identity and
// the Holder, and battery-operator's namespace and serving CA ConfigMap,
// which deploy/runner/rbac.yaml names too.
const (
	RunnerNamespace    = "flintlock-system"
	RunnerAccount      = "flintlock-runner"
	HolderAccount      = "flintlock-runner-holder"
	OperatorNamespace  = "battery-operator-system"
	ServingCAConfigMap = "flintlockd-ca"
	servingCAKey       = "serving-ca.crt"
)

// runnerManifests are the files of deploy/runner that make the Runner's
// identities and permissions.
var runnerManifests = []string{"serviceaccount.yaml", "holder.yaml", "rbac.yaml"}

// tokenLifetime is how long the Runner's ServiceAccount token lasts.
const tokenLifetime = time.Hour

// Host is one Host of the Stack.
type Host struct {
	// Name is the name of the Host's Node, and of its fake flintlockd.
	Name string
	// Annotations are set on the Node, such as the Host Service addresses
	// that the Host Agent publishes (KF-194).
	Annotations map[string]string
	// BootDelay is how long a MicroVM of the fake flintlockd stays PENDING
	// (TD-022).
	BootDelay time.Duration
}

// Options configure a Stack.
type Options struct {
	// Dir is a directory the Stack owns: certificates, the kubeconfig and
	// the sandboxes of the fake Hosts. It is required.
	Dir string
	// DeployDir is this module's deploy/runner directory. It is required.
	DeployDir string
	// Hosts are the Hosts; empty means one, "host-a".
	Hosts []Host
}

// Stack is a running claim stack.
type Stack struct {
	// Env is the API server.
	Env *claimtest.Env
	// Admin is the API server's administrator client.
	Admin client.WithWatch
	// Battery is the fake battery.
	Battery *claimtest.Battery
	// Hosts are the fake flintlockds, in the order of Options.Hosts.
	Hosts []*hostfake.Host
	// Agents are the Exec Agents, one in front of each Host.
	Agents []*fakeexecagent.Agent
	// Kubeconfig is a kubeconfig file for the Runner's ServiceAccount.
	Kubeconfig string

	cancel context.CancelFunc
	done   chan struct{}

	stopOnce sync.Once
	stopErr  error
}

// Start starts the stack. It returns a nil Stack and the reason when the
// envtest binaries are missing, for the caller to skip on; with
// claimtest.RequireEnv set that is an error instead. On error everything
// started so far is stopped.
func Start(ctx context.Context, opts Options) (s *Stack, note string, err error) {
	if opts.Dir == "" || opts.DeployDir == "" {
		return nil, "", errors.New("claimstack: Options.Dir and Options.DeployDir are required")
	}
	if len(opts.Hosts) == 0 {
		opts.Hosts = []Host{{Name: "host-a"}}
	}
	env, note, err := claimtest.Start()
	if err != nil || env == nil {
		return nil, note, err
	}
	s = &Stack{Env: env, Admin: env.Client}
	defer func() {
		if err != nil {
			_ = s.Stop()
			s = nil
		}
	}()

	for _, ns := range []string{RunnerNamespace, OperatorNamespace} {
		if err := s.Admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			return s, "", fmt.Errorf("claimstack: namespace %s: %w", ns, err)
		}
	}
	for _, f := range runnerManifests {
		if err := applyManifest(ctx, s.Admin, filepath.Join(opts.DeployDir, f)); err != nil {
			return s, "", err
		}
	}

	// Every Exec Agent serves a certificate of the CA that battery-operator
	// publishes, which the Runner reads (KF-186).
	certs, err := hostfake.WriteTestCerts(filepath.Join(opts.Dir, "certs"))
	if err != nil {
		return s, "", fmt.Errorf("claimstack: %w", err)
	}
	ca, err := os.ReadFile(certs.CAFile)
	if err != nil {
		return s, "", fmt.Errorf("claimstack: %w", err)
	}
	if err := s.Admin.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: OperatorNamespace, Name: ServingCAConfigMap},
		Data:       map[string]string{servingCAKey: string(ca)},
	}); err != nil {
		return s, "", fmt.Errorf("claimstack: serving CA: %w", err)
	}

	var hosts []claimtest.Host
	for _, h := range opts.Hosts {
		if err := s.Admin.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: h.Name, Annotations: h.Annotations,
		}}); err != nil {
			return s, "", fmt.Errorf("claimstack: node %s: %w", h.Name, err)
		}
		host := hostfake.New(flintlock.FakeHostConfig{
			Name: h.Name, ExecEnabled: true, Version: "v0.9.0", BootDelay: h.BootDelay,
			SandboxRoot: filepath.Join(opts.Dir, "hosts", h.Name),
		})
		s.Hosts = append(s.Hosts, host)
		agent, err := fakeexecagent.Start(fakeexecagent.Config{
			Node: h.Name, CertFile: certs.ServerCertFile, KeyFile: certs.ServerKeyFile,
			Upstream: host.Client(), Authorizer: fakeexecagent.Reviewer{Kube: s.Admin},
		})
		if err != nil {
			return s, "", fmt.Errorf("claimstack: exec agent of %s: %w", h.Name, err)
		}
		s.Agents = append(s.Agents, agent)
		hosts = append(hosts, claimtest.Host{NodeName: h.Name, AgentAddress: agent.Addr(), MicroVMs: host.Client()})
	}

	s.Battery = claimtest.NewBattery(s.Admin, RunnerNamespace, hosts...)
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = cancel, make(chan struct{})
	go func() { defer close(s.done); s.Battery.Run(runCtx) }()

	if err := s.writeKubeconfig(ctx, filepath.Join(opts.Dir, "kubeconfig")); err != nil {
		return s, "", err
	}
	return s, "", nil
}

// writeKubeconfig writes a kubeconfig with a token of the Runner's own
// ServiceAccount, which has deploy/runner/rbac.yaml's permissions and no
// others.
func (s *Stack) writeKubeconfig(ctx context.Context, path string) error {
	clientset, err := kubernetes.NewForConfig(s.Env.Config)
	if err != nil {
		return fmt.Errorf("claimstack: %w", err)
	}
	expiry := int64(tokenLifetime / time.Second)
	tok, err := clientset.CoreV1().ServiceAccounts(RunnerNamespace).CreateToken(ctx, RunnerAccount,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry}}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("claimstack: a token of the Runner's ServiceAccount: %w", err)
	}
	caFile := filepath.Join(filepath.Dir(path), "apiserver-ca.crt")
	if err := os.WriteFile(caFile, s.Env.Config.CAData, 0o600); err != nil {
		return fmt.Errorf("claimstack: %w", err)
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, `apiVersion: v1
kind: Config
current-context: runner
clusters:
  - name: envtest
    cluster:
      server: %s
      certificate-authority: %s
contexts:
  - name: runner
    context:
      cluster: envtest
      user: runner
users:
  - name: runner
    user:
      token: %s
`, s.Env.Config.Host, caFile, tok.Status.Token), 0o600); err != nil {
		return fmt.Errorf("claimstack: %w", err)
	}
	s.Kubeconfig = path
	return nil
}

// Host is the fake flintlockd of the named Host, nil if there is none.
func (s *Stack) Host(name string) *hostfake.Host {
	for _, h := range s.Hosts {
		if h.Config().Name == name {
			return h
		}
	}
	return nil
}

// Agent is the Exec Agent of the named Host, nil if there is none.
func (s *Stack) Agent(name string) *fakeexecagent.Agent {
	for _, a := range s.Agents {
		if a.Node() == name {
			return a
		}
	}
	return nil
}

// Claims lists the MicroVMClaims in the Runner's namespace.
func (s *Stack) Claims(ctx context.Context) ([]batteryv1alpha1.MicroVMClaim, error) {
	claims := &batteryv1alpha1.MicroVMClaimList{}
	if err := s.Admin.List(ctx, claims, client.InNamespace(RunnerNamespace)); err != nil {
		return nil, fmt.Errorf("claimstack: listing claims: %w", err)
	}
	return claims.Items, nil
}

// Pools lists the Pools in the Runner's namespace.
func (s *Stack) Pools(ctx context.Context) ([]batteryv1alpha1.Pool, error) {
	pools := &batteryv1alpha1.PoolList{}
	if err := s.Admin.List(ctx, pools, client.InNamespace(RunnerNamespace)); err != nil {
		return nil, fmt.Errorf("claimstack: listing pools: %w", err)
	}
	return pools.Items, nil
}

// Stop stops the fake battery, the Exec Agents, the Hosts and the API
// server, in that order. The Hosts kill whatever they still run; their
// sandbox directories stay, for a caller to check. Stop is idempotent.
func (s *Stack) Stop() error {
	s.stopOnce.Do(func() {
		var errs []error
		if s.cancel != nil {
			s.cancel()
			<-s.done
		}
		for _, a := range s.Agents {
			errs = append(errs, a.Close())
		}
		for _, h := range s.Hosts {
			errs = append(errs, h.Close())
		}
		if s.Env != nil {
			errs = append(errs, s.Env.Stop())
		}
		s.stopErr = errors.Join(errs...)
	})
	return s.stopErr
}

// applyManifest creates every object of the YAML file path, unchanged.
func applyManifest(ctx context.Context, c client.Client, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("claimstack: %w", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("claimstack: %s: %w", path, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Create(ctx, obj); err != nil {
			return fmt.Errorf("claimstack: %s: creating %s %s: %w", path, obj.GetKind(), obj.GetName(), err)
		}
	}
}
