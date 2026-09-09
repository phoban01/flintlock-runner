package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// probeLoop probes every Host in the Inventory at the configured health
// interval until ctx is cancelled. The first round runs at once, so a Host
// that is down at startup is known to be down before the first Job is placed
// on it and its flintlock version is logged (HO-011).
func (s *impl) probeLoop(ctx context.Context) {
	interval := s.hostHealthInterval()
	s.probeAll(ctx)
	timer := s.clk.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		}
		s.probeAll(ctx)
		timer.Reset(interval)
	}
}

// hostHealthInterval is the configured probe interval (SC-040).
func (s *impl) hostHealthInterval() time.Duration {
	if d := s.set.Scheduler.HostHealthInterval; d > 0 {
		return d
	}
	return config.DefaultHostHealthInterval
}

// hostCallDeadline bounds one flintlock call (HO-003).
func (s *impl) hostCallDeadline() time.Duration {
	if d := s.set.Scheduler.HostCallDeadline; d > 0 {
		return d
	}
	return config.DefaultHostCallDeadline
}

// hostUnhealthyThreshold is how many consecutive failed probes make a Host
// unhealthy (SC-041).
func (s *impl) hostUnhealthyThreshold() int {
	if n := s.set.Scheduler.HostUnhealthyThreshold; n > 0 {
		return n
	}
	return config.DefaultHostUnhealthyThreshold
}

//= docs/requirements/03-scheduler.md#host-health
//# The Scheduler SHALL probe every Host in the Inventory at the
//# configured health interval by calling `ServerInfo`, falling back to
//# `ListMicroVMs` on the Runner's namespace when `ServerInfo` is not
//# implemented by the Host.

// probeAll probes every Host once, in parallel, and publishes the gauges. The
// names come from the health table, which resetHosts builds from the
// Inventory, and not from the Registry: a Host the Registry could not dial is
// exactly the one whose health matters, and taking the names from the
// Registry would leave it with the healthy state resetHosts seeded it with
// for ever. probeHost turns the missing Registry entry into a failed probe.
func (s *impl) probeAll(ctx context.Context) {
	names := s.inventoryHosts()
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.probeHost(ctx, name)
		}()
	}
	wg.Wait()
	s.publishGauges()
}

// inventoryHosts is the Host names of the Inventory, which is what resetHosts
// keys the health table by.
func (s *impl) inventoryHosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.hosts))
	for name := range s.hosts {
		out = append(out, name)
	}
	return out
}

//= docs/requirements/03-scheduler.md#host-health
//# The Scheduler SHALL probe every Host in the Inventory at the
//# configured health interval by calling `ServerInfo`, falling back to
//# `ListMicroVMs` on the Runner's namespace when `ServerInfo` is not
//# implemented by the Host.

// probeHost probes one Host, from probeLoop's round at the configured health
// interval. A Host that predates the ServerInfo RPC answers UNIMPLEMENTED, in
// which case ListMicroVMs on the Runner's own namespace is the probe instead
// and the Host's version stays unknown (HO-013); ListMicroVMs is called with
// no other namespace and for no other purpose (HO-008). An Inventory Host the
// Registry does not hold, because Apply could not dial it, has no client at
// all and counts as a failed probe like any other unreachable Host.
func (s *impl) probeHost(ctx context.Context, name string) {
	client, err := s.deps.Hosts.Get(name)
	if err != nil {
		s.recordProbe(name, nil, fmt.Errorf("host %q: %w", name, err))
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, s.hostCallDeadline())
	defer cancel()

	info, err := client.ServerInfo(callCtx)
	if errors.Is(err, flintlock.ErrUnimplemented) {
		if _, listErr := client.ListMicroVMs(callCtx, s.set.Namespace); listErr != nil {
			s.recordProbe(name, nil, fmt.Errorf("host %q: server info unimplemented and list failed: %w", name, listErr))
			return
		}
		s.recordProbe(name, &flintlock.HostInfo{Name: name}, nil)
		return
	}
	if err != nil {
		s.recordProbe(name, nil, fmt.Errorf("host %q: server info: %w", name, err))
		return
	}
	s.recordProbe(name, info, nil)
}

