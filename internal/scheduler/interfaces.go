package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// Profile is a resolved Profile: the configured Profile plus the PoolRef it
// declares (CF-028), so that Allocate has its Pool without a lookup (PL-030).
type Profile struct {
	config.Profile
	PoolRef poolmgr.PoolRef
}

// Sentinel errors. Typed errors below wrap or match them so errors.Is works.
var (
	// ErrRefused matches every *Refusal (SC-004).
	ErrRefused = errors.New("scheduler: reservation refused")
	// ErrNoProfile matches every *ProfileError (SC-011, SC-012); the
	// Executor fails the Job with runner_unsupported (EX-011).
	ErrNoProfile = errors.New("scheduler: no profile for job")
	// ErrAllocationTimeout is the cause of an AllocationError when the
	// allocation timeout elapses before a MicroVM is claimed (SC-022).
	ErrAllocationTimeout = errors.New("scheduler: allocation timed out")
	// ErrPlacementUnresolved is the cause when no Host returned the MicroVM
	// (SC-033). The Lease has been released.
	ErrPlacementUnresolved = errors.New("scheduler: placement could not be resolved")
	// ErrHostNotInInventory is the cause when the Placement names a Host
	// missing from the Inventory (SC-034). The Lease has been released.
	ErrHostNotInInventory = errors.New("scheduler: placed on host not in inventory")
	// ErrProfileLimit is the cause when the Profile's maximum concurrency
	// (SC-006) stays reached until the allocation timeout.
	ErrProfileLimit = errors.New("scheduler: profile concurrency limit reached")
	// ErrNotRunning is returned by Reserve and Allocate before Run has
	// started or after it has returned.
	ErrNotRunning = errors.New("scheduler: not running")

	// ErrHostUnhealthy is Handle.Err when the Host of the MicroVM was marked
	// unhealthy (SC-043).
	ErrHostUnhealthy = errors.New("scheduler: host of microvm became unhealthy")
	// ErrLeaseLost is Handle.Err when a heartbeat reported the Lease gone
	// (SC-061) or heartbeats failed for longer than the expiry (SC-062).
	ErrLeaseLost = errors.New("scheduler: lease lost")
	// ErrShutdown is Handle.Err when the shutdown timeout elapsed (GL-072).
	ErrShutdown = errors.New("scheduler: shutdown timeout elapsed")
)

// RefusalReason says why a Reservation was refused (SC-004). It is the label
// of the refusal counter (OB-015) and the key of the once-per-minute log
// (OB-003).
type RefusalReason string

// Refusal reasons.
const (
	// RefusalNoFreeSlot: every Slot is in use (SC-001, SC-002).
	RefusalNoFreeSlot RefusalReason = "no_free_slot"
	// RefusalNoWarmMicroVM: no Pool has an available warm MicroVM (SC-002),
	// including while the Pool Manager is unhealthy (PL-035).
	RefusalNoWarmMicroVM RefusalReason = "no_warm_microvm"
	// RefusalPoolManagerNotContacted: the Pool Manager has not answered yet
	// since start (SC-008, PL-004).
	RefusalPoolManagerNotContacted RefusalReason = "pool_manager_not_contacted"
	// RefusalShuttingDown: Run's context is cancelled (GL-070).
	RefusalShuttingDown RefusalReason = "shutting_down"
)

// Refusal is the error Reserve returns when it refuses (SC-004). The Executor
// maps it onto gitlab-runner's NoFreeExecutorError (EX-004) and reads Reason
// for the refusal counter (OB-015) without string parsing.
type Refusal struct {
	Reason RefusalReason
}

// Error implements error.
func (r *Refusal) Error() string { return ErrRefused.Error() + ": " + string(r.Reason) }

// Is reports whether target is ErrRefused, so errors.Is(err, ErrRefused)
// matches any Refusal.
func (r *Refusal) Is(target error) bool { return target == ErrRefused }

// ProfileError is the error ResolveProfile returns when no Profile matches
// (SC-011, SC-012). Image is the requested Job Image, for the EX-011 log
// line; it is empty when the Job had none.
type ProfileError struct {
	Image string
}

// Error implements error.
func (e *ProfileError) Error() string {
	if e.Image == "" {
		return ErrNoProfile.Error() + ": job has no image and no default profile is configured"
	}
	return fmt.Sprintf("%s: image %q matches no profile", ErrNoProfile.Error(), e.Image)
}

// Is reports whether target is ErrNoProfile.
func (e *ProfileError) Is(target error) bool { return target == ErrNoProfile }

// AllocationError is the error Allocate returns (SC-022, SC-033, SC-034,
// EX-013, OB-042). Err is one of the sentinel causes above or the Pool
// Manager error; Profile and Pool are always set and Host when known.
type AllocationError struct {
	Profile string
	Pool    poolmgr.PoolRef
	Host    string
	Err     error
}

