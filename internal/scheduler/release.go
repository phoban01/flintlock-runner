package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/03-scheduler.md#release
//# When a Job finishes, the Scheduler SHALL release its Lease with
//# the Pool Manager and SHALL NOT delete the MicroVM itself.

//= docs/requirements/03-scheduler.md#release
//# The Scheduler SHALL return the Slot of a released MicroVM as
//# soon as the release has been requested rather than when it completes.

// Release implements Allocator. The Slot and the Profile's concurrency unit
// are returned before the call returns, and the Lease is handed back to the
// Pool Manager in the background: the run loop's worker is free to ask GitLab
// for the next Job while the ReleaseVM call is still in flight. The MicroVM
// itself is not touched, because the Pool Manager deletes a released MicroVM
// and provisions its replacement (PL-044).
//
// Releasing a Handle whose Lease is already gone, because a heartbeat found
// it expired, makes no call, and releasing the same Handle twice is a no-op.
func (s *impl) Release(h Handle) {
	hh, ok := h.(*handle)
	if !ok || hh == nil {
		return
	}
	if !s.finish(hh) {
		s.stopHeartbeat(hh)
		return
	}
	s.stopHeartbeat(hh)
	if hh.leaseIsGone() {
		return
	}
	s.releaseInBackground(hh.lease())
}

//= docs/requirements/03-scheduler.md#release
//# Where the keep-on-failure debug option is enabled, the Scheduler
//# SHALL keep heartbeating the Lease of a failed Job for the configured keep
//# duration before releasing it, and SHALL log the MicroVM uid and Host name.

// Retain implements Allocator. The Slot is returned at once, as it is by
// Release, but the Lease is kept: the heartbeat loop keeps running for the
// configured keep duration so that the MicroVM stays alive for an operator to
// look at, and the Lease is released when the duration elapses. With the
// keep-on-failure option disabled Retain is exactly Release.
func (s *impl) Retain(h Handle) {
	hh, ok := h.(*handle)
	if !ok || hh == nil {
		return
	}
	if !s.set.Scheduler.KeepOnFailure {
		s.Release(h)
		return
	}
	if !s.finish(hh) {
		s.stopHeartbeat(hh)
		return
	}
	if hh.leaseIsGone() {
		s.stopHeartbeat(hh)
		return
	}

	alloc := hh.Allocation()
	keep := s.keepDuration()
	s.log.Warn("keeping the microvm of a failed job alive for inspection",
		"job", alloc.JobID,
		"profile", alloc.Profile,
		"vm", alloc.VMUID,
		"host", alloc.Placement.Host,
		"lease", alloc.Lease.ID,
		"keep_for", keep)

	if !s.inBackground(func() { s.retainThenRelease(hh, keep) }) {
		s.stopHeartbeat(hh)
		s.releaseLease(hh.lease())
	}
}

// retainThenRelease waits out the keep duration on the injected Clock and
// then releases the Lease. It ends early when the Lease is lost anyway or the
// Scheduler is shutting down.
func (s *impl) retainThenRelease(h *handle, keep time.Duration) {
	timer := s.clk.NewTimer(keep)
	defer timer.Stop()
	select {
	case <-timer.C():
	case <-h.Done():
	case <-s.bgDone():
	}
	s.stopHeartbeat(h)
	if h.leaseIsGone() {
		return
	}
	s.releaseLease(h.lease())
}

// keepDuration is how long a retained MicroVM is kept (SC-054).
func (s *impl) keepDuration() time.Duration {
	if d := s.set.Scheduler.KeepDuration; d > 0 {
		return d
	}
	return config.DefaultKeepDuration
}

// releaseInBackground releases a Lease without blocking the caller. When the
// Scheduler is past the point where new background work can be started the
// release is made inline instead, so no Lease is dropped silently.
func (s *impl) releaseInBackground(lease Lease) {
	if !s.inBackground(func() { s.releaseLease(lease) }) {
		s.releaseLease(lease)
	}
}

//= docs/requirements/03-scheduler.md#release
//# If a release fails, then the Scheduler SHALL retry it with
//# exponential backoff up to the configured retry limit and SHALL then log
//# the Lease id and rely on Lease expiry.

