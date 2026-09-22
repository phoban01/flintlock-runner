package kube_test

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube"
)

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# When claiming from a Pool, the Scheduler SHALL choose a ready
//# idle pod of that Pool's current template and SHALL set its state label to
//# `claimed`, its lease annotation to the current time and its active
//# deadline to the Job timeout in one update conditioned on the pod's
//# resource version.

// TestClaimUpdatesThePodOnce claims a pod and checks the three fields, and
// that they arrived in a single write: the pod's resource version moved
// exactly once between the idle pod and the claimed one the update returned.
// It then checks what the claim sets off: the pod has left the ReplicaSet and
// a replacement brings the Pool back to size.
func TestClaimUpdatesThePodOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var puts putCounter
	b := f.backend(func(_ *kube.Options, cfg *rest.Config) { cfg.Wrap(puts.wrap) })
	ref := f.declare(b)
	idle := map[string]corev1.Pod{}
	for _, pod := range f.podsInState(kube.StateIdle) {
		idle[pod.Name] = pod
	}

	claim, err := b.ClaimVM(kube.WithJobTimeout(f.ctx, 90*time.Second), ref)
	if err != nil {
		t.Fatal(err)
	}
	before, wasIdle := idle[claim.LeaseID]
	if !wasIdle {
		t.Fatalf("lease %q is none of the ready idle pods %v", claim.LeaseID, idle)
	}
	pod := f.pod(claim.LeaseID)
	if got := pod.Labels[kube.LabelState]; got != kube.StateClaimed {
		t.Errorf("state label = %q, want claimed", got)
	}
	lease, ok := kube.LeaseTime(pod)
	if !ok || !lease.Equal(f.clock.Now()) {
		t.Errorf("lease annotation = %q, want the current time %s", pod.Annotations[kube.AnnotationLease], f.clock.Now())
	}
	if d := pod.Spec.ActiveDeadlineSeconds; d == nil || *d != 90 {
		t.Errorf("active deadline = %v, want the job timeout of 90s", d)
	}
	if n := puts.count(claim.LeaseID); n != 1 {
		t.Errorf("%d updates of the claimed pod, want one", n)
	}
	if before.Spec.ActiveDeadlineSeconds != nil || before.Annotations[kube.AnnotationLease] != "" {
		t.Errorf("the idle pod already carried a deadline or a lease: %+v", before.ObjectMeta)
	}

	// Without a Job timeout on the context the configured one applies.
	second, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if d := f.pod(second.LeaseID).Spec.ActiveDeadlineSeconds; d == nil || *d != 3600 {
		t.Errorf("active deadline = %v, want the configured 3600s", d)
	}

	// Both claimed pods are out of the selector, so the Pool is refilled.
	f.waitAvailable(b, ref, 2)
	pool, err := b.GetPool(f.ctx, ref)
	if err != nil || pool.Status.Leased != 2 {
		t.Errorf("pool status = %+v, %v, want 2 leased", pool, err)
	}
	f.eventually("the claimed pod is released by the replicaset", func() bool {
		return len(f.pod(claim.LeaseID).OwnerReferences) == 0
	})
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# The Scheduler SHALL return the claimed pod's name as the Lease
//# id and the pod's Virtual Node as the Placement.

