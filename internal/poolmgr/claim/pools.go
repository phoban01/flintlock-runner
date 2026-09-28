package claim

import (
	"context"
	"fmt"
	"maps"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// updateAttempts bounds the retries of a Pool update that lost a race.
const updateAttempts = 5

// CreatePool implements poolmgr.PoolAdmin: it creates the Pool resource and
// returns poolmgr.ErrAlreadyExists when there is one, which sends the
// Declarer to UpdatePool. The Pool's heartbeat interval is the Profile's,
// and the Client Library renews each claim on the Pool at that interval
// (KF-152).
func (b *Backend) CreatePool(ctx context.Context, spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	pool, err := b.poolResource(spec)
	if err != nil {
		return nil, err
	}
	ctx, cancel := b.call(ctx)
	defer cancel()
	if err := b.kube.Create(ctx, pool); err != nil {
		return nil, translate("creating pool "+pool.Name, err)
	}
	b.log.Info("pool created", "pool", spec.Ref.String(), "size", spec.Size)
	return b.poolOf(pool), nil
}

// UpdatePool implements poolmgr.PoolAdmin: it brings the Pool resource to
// the spec and returns poolmgr.ErrNotFound when there is none. Labels and
// annotations are merged, so that those others put on the Pool stay, and
// the status is left to battery-operator.
func (b *Backend) UpdatePool(ctx context.Context, spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	want, err := b.poolResource(spec)
	if err != nil {
		return nil, err
	}
	ctx, cancel := b.call(ctx)
	defer cancel()
	// Another instance of the Runner may be declaring the same Pool, so the
	// update is retried on the version that beat it. Both write the same
	// spec, so a handful of attempts is plenty.
	for attempt := 0; ; attempt++ {
		current := &batteryv1alpha1.Pool{}
		if err := b.kube.Get(ctx, client.ObjectKeyFromObject(want), current); err != nil {
			return nil, translate("reading pool "+want.Name, err)
		}
		next := current.DeepCopy()
		next.Labels = mergeMaps(next.Labels, want.Labels)
		next.Annotations = mergeMaps(next.Annotations, want.Annotations)
		next.Spec = want.Spec
		updateErr := b.kube.Update(ctx, next)
		if apierrors.IsConflict(updateErr) && attempt < updateAttempts {
			continue
		}
		if updateErr != nil {
			return nil, translate("updating pool "+want.Name, updateErr)
		}
		return b.poolOf(next), nil
	}
}

// DeletePool implements poolmgr.PoolAdmin. The Runner never calls it on a
// reload (PL-015); only teardown does.
func (b *Backend) DeletePool(ctx context.Context, ref poolmgr.PoolRef) error {
	ctx, cancel := b.call(ctx)
	defer cancel()
	pool := &batteryv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Namespace: b.opts.Namespace, Name: ref.Name}}
	if err := b.kube.Delete(ctx, pool); err != nil {
		return translate("deleting pool "+ref.Name, err)
	}
	b.log.Info("pool deleted", "pool", ref.String())
	return nil
}

// GetPool implements poolmgr.PoolAdmin. It asks the API server rather than
// the watch, because it is the Tracker's fallback for exactly the case where
// the watch has fallen behind (PL-051).
func (b *Backend) GetPool(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Pool, error) {
	ctx, cancel := b.call(ctx)
	defer cancel()
	pool := &batteryv1alpha1.Pool{}
	if err := b.kube.Get(ctx, client.ObjectKey{Namespace: b.opts.Namespace, Name: ref.Name}, pool); err != nil {
		return nil, translate("reading pool "+ref.Name, err)
	}
	out := b.poolOf(pool)
	out.Spec.Ref = ref
	return out, nil
}

// ListPools implements poolmgr.PoolAdmin. It lists this Runner's Pools; the
// namespace argument is the Runner namespace the Pools were declared under,
// as at battery, and empty lists them all. It is the health probe (PL-005).
func (b *Backend) ListPools(ctx context.Context, namespace string) ([]*poolmgr.Pool, error) {
	ctx, cancel := b.call(ctx)
	defer cancel()
	list := &batteryv1alpha1.PoolList{}
	if err := b.kube.List(ctx, list, client.InNamespace(b.opts.Namespace),
		client.MatchingLabels{LabelRunner: b.opts.RunnerName}); err != nil {
		return nil, translate("listing pools", err)
	}
	out := make([]*poolmgr.Pool, 0, len(list.Items))
	for i := range list.Items {
		pool := b.poolOf(&list.Items[i])
		if namespace != "" && pool.Spec.Ref.Namespace != namespace {
			continue
		}
		out = append(out, pool)
	}
	return out, nil
}

// poolOf pairs a Pool resource's spec with the counts in its status.
func (b *Backend) poolOf(pool *batteryv1alpha1.Pool) *poolmgr.Pool {
	return &poolmgr.Pool{Spec: specFromPool(pool, b.opts.RunnerNamespace), Status: statusFromPool(pool)}
}

// poolResource is the Pool resource a spec declares.
func (b *Backend) poolResource(spec poolmgr.PoolSpec) (*batteryv1alpha1.Pool, error) {
	profile, err := b.profileOf(spec)
	if err != nil {
		return nil, err
	}
	return poolFromSpec(spec, profile, b.opts.Namespace, b.opts.RunnerName)
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

func mergeMaps(into, from map[string]string) map[string]string {
	if into == nil {
		into = make(map[string]string, len(from))
	}
	maps.Copy(into, from)
	return into
}
