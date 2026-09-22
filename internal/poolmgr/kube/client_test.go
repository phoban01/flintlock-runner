package kube_test

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube"
)

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# The Kubernetes pool backend SHALL implement the same
//# `poolmgr.Client` interface as the battery client, so that the Scheduler's
//# requirements in `03-scheduler.md` hold unchanged over either.

// TestBackendIsAPoolManagerClient drives the backend through the units the
// Scheduler is composed of, each of which takes a part of poolmgr.Client and
// knows nothing of Kubernetes: the Declarer declares the Pool, the Claimer
// claims from it and turns an empty Pool into the ErrExhausted the allocation
// loop waits on, and the Tracker it reports to has counted the Pool down.
func TestBackendIsAPoolManagerClient(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.profile.Pool.Size = 1
	var client poolmgr.Client = f.backend()
	b := client.(*kube.Backend)

	ref := f.declare(b)
	tracker := f.tracker(b, ref)
	f.eventually("the tracker counts the pool", func() bool { return tracker.Available(ref) == 1 })
	claimer, err := poolmgr.NewClaimer(poolmgr.ClaimerConfig{Lease: client, Tracker: tracker})
	if err != nil {
		t.Fatal(err)
	}

	f.kubelet.Pause()
	claim, err := claimer.Claim(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimer.Claim(f.ctx, ref); !errors.Is(err, poolmgr.ErrExhausted) {
		t.Fatalf("claim from the emptied pool = %v, want ErrExhausted", err)
	}
	if got := tracker.Available(ref); got != 0 {
		t.Errorf("available = %d after exhaustion, want 0", got)
	}
	if err := client.ReleaseVM(f.ctx, claim.LeaseID); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReplicaSetStandIn checks the test reconciler itself against what the
// backend relies on the real controller for: pods up to the replica count,
// a pod that leaves the selector released and replaced, surplus pods removed
// and a deleted ReplicaSet's pods deleted with it.
func TestReplicaSetStandIn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	labels := map[string]string{"app": "stand-in", "state": "idle"}
	replicas := int32(2)
	rs, err := f.admin.AppsV1().ReplicaSets(f.namespace).Create(f.ctx, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "stand-in"},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.com/image:1"}}},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	readyPods := func(n int) func() bool {
		return func() bool {
			ready := 0
			for _, pod := range f.pods() {
				if pod.Labels["state"] != "idle" {
					continue
				}
				for _, c := range pod.Status.Conditions {
					if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && pod.Spec.NodeName != "" {
						ready++
					}
				}
			}
			return ready == n && len(f.pods()) >= n
		}
	}
	f.eventually("two ready pods", readyPods(2))

	taken := f.pods()[0].Name
	f.mutate(taken, func(pod *corev1.Pod) { pod.Labels["state"] = "claimed" })
	f.eventually("the relabelled pod is released and replaced", func() bool {
		return len(f.pod(taken).OwnerReferences) == 0 && readyPods(2)() && len(f.pods()) == 3
	})

	replicas = 1
	rs, err = f.admin.AppsV1().ReplicaSets(f.namespace).Get(f.ctx, rs.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rs.Spec.Replicas = &replicas
	if _, err := f.admin.AppsV1().ReplicaSets(f.namespace).Update(f.ctx, rs, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.eventually("the surplus pod is deleted", func() bool { return len(f.pods()) == 2 })

	if err := f.admin.AppsV1().ReplicaSets(f.namespace).Delete(f.ctx, rs.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.eventually("only the released pod outlives the replicaset", func() bool {
		pods := f.pods()
		return len(pods) == 1 && pods[0].Name == taken
	})
}
