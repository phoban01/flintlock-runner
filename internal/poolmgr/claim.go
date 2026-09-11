package poolmgr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Redeclarer declares a Pool again from the Profile it came from. It is the
// seam PL-033 needs: a claim that finds the Pool gone asks for it to be
// declared again. *Declaration implements it.
type Redeclarer interface {
	Redeclare(ctx context.Context, ref PoolRef) error
}

// ClaimerConfig is what NewClaimer needs. Lease is required; a nil Tracker,
// Health or Redeclarer simply skips that part of the policy.
type ClaimerConfig struct {
	// Lease is the Pool Manager's Lease service.
	Lease Lease
	// Tracker learns that a Pool answered RESOURCE_EXHAUSTED (PL-053).
	Tracker Tracker
	// Health learns that the Pool Manager could not be reached (PL-034).
	Health Health
	// Redeclarer re-declares a Pool the Pool Manager does not know (PL-033).
	Redeclarer Redeclarer
	// Log receives the exhaustion and re-declaration notices.
	Log *slog.Logger
}

// Claimer performs one claim attempt and applies the policy the outcome
// calls for (PL-030 to PL-034). It holds no state of its own; the retry
// loop, the backoff and the wait on the Tracker belong to the Scheduler
// (SC-020, SC-021). It is safe for concurrent use.
type Claimer struct {
	cfg ClaimerConfig
	log *slog.Logger
}

// NewClaimer builds a Claimer.
func NewClaimer(cfg ClaimerConfig) (*Claimer, error) {
	if cfg.Lease == nil {
		return nil, errors.New("poolmgr: claimer needs a Lease")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Claimer{cfg: cfg, log: log.With("component", "poolmgr-claimer")}, nil
}

//= docs/requirements/04-pool-manager.md#claiming
//# When a warm MicroVM is needed for a Profile, the Scheduler SHALL call
//# `ClaimVM` with the `PoolRef` of that Profile's Pool.

//= docs/requirements/04-pool-manager.md#claiming
//# If `ClaimVM` returns `RESOURCE_EXHAUSTED`, then the Scheduler SHALL
//# treat the Pool as having no warm MicroVM available.

//= docs/requirements/04-pool-manager.md#claiming
//# If `ClaimVM` fails with `UNAVAILABLE` or a connection error, then the
//# Scheduler SHALL mark the Pool Manager unhealthy for the configured
//# backoff period.

// Claim asks the Pool Manager for a warm MicroVM from one Profile's Pool
// and turns the answer into the state the rest of the Scheduler reads:
//
//   - a granted Lease is returned as it came off the wire (PL-031);
//   - RESOURCE_EXHAUSTED sets the Pool's available count to zero until an
//     event or a poll says otherwise, and comes back as ErrExhausted
//     (PL-032, PL-053);
//   - NOT_FOUND re-declares the Pool from its Profile and is reported as an
//     exhausted Pool as well, because re-declaring one does not make a warm
//     MicroVM appear (PL-033);
//   - UNAVAILABLE, or a connection error, marks the Pool Manager unhealthy
//     for the configured backoff period, which zeroes the capacity of every
//     Pool until it recovers (PL-034, PL-035).
func (c *Claimer) Claim(ctx context.Context, ref PoolRef) (*Claim, error) {
	claim, err := c.cfg.Lease.ClaimVM(ctx, ref)
	switch {
	case err == nil:
		return claim, nil

	case errors.Is(err, ErrExhausted):
		c.markExhausted(ref)
		return nil, err

	case errors.Is(err, ErrNotFound):
		c.markExhausted(ref)
		c.redeclare(ctx, ref)
		return nil, fmt.Errorf("%w: pool %s is not known to the pool manager: %w", ErrExhausted, ref, err)

	case errors.Is(err, ErrUnavailable):
		if c.cfg.Health != nil {
			c.cfg.Health.MarkUnavailable()
		}
		c.log.Warn("pool manager unavailable on claim; marking it unhealthy", "pool", ref.String(), "error", err)
		return nil, err
	}
	return nil, err
}

// markExhausted zeroes the Pool's available count.
func (c *Claimer) markExhausted(ref PoolRef) {
	if c.cfg.Tracker != nil {
		c.cfg.Tracker.MarkExhausted(ref)
	}
}

// redeclare asks for the Pool to be declared again. A failure is logged and
// swallowed: the claim has already failed, and Declaration retries the
// declaration at the configured interval (PL-016).
func (c *Claimer) redeclare(ctx context.Context, ref PoolRef) {
	if c.cfg.Redeclarer == nil {
		c.log.Warn("pool is not known to the pool manager and nothing can re-declare it", "pool", ref.String())
		return
	}
	if err := c.cfg.Redeclarer.Redeclare(ctx, ref); err != nil {
		c.log.Error("pool is not known to the pool manager and re-declaring it failed",
			"pool", ref.String(), "error", err)
		return
	}
	c.log.Warn("pool was not known to the pool manager and has been re-declared", "pool", ref.String())
}
