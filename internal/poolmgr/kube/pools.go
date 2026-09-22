package kube

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// CreatePool implements poolmgr.PoolAdmin: it creates the Pool's ReplicaSet
// (KF-040) and returns poolmgr.ErrAlreadyExists when there is one, which
// sends the Declarer to UpdatePool.
func (b *Backend) CreatePool(ctx context.Context, spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	profile, err := b.profileOf(spec)
	if err != nil {
		return nil, err
	}
	rs, err := b.replicaSet(spec, profile)
	if err != nil {
		return nil, err
	}
	b.warnUndelivered(spec, profile)

	ctx, cancel := b.call(ctx)
	defer cancel()
	created, err := b.opts.Client.AppsV1().ReplicaSets(b.opts.Namespace).Create(ctx, rs, metav1.CreateOptions{})
	if err != nil {
		return nil, translate("creating replicaset "+rs.Name, err)
	}
	b.log.Info("pool created", "pool", spec.Ref.String(), "replicaset", created.Name,
		"size", spec.Size, "template", currentHash(created))
	return b.pool(ctx, created)
}

// UpdatePool implements poolmgr.PoolAdmin: it brings the Pool's ReplicaSet
// to the spec (KF-040) and returns poolmgr.ErrNotFound when there is none.
// The selector is never changed, because a ReplicaSet's selector is
// immutable and derived from the same two names the ReplicaSet's own name
// comes from. A changed pod template only changes what the ReplicaSet
// creates from now on; the rollout loop retires the idle pods of the
// previous template (KF-050).
func (b *Backend) UpdatePool(ctx context.Context, spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	profile, err := b.profileOf(spec)
	if err != nil {
		return nil, err
	}
	want, err := b.replicaSet(spec, profile)
	if err != nil {
		return nil, err
	}
	b.warnUndelivered(spec, profile)

	ctx, cancel := b.call(ctx)
	defer cancel()
	client := b.opts.Client.AppsV1().ReplicaSets(b.opts.Namespace)
	// Another instance of the Runner may be declaring the same Pool, so the
	// update is retried on the version that beat it. Both write the same
	// spec, so a handful of attempts is plenty.
	for attempt := 0; ; attempt++ {
		current, err := client.Get(ctx, want.Name, metav1.GetOptions{})
		if err != nil {
			return nil, translate("reading replicaset "+want.Name, err)
		}
		previous := currentHash(current)
		next := current.DeepCopy()
		next.Labels = mergeMaps(next.Labels, want.Labels)
		next.Annotations = mergeMaps(next.Annotations, want.Annotations)
		next.Spec.Replicas = want.Spec.Replicas
		next.Spec.Template = want.Spec.Template
		updated, err := client.Update(ctx, next, metav1.UpdateOptions{})
		if isConflict(err) && attempt < updateAttempts {
			continue
		}
		if err != nil {
			return nil, translate("updating replicaset "+want.Name, err)
		}
		if hash := currentHash(updated); hash != previous {
			b.log.Info("pool template changed, idle pods of the previous template will be rolled out",
				"pool", spec.Ref.String(), "replicaset", updated.Name, "previous", previous, "template", hash)
		}
		return b.pool(ctx, updated)
	}
}

// updateAttempts bounds the retries of an update that lost a race.
const updateAttempts = 5

// DeletePool implements poolmgr.PoolAdmin. Deleting the ReplicaSet deletes
// the Pool's idle pods with it, through their owner references; claimed pods
// left the ReplicaSet when they were claimed and are untouched. The Runner
// never calls it on a reload (KF-051); only teardown does.
func (b *Backend) DeletePool(ctx context.Context, ref poolmgr.PoolRef) error {
	ctx, cancel := b.call(ctx)
	defer cancel()
	name := replicaSetName(ref)
	policy := metav1.DeletePropagationBackground
	err := b.opts.Client.AppsV1().ReplicaSets(b.opts.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil {
		return translate("deleting replicaset "+name, err)
	}
	b.log.Info("pool deleted", "pool", ref.String(), "replicaset", name)
	return nil
}

// GetPool implements poolmgr.PoolAdmin. It asks the API server rather than
// the watch cache, because it is the Tracker's fallback for exactly the case
// where the watch has fallen behind (PL-051).
func (b *Backend) GetPool(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Pool, error) {
	ctx, cancel := b.call(ctx)
	defer cancel()
	name := replicaSetName(ref)
	rs, err := b.opts.Client.AppsV1().ReplicaSets(b.opts.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, translate("reading replicaset "+name, err)
	}
	return b.pool(ctx, rs)
}

// ListPools implements poolmgr.PoolAdmin. It lists this Runner's Pools; the
// namespace argument is the Pool namespace, as at battery, and empty lists
// them all. It is the health probe (PL-005), so it too asks the API server.
func (b *Backend) ListPools(ctx context.Context, namespace string) ([]*poolmgr.Pool, error) {
	ctx, cancel := b.call(ctx)
	defer cancel()
	list, err := b.opts.Client.AppsV1().ReplicaSets(b.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: b.runnerSelector().String(),
	})
	if err != nil {
		return nil, translate("listing replicasets", err)
	}
	pods, err := b.livePods(ctx, b.runnerSelector())
	if err != nil {
		return nil, err
	}
	out := make([]*poolmgr.Pool, 0, len(list.Items))
	for i := range list.Items {
		rs := &list.Items[i]
		if namespace != "" && rs.Annotations[annotationPoolNamespace] != namespace {
			continue
		}
		out = append(out, &poolmgr.Pool{Spec: poolSpecFromReplicaSet(rs), Status: status(rs, pods)})
	}
	return out, nil
}

