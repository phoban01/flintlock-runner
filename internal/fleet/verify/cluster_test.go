package verify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube/kubetest"
	"github.com/phoban01/flintlock-runner/internal/transport"
	"github.com/phoban01/flintlock-runner/internal/transport/kubeexectest"
)

// The cluster verification tests run against a real kube-apiserver with the
// shipped Host Agent RBAC and admission policy, the real Pod Provider in
// front of the fake Host on each Host, and the Runner's shipped Role as the
// only permissions of the verifier: kubeexectest sets all of that up, as it
// does for the kube-exec transport's own tests. No controller manager runs,
// so kubetest's reconciler stands in for the ReplicaSet controller and its
// garbage collection. Every command the verifier runs in a verification
// pod goes through kube-exec, the API server and the Pod Provider to the
// fake Host, which runs it as a local process: the guest_verify script
// really runs, with curl, against HTTP servers that stand in for the Host
// Services, and with a buildctl stand-in on PATH that reaches the buildkit
// address it is given.

// kubeEnv is the shared API server, nil when there is none.
var kubeEnv *kubeexectest.Env

func TestMain(m *testing.M) {
	env, err := kubeexectest.Start()
	switch {
	case errors.Is(err, kubeexectest.ErrNoAssets):
		// The cluster tests skip; the rest need no API server.
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	default:
		kubeEnv = env
	}
	code := m.Run()
	if kubeEnv != nil {
		_ = kubeEnv.Stop()
	}
	os.Exit(code)
}

// services are the Host Services of one fake Host: an HTTP server per
// service, each of which can be made to fail.
type services struct {
	mu      sync.Mutex
	failing map[string]bool
	hits    map[string][]string
	ports   map[string]int
}

func newServices(t *testing.T) *services {
	t.Helper()
	s := &services{failing: map[string]bool{}, hits: map[string][]string{}, ports: map[string]int{}}
	for _, name := range kubelabels.HostServiceNames() {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			s.hits[name] = append(s.hits[name], r.URL.Path)
			failing := s.failing[name]
			s.mu.Unlock()
			if failing {
				http.Error(w, name+" is broken", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte("ok"))
		}))
		l, err := net.Listen("tcp", net.JoinHostPort(kubeexectest.HostAddress, "0"))
		if err != nil {
			t.Fatal(err)
		}
		srv.Listener = l
		srv.Start()
		t.Cleanup(srv.Close)
		s.ports[name] = l.Addr().(*net.TCPAddr).Port
	}
	return s
}

func (s *services) fail(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[name] = true
}

func (s *services) requests(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits[name]...)
}

// clusterFixture is a namespace standing for the Runner's, the Runner's
// client configuration for it, the ReplicaSet reconciler and the Hosts.
type clusterFixture struct {
	t         *testing.T
	namespace string
	runner    *rest.Config
	hosts     []*kubeexectest.Host
	services  []*services
	cfg       *config.Config
}

// newClusterFixture starts one fake Host per HostOptions. A nil
// HostServices map gives the Host a full set of working Host Services.
func newClusterFixture(t *testing.T, hosts ...kubeexectest.HostOptions) *clusterFixture {
	t.Helper()
	if kubeEnv == nil {
		t.Skip(kubeexectest.ErrNoAssets)
	}
	ns := kubeEnv.Namespace(t)
	f := &clusterFixture{t: t, namespace: ns, runner: kubeEnv.RunnerConfig(t, ns)}
	for _, opts := range hosts {
		var svc *services
		if opts.HostServices == nil {
			svc = newServices(t)
			opts.HostServices = svc.ports
		}
		h := kubeEnv.NewHost(t, opts)
		if svc != nil {
			f.awaitNode(h, true)
		}
		f.hosts = append(f.hosts, h)
		f.services = append(f.services, svc)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&kubetest.ReplicaSets{Client: kubeEnv.Admin, Namespace: ns}).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	f.cfg = &config.Config{
		PoolManager: config.PoolManager{Backend: config.PoolBackendKubernetes},
		Profiles: []config.Profile{{
			Name:   "default",
			Arch:   config.Architecture(runtime.GOARCH),
			VCPU:   1,
			Kernel: config.Kernel{Image: "ghcr.io/example/kernel:6.1", Cmdline: map[string]string{"console": "ttyS0", "quiet": ""}},
			RootFS: "ghcr.io/example/rootfs:ubuntu",
			// The fake Host runs commands on the test machine, which may
			// have no /bin/bash.
			Shell: "bash",
		}},
		HostServices: config.HostServices{HTTPCache: config.HTTPCache{
			Upstreams: []config.HTTPCacheUpstream{{Name: "npm", URL: "https://registry.npmjs.org"}},
		}},
	}
	config.ApplyDefaults(f.cfg)
	return f
}

