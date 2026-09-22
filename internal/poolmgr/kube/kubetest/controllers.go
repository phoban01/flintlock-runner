package kubetest

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// defaultInterval is how often the stand-ins look at the namespace. They
// poll the API server rather than watch it: a list is always current, which
// keeps them free of the cache races the backend under test has to get
// right on its own.
const defaultInterval = 20 * time.Millisecond

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The Kubernetes pool backend SHALL be tested against a
//# Kubernetes API server test environment, with a test reconciler standing in
//# for the ReplicaSet controller where no controller manager runs.

// ReplicaSets is the test reconciler that stands in for the ReplicaSet
// controller in one namespace (KF-121). It does the three things the backend
// relies on: it creates pods from a ReplicaSet's current template until as
// many match the selector as there are replicas, it releases a pod whose
// labels stopped matching, which is what a claim does to it, and it deletes
// surplus pods. It also deletes the pods of a deleted ReplicaSet, which in a
// cluster is the garbage collector's work. It does not adopt orphans and
// writes no status, because nothing under test reads either.
type ReplicaSets struct {
	Client    kubernetes.Interface
	Namespace string
	// Interval is the polling interval; zero means defaultInterval.
	Interval time.Duration
}

// Run reconciles until ctx ends.
func (r *ReplicaSets) Run(ctx context.Context) {
	run(ctx, r.Interval, func() { _ = r.Reconcile(ctx) })
}

// Reconcile makes one pass. An error is a conflict or a race with a test's
// own writes as often as not, and the next pass starts from a fresh list, so
// Run ignores it.
func (r *ReplicaSets) Reconcile(ctx context.Context) error {
	sets, err := r.Client.AppsV1().ReplicaSets(r.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	pods, err := r.Client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	live := map[types.UID]*appsv1.ReplicaSet{}
	for i := range sets.Items {
		live[sets.Items[i].UID] = &sets.Items[i]
	}
	owned := map[types.UID][]*corev1.Pod{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != "ReplicaSet" {
			continue
		}
		rs, exists := live[owner.UID]
		switch {
		case !exists:
			if err := r.delete(ctx, pod); err != nil {
				return err
			}
		case !selects(rs, pod):
			if err := r.release(ctx, pod); err != nil {
				return err
			}
		case active(pod):
			owned[rs.UID] = append(owned[rs.UID], pod)
		}
	}
	for _, rs := range live {
		if rs.DeletionTimestamp != nil {
			continue
		}
		if err := r.scale(ctx, rs, owned[rs.UID]); err != nil {
			return err
		}
	}
	return nil
}

// scale creates or deletes pods until the ReplicaSet has its replicas.
func (r *ReplicaSets) scale(ctx context.Context, rs *appsv1.ReplicaSet, pods []*corev1.Pod) error {
	want := 1
	if rs.Spec.Replicas != nil {
		want = int(*rs.Spec.Replicas)
	}
	for n := len(pods); n < want; n++ {
		if _, err := r.Client.CoreV1().Pods(r.Namespace).Create(ctx, podFor(rs), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating a pod of %s: %w", rs.Name, err)
		}
	}
	if len(pods) <= want {
		return nil
	}
	// Surplus: pods that are not ready go first, then the newest, as the
	// real controller prefers.
	sort.SliceStable(pods, func(i, j int) bool {
		if ri, rj := isReady(pods[i]), isReady(pods[j]); ri != rj {
			return !ri
		}
		return pods[j].CreationTimestamp.Before(&pods[i].CreationTimestamp)
	})
	for _, pod := range pods[:len(pods)-want] {
		if err := r.delete(ctx, pod); err != nil {
			return err
		}
	}
	return nil
}

// podFor instantiates a ReplicaSet's template.
func podFor(rs *appsv1.ReplicaSet) *corev1.Pod {
	controller := true
	template := rs.Spec.Template.DeepCopy()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: rs.Name + "-",
			Namespace:    rs.Namespace,
			Labels:       template.Labels,
			Annotations:  template.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
				Controller: &controller, BlockOwnerDeletion: &controller,
			}},
		},
		Spec: template.Spec,
	}
}

