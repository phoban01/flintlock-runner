//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/kubelet/kubelettest"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// The scenarios of a cluster fleet that have no battery counterpart
// (docs/requirements/12-cluster-fleet.md#cluster-test-doubles). Each runs
// the real flr binary on the Kubernetes stack.

// guardNamePrefix names the Pod Provider's drain guard pod and its
// PodDisruptionBudget after the Host's Node (internal/kubelet).
const guardNamePrefix = "flintlock-drain-guard-"

// kubeStack starts a Stack on the Kubernetes backend with its Runner
// polling, adjusted by adjust.
func newKubeStack(t *testing.T, adjust func(*Options)) *Stack {
	t.Helper()
	opts := KubernetesTier(t, cluster, clusterNote)
	opts.RunnerBinary = runnerBinary
	if adjust != nil {
		adjust(&opts)
	}
	s := New(t, opts)
	t.Cleanup(func() {
		if t.Failed() {
			logStack(t, s)
		}
	})
	if err := startRunner(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// eventually polls ok until it holds, failing the test after a minute.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	if err := poll(context.Background(), time.Minute, ok); err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
}

// pods lists the Stack's pods that match selector and are not being
// deleted.
func pods(t *testing.T, s *Stack, selector labels.Set) []corev1.Pod {
	t.Helper()
	list, err := s.Cluster().Admin().CoreV1().Pods(s.KubeNamespace()).List(context.Background(),
		metav1.ListOptions{LabelSelector: labels.SelectorFromSet(selector).String()})
	if err != nil {
		t.Fatal(err)
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

var (
	idle    = labels.Set{kubelabels.LabelState: kubelabels.StateIdle}
	claimed = labels.Set{kubelabels.LabelState: kubelabels.StateClaimed}
)

// readyIdle counts the ready idle pods, on node when it is not empty.
func readyIdle(t *testing.T, s *Stack, node string) int {
	t.Helper()
	n := 0
	for _, p := range pods(t, s, idle) {
		if podReady(&p) && (node == "" || p.Spec.NodeName == node) {
			n++
		}
	}
	return n
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The harness SHALL include a scenario in which two Runners claim
//# from a Pool of one, and SHALL assert that exactly one obtains the pod.

// TestKubernetesTwoRunnersOnePool runs two replicas of the Runner, the same
// runner name and so the same Pool, sized one. With the one warm pod ready
// and its replacement held back from scheduling, a Job is queued for each
// Runner: both reach for the pod, exactly one claims it and runs its Job,
// and the other waits for the Pool, as it would wait on an exhausted battery
// Pool. Once the replacement may be scheduled the second Job runs on it.
func TestKubernetesTwoRunnersOnePool(t *testing.T) {
	s := newKubeStack(t, func(o *Options) {
		o.Hosts = 1
		// One Job per Runner, so that each Runner takes one of the two.
		o.Configure = func(c *config.Config) { c.GitLab.Concurrent = 1 }
	})
	second, err := s.StartAnotherRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last lines of the second runner's log:\n%s", logTail(second, 40))
		}
	})
	eventually(t, "the second runner to poll for jobs", func() bool {
		return readyIdle(t, s, "") == 1 && containsLine(logTail(second, 200), "reservations may now be granted")
	})
	// Each Runner's log, by its system identifier, which GitLab records
	// with each Job it hands out.
	logs := map[string]string{}
	for dir, log := range map[string]string{"state": s.RunnerLog, "state-2": second} {
		id, err := os.ReadFile(filepath.Join(s.Root, dir, ".runner_system_id"))
		if err != nil {
			t.Fatal(err)
		}
		logs[strings.TrimSpace(string(id))] = log
	}
	const exhausted = "pool is exhausted, waiting for a warm microvm"
	exhaustions := func(log string) int { return strings.Count(logTail(log, 100000), exhausted) }

	// A Runner asks GitLab for a Job only while its Scheduler holds a
	// Reservation, which it grants against the Pool's ready idle pods
	// (GL-030, KF-049), and only while it long polls. Both Runners reach for
	// the one pod only when each is polling as the Jobs arrive; a round in
	// which one of them was between polls, saw the pod claimed and asked for
	// nothing is finished and tried again.
	var (
		ids            []int64
		gates          []*Gate
		winner, loser  int64
		before         map[string]int
		rounds         = 0
		bothHandedOut  bool
		handedOutRound = 5 * time.Second
	)
	for !bothHandedOut {
		rounds++
		if rounds > 5 {
			t.Fatalf("in %d rounds the two runners never both took a job while the pool had its one pod", rounds-1)
		}
		eventually(t, "the pool's one pod ready", func() bool { return readyIdle(t, s, "") == 1 })
		s.PauseScheduling()
		time.Sleep(3 * time.Second)
		before = map[string]int{}
		for id, log := range logs {
			before[id] = exhaustions(log)
		}
		gates = []*Gate{s.NewGate(fmt.Sprintf("a%d", rounds)), s.NewGate(fmt.Sprintf("b%d", rounds))}
		ids = nil
		for i, g := range gates {
			id, err := s.Enqueue(Job{Name: []string{"a", "b"}[i], Script: []string{`echo "job started"`, g.Wait()}})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		// Whatever happens, the pool of one yields one claim.
		deadline := time.Now().Add(handedOutRound)
		for time.Now().Before(deadline) {
			if n := len(pods(t, s, claimed)); n > 1 {
				t.Fatalf("%d pods claimed from a pool of one", n)
			}
			time.Sleep(100 * time.Millisecond)
		}
		a, b := s.GitLab.Record(ids[0]), s.GitLab.Record(ids[1])
		bothHandedOut = a != nil && b != nil && a.SystemID != b.SystemID
		if bothHandedOut {
			break
		}
		t.Logf("round %d: only one runner took a job while the pod was warm; trying again", rounds)
		s.ResumeScheduling()
		for _, g := range gates {
			if err := g.Open(); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range ids {
			wantStatus(t, wait(t, s, id), fakegitlab.StatusSuccess, "")
		}
	}

	if n := len(pods(t, s, claimed)); n != 1 {
		t.Fatalf("%d pods claimed with both jobs handed out, want 1", n)
	}
	started := 0
	for _, id := range ids {
		if containsLine(s.Trace(id), "job started") {
			started++
			winner = id
		} else {
			loser = id
		}
	}
	if started != 1 {
		t.Fatalf("%d jobs started with one pod in the pool, want exactly 1", started)
	}
	firstPod := pods(t, s, claimed)[0].Name
	// The Runner that lost reached for the pod too, and found the Pool
	// exhausted; the one that won did not.
	loserID, winnerID := s.GitLab.Record(loser).SystemID, s.GitLab.Record(winner).SystemID
	eventually(t, "the losing runner to find the pool exhausted", func() bool {
		return exhaustions(logs[loserID]) > before[loserID]
	})
	if exhaustions(logs[winnerID]) > before[winnerID] {
		t.Error("the runner that obtained the pod also found the pool exhausted")
	}

	// The replacement is scheduled; the Runner that lost claims it.
	s.ResumeScheduling()
	waitTrace(t, s, loser, "job started")
	for _, g := range gates {
		if err := g.Open(); err != nil {
			t.Fatal(err)
		}
	}
	w, l := wait(t, s, winner), wait(t, s, loser)
	wantStatus(t, w, fakegitlab.StatusSuccess, "")
	wantStatus(t, l, fakegitlab.StatusSuccess, "")
	vmW := allocation(t, w)
	vmL := allocation(t, l)
	if vmW == vmL {
		t.Errorf("both jobs ran on microvm %s", vmW)
	}
	t.Logf("job %d claimed %s; job %d waited and ran on the replacement", winner, firstPod, loser)
}

func containsLine(text, want string) bool { return strings.Contains(text, want) }

// logCount counts the lines of a log that contain every one of parts; the
// parts are matched whole, so "pod=ns/a" does not match "pod=ns/ab".
func logCount(log string, parts ...string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		words := strings.Fields(line)
		all := true
		for _, p := range parts {
			if !strings.Contains(line, p) || (strings.HasPrefix(p, "pod=") && !slices.Contains(words, p)) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

func logHas(log string, parts ...string) bool { return logCount(log, parts...) > 0 }

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The harness SHALL include a scenario in which a Host's Node is
//# cordoned while it runs a Job, and SHALL assert that the Job finishes, that
//# the idle pods leave the Host and that the drain completes afterwards.

// TestKubernetesCordonDuringAJob cordons the Node of the Host a Job runs on,
// as a MachineDeployment rollout or `kubectl drain` begins. The Job
// finishes; the Host's idle pods leave it and are replaced on the other
// Host (KF-090); the drain cannot evict the Pod Provider's guard while the
// Job runs (KF-091) and can once it has finished (KF-092).
func TestKubernetesCordonDuringAJob(t *testing.T) {
	s := newKubeStack(t, func(o *Options) { o.PoolSize = 4 })
	ctx := context.Background()
	admin := s.Cluster().Admin()
	eventually(t, "the pool of four ready", func() bool { return readyIdle(t, s, "") == 4 })

	gate := s.NewGate("cordon")
	id, err := s.Enqueue(Job{Name: "cordoned", Script: []string{`echo "job started"`, gate.Wait(), `echo "job finished"`}})
	if err != nil {
		t.Fatal(err)
	}
	waitTrace(t, s, id, "job started")
	hosts, err := s.LeaseHosts(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("lease hosts %v (%v), want one", hosts, err)
	}
	host := s.KubeHost(hosts[0])
	other := s.KubeHosts()[0]
	if other == host {
		other = s.KubeHosts()[1]
	}
	eventually(t, "an idle pod on the job's host", func() bool { return readyIdle(t, s, host.VirtualNode) > 0 })

	// The Host's kubelet runs the guard the Pod Provider placed on the
	// Host's own Node when the pod was claimed.
	guardName := guardNamePrefix + host.Node
	var guard *corev1.Pod
	eventually(t, "the drain guard", func() bool {
		guard, err = admin.CoreV1().Pods(kubelettest.AgentNamespace).Get(ctx, guardName, metav1.GetOptions{})
		return err == nil
	})
	guard.Status.Phase = corev1.PodRunning
	guard.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := admin.CoreV1().Pods(kubelettest.AgentNamespace).UpdateStatus(ctx, guard, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	cordon(t, s, host.Node, true)
	eventually(t, "the virtual node cordoned", func() bool {
		n, err := admin.CoreV1().Nodes().Get(ctx, host.VirtualNode, metav1.GetOptions{})
		return err == nil && n.Spec.Unschedulable
	})
	eventually(t, "the idle pods to leave the cordoned host and the pool to refill on the other", func() bool {
		for _, p := range pods(t, s, idle) {
			if p.Spec.NodeName == host.VirtualNode {
				return false
			}
		}
		return readyIdle(t, s, other.VirtualNode) == 4
	})

	// The drain evicts what is on the Host's Node; the guard refuses while
	// the Job runs. One attempt: a refusal is a 429 client-go would retry.
	evict := func() error {
		return admin.CoreV1().RESTClient().Post().
			Namespace(kubelettest.AgentNamespace).Resource("pods").Name(guardName).SubResource("eviction").
			Body(&policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: guardName, Namespace: kubelettest.AgentNamespace}}).
			MaxRetries(0).Do(ctx).Error()
	}
	if err := evict(); !apierrors.IsTooManyRequests(err) {
		t.Fatalf("evicting the guard while the job runs = %v, want it refused by the disruption budget", err)
	}
	if rec := s.GitLab.Record(id); Final(rec.Status) {
		t.Fatalf("the job ended when its host was cordoned: %s\n%s", rec.Status, rec.Trace)
	}

	if err := gate.Open(); err != nil {
		t.Fatal(err)
	}
	rec := wait(t, s, id)
	wantStatus(t, rec, fakegitlab.StatusSuccess, "")
	wantTrace(t, rec, "job finished")

	// Nothing holds the drain now: the guard and its budget go, and no pod
	// of the Runner is left on the cordoned Host.
	eventually(t, "the guard and its budget removed, so the drain completes", func() bool {
		_, podErr := admin.CoreV1().Pods(kubelettest.AgentNamespace).Get(ctx, guardName, metav1.GetOptions{})
		_, pdbErr := admin.PolicyV1().PodDisruptionBudgets(kubelettest.AgentNamespace).Get(ctx, guardName, metav1.GetOptions{})
		return apierrors.IsNotFound(podErr) && apierrors.IsNotFound(pdbErr)
	})
	eventually(t, "no pod left on the drained host", func() bool {
		all, err := admin.CoreV1().Pods(s.KubeNamespace()).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, p := range all.Items {
			if p.Spec.NodeName == host.VirtualNode {
				return false
			}
		}
		return true
	})
	if left, err := host.Fake.Sandboxes(); err != nil || len(left) != 0 {
		t.Errorf("microvms %v (%v) left on the drained host", left, err)
	}
	cordon(t, s, host.Node, false)
}

// cordon marks a Host's Node unschedulable, or schedulable again.
func cordon(t *testing.T, s *Stack, node string, unschedulable bool) {
	t.Helper()
	patch := `{"spec":{"unschedulable":true}}`
	if !unschedulable {
		patch = `{"spec":{"unschedulable":null}}`
	}
	if _, err := s.Cluster().Admin().CoreV1().Nodes().Patch(context.Background(), node, k8stypes.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The harness SHALL include a scenario in which the Pod Provider
//# restarts while a Job runs, and SHALL assert that the Job's MicroVM is
//# adopted and the Job finishes.

// TestKubernetesPodProviderRestartDuringAJob restarts the Pod Provider of
// the Host a Job runs on while the Job's script runs, as a rollout of the
// Host Agent or a crash of its container does: the process goes, and with
// it every connection to its kubelet endpoint, the exec session of the
// running Stage included. The restarted provider adopts the Job's MicroVM
// without creating or restarting it (KF-028, KF-029), the pod stays running,
// the next Stage runs in the same machine and finds what the first left
// there, and the Job finishes. How the severed Stage ends is not asserted
// here: today the Runner counts it a success, which
// TestKnownBugKubernetesSeveredExecCountsAsSuccess shows is wrong.
func TestKubernetesPodProviderRestartDuringAJob(t *testing.T) {
	s := newKubeStack(t, nil)
	ctx := context.Background()
	gate := s.NewGate("restart")
	// after_script is a Stage of its own, run over a new exec session once
	// the script's has ended.
	id, err := s.Enqueue(Job{
		Name:        "restarted",
		Script:      []string{`echo "job started"`, `echo "stage one" > marker`, gate.Wait()},
		AfterScript: []string{`echo "after_script sees: $(cat marker)"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Open() }()
	waitTrace(t, s, id, "job started")
	hosts, err := s.LeaseHosts(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("lease hosts %v (%v), want one", hosts, err)
	}
	host := s.KubeHost(hosts[0])
	jobPod := "pod=" + s.KubeNamespace() + "/" + pods(t, s, claimed)[0].Name
	if err := host.Restart(); err != nil {
		t.Fatal(err)
	}
	// The restarted provider adopts the Job's MicroVM; it creates a MicroVM
	// for the pod only once, before the restart.
	eventually(t, "the restarted provider to adopt the job's microvm", func() bool {
		return logHas(host.Log(), "adopted microvm", jobPod)
	})
	if n := logCount(host.Log(), "created microvm", jobPod); n != 1 {
		t.Errorf("the provider created %d microvms for the job's pod (%s), want 1", n, jobPod)
	}
	if err := gate.Open(); err != nil {
		t.Fatal(err)
	}
	rec := wait(t, s, id)
	if !Final(rec.Status) {
		t.Errorf("job ended %s", rec.Status)
	}
	wantTrace(t, rec, "after_script sees: stage one")
}
