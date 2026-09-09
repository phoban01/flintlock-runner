package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc/codes"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL support fault injection for claim latency, a
//# period of `UNAVAILABLE`, hook failure and a dropped `Events` stream.

// TestFaultClaimLatency holds a claim for exactly the injected latency on the
// fake clock: the call is still outstanding until the clock reaches the
// deadline, and it succeeds when it does. The Scheduler's claim deadline
// (PL-002) is exercised the same way, by cancelling instead of advancing.
func TestFaultClaimLatency(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	h.createPool(h.spec("pool", 2, "host-a"))
	h.pm.SetFaults(poolmgr.Faults{ClaimLatency: 5 * time.Second})

	type result struct {
		claim *poolmgr.Claim
		err   error
	}
	done := make(chan result, 1)
	go func() {
		claim, err := h.client.ClaimVM(h.ctx, h.ref("pool"))
		done <- result{claim, err}
	}()

	// The claim arms a second timer on the fake clock, next to the control
	// loop's; until it fires the claim is outstanding.
	h.waitTimers(2)
	select {
	case got := <-done:
		t.Fatalf("ClaimVM returned %+v before the injected latency elapsed", got)
	default:
	}
	h.clk.Advance(5 * time.Second)
	got := <-done
	if got.err != nil {
		t.Fatalf("ClaimVM after the latency: %v", got.err)
	}
	if got.claim.LeaseID == "" {
		t.Fatalf("ClaimVM returned %+v, want a lease", got.claim)
	}
	if leases := h.pm.Leases(); len(leases) != 1 || !leases[0].ClaimedAt.Equal(testEpoch.Add(5*time.Second)) {
		t.Fatalf("lease = %+v, want one claimed at the end of the latency", leases)
	}

	// A caller that gives up while the latency runs gets its own context
	// error, and no Lease is left behind.
	h.pm.SetFaults(poolmgr.Faults{ClaimLatency: time.Hour})
	ctx, cancel := context.WithCancel(h.ctx)
	errc := make(chan error, 1)
	go func() {
		_, err := h.client.ClaimVM(ctx, h.ref("pool"))
		errc <- err
	}()
	h.waitTimers(2)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ClaimVM = %v, want context.Canceled", err)
	}
	if leases := h.pm.Leases(); len(leases) != 1 {
		t.Fatalf("leases after the cancelled claim = %+v, want only the first one", leases)
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL support fault injection for claim latency, a
//# period of `UNAVAILABLE`, hook failure and a dropped `Events` stream.

// TestFaultUnavailablePeriod makes every RPC fail with UNAVAILABLE for a
// period measured on the fake clock, which is what PL-034 and PL-035 need,
// and checks that the fake serves normally again once the period is over.
func TestFaultUnavailablePeriod(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	h.createPool(h.spec("pool", 1, "host-a"))
	admin := poolmgrv1.NewPoolAdminClient(h.rawConn())
	ref := refToProto(h.ref("pool"))

	h.pm.SetFaults(poolmgr.Faults{UnavailableFor: 30 * time.Second})
	if _, err := admin.GetPool(h.ctx, &poolmgrv1.GetPoolRequest{Ref: ref}); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("GetPool while unavailable: code %v, want UNAVAILABLE", statusCode(t, err))
	}
	if _, err := h.rawLease().ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: ref}); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("ClaimVM while unavailable: code %v, want UNAVAILABLE", statusCode(t, err))
	}
	if _, err := h.client.GetPool(h.ctx, h.ref("pool")); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Fatalf("GetPool through the client = %v, want ErrUnavailable", err)
	}

	// Halfway through it is still unavailable; at the end it is not.
	h.advance(20 * time.Second)
	if _, err := admin.GetPool(h.ctx, &poolmgrv1.GetPoolRequest{Ref: ref}); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("GetPool 20s into a 30s outage: code %v, want UNAVAILABLE", statusCode(t, err))
	}
	h.advance(10 * time.Second)
	if _, err := admin.GetPool(h.ctx, &poolmgrv1.GetPoolRequest{Ref: ref}); err != nil {
		t.Fatalf("GetPool after the outage: %v", err)
	}
	if _, err := h.client.ClaimVM(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("ClaimVM after the outage: %v", err)
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL support fault injection for claim latency, a
//# period of `UNAVAILABLE`, hook failure and a dropped `Events` stream.

// TestFaultHookFailure fails a create hook and a pre-lease hook, once each,
// and checks that the Pool's hook failure policy is what decides the
// MicroVM's fate: DELETE_AND_REPLACE returns it to its Host, QUARANTINE
// keeps it for inspection. Both emit VM_HOOK_FAILED, and the injection is
// consumed, so the retry that follows succeeds.
func TestFaultHookFailure(t *testing.T) {
	t.Run("create hook, delete and replace", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		host := h.stubs["host-a"]
		h.pm.SetFaults(poolmgr.Faults{HookFailures: []poolmgr.HookFailure{{Hook: poolmgr.HookCreate, Remaining: 1}}})
		if _, err := h.client.CreatePool(h.ctx, h.spec("pool", 1, "host-a")); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}

		failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
		if p := payloadOf(t, failed); p["hook"] != string(poolmgr.HookCreate) {
			t.Fatalf("VM_HOOK_FAILED payload = %v, want the create hook", p)
		}
		h.waitDeletion(host, failed.VMUID)
		assertDeleted(t, host, failed.VMUID)

		// The injection was consumed, so the tick's replacement comes up.
		h.advance(testInterval)
		available := h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if available.VMUID == failed.VMUID {
			t.Fatalf("VM_AVAILABLE for %q, the microvm whose hook failed", available.VMUID)
		}
		if st := h.pool("pool").Status; st.Available != 1 || st.Quarantined != 0 {
			t.Fatalf("status after the replacement = %+v, want one available and none quarantined", st)
		}
	})

	t.Run("create hook, quarantine", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		host := h.stubs["host-a"]
		spec := h.spec("pool", 1, "host-a")
		spec.HookFailurePolicy = poolmgrv1.HookFailurePolicy_QUARANTINE
		h.pm.SetFaults(poolmgr.Faults{HookFailures: []poolmgr.HookFailure{{Hook: poolmgr.HookCreate, Remaining: 1}}})
		if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}

		failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
		if p := payloadOf(t, failed); p["policy"] != poolmgrv1.HookFailurePolicy_QUARANTINE.String() {
			t.Fatalf("VM_HOOK_FAILED payload = %v, want the quarantine policy", p)
		}
		h.advance(testInterval)
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if st := h.pool("pool").Status; st.Quarantined != 1 || st.Available != 1 {
			t.Fatalf("status = %+v, want the quarantined microvm kept and a replacement available", st)
		}
		host.mu.Lock()
		_, kept := host.vms[failed.VMUID]
		host.mu.Unlock()
		if !kept {
			t.Fatalf("quarantined microvm %s was deleted from its host", failed.VMUID)
		}
	})

	t.Run("pre-lease hook", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		host := h.stubs["host-a"]
		h.createPool(h.spec("pool", 1, "host-a"))
		h.pm.SetFaults(poolmgr.Faults{HookFailures: []poolmgr.HookFailure{{
			Pool: h.ref("pool"), Hook: poolmgr.HookPreLease, Remaining: 1,
		}}})

		_, err := h.rawLease().ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("pool"))})
		if code := statusCode(t, err); code != codes.Internal {
			t.Fatalf("ClaimVM with a failing pre-lease hook: code %v, want INTERNAL", code)
		}
		failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
		if p := payloadOf(t, failed); p["hook"] != string(poolmgr.HookPreLease) {
			t.Fatalf("VM_HOOK_FAILED payload = %v, want the pre-lease hook", p)
		}
		if leases := h.pm.Leases(); len(leases) != 0 {
			t.Fatalf("leases after the failed claim = %+v, want none", leases)
		}
		h.waitDeletion(host, failed.VMUID)

		// The next claim, on the replacement, works.
		h.advance(testInterval)
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if _, err := h.client.ClaimVM(h.ctx, h.ref("pool")); err != nil {
			t.Fatalf("ClaimVM after the injection was consumed: %v", err)
		}
	})
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL support fault injection for claim latency, a
//# period of `UNAVAILABLE`, hook failure and a dropped `Events` stream.