// verifier is the cluster verifier with the Runner's permissions only,
// confined to the fixture's Virtual Nodes: other tests' Hosts share the API
// server.
func (f *clusterFixture) verifier(timeout time.Duration) *Cluster {
	f.t.Helper()
	// A fixture without Hosts selects a Host nobody has.
	names := []string{"no-host-" + f.namespace}
	for _, h := range f.hosts {
		names = append(names, h.Node)
	}
	sel := labels.SelectorFromSet(labels.Set{kubelabels.LabelVirtualNode: "true"})
	req, err := labels.NewRequirement(kubelabels.LabelHostNode, selection.In, names)
	if err != nil {
		f.t.Fatal(err)
	}
	v, err := NewCluster(ClusterConfig{
		Client:       kubernetes.NewForConfigOrDie(f.runner),
		Namespace:    f.namespace,
		Transports:   transport.NewFactory(transport.WithKubeExec(f.runner, f.namespace)),
		Profiles:     f.cfg.Profiles,
		HostServices: f.cfg.HostServices,
		NodeSelector: sel.Add(*req),
		Timeout:      timeout,
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// noVerificationObjectsLeft checks that the ReplicaSets and pods are gone.
func (f *clusterFixture) noVerificationObjectsLeft() {
	f.t.Helper()
	ctx := context.Background()
	sel := metav1.ListOptions{LabelSelector: LabelVerify}
	deadline := time.Now().Add(kubeexectest.WaitTimeout)
	for {
		sets, err := kubeEnv.Admin.AppsV1().ReplicaSets(f.namespace).List(ctx, sel)
		if err != nil {
			f.t.Fatal(err)
		}
		pods, err := kubeEnv.Admin.CoreV1().Pods(f.namespace).List(ctx, sel)
		if err != nil {
			f.t.Fatal(err)
		}
		if len(sets.Items) == 0 && len(pods.Items) == 0 {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("verification left %d ReplicaSets and %d pods behind", len(sets.Items), len(pods.Items))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// withBuildctl puts a buildctl stand-in on the PATH of the fake Hosts'
// commands, which run as local processes of the test: it reaches the
// buildkit address it is given over HTTP, so the buildkit check passes or
// fails with the Host Service standing in for buildkitd.
func withBuildctl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	stub := `#!/bin/sh
addr=
while [ $# -gt 0 ]; do
	case $1 in --addr) addr=$2; shift ;; esac
	shift
done
exec curl -fsS -o /dev/null "http://${addr#tcp://}/"
`
	if err := os.WriteFile(filepath.Join(dir, "buildctl"), []byte(stub), 0o755); err != nil { //nolint:gosec // an executable stand-in
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func hostFailures(r *fleet.VerifyReport, host string) []fleet.Failure {
	var out []fleet.Failure
	for _, f := range r.Failures {
		if f.Host == host {
			out = append(out, f)
		}
	}
	return out
}

//= docs/requirements/12-cluster-fleet.md#cluster-verification
//= type=test
//# Where the Kubernetes pool backend is configured, the
//# verification command SHALL create one verification pod bound by name to
//# each ready Virtual Node, run a trivial command in it through the
//# `kube-exec` Guest Transport, report the time from creation to readiness
//# per Host and delete the pod.

//= docs/requirements/12-cluster-fleet.md#cluster-verification
//= type=test
//# The verification command SHALL perform the Host Service checks
//# of FL-109 from inside each verification pod.

func TestClusterVerifyExercisesEveryHost(t *testing.T) {
	withBuildctl(t)
	f := newClusterFixture(t, kubeexectest.HostOptions{}, kubeexectest.HostOptions{})
	var out strings.Builder
	v := f.verifier(kubeexectest.WaitTimeout)
	v.cfg.Out = &out

	// Watch the verification pods as they come, to see where they are bound.
	bound := map[string]string{}
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			pods, err := kubeEnv.Admin.CoreV1().Pods(f.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelVerify})
			if err == nil {
				mu.Lock()
				for _, p := range pods.Items {
					bound[p.Name] = p.Spec.NodeName
				}
				mu.Unlock()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	report, err := v.Verify(context.Background())
	cancel()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %v, want none\n%s", report.Failures, out.String())
	}
	if err := Summary(report); err != nil {
		t.Errorf("Summary = %v, want nil for a healthy fleet", err)
	}
	if len(report.Hosts) != 2 {
		t.Fatalf("hosts = %+v, want both Hosts", report.Hosts)
	}
	for i, h := range f.hosts {
		hv := report.Hosts[i]
		if hv.Host != h.Node {
			t.Errorf("host %d = %s, want %s", i, hv.Host, h.Node)
		}
		if !hv.Exercised || hv.ClaimToReady <= 0 {
			t.Errorf("%s: exercised %v in %v, want a trivial command run and the time from creation to readiness", h.Node, hv.Exercised, hv.ClaimToReady)
		}
		want := []string{"buildkit", "go_proxy", "registry_mirror", "http_cache/npm"}
		var got []string
		for _, s := range hv.Services {
			if s.Err != nil {
				t.Errorf("%s: service %s failed: %v", h.Node, s.Service, s.Err)
			}
			got = append(got, s.Service)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: services checked = %v, want %v", h.Node, got, want)
		}
		// Each check reached this Host's own service, at the address its
		// Virtual Node publishes, from inside its pod.
		svc := f.services[i]
		for name, path := range map[string]string{
			"buildkit":        "/",
			"go_proxy":        ".zip",
			"registry_mirror": "/v2/library/alpine/manifests/latest",
			"http_cache":      "/npm/",
		} {
			reqs := svc.requests(name)
			if len(reqs) != 1 || !strings.HasSuffix(reqs[0], path) {
				t.Errorf("%s: %s got %v, want one request for ...%s", h.Node, name, reqs, path)
			}
		}
	}

	mu.Lock()
	nodes := map[string]int{}
	for _, node := range bound {
		nodes[node]++
	}
	mu.Unlock()
	for _, h := range f.hosts {
		if nodes[h.VirtualNode] != 1 {
			t.Errorf("verification pods bound to %s = %d, want one: %v", h.VirtualNode, nodes[h.VirtualNode], bound)
		}
	}
	if len(nodes) != 2 {
		t.Errorf("verification pods were bound to %v, want the two Virtual Nodes only", bound)
	}
	for _, want := range []string{"creation to ready", "ok   " + f.hosts[0].Node + " guest_verify: go_proxy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not report %q:\n%s", want, out.String())
		}
	}
	f.noVerificationObjectsLeft()
}

func TestClusterVerifyReportsServiceFailures(t *testing.T) {
	withBuildctl(t)
	f := newClusterFixture(t, kubeexectest.HostOptions{}, kubeexectest.HostOptions{})
	f.services[1].fail("go_proxy")
	f.services[1].fail("buildkit")

	report, err := f.verifier(kubeexectest.WaitTimeout).Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := hostFailures(report, f.hosts[0].Node); len(got) != 0 {
		t.Errorf("%s failures = %v, want none", f.hosts[0].Node, got)
	}
	got := hostFailures(report, f.hosts[1].Node)
	if len(got) != 2 {
		t.Fatalf("%s failures = %v, want the two failing services", f.hosts[1].Node, got)
	}
	for i, service := range []string{"buildkit", "go_proxy"} {
		if got[i].Step != fleet.StepGuestVerify || !strings.HasPrefix(got[i].Err.Error(), service+": ") || !strings.Contains(got[i].Err.Error(), "502") {
			t.Errorf("failure %d = %s: %v, want %s failing at step %s with the service's answer", i, got[i].Step, got[i].Err, service, fleet.StepGuestVerify)
		}
	}
	if !report.Hosts[1].Exercised {
		t.Errorf("%s was not exercised; a Host Service failure is not a pod failure", f.hosts[1].Node)
	}
	err = Summary(report)
	if err == nil || !strings.Contains(err.Error(), f.hosts[1].Node+": step guest_verify: go_proxy") {
		t.Errorf("Summary = %v, want the Host and the failing service named", err)
	}
	f.noVerificationObjectsLeft()
}

//= docs/requirements/12-cluster-fleet.md#cluster-verification
//= type=test
//# If a Virtual Node is not ready or its verification pod does not
//# become ready within the verification timeout, then the verification
//# command SHALL exit with a non-zero status naming the Host and the
//# reason.

func TestClusterVerifyReportsNotReadyVirtualNode(t *testing.T) {
	withBuildctl(t)
	// The second Host publishes a Host Service nothing listens on, so its
	// Pod Provider reports its Virtual Node not ready (KF-015).
	closed, err := net.Listen("tcp", net.JoinHostPort(kubeexectest.HostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	f := newClusterFixture(t, kubeexectest.HostOptions{}, kubeexectest.HostOptions{
		HostServices: map[string]int{kubelabels.HostServiceGoProxy: closedPort},
	})
	f.awaitNode(f.hosts[1], false)

	report, err := f.verifier(kubeexectest.WaitTimeout).Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := hostFailures(report, f.hosts[0].Node); len(got) != 0 {
		t.Errorf("%s failures = %v, want none", f.hosts[0].Node, got)
	}
	got := hostFailures(report, f.hosts[1].Node)
	if len(got) != 1 || got[0].Step != StepVirtualNode || !strings.Contains(got[0].Err.Error(), "not ready") {
		t.Fatalf("%s failures = %v, want one %s failure saying it is not ready", f.hosts[1].Node, got, StepVirtualNode)
	}
	if report.Hosts[1].Exercised {
		t.Errorf("%s was exercised; a not-ready Virtual Node gets no verification pod", f.hosts[1].Node)
	}
	err = Summary(report)
	if err == nil || !strings.Contains(err.Error(), f.hosts[1].Node+": step virtual_node: virtual node "+f.hosts[1].VirtualNode+" is not ready") {
		t.Errorf("Summary = %v, want the Host and the reason named", err)
	}
	f.noVerificationObjectsLeft()
}

func TestClusterVerifyReportsPodThatNeverBecomesReady(t *testing.T) {
	// The Host's MicroVMs stay PENDING far longer than the verification
	// timeout, so its Pod Provider never reports the pod ready.
	f := newClusterFixture(t, kubeexectest.HostOptions{BootDelay: time.Hour})

	start := time.Now()
	report, err := f.verifier(2 * time.Second).Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("verification took %v; it has to stop at the verification timeout", elapsed)
	}
	got := hostFailures(report, f.hosts[0].Node)
	if len(got) != 1 || got[0].Step != StepPodReady || !strings.Contains(got[0].Err.Error(), "was not ready within 2s") {
		t.Fatalf("failures = %v, want one %s failure naming the timeout", report.Failures, StepPodReady)
	}
	if !strings.Contains(got[0].Err.Error(), "pod flr-verify-") {
		t.Errorf("failure %v does not name the pod and its status", got[0].Err)
	}
	if report.Hosts[0].Exercised {
		t.Error("a pod that never became ready was exercised")
	}
	if err := Summary(report); err == nil || !strings.Contains(err.Error(), f.hosts[0].Node+": step pod_ready:") {
		t.Errorf("Summary = %v, want the Host named", err)
	}
	f.noVerificationObjectsLeft()
}

func TestClusterVerifyWithoutVirtualNodesCannotRun(t *testing.T) {
	t.Parallel()
	f := newClusterFixture(t)
	if _, err := f.verifier(time.Second).Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "no Virtual Node") {
		t.Errorf("Verify = %v, want an error saying there is nothing to verify", err)
	}
}

func TestNewClusterRequiresDependencies(t *testing.T) {
	t.Parallel()
	if _, err := NewCluster(ClusterConfig{}); err == nil {
		t.Error("NewCluster accepted an empty ClusterConfig")
	}
}

// awaitNode waits until the Pod Provider reports the Host's Virtual Node
// ready, or not ready.
func (f *clusterFixture) awaitNode(h *kubeexectest.Host, ready bool) {
	f.t.Helper()
	deadline := time.Now().Add(kubeexectest.WaitTimeout)
	for time.Now().Before(deadline) {
		node, err := kubeEnv.Admin.CoreV1().Nodes().Get(context.Background(), h.VirtualNode, metav1.GetOptions{})
		if err == nil && len(node.Status.Conditions) > 0 {
			if got, _ := nodeReady(node); got == ready {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	f.t.Fatalf("virtual node %s did not become ready=%v\n%s", h.VirtualNode, ready, h.Log())
}
