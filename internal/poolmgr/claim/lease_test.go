package claim_test

import (
	"context"
	"errors"
	"testing"
	"time"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
)

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# Where the claim backend is configured, the Scheduler SHALL
//# obtain a MicroVM for a Job by creating a `MicroVMClaim`

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# The Scheduler SHALL take the claimed MicroVM's uid

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# and from nothing else.

// TestClaimVMMapsTheBoundClaim claims a MicroVM and checks the claim the
// backend made, a MicroVMClaim on the Pool for the Holder, and that the
// poolmgr.Claim it returned is the Bound claim's status: its name as the
// lease id, the MicroVM's uid, and the Host's node name and Exec Agent
// address.
func TestClaimVMMapsTheBoundClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)

	got, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := f.claim(got.LeaseID)
	if !ok {
		t.Fatalf("no claim named %s, the lease id", got.LeaseID)
	}
	if obj.Spec.PoolRef.Name != ref.Name || obj.Spec.ServiceAccountName != holder {
		t.Errorf("claim spec = %+v, want pool %s for holder %s", obj.Spec, ref.Name, holder)
	}
	if obj.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("claim phase = %s, want Bound", obj.Status.Phase)
	}
	want := poolmgr.Claim{
		LeaseID: obj.Name,
		VMUID:   obj.Status.MicroVM.UID,
		Host:    poolmgr.HostRef{Name: obj.Status.Host.NodeName, Address: obj.Status.Host.AgentAddress},
	}
	if got.LeaseID != want.LeaseID || got.VMUID != want.VMUID || got.Host != want.Host {
		t.Errorf("claim = %+v, want %+v", *got, want)
	}
	if got.LeaseID == obj.Status.LeaseID {
		t.Errorf("lease id = battery's %s, want the claim's name", obj.Status.LeaseID)
	}
	if cl, ok := b.Held(got.LeaseID); !ok || cl.Name() != got.LeaseID {
		t.Errorf("Held(%s) = %v, %t, want the claim", got.LeaseID, cl, ok)
	}
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# then the Scheduler SHALL treat the Pool as exhausted as SC-021
//# requires and SHALL delete the claim when it stops waiting.

// TestExhaustedPoolIsErrExhausted empties a Pool of one and claims again.
// The fake battery leaves the second claim Pending with PoolExhausted; the
// backend returns ErrExhausted at once, well within its deadline, and the
// claim is deleted.
func TestExhaustedPoolIsErrExhausted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend(func(o *claim.Options) { o.Deadline = 20 * time.Second })
	ref := f.declare(b)

	first, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	f.waitAvailable(b, ref, 0)

	start := time.Now()
	_, err = b.ClaimVM(f.ctx, ref)
	if !errors.Is(err, poolmgr.ErrExhausted) {
		t.Fatalf("claim from the emptied pool = %v, want ErrExhausted", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("exhaustion took %s to report, want it at once rather than at the deadline", took)
	}
	exhausted := 0
	for name, reason := range f.battery.Pending() {
		if reason == batteryv1alpha1.ReasonPoolExhausted {
			exhausted++
			if _, ok := f.claim(name); ok {
				t.Errorf("claim %s is still there after the backend stopped waiting for it", name)
			}
		}
	}
	if exhausted != 1 {
		t.Errorf("claims left Pending with PoolExhausted = %d, want 1", exhausted)
	}
	if claims := f.claims(); len(claims) != 1 || claims[0].Name != first.LeaseID {
		t.Errorf("claims = %d, want only the first", len(claims))
	}
}

// TestUnknownPoolIsErrNotFound claims from a Pool nobody declared. The
// claim waits with PoolNotFound, which is ErrNotFound, so that the
// Scheduler declares the Pool again (PL-033).
func TestUnknownPoolIsErrNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()

	_, err := b.ClaimVM(f.ctx, poolmgr.PoolRef{Name: "nowhere", Namespace: runnerNamespace})
	if !errors.Is(err, poolmgr.ErrNotFound) {
		t.Fatalf("claim from an unknown pool = %v, want ErrNotFound", err)
	}
	f.eventually("the claim is deleted", func() bool { return len(f.claims()) == 0 })
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# While a Job holds a Bound claim, the Scheduler SHALL renew the
//# claim's