// TestFaultDropEventsStream drops every open Events stream once. Each
// subscriber sees the stream end with UNAVAILABLE, which is what makes the
// Tracker fall back to polling and re-subscribe (PL-051), and a new
// subscription made afterwards works and is replayed the events it missed.
func TestFaultDropEventsStream(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	h.createPool(h.spec("pool", 1, "host-a"))

	second, err := h.client.Subscribe(h.ctx, poolmgr.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = second.Close() }()
	// Drain the replay so that the next Recv is the drop itself.
	for i := 0; i < 3; i++ {
		if _, err := second.Recv(h.ctx); err != nil {
			t.Fatalf("Recv of the replay: %v", err)
		}
	}

	if open := h.pm.events.open(); open != 2 {
		t.Fatalf("%d open Events streams, want the harness's and this test's", open)
	}
	h.pm.SetFaults(poolmgr.Faults{DropEventsStream: true})
	for name, stream := range map[string]poolmgr.EventStream{"harness": h.events, "second": second} {
		_, err := stream.Recv(h.ctx)
		if !errors.Is(err, poolmgr.ErrUnavailable) {
			t.Fatalf("Recv on the %s stream = %v, want ErrUnavailable", name, err)
		}
		// The stream stays ended; it does not silently resume.
		if _, err := stream.Recv(h.ctx); !errors.Is(err, poolmgr.ErrUnavailable) {
			t.Fatalf("second Recv on the %s stream = %v, want ErrUnavailable again", name, err)
		}
	}
	// The switch clears itself, so the Tracker's re-subscription survives.
	if got := h.pm.Faults(); got.DropEventsStream {
		t.Fatalf("Faults().DropEventsStream = true, want it cleared after the drop")
	}

	claim, err := h.client.ClaimVM(h.ctx, h.ref("pool"))
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	resubscribed, err := h.client.Subscribe(h.ctx, poolmgr.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe after the drop: %v", err)
	}
	defer func() { _ = resubscribed.Close() }()
	h.recvUntilClaim(resubscribed, claim.VMUID)
}

// recvUntilClaim reads stream until it carries VM_CLAIMED for uid, which the
// replay a new subscriber is given already holds.
func (h *harness) recvUntilClaim(stream poolmgr.EventStream, uid string) {
	h.t.Helper()
	for {
		e, err := stream.Recv(h.ctx)
		if err != nil {
			h.t.Fatalf("waiting for the claim of %s on the re-subscribed stream: %v", uid, err)
		}
		if e.Type == poolmgrv1.EventType_VM_CLAIMED && e.VMUID == uid {
			return
		}
	}
}
