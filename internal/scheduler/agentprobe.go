package scheduler

import (
	"context"
	"fmt"
	"log/slog"
)

// startAgentProbe starts the Exec Agent probe of one Allocation when the
// Scheduler has an AgentProber, which only the claim backend's Runner has.
// The probe runs under the keep-alive loop's context, so it ends with that
// loop: when the Allocation is released or retained, or its Lease is lost.
func (s *impl) startAgentProbe(ctx context.Context, h *handle) {
	if s.deps.Agents == nil {
		return
	}
	s.inBackground(func() { s.agentProbeLoop(ctx, h) })
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//# Where the claim backend is configured, the Scheduler SHALL
//# probe the Exec Agent of each claim that a Job holds at the configured
//# Host health interval, by calling `GetMicroVM` for the claim's MicroVM
//# with a claim token of that claim, bounded by the configured Host call
//# deadline.

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//# The Scheduler SHALL abort a Job under KF-201 no later than the
//# Host unhealthy threshold times the sum of the Host health interval and
//# the Host call deadline after the claim's Exec Agent stops answering.

// agentProbeLoop probes the Exec Agent of one claim until the Allocation
// ends. The next probe is due one Host health interval after the last one
// returned, and each probe is bounded by the Host call deadline, so
// consecutive probes start at most one interval plus one deadline apart.
// An agent that stops answering therefore fails the threshold's worth of
// probes within the threshold times that sum, and the Job is aborted then
// (KF-202). A passing probe resets the count, so an agent that restarts
// between two probes costs the Job nothing.
//
// The loop stops probing once the Job no longer holds the claim: a claim
// retained for inspection (SC-054) is not the Job's any more, and nothing
// is left to abort.
func (s *impl) agentProbeLoop(ctx context.Context, h *handle) {
	alloc := h.Allocation()
	log := s.log.With("job", alloc.JobID, "vm", alloc.VMUID, "lease", alloc.Lease.ID,
		"host", alloc.Host.Name, "agent", alloc.Host.Address)
	interval := s.hostHealthInterval()
	threshold := s.hostUnhealthyThreshold()
	failures := 0

	timer := s.clk.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.Done():
			return
		case <-timer.C():
		}
		if !s.holds(h) {
			return
		}

		callCtx, cancel := context.WithTimeout(ctx, s.hostCallDeadline())
		err := s.deps.Agents.ProbeAgent(callCtx, alloc)
		cancel()

		// The Allocation can end while the probe is in flight, and its
		// context with it. A probe cut short that way says nothing about
		// the agent.
		if ctx.Err() != nil || !s.holds(h) {
			return
		}

		if err == nil {
			if failures > 0 {
				log.Info("exec agent answers again", "failures", failures)
			}
			failures = 0
			timer.Reset(interval)
			continue
		}
		failures++
		if failures >= threshold {
			s.agentUnreachable(h, log, failures, err)
			return
		}
		log.Warn("exec agent probe failed", "failures", failures, "threshold", threshold, "error", err)
		timer.Reset(interval)
	}
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//# If the Exec Agent of a claim that a Job holds fails as many
//# consecutive probes as the configured Host unhealthy threshold, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and delete the claim.

// agentUnreachable aborts the Job of a claim whose Exec Agent failed the
// threshold's worth of consecutive probes, as hostUnhealthy does for an
// Inventory Host (SC-043). The Handle is failed with ErrHostUnhealthy, which
// the Executor reports as runner_system_failure, and the Lease is released,
// which with the claim backend deletes the claim (KF-153).
//
// The probe's error is quoted in the Handle's error, not wrapped. A probe
// that ran out of time wraps context.DeadlineExceeded, and gitlab-runner
// reports any Job error that matches it as job_execution_timeout, whatever
// failure reason the error carries.
func (s *impl) agentUnreachable(h *handle, log *slog.Logger, failures int, probeErr error) {
	alloc := h.Allocation()
	log.Error("aborting a job whose exec agent stopped answering",
		"failures", failures, "error", probeErr)
	h.fail(fmt.Errorf("%w: the exec agent of claim %s on host %q did not answer for microvm %s in %d consecutive probes: %s",
		ErrHostUnhealthy, alloc.Lease.ID, alloc.Host.Name, alloc.VMUID, failures, probeErr.Error()))
	s.endAllocation(h)
}
