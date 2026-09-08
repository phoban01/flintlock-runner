package poolmgr

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// The enum types below are aliases onto the generated proto enums, so the
// fake, the client and the Scheduler share one definition (TD-006). Use the
// proto constants, for example poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE.

type (
	// ReplenishmentStrategyType is the proto ReplenishmentStrategyType (PL-013).
	ReplenishmentStrategyType = poolmgrv1.ReplenishmentStrategyType
	// HookFailurePolicy is the proto HookFailurePolicy (CF-025).
	HookFailurePolicy = poolmgrv1.HookFailurePolicy
	// VMPhase is the proto VMPhase (TD-005).
	VMPhase = poolmgrv1.VMPhase
	// EventType is the proto EventType (PL-052, TD-006).
	EventType = poolmgrv1.EventType
)

// Sentinel errors. Implementations wrap them so errors.Is works.
var (
	// ErrExhausted is RESOURCE_EXHAUSTED from ClaimVM: the Pool has no
	// AVAILABLE MicroVM (PL-032, PL-053, TD-005).
	ErrExhausted = errors.New("poolmgr: no warm microvm available")
	// ErrNotFound is NOT_FOUND: an unknown Pool on ClaimVM or GetPool
	// (PL-033), an unknown lease on ReleaseVM (PL-042) or Heartbeat
	// (SC-061).
	ErrNotFound = errors.New("poolmgr: not found")
	// ErrAlreadyExists is ALREADY_EXISTS from CreatePool; the Declarer then
	// calls UpdatePool (PL-010).
	ErrAlreadyExists = errors.New("poolmgr: pool already exists")
	// ErrUnavailable is UNAVAILABLE or a connection error (PL-004, PL-034,
	// PL-006).
	ErrUnavailable = errors.New("poolmgr: unavailable")
	// ErrInvalid is INVALID_ARGUMENT, for a spec the Pool Manager rejects
	// (PL-016).
	ErrInvalid = errors.New("poolmgr: invalid request")
)

// PoolRef identifies a Pool (proto PoolRef). The namespace is the Runner
// namespace (PL-017, CF-028).
type PoolRef struct {
	Name      string
	Namespace string
}

// String renders namespace/name for logs and error messages.
func (r PoolRef) String() string { return r.Namespace + "/" + r.Name }

// ReplenishmentStrategy is the proto ReplenishmentStrategy (PL-013).
type ReplenishmentStrategy struct {
	Type ReplenishmentStrategyType
	// MinSize is meaningful only for MIN_SIZE_THRESHOLD.
	MinSize *int32
}

// PoolSpec is the proto PoolSpec. SpecBuilder derives one per Profile
// (PL-010, PL-020 to PL-026).
type PoolSpec struct {
	Ref PoolRef
	// Template is the flintlock MicroVMSpec every Pool MicroVM is created
	// from. The proto type is used directly because PL-020 maps Profile
	// fields onto it one to one. allow_guest_agent is set (PL-012), id is
	// empty (PL-021) and it carries the two Runner labels (PL-014).
	Template *types.MicroVMSpec
	// Size is the target number of warm MicroVMs.
	Size int32
	// FlintlockHosts are the Host names the Pool may place on (PL-011).
	FlintlockHosts []string
	Replenishment  ReplenishmentStrategy
	// CreateCommands and PreLeaseCommands are the Pool hooks (CF-025).
	CreateCommands    []string
	PreLeaseCommands  []string
	HookFailurePolicy HookFailurePolicy
	// HeartbeatInterval and HeartbeatExpiryThreshold are the lease settings
	// (PL-040, TD-004).
	HeartbeatInterval        time.Duration
	HeartbeatExpiryThreshold time.Duration
}

// PoolStatus is the proto PoolStatus (PL-050, OB-016, OB-019).
type PoolStatus struct {
	Available    int32
	Leased       int32
	Provisioning int32
	Quarantined  int32
}

// Pool pairs a spec with its live status (proto Pool).
type Pool struct {
	Spec   PoolSpec
	Status PoolStatus
}

// HostRef is the proto HostInfo on ClaimVMResponse: the Host the leased
// MicroVM runs on (SC-030, PL-031).
type HostRef struct {
	// Name is the Host's configured name, which is also its Inventory name
	// (CF-032, FL-052).
	Name string
	// Address is the Host's flintlockd gRPC endpoint. The Scheduler checks
	// it against the Inventory entry's endpoint (SC-030).
	Address string
}

