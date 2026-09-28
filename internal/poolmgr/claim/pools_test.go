package claim_test

import (
	"errors"
	"maps"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
)

// TestPoolIsDeclaredFromTheProfile creates a Pool from the Profile's spec
// and checks what the API server holds against battery-operator's CRD: the
// size, the lease timings, a node selector made of the Profile's
// architecture and Host selector, and a template that reads back as the
// spec's. It then checks that a second create is ErrAlreadyExists, that an
// update changes the Pool, and that GetPool and ListPools read it back
// under the Runner namespace it was declared in.
func TestPoolIsDeclaredFromTheProfile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	spec := f.spec()

	if _, err := b.CreatePool(f.ctx, spec); err != nil {
		t.Fatal(err)
	}
	pool := &batteryv1alpha1.Pool{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: f.namespace, Name: spec.Ref.Name}, pool); err != nil {
		t.Fatal(err)
	}
	wantSelector := map[string]string{
		claim.LabelArch:                    "arm64",
		"gitlab-runner.flintlock.dev/disk": "nvme",
		"example.com/zone":                 "a",
	}
	if got := pool.Spec.Placement.NodeSelector; !maps.Equal(got, wantSelector) {
		t.Errorf("node selector = %v, want %v", got, wantSelector)
	}
	if pool.Spec.Size != spec.Size || pool.Spec.Lease.ExpiryThreshold.Duration != spec.HeartbeatExpiryThreshold {
		t.Errorf("pool spec = size %d, expiry %s; want %d, %s",
			pool.Spec.Size, pool.Spec.Lease.ExpiryThreshold.Duration, spec.Size, spec.HeartbeatExpiryThreshold)
	}
	if pool.Labels[claim.LabelRunner] != runnerName || pool.Labels[claim.LabelProfile] != f.profile.Name {
		t.Errorf("pool labels = %v", pool.Labels)
	}

	if _, err := b.CreatePool(f.ctx, spec); !errors.Is(err, poolmgr.ErrAlreadyExists) {
		t.Errorf("second create = %v, want ErrAlreadyExists", err)
	}
	spec.Size = 3
	if _, err := b.UpdatePool(f.ctx, spec); err != nil {
		t.Fatal(err)
	}

	got, err := b.GetPool(f.ctx, spec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Ref != spec.Ref || got.Spec.Size != 3 {
		t.Errorf("GetPool = %s size %d, want %s size 3", got.Spec.Ref, got.Spec.Size, spec.Ref)
	}
	// The flintlock namespace is battery-operator's to set.
	want := spec.Template
	want.Namespace = ""
	if !proto.Equal(want, got.Spec.Template) {
		t.Errorf("template read back = %v, want %v", got.Spec.Template, want)
	}

	listed, err := b.ListPools(f.ctx, runnerNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Spec.Ref != spec.Ref {
		t.Errorf("ListPools(%s) = %d pools, want %s", runnerNamespace, len(listed), spec.Ref)
	}
	if listed, err := b.ListPools(f.ctx, "elsewhere"); err != nil || len(listed) != 0 {
		t.Errorf("ListPools(elsewhere) = %d, %v, want none", len(listed), err)
	}

	if err := b.DeletePool(f.ctx, spec.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetPool(f.ctx, spec.Ref); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("GetPool after delete = %v, want ErrNotFound", err)
	}
}

// TestPoolWithoutAProfileIsInvalid declares a spec whose Profile the
// backend was not given. It cannot build the node selector, so the spec is
// refused rather than declared on every Host.
func TestPoolWithoutAProfileIsInvalid(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend(func(o *claim.Options) { o.Profiles = nil })
	if _, err := b.CreatePool(f.ctx, f.spec()); !errors.Is(err, poolmgr.ErrInvalid) {
		t.Errorf("create without a profile = %v, want ErrInvalid", err)
	}
}

// TestPoolWatchDrivesTheTracker runs the real PoolTracker over the backend.
// The watch on the Pool resources brings its available count up to the
// Pool's, down on a claim, and up again on the release, and the release
// wakes a Job waiting on the exhausted Pool (SC-021).
func TestPoolWatchDrivesTheTracker(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)
	tracker := f.tracker(b, ref)
	f.eventually("the tracker counts the pool", func() bool { return tracker.Available(ref) == 1 })

	c, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	f.eventually("the tracker counts the claim", func() bool { return tracker.Available(ref) == 0 })

	wake := tracker.Wait(ref)
	if err := b.ReleaseVM(f.ctx, c.LeaseID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	case <-time.After(waitFor):
		t.Fatal("the release woke nothing waiting on the pool")
	}
	if got := tracker.Available(ref); got != 1 {
		t.Errorf("available = %d after the release, want 1", got)
	}
}

// tracker starts a PoolTracker over the backend, tracking one Pool.
func (f *fixture) tracker(b *claim.Backend, ref poolmgr.PoolRef) *poolmgr.PoolTracker {
	f.t.Helper()
	tracker, err := poolmgr.NewTracker(poolmgr.TrackerConfig{Events: b, Admin: b, PollInterval: time.Hour})
	if err != nil {
		f.t.Fatal(err)
	}
	tracker.Track(ref, true)
	go func() { _ = tracker.Run(f.ctx) }()
	return tracker
}

//= docs/requirements/08-observability.md#health
//= type=test
//# except that where the claim backend is configured, at least one of the
//# Runner's Pools reporting Ready in its status takes the place of the
//# healthy Host.

// TestPoolsReadyFollowsThePoolStatus checks the Pool readiness the Runner's
// /readyz reads under the claim backend: the watch reports the Runner's Pool
// not Ready until battery-operator sets the Ready condition in its status,
// and Ready once it does.
func TestPoolsReadyFollowsThePoolStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)

	readyOf := func() (ready, seen bool) {
		for _, p := range b.PoolsReady() {
			if p.Pool == ref {
				return p.Ready, true
			}
		}
		return false, false
	}
	f.eventually("the watch sees the pool", func() bool { _, seen := readyOf(); return seen })
	if ready, _ := readyOf(); ready {
		t.Fatal("the pool is ready before its status says so")
	}

	f.eventually("the ready condition is written", func() bool {
		pool := &batteryv1alpha1.Pool{}
		if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: f.namespace, Name: ref.Name}, pool); err != nil {
			return false
		}
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type: batteryv1alpha1.PoolConditionReady, Status: metav1.ConditionTrue, Reason: "AtSize",
		})
		return f.kube.Status().Update(f.ctx, pool) == nil
	})
	f.eventually("the watch reports the pool ready", func() bool { ready, _ := readyOf(); return ready })
}
