package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
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
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim/claimtest"
	"github.com/phoban01/flintlock-runner/internal/testing/fakeexecagent"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// The names deploy/runner gives the Runner's namespace, its identity and
// the Holder, and battery-operator's namespace and serving CA ConfigMap,
// which deploy/runner/rbac.yaml names too.
const (
	claimRunnerNamespace = "flintlock-system"
	claimRunnerAccount   = "flintlock-runner"
	claimHolderAccount   = "flintlock-runner-holder"
	operatorNamespace    = "battery-operator-system"
	servingCAConfigMap   = "flintlockd-ca"
	claimHostNode        = "host-a"
)

// applyManifest creates every object of the YAML file path, unchanged.
func applyManifest(ctx context.Context, t *testing.T, c client.Client, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("%s: creating %s %s: %v", path, obj.GetKind(), obj.GetName(), err)
		}
	}
}

// claimStack is the claim design in the test process: an API server with
// battery-operator's CRDs and deploy/runner's identities and permissions,
// the fake battery binding claims on one fake Host, the test double of
// battery-operator's Exec Agent in front of that Host, and the fake GitLab.
type claimStack struct {
	kube     client.Client
	agent    *fakeexecagent.Agent
	gitlab   *fakegitlab.Server
	buildDir string
}

// startClaimStack starts the claim stack and writes the Runner's
// configuration: the claim backend, as the Runner's own ServiceAccount,
// with deploy/runner/config.yaml's Holder and serving CA.
func startClaimStack(t *testing.T) (*claimStack, string) {
	t.Helper()
	env, note, err := claimtest.Start()
	if err != nil {
		t.Fatal(err)
	}
	if env == nil {
		t.Skip(note)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	admin := env.Client
	clientset, err := kubernetes.NewForConfig(env.Config)
	if err != nil {
		t.Fatal(err)
	}

	for _, ns := range []string{claimRunnerNamespace, operatorNamespace} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"serviceaccount.yaml", "holder.yaml", "rbac.yaml"} {
		applyManifest(ctx, t, admin, filepath.Join("..", "..", "deploy", "runner", f))
	}

	// The Host: its Node, with the Host Services the Host Agent publishes
	// (KF-194); the fake flintlockd; and the Exec Agent in front of it,
	// serving a certificate of the CA that battery-operator publishes.
	if err := admin.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        claimHostNode,
		Annotations: map[string]string{kubelabels.HostServiceAnnotation("buildkit"): "10.200.0.1:1234"},
	}}); err != nil {
		t.Fatal(err)
	}
	certs, err := hostfake.WriteTestCerts(filepath.Join(dir, "certs"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(certs.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: servingCAConfigMap},
		Data:       map[string]string{"serving-ca.crt": string(ca)},
	}); err != nil {
		t.Fatal(err)
	}
	host := hostfake.New(flintlock.FakeHostConfig{Name: claimHostNode, ExecEnabled: true, Version: "v0.9.0", SandboxRoot: filepath.Join(dir, "sandboxes")})
	t.Cleanup(func() { _ = host.Close() })
	agent, err := fakeexecagent.Start(fakeexecagent.Config{
		Node: claimHostNode, CertFile: certs.ServerCertFile, KeyFile: certs.ServerKeyFile,
		Upstream: host.Client(), Authorizer: fakeexecagent.Reviewer{Kube: admin},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })

	battery := claimtest.NewBattery(admin, claimRunnerNamespace,
		claimtest.Host{NodeName: claimHostNode, AgentAddress: agent.Addr(), MicroVMs: host.Client()})
	done := make(chan struct{})
	go func() { defer close(done); battery.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// The Runner's kubeconfig: a token of its own ServiceAccount, which has
	// deploy/runner/rbac.yaml's permissions and no others.
	expiry := int64(3600)
	tok, err := clientset.CoreV1().ServiceAccounts(claimRunnerNamespace).CreateToken(ctx, claimRunnerAccount,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(dir, "apiserver-ca.crt")
	if err := os.WriteFile(caFile, env.Config.CAData, 0o600); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, fmt.Appendf(nil, `apiVersion: v1
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
`, env.Config.Host, caFile, tok.Status.Token), 0o600); err != nil {
		t.Fatal(err)
	}

	gl := fakegitlab.New(fakegitlab.Options{RunnerToken: testRunnerToken, LongPollTimeout: 500 * time.Millisecond})
	if err := gl.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gl.Close)

	s := &claimStack{kube: admin, agent: agent, gitlab: gl, buildDir: filepath.Join(dir, "guest", "builds")}
	cfg := fmt.Sprintf(`
gitlab:
  url: %s
  token: %s
  name: claim-e2e-runner
  allow_insecure: true
  check_interval: 1s
  shutdown_timeout: 10s
pool_manager:
  backend: claim
  claim:
    kubeconfig: %s
    namespace: %s
    holder_service_account: %s
profiles:
  - name: default
    arch: arm64
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    builds_dir: %s
    cache_dir: %s
    ready_timeout: 20s
    shell: %s
    pool:
      size: 1
host_services:
  buildkit:
    port: 1234
observability:
  log_format: text
  listen_address: 127.0.0.1:0
state_dir: %s
`, gl.URL(), testRunnerToken, kubeconfig, claimRunnerNamespace, claimHolderAccount,
		s.buildDir, filepath.Join(dir, "guest", "cache"), bashPath(t), filepath.Join(dir, "state"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, path
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# Where the claim backend is configured, the Executor SHALL run
//# each Stage through the Exec Agent of the Host named in the Job's claim,
//# with the Stage script on standard input.

//= docs/requirements/12-cluster-fleet.md#cluster-least-privilege
//= type=test
//# The Fleet Manifests SHALL grant the Runner only the
//# permissions to manage `MicroVMClaim` and `Pool` resources and to create
//# Secrets in its own namespace, to request tokens of the Holder alone, to
//# read Nodes and to read the serving CA ConfigMap in battery-operator's
//# namespace, and SHALL grant the Holder no permission.

// TestRunRunsAJobOverTheClaimBackend starts the flr binary's `run` on the
// claim backend, as the Runner's ServiceAccount with deploy/runner's
// permissions applied unchanged, against an API server with
// battery-operator's CRDs, the fake battery, a fake Host and the test
// double of battery-operator's Exec Agent. One Job from the fake GitLab
// runs end to end: the Runner declares its Pool and claims a MicroVM from
// it, the Job's Stages reach the MicroVM through the Exec Agent of the
// claim's Host with a claim token of the Holder, the Job sees the Host
// Service the Host's Node publishes, and it succeeds. When the Job ends the
// claim is deleted; SIGTERM then stops the Runner.
func TestRunRunsAJobOverTheClaimBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end")
	}
	bin := buildBinary(t)
	s, path := startClaimStack(t)

	job := &spec.Job{
		ID:         77,
		JobInfo:    spec.JobInfo{Name: "hello", Stage: "test", ProjectID: 7, ProjectName: "demo"},
		GitInfo:    spec.GitInfo{RepoURL: "https://gitlab.example.com/group/demo.git", Sha: "0123456789abcdef0123456789abcdef01234567", Ref: "main"},
		RunnerInfo: spec.RunnerInfo{Timeout: 120},
		Variables: spec.Variables{
			{Key: "GIT_STRATEGY", Value: "none", Public: true},
			{Key: "GREETING", Value: "hello-over-agent-exec", Public: true},
		},
		Steps: spec.Steps{{
			Name:   spec.StepNameScript,
			Script: spec.StepScript{`echo "$GREETING"`, `echo "buildkit is $BUILDKIT_HOST"`, `pwd`},
			When:   spec.StepWhenOnSuccess,
		}},
	}
	if err := s.gitlab.Enqueue(job); err != nil {
		t.Fatal(err)
	}

	r := startRunner(t, bin, path)
	waitFor(t, 90*time.Second, "the job to finish", func() bool {
		select {
		case err := <-r.exited:
			t.Fatalf("runner exited early: %v\n%s", err, r.out.String())
		default:
		}
		rec := s.gitlab.Record(77)
		return rec != nil && (rec.Status == fakegitlab.StatusSuccess || rec.Status == fakegitlab.StatusFailed)
	})
	rec := s.gitlab.Record(77)
	if rec.Status != fakegitlab.StatusSuccess {
		t.Fatalf("job status = %s (%s)\ntrace:\n%s\nrunner:\n%s", rec.Status, rec.FailureReason, rec.Trace, r.out.String())
	}
	for _, want := range []string{"hello-over-agent-exec", "buildkit is tcp://10.200.0.1:1234", s.buildDir} {
		if !strings.Contains(rec.Trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, rec.Trace)
		}
	}

	// Every Stage went through the Exec Agent, each with a claim token it
	// admitted, and nothing it refused.
	execs := 0
	for _, c := range s.agent.Calls() {
		if !c.Admitted {
			t.Errorf("the exec agent refused %s on %q", c.Method, c.VMUID)
		}
		if c.Method == "ExecCommand" {
			execs++
		}
	}
	if execs == 0 {
		t.Error("no Stage reached the exec agent")
	}

	// The Job's claim is gone, and the Runner declared its Pool.
	ctx := context.Background()
	waitFor(t, 20*time.Second, "the job's claim to be deleted", func() bool {
		claims := &batteryv1alpha1.MicroVMClaimList{}
		return s.kube.List(ctx, claims, client.InNamespace(claimRunnerNamespace)) == nil && len(claims.Items) == 0
	})
	pools := &batteryv1alpha1.PoolList{}
	if err := s.kube.List(ctx, pools, client.InNamespace(claimRunnerNamespace)); err != nil || len(pools.Items) != 1 {
		t.Errorf("the Runner's Pools = %d (%v), want the one of its Profile", len(pools.Items), err)
	}

	r.sigterm(t)
	r.waitExit(t, 30*time.Second)
	if out := r.out.String(); strings.Contains(out, "forbidden") {
		t.Errorf("the Runner was refused a call:\n%s", out)
	}
}
