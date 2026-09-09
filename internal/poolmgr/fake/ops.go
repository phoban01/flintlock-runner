package fake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// The operations in this file are what the gRPC handlers call. They return
// gRPC status errors directly, because the only consumers are the handlers
// and, through them, the loopback client, which maps codes back onto the
// poolmgr sentinel errors.

func errNotFound(format string, args ...any) error {
	return status.Errorf(codes.NotFound, format, args...)
}

func errInvalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

// validateSpec applies battery's PoolAdmin validation plus a check that the
// heartbeat expiry threshold is positive, because a zero threshold would
// expire every Lease on the next tick, which is a silent way to lose Jobs.
func validateSpec(spec *poolmgr.PoolSpec) error {
	if spec.Ref.Name == "" {
		return errInvalid("spec.name is required")
	}
	if spec.Ref.Namespace == "" {
		return errInvalid("spec.namespace is required")
	}
	switch spec.Replenishment.Type {
	case poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE, poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE:
	case poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD:
		if spec.Replenishment.MinSize == nil || *spec.Replenishment.MinSize <= 0 {
			return errInvalid("spec.replenishment_strategy: MIN_SIZE_THRESHOLD requires a positive min_size")
		}
	default:
		return errInvalid("spec.replenishment_strategy: unknown replenishment strategy %v", spec.Replenishment.Type)
	}
	switch spec.HookFailurePolicy {
	case poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE, poolmgrv1.HookFailurePolicy_QUARANTINE:
	default:
		return errInvalid("spec.hook_failure_policy: invalid value %v", spec.HookFailurePolicy)
	}
	if spec.Template == nil {
		return errInvalid("spec.microvm_template is required")
	}
	if spec.Size < 0 {
		return errInvalid("spec.size must not be negative")
	}
	if spec.HeartbeatExpiryThreshold <= 0 {
		return errInvalid("spec.heartbeat_expiry_threshold must be positive")
	}
	// battery forces allow_guest_agent on server side (PL-012).
	spec.Template = proto.Clone(spec.Template).(*types.MicroVMSpec)
	spec.Template.AllowGuestAgent = true
	spec.FlintlockHosts = append([]string(nil), spec.FlintlockHosts...)
	spec.CreateCommands = append([]string(nil), spec.CreateCommands...)
	spec.PreLeaseCommands = append([]string(nil), spec.PreLeaseCommands...)
	return nil
}

// createPool implements PoolAdmin.CreatePool.
func (p *PoolManager) createPool(spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkNamespace(spec.Ref.Namespace); err != nil {
		return nil, err
	}
	key := keyOf(spec.Ref)
	if _, exists := p.pools[key]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "pool %s already exists", key)
	}
	p.pools[key] = &poolState{key: key, spec: spec, fresh: true, warned: make(map[string]int64)}
	p.kickReconcile()
	return &poolmgr.Pool{Spec: spec}, nil
}

// updatePool implements PoolAdmin.UpdatePool. Existing MicroVMs are kept,
// including any on a Host the new spec no longer lists; the new spec applies
// to every later decision.
func (p *PoolManager) updatePool(spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, err := p.poolLocked(spec.Ref)
	if err != nil {
		return nil, err
	}
	ps.spec = spec
	ps.fresh = true
	p.kickReconcile()
	return &poolmgr.Pool{Spec: spec, Status: p.countsLocked(ps.key).status()}, nil
}

// deletePool implements PoolAdmin.DeletePool. battery refuses while the Pool
// owns any MicroVM; the fake refuses only while a Lease is outstanding and
// otherwise deletes the Pool's MicroVMs from their Hosts, because that is
// what teardown (FL-081) needs from it.
func (p *PoolManager) deletePool(ctx context.Context, ref poolmgr.PoolRef) error {
	p.mu.Lock()
	ps, err := p.poolLocked(ref)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	for _, ls := range p.leases {
		if keyOf(ls.rec.Pool) == ps.key {
			p.mu.Unlock()
			return status.Errorf(codes.FailedPrecondition, "pool %s has outstanding lease %s; release it first", ps.key, ls.rec.LeaseID)
		}
	}
	delete(p.pools, ps.key)
	var vms []*vmState
	for _, vm := range p.vms {
		if vm.pool == ps.key {
			vms = append(vms, vm)
		}
	}
	p.mu.Unlock()

	var firstErr error
	for _, vm := range vms {
		if vm.uid == "" {
			// CreateMicroVM is still in flight; the provisioner sees the
			// Pool is gone when it returns and deletes the MicroVM itself.
			continue
		}
		if err := p.deleteVM(ctx, vm, 0); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return status.Errorf(codes.Unavailable, "pool %s deleted but some microvms remain pending deletion: %v", ps.key, firstErr)
	}
	return nil
}