// Error implements error.
func (e *AllocationError) Error() string {
	msg := fmt.Sprintf("scheduler: allocation for profile %q from pool %s failed", e.Profile, e.Pool)
	if e.Host != "" {
		msg += fmt.Sprintf(" on host %q", e.Host)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the cause.
func (e *AllocationError) Unwrap() error { return e.Err }

// JobInfo is what the Scheduler needs to know about a Job. It is a plain
// struct rather than the gitlab-runner spec.Job so that this package and its
// tests do not import gitlab-runner.
type JobInfo struct {
	// ID is the GitLab job id (SC-024, OB-002).
	ID int64
	// Image is the Job Image name; empty when the Job has none (SC-010,
	// SC-011).
	Image string
}

// Reservation is the Scheduler's promise of capacity for one not-yet-known
// Job (SC-003). It is opaque to the Executor, which stores it as the
// executor data (EX-003).
type Reservation struct {
	// ID is unique for the life of the process.
	ID uint64
	// GrantedAt is when the Slot was taken.
	GrantedAt time.Time
}

// PlacementSource records which SC-030/SC-031 path produced a Placement, so
// the harness can prove both are exercised (TD-008).
type PlacementSource string

// Placement sources.
const (
	// PlacementFromClaim: the claim response named the Host (SC-030).
	PlacementFromClaim PlacementSource = "claim"
	// PlacementFromLookup: GetMicroVM fan-out over the Pool's Hosts (SC-031).
	PlacementFromLookup PlacementSource = "lookup"
)

// Placement is the Host that runs a MicroVM (SC-030, SC-031). It is cached
// for the lifetime of the Lease (SC-032).
type Placement struct {
	// Host is the Inventory name (SC-034).
	Host string
	// Source is which path resolved it.
	Source PlacementSource
	// ResolvedAt is when it was known; with ClaimedAt it gives the
	// claim-to-Placement time (SC-025, OB-013).
	ResolvedAt time.Time
}

// PlacementResolver is the SC-030/SC-031 step on its own, so both paths are
// tested directly against a poolmgr.Claim literal and a fake Registry.
type PlacementResolver interface {
	// ResolvePlacement records claim.Host.Name when set (SC-030); otherwise
	// it fans GetMicroVM out over the Pool's Hosts (SC-031). When the claim
	// carries an address that differs from the Registry's Endpoint for that
	// name it logs a warning with both values and counts
	// FailureAddressMismatch, then continues with the Inventory endpoint:
	// battery reports the address as battery dials it, so a difference is a
	// configuration inconsistency, not a placement error. It returns an
	// *AllocationError whose cause is ErrHostNotInInventory or
	// ErrPlacementUnresolved; the caller releases the Lease (SC-033, SC-034).
	ResolvePlacement(ctx context.Context, p *Profile, claim *poolmgr.Claim) (Placement, error)
}

// Lease is the Scheduler's view of a Pool Manager Lease (PL-031, PL-040).
type Lease struct {
	// ID is the Pool Manager's lease id.
	ID string
	// Pool is the Pool it was claimed from.
	Pool poolmgr.PoolRef
	// ExpiresAt is the expiry last returned by Heartbeat (SC-060). Until the
	// first heartbeat it is ClaimedAt plus the Pool's heartbeat expiry
	// threshold, because ClaimVMResponse carries no expiry.
	ExpiresAt time.Time
	// LastHeartbeatAt is when the last successful heartbeat was sent.
	LastHeartbeatAt time.Time
}

// Allocation is the binding of a leased MicroVM to a Job (SC-024). It is a
// plain value; the live state lives on the Handle.
type Allocation struct {
	// JobID and Profile identify the Job and its Profile (SC-024).
	JobID   int64
	Profile string
	// VMUID is the flintlock uid of the MicroVM (SC-024).
	VMUID string
	// Lease is the Pool Manager lease (SC-024).
	Lease Lease
	// Placement is the Host (SC-024).
	Placement Placement
	// Host is the claim's host name and address as returned by the Pool
	// Manager (PL-031); zero when the Placement came from the fan-out.
	Host poolmgr.HostRef
	// NetworkInterfaces is copied from the claim (PL-031), for the direct
	// ssh transport (EX-048).
	NetworkInterfaces map[string]*types.NetworkInterfaceStatus
	// ReservationID is the Reservation this Allocation was converted from
	// (SC-026).
	ReservationID uint64
	// ClaimedAt is when ClaimVM succeeded; with Placement.ResolvedAt it
	// gives the allocation duration (SC-025, OB-013).
	ClaimedAt time.Time
}

// Handle is a live Allocation. It mirrors context.Context so that the
// Executor selects on Done alongside the Job's context while a Stage runs
// and, through common.WithContext, derives the Build's context from it. The
// Executor cancels that context with a cause of *common.BuildError carrying
// RunnerSystemFailure and Err(); a plain cancellation would be reported to
// GitLab as job_canceled (SC-043, SC-061, SC-062, GL-072).
type Handle interface {
	// Allocation returns the immutable Allocation.
	Allocation() Allocation
	// Done is closed when the Scheduler decides the Job can no longer run,
	// and stays open after a normal Release.
	Done() <-chan struct{}
	// Err is nil until Done is closed and then wraps ErrHostUnhealthy
	// (SC-043), ErrLeaseLost (SC-061, SC-062) or ErrShutdown (GL-072).
	Err() error
}

// Reserver grants and returns Reservations (SC-001 to SC-005, SC-008).
type Reserver interface {
	// Reserve grants a Reservation when available capacity is greater than
	// zero (SC-003) and returns a *Refusal otherwise (SC-004). It does not
	// block; ctx is for cancellation and tracing only. It is safe for
	// concurrent use by the run loop's workers (SC-007).
	Reserve(ctx context.Context) (*Reservation, error)

	// ReleaseReservation returns the Slot of a Reservation that was never
	// converted (SC-005, GL-032, GL-035). Releasing twice or after
	// conversion is a no-op.
	ReleaseReservation(r *Reservation)
}

// ProfileResolver maps a Job onto a Profile (SC-010 to SC-013).
type ProfileResolver interface {
	// ResolveProfile matches exact image names, then glob patterns in
	// configuration order, then the Default Profile (SC-010). It returns a
	// *ProfileError when nothing matches (SC-011, SC-012). It touches no
	// Pool (SC-013).
	ResolveProfile(job JobInfo) (*Profile, error)
}

// Allocator converts Reservations into Allocations and releases them (SC-020
// to SC-034, SC-050 to SC-054).
type Allocator interface {
	// Allocate claims a warm MicroVM from the Profile's Pool (SC-020),
	// retrying with backoff and waking on availability events until the
	// allocation timeout (SC-021, SC-022), resolves the Placement (SC-030,
	// SC-031), and converts r into the returned Handle without releasing
	// the Slot (SC-026). Cancelling ctx stops the allocation and releases
	// any Lease obtained (SC-023). On error, which is an *AllocationError,
	// r is still held and the caller releases it.
	Allocate(ctx context.Context, r *Reservation, job JobInfo, p *Profile) (Handle, error)

	// Release hands the Lease back (SC-050). It returns as soon as the
	// release has been requested (SC-052); retries continue in the
	// background (SC-051). The Slot is returned immediately. Releasing a
	// Handle whose Err is ErrLeaseLost makes no call (SC-061).
	Release(h Handle)

	// Retain is the keep-on-failure path (SC-054, EX-033): the Lease is kept
	// alive for the configured keep duration and then released as by
	// Release. The Slot is returned immediately.
	Retain(h Handle)
}

// HostHealth is the Scheduler's view of one Host (SC-040 to SC-042, OB-018).
type HostHealth struct {
	Name    string
	Healthy bool
	// ConsecutiveFailures counts probes since the last success (SC-041).
	ConsecutiveFailures int
	LastProbeAt         time.Time
	// Info is the last successful ServerInfo, nil if none (HO-011).
	Info *flintlock.HostInfo
	// Leased is the number of Allocations placed here (OB-018).
	Leased int
}

// Snapshot is the Scheduler's state for the readiness endpoint (OB-031,
// OB-032) and for tests, which read it instead of private fields.
type Snapshot struct {
	// PoolManagerContacted is true once any call has succeeded (SC-008).
	PoolManagerContacted bool
	// PoolManagerHealthy is false during the health backoff (PL-034) or
	// after consecutive probe failures (PL-005).
	PoolManagerHealthy bool
	// Slots is the concurrency limit; SlotsInUse is Reservations plus
	// Allocations (SC-001).
	Slots      int
	SlotsInUse int
	Pools      []poolmgr.PoolAvailability
	Hosts      []HostHealth
}

// Ready is true when the Pool Manager has answered and at least one Host
// is healthy (OB-031).
func (s Snapshot) Ready() bool {
	if !s.PoolManagerContacted {
		return false
	}
	for _, h := range s.Hosts {
		if h.Healthy {
			return true
		}
	}
	return false
}

// Status exposes state for the readiness endpoint (OB-031, OB-032).
type Status interface {
	Snapshot() Snapshot
}

// Lifecycle starts and reconfigures the Scheduler.
type Lifecycle interface {
	// Run declares the Pools (PL-010), compares their host lists with the
	// Inventory (HO-015), starts the Tracker, Health, Host probing (SC-040)
	// and the heartbeat loops, and blocks until ctx is cancelled. On
	// cancellation it releases every unconverted Reservation (GL-070),
	// keeps heartbeating held Leases until they are released or the
	// shutdown timeout elapses, then returns. It performs no cleanup from a
	// previous incarnation (SC-053).
	Run(ctx context.Context) error

	// Ready is closed once the Pool Manager has answered at least once
	// (SC-008, PL-004), which is when Reserve may first grant. main blocks
	// on it before reporting ready; /readyz additionally consults
	// Snapshot for Host health (OB-031).
	Ready() <-chan struct{}

	// Reload replaces the Profiles and the Inventory (CF-007). Pools are
	// re-declared (PL-010); a removed Profile's Pool is left in place
	// (PL-015); the Host Registry is reconciled (HO-014). Running Jobs are
	// not interrupted.
	Reload(ctx context.Context, profiles []config.Profile, inventory []config.HostEntry) error
}

// Scheduler is the whole component. Consumers take the smallest embedded
// interface that serves them.
type Scheduler interface {
	Reserver
	ProfileResolver
	Allocator
	Status
	Lifecycle
}

// FailureKind labels the failure counters of OB-020.
type FailureKind string

// Failure kinds.
const (
	FailureRelease   FailureKind = "release"
	FailureHeartbeat FailureKind = "heartbeat"
	// FailureAddressMismatch counts a claim whose host.address differed from
	// the Inventory endpoint (SC-030); the Job still runs.
	FailureAddressMismatch FailureKind = "address_mismatch"
)

// Metrics is the Scheduler's metrics sink (OB-013, OB-015 to OB-021). The
// production implementation updates Prometheus; tests install a recording
// one. Logs are not part of it: they go to Deps.Logger.
type Metrics interface {
	// ReservationRefused counts a refusal by reason (OB-015).
	ReservationRefused(reason RefusalReason)
	// AllocationObserved records the time from first claim attempt to
	// Placement for a Profile (OB-013).
	AllocationObserved(profile string, claimToPlacement time.Duration)
	// PoolWaited records a Job that waited on an exhausted Pool and for how
	// long (OB-021).
	PoolWaited(pool poolmgr.PoolRef, waited time.Duration)
	// PoolGauges sets the per-Pool gauges from the latest status (PL-054,
	// OB-016, OB-017, OB-019).
	PoolGauges(pool poolmgr.PoolRef, status poolmgr.PoolStatus, target int32)
	// HostGauges sets the per-Host gauges (OB-018).
	HostGauges(host string, healthy bool, leased int)
	// FailureCounted counts a failed release or heartbeat (OB-020).
	FailureCounted(kind FailureKind)
}

// Deps are the Scheduler's dependencies. Every field is an interface with an
// in-memory implementation; nothing here opens a socket.
type Deps struct {
	// PoolManager is the battery client, or the in-memory fake.
	PoolManager poolmgr.Client
	// Specs, HostSelector and Declarer declare Pools from Profiles (PL-010
	// to PL-026).
	Specs        poolmgr.SpecBuilder
	HostSelector poolmgr.HostSelector
	Declarer     poolmgr.Declarer
	// Tracker and Health are the capacity and health inputs (PL-050 to
	// PL-056, PL-004, PL-005, PL-034, PL-035).
	Tracker poolmgr.Tracker
	Health  poolmgr.Health
	// Hosts is the Host Registry (HO-010), built over a flintlock.Dialer.
	Hosts flintlock.Registry
	// Clock and Backoff default to real time and the configured exponential
	// policy when nil.
	Clock   clock.Clock
	Backoff clock.Backoff
	// Metrics defaults to a no-op when nil. Logger defaults to slog.Default.
	Metrics Metrics
	Logger  *slog.Logger
}

// Settings are the Scheduler's configuration, gathered from the sections
// the requirements spread it over.
type Settings struct {
	// RunnerName is the runner name for Pool labels (PL-014).
	RunnerName string
	// Namespace is the Runner namespace (PL-017, PL-021, CF-051).
	Namespace string
	// Slots is the concurrency limit (GL-033, SC-001).
	Slots int
	// ShutdownTimeout bounds how long held Leases are heartbeated after
	// Run's context is cancelled (GL-071, GL-072).
	ShutdownTimeout time.Duration
	// Scheduler and PoolManager are the configuration sections (CF-050,
	// CF-040).
	Scheduler   config.Scheduler
	PoolManager config.PoolManager
	// Profiles and Inventory are the initial values; Reload replaces them.
	Profiles  []config.Profile
	Inventory []config.HostEntry
}
