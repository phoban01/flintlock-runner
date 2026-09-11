package executor

import (
	"context"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// Name is the executor name in the provider registry (EX-002) and in
// info.executor on every job request (GL-020).
const Name = "flintlock"

// DefaultShell is the shell name reported by GetDefaultShell (EX-006,
// GL-021).
const DefaultShell = "bash"

// PrepareSection is the name of the collapsible Job log section written
// during Prepare (EX-019, OB-040, OB-041, EX-064).
const PrepareSection = "flintlock_prepare"

// Scheduler is the part of the scheduling component the Executor calls. It
// is a consumer-side narrowing of scheduler.Scheduler: Acquire and Release
// use Reserver (EX-003 to EX-005), Prepare uses ProfileResolver and Allocator
// (EX-010 to EX-013), Cleanup uses Allocator (EX-030, EX-032, EX-033).
type Scheduler interface {
	scheduler.Reserver
	scheduler.ProfileResolver
	scheduler.Allocator
}

// Provider is what the flintlock executor registers in the provider registry
// (EX-001, EX-002, GL-004). ManagedExecutorProvider is included so that the
// run loop's Init and Shutdown hooks start and stop the Scheduler's Run
// (GL-070 to GL-073).
type Provider interface {
	common.ExecutorProvider
	common.ManagedExecutorProvider
}

// Executor is one Job's executor (EX-001). The alias exists so that the
// package's own tests and the harness name the contract without spelling
// out the gitlab-runner import.
type Executor interface {
	common.Executor
}

// Data is the executor data returned by Acquire and passed back to Release
// and Prepare (EX-003, EX-005). It carries the Reservation from Acquire and,
// once Prepare has allocated, the Handle. It implements common.WithContext
// so that the Build's context is cancelled when the Handle is done, and
// common.ExecutorDataLogger for OB-002.
//
// The cancellation carries a cause. At the pinned gitlab-runner commit,
// Build.run handles ctx.Done() with context.Cause(ctx) and maps a bare
// context.Canceled onto job_canceled; a *common.BuildError cause is reported
// as is. SC-043, SC-061, SC-062 and GL-072 require runner_system_failure, so
// the cause is a BuildError with that reason wrapping Handle.Err(). The
// executor work package keeps this invariant when it implements Run and
// Finish: never cancel the Build context without a BuildError cause.
type Data struct {
	Reservation *scheduler.Reservation
	// Handle is set by Prepare and read by WithContext and Cleanup.
	Handle scheduler.Handle
}

// WithContext implements common.WithContext. The run loop calls it after
// Prepare and before the first Stage; the returned context is cancelled with
// a runner_system_failure BuildError cause when the Handle is done, or
// plainly when the parent is cancelled.
func (d *Data) WithContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(parent)
	cancel := func() { cancelCause(nil) }
	if d == nil || d.Handle == nil {
		return ctx, cancel
	}
	h := d.Handle
	go func() {
		select {
		case <-h.Done():
			cancelCause(&common.BuildError{
				Inner:         h.Err(),
				FailureReason: common.RunnerSystemFailure,
			})
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// LogFields implements common.ExecutorDataLogger (OB-002).
func (d *Data) LogFields() map[string]string {
	if d == nil || d.Handle == nil {
		return nil
	}
	a := d.Handle.Allocation()
	return map[string]string{
		"profile": a.Profile,
		"vm_uid":  a.VMUID,
		"host":    a.Placement.Host,
	}
}

// Compile-time checks against the upstream optional interfaces.
var (
	_ common.WithContext        = (*Data)(nil)
	_ common.ExecutorDataLogger = (*Data)(nil)
)

// PrepareReport is what the flintlock_prepare section is rendered from
// (EX-019, OB-040, OB-041, OB-043, EX-064). It carries no endpoint or token.
type PrepareReport struct {
	Profile    string
	Pool       string
	Source     scheduler.PlacementSource
	HostName   string
	MicroVMUID string
	// AllocatedIn is claim to Placement; ReadyIn is allocation to guest
	// readiness (OB-013, OB-014).
	AllocatedIn time.Duration
	ReadyIn     time.Duration
	// Services names the Host Services made available (EX-064).
	Services []string
}

// HostServiceEnv is the environment the Executor adds for one Host's Host
// Services (EX-060, EX-065, EX-066).
type HostServiceEnv struct {
	// Vars are the variables to add, unless the Job sets the same name
	// (EX-061). The Executor applies that filter, not the resolver, so the
	// resolver is a pure function of the Inventory entry.
	Vars map[string]string
	// Services names the Host Services that were made available, for the
	// flintlock_prepare log line (EX-064). Disabled services and services
	// without an address are absent (EX-062).
	Services []string
}

// HostServiceVarNames is the closed list of fixed variable names a
// HostServiceEnvResolver may emit (EX-060, EX-065). The names of the
// configured HTTP cache upstreams are added per configuration. GOPRIVATE is
// deliberately absent (EX-066); the Executor drops any key outside this list
// plus the configured upstream names when it merges the environment, so
// EX-066 and SE-053 hold by construction rather than by convention.
var HostServiceVarNames = []string{
	"BUILDKIT_HOST",
	"GOPROXY",
	"GOFLAGS",
	"GONOSUMDB",
	"GONOPROXY",
	"CI_REGISTRY_MIRROR",
}

// HostServiceEnvResolver computes the Host Service environment for the Host
// a Job was placed on (EX-060 to EX-066, SE-053). It reads only the
// configuration and the Inventory entry, so its tests are table-driven.
type HostServiceEnvResolver interface {
	// Resolve returns the environment for host. A nil host, which happens
	// when the Placement names a Host missing from the Inventory, yields an
	// empty environment.
	Resolve(host *config.HostEntry) HostServiceEnv
}

// InventoryLookup resolves a Placement's Host name to its Inventory entry
// (EX-060, EX-062, SE-053). The flintlock Registry knows Endpoints, not
// entries, and internal/flintlock does not import internal/config, so the
// Executor takes its own view of the Inventory; on reload main swaps it.
type InventoryLookup interface {
	Host(name string) (*config.HostEntry, bool)
}

// Timeouts are the Executor's configured bounds, copied from config.Executor
// so that tests set them without a Config.
type Timeouts struct {
	// Prepare bounds Prepare as a whole (EX-017).
	Prepare time.Duration
	// GracefulKill bounds Run after cancellation (EX-024).
	GracefulKill time.Duration
	// Transport bounds an in-flight transport operation (EX-051).
	Transport time.Duration
}

// Deps are the Executor's dependencies. Every field has an in-memory fake.
type Deps struct {
	Scheduler  Scheduler
	Transports transport.Factory
	Env        HostServiceEnvResolver
	Inventory  InventoryLookup
	Timeouts   Timeouts
	// KeepOnFailure enables the debug option of EX-033 and SC-054.
	KeepOnFailure bool
	// CacheConfigured is false when the Distributed cache section is absent,
	// in which case Jobs using `cache:` fail with runner_unsupported
	// (CF-083).
	CacheConfigured bool
	// Hosts is the Host Registry the Placement is looked up in. The
	// Executor takes a Lease on the Placement's Host for the life of the
	// Job, so that a reload removing the Host does not close the
	// connection under a running Stage (HO-014).
	Hosts flintlock.Registry
}
