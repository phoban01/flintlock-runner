package kube_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube/kubetest"
)

// roleManifest is the Role the Runner is deployed with (KF-110). Every
// backend in these tests runs as a user bound to exactly this file.
const roleManifest = "../../../deploy/runner/role.yaml"

const (
	runnerName = "runner-a"
	// waitFor bounds every eventually; tick is how often it looks.
	waitFor = 30 * time.Second
	tick    = 20 * time.Millisecond
)

// The API server is shared by the tests of the package, each of which works
// in a namespace of its own. envNote says why there is none.
var (
	env     *kubetest.Env
	envNote string
)

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The Kubernetes pool backend SHALL be tested against a
//# Kubernetes API server test environment, with a test reconciler standing in
//# for the ReplicaSet controller where no controller manager runs.

// TestMain starts the API server test environment: a real kube-apiserver and
// etcd, with kubetest's reconciler for the ReplicaSet controller and its
// kubelet stand-in started per test by newFixture. Where the binaries are
// missing the tests that need them skip with the way to get them, unless
// FLINTLOCK_RUNNER_REQUIRE_ENVTEST is set, as it is in CI, in which case
// the run fails.
func TestMain(m *testing.M) {
	var err error
	env, envNote, err = kubetest.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if env != nil {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stopping the test api server:", err)
		}
	}
	os.Exit(code)
}

// fixture is one test's namespace, with the stand-ins running in it.
type fixture struct {
	t         *testing.T
	ctx       context.Context
	admin     kubernetes.Interface
	namespace string
	nodes     []string
	kubelet   *kubetest.Kubelet
	clock     *clock.Fake
	logs      *logBuffer
	profile   config.Profile
	users     atomic.Int32
}

var namespaces atomic.Int32

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if env == nil {
		t.Skip(envNote)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{
		t:         t,
		ctx:       ctx,
		admin:     env.Admin,
		namespace: fmt.Sprintf("kube-pool-%d", namespaces.Add(1)),
		nodes:     []string{"host-a-microvms", "host-b-microvms"},
		clock:     clock.NewFake(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)),
		logs:      &logBuffer{},
		profile: config.Profile{
			Name:     "default",
			Arch:     config.ArchARM64,
			VCPU:     2,
			MemoryMB: 4096,
			Kernel: config.Kernel{
				Image:   "ghcr.io/example/kernel:6.1",
				Cmdline: map[string]string{"console": "ttyS0", "quiet": ""},
			},
			RootFS:       "ghcr.io/example/ubuntu-ci:24.04",
			Provider:     "firecracker",
			HostSelector: map[string]string{"disk": "nvme", "example.com/zone": "a"},
			Pool: config.PoolSettings{
				Size:              2,
				HeartbeatInterval: 10 * time.Second,
				HeartbeatExpiry:   45 * time.Second,
			},
		},
	}
	_, err := f.admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.namespace}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	f.kubelet = &kubetest.Kubelet{Client: f.admin, Namespace: f.namespace, Nodes: f.nodes}
	replicaSets := &kubetest.ReplicaSets{Client: f.admin, Namespace: f.namespace}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); replicaSets.Run(ctx) }()
	go func() { defer wg.Done(); f.kubelet.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return f
}

// runnerConfig is the client configuration of a new user bound to the
// Runner's Role in the fixture's namespace, and to nothing else.
func (f *fixture) runnerConfig() *rest.Config {
	f.t.Helper()
	user := fmt.Sprintf("%s-runner-%d", f.namespace, f.users.Add(1))
	cfg := env.User(f.t, user)
	subject := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}

	role, clusterRole := loadRoles(f.t)
	role.Namespace = f.namespace
	if _, err := f.admin.RbacV1().Roles(f.namespace).Create(f.ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		f.t.Fatal(err)
	}
	if _, err := f.admin.RbacV1().ClusterRoles().Create(f.ctx, clusterRole, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		f.t.Fatal(err)
	}
	_, err := f.admin.RbacV1().RoleBindings(f.namespace).Create(f.ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: user},
		Subjects:   subject,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
	}, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	_, err = f.admin.RbacV1().ClusterRoleBindings().Create(f.ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: user},
		Subjects:   subject,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole.Name},
	}, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatal(err)
	}

	// The authoriser learns of a binding through its own watch, so the
	// first moments after creating one can still be refused.
	client := kubernetes.NewForConfigOrDie(cfg)
	f.eventually("the role binding takes effect", func() bool {
		_, err := client.AppsV1().ReplicaSets(f.namespace).List(f.ctx, metav1.ListOptions{})
		return err == nil
	})
	return cfg
}