func TestClaimReturnsThePodAndItsVirtualNode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)

	claim, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	pod := f.pod(claim.LeaseID)
	if claim.LeaseID != pod.Name {
		t.Errorf("lease id = %q, want the pod name %q", claim.LeaseID, pod.Name)
	}
	if claim.Host.Name == "" || claim.Host.Name != pod.Spec.NodeName {
		t.Errorf("host = %q, want the pod's virtual node %q", claim.Host.Name, pod.Spec.NodeName)
	}
	if claim.Host.Name != f.nodes[0] && claim.Host.Name != f.nodes[1] {
		t.Errorf("host = %q, want one of %v", claim.Host.Name, f.nodes)
	}
	if claim.VMUID != string(pod.UID) {
		t.Errorf("vm uid = %q, want the pod uid %q", claim.VMUID, pod.UID)
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# If the claim update is rejected because the pod changed, then
//# the Scheduler SHALL try another ready idle pod, and SHALL treat the Pool
//# as exhausted only when none is left.

// TestClaimConflict gets between the backend and the API server. The moment
// the backend sends its first claim update, an administrator claims that very
// pod, so the update is rejected with a conflict; the backend has to come
// back with the other pod. With that one gone as well, and the replacements
// held back, the next rejected claim leaves nothing, and only then is the
// Pool exhausted.
func TestClaimConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var steal stealer
	b := f.backend(func(_ *kube.Options, cfg *rest.Config) { cfg.Wrap(steal.wrap) })
	ref := f.declare(b)
	f.kubelet.Pause()

	steal.arm(f)
	claim, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatalf("claim after a conflict = %v, want the other pod", err)
	}
	stolen := steal.stolen()
	if stolen == "" {
		t.Fatal("no claim update was intercepted")
	}
	if claim.LeaseID == stolen {
		t.Fatalf("the backend was given %s, which another runner had claimed first", stolen)
	}
	if got := f.pod(stolen).Annotations["stolen-by"]; got != "test" {
		t.Errorf("the stolen pod was overwritten: annotations %v", f.pod(stolen).Annotations)
	}

	// A conflict that is not a claim, such as a status update by the Pod
	// Provider, does not cost the pod: the same one is claimed on its new
	// resource version.
	f.kubelet.Resume()
	f.waitAvailable(b, ref, 2)
	f.kubelet.Pause()
	steal.armTouch(f)
	claim, err = b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatalf("claim after a harmless conflict = %v", err)
	}
	if touched := steal.stolen(); claim.LeaseID != touched {
		t.Errorf("claimed %s, want the pod %s whose update was retried", claim.LeaseID, touched)
	}

	// One ready idle pod is left. It is stolen too, and nothing is left.
	steal.arm(f)
	if _, err := b.ClaimVM(f.ctx, ref); !errors.Is(err, poolmgr.ErrExhausted) {
		t.Fatalf("claim with every pod taken = %v, want ErrExhausted", err)
	}
	if steal.stolen() == "" {
		t.Error("the pool was reported exhausted without trying its last pod")
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The harness SHALL include a scenario in which two Runners claim
//# from a Pool of one, and SHALL assert that exactly one obtains the pod.

// TestTwoRunnersClaimFromAPoolOfOne runs two instances of a Runner, each a
// backend with its own client and its own watch, against a Pool of one. Both
// see the pod ready and idle, both claim at the same moment, and exactly one
// gets it; the other finds the Pool exhausted, which is what makes its
// Scheduler wait. The replacement is held back during the race so that the
// loser cannot be handed a second pod, then let through for the next round.
func TestTwoRunnersClaimFromAPoolOfOne(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.profile.Pool.Size = 1
	runners := []*kube.Backend{f.backend(), f.backend()}
	ref := f.declare(runners[0])
	if _, err := poolmgr.NewDeclarer(runners[1]).Declare(f.ctx, f.spec()); err != nil {
		t.Fatalf("the second runner declaring the same pool: %v", err)
	}

	for round := 0; round < 5; round++ {
		for _, b := range runners {
			f.waitAvailable(b, ref, 1)
		}
		f.kubelet.Pause()

		var (
			start  = make(chan struct{})
			wg     sync.WaitGroup
			claims = make([]*poolmgr.Claim, len(runners))
			errs   = make([]error, len(runners))
		)
		for i, b := range runners {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				claims[i], errs[i] = b.ClaimVM(f.ctx, ref)
			}()
		}
		close(start)
		wg.Wait()

		winner := -1
		for i := range runners {
			switch {
			case errs[i] == nil && winner >= 0:
				t.Fatalf("round %d: both runners obtained a pod: %s and %s", round, claims[winner].LeaseID, claims[i].LeaseID)
			case errs[i] == nil:
				winner = i
			case !errors.Is(errs[i], poolmgr.ErrExhausted):
				t.Fatalf("round %d: runner %d: %v, want ErrExhausted", round, i, errs[i])
			}
		}
		if winner < 0 {
			t.Fatalf("round %d: neither runner obtained the pod: %v", round, errs)
		}
		if claimed := f.podsInState(kube.StateClaimed); len(claimed) != 1 || claimed[0].Name != claims[winner].LeaseID {
			t.Fatalf("round %d: claimed pods = %v, want only %s", round, names(claimed), claims[winner].LeaseID)
		}

		if err := runners[winner].ReleaseVM(f.ctx, claims[winner].LeaseID); err != nil {
			t.Fatal(err)
		}
		f.kubelet.Resume()
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# When a Lease heartbeat is due, the Scheduler SHALL update the
//# pod's lease annotation.

func TestHeartbeatRenewsTheLeaseAnnotation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)
	claim, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	claimedAt := f.clock.Now()

	f.clock.Advance(10 * time.Second)
	expires, err := b.Heartbeat(f.ctx, claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	lease, ok := kube.LeaseTime(f.pod(claim.LeaseID))
	if !ok || !lease.Equal(claimedAt.Add(10*time.Second)) {
		t.Errorf("lease annotation = %v, want the heartbeat time %v", lease, claimedAt.Add(10*time.Second))
	}
	if want := f.clock.Now().Add(f.profile.Pool.HeartbeatExpiry); !expires.Equal(want) {
		t.Errorf("expiry = %v, want the heartbeat plus the pool's expiry threshold, %v", expires, want)
	}
	if got := f.pod(claim.LeaseID).Labels[kube.LabelState]; got != kube.StateClaimed {
		t.Errorf("state label = %q after a heartbeat, want claimed", got)
	}

	// A Lease that does not exist, SC-061's signal, is ErrNotFound: a pod
	// that was never there, and one that is there but was never claimed.
	if _, err := b.Heartbeat(f.ctx, "no-such-pod"); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("heartbeat of an unknown lease = %v, want ErrNotFound", err)
	}
	idle := f.podsInState(kube.StateIdle)
	if len(idle) == 0 {
		t.Fatal("no idle pod left to heartbeat")
	}
	if _, err := b.Heartbeat(f.ctx, idle[0].Name); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("heartbeat of an idle pod = %v, want ErrNotFound", err)
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# When a Lease is released, the Scheduler SHALL delete the pod.

func TestReleaseDeletesThePod(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)
	claim, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseVM(f.ctx, claim.LeaseID); err != nil {
		t.Fatal(err)
	}
	f.eventually("the released pod is gone", func() bool {
		_, err := f.admin.CoreV1().Pods(f.namespace).Get(f.ctx, claim.LeaseID, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
	if err := b.ReleaseVM(f.ctx, claim.LeaseID); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("second release = %v, want ErrNotFound, which counts as released", err)
	}

	// A Lease id never deletes a pod that is not a Lease.
	f.waitAvailable(b, ref, 2)
	idle := f.podsInState(kube.StateIdle)[0]
	if err := b.ReleaseVM(f.ctx, idle.Name); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("release of an idle pod = %v, want ErrNotFound", err)
	}
	if pod := f.pod(idle.Name); pod.DeletionTimestamp != nil {
		t.Error("releasing an idle pod's name deleted it")
	}
}

func names(pods []corev1.Pod) []string {
	out := make([]string, 0, len(pods))
	for _, pod := range pods {
		out = append(out, pod.Name)
	}
	return out
}

// podOfUpdate is the pod a request updates, or empty.
func podOfUpdate(req *http.Request) string {
	if req.Method != http.MethodPut || !strings.HasSuffix(path.Dir(req.URL.Path), "/pods") {
		return ""
	}
	return path.Base(req.URL.Path)
}

type roundTripper func(*http.Request) (*http.Response, error)

func (rt roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return rt(req) }

// putCounter counts the pod updates a backend sends, by pod.
type putCounter struct {
	mu   sync.Mutex
	puts map[string]int
}

func (c *putCounter) wrap(next http.RoundTripper) http.RoundTripper {
	return roundTripper(func(req *http.Request) (*http.Response, error) {
		if pod := podOfUpdate(req); pod != "" {
			c.mu.Lock()
			if c.puts == nil {
				c.puts = map[string]int{}
			}
			c.puts[pod]++
			c.mu.Unlock()
		}
		return next.RoundTrip(req)
	})
}

func (c *putCounter) count(pod string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts[pod]
}

// stealer changes a pod behind the backend's back, just before the first
// claim update of a still idle pod that the backend sends after it was armed
// reaches the API server. An update of a pod that is claimed already, which a
// backend whose watch is a moment behind may still send, is let through to
// fail on its own.
type stealer struct {
	mu     sync.Mutex
	idle   func(pod string) bool
	action func(pod string)
	took   string
}

func (s *stealer) wrap(next http.RoundTripper) http.RoundTripper {
	return roundTripper(func(req *http.Request) (*http.Response, error) {
		if pod := podOfUpdate(req); pod != "" && s.armedFor(pod) {
			s.mu.Lock()
			action := s.action
			s.action = nil
			if action != nil {
				s.took = pod
			}
			s.mu.Unlock()
			if action != nil {
				action(pod)
			}
		}
		return next.RoundTrip(req)
	})
}

// armedFor reports whether the stealer is armed and the pod still idle.
func (s *stealer) armedFor(pod string) bool {
	s.mu.Lock()
	idle := s.idle
	armed := s.action != nil
	s.mu.Unlock()
	return armed && idle != nil && idle(pod)
}

// arm makes the next claim update lose its pod to another claim.
func (s *stealer) arm(f *fixture) {
	s.watch(f)
	s.set(func(name string) {
		f.mutate(name, func(pod *corev1.Pod) {
			pod.Labels[kube.LabelState] = kube.StateClaimed
			pod.Annotations["stolen-by"] = "test"
		})
	})
}

// armTouch makes the next claim update conflict with a change that leaves
// the pod claimable.
func (s *stealer) armTouch(f *fixture) {
	s.watch(f)
	s.set(func(name string) {
		f.mutate(name, func(pod *corev1.Pod) { pod.Annotations["touched-by"] = "test" })
	})
}

// watch tells the stealer how to ask the API server whether a pod is idle.
func (s *stealer) watch(f *fixture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idle = func(name string) bool {
		pod, err := f.admin.CoreV1().Pods(f.namespace).Get(f.ctx, name, metav1.GetOptions{})
		return err == nil && pod.Labels[kube.LabelState] == kube.StateIdle
	}
}

func (s *stealer) set(action func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.action, s.took = action, ""
}

func (s *stealer) stolen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.took
}

// mutate updates a pod as the administrator.
func (f *fixture) mutate(name string, change func(*corev1.Pod)) {
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	for {
		pod, err := f.admin.CoreV1().Pods(f.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			f.t.Errorf("reading %s: %v", name, err)
			return
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		change(pod)
		_, err = f.admin.CoreV1().Pods(f.namespace).Update(ctx, pod, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			f.t.Errorf("updating %s: %v", name, err)
		}
		return
	}
}
