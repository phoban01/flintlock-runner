package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"log/slog"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// Allocate implements Allocator. It holds the Reservation throughout: on
// every error path the Slot is still the caller's to release, and on success
// the Reservation has become the Allocation without the Slot being freed in
// between (SC-026).
func (s *impl) Allocate(ctx context.Context, r *Reservation, job JobInfo, p *Profile) (Handle, error) {
	if r == nil {
		return nil, fmt.Errorf("scheduler: allocate: %w", errors.New("a reservation is required"))
	}
	if p == nil {
		return nil, &ProfileError{Image: job.Image}
	}
	switch s.runningState() {
	case stateNew, stateStopped:
		return nil, ErrNotRunning
	case stateStopping:
		return nil, &AllocationError{Profile: p.Name, Pool: p.PoolRef, Err: ErrShutdown}
	case stateRunning:
	}

	log := s.log.With("job", job.ID, "profile", p.Name, "pool", p.PoolRef.String())
	start := s.clk.Now()
	deadline := s.clk.NewTimer(s.allocationTimeout())
	defer deadline.Stop()

	if err := s.awaitProfileLimit(ctx, p, deadline); err != nil {
		return nil, err
	}
	h, err := s.claim(ctx, r, job, p, start, deadline, log)
	if err != nil {
		s.leaveProfile(p)
		return nil, err
	}
	return h, nil
}

// allocationTimeout is the configured bound on one allocation (SC-021).
func (s *impl) allocationTimeout() time.Duration {
	if d := s.set.Scheduler.AllocationTimeout; d > 0 {
		return d
	}
	return config.DefaultAllocationTimeout
}

// awaitProfileLimit takes one of the Profile's concurrency units, waiting for
// one to be returned while the limit is reached (SC-006). It gives up when
// the Job's context is cancelled or the allocation timeout elapses, so a
// Profile whose limit stays reached fails with ErrProfileLimit rather than
// blocking a worker for ever.
func (s *impl) awaitProfileLimit(ctx context.Context, p *Profile, deadline clock.Timer) error {
	for {
		entered, free := s.enterProfile(p)
		if entered {
			return nil
		}
		select {
		case <-free:
		case <-ctx.Done():
			return &AllocationError{Profile: p.Name, Pool: p.PoolRef, Err: ctx.Err()}
		case <-deadline.C():
			return &AllocationError{Profile: p.Name, Pool: p.PoolRef, Err: ErrProfileLimit}
		}
	}
}

//= docs/requirements/03-scheduler.md#allocation
//# When an Allocation is requested for a Profile, the Scheduler
//# SHALL claim a warm MicroVM from the Profile's Pool.

// claim claims a warm MicroVM from the Profile's Pool, retrying while the
// Pool is exhausted or the Pool Manager is unavailable, and turns the
// successful claim into an Allocation.
func (s *impl) claim(
	ctx context.Context,
	r *Reservation,
	job JobInfo,
	p *Profile,
	start time.Time,
	deadline clock.Timer,
	log *slog.Logger,
) (Handle, error) {
	attempt := 0
	waiting := false
	defer func() {
		if waiting {
			s.waitOnPool(p.PoolRef, -1)
		}
	}()

	for {
		// The wake-up channel is taken before the attempt, so an event that
		// arrives while the claim is in flight still wakes the wait below.
		wake := s.deps.Tracker.Wait(p.PoolRef)

		claim, err := s.deps.PoolManager.ClaimVM(ctx, p.PoolRef)
		if err == nil {
			if waiting {
				s.metrics.PoolWaited(p.PoolRef, s.clk.Now().Sub(start))
			}
			return s.allocated(ctx, r, job, p, claim, start, log)
		}

		switch {
		case errors.Is(err, poolmgr.ErrExhausted):
			s.deps.Tracker.MarkExhausted(p.PoolRef)
			if !waiting {
				waiting = true
				n := s.waitOnPool(p.PoolRef, 1)
				if !s.throttled("exhausted:" + p.PoolRef.String()) {
					log.Warn("pool is exhausted, waiting for a warm microvm",
						"waiting", n)
				}
			}
		case errors.Is(err, poolmgr.ErrNotFound):
			//= docs/requirements/04-pool-manager.md#claiming
			//# If `ClaimVM` returns `NOT_FOUND`, then the Scheduler
			//# SHALL re-declare the Pool from its Profile and treat the Pool as having
			//# no warm MicroVM available.
			log.Warn("pool is unknown to the pool manager, re-declaring it", "error", err)
			s.deps.Tracker.MarkExhausted(p.PoolRef)
			s.declare(ctx, p, s.inventorySnapshot())
		case errors.Is(err, poolmgr.ErrUnavailable):
			//= docs/requirements/04-pool-manager.md#claiming
			//# If `ClaimVM` fails with `UNAVAILABLE` or a connection error,
			//# then the Scheduler SHALL mark the Pool Manager unhealthy for the configured
			//# backoff period.
			s.deps.Health.MarkUnavailable()
			log.Warn("pool manager is unavailable, retrying the claim", "error", err)
		case ctx.Err() != nil:
			return nil, &AllocationError{Profile: p.Name, Pool: p.PoolRef, Err: ctx.Err()}
		default:
			return nil, &AllocationError{Profile: p.Name, Pool: p.PoolRef, Err: err}
		}

		if err := s.waitToRetry(ctx, p, attempt, wake, deadline); err != nil {
			return nil, err
		}
		attempt++
	}
}

//= docs/requirements/03-scheduler.md#allocation
//# If the Pool has no warm MicroVM available, then the Scheduler
//# SHALL retry the claim with exponential backoff, waking early on a Pool
//# Manager event that reports a MicroVM in that Pool becoming available,
//# until the allocation timeout elapses.