// Claim is a successful ClaimVMResponse (PL-031).
type Claim struct {
	// LeaseID is presented on every Heartbeat and ReleaseVM.
	LeaseID string
	// VMUID is the flintlock uid of the leased MicroVM.
	VMUID string
	// NetworkInterfaces is copied from flintlock's MicroVMStatus.
	NetworkInterfaces map[string]*types.NetworkInterfaceStatus
	// Host is the Host that runs the MicroVM, from the response's host
	// field (SC-030). It is zero when the Pool Manager predates that field,
	// in which case the Scheduler falls back to the GetMicroVM fan-out
	// (SC-031). The fake Pool Manager omits it on request (TD-008).
	Host HostRef
}

// Event is one proto Event (PL-050, PL-052).
type Event struct {
	// ID is monotonic per Pool; the fake replays recent events to new
	// subscribers from it.
	ID   int64
	Pool PoolRef
	// VMUID is empty for pool-level events.
	VMUID string
	Type  EventType
	At    time.Time
	// Payload is the type-specific payload_json, left for the consumer to
	// decode (PL-055 reads the counts from it).
	Payload json.RawMessage
}

// EventStream is one Events.Subscribe subscription (PL-050).
type EventStream interface {
	// Recv blocks for the next event. It returns a non-nil error, wrapping
	// ErrUnavailable, when the stream is dropped (PL-051, TD-010); the
	// Tracker then polls GetPool and re-subscribes.
	Recv(ctx context.Context) (*Event, error)
	// Close ends the subscription.
	Close() error
}

// EventFilter narrows a subscription to one Pool, as the proto
// SubscribeRequest does with its PoolRef; nil subscribes to every Pool.
type EventFilter struct {
	Pool *PoolRef
}

// PoolAdmin mirrors the PoolAdmin service. The Declarer uses it to declare
// Pools (PL-010) and Health to probe (PL-005); the Fleet Controller uses it
// for verify, drain and teardown (FL-054, FL-080, FL-081).
type PoolAdmin interface {
	// CreatePool returns ErrAlreadyExists for a known name and namespace.
	CreatePool(ctx context.Context, spec PoolSpec) (*Pool, error)
	// UpdatePool returns ErrNotFound for an unknown name and namespace.
	UpdatePool(ctx context.Context, spec PoolSpec) (*Pool, error)
	// DeletePool returns ErrNotFound for an unknown Pool. The Runner never
	// calls it on reload (PL-015); only teardown does (FL-081).
	DeletePool(ctx context.Context, ref PoolRef) error
	// GetPool returns ErrNotFound for an unknown Pool. It is the polling
	// fallback for capacity (PL-051) and the source of a Pool's host list
	// for the Placement fan-out (SC-031) and the HO-015 comparison.
	GetPool(ctx context.Context, ref PoolRef) (*Pool, error)
	// ListPools lists the Pools in a namespace; empty lists every namespace.
	// It is the health probe (PL-005).
	ListPools(ctx context.Context, namespace string) ([]*Pool, error)
}

// Lease mirrors the Lease service (PL-030 to PL-043).
type Lease interface {
	// ClaimVM returns ErrExhausted when no MicroVM is AVAILABLE (PL-032),
	// ErrNotFound for an unknown Pool (PL-033) and ErrUnavailable when the
	// Pool Manager cannot be reached (PL-034).
	ClaimVM(ctx context.Context, pool PoolRef) (*Claim, error)
	// Heartbeat extends the lease and returns the new expiry (PL-040,
	// SC-060). ErrNotFound means the lease no longer exists (SC-061).
	Heartbeat(ctx context.Context, leaseID string) (expiresAt time.Time, err error)
	// ReleaseVM releases the lease (PL-041). ErrNotFound counts as released
	// (PL-042); other errors are retried by the caller (PL-043).
	ReleaseVM(ctx context.Context, leaseID string) error
}

// Events mirrors the Events service (PL-050).
type Events interface {
	// Subscribe opens a stream. It returns ErrUnavailable when the Pool
	// Manager cannot be reached; the stream itself reports later drops.
	Subscribe(ctx context.Context, filter EventFilter) (EventStream, error)
}

// Client is the whole Pool Manager surface behind one long-lived connection
// with keepalive (PL-006) and the configured deadline on every unary call
// (PL-002). Implementations are safe for concurrent use. Consumers that need
// less take PoolAdmin, Lease or Events instead.
type Client interface {
	PoolAdmin
	Lease
	Events
	// Close closes the connection; open EventStreams end with ErrUnavailable.
	Close() error
}

// Policy above the wire. Each of these is a small, separately testable unit
// of the PL requirements that the Scheduler composes.

