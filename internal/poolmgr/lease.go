package poolmgr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// Defaults for the lease units.
const (
	defaultHeartbeatInterval = 10 * time.Second
	defaultReleaseRetryLimit = 5
)

// LeaseKeeperConfig is what NewLeaseKeeper needs.
type LeaseKeeperConfig struct {
	// Lease is the Pool Manager's Lease service.
	Lease Lease
	// LeaseID is the lease to keep alive.
	LeaseID string
	// Interval is the Pool's configured heartbeat interval. A heartbeat is
	// sent at most this far apart, and sooner when the expiry the Pool
	// Manager last returned is closer than twice the interval.
	Interval time.Duration
	// ExpiresAt is the expiry known at the claim, if any. Zero means the
	// first heartbeat establishes it.
	ExpiresAt time.Time
	// Clock paces the loop.
	Clock clock.Clock
	// Log receives heartbeat failures.
	Log *slog.Logger
}

// ErrLeaseLost is what LeaseKeeper.Run returns when the Lease is gone: the
// Pool Manager answered NOT_FOUND, or heartbeats failed for long enough
// that the Lease expired. The Scheduler aborts the Job on it (SC-061,
// SC-062) and does not call ReleaseVM, because there is nothing to release.
var ErrLeaseLost = errors.New("poolmgr: lease lost")

// LeaseKeeper keeps one Lease alive for as long as an Allocation holds it
// (PL-040). The Scheduler starts one per Allocation and stops it by
// cancelling the context when the Job ends.
type LeaseKeeper struct {
	cfg LeaseKeeperConfig
	log *slog.Logger

	mu        sync.Mutex
	expiresAt time.Time
	beats     int
}

// NewLeaseKeeper builds a LeaseKeeper for one Lease.
func NewLeaseKeeper(cfg LeaseKeeperConfig) (*LeaseKeeper, error) {
	switch {
	case cfg.Lease == nil:
		return nil, errors.New("poolmgr: lease keeper needs a Lease")
	case cfg.LeaseID == "":
		return nil, errors.New("poolmgr: lease keeper needs a lease id")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultHeartbeatInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &LeaseKeeper{
		cfg:       cfg,
		log:       cfg.Log.With("component", "poolmgr-lease", "lease_id", cfg.LeaseID),
		expiresAt: cfg.ExpiresAt,
	}, nil
}

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//# While an Allocation holds a Lease, the Scheduler SHALL call `Heartbeat`
//# with the `lease_id` and SHALL update the Lease expiry from the returned
//# `expires_at`.

// Run heartbeats the Lease until ctx is cancelled, sending each Heartbeat
// with the lease id and replacing the Lease expiry with the `expires_at`
// the Pool Manager answers with. The next heartbeat is scheduled from that
// expiry, at half the time remaining or at the configured interval,
// whichever is sooner, so that a Pool with a short expiry threshold is
// heartbeated often enough and one with a long threshold is not
// heartbeated needlessly.
//
// Run returns nil when ctx ends, which is the normal end of a Job, and
// ErrLeaseLost when the Lease no longer exists: either because a Heartbeat
// answered NOT_FOUND, or because heartbeats kept failing until the last
// known expiry passed. The Scheduler turns that into an aborted Job
// (SC-061, SC-062).
func (k *LeaseKeeper) Run(ctx context.Context) error {
	timer := k.cfg.Clock.NewTimer(k.nextInterval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C():
		}

		expires, err := k.cfg.Lease.Heartbeat(ctx, k.cfg.LeaseID)
		switch {
		case err == nil:
			k.record(expires)

		case ctx.Err() != nil:
			return nil

		case errors.Is(err, ErrNotFound):
			k.log.Warn("lease no longer exists", "error", err)
			return fmt.Errorf("%w: %s: %w", ErrLeaseLost, k.cfg.LeaseID, err)

		default:
			if expiry := k.ExpiresAt(); !expiry.IsZero() && !k.cfg.Clock.Now().Before(expiry) {
				k.log.Error("heartbeats failed until the lease expired", "expires_at", expiry, "error", err)
				return fmt.Errorf("%w: %s expired at %s: %w", ErrLeaseLost, k.cfg.LeaseID, expiry, err)
			}
			k.log.Warn("heartbeat failed; retrying", "error", err)
		}
		timer.Reset(k.nextInterval())
	}
}

// ExpiresAt is the Lease expiry last returned by the Pool Manager, zero
// until the first heartbeat when the claim did not carry one.
func (k *LeaseKeeper) ExpiresAt() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.expiresAt
}

