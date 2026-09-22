package kube

import (
	"context"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// runRollout runs a rollout pass every rollout interval until ctx ends.
func (b *Backend) runRollout(ctx context.Context) {
	timer := b.opts.Clock.NewTimer(b.opts.RolloutInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		}
		b.Rollout(ctx)
		timer.Reset(b.opts.RolloutInterval)
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# When a Profile's pod template changes, the Scheduler SHALL
//# delete the idle pods of the previous template at no more than the
//# configured rate and SHALL NOT delete or modify a claimed pod.

// Rollout makes one pass over the Runner's Pools and deletes, from each Pool
// that is due, at most one idle pod of a previous template; the ReplicaSet
// controller replaces it from the current one. The rollout loop calls it
// every rollout interval, so a Pool loses warm MicroVMs no faster than one
// per interval and is never emptied by a Profile change.
//
// The rate is the Pool's, not this process's. The time of the last deletion
// is an annotation on the ReplicaSet, written by an update conditioned on
// the ReplicaSet's resource version before the pod is deleted, so of several
// instances of one Runner that find the Pool due at the same moment one
// takes the turn and the others skip it.
//
// A claimed pod is out of reach twice over. Only pods that carry the idle
// state label are considered, and the deletion is conditioned on the pod's
// UID and resource version, so a pod claimed between being read and being
// deleted, whose resource version the claim moved, is not deleted: the API
// server answers with a conflict and the pod is left alone. Nothing here
// ever updates a pod.
//
// A Pool whose Profile is gone from the configuration is not rolled: its
// ReplicaSet is left exactly as it is (KF-051).
func (b *Backend) Rollout(ctx context.Context) {
	sets, err := b.replicaSets.List(b.runnerSelector())
	if err != nil {
		b.log.Warn("rollout: listing replicasets failed", "error", err)
		return
	}
	for _, rs := range sets {
		if ctx.Err() != nil {
			return
		}
		if !b.referenced(rs.Labels[LabelProfile]) {
			continue
		}
		b.rolloutPool(ctx, rs)
	}
}

// rolloutPool deletes at most one outdated idle pod of one Pool.
func (b *Backend) rolloutPool(ctx context.Context, rs *appsv1.ReplicaSet) {
	outdated := b.outdatedIdlePods(rs)
	if len(outdated) == 0 {
		return
	}
	now := b.opts.Clock.Now()
	if last, err := time.Parse(time.RFC3339Nano, rs.Annotations[annotationRolledAt]); err == nil {
		if now.Sub(last) < b.opts.RolloutInterval {
			return
		}
	}

	ctx, cancel := b.call(ctx)
	defer cancel()
	turn := rs.DeepCopy()
	if turn.Annotations == nil {
		turn.Annotations = map[string]string{}
	}
	turn.Annotations[annotationRolledAt] = formatLease(now)
	if _, err := b.opts.Client.AppsV1().ReplicaSets(b.opts.Namespace).Update(ctx, turn, metav1.UpdateOptions{}); err != nil {
		if !isConflict(err) {
			b.log.Warn("rollout: recording the turn failed", "replicaset", rs.Name, "error", err)
		}
		return
	}

	pod := outdated[0]
	err := b.opts.Client.CoreV1().Pods(b.opts.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion},
	})
	switch {
	case err == nil:
		b.log.Info("idle pod of a previous template deleted",
			"replicaset", rs.Name, "pod", pod.Name,
			"pod_template", pod.Labels[LabelTemplateHash], "template", currentHash(rs),
			"remaining", len(outdated)-1)
	case apierrors.IsNotFound(err), isConflict(err):
		// Gone already, or changed since it was read, which is what a claim
		// looks like from here. Either way it is not this pass's to delete.
		b.log.Debug("rollout: pod changed before it could be deleted, leaving it", "pod", pod.Name)
	default:
		b.log.Warn("rollout: deleting an idle pod failed", "pod", pod.Name, "error", err)
	}
}

// outdatedIdlePods are the Pool's idle pods that were not created from the
// ReplicaSet's current template, oldest first, leaving out any already being
// deleted.
func (b *Backend) outdatedIdlePods(rs *appsv1.ReplicaSet) []*corev1.Pod {
	pods, err := b.pods.List(labels.SelectorFromSet(idleSelectorOf(rs)))
	if err != nil {
		return nil
	}
	var out []*corev1.Pod
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil && pod.Labels[LabelTemplateHash] != currentHash(rs) {
			out = append(out, pod)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreationTimestamp.Equal(&out[j].CreationTimestamp) {
			return out[i].CreationTimestamp.Before(&out[j].CreationTimestamp)
		}
		return out[i].Name < out[j].Name
	})
	return out
}
