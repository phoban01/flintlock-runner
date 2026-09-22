package kube_test

import (
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube"
)

// byTemplate counts the idle pods of a template hash and of every other.
func (f *fixture) byTemplate(hash string) (current, previous int) {
	for _, pod := range f.podsInState(kube.StateIdle) {
		if pod.Labels[kube.LabelTemplateHash] == hash {
			current++
		} else {
			previous++
		}
	}
	return current, previous
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# When a Profile's pod template changes, the Scheduler SHALL
//# delete the idle pods of the previous template at no more than the
//# configured rate and SHALL NOT delete or modify a claimed pod.

// TestTemplateRollout changes a Profile under a Pool of three with one Lease
// out. Two instances of the Runner roll the Pool, on a fake clock and a
// rollout interval of a minute. However often either of them makes a pass,
// one idle pod of the previous template goes per minute of that clock and no
// more, each is replaced from the new template, a pod of the previous
// template is never claimed, and the claimed pod is, at the end, the very
// object it was when it was claimed.
func TestTemplateRollout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.profile.Pool.Size = 3
	runners := []*kube.Backend{f.backend(), f.backend()}
	b := runners[0]
	ref := f.declare(b)
	claim, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	f.waitAvailable(b, ref, 3)
	claimed := f.pod(claim.LeaseID)
	oldHash := f.replicaSet().Spec.Template.Labels[kube.LabelTemplateHash]

	// The Profile changes: every MicroVM gets more memory.
	f.profile.MemoryMB = 8192
	if _, err := poolmgr.NewDeclarer(b).Declare(f.ctx, f.spec()); err != nil {
		t.Fatal(err)
	}
	newHash := f.replicaSet().Spec.Template.Labels[kube.LabelTemplateHash]
	if newHash == oldHash || newHash == "" {
		t.Fatalf("template hash %q did not change from %q", newHash, oldHash)
	}

	// Pods of the previous template are warm but are not handed out.
	f.eventually("the previous template's pods stop being available", func() bool {
		pool, err := b.GetPool(f.ctx, ref)
		return err == nil && pool.Status.Available == 0
	})
	if _, err := b.ClaimVM(f.ctx, ref); !errors.Is(err, poolmgr.ErrExhausted) {
		t.Fatalf("claim with only previous-template pods = %v, want ErrExhausted", err)
	}

	rollAll := func() {
		for _, r := range runners {
			r.Rollout(f.ctx)
		}
	}
	for left := 2; left >= 0; left-- {
		f.eventually("one previous-template pod is rolled", func() bool {
			rollAll()
			_, previous := f.byTemplate(newHash)
			if previous < left {
				t.Fatalf("%d previous-template pods left, want no fewer than %d within this interval", previous, left)
			}
			return previous == left
		})
		f.consistently("no second pod is rolled within the interval", consistentFor, func() bool {
			rollAll()
			_, previous := f.byTemplate(newHash)
			return previous == left
		})
		f.eventually("the rolled pod is replaced from the new template", func() bool {
			current, _ := f.byTemplate(newHash)
			return current == 3-left
		})
		// Just short of the interval nothing moves; at the interval the
		// next pod may go.
		f.clock.Advance(time.Minute - time.Second)
		f.consistently("the interval has not elapsed", consistentFor, func() bool {
			rollAll()
			_, previous := f.byTemplate(newHash)
			return previous == left
		})
		f.clock.Advance(time.Second)
	}

	f.waitAvailable(b, ref, 3)
	for _, pod := range f.podsInState(kube.StateIdle) {
		if got := pod.Spec.Containers[0].Resources.Limits.Memory().Value(); got != 8192<<20 {
			t.Errorf("pod %s memory limit = %d, want the new template's 8192 MiB", pod.Name, got)
		}
	}

	after := f.pod(claim.LeaseID)
	if after.UID != claimed.UID || after.ResourceVersion != claimed.ResourceVersion || after.DeletionTimestamp != nil {
		t.Errorf("the claimed pod was touched by the rollout: resource version %s, was %s", after.ResourceVersion, claimed.ResourceVersion)
	}
	if after.Labels[kube.LabelTemplateHash] != oldHash || after.Status.Phase != corev1.PodRunning {
		t.Errorf("the claimed pod changed: %+v", after.ObjectMeta)
	}
}
