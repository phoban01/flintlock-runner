package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// errHostNotReady is what the claim backend's Heartbeat answers for a
// claim whose Host is not ready: poolmgr.ErrNotFound, with the reason and
// message of the claim's HostReady condition.
var errHostNotReady = fmt.Errorf("claim: heartbeat: %w: the host is not ready: claim lease-1: KVMUnavailable: /dev/kvm is missing",
	poolmgr.ErrNotFound)

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

// TestHostNotReadyAtTheHeartbeatAbortsTheJob holds a claim on the claim
// backend whose Exec Agent is never probed, so only the heartbeat can see
// the Host. Once the claim's Host is not ready, the next heartbeat answers
// as the claim backend does, and the Job is aborted with ErrLeaseLost,
// which the Executor reports as runner_system_failure (SC-061). The
// failure quotes the condition's reason and message. The Scheduler makes
// no release call: the claim backend has deleted the claim already.
func TestHostNotReadyAtTheHeartbeatAbortsTheJob(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	var notReady atomic.Bool
	var beats atomic.Int32
	client := newStubClient()
	e := newEnv(t, envConfig{client: client, inventory: []config.HostEntry{}, tune: claimBackend})
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("node-7")
		c.beatFn = func(context.Context, string) (time.Time, error) {
			beats.Add(1)
			if notReady.Load() {
				return time.Time{}, errHostNotReady
			}
			return e.clk.Now().Add(time.Hour), nil
		}
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)
	h := e.allocate(ctx, JobInfo{ID: 31}, "default")
	interval := e.profile("default").Pool.HeartbeatInterval

	e.awaitTimer(ctx, interval)
	e.clk.Advance(interval)
	waitFor(t, ctx, func() bool { return beats.Load() == 1 })
	select {
	case <-h.Done():
		t.Fatalf("the Job was aborted while its host was ready: %v", h.Err())
	default:
	}

	notReady.Store(true)
	e.awaitTimer(ctx, interval)
	e.clk.Advance(interval)
	select {
	case <-h.Done():
	case <-ctx.Done():
		t.Fatal("the Job was not aborted at the first heartbeat after its host was not ready")
	}
	if n := beats.Load(); n != 2 {
		t.Errorf("%d heartbeats, want the Job aborted at the second", n)
	}

	err := h.Err()
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Err = %v, want ErrLeaseLost", err)
	}
	for _, want := range []string{"KVMUnavailable", "/dev/kvm is missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Err = %q, want it to name %q", err, want)
		}
	}
	// The answer is quoted: the Job's failure is the lost Lease, not the
	// backend's sentinel.
	if errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("Err = %v wraps poolmgr.ErrNotFound, want it quoted", err)
	}
	waitFor(t, ctx, func() bool { return e.sched.Snapshot().SlotsInUse == 0 })
	if n := client.releaseCount(); n != 0 {
		t.Errorf("%d release calls, want none: the claim backend deletes the claim", n)
	}
}
