package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// The Executor finds CheckLease by a type assertion on its Scheduler, so
// losing the method would silently turn the check off.
var _ interface {
	CheckLease(ctx context.Context, h Handle) error
} = (*impl)(nil)

//= docs/requirements/03-scheduler.md#lease-keep-alive
//= type=test
//# If a heartbeat reports that the Lease no longer exists, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and drop the Allocation without a release call.

// TestCheckLeaseFindsALostLeaseAtOnce checks that CheckLease asks the Pool
// Manager at once, not at the next scheduled heartbeat: the clock never
// moves, so the keep-alive loop sends nothing. The Lease is gone, so the
// Handle fails with ErrLeaseLost and the Allocation is dropped with no
// release call.
func TestCheckLeaseFindsALostLeaseAtOnce(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = func(context.Context, string) (time.Time, error) {
			return time.Time{}, fmt.Errorf("heartbeat: %w", poolmgr.ErrNotFound)
		}
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)
	h := e.allocate(ctx, JobInfo{ID: 31}, "default")

	err := e.sched.CheckLease(ctx, h)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("CheckLease = %v, want ErrLeaseLost", err)
	}
	if got := client.beatCount(); got != 1 {
		t.Fatalf("heartbeats = %d, want the check's one", got)
	}
	select {
	case <-h.Done():
	default:
		t.Fatal("the handle is not done after the lease was found lost")
	}
	if !errors.Is(h.Err(), ErrLeaseLost) {
		t.Fatalf("handle error = %v, want ErrLeaseLost", h.Err())
	}
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use = %d, want 0 once the allocation is dropped", got)
	}

	// The Executor's Cleanup still calls Release; it makes no call.
	e.sched.Release(h)
	if got := client.releaseCount(); got != 0 {
		t.Fatalf("release calls = %d, want 0 for a lost lease", got)
	}
	// A second check answers from the Handle and asks nothing.
	if err := e.sched.CheckLease(ctx, h); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("second CheckLease = %v, want ErrLeaseLost", err)
	}
	if got := client.beatCount(); got != 1 {
		t.Fatalf("heartbeats after the second check = %d, want 1", got)
	}
}

// TestCheckLeaseKeepsAHeldLease checks the answers that must not lose a
// Lease: a heartbeat that succeeds, which also records the new expiry, and
// a heartbeat that fails for another reason before the expiry.
func TestCheckLeaseKeepsAHeldLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		beat func(clk *clock.Fake) func(context.Context, string) (time.Time, error)
		// extended is set when the check has to move the expiry out.
		extended bool
	}{
		{
			name: "the heartbeat succeeds",
			beat: func(clk *clock.Fake) func(context.Context, string) (time.Time, error) {
				return beatsFor(clk, time.Hour)
			},
			extended: true,
		},
		{
			name: "the heartbeat fails before the expiry",
			beat: func(*clock.Fake) func(context.Context, string) (time.Time, error) {
				return func(context.Context, string) (time.Time, error) {
					return time.Time{}, errors.New("connection reset")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := testContext(t)

			client := newStubClient()
			e := newEnv(t, envConfig{client: client})
			client.script(func(c *stubClient) {
				c.claimFn = claimsFrom("host-1")
				c.beatFn = tt.beat(e.clk)
			})
			e.startBare(ctx)
			e.tracker.setAvailable(e.poolOf("default"), 1)
			h := e.allocate(ctx, JobInfo{ID: 32}, "default")
			defer e.sched.Release(h)
			before := h.Allocation().Lease.ExpiresAt

			if err := e.sched.CheckLease(ctx, h); err != nil {
				t.Fatalf("CheckLease = %v, want nil while the lease is held", err)
			}
			if got := client.beatCount(); got != 1 {
				t.Fatalf("heartbeats = %d, want the check's one", got)
			}
			select {
			case <-h.Done():
				t.Fatalf("the handle failed with %v while the lease is held", h.Err())
			default:
			}
			after := h.Allocation().Lease.ExpiresAt
			if tt.extended && !after.After(before) {
				t.Fatalf("lease expiry = %v, want it moved past %v", after, before)
			}
			if !tt.extended && !after.Equal(before) {
				t.Fatalf("lease expiry = %v, want it kept at %v", after, before)
			}
		})
	}
}

// TestCheckLeaseAfterReleaseAsksNothing checks that a released Allocation
// is not checked: its Lease was handed back on purpose, and a NOT_FOUND for
// it is not a lost Lease.
func TestCheckLeaseAfterReleaseAsksNothing(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = func(context.Context, string) (time.Time, error) {
			return time.Time{}, poolmgr.ErrNotFound
		}
	})
	e := newEnv(t, envConfig{client: client})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)
	h := e.allocate(ctx, JobInfo{ID: 33}, "default")
	e.sched.Release(h)

	if err := e.sched.CheckLease(ctx, h); err != nil {
		t.Fatalf("CheckLease after Release = %v, want nil", err)
	}
	if got := client.beatCount(); got != 0 {
		t.Fatalf("heartbeats = %d, want none for a released allocation", got)
	}
	if h.Err() != nil {
		t.Fatalf("handle error = %v, want nil after a normal release", h.Err())
	}
}
