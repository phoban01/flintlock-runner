package claim

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/pkg/claimclient"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// releaseTimeout bounds the release of a claim the backend lets go of on its
// own: one whose Lease is lost, or one that bound without the status a
// Claim needs.
const releaseTimeout = 30 * time.Second

//= docs/requirements/12-cluster-fleet.md#battery-claims
//# Where the claim backend is configured, the Scheduler SHALL
//# obtain a MicroVM for a Job by creating a `MicroVMClaim`

// ClaimVM implements poolmgr.Lease. It creates a claim on the Pool for the
// Holder with the Client Library, which returns only once the claim is
// Bound, and maps the Bound claim to a poolmgr.Claim.
//
// A claim that stays Pending says why in its Bound condition, and ClaimVM
// stops waiting as soon as the reason tells the Scheduler what to do: an
// exhausted Pool is poolmgr.ErrExhausted, which the Scheduler waits out
// (SC-021), and an unknown Pool is poolmgr.ErrNotFound, which makes it
// declare the Pool again (PL-033). The Client Library deletes a claim it
// stops waiting for. A claim that neither binds nor says why within the
// deadline is poolmgr.ErrUnavailable: battery-operator is not answering.
func (b *Backend) ClaimVM(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
	claims, err := b.claimClient(ctx)
	if err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, b.opts.Deadline)
	defer cancel()

	// OnPending runs on Claim's own goroutine, before Claim returns, so the
	// reason it records is read below without a lock.
	var stopped error
	cl, err := claims.Claim(waitCtx, claimclient.Request{
		Pool:               ref.Name,
		ServiceAccountName: b.opts.Holder,
		GenerateName:       ref.Name + "-",
		Labels:             map[string]string{LabelRunner: b.opts.RunnerName},
		OnPending: func(p claimclient.Pending) {
			b.log.Debug("claim is pending", "pool", ref.String(), "reason", p.Reason, "message", p.Message)
			if stopped = pendingError(ref, p); stopped != nil {
				cancel()
			}
		},
	})
	switch {
	case err == nil:
	case stopped != nil && ctx.Err() == nil:
		return nil, stopped
	case ctx.Err() != nil:
		return nil, fmt.Errorf("claim: claiming from %s: %w", ref, ctx.Err())
	case errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("claim: claiming from %s: %w: the claim did not bind within %s: %w",
			ref, poolmgr.ErrUnavailable, b.opts.Deadline, err)
	default:
		return nil, claimError(ref, err)
	}

	claim, err := claimOf(cl)
	if err != nil {
		b.letGo(cl, "it bound without the status a claim needs")
		return nil, fmt.Errorf("claim: claiming from %s: %w", ref, err)
	}
	b.mu.Lock()
	b.held[claim.LeaseID] = cl
	b.mu.Unlock()
	b.log.Info("microvm claimed", "pool", ref.String(), "claim", claim.LeaseID,
		"vm", claim.VMUID, "node", claim.Host.Name)
	return claim, nil
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//# then the Scheduler SHALL treat the Pool as exhausted as SC-021
//# requires and SHALL delete the claim when it stops waiting.

// pendingError is the error a Pending claim's reason stands for, or nil
// while the claim should be waited for. An exhausted Pool and a Pool with no
// Host that can run its MicroVMs both have no warm MicroVM to give, and a
// failed call to battery is battery being unavailable. The Client Library
// deletes the claim once ClaimVM stops waiting for it.
func pendingError(ref poolmgr.PoolRef, p claimclient.Pending) error {
	switch p.Reason {
	case batteryv1alpha1.ReasonPoolExhausted, batteryv1alpha1.ReasonNoEligibleHost:
		return fmt.Errorf("claim: claiming from %s: %w: %s", ref, poolmgr.ErrExhausted, p.Message)
	case batteryv1alpha1.ReasonPoolNotFound:
		return fmt.Errorf("claim: claiming from %s: %w: %s", ref, poolmgr.ErrNotFound, p.Message)
	case batteryv1alpha1.ReasonBatteryUnavailable:
		return fmt.Errorf("claim: claiming from %s: %w: %s", ref, poolmgr.ErrUnavailable, p.Message)
	default:
		return nil
	}
}

