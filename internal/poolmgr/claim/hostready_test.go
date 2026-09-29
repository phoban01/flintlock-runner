package claim_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/pkg/claimclient"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# If a claim that a Job holds has the condition `HostReady`
//# false, then the Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and delete the claim.

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# When the Scheduler aborts a Job under KF-203, the Scheduler
//# SHALL name the reason and the message of the claim's `HostReady`
//# condition in the Job's failure.

// TestHostNotReadyIsErrNotFound holds two claims, one on each Host, and
// reports one Host not ready while its MicroVM lives on. Heartbeat for
// the claim on that Host answers poolmgr.ErrNotFound, which the Scheduler
// takes as a lost Lease (SC-061), wrapping claimclient.ErrHostNotReady and
// naming the condition's reason and message. That claim is deleted; the
// claim on the other Host is still held.
func TestHostNotReadyIsErrNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.profile.Pool.Size = 2
	b := f.backend()
	ref := f.declare(b)

	first, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if first.Host.Name == second.Host.Name {
		t.Fatalf("both claims are on %s, want one on each host", first.Host.Name)
	}

	const reason, message = "KVMUnavailable", "/dev/kvm is missing"
	f.battery.SetHostNotReady(first.Host.Name, reason, message)
	var got error
	f.eventually("the heartbeat to report the host not ready", func() bool {
		_, got = b.Heartbeat(f.ctx, first.LeaseID)
		return got != nil
	})
	if !errors.Is(got, poolmgr.ErrNotFound) || !errors.Is(got, claimclient.ErrHostNotReady) {
		t.Fatalf("Heartbeat = %v, want ErrNotFound wrapping ErrHostNotReady", got)
	}
	for _, want := range []string{first.LeaseID, reason, message} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("Heartbeat = %q, want it to name %q", got, want)
		}
	}
	f.eventually("the claim on the host that is not ready to be deleted", func() bool {
		_, ok := f.claim(first.LeaseID)
		return !ok
	})
	if _, ok := b.Held(first.LeaseID); ok {
		t.Error("the claim on the host that is not ready is still held")
	}

	if _, err := b.Heartbeat(f.ctx, second.LeaseID); err != nil {
		t.Errorf("Heartbeat of the claim on the ready host = %v, want nil", err)
	}
	if _, ok := b.Held(second.LeaseID); !ok {
		t.Error("the claim on the ready host is no longer held")
	}
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# The Scheduler SHALL abort a Job under KF-203 no later than the
//# heartbeat interval of the claim's Pool plus the Pool Manager call
//# deadline after the claim's condition `HostReady` becomes false.

// TestHeartbeatSeesHostNotReadyAtOnce holds a claim whose Pool renews it
// once a minute, so the Client Library does not read the claim again
// during the test. Once the claim carries HostReady false, the very next
// Heartbeat reports it: Heartbeat reads the condition from the claim
// itself, and does not wait for the next renewal (KF-205).
func TestHeartbeatSeesHostNotReadyAtOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.profile.Pool.HeartbeatInterval = time.Minute
	f.profile.Pool.HeartbeatExpiry = 5 * time.Minute
	b := f.backend()
	ref := f.declare(b)

	c, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Heartbeat(f.ctx, c.LeaseID); err != nil {
		t.Fatalf("Heartbeat before the host is reported = %v, want nil", err)
	}

	f.battery.SetHostNotReady(c.Host.Name, batteryv1alpha1.ReasonHostNotReady, "the thin pool is gone")
	f.eventually("the claim to carry HostReady false", func() bool {
		obj, ok := f.claim(c.LeaseID)
		return ok && meta.IsStatusConditionPresentAndEqual(obj.Status.Conditions,
			batteryv1alpha1.ConditionHostReady, metav1.ConditionFalse)
	})
	cl, ok := b.Held(c.LeaseID)
	if !ok {
		t.Fatal("the claim is not held")
	}
	if err := cl.HostErr(); err != nil {
		t.Fatalf("the Client Library already reports the host (%v); the test needs it not to", err)
	}
	_, err = b.Heartbeat(f.ctx, c.LeaseID)
	if !errors.Is(err, poolmgr.ErrNotFound) || !strings.Contains(err.Error(), "the thin pool is gone") {
		t.Fatalf("the first Heartbeat after the condition = %v, want ErrNotFound naming the condition", err)
	}
}
