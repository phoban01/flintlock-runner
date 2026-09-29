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
// still heartbeated while the Runner is shutting down (GL-071). The Exec
// Agent probe of a claim runs under the same context (KF-200).
func (s *impl) startHeartbeat(h *handle) {
	ctx, cancel := context.WithCancel(s.background())

	h.mu.Lock()
	h.stopHeartbeat = cancel
	h.mu.Unlock()

	// The loop releases the context when it returns, so a Lease that was lost
	// rather than released leaves nothing attached to the background context.
	if !s.inBackground(func() { defer cancel(); s.heartbeatLoop(ctx, h) }) {
		cancel()
		return
	}
	s.startAgentProbe(ctx, h)
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
		// A cancelled loop outranks anything the last round learned. The
		// Allocation is over, so neither an expiry that has since passed nor
		// a heartbeat that failed on the way out is a lost Lease.
		if ctx.Err() != nil {
			return
		}

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

		// The call deliberately survives cancellation (callContext), so it is
		// still in flight while Release runs stopHeartbeat and ReleaseVM. If
		// the release reached the Pool Manager first this heartbeat answers
		// NOT_FOUND for a Lease that was handed back on purpose; failing the
		// Handle for it would turn a finished Job into a system failure.
		if ctx.Err() != nil {
			return
		}

		switch {
		case err == nil:
			attempt = 0
			lease.ExpiresAt = expiresAt
			lease.LastHeartbeatAt = s.clk.Now()
			h.setLease(lease)
		case errors.Is(err, poolmgr.ErrNotFound):
			s.leaseLost(h, log, leaseGone(lease.ID, err))
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
// expiry passed. The Handle is failed with ErrLeaseLost and the Slot is
// returned. No ReleaseVM call is made: the Lease the Runner would name is
// exactly the one that no longer exists.
//
// SC-061 is split across two packages, and only the abort and the dropped
// Allocation are this one's. The `runner_system_failure` half is the
// Executor's: it turns ErrLeaseLost from Handle.Err into a common.BuildError
// carrying RunnerSystemFailure by cancelling the Build's context with that
// cause. The Executor also calls CheckLease when a Stage ends without an
// exit status, so that a MicroVM deleted with its Lease is not reported as
// a script failure.
func (s *impl) leaseLost(h *handle, log *slog.Logger, cause error) {
	h.markLeaseGone()
	h.fail(cause)
	log.Error("lease lost, aborting the job", "error", cause)
	s.finish(h)
}

//= docs/requirements/03-scheduler.md#lease-keep-alive
//# If a heartbeat reports that the Lease no longer exists, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and drop the Allocation without a release call.

// CheckLease sends one heartbeat for the Lease of h at once, outside the
// keep-alive loop, and returns the Handle's error when the Lease is lost. It
// returns nil while the Lease is held, and also when the answer is unknown.
//
// The Executor calls it when a Stage ends without an exit status. Battery
// deletes the MicroVM of a Lease that has expired, and that kills the Stage
// at once, often before the next scheduled heartbeat. Without this call the
// Job would be reported by how its command died, not by why. A lost Lease
// is handled as the keep-alive loop handles it: the Handle is failed with
// ErrLeaseLost and the Allocation is dropped with no release call.
//
// A heartbeat that fails for another reason proves nothing, unless the
// Lease expiry it last received has passed (SC-062). An Allocation that has
// already ended, by Release or otherwise, is not checked.
func (s *impl) CheckLease(ctx context.Context, h Handle) error {
	hh, ok := h.(*handle)
	if !ok || hh == nil {
		return nil
	}
	select {
	case <-hh.Done():
		return hh.Err()
	default:
	}
	if !s.holds(hh) {
		return nil
	}

	alloc := hh.Allocation()
	log := s.log.With("job", alloc.JobID, "vm", alloc.VMUID, "lease", alloc.Lease.ID,
		"pool", alloc.Lease.Pool.String(), "host", alloc.Placement.Host)
	lease := alloc.Lease

	deadline := s.set.PoolManager.Deadline
	if deadline <= 0 {
		deadline = config.DefaultPoolManagerDeadline
	}
	callCtx, cancel := context.WithTimeout(ctx, deadline)
	expiresAt, err := s.deps.PoolManager.Heartbeat(callCtx, lease.ID)
	cancel()

	// The Allocation can end while the call is in flight. A Lease handed
	// back on purpose is not a lost one.
	if !s.holds(hh) {
		return hh.Err()
	}

	var cause error
	switch {
	case err == nil:
		lease.ExpiresAt = expiresAt
		lease.LastHeartbeatAt = s.clk.Now()
		hh.setLease(lease)
		return nil
	case errors.Is(err, poolmgr.ErrNotFound):
		cause = leaseGone(lease.ID, err)
	case !s.clk.Now().Before(lease.ExpiresAt):
		cause = fmt.Errorf("%w: lease %s expired at %s without a successful heartbeat",
			ErrLeaseLost, lease.ID, lease.ExpiresAt.Format(time.RFC3339))
	default:
		log.Warn("lease check failed", "error", err, "expires_at", lease.ExpiresAt)
		return nil
	}

	select {
	case <-hh.Done():
		// The keep-alive loop found the loss first.
	default:
		s.leaseLost(hh, log, cause)
	}
	return hh.Err()
}

// holds reports whether h is still one of the Scheduler's Allocations.
func (s *impl) holds(h *handle) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.allocations[h.id]
	return ok
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//# When the Scheduler aborts a Job under KF-203, the Scheduler
//# SHALL name the reason and the message of the claim's `HostReady`
//# condition in the Job's failure.

// leaseGone is the cause of a Lease that a heartbeat says no longer exists.
// It quotes the heartbeat's answer, which says why: with the claim backend
// that may be a claim that is Expired or gone, or a claim whose Host is not
// ready, with the reason and message of its HostReady condition (KF-204).
// The answer is quoted, not wrapped, so the Job's failure is ErrLeaseLost
// and nothing else the answer happens to wrap.
func leaseGone(leaseID string, answer error) error {
	return fmt.Errorf("%w: the pool manager no longer has lease %s: %s", ErrLeaseLost, leaseID, answer.Error())
}