//= docs/requirements/03-scheduler.md#host-health
//# When a Host fails the configured number of consecutive probes,
//# the Scheduler SHALL mark the Host unhealthy.

//= docs/requirements/03-scheduler.md#host-health
//# When an unhealthy Host passes a probe, the Scheduler SHALL mark
//# the Host healthy.

// recordProbe folds the result of one probe into the Host's health. Failures
// are counted consecutively and reset by any success, so a single lost call
// does not take a Host out; the count reaching the configured threshold does,
// and the next successful probe puts it back.
func (s *impl) recordProbe(name string, info *flintlock.HostInfo, probeErr error) {
	threshold := s.hostUnhealthyThreshold()
	now := s.clk.Now()

	s.mu.Lock()
	st, ok := s.hosts[name]
	if !ok {
		st = &hostState{healthy: true}
		s.hosts[name] = st
	}
	st.lastProbeAt = now
	var becameHealthy, becameUnhealthy bool
	if probeErr == nil {
		st.failures = 0
		if info != nil {
			st.info = info
		}
		if !st.healthy {
			st.healthy = true
			becameHealthy = true
		}
	} else {
		st.failures++
		if st.healthy && st.failures >= threshold {
			st.healthy = false
			becameUnhealthy = true
		}
	}
	failures := st.failures
	s.mu.Unlock()

	switch {
	case becameHealthy:
		s.log.Info("host is healthy again", "host", name)
	case becameUnhealthy:
		s.log.Error("host marked unhealthy after consecutive failed probes",
			"host", name, "failures", failures, "threshold", threshold, "error", probeErr)
		s.hostUnhealthy(name)
	case probeErr != nil:
		s.log.Warn("host probe failed", "host", name, "failures", failures, "error", probeErr)
	case info != nil:
		s.logHostInfo(name, info)
	}
}

// logHostInfo logs a Host's flintlock version and guest services the first
// time they are seen (HO-011, HO-012).
func (s *impl) logHostInfo(name string, info *flintlock.HostInfo) {
	if s.throttled("hostinfo:" + name) {
		return
	}
	s.log.Info("host probed",
		"host", name,
		"version_known", info.VersionKnown,
		"version", info.Version,
		"exec_enabled", info.Exec.Enabled,
		"ssh_proxy_enabled", info.SSHProxy.Enabled)
	if info.VersionKnown && !info.Exec.Enabled {
		s.log.Warn("host reports the exec service disabled; jobs placed there cannot use the exec guest transport",
			"host", name)
	}
}

//= docs/requirements/03-scheduler.md#host-health
//# When a Host becomes unhealthy, the Scheduler SHALL abort every
//# Job whose MicroVM is placed on that Host with the failure reason
//# `runner_system_failure` and release their Leases.

// hostUnhealthy aborts every Job whose MicroVM is placed on a Host that has
// just been marked unhealthy. The Handle is failed with ErrHostUnhealthy,
// which the Executor turns into a `runner_system_failure` by cancelling the
// Build's context with that cause rather than letting the Job sit until its
// timeout, and each Lease is handed back to the Pool Manager.
func (s *impl) hostUnhealthy(name string) {
	for _, h := range s.liveHandles() {
		if h.placementHost() != name {
			continue
		}
		alloc := h.Allocation()
		s.log.Error("aborting a job whose host became unhealthy",
			"job", alloc.JobID, "host", name, "vm", alloc.VMUID, "lease", alloc.Lease.ID)
		h.fail(fmt.Errorf("%w: host %q", ErrHostUnhealthy, name))
		s.endAllocation(h)
	}
}

// publishGauges publishes the per-Pool and per-Host gauges (OB-016 to
// OB-019).
func (s *impl) publishGauges() {
	for _, pa := range s.deps.Tracker.Pools() {
		target := int32(0)
		if p := findProfileByPool(s.profileSnapshot(), pa.Pool); p != nil {
			target = int32(p.Pool.Size) //nolint:gosec // a pool size is small and non-negative
		}
		s.metrics.PoolGauges(pa.Pool, pa.Status, target)
	}
	for _, h := range s.Snapshot().Hosts {
		s.metrics.HostGauges(h.Name, h.Healthy, h.Leased)
	}
}
