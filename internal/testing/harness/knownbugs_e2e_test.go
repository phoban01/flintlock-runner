//go:build e2e

package harness

import (
	"context"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// The tests in this file demonstrate defects the cluster stack found in
// production code. They fail today, so they run only where
// FLINTLOCK_RUNNER_E2E_KNOWN_BUGS is set to 1; once a defect is fixed its
// test passes and moves into the scenarios.

const envKnownBugs = "FLINTLOCK_RUNNER_E2E_KNOWN_BUGS"

func knownBug(t *testing.T) {
	t.Helper()
	if os.Getenv(envKnownBugs) != "1" {
		t.Skipf("demonstrates a known defect; set %s=1 to run it", envKnownBugs)
	}
}

// TestKnownBugKubernetesUnresponsiveHost: when the flintlockd of the Host a
// Job runs on stops answering, the battery stack fails the Job as a system
// failure within seconds (SC-043). On the cluster stack the Pod Provider
// gives up on the stalled stage after its 5s probe deadline and reports the
// Virtual Node not ready, but the Runner then opens the next stage's exec,
// the provider's relay waits to open an exec stream on flintlockd with no
// deadline (internal/transport/exec.go opens with the request's context
// only), and the claimed pod stays Running and Ready. Nothing fails the
// Job: it runs out its own timeout, ten minutes by default here.
func TestKnownBugKubernetesUnresponsiveHost(t *testing.T) {
	knownBug(t)
	s := newKubeStack(t, nil)
	gate := s.NewGate("host")
	id, err := s.Enqueue(Job{Name: "stranded", Script: []string{`echo "job started"`, gate.Wait()}})
	if err != nil {
		t.Fatal(err)
	}
	waitTrace(t, s, id, "job started")
	host := leaseHost(t, s)
	host.SetFaults(flintlock.HostFaults{Unresponsive: true})
	defer host.SetFaults(flintlock.HostFaults{})
	defer func() { _ = gate.Open() }()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rec, err := s.Wait(ctx, id)
	if err != nil {
		t.Fatalf("the job did not end within 90s of its host becoming unresponsive: %v", err)
	}
	wantStatus(t, rec, fakegitlab.StatusFailed, "runner_system_failure")
}

// TestKnownBugKubernetesSeveredExecCountsAsSuccess: when the exec session
// of a running Stage is cut - here the Pod Provider restarts, as a Host
// Agent rollout does, and its connections go with the process - the
// kube-exec Guest Transport reports exit status 0. client-go's
// StreamWithContext returns nil when the session closes without a status
// message, and internal/transport/kubeexec.go maps a nil error to success,
// so the Runner moves on as if the script had finished: the Job succeeds
// without its script ever reaching its last line. The exec transport treats
// a stream that ends without an exit status as a failure (EX-023), which
// KF-061 requires of kube-exec too.
func TestKnownBugKubernetesSeveredExecCountsAsSuccess(t *testing.T) {
	knownBug(t)
	s := newKubeStack(t, nil)
	gate := s.NewGate("severed")
	id, err := s.Enqueue(Job{Name: "severed", Script: []string{`echo "job started"`, gate.Wait(), `echo "script reached its end"`}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Open() }()
	waitTrace(t, s, id, "job started")
	hosts, err := s.LeaseHosts(context.Background())
	if err != nil || len(hosts) != 1 {
		t.Fatalf("lease hosts %v (%v), want one", hosts, err)
	}
	if err := s.KubeHost(hosts[0]).Restart(); err != nil {
		t.Fatal(err)
	}
	// The gate stays closed: the script cannot have reached its end.
	rec := wait(t, s, id)
	if rec.Status == fakegitlab.StatusSuccess {
		t.Errorf("the job succeeded although its script was cut off before its last line:\n%s", rec.Trace)
	}
}

// TestKnownBugKubernetesReleasedPodLingers: the Pod Provider deletes a
// released pod's MicroVM at once (KF-027), but the pod stays in the API
// server, bound to the Virtual Node and counted against its pod limit
// (KF-012), for the whole termination grace period: the Pool's pod template
// sets none, so thirty seconds after every Job. A kubelet removes a pod
// whose containers have terminated without waiting that out.
func TestKnownBugKubernetesReleasedPodLingers(t *testing.T) {
	knownBug(t)
	s := newKubeStack(t, func(o *Options) { o.Hosts = 1 })
	gate := s.NewGate("short")
	id, err := s.Enqueue(Job{Name: "short", Script: []string{`echo "job started"`, gate.Wait()}})
	if err != nil {
		t.Fatal(err)
	}
	waitTrace(t, s, id, "job started")
	claimedPods := pods(t, s, claimed)
	if len(claimedPods) != 1 {
		t.Fatalf("%d claimed pods, want 1", len(claimedPods))
	}
	pod := claimedPods[0].Name
	if err := gate.Open(); err != nil {
		t.Fatal(err)
	}
	rec := wait(t, s, id)
	wantStatus(t, rec, fakegitlab.StatusSuccess, "")
	released := time.Now()
	admin := s.Cluster().Admin()
	eventually(t, "the released pod to leave the API server", func() bool {
		_, err := admin.CoreV1().Pods(s.KubeNamespace()).Get(context.Background(), pod, metav1.GetOptions{})
		return err != nil
	})
	if d := time.Since(released); d > 5*time.Second {
		t.Errorf("the released pod %s stayed in the API server for %s after its job, its microvm gone", pod, d.Round(time.Second))
	}
}