// claimError maps a failed claim onto the sentinel errors. A claim deleted
// or expired before it bound is someone else's doing, and the next attempt
// makes a new one, so it is retried as an unavailable Pool Manager is. An
// API error that says nothing about the Pool, such as a missing Holder, is
// left unwrapped, because re-declaring the Pool cannot help.
func claimError(ref poolmgr.PoolRef, err error) error {
	op := "claiming from " + ref.String()
	switch {
	case errors.Is(err, claimclient.ErrLeaseLost):
		return fmt.Errorf("claim: %s: %w: %w", op, poolmgr.ErrUnavailable, err)
	case apierrors.IsNotFound(err), apierrors.IsAlreadyExists(err):
		return fmt.Errorf("claim: %s: %w", op, err)
	default:
		return translate(op, err)
	}
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//# The Scheduler SHALL take the claimed MicroVM's uid

//= docs/requirements/12-cluster-fleet.md#battery-claims
//# and from nothing else.

// claimOf maps a Bound claim to a poolmgr.Claim from the claim's status and
// nothing else: the lease id is the claim's name, the uid is
// status.microVM.uid, and the Host is status.host.nodeName with the Exec
// Agent's address, status.host.agentAddress. The Client Library reads all
// three from the status of the Bound claim. A Bound claim without a MicroVM
// or a node name is refused, because the Scheduler cannot place it.
func claimOf(cl *claimclient.Claim) (*poolmgr.Claim, error) {
	claim := &poolmgr.Claim{
		LeaseID: cl.Name(),
		VMUID:   cl.MicroVMUID(),
		Host:    poolmgr.HostRef{Name: cl.NodeName(), Address: cl.AgentAddress()},
	}
	switch {
	case claim.VMUID == "":
		return nil, fmt.Errorf("claim %s is Bound but names no MicroVM uid", claim.LeaseID)
	case claim.Host.Name == "":
		return nil, fmt.Errorf("claim %s is Bound but names no node", claim.LeaseID)
	}
	return claim, nil
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//# While a Job holds a Bound claim, the Scheduler SHALL renew the
//# claim's

//= docs/requirements/12-cluster-fleet.md#battery-claims
//# or the claim
//# disappears while its Job runs, then the Scheduler SHALL treat the Lease as
//# lost as SC-061 requires.

// Heartbeat implements poolmgr.Lease. It renews nothing itself: the Client
// Library renews a held claim at the heartbeat interval of the claim's
// Pool, which CreatePool sets from the Profile. Heartbeat reads the claim
// and returns the lease expiry in its status, which is battery's own
// expiry. It returns poolmgr.ErrNotFound, which the Scheduler takes as a
// lost Lease (SC-061), when the Client Library has seen the Lease lost, and
// when the claim is Expired, being deleted, replaced or gone. Such a claim
// is let go of: its renewal stops and it is deleted, since the Scheduler
// makes no release call for a lost Lease.
func (b *Backend) Heartbeat(ctx context.Context, leaseID string) (time.Time, error) {
	cl, ok := b.Held(leaseID)
	if !ok {
		return time.Time{}, fmt.Errorf("claim: heartbeat: %w: no claim is held under lease %s", poolmgr.ErrNotFound, leaseID)
	}
	if err := cl.Err(); err != nil {
		b.lose(leaseID, cl, err.Error())
		return time.Time{}, fmt.Errorf("claim: heartbeat: %w: %w", poolmgr.ErrNotFound, err)
	}

	ctx, cancel := b.call(ctx)
	defer cancel()
	obj := &batteryv1alpha1.MicroVMClaim{}
	err := b.kube.Get(ctx, client.ObjectKey{Namespace: b.opts.Namespace, Name: leaseID}, obj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			b.lose(leaseID, cl, "the claim is gone")
		}
		return time.Time{}, translate("reading claim "+leaseID, err)
	}
	if why := lost(obj, cl); why != "" {
		b.lose(leaseID, cl, why)
		return time.Time{}, fmt.Errorf("claim: heartbeat: %w: claim %s %s", poolmgr.ErrNotFound, leaseID, why)
	}
	if obj.Status.LeaseExpiresAt == nil {
		return time.Time{}, fmt.Errorf("claim: heartbeat: %w: claim %s has no lease expiry yet", poolmgr.ErrUnavailable, leaseID)
	}
	return obj.Status.LeaseExpiresAt.Time, nil
}

// lost says why a claim read from the API server no longer holds the Lease
// the Runner claimed, or is empty while it does.
func lost(obj *batteryv1alpha1.MicroVMClaim, cl *claimclient.Claim) string {
	switch {
	case obj.Status.Phase == batteryv1alpha1.MicroVMClaimExpired:
		return "is Expired"
	case obj.DeletionTimestamp != nil:
		return "is being deleted"
	case obj.Status.MicroVM == nil || obj.Status.MicroVM.UID != cl.MicroVMUID():
		return "no longer holds the claimed MicroVM"
	default:
		return ""
	}
}

// lose forgets a claim whose Lease is lost and lets it go.
func (b *Backend) lose(leaseID string, cl *claimclient.Claim, why string) {
	b.mu.Lock()
	if b.held[leaseID] != cl {
		b.mu.Unlock()
		return
	}
	delete(b.held, leaseID)
	b.mu.Unlock()
	b.log.Warn("lease lost", "claim", leaseID, "reason", why)
	b.letGo(cl, why)
}

// letGo stops renewing a claim and deletes it, in the background. A claim
// already gone is released without error.
func (b *Backend) letGo(cl *claimclient.Claim, why string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		if err := cl.Release(ctx); err != nil {
			b.log.Warn("claim could not be deleted", "claim", cl.Name(), "reason", why, "error", err)
		}
	}()
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//# When a Job ends, the Scheduler SHALL delete its claim, which
//# releases the MicroVM to battery.

// ReleaseVM implements poolmgr.Lease. It deletes the claim, through the
// Client Library for a claim the Runner holds, which stops its renewal
// first, and directly for one it does not. A claim that is gone already
// counts as released (PL-042). A failed deletion keeps the claim held, so
// the Scheduler's retry deletes it (PL-043).
func (b *Backend) ReleaseVM(ctx context.Context, leaseID string) error {
	ctx, cancel := b.call(ctx)
	defer cancel()
	if cl, ok := b.Held(leaseID); ok {
		if err := cl.Release(ctx); err != nil {
			return translate("deleting claim "+leaseID, err)
		}
		b.mu.Lock()
		if b.held[leaseID] == cl {
			delete(b.held, leaseID)
		}
		b.mu.Unlock()
		b.log.Info("claim released", "claim", leaseID)
		return nil
	}
	obj := &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: b.opts.Namespace, Name: leaseID}}
	if err := b.kube.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return translate("deleting claim "+leaseID, err)
	}
	return nil
}
