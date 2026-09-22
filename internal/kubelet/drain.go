package kubelet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

const (
	// guardNamePrefix names the guard pod and its PodDisruptionBudget after
	// the Host's Node.
	guardNamePrefix = "flintlock-drain-guard-"
	// labelDrainGuard selects the guard pod of one Host's Node.
	labelDrainGuard = kubelabels.Prefix + "drain-guard"
	// annotationCordoned marks a Virtual Node this provider cordoned, so
	// that it uncordons only what it cordoned and a restart remembers.
	annotationCordoned = kubelabels.Prefix + "cordoned-by-provider"
	// annotationDrainStarted is when the Host's Node was first seen
	// unschedulable. It is kept on the Virtual Node so that a provider
	// restart does not restart the drain timeout.
	annotationDrainStarted = kubelabels.Prefix + "drain-started"
)

// drainer ties the Host's Node to its Virtual Node, which the cluster does
// not: a drain of the first leaves the pods of the second alone. It is
// driven by reconcile, from the provider's sync loop and from every event on
// the Host's Node.
//
// reconcile runs often, so it remembers what it last did and talks to the
// API server only when something has to change; every resync period it
// forgets, reads the Virtual Node again and repairs whatever drifted.
type drainer struct {
	cfg      *Config
	log      *slog.Logger
	clk      clock.Clock
	kube     kubernetes.Interface
	provider *Provider
	hostNode string
	nodeName string
	// host returns the Host's Node from the informer's cache.
	host func() (*corev1.Node, error)
	// resync is how often the remembered state is dropped.
	resync time.Duration

	// The remembered state, used by one goroutine only.
	synced     time.Time
	vnodeUID   types.UID
	cordoned   bool
	started    time.Time
	guardKnown bool
	guard      bool
	abandoned  map[string]bool
}

// guardName is the name of the guard pod and of its PodDisruptionBudget.
func (d *drainer) guardName() string { return guardNamePrefix + d.hostNode }