// SpecInput is what a SpecBuilder needs besides the Profile.
type SpecInput struct {
	// RunnerName labels the template (PL-014).
	RunnerName string
	// Namespace is the Runner namespace for the Pool and the template
	// (PL-017, PL-021).
	Namespace string
	// Hosts is the Pool's flintlock_hosts, from HostSelector (PL-011).
	Hosts []string
}

// GuestDeviceID is the device_id of the single network interface every Pool
// MicroVM template carries (PL-020). It is a TAP interface on the Host's
// guest bridge with DHCP addressing (FL-040, FL-041); the name is fixed
// because a Profile has no interface configuration and `eth0` is reserved by
// Firecracker (PL-023).
const GuestDeviceID = "net0"

// SpecBuilder derives a PoolSpec from a Profile (PL-010, PL-012 to PL-014,
// PL-020 to PL-026). It is a pure function of its inputs, so its tests are
// table-driven, and it never sees a Job (PL-026). The template's only
// interface is GuestDeviceID (PL-020, PL-023).
type SpecBuilder interface {
	Build(p config.Profile, in SpecInput) (PoolSpec, error)
}

// HostSelector picks the Inventory Hosts a Profile's Pool may place on by
// architecture and Host selector labels (PL-011, CF-024).
type HostSelector interface {
	Select(p config.Profile, inventory []config.HostEntry) []string
}

// Declarer creates or updates Pools (PL-010, PL-015, PL-016, PL-033). It
// hides create-then-update behind one call and reports what happened.
type Declarer interface {
	// Declare creates the Pool, or updates it when CreatePool returns
	// ErrAlreadyExists, and returns the Pool as the Pool Manager holds it.
	// A failure is returned for the caller to log and retry at the
	// configured interval (PL-016); no retry happens inside.
	Declare(ctx context.Context, spec PoolSpec) (*Pool, error)
}

// PoolAvailability is one Pool's contribution to capacity (SC-002, SC-009,
// PL-050).
type PoolAvailability struct {
	Pool   PoolRef
	Status PoolStatus
	// Declared is false while CreatePool or UpdatePool is failing (PL-016);
	// the Pool then counts as empty.
	Declared bool
	// Waiting is the number of allocations currently waiting on this Pool
	// (OB-004, OB-021).
	Waiting int
}

// Tracker tracks available warm MicroVMs per Pool (PL-050 to PL-056, SC-009).
// It is the wake-up source for SC-021 and the input to SC-002.
type Tracker interface {
	// Run subscribes to Events and falls back to polling GetPool while the
	// stream is unavailable (PL-050, PL-051). It blocks until ctx is
	// cancelled.
	Run(ctx context.Context) error
	// Track starts tracking a Pool and Untrack stops; the Scheduler calls
	// them from Pool declaration and reload.
	Track(ref PoolRef, declared bool)
	Untrack(ref PoolRef)
	// Available is the Pool's last known available count (PL-052); zero
	// after MarkExhausted until the next event or poll (PL-053) and zero
	// for an undeclared Pool (PL-016).
	Available(ref PoolRef) int32
	// MarkExhausted records a RESOURCE_EXHAUSTED claim (PL-053).
	MarkExhausted(ref PoolRef)
	// Wait returns a channel that is closed the next time an event reports a
	// MicroVM in the Pool becoming available (SC-021).
	Wait(ref PoolRef) <-chan struct{}
	// Pools is a snapshot of every tracked Pool (OB-016, OB-019, OB-032).
	Pools() []PoolAvailability
}

// Health tracks whether the Pool Manager is reachable (PL-004, PL-005,
// PL-034, PL-035, SC-008).
type Health interface {
	// Run probes ListPools at the configured interval (PL-005) and retries
	// the first contact with backoff (PL-004). It blocks until ctx is
	// cancelled.
	Run(ctx context.Context) error
	// Contacted is closed once any call has succeeded (SC-008, PL-004).
	Contacted() <-chan struct{}
	// Healthy is false during the backoff after MarkUnavailable (PL-034)
	// and after the configured number of consecutive probe failures
	// (PL-005).
	Healthy() bool
	// MarkUnavailable records an UNAVAILABLE claim or connection error
	// (PL-034).
	MarkUnavailable()
}

// Test-double shapes. These are the configuration and control surface of
// the fake Pool Manager in package fake; they live here so that the harness,
// the standalone binary (TD-009) and the fake share one definition.

