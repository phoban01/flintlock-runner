package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/04-pool-manager.md#claiming
//= type=test
//# The Scheduler SHALL send a `request_id` on every `ClaimVM`, chosen
//# once for the allocation and sent unchanged on every retry of it.

// TestClaimRetriesCarryOneRequestID allocates twice. The first allocation's
// claim succeeds at once. The second's first ClaimVM fails with UNAVAILABLE,
// as a call that timed out looks to the Scheduler, and its retry succeeds.
// Every call carries a request id; the retry carries the id of the call it
// retries, which is what lets battery answer it with the Lease the first
// call made; and the two allocations' ids differ.
func TestClaimRetriesCarryOneRequestID(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	var calls atomic.Int64
	client := newStubClient()
	client.script(func(c *stubClient) {
		succeed := claimsFrom("host-1")
		c.claimFn = func(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
			if calls.Add(1) == 2 {
				return nil, poolmgr.ErrUnavailable
			}
			return succeed(ctx, ref)
		}
	})
	e := newEnv(t, envConfig{
		client:  client,
		backoff: clock.Exponential{Base: time.Second, Max: time.Second},
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 2)

	first, err := e.sched.Allocate(ctx, e.reserve(ctx), JobInfo{ID: 1}, e.profile("default"))
	if err != nil {
		t.Fatalf("first Allocate = %v", err)
	}
	defer e.sched.Release(first)

	// The reservation is taken while the Pool Manager is still healthy;
	// the UNAVAILABLE answer marks it unhealthy for the backoff (PL-034).
	r := e.reserve(ctx)
	type result struct {
		h   Handle
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := e.sched.Allocate(ctx, r, JobInfo{ID: 2}, e.profile("default"))
		done <- result{h, err}
	}()
	// The failed attempt arms the backoff timer; waiting for that timer,
	// not a count, keeps the held Lease's heartbeat timer out of it.
	e.awaitTimer(ctx, time.Second)
	e.clk.Advance(time.Second)
	second := <-done
	if second.err != nil {
		t.Fatalf("second Allocate, after a retried claim = %v", second.err)
	}
	defer e.sched.Release(second.h)

	ids := client.requestIDsSeen()
	if len(ids) != 3 {
		t.Fatalf("claim calls = %d (ids %q), want the first allocation's, then the failed call and its retry", len(ids), ids)
	}
	for i, id := range ids {
		if id == "" {
			t.Fatalf("ClaimVM call %d was made with no request id", i+1)
		}
	}
	if ids[1] != ids[2] {
		t.Fatalf("the retry's request id %q differs from the failed call's %q", ids[2], ids[1])
	}
	if ids[0] == ids[1] {
		t.Fatalf("both allocations used the request id %q", ids[0])
	}
}

//= docs/requirements/04-pool-manager.md#claiming
//= type=test
//# If `ClaimVM` returns `ABORTED`, then the Scheduler SHALL
//# retry the claim after the backoff, without marking the Pool Manager
//# unhealthy or the Pool exhausted.

// TestAbortedClaimIsRetriedWithoutAMark answers the first ClaimVM with
// ABORTED, battery's reply while the Lease the request id names is being
// released, and the retry with a Lease. The retry waits out the backoff,
// carries the same request id, and neither the Pool Manager's health nor
// the Pool's capacity is touched: nothing is wrong with either.
func TestAbortedClaimIsRetriedWithoutAMark(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	var calls atomic.Int64
	client := newStubClient()
	client.script(func(c *stubClient) {
		succeed := claimsFrom("host-1")
		c.claimFn = func(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Claim, error) {
			if calls.Add(1) == 1 {
				return nil, errors.Join(poolmgr.ErrAborted, errors.New("lease x for request_id y is being released, retry"))
			}
			return succeed(ctx, ref)
		}
	})
	e := newEnv(t, envConfig{
		client:  client,
		backoff: clock.Exponential{Base: time.Second, Max: time.Second},
	})
	e.startBare(ctx)
	pool := e.poolOf("default")
	e.tracker.setAvailable(pool, 1)

	r := e.reserve(ctx)
	type result struct {
		h   Handle
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := e.sched.Allocate(ctx, r, JobInfo{ID: 3}, e.profile("default"))
		done <- result{h, err}
	}()
	e.awaitTimer(ctx, time.Second)
	e.clk.Advance(time.Second)
	got := <-done
	if got.err != nil {
		t.Fatalf("Allocate after an ABORTED claim = %v", got.err)
	}
	defer e.sched.Release(got.h)

	if n := client.claimCount(); n != 2 {
		t.Fatalf("claim calls = %d, want the aborted call and its retry", n)
	}
	if ids := client.requestIDsSeen(); ids[0] != ids[1] {
		t.Fatalf("request ids = %q, want the same on the retry", ids)
	}
	if n := e.health.unavailableCount(); n != 0 {
		t.Errorf("the pool manager was marked unavailable %d times after ABORTED, want 0", n)
	}
	if n := e.tracker.exhaustedCount(pool); n != 0 {
		t.Errorf("the pool was marked exhausted %d times after ABORTED, want 0", n)
	}
	if len(e.logs.find("being released")) == 0 {
		t.Error("the retry after ABORTED was not logged")
	}
}
