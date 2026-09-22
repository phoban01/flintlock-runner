package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

func isConflict(err error) bool { return apierrors.IsConflict(err) }

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# If the claim update is rejected because the pod changed, then
//# the Scheduler SHALL try another ready idle pod, and SHALL treat the Pool
//# as exhausted only when none is left.

// ClaimVM implements poolmgr.Lease. It tries the ready idle pods of the
// Pool's current template one after another until a claim update goes
// through. The candidates come from the watch cache first, in a random order
// so that Runners racing for one Pool do not all reach for the same pod. A
// cache can be behind, so when it yields no Lease the API server is asked
// for the Pool's idle pods and the ones not tried yet are tried: only when
// none of those is left either is the Pool reported exhausted, with the
// battery client's poolmgr.ErrExhausted, which the Scheduler waits out as it
// always has (SC-021). An unknown Pool is poolmgr.ErrNotFound, which makes
// the Scheduler declare it again (PL-033).
func (b *Backend) ClaimVM(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
	ctx, cancel := b.call(ctx)
	defer cancel()
	rs, err := b.replicaSetOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	timeout := b.jobTimeout(ctx)

	tried := map[types.UID]bool{}
	cached, err := b.pods.List(labels.SelectorFromSet(idleSelectorOf(rs)))
	if err != nil {
		return nil, translate("listing cached pods", err)
	}
	if claim, err := b.claimAny(ctx, rs, cached, timeout, tried); claim != nil || err != nil {
		return claim, err
	}
	live, err := b.livePods(ctx, labels.SelectorFromSet(idleSelectorOf(rs)))
	if err != nil {
		return nil, err
	}
	if claim, err := b.claimAny(ctx, rs, live, timeout, tried); claim != nil || err != nil {
		return claim, err
	}
	return nil, fmt.Errorf("kube: claiming from %s: %w", ref, poolmgr.ErrExhausted)
}

// claimAny tries each claimable pod not tried before. It returns a nil
// Claim and a nil error when none of them could be claimed.
func (b *Backend) claimAny(
	ctx context.Context,
	rs *appsv1.ReplicaSet,
	pods []*corev1.Pod,
	timeout time.Duration,
	tried map[types.UID]bool,
) (*poolmgr.Claim, error) {
	candidates := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if claimable(rs, pod) && !tried[pod.UID] {
			candidates = append(candidates, pod)
		}
	}
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	for _, pod := range candidates {
		tried[pod.UID] = true
		claimed, err := b.claim(ctx, rs, pod, timeout)
		if err != nil {
			return nil, err
		}
		if claimed != nil {
			return claimOf(claimed), nil
		}
	}
	return nil, nil
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# When claiming from a Pool, the Scheduler SHALL choose a ready
//# idle pod of that Pool's current template and SHALL set its state label to
//# `claimed`, its lease annotation to the current time and its active
//# deadline to the Job timeout in one update conditioned on the pod's
//# resource version.

