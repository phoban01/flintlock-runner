// Package fake is the fake Pool Manager
// (docs/requirements/10-test-doubles.md#fake-pool-manager): a minimal but
// real pool manager on the battery protos that creates, places and deletes
// MicroVMs through flintlock.PoolHostClient (TD-002). It serves the
// poolmgr.v1alpha1 services over gRPC (TD-001) and is also usable in process
// through Client, so Scheduler tests need no network.
//
// Phase 0 provides the compiling skeleton: every method returns
// errors.ErrUnsupported or a zero value. The `fakes` work package of
// docs/PLAN.md fills it in; each todo annotation below names the requirement
// it will satisfy.
package fake

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=todo
//= tracking-issue=TBD
//# The project SHALL provide a fake Pool Manager that serves the
//# `poolmgr.v1alpha1` `PoolAdmin`, `Lease` and `Events` services over gRPC
//# using the generated server stubs from the battery module.

// PoolManager is the fake Pool Manager. It is constructed from a
// poolmgr.FakeConfig and served with Serve, or used in process via Client.
type PoolManager struct {
	cfg poolmgr.FakeConfig

	mu     sync.Mutex
	faults poolmgr.Faults
}

// New builds a fake Pool Manager from cfg. Zero fields take the defaults
// documented on poolmgr.FakeConfig.
func New(cfg poolmgr.FakeConfig) *PoolManager {
	if cfg.Placement == "" {
		cfg.Placement = poolmgr.PlacementLeastVMs
	}
	return &PoolManager{cfg: cfg}
}

// Config returns the configuration the fake was built with.
func (p *PoolManager) Config() poolmgr.FakeConfig { return p.cfg }

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=todo
//= tracking-issue=TBD
//# The fake Pool Manager SHALL be runnable as a standalone binary as well as
//# in process, so that a fleet without battery can run on it.

// Serve listens on cfg.Listen and serves the three gRPC services until ctx
// is cancelled (TD-001, TD-009).
func (p *PoolManager) Serve(ctx context.Context) error {
	_ = ctx
	return errors.ErrUnsupported
}

// Addr is the bound listen address once Serve is running, empty before.
func (p *PoolManager) Addr() string { return "" }

// Client returns an in-process poolmgr.Client bound to this fake, for tests
// that need no gRPC.
func (p *PoolManager) Client() poolmgr.Client { return &Client{pm: p} }

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=todo
//= tracking-issue=TBD
//# The fake Pool Manager SHALL support fault injection for claim latency, a
//# period of `UNAVAILABLE`, hook failure and a dropped `Events` stream.

// SetFaults implements poolmgr.FaultInjector.
func (p *PoolManager) SetFaults(f poolmgr.Faults) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults = f
}

// Faults implements poolmgr.FaultInjector.
func (p *PoolManager) Faults() poolmgr.Faults {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.faults
}

// Leases implements poolmgr.Inspector (TD-054).
func (p *PoolManager) Leases() []poolmgr.LeaseRecord { return nil }

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=todo
//= tracking-issue=TBD
//# The fake Pool Manager SHALL place MicroVMs across a Pool's
//# `flintlock_hosts` by least MicroVM count, matching battery's design.

// VMs implements poolmgr.Inspector (TD-007).
func (p *PoolManager) VMs() []poolmgr.VMRecord { return nil }

// Client is the in-process poolmgr.Client over a PoolManager. The gRPC
// client in package poolmgr and this one are interchangeable to the
// Scheduler.
type Client struct {
	pm *PoolManager
}

// CreatePool implements poolmgr.PoolAdmin.
func (c *Client) CreatePool(context.Context, poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	return nil, errors.ErrUnsupported
}

// UpdatePool implements poolmgr.PoolAdmin.
func (c *Client) UpdatePool(context.Context, poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	return nil, errors.ErrUnsupported
}

// DeletePool implements poolmgr.PoolAdmin.
func (c *Client) DeletePool(context.Context, poolmgr.PoolRef) error { return errors.ErrUnsupported }

// GetPool implements poolmgr.PoolAdmin.
func (c *Client) GetPool(context.Context, poolmgr.PoolRef) (*poolmgr.Pool, error) {
	return nil, errors.ErrUnsupported
}

// ListPools implements poolmgr.PoolAdmin.
func (c *Client) ListPools(context.Context, string) ([]*poolmgr.Pool, error) {
	return nil, errors.ErrUnsupported
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=todo
//= tracking-issue=TBD
//# The fake Pool Manager SHALL return `RESOURCE_EXHAUSTED` from `ClaimVM`
//# when the Pool has no MicroVM in the `AVAILABLE` phase.

// ClaimVM implements poolmgr.Lease.
func (c *Client) ClaimVM(context.Context, poolmgr.PoolRef) (*poolmgr.Claim, error) {
	return nil, errors.ErrUnsupported
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=todo
//= tracking-issue=TBD
//# The fake Pool Manager SHALL expire a Lease whose last heartbeat is older
//# than the Pool's heartbeat expiry threshold and SHALL delete the expired
//# Lease's MicroVM.

// Heartbeat implements poolmgr.Lease.
func (c *Client) Heartbeat(context.Context, string) (time.Time, error) {
	return time.Time{}, errors.ErrUnsupported
}

// ReleaseVM implements poolmgr.Lease.
func (c *Client) ReleaseVM(context.Context, string) error { return errors.ErrUnsupported }

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=todo
//= tracking-issue=TBD
//# The fake Pool Manager SHALL emit every `Event` type defined in the proto
//# at the corresponding phase transition.

// Subscribe implements poolmgr.Events.
func (c *Client) Subscribe(context.Context, poolmgr.EventFilter) (poolmgr.EventStream, error) {
	return nil, errors.ErrUnsupported
}

// Close implements poolmgr.Client.
func (c *Client) Close() error { return nil }

// Compile-time interface checks.
var (
	_ poolmgr.Client        = (*Client)(nil)
	_ poolmgr.FaultInjector = (*PoolManager)(nil)
	_ poolmgr.Inspector     = (*PoolManager)(nil)
)