// refresh reads the Virtual Node when the remembered state is stale.
func (d *drainer) refresh(ctx context.Context) error {
	if !d.synced.IsZero() && d.clk.Now().Sub(d.synced) < d.resync {
		return nil
	}
	vnode, err := d.kube.CoreV1().Nodes().Get(ctx, d.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the virtual node %s: %w", d.nodeName, err)
	}
	d.vnodeUID = vnode.UID
	_, d.cordoned = vnode.Annotations[annotationCordoned]
	d.started = time.Time{}
	if started, err := time.Parse(time.RFC3339Nano, vnode.Annotations[annotationDrainStarted]); err == nil {
		d.started = started
	}
	d.guardKnown = false
	d.synced = d.clk.Now()
	return nil
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The harness SHALL include a scenario in which a Host's Node is
//# cordoned while it runs a Job, and SHALL assert that the Job finishes, that
//# the idle pods leave the Host and that the drain completes afterwards.

// reconcile brings the Virtual Node's schedulability and the guard in line
// with the Host's Node and the claimed pods. It is what the cordon scenario
// of KF-124 exercises: TestCordonDrainsAroundARunningJob runs it in this
// package, and the harness runs it under a Runner.
func (d *drainer) reconcile(ctx context.Context) error {
	host, err := d.host()
	if err != nil {
		return fmt.Errorf("reading the host's node %s: %w", d.hostNode, err)
	}
	if err := d.refresh(ctx); err != nil {
		return err
	}
	claimed := d.provider.podsByState(kubelabels.StateClaimed)

	if !host.Spec.Unschedulable {
		d.abandoned = nil
		if err := d.uncordon(ctx); err != nil {
			return err
		}
		return d.setGuard(ctx, len(claimed) > 0)
	}

	if err := d.cordon(ctx); err != nil {
		return err
	}
	d.deleteIdlePods(ctx)

	//= docs/requirements/12-cluster-fleet.md#cluster-drain
	//# If the configured drain timeout elapses while claimed pods
	//# remain, then the Pod Provider SHALL let the drain complete and SHALL log
	//# each pod it abandoned.
	if waited := d.clk.Now().Sub(d.started); len(claimed) > 0 && waited >= d.cfg.DrainTimeout {
		for _, pod := range claimed {
			key := podKey(pod.Namespace, pod.Name)
			if d.abandoned[key] {
				continue
			}
			if d.abandoned == nil {
				d.abandoned = map[string]bool{}
			}
			d.abandoned[key] = true
			d.log.Warn("abandoning a claimed pod: the drain timeout elapsed and the drain of the host's node is let through",
				"pod", key, "host_node", d.hostNode, "waited", waited.String(), "drain_timeout", d.cfg.DrainTimeout.String())
		}
		return d.setGuard(ctx, false)
	}

	//= docs/requirements/12-cluster-fleet.md#cluster-drain
	//# When no claimed pod remains on an unschedulable Host, the Pod
	//# Provider SHALL let the drain of the Host's Node complete.
	return d.setGuard(ctx, len(claimed) > 0)
}

//= docs/requirements/12-cluster-fleet.md#cluster-drain
//# When the Host's Node becomes unschedulable, the Pod Provider
//# SHALL mark the Virtual Node unschedulable and SHALL delete the idle pods
//# bound to it, so that their ReplicaSets replace them on other Hosts.

// cordon marks the Virtual Node unschedulable and records when the drain
// started, on first sight.
func (d *drainer) cordon(ctx context.Context) error {
	if d.cordoned && !d.started.IsZero() {
		return nil
	}
	started := d.clk.Now().UTC()
	err := d.patchNode(ctx, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			annotationCordoned:     "true",
			annotationDrainStarted: started.Format(time.RFC3339Nano),
		}},
		"spec": map[string]any{"unschedulable": true},
	})
	if err != nil {
		return err
	}
	d.cordoned, d.started = true, started
	d.log.Info("the host's node is unschedulable; cordoned the virtual node", "host_node", d.hostNode, "virtual_node", d.nodeName)
	return nil
}

//= docs/requirements/12-cluster-fleet.md#cluster-drain
//# When the Host's Node becomes schedulable again, the Pod
//# Provider SHALL mark the Virtual Node schedulable.

// uncordon undoes cordon. A Virtual Node an operator cordoned by hand does
// not carry the provider's mark and is left as it is.
func (d *drainer) uncordon(ctx context.Context) error {
	if !d.cordoned {
		return nil
	}
	err := d.patchNode(ctx, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			annotationCordoned:     nil,
			annotationDrainStarted: nil,
		}},
		"spec": map[string]any{"unschedulable": nil},
	})
	if err != nil {
		return err
	}
	d.cordoned, d.started = false, time.Time{}
	d.log.Info("the host's node is schedulable again; uncordoned the virtual node", "host_node", d.hostNode, "virtual_node", d.nodeName)
	return nil
}

// patchNode applies a JSON merge patch to the Virtual Node.
func (d *drainer) patchNode(ctx context.Context, patch map[string]any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if _, err := d.kube.CoreV1().Nodes().Patch(ctx, d.nodeName, types.MergePatchType, data, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patching the virtual node %s: %w", d.nodeName, err)
	}
	return nil
}