// claim makes one pod a Lease. The state label, the lease annotation and the
// active deadline go into a single Update of the pod as it was read, resource
// version included, so the API server applies all three or none, and of
// several Runners updating the same version exactly one succeeds. It returns
// a nil pod and a nil error when the pod is lost to this Runner: it is gone,
// or it changed and is no longer claimable.
//
// A conflict does not by itself mean another Runner has the pod. The Pod
// Provider updates a pod's status, which moves its resource version too, so
// the pod is read again and the claim repeated on the new version while the
// pod is still claimable; a pod another Runner claimed no longer is.
func (b *Backend) claim(ctx context.Context, rs *appsv1.ReplicaSet, pod *corev1.Pod, timeout time.Duration) (*corev1.Pod, error) {
	client := b.opts.Client.CoreV1().Pods(b.opts.Namespace)
	for {
		next := pod.DeepCopy()
		next.Labels[LabelState] = StateClaimed
		if next.Annotations == nil {
			next.Annotations = map[string]string{}
		}
		next.Annotations[AnnotationLease] = formatLease(b.opts.Clock.Now())
		deadline := deadlineSeconds(timeout)
		next.Spec.ActiveDeadlineSeconds = &deadline

		claimed, err := client.Update(ctx, next, metav1.UpdateOptions{})
		switch {
		case err == nil:
			b.log.Debug("pod claimed", "pod", claimed.Name, "node", claimed.Spec.NodeName,
				"replicaset", rs.Name, "active_deadline", deadline)
			return claimed, nil
		case apierrors.IsNotFound(err):
			return nil, nil
		case !isConflict(err):
			return nil, translate("claiming pod "+pod.Name, err)
		}

		fresh, err := client.Get(ctx, pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, translate("reading pod "+pod.Name, err)
		}
		if fresh.UID != pod.UID || !claimable(rs, fresh) {
			b.log.Debug("pod was taken before the claim update, trying another", "pod", pod.Name)
			return nil, nil
		}
		pod = fresh
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# The Scheduler SHALL return the claimed pod's name as the Lease
//# id and the pod's Virtual Node as the Placement.

// claimOf renders a claimed pod as a Claim. The Lease id is the pod's name,
// which is all Heartbeat and ReleaseVM need to find it again, and the Host is
// the Virtual Node the pod is bound to, so the Scheduler records the
// Placement from the claim with no lookup (SC-030). The address is left
// empty: the Runner reaches no Host in a cluster fleet (KF-063), so there is
// no endpoint to compare it with. The MicroVM is identified by the pod's UID,
// which is what the Pod Provider labels it with (KF-021).
func claimOf(pod *corev1.Pod) *poolmgr.Claim {
	return &poolmgr.Claim{
		LeaseID: pod.Name,
		VMUID:   string(pod.UID),
		Host:    poolmgr.HostRef{Name: pod.Spec.NodeName},
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# When a Lease heartbeat is due, the Scheduler SHALL update the
//# pod's lease annotation.

// Heartbeat implements poolmgr.Lease. It patches the lease annotation alone,
// so a status update of the Pod Provider landing at the same moment cannot
// make it fail. The Lease no longer exists, poolmgr.ErrNotFound, when the
// pod is gone, was never claimed, or has finished or is being deleted, which
// is how an expired Lease looks (KF-032, KF-044); the Scheduler then fails
// the Job (SC-061). The expiry returned is the renewal plus the Pool's
// heartbeat expiry threshold.
func (b *Backend) Heartbeat(ctx context.Context, leaseID string) (time.Time, error) {
	ctx, cancel := b.call(ctx)
	defer cancel()
	now := b.opts.Clock.Now()
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{AnnotationLease: formatLease(now)},
		},
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("kube: heartbeat %s: %w", leaseID, err)
	}
	pod, err := b.opts.Client.CoreV1().Pods(b.opts.Namespace).Patch(ctx, leaseID, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return time.Time{}, translate("heartbeat "+leaseID, err)
	}
	if !b.leased(pod) {
		return time.Time{}, fmt.Errorf("kube: heartbeat %s: %w: the pod is not a live lease of this runner", leaseID, poolmgr.ErrNotFound)
	}
	return now.Add(b.leaseExpiry(pod)), nil
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# When a Lease is released, the Scheduler SHALL delete the pod.

// ReleaseVM implements poolmgr.Lease. A MicroVM is never reused, so releasing
// a Lease deletes its pod, and the Pod Provider deletes the MicroVM (KF-027).
// The pod is read first and deleted by UID, so that a Lease id can only ever
// delete the claimed pod it named and never an idle pod or another Runner's.
// A pod that is already gone or going is poolmgr.ErrNotFound, which counts
// as released (PL-042).
func (b *Backend) ReleaseVM(ctx context.Context, leaseID string) error {
	ctx, cancel := b.call(ctx)
	defer cancel()
	client := b.opts.Client.CoreV1().Pods(b.opts.Namespace)
	pod, err := client.Get(ctx, leaseID, metav1.GetOptions{})
	if err != nil {
		return translate("reading pod "+leaseID, err)
	}
	if pod.Labels[LabelRunner] != labelValue(b.opts.RunnerName) || pod.Labels[LabelState] != StateClaimed {
		return fmt.Errorf("kube: release %s: %w: the pod is not a lease of this runner", leaseID, poolmgr.ErrNotFound)
	}
	if pod.DeletionTimestamp != nil {
		return fmt.Errorf("kube: release %s: %w: the pod is already being deleted", leaseID, poolmgr.ErrNotFound)
	}
	err = client.Delete(ctx, leaseID, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}})
	if err != nil && !isConflict(err) {
		return translate("deleting pod "+leaseID, err)
	}
	// A conflict on the UID means the pod named leaseID is a different pod
	// now, so the one that was leased is gone.
	if err != nil {
		return fmt.Errorf("kube: release %s: %w", leaseID, poolmgr.ErrNotFound)
	}
	b.log.Debug("lease released, pod deleted", "pod", leaseID)
	return nil
}

// leased reports whether a pod is a live Lease of this Runner.
func (b *Backend) leased(pod *corev1.Pod) bool {
	return pod.Labels[LabelRunner] == labelValue(b.opts.RunnerName) &&
		pod.Labels[LabelState] == StateClaimed &&
		!terminal(pod)
}

// leaseExpiry is the heartbeat expiry threshold of the pod's Pool, from its
// ReplicaSet, or the default where the ReplicaSet is not known.
func (b *Backend) leaseExpiry(pod *corev1.Pod) time.Duration {
	sets, err := b.replicaSets.List(labels.SelectorFromSet(labels.Set{
		LabelRunner:  pod.Labels[LabelRunner],
		LabelProfile: pod.Labels[LabelProfile],
	}))
	if err == nil {
		for _, rs := range sets {
			if d := poolSpecFromReplicaSet(rs).HeartbeatExpiryThreshold; d > 0 {
				return d
			}
		}
	}
	return defaultLeaseExpiry
}

// replicaSetOf finds a Pool's ReplicaSet in the watch cache and, because a
// Pool declared a moment ago may not have reached it, at the API server.
func (b *Backend) replicaSetOf(ctx context.Context, ref poolmgr.PoolRef) (*appsv1.ReplicaSet, error) {
	name := replicaSetName(ref)
	if rs, err := b.replicaSets.Get(name); err == nil {
		return rs, nil
	}
	rs, err := b.opts.Client.AppsV1().ReplicaSets(b.opts.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, translate("reading replicaset "+name, err)
	}
	return rs, nil
}

// idleSelectorOf selects the idle pods of a ReplicaSet's Pool.
func idleSelectorOf(rs *appsv1.ReplicaSet) labels.Set {
	set := poolLabelsOf(rs)
	set[LabelState] = StateIdle
	return set
}
