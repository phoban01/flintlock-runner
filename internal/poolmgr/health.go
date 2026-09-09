package poolmgr

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// Defaults for the health monitor.
const (
	defaultHealthInterval         = 10 * time.Second
	defaultHealthFailureThreshold = 3
	defaultHealthBackoff          = 30 * time.Second
)

// HealthConfig is what NewHealth needs. Admin is required.
type HealthConfig struct {
	// Admin is the PoolAdmin service; ListPools is the probe (PL-005).
	Admin PoolAdmin
	// Namespace is the namespace ListPools is called with; empty probes
	// every namespace.
	Namespace string
	// Clock paces the probes and measures the unhealthy period.
	Clock clock.Clock
	// Backoff is the retry policy for the first contact (PL-004).
	Backoff clock.Backoff
	// Interval is the probe interval and FailureThreshold the number of
	// consecutive failures that make the Pool Manager unhealthy (PL-005).
	Interval         time.Duration
	FailureThreshold int
	// UnhealthyFor is how long a claim that failed with UNAVAILABLE keeps
	// the Pool Manager marked unhealthy (PL-034).
	UnhealthyFor time.Duration
	// Log receives the transitions.
	Log *slog.Logger
}

// HealthMonitor tracks whether the Pool Manager is reachable (PL-004,
// PL-005, PL-034). It is safe for concurrent use.
type HealthMonitor struct {
	cfg HealthConfig
	log *slog.Logger

	// contacted is closed by the first successful call, once.
	contacted     chan struct{}
	contactedOnce sync.Once

	mu sync.Mutex
	// failures counts consecutive probe failures.
	failures int
	// healthy is the state the probes last decided.
	healthy bool
	// unhealthyUntil is the end of the backoff period a failed claim
	// started (PL-034).
	unhealthyUntil time.Time
}

// NewHealth builds the health monitor.
func NewHealth(cfg HealthConfig) (*HealthMonitor, error) {
	if cfg.Admin == nil {
		return nil, errors.New("poolmgr: health needs a PoolAdmin client")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Backoff == nil {
		cfg.Backoff = clock.Exponential{Base: time.Second, Max: 30 * time.Second}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultHealthInterval
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = defaultHealthFailureThreshold
	}
	if cfg.UnhealthyFor <= 0 {
		cfg.UnhealthyFor = defaultHealthBackoff
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &HealthMonitor{
		cfg:       cfg,
		log:       cfg.Log.With("component", "poolmgr-health"),
		contacted: make(chan struct{}),
	}, nil
}

//= docs/requirements/04-pool-manager.md#client
//# If the Pool Manager is unreachable at startup, then the Scheduler SHALL
//# keep retrying the connection with exponential backoff and the Runner
//# SHALL NOT report itself ready until it succeeds.

//= docs/requirements/04-pool-manager.md#client
//# The Scheduler SHALL probe the Pool Manager at the configured health
//# interval by calling `ListPools` and SHALL mark it unhealthy after the
//# configured number of consecutive failures and healthy again after a
//# successful probe.

// Run probes the Pool Manager until ctx is cancelled. Until the first call
// succeeds it retries with exponential backoff and leaves Contacted open,
// which is what holds the Runner's readiness endpoint closed (OB-031): a
// Runner that has never reached the Pool Manager cannot start a Job and
// says so rather than taking one from GitLab. From the first success on it
// probes with ListPools at the configured interval, counts consecutive
// failures and marks the Pool Manager unhealthy once there have been as
// many as the configured threshold, and healthy again on the next probe
// that succeeds.
func (h *HealthMonitor) Run(ctx context.Context) error {
	if err := h.firstContact(ctx); err != nil {
		return nil
	}
	timer := h.cfg.Clock.NewTimer(h.cfg.Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C():
		}
		h.probe(ctx)
		timer.Reset(h.cfg.Interval)
	}
}

// firstContact retries ListPools with exponential backoff until it
// succeeds. It returns the context's error when ctx ends first.
func (h *HealthMonitor) firstContact(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		if err := h.call(ctx); err == nil {
			h.markHealthy()
			h.log.Info("pool manager contacted")
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else {
			delay := h.cfg.Backoff.Next(attempt)
			h.log.Warn("pool manager unreachable; retrying",
				"attempt", attempt+1, "retry_in", delay, "error", err)
			if werr := h.sleep(ctx, delay); werr != nil {
				return werr
			}
		}
	}
}

// probe runs one health probe and folds the result into the state.
func (h *HealthMonitor) probe(ctx context.Context) {
	if err := h.call(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		h.markFailure(err)
		return
	}
	h.markHealthy()
}

// call is the probe itself: ListPools in the configured namespace.
func (h *HealthMonitor) call(ctx context.Context) error {
	_, err := h.cfg.Admin.ListPools(ctx, h.cfg.Namespace)
	return err
}

// markFailure counts one failed probe and marks the Pool Manager unhealthy
// once the threshold is reached.
func (h *HealthMonitor) markFailure(err error) {
	h.mu.Lock()
	h.failures++
	failures := h.failures
	crossed := failures == h.cfg.FailureThreshold
	if failures >= h.cfg.FailureThreshold {
		h.healthy = false
	}
	h.mu.Unlock()
	if crossed {
		h.log.Error("pool manager unhealthy", "consecutive_failures", failures, "error", err)
		return
	}
	h.log.Warn("pool manager probe failed", "consecutive_failures", failures, "error", err)
}

// markHealthy records a successful call: the failure count is cleared, the
// Pool Manager is healthy again and Contacted is closed if this was the
// first success.
func (h *HealthMonitor) markHealthy() {
	h.mu.Lock()
	recovered := !h.healthy
	h.failures = 0
	h.healthy = true
	h.unhealthyUntil = time.Time{}
	h.mu.Unlock()
	h.contactedOnce.Do(func() { close(h.contacted) })
	if recovered {
		h.log.Info("pool manager healthy")
	}
}

// Contacted implements Health. The channel is closed once, by the first
// call that succeeds, and stays closed: the Runner is allowed to start
// taking Jobs from then on (SC-008).
func (h *HealthMonitor) Contacted() <-chan struct{} { return h.contacted }

//= docs/requirements/04-pool-manager.md#claiming
//# If `ClaimVM` fails with `UNAVAILABLE` or a connection error, then the
//# Scheduler SHALL mark the Pool Manager unhealthy for the configured
//# backoff period.

// MarkUnavailable implements Health. A claim that could not reach the Pool
// Manager marks it unhealthy for the configured backoff period, without
// waiting for the probes to notice: every Pool then counts as empty
// (PL-035), so the Runner stops accepting Jobs it could not start. The
// period ends by itself, and any probe or claim that succeeds in the
// meantime ends it early.
func (h *HealthMonitor) MarkUnavailable() {
	h.mu.Lock()
	h.unhealthyUntil = h.cfg.Clock.Now().Add(h.cfg.UnhealthyFor)
	until := h.unhealthyUntil
	h.mu.Unlock()
	h.log.Warn("pool manager marked unhealthy", "until", until, "backoff", h.cfg.UnhealthyFor)
}

// Healthy implements Health: false before the first contact, during the
// backoff period a failed claim started, and from the moment the configured
// number of consecutive probes has failed until one succeeds.
func (h *HealthMonitor) Healthy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.healthy {
		return false
	}
	return !h.cfg.Clock.Now().Before(h.unhealthyUntil)
}

// sleep waits for d on the clock, or returns the context's error.
func (h *HealthMonitor) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := h.cfg.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

// Compile-time interface check.
var _ Health = (*HealthMonitor)(nil)