// deleteIdlePods deletes the idle pods bound to the Virtual Node (KF-090).
// Claimed pods are never touched here: a Job is running in them.
func (d *drainer) deleteIdlePods(ctx context.Context) {
	for _, pod := range d.provider.podsByState(kubelabels.StateIdle) {
		uid := pod.UID
		err := d.kube.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		switch {
		case err == nil:
			d.log.Info("deleted an idle pod from a draining host", "pod", podKey(pod.Namespace, pod.Name), "host_node", d.hostNode)
		case !apierrors.IsNotFound(err):
			d.log.Warn("could not delete an idle pod from a draining host", "pod", podKey(pod.Namespace, pod.Name), "error", err)
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-drain
//# While a claimed pod is bound to the Virtual Node, the Pod
//# Provider SHALL prevent an eviction-based drain of the Host's Node from
//# completing.

// setGuard creates or removes the guard: a pod on the Host's own Node under
// a PodDisruptionBudget that allows no disruption. An eviction-based drain
// evicts every pod of the node and cannot finish while one refuses, so the
// guard is what holds the drain open. It exists whenever a claimed pod does,
// not only once a cordon is seen, because a drain's evictions follow its
// cordon faster than the provider can be sure to notice.
//
// The budget is written as minAvailable 1 rather than maxUnavailable 0: the
// second needs the disruption controller to find the pod's scale through a
// controller it knows, and the guard has none. The budget is made before
// the pod and removed after it, so the pod is never unprotected. The Virtual
// Node owns both, so that they go when the Host's Node does (KF-011).
func (d *drainer) setGuard(ctx context.Context, want bool) error {
	if d.guardKnown && d.guard == want {
		return nil
	}
	var err error
	if want {
		err = d.placeGuard(ctx)
	} else {
		err = d.removeGuard(ctx)
	}
	if err != nil {
		d.guardKnown = false
		return err
	}
	d.guardKnown, d.guard = true, want
	return nil
}

// removeGuard deletes the guard pod and then its budget.
func (d *drainer) removeGuard(ctx context.Context) error {
	name, namespace := d.guardName(), d.cfg.Guard.Namespace
	err := d.kube.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the drain guard pod: %w", err)
	}
	if err == nil {
		d.log.Info("removed the drain guard; a drain of the host's node can complete", "host_node", d.hostNode)
	}
	err = d.kube.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the drain guard's pod disruption budget: %w", err)
	}
	return nil
}

// placeGuard creates the budget and then the guard pod. A guard pod that is
// there but has ended, which a node reboot can leave behind, is deleted so
// that the next reconcile places a live one.
func (d *drainer) placeGuard(ctx context.Context) error {
	name, namespace := d.guardName(), d.cfg.Guard.Namespace
	pods := d.kube.CoreV1().Pods(namespace)
	selector := map[string]string{labelDrainGuard: d.hostNode}
	owner := func(controller bool) []metav1.OwnerReference {
		ref := metav1.OwnerReference{APIVersion: "v1", Kind: "Node", Name: d.nodeName, UID: d.vnodeUID}
		if controller {
			ref.Controller = &controller
		}
		return []metav1.OwnerReference{ref}
	}

	minAvailable := intstr.FromInt32(1)
	_, err := d.kube.PolicyV1().PodDisruptionBudgets(namespace).Create(ctx, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: selector, OwnerReferences: owner(false)},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: selector},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the drain guard's pod disruption budget: %w", err)
	}

	noToken := false
	_, err = pods.Create(ctx, &corev1.Pod{
		// The owner reference names a controller so that a drain treats the
		// guard as a managed pod and evicts it, which the budget refuses,
		// instead of stopping to ask for --force.
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: selector, OwnerReferences: owner(true)},
		Spec: corev1.PodSpec{
			// Bound by name: the Host's Node may already be cordoned, and
			// the scheduler would refuse to put anything there.
			NodeName:                     d.hostNode,
			AutomountServiceAccountToken: &noToken,
			Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers:                   []corev1.Container{{Name: "guard", Image: d.cfg.Guard.Image}},
		},
	}, metav1.CreateOptions{})
	if err == nil {
		d.log.Info("placed the drain guard: claimed pods hold a drain of the host's node open", "host_node", d.hostNode)
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the drain guard pod: %w", err)
	}
	existing, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the drain guard pod: %w", err)
	}
	if existing.Status.Phase == corev1.PodFailed || existing.Status.Phase == corev1.PodSucceeded || existing.DeletionTimestamp != nil {
		if err := pods.Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting the ended drain guard pod: %w", err)
		}
		return fmt.Errorf("the drain guard pod had ended (%s) and was deleted; the next reconcile places a new one", existing.Status.Phase)
	}
	return nil
}