// loadRoles reads the Role and the ClusterRole of deploy/runner/role.yaml.
func loadRoles(t *testing.T) (*rbacv1.Role, *rbacv1.ClusterRole) {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(roleManifest))
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
			t.Fatalf("%s: %v", roleManifest, err)
		}
		switch raw.Kind {
		case "Role":
			role = &rbacv1.Role{ObjectMeta: raw.ObjectMeta, Rules: raw.Rules}
		case "ClusterRole":
			clusterRole = &raw
		case "":
		default:
			t.Fatalf("%s: unexpected kind %q", roleManifest, raw.Kind)
		}
	}
	if role == nil || clusterRole == nil {
		t.Fatalf("%s: want one Role and one ClusterRole", roleManifest)
	}
	return role, clusterRole
}

// backend builds a backend that runs as a fresh user with the Runner's Role.
// mutate, when given, adjusts the options and may replace the client.
func (f *fixture) backend(mutate ...func(*kube.Options, *rest.Config)) *kube.Backend {
	f.t.Helper()
	cfg := f.runnerConfig()
	opts := kube.Options{
		Namespace:       f.namespace,
		RunnerName:      runnerName,
		Profiles:        []config.Profile{f.profile},
		JobTimeout:      time.Hour,
		RolloutInterval: time.Minute,
		Clock:           f.clock,
		Log:             slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, m := range mutate {
		m(&opts, cfg)
	}
	if opts.Client == nil {
		opts.Client = kubernetes.NewForConfigOrDie(cfg)
	}
	b, err := kube.New(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithTimeout(f.ctx, waitFor)
	defer cancel()
	if err := b.WaitForSync(ctx); err != nil {
		f.t.Fatal(err)
	}
	return b
}

// spec derives the Pool's spec from the fixture's Profile exactly as the
// Scheduler does, with the real SpecBuilder.
func (f *fixture) spec() poolmgr.PoolSpec {
	f.t.Helper()
	spec, err := poolmgr.NewSpecBuilder().Build(f.profile, poolmgr.SpecInput{RunnerName: runnerName, Namespace: "ci"})
	if err != nil {
		f.t.Fatal(err)
	}
	return spec
}

// declare declares the fixture's Pool through the real Declarer and waits
// for its warm pods.
func (f *fixture) declare(b *kube.Backend) poolmgr.PoolRef {
	f.t.Helper()
	spec := f.spec()
	if _, err := poolmgr.NewDeclarer(b).Declare(f.ctx, spec); err != nil {
		f.t.Fatal(err)
	}
	f.waitAvailable(b, spec.Ref, spec.Size)
	return spec.Ref
}

// waitAvailable waits until GetPool reports n available pods and the
// backend's own watch has caught up with them.
func (f *fixture) waitAvailable(b *kube.Backend, ref poolmgr.PoolRef, n int32) {
	f.t.Helper()
	f.eventually(fmt.Sprintf("%d available in %s", n, ref), func() bool {
		pool, err := b.GetPool(f.ctx, ref)
		return err == nil && pool.Status.Available == n && pool.Status.Provisioning == 0
	})
}

// pods lists the namespace's pods that are not being deleted.
func (f *fixture) pods() []corev1.Pod {
	f.t.Helper()
	list, err := f.admin.CoreV1().Pods(f.namespace).List(f.ctx, metav1.ListOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	out := list.Items[:0]
	for _, pod := range list.Items {
		if pod.DeletionTimestamp == nil {
			out = append(out, pod)
		}
	}
	return out
}

// podsInState are the pods carrying that state label.
func (f *fixture) podsInState(state string) []corev1.Pod {
	var out []corev1.Pod
	for _, pod := range f.pods() {
		if pod.Labels[kube.LabelState] == state {
			out = append(out, pod)
		}
	}
	return out
}

func (f *fixture) pod(name string) *corev1.Pod {
	f.t.Helper()
	pod, err := f.admin.CoreV1().Pods(f.namespace).Get(f.ctx, name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return pod
}

// eventually polls cond until it holds.
func (f *fixture) eventually(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for: %s\nlogs:\n%s", what, f.logs.String())
		}
		time.Sleep(tick)
	}
}

// consistently checks that cond holds for a while.
func (f *fixture) consistently(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			f.t.Fatalf("stopped holding: %s\nlogs:\n%s", what, f.logs.String())
		}
		time.Sleep(tick)
	}
}

// logBuffer is a concurrency-safe log sink.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