// Heartbeats is how many heartbeats have been answered.
func (k *LeaseKeeper) Heartbeats() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.beats
}

// record stores the expiry from a successful heartbeat.
func (k *LeaseKeeper) record(expires time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !expires.IsZero() {
		k.expiresAt = expires
	}
	k.beats++
}

// nextInterval is how long to wait before the next heartbeat: the
// configured interval, or half the time left until the known expiry when
// that is sooner. It never returns a non-positive duration, so a Lease
// already at its expiry is heartbeated at once rather than spun on.
func (k *LeaseKeeper) nextInterval() time.Duration {
	d := k.cfg.Interval
	if expiry := k.ExpiresAt(); !expiry.IsZero() {
		if half := expiry.Sub(k.cfg.Clock.Now()) / 2; half < d {
			d = half
		}
	}
	if d <= 0 {
		d = time.Nanosecond
	}
	return d
}

// ReleaserConfig is what NewReleaser needs.
type ReleaserConfig struct {
	// Lease is the Pool Manager's Lease service.
	Lease Lease
	// Clock and Backoff pace the retries.
	Clock   clock.Clock
	Backoff clock.Backoff
	// RetryLimit is how many further attempts are made after the first one
	// fails (PL-043). Zero takes defaultReleaseRetryLimit.
	RetryLimit int
	// Log receives the failure that ends the retries.
	Log *slog.Logger
}

// Releaser releases a Lease and retries a failure (PL-041 to PL-043). It is
// safe for concurrent use.
type Releaser struct {
	cfg ReleaserConfig
	log *slog.Logger
}

// NewReleaser builds a Releaser.
func NewReleaser(cfg ReleaserConfig) (*Releaser, error) {
	if cfg.Lease == nil {
		return nil, errors.New("poolmgr: releaser needs a Lease")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Backoff == nil {
		cfg.Backoff = clock.Exponential{Base: time.Second, Max: 30 * time.Second}
	}
	if cfg.RetryLimit <= 0 {
		cfg.RetryLimit = defaultReleaseRetryLimit
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Releaser{cfg: cfg, log: cfg.Log.With("component", "poolmgr-releaser")}, nil
}

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//# When a Job holding a Lease finishes, the Scheduler SHALL call
//# `ReleaseVM` with the `lease_id`.

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//# If `ReleaseVM` returns `NOT_FOUND`, then the Scheduler SHALL treat the
//# release as complete.

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//# If `ReleaseVM` fails for any other reason, then the Scheduler SHALL
//# retry with exponential backoff up to the configured retry limit and
//# SHALL then log the Lease id and rely on Lease expiry.

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//# The Scheduler SHALL NOT call `DeleteMicroVM` on any Host.

// Release gives the Lease back to the Pool Manager, which deletes the
// MicroVM and provisions a replacement; the Runner never deletes a MicroVM
// itself, and this package holds no Host client with which it could
// (PL-044, HO-007). A Lease the Pool Manager no longer has is already
// released, so NOT_FOUND is success (PL-042). Any other failure is retried
// with exponential backoff up to the configured limit, after which the
// Lease id is logged and the error returned: the Lease expiry sweeper
// deletes the MicroVM within the Pool's expiry threshold anyway, so the
// Runner gives up rather than holding a Slot forever (PL-043).
func (r *Releaser) Release(ctx context.Context, leaseID string) error {
	var err error
	for attempt := 0; attempt <= r.cfg.RetryLimit; attempt++ {
		if attempt > 0 {
			if werr := r.wait(ctx, r.cfg.Backoff.Next(attempt-1)); werr != nil {
				return werr
			}
		}
		err = r.cfg.Lease.ReleaseVM(ctx, leaseID)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrNotFound):
			return nil
		case ctx.Err() != nil:
			return fmt.Errorf("releasing lease %s: %w", leaseID, err)
		}
		r.log.Warn("release failed; retrying", "lease_id", leaseID, "attempt", attempt+1, "error", err)
	}
	r.log.Error("release failed; relying on lease expiry to delete the microvm",
		"lease_id", leaseID, "attempts", r.cfg.RetryLimit+1, "error", err)
	return fmt.Errorf("releasing lease %s after %d attempts: %w", leaseID, r.cfg.RetryLimit+1, err)
}

// wait sleeps on the clock for d, or returns the context's error.
func (r *Releaser) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := r.cfg.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}