// getPool implements PoolAdmin.GetPool.
func (p *PoolManager) getPool(ref poolmgr.PoolRef) (*poolmgr.Pool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, err := p.poolLocked(ref)
	if err != nil {
		return nil, err
	}
	return &poolmgr.Pool{Spec: ps.spec, Status: p.countsLocked(ps.key).status()}, nil
}

// listPools implements PoolAdmin.ListPools; an empty namespace lists every
// Pool. Results are sorted by namespace then name.
func (p *PoolManager) listPools(namespace string) []*poolmgr.Pool {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*poolmgr.Pool
	for key, ps := range p.pools {
		if namespace != "" && key.namespace != namespace {
			continue
		}
		out = append(out, &poolmgr.Pool{Spec: ps.spec, Status: p.countsLocked(key).status()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Spec.Ref.Namespace != out[j].Spec.Ref.Namespace {
			return out[i].Spec.Ref.Namespace < out[j].Spec.Ref.Namespace
		}
		return out[i].Spec.Ref.Name < out[j].Spec.Ref.Name
	})
	return out
}

// claimResult is what claimVM hands the handler to build ClaimVMResponse.
type claimResult struct {
	leaseID string
	vmUID   string
	ifaces  map[string]*types.NetworkInterfaceStatus
	// host is nil when OmitHostOnClaim is set (TD-008).
	host *poolmgr.HostRef
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL return `RESOURCE_EXHAUSTED` from `ClaimVM`
//# when the Pool has no MicroVM in the `AVAILABLE` phase.

// claimVM implements Lease.ClaimVM: after the injected claim latency it
// takes the longest-available MicroVM, runs the pre-lease hooks, creates the
// Lease and starts the strategy's replacement (TD-003). It returns
// RESOURCE_EXHAUSTED when nothing is AVAILABLE (TD-005) and NOT_FOUND for an
// unknown Pool.
func (p *PoolManager) claimVM(ctx context.Context, ref poolmgr.PoolRef) (*claimResult, error) {
	if err := p.claimLatency(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	ps, err := p.poolLocked(ref)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	vm := p.oldestAvailableLocked(ps.key)
	if vm == nil {
		p.mu.Unlock()
		return nil, status.Errorf(codes.ResourceExhausted, "no available vm in pool %s", ps.key)
	}
	p.setPhaseLocked(vm, poolmgrv1.VMPhase_PRE_LEASE_HOOK_RUNNING)
	hooks := ps.spec.PreLeaseCommands
	p.mu.Unlock()

	if err := p.runHooks(ctx, vm, poolmgr.HookPreLease, hooks); err != nil {
		p.applyHookFailurePolicy(ctx, vm, poolmgr.HookPreLease, err)
		return nil, status.Errorf(codes.Internal, "pre-lease hook: %v", err)
	}

	p.mu.Lock()
	if !p.aliveLocked(vm) {
		p.mu.Unlock()
		return nil, errNotFound("pool %s was deleted during the claim", ps.key)
	}
	now := p.cfg.Clock.Now()
	leaseID := newLeaseID()
	p.leases[leaseID] = &leaseState{rec: poolmgr.LeaseRecord{
		LeaseID:         leaseID,
		VMUID:           vm.uid,
		Pool:            ps.key.ref(),
		ClaimedAt:       now,
		LastHeartbeatAt: now,
		ExpiresAt:       now.Add(ps.spec.HeartbeatExpiryThreshold),
	}}
	vm.leaseID = leaseID
	p.setPhaseLocked(vm, poolmgrv1.VMPhase_LEASED)
	p.emitLocked(ps.key, vm.uid, poolmgrv1.EventType_VM_CLAIMED, map[string]any{"lease_id": leaseID, "host": vm.host})
	p.provisionNLocked(ps, strategyOf(ps.spec).onClaimed(), "claim")
	res := &claimResult{leaseID: leaseID, vmUID: vm.uid}
	if !p.cfg.OmitHostOnClaim {
		res.host = &poolmgr.HostRef{Name: vm.host, Address: p.hostAddress(vm.host)}
	}
	host := vm.host
	p.mu.Unlock()

	// Best effort, as in battery: the Lease exists whether or not the Host
	// answers, so a failure here only leaves the interfaces empty.
	if hc, err := p.hostClient(host); err == nil {
		if m, err := hc.GetMicroVM(ctx, res.vmUID); err == nil {
			res.ifaces = m.GetStatus().GetNetworkInterfaces()
		}
	}
	return res, nil
}

// claimLatency waits out Faults.ClaimLatency on the fake's clock (TD-010).
func (p *PoolManager) claimLatency(ctx context.Context) error {
	p.mu.Lock()
	d := p.faults.ClaimLatency
	p.mu.Unlock()
	if d <= 0 {
		return nil
	}
	timer := p.cfg.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

// heartbeat implements Lease.Heartbeat: it moves the Lease's expiry to now
// plus the Pool's threshold (TD-004) and returns it. RefuseHeartbeats makes
// every Lease look expired (TD-010).
func (p *PoolManager) heartbeat(leaseID string) (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.faults.RefuseHeartbeats {
		return time.Time{}, errNotFound("lease %s not found (fault injection: heartbeats refused)", leaseID)
	}
	ls, ok := p.leases[leaseID]
	if !ok {
		return time.Time{}, errNotFound("lease %s not found", leaseID)
	}
	ps, ok := p.pools[keyOf(ls.rec.Pool)]
	if !ok {
		return time.Time{}, errNotFound("pool %s of lease %s not found", ls.rec.Pool, leaseID)
	}
	now := p.cfg.Clock.Now()
	ls.rec.LastHeartbeatAt = now
	ls.rec.ExpiresAt = now.Add(ps.spec.HeartbeatExpiryThreshold)
	return ls.rec.ExpiresAt, nil
}

// releaseVM implements Lease.ReleaseVM: the MicroVM is deleted from its Host
// and the Lease removed. As in battery the Lease stays until the Host
// confirms the deletion; until then a retry gets UNAVAILABLE and the control
// loop keeps retrying the deletion.
func (p *PoolManager) releaseVM(ctx context.Context, leaseID string) error {
	p.mu.Lock()
	ls, ok := p.leases[leaseID]
	if !ok {
		p.mu.Unlock()
		return errNotFound("lease %s not found", leaseID)
	}
	vm := p.vmByUID[ls.rec.VMUID]
	if vm == nil {
		p.dropLeaseLocked(leaseID)
		p.mu.Unlock()
		return nil
	}
	key := keyOf(ls.rec.Pool)
	if !vm.released {
		vm.released = true
		p.emitLocked(key, vm.uid, poolmgrv1.EventType_VM_RELEASED, map[string]any{"lease_id": leaseID})
	}
	p.mu.Unlock()

	if err := p.deleteVM(ctx, vm, poolmgrv1.EventType_VM_DELETED_ON_RELEASE); err != nil {
		return status.Errorf(codes.Unavailable, "vm cleanup pending, retry later: %v", err)
	}
	return nil
}

// unavailable reports the injected UNAVAILABLE period (TD-010). Every RPC
// checks it through the server interceptors.
func (p *PoolManager) unavailable() error {
	p.mu.Lock()
	until := p.unavailableUntil
	p.mu.Unlock()
	if !until.IsZero() && p.cfg.Clock.Now().Before(until) {
		return status.Error(codes.Unavailable, "fault injection: pool manager unavailable")
	}
	return nil
}

// setPhaseLocked moves vm to phase and stamps it.
func (p *PoolManager) setPhaseLocked(vm *vmState, phase poolmgr.VMPhase) {
	vm.phase = phase
	vm.updated = p.cfg.Clock.Now()
}

// isCtxErr reports whether err is a context cancellation or deadline, which
// the provisioner treats as a shutdown rather than a Host failure.
func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		status.Code(err) == codes.Canceled || status.Code(err) == codes.DeadlineExceeded
}

// describe renders an error for logs and event payloads.
func describe(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