//= docs/requirements/03-scheduler.md#allocation
//# If the allocation timeout elapses before a MicroVM is claimed,
//# then the Scheduler SHALL return an allocation error naming the Profile and
//# the Pool.

// waitToRetry waits out the backoff of one failed claim. It returns nil when
// the caller should try again, which is when the backoff elapsed or the
// Tracker reported a MicroVM in the Pool becoming available, and an
// *AllocationError when the allocation timeout elapsed or the Job's context
// was cancelled. Every wait runs on the injected Clock.
func (s *impl) waitToRetry(
	ctx context.Context,
	p *Profile,
	attempt int,
	wake <-chan struct{},
	deadline clock.Timer,
) error {
	backoff := s.clk.NewTimer(s.backoff.Next(attempt))
	defer backoff.Stop()
	select {
	case <-backoff.C():
		return nil
	case <-wake:
		return nil
	case <-deadline.C():
		return &AllocationError{Profile: p.Name, Pool: p.PoolRef, Err: ErrAllocationTimeout}
	case <-ctx.Done():
		return &AllocationError{Profile: p.Name, Pool: p.PoolRef, Err: ctx.Err()}
	}
}

//= docs/requirements/03-scheduler.md#allocation
//# When the Job's context is cancelled during allocation, the
//# Scheduler SHALL stop the allocation and release any Lease that was
//# obtained.

//= docs/requirements/03-scheduler.md#placement
//# If Placement cannot be resolved for a MicroVM, then the Scheduler
//# SHALL release its Lease and report an allocation error.

//= docs/requirements/03-scheduler.md#placement
//# If the Host named in a Placement is not in the Inventory, then
//# the Scheduler SHALL release the Lease and report an allocation error
//# naming the Host.

// allocated turns a successful claim into an Allocation. Between the claim
// and the Allocation the MicroVM is leased but not yet the Job's, so every
// exit from here other than success hands the Lease straight back: a Job
// context that was cancelled while the claim was in flight, a Placement that
// no Host owns and a Placement naming a Host the Inventory does not have all
// release the Lease before returning the error.
func (s *impl) allocated(
	ctx context.Context,
	r *Reservation,
	job JobInfo,
	p *Profile,
	claim *poolmgr.Claim,
	start time.Time,
	log *slog.Logger,
) (Handle, error) {
	claimedAt := s.clk.Now()
	lease := Lease{ID: claim.LeaseID, Pool: p.PoolRef, ExpiresAt: claimedAt.Add(leaseExpiry(p))}

	if err := ctx.Err(); err != nil {
		s.abandon(lease, "the job context was cancelled during allocation")
		return nil, &AllocationError{Profile: p.Name, Pool: p.PoolRef, Host: claim.Host.Name, Err: err}
	}

	placement, err := s.ResolvePlacement(ctx, p, claim)
	if err != nil {
		s.abandon(lease, "the placement of the leased microvm could not be resolved")
		return nil, err
	}

	//= docs/requirements/03-scheduler.md#allocation
	//# The Scheduler SHALL record for every Allocation the Job id, the
	//# Profile name, the MicroVM uid, the Lease id and the Placement.
	alloc := Allocation{
		JobID:             job.ID,
		Profile:           p.Name,
		VMUID:             claim.VMUID,
		Lease:             lease,
		Placement:         placement,
		Host:              claim.Host,
		NetworkInterfaces: claim.NetworkInterfaces,
		ReservationID:     r.ID,
		ClaimedAt:         claimedAt,
	}

	s.mu.Lock()
	s.nextID++
	h := newHandle(s.nextID, alloc)
	s.mu.Unlock()

	if !s.convert(r, h) {
		s.abandon(lease, "the reservation was released while the allocation was in flight")
		return nil, &AllocationError{Profile: p.Name, Pool: p.PoolRef, Host: placement.Host, Err: ErrNotRunning}
	}

	s.startHeartbeat(h)
	s.logAllocation(log, alloc, start)
	s.metrics.AllocationObserved(p.Name, placement.ResolvedAt.Sub(start))
	return h, nil
}

//= docs/requirements/03-scheduler.md#allocation
//# The Scheduler SHALL log every allocation with the Job id,
//# Profile, Host name, MicroVM uid and elapsed time from claim to Placement.

// logAllocation writes the one line an operator correlates an Allocation by.
// The elapsed time is from the claim to the Placement being known; the time
// from the first claim attempt, which includes any wait on an exhausted Pool,
// is the OB-013 histogram.
func (s *impl) logAllocation(log *slog.Logger, alloc Allocation, start time.Time) {
	log.Info("microvm allocated",
		"job", alloc.JobID,
		"profile", alloc.Profile,
		"host", alloc.Placement.Host,
		"vm", alloc.VMUID,
		"lease", alloc.Lease.ID,
		"placement_source", string(alloc.Placement.Source),
		"elapsed", alloc.Placement.ResolvedAt.Sub(alloc.ClaimedAt),
		"elapsed_total", alloc.Placement.ResolvedAt.Sub(start),
	)
}

// leaseExpiry is how long a Lease is good for without a heartbeat, from the
// Profile's Pool settings; ClaimVMResponse carries no expiry, so the first
// one is derived from the Pool's heartbeat expiry threshold.
func leaseExpiry(p *Profile) time.Duration {
	if d := p.Pool.HeartbeatExpiry; d > 0 {
		return d
	}
	return config.DefaultHeartbeatExpiry
}

// abandon hands a Lease back that never became an Allocation.
func (s *impl) abandon(lease Lease, why string) {
	s.log.Warn("releasing a leased microvm that did not become an allocation",
		"lease", lease.ID, "pool", lease.Pool.String(), "reason", why)
	s.releaseLease(lease)
}