// releaseLease calls ReleaseVM and retries it. NOT_FOUND counts as released
// (PL-042); any other failure is counted, backed off on the injected Clock
// and tried again up to the configured limit, after which the Lease id is
// logged and nothing more is done: the Pool Manager expires a Lease that
// stops being heartbeated and deletes its MicroVM.
func (s *impl) releaseLease(lease Lease) {
	limit := s.releaseRetryLimit()
	for attempt := 0; ; attempt++ {
		ctx, cancel := s.callContext()
		err := s.deps.PoolManager.ReleaseVM(ctx, lease.ID)
		cancel()
		switch {
		case err == nil:
			s.log.Debug("lease released", "lease", lease.ID, "pool", lease.Pool.String())
			return
		case errors.Is(err, poolmgr.ErrNotFound):
			s.log.Debug("lease was already gone, treating the release as complete",
				"lease", lease.ID, "pool", lease.Pool.String())
			return
		}

		s.metrics.FailureCounted(FailureRelease)
		if attempt >= limit {
			s.log.Error("release failed after the retry limit, relying on lease expiry",
				"lease", lease.ID, "pool", lease.Pool.String(), "attempts", attempt+1, "error", err)
			return
		}
		s.log.Warn("release failed, retrying",
			"lease", lease.ID, "pool", lease.Pool.String(), "attempt", attempt+1, "error", err)

		timer := s.clk.NewTimer(s.backoff.Next(attempt))
		select {
		case <-timer.C():
		case <-s.bgDone():
			timer.Stop()
			s.log.Error("release abandoned during shutdown, relying on lease expiry",
				"lease", lease.ID, "pool", lease.Pool.String(), "error", err)
			return
		}
	}
}

// releaseRetryLimit is the configured bound on release retries (PL-043).
func (s *impl) releaseRetryLimit() int {
	if n := s.set.PoolManager.ReleaseRetryLimit; n > 0 {
		return n
	}
	return config.DefaultPoolManagerReleaseRetry
}

// callContext is the context of one Pool Manager call made outside a Job: it
// is not cancelled by shutdown, so the last release attempt completes, but it
// is bounded so that an unresponsive Pool Manager cannot hold a goroutine for
// ever.
func (s *impl) callContext() (context.Context, context.CancelFunc) {
	deadline := s.set.PoolManager.Deadline
	if deadline <= 0 {
		deadline = config.DefaultPoolManagerDeadline
	}
	return context.WithTimeout(context.WithoutCancel(s.background()), deadline)
}

// background is the context the heartbeat and release goroutines run under.
// It is context.Background before Run has built one.
func (s *impl) background() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bgCtx == nil {
		return context.Background()
	}
	return s.bgCtx
}

// bgDone is closed when the Scheduler has finished shutting down and no
// background work should wait any longer.
func (s *impl) bgDone() <-chan struct{} { return s.background().Done() }

// inBackground starts fn in a goroutine that Run waits for, and reports
// whether it did. It returns false once Run has stopped accepting background
// work, so that no goroutine is started after Run's wait began.
func (s *impl) inBackground(fn func()) bool {
	s.mu.Lock()
	if s.bgClosed {
		s.mu.Unlock()
		return false
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		fn()
	}()
	return true
}

// closeBackground stops new background work from being started; Run calls it
// before waiting for what is already running.
func (s *impl) closeBackground() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bgClosed = true
}

// shutdown is the second half of Run: it releases every unconverted
// Reservation (GL-070), lets running Jobs finish for up to the shutdown
// timeout (GL-071) and aborts whatever is left (GL-072).
func (s *impl) shutdown() {
	s.mu.Lock()
	held := len(s.reservations)
	s.reservations = make(map[uint64]time.Time)
	s.mu.Unlock()
	if held > 0 {
		s.log.Info("shutting down: released unconverted reservations", "reservations", held)
	}

	timer := s.clk.NewTimer(s.shutdownTimeout())
	defer timer.Stop()
	select {
	case <-s.drained():
	case <-timer.C():
		s.abortRunning()
	}
	s.closeBackground()
}

// shutdownTimeout is how long running Jobs are given (GL-071).
func (s *impl) shutdownTimeout() time.Duration {
	if d := s.set.ShutdownTimeout; d > 0 {
		return d
	}
	return config.DefaultShutdownTimeout
}

// abortRunning fails every Allocation that is still held when the shutdown
// timeout elapses and releases its Lease (GL-072).
func (s *impl) abortRunning() {
	for _, h := range s.liveHandles() {
		alloc := h.Allocation()
		s.log.Error("shutdown timeout elapsed, aborting the job and releasing its microvm",
			"job", alloc.JobID, "vm", alloc.VMUID, "host", alloc.Placement.Host, "lease", alloc.Lease.ID)
		h.fail(ErrShutdown)
		s.endAllocation(h)
	}
}

// liveHandles snapshots the Allocations currently held.
func (s *impl) liveHandles() []*handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*handle, 0, len(s.allocations))
	for _, h := range s.allocations {
		out = append(out, h)
	}
	return out
}

// endAllocation returns the Slot of an Allocation the Scheduler itself is
// ending and releases its Lease, unless the Lease is already gone.
func (s *impl) endAllocation(h *handle) {
	if !s.finish(h) {
		return
	}
	s.stopHeartbeat(h)
	if h.leaseIsGone() {
		return
	}
	s.releaseInBackground(h.lease())
}