// TestHeldClaimIsRenewed checks that a held claim is renewed at the
// Profile's heartbeat interval, which the backend writes to the Pool, and
// that Heartbeat returns the lease expiry battery reports for it, which the
// renewals move on.
func TestHeldClaimIsRenewed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)

	pool := &batteryv1alpha1.Pool{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: f.namespace, Name: ref.Name}, pool); err != nil {
		t.Fatal(err)
	}
	if got := pool.Spec.Lease.HeartbeatInterval; got == nil || got.Duration != f.profile.Pool.HeartbeatInterval {
		t.Fatalf("pool heartbeat interval = %v, want the profile's %s", got, f.profile.Pool.HeartbeatInterval)
	}

	c, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	first, err := b.Heartbeat(f.ctx, c.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	renewals := map[time.Time]bool{}
	f.eventually("three renewals within a second or so", func() bool {
		obj, ok := f.claim(c.LeaseID)
		if ok && obj.Spec.RenewTime != nil {
			renewals[obj.Spec.RenewTime.Time] = true
		}
		return len(renewals) >= 3
	})
	f.eventually("the lease expiry moves on", func() bool {
		next, err := b.Heartbeat(f.ctx, c.LeaseID)
		return err == nil && next.After(first)
	})
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# or the claim
//# disappears while its Job runs, then the Scheduler SHALL treat the Lease as
//# lost as SC-061 requires.

// TestLostLeaseIsErrNotFound checks both ways a Lease is lost: the claim
// becomes Expired, and the claim is deleted by someone else. Heartbeat then
// answers ErrNotFound, which the Scheduler takes as a lost Lease, and an
// Expired claim is deleted.
func TestLostLeaseIsErrNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.profile.Pool.Size = 2
	b := f.backend()
	ref := f.declare(b)

	expired, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}

	f.battery.Expire(expired.LeaseID)
	f.eventually("the expired lease is lost", func() bool {
		_, err := b.Heartbeat(f.ctx, expired.LeaseID)
		return errors.Is(err, poolmgr.ErrNotFound)
	})
	f.eventually("the expired claim is deleted", func() bool {
		_, ok := f.claim(expired.LeaseID)
		return !ok
	})

	obj, _ := f.claim(deleted.LeaseID)
	if err := f.kube.Delete(f.ctx, obj); err != nil {
		t.Fatal(err)
	}
	f.eventually("the deleted lease is lost", func() bool {
		_, err := b.Heartbeat(f.ctx, deleted.LeaseID)
		return errors.Is(err, poolmgr.ErrNotFound)
	})
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# When a Job ends, the Scheduler SHALL delete its claim, which
//# releases the MicroVM to battery.

// TestReleaseVMDeletesTheClaim releases a claim: the claim is deleted, the
// fake battery puts a warm MicroVM back, a second release of the same lease
// counts as released, and a heartbeat after it finds no Lease.
func TestReleaseVMDeletesTheClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)

	c, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	f.waitAvailable(b, ref, 0)
	if err := b.ReleaseVM(f.ctx, c.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.claim(c.LeaseID); ok {
		t.Error("the claim is still there after ReleaseVM")
	}
	f.waitAvailable(b, ref, 1)
	if err := b.ReleaseVM(f.ctx, c.LeaseID); err != nil && !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("second release = %v, want it counted as released", err)
	}
	if _, err := b.Heartbeat(f.ctx, c.LeaseID); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("heartbeat after release = %v, want ErrNotFound", err)
	}
}

// TestCloseStopsRenewals closes a backend that still holds a claim. The
// claim is not deleted, and its renewals stop, so that battery expires it
// as it expires the Lease of a Runner that stopped.
func TestCloseStopsRenewals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)

	c, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	f.eventually("a first renewal", func() bool {
		obj, ok := f.claim(c.LeaseID)
		return ok && obj.Spec.RenewTime != nil
	})
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// A renewal already in flight when Close was called may still land.
	time.Sleep(2 * f.profile.Pool.HeartbeatInterval)
	obj, ok := f.claim(c.LeaseID)
	if !ok {
		t.Fatal("Close deleted the claim")
	}
	last := obj.Spec.RenewTime.Time
	time.Sleep(5 * f.profile.Pool.HeartbeatInterval)
	obj, ok = f.claim(c.LeaseID)
	if !ok {
		t.Fatal("the claim is gone")
	}
	if !obj.Spec.RenewTime.Time.Equal(last) {
		t.Errorf("claim renewed at %s after Close, last renewal before it %s", obj.Spec.RenewTime.Time, last)
	}
	if _, err := b.Subscribe(context.Background(), poolmgr.EventFilter{}); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Errorf("Subscribe after Close = %v, want ErrUnavailable", err)
	}
}