// HostSource resolves a flintlock_hosts name to a Host the fake Pool Manager
// can create on. It is the seam of TD-002: in process the harness returns
// fake Hosts, the standalone binary (TD-009) dials real flintlockd instances
// through flintlock.AdminDialer, and the fake's placement, hook and delete
// code is identical in both cases.
type HostSource interface {
	// Host returns the client for a name or flintlock.ErrUnknownHost.
	Host(name string) (flintlock.PoolHostClient, error)
	// Names lists every known Host, for FL-054 reachability reporting.
	Names() []string
}

// PlacementStrategy names how the fake Pool Manager spreads MicroVMs over a
// Pool's flintlock_hosts (TD-007).
type PlacementStrategy string

// Placement strategies. PlacementLeastVMs is the default and matches
// battery's design (TD-007); PlacementRoundRobin is battery's other mode.
const (
	PlacementLeastVMs   PlacementStrategy = "least_vms"
	PlacementRoundRobin PlacementStrategy = "round_robin"
)

// FakeConfig is the static configuration of the fake Pool Manager (TD-001 to
// TD-009). It is a value type built from a literal by tests and the harness
// and parsed from flags by the standalone binary.
type FakeConfig struct {
	// Listen is the gRPC listen address; ":0" picks a free port.
	Listen string
	// Hosts resolves flintlock_hosts names (TD-002).
	Hosts HostSource
	// Clock drives expiry and latency (TD-004, TD-010).
	Clock clock.Clock
	// Placement selects the placement strategy; empty means
	// PlacementLeastVMs (TD-007).
	Placement PlacementStrategy
	// ReconcileInterval is how often Pools are topped up and expired leases
	// swept (TD-003, TD-004).
	ReconcileInterval time.Duration
	// ReadyTimeout bounds the wait for a created MicroVM to run its create
	// hooks before it is AVAILABLE.
	ReadyTimeout time.Duration
	// EventReplay is how many recent events per Pool a new subscriber is
	// replayed.
	EventReplay int
	// OmitHostOnClaim leaves the host field of ClaimVMResponse unset so that
	// the Scheduler's fan-out path (SC-031) is exercised (TD-008).
	OmitHostOnClaim bool
	// Namespace, when set, restricts the fake to Pools in that namespace and
	// rejects others with ErrInvalid; the harness uses it to prove PL-017.
	Namespace string
}

// HookKind names a Pool hook for fault injection (TD-010).
type HookKind string

// Hook kinds.
const (
	HookCreate   HookKind = "create"
	HookPreLease HookKind = "pre_lease"
)

// HookFailure makes the next Remaining executions of Hook in Pool fail so
// that VM_HOOK_FAILED and the hook failure policy are exercised (TD-010,
// PL-056).
type HookFailure struct {
	Pool      PoolRef
	Hook      HookKind
	Remaining int
}

// Faults are the runtime fault injection switches of the fake Pool Manager
// (TD-010). All zero means a healthy Pool Manager. They are read per request
// and may be changed while the fake is serving.
type Faults struct {
	// ClaimLatency delays every ClaimVM response on the fake's Clock.
	ClaimLatency time.Duration
	// UnavailableFor makes every RPC fail with UNAVAILABLE for this long
	// after SetFaults, measured on the fake's Clock (PL-034, PL-035).
	UnavailableFor time.Duration
	// HookFailures are pending hook failures, consumed as they fire.
	HookFailures []HookFailure
	// DropEventsStream ends every open Subscribe stream once, then clears
	// itself (PL-051).
	DropEventsStream bool
	// RefuseHeartbeats makes Heartbeat return NOT_FOUND for every lease,
	// which is how "a Lease expiring during a Job" (TD-051, SC-061) is
	// produced without waiting out the expiry threshold.
	RefuseHeartbeats bool
}

// FaultInjector is implemented by the fake Pool Manager so that tests and
// the harness set Faults at runtime.
type FaultInjector interface {
	SetFaults(Faults)
	Faults() Faults
}

// LeaseRecord is the proto LeaseRecord, exposed by Inspector.
type LeaseRecord struct {
	LeaseID         string
	VMUID           string
	Pool            PoolRef
	ClaimedAt       time.Time
	LastHeartbeatAt time.Time
	ExpiresAt       time.Time
}

// VMRecord is the proto VMRecord, exposed by Inspector.
type VMRecord struct {
	UID       string
	Pool      PoolRef
	Host      string
	Phase     VMPhase
	LeaseID   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Inspector is implemented by the fake Pool Manager so that the harness can
// assert that no lease is held after shutdown (TD-054) and that tests can
// check placement (TD-007) without a second protocol.
type Inspector interface {
	Leases() []LeaseRecord
	VMs() []VMRecord
}