// pool pairs a ReplicaSet with the live status of its pods.
func (b *Backend) pool(ctx context.Context, rs *appsv1.ReplicaSet) (*poolmgr.Pool, error) {
	pods, err := b.livePods(ctx, labels.SelectorFromSet(poolLabelsOf(rs)))
	if err != nil {
		return nil, err
	}
	return &poolmgr.Pool{Spec: poolSpecFromReplicaSet(rs), Status: status(rs, pods)}, nil
}

// livePods lists pods from the API server.
func (b *Backend) livePods(ctx context.Context, selector labels.Selector) ([]*corev1.Pod, error) {
	list, err := b.opts.Client.CoreV1().Pods(b.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, translate("listing pods", err)
	}
	out := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// status counts a Pool's pods the way battery counts its MicroVMs: available
// is what a claim could take now, leased is what claims hold, and
// provisioning is every idle pod that is not ready yet. A pod of a previous
// template is neither available nor provisioning: it is on its way out. No
// pod is ever quarantined, because there are no hooks to fail.
func status(rs *appsv1.ReplicaSet, pods []*corev1.Pod) poolmgr.PoolStatus {
	var st poolmgr.PoolStatus
	for _, pod := range pods {
		if !inPool(rs, pod) || terminal(pod) {
			continue
		}
		switch {
		case pod.Labels[LabelState] == StateClaimed:
			st.Leased++
		case claimable(rs, pod):
			st.Available++
		case pod.Labels[LabelTemplateHash] == currentHash(rs):
			st.Provisioning++
		}
	}
	return st
}

// poolLabelsOf are the Runner and Profile labels of a ReplicaSet, which its
// pods carry too.
func poolLabelsOf(rs *appsv1.ReplicaSet) labels.Set {
	return labels.Set{LabelRunner: rs.Labels[LabelRunner], LabelProfile: rs.Labels[LabelProfile]}
}

// inPool reports whether a pod belongs to the ReplicaSet's Pool, idle or
// claimed.
func inPool(rs *appsv1.ReplicaSet, pod *corev1.Pod) bool {
	return pod.Labels[LabelRunner] == rs.Labels[LabelRunner] && pod.Labels[LabelProfile] == rs.Labels[LabelProfile]
}

// terminal reports whether a pod is being deleted or has finished, which is
// what the active deadline does to a claimed pod whose Runner died (KF-044).
func terminal(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

// ready reports whether the Pod Provider has reported the pod's MicroVM
// running with its guest agent answering (KF-024), on a Virtual Node.
func ready(pod *corev1.Pod) bool {
	if terminal(pod) || pod.Spec.NodeName == "" {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// claimable reports whether a claim may take the pod: it is idle, ready and
// of the Pool's current template (KF-044).
func claimable(rs *appsv1.ReplicaSet, pod *corev1.Pod) bool {
	return inPool(rs, pod) &&
		pod.Labels[LabelState] == StateIdle &&
		pod.Labels[LabelTemplateHash] == currentHash(rs) &&
		ready(pod)
}

// profileOf finds the Profile a spec was built from, by the Profile label
// the SpecBuilder put on its template (PL-014).
func (b *Backend) profileOf(spec poolmgr.PoolSpec) (config.Profile, error) {
	name := spec.Template.GetLabels()[poolmgr.LabelProfile]
	if name == "" {
		return config.Profile{}, fmt.Errorf("%w: pool %s: the template carries no %s label", poolmgr.ErrInvalid, spec.Ref, poolmgr.LabelProfile)
	}
	profile, ok := b.profile(name)
	if !ok {
		return config.Profile{}, fmt.Errorf("%w: pool %s: no profile named %q", poolmgr.ErrInvalid, spec.Ref, name)
	}
	return profile, nil
}

// warnUndelivered says what a Profile asks for that a Pool pod cannot
// carry, rather than dropping it silently. Hooks are battery's; user data
// reaches a MicroVM only through a ConfigMap (KF-023), which the Runner's
// Role does not let it write (KF-110); and a pod has one image, so there is
// nowhere to name an additional volume's (KF-022).
func (b *Backend) warnUndelivered(spec poolmgr.PoolSpec, profile config.Profile) {
	var dropped []string
	if len(spec.CreateCommands) > 0 || len(spec.PreLeaseCommands) > 0 {
		dropped = append(dropped, "pool hooks")
	}
	if profile.UserData != "" && b.opts.CloudInitConfigMaps[profile.Name] == "" {
		dropped = append(dropped, "user_data (name a ConfigMap under pool_manager.kubernetes.cloud_init_config_maps)")
	}
	if len(spec.Template.GetAdditionalVolumes()) > 0 {
		dropped = append(dropped, "additional_volumes")
	}
	if len(dropped) > 0 {
		b.log.Warn("profile settings the kubernetes pool backend cannot deliver are ignored",
			"profile", profile.Name, "pool", spec.Ref.String(), "ignored", dropped)
	}
}

func mergeMaps(into, from map[string]string) map[string]string {
	if into == nil {
		into = make(map[string]string, len(from))
	}
	for k, v := range from {
		into[k] = v
	}
	return into
}

func labelsString(set map[string]string) string { return labels.Set(set).String() }