// release drops the owner reference of a pod the selector no longer matches,
// so that deleting the ReplicaSet later does not take the pod with it.
func (r *ReplicaSets) release(ctx context.Context, pod *corev1.Pod) error {
	next := pod.DeepCopy()
	next.OwnerReferences = nil
	_, err := r.Client.CoreV1().Pods(r.Namespace).Update(ctx, next, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *ReplicaSets) delete(ctx context.Context, pod *corev1.Pod) error {
	if pod.DeletionTimestamp != nil {
		return nil
	}
	err := r.Client.CoreV1().Pods(r.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func selects(rs *appsv1.ReplicaSet, pod *corev1.Pod) bool {
	selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(pod.Labels))
}

// active reports whether a pod counts towards its ReplicaSet's replicas.
func active(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp == nil &&
		pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

func isReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Kubelet stands in for the scheduler and the Pod Provider in one namespace:
// it binds every pending pod to one of its Nodes, the one with the fewest
// pods, reports it running and ready, and finishes the deletion of a pod
// that has a deletion timestamp, which for a bound pod the API server leaves
// to the Node's kubelet. It ignores selectors, taints and deadlines: what
// the backend writes into a pod spec is asserted directly by the tests.
type Kubelet struct {
	Client    kubernetes.Interface
	Namespace string
	// Nodes are the Virtual Nodes pods are bound to.
	Nodes []string
	// Interval is the polling interval; zero means defaultInterval.
	Interval time.Duration

	paused atomic.Bool
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The harness SHALL include a scenario in which two Runners claim
//# from a Pool of one, and SHALL assert that exactly one obtains the pod.

// Pause stops new pods from being bound and made ready, so that a test can
// hold a Pool's replacement back; Resume lets them through again. Deletions
// are finished either way. It is what makes the scenario of two Runners and
// a Pool of one decidable: the claim that wins takes the pod out of the
// ReplicaSet, which replaces it at once, and unless that replacement is kept
// from becoming ready the Runner that lost could be handed it and the
// scenario would see two claims succeed without either being wrong. Each
// Runner in that scenario is a backend with a client of its own from
// Env.User.
func (k *Kubelet) Pause() { k.paused.Store(true) }

// Resume lets the pods Pause held back be bound and made ready.
func (k *Kubelet) Resume() { k.paused.Store(false) }

// Run syncs until ctx ends.
func (k *Kubelet) Run(ctx context.Context) {
	run(ctx, k.Interval, func() { _ = k.Sync(ctx) })
}

// Sync makes one pass.
func (k *Kubelet) Sync(ctx context.Context) error {
	pods, err := k.Client.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	load := map[string]int{}
	for i := range pods.Items {
		load[pods.Items[i].Spec.NodeName]++
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		switch {
		case pod.DeletionTimestamp != nil:
			now := int64(0)
			err = k.Client.CoreV1().Pods(k.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &now})
		case k.paused.Load() || isReady(pod) || !active(pod):
			continue
		case pod.Spec.NodeName == "":
			node := k.leastLoaded(load)
			load[node]++
			err = k.Client.CoreV1().Pods(k.Namespace).Bind(ctx, &corev1.Binding{
				ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: k.Namespace},
				Target:     corev1.ObjectReference{Kind: "Node", Name: node},
			}, metav1.CreateOptions{})
		default:
			err = MarkReady(ctx, k.Client, pod)
		}
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return err
		}
	}
	return nil
}

func (k *Kubelet) leastLoaded(load map[string]int) string {
	best := k.Nodes[0]
	for _, node := range k.Nodes[1:] {
		if load[node] < load[best] {
			best = node
		}
	}
	return best
}

// MarkReady reports a bound pod running and ready, as the Pod Provider does
// once the MicroVM's guest agent answers (KF-024).
func MarkReady(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod) error {
	return setReady(ctx, client, pod, corev1.ConditionTrue)
}

// MarkUnready takes a pod's readiness away again.
func MarkUnready(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod) error {
	return setReady(ctx, client, pod, corev1.ConditionFalse)
}

func setReady(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, status corev1.ConditionStatus) error {
	next := pod.DeepCopy()
	next.Status.Phase = corev1.PodRunning
	next.Status.HostIP = "10.0.0.1"
	next.Status.PodIP = "192.168.127.2"
	next.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.Now(),
	}}
	_, err := client.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, next, metav1.UpdateOptions{})
	return err
}

// run calls step every interval until ctx ends.
func run(ctx context.Context, interval time.Duration, step func()) {
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		step()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
