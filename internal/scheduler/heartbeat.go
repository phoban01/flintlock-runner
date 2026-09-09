package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// minHeartbeatDelay keeps the keep-alive loop from spinning when the Lease
// expiry is so close that half of what is left rounds to nothing.
const minHeartbeatDelay = time.Millisecond

// startHeartbeat starts the keep-alive loop of one Allocation. The loop runs
// under the Scheduler's background context, not the Job's, so a Lease is
// still heartbeated while the Runner is shutting down (GL-071).
func (s *impl) startHeartbeat(h *handle) {
	ctx, cancel := context.WithCancel(s.background())

	h.mu.Lock()
	h.stopHeartbeat = cancel
	h.mu.Unlock()

	// The loop releases the context when it returns, so a Lease that was lost
	// rather than released leaves nothing attached to the background context.
	if !s.inBackground(func() { defer cancel(); s.heartbeatLoop(ctx, h) }) {
		cancel()
	}
}

// stopHeartbeat ends the keep-alive loop of an Allocation. It does not wait
// for the loop, so it is safe to call from the loop itself.
func (s *impl) stopHeartbeat(h *handle) {
	h.mu.Lock()
	cancel := h.stopHeartbeat
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

//= docs/requirements/03-scheduler.md#lease-keep-alive
//# While an Allocation holds a Lease, the Scheduler SHALL send a
//# heartbeat to the Pool Manager at an interval no longer than half the time
//# remaining until the Lease expiry it last received.

// heartbeatLoop keeps one Lease alive until the Allocation ends. Each round
// waits at most half the time left on the expiry the last Heartbeat returned,
// so a Lease is heartbeated at least twice within its own lifetime and one
// lost call is never enough to lose it.
func (s *impl) heartbeatLoop(ctx context.Context, h *handle) {
	alloc := h.Allocation()
	log := s.log.With("job", alloc.JobID, "vm", alloc.VMUID, "lease", alloc.Lease.ID,
		"pool", alloc.Lease.Pool.String(), "host", alloc.Placement.Host)
	interval := s.heartbeatInterval(alloc.Profile)
	attempt := 0

	for {
		lease := h.lease()
		now := s.clk.Now()
		if !now.Before(lease.ExpiresAt) {
			s.leaseLost(h, log, fmt.Errorf("%w: lease %s expired at %s without a successful heartbeat",
				ErrLeaseLost, lease.ID, lease.ExpiresAt.Format(time.RFC3339)))
			return
		}

		timer := s.clk.NewTimer(heartbeatDelay(lease, now, interval, s.backoff, attempt))
		select {
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()
			return
		case <-h.Done():
			timer.Stop()
			return
		}

		callCtx, cancel := s.callContext()
		expiresAt, err := s.deps.PoolManager.Heartbeat(callCtx, lease.ID)
		cancel()

		switch {
		case err == nil:
			attempt = 0
			lease.ExpiresAt = expiresAt
			lease.LastHeartbeatAt = s.clk.Now()
			h.setLease(lease)
		case errors.Is(err, poolmgr.ErrNotFound):
			s.leaseLost(h, log, fmt.Errorf("%w: the pool manager no longer has lease %s", ErrLeaseLost, lease.ID))
			return
		default:
			attempt++
			s.metrics.FailureCounted(FailureHeartbeat)
			log.Warn("lease heartbeat failed", "attempt", attempt, "error", err,
				"expires_at", lease.ExpiresAt)
		}
	}
}

// heartbeatDelay is how long to wait before the next heartbeat: never more
// than half the time left on the Lease, never more than the Pool's configured
// heartbeat interval, and after a failure no more than the backoff, so that a
// failing heartbeat is retried well before the expiry.
func heartbeatDelay(lease Lease, now time.Time, interval time.Duration, backoff backoffPolicy, attempt int) time.Duration {
	d := lease.ExpiresAt.Sub(now) / 2
	if interval > 0 && interval < d {
		d = interval
	}
	if attempt > 0 {
		if b := backoff.Next(attempt - 1); b < d {
			d = b
		}
	}
	if d < minHeartbeatDelay {
		d = minHeartbeatDelay
	}
	return d
}

// backoffPolicy is clock.Backoff, named locally so heartbeatDelay is a pure
// function that a table-driven test can call without a Scheduler.
type backoffPolicy interface {
	Next(attempt int) time.Duration
}

// heartbeatInterval is the Pool's configured heartbeat interval for the
// Profile of an Allocation (PL-040).
func (s *impl) heartbeatInterval(profile string) time.Duration {
	if p := s.profileByName(profile); p != nil && p.Pool.HeartbeatInterval > 0 {
		return p.Pool.HeartbeatInterval
	}
	return config.DefaultHeartbeatInterval
}

//= docs/requirements/03-scheduler.md#lease-keep-alive
//# If a heartbeat reports that the Lease no longer exists, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and drop the Allocation without a release call.

//= docs/requirements/03-scheduler.md#lease-keep-alive
//# If heartbeats fail for longer than the Lease expiry, then the
//# Scheduler SHALL treat the Lease as lost and abort the Job.

// leaseLost ends an Allocation whose Lease is gone, either because the Pool
// Manager answered NOT_FOUND or because heartbeats kept failing until the
// expiry passed. The Handle is failed with ErrLeaseLost, which the Executor
// turns into a `runner_system_failure` by cancelling the Build's context with
// that cause, and the Slot is returned. No ReleaseVM call is made: the Lease
// the Runner would name is exactly the one that no longer exists.
func (s *impl) leaseLost(h *handle, log *slog.Logger, cause error) {
	h.markLeaseGone()
	h.fail(cause)
	log.Error("lease lost, aborting the job", "error", cause)
	s.finish(h)
}
