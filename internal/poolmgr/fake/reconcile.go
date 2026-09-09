package fake

import (
	"context"
	"errors"
	"fmt"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/protobuf/proto"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL implement the immediate-on-lease,
//# minimum-size-threshold and replace-on-delete replenishment strategies as
//# declared in the `PoolSpec`.

// strategy is a Pool's replenishment strategy (TD-003). The event-driven
// part is battery's: IMMEDIATE_ON_LEASE starts one MicroVM per claim,
// REPLACE_ON_DELETE one per deletion, MIN_SIZE_THRESHOLD acts only on the
// tick. The tick additionally tops every Pool up to its target, which is how
// a fresh Pool fills and how a Pool recovers from a failed create or a
// quarantined MicroVM; battery's tick does that only for
// MIN_SIZE_THRESHOLD. The target is the idle warm size for
// IMMEDIATE_ON_LEASE, where size is headroom rather than a ceiling, and the
// total population for the other two.
type strategy struct {
	typ     poolmgr.ReplenishmentStrategyType
	size    int32
	minSize int32
}

func strategyOf(spec poolmgr.PoolSpec) strategy {
	s := strategy{typ: spec.Replenishment.Type, size: spec.Size}
	if spec.Replenishment.MinSize != nil {
		s.minSize = *spec.Replenishment.MinSize
	}
	return s
}

// onClaimed is how many MicroVMs to start after a successful claim.
func (s strategy) onClaimed() int {
	if s.typ == poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE {
		return 1
	}
	return 0
}

// onDeleted is how many MicroVMs to start after a deletion.
func (s strategy) onDeleted() int {
	if s.typ == poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE {
		return 1
	}
	return 0
}

// tickDeficit is how many MicroVMs the tick starts to reach the target.
func (s strategy) tickDeficit(c counts) int {
	var deficit int32
	switch s.typ {
	case poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE:
		deficit = s.size - (c.available + c.provisioning)
	case poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD:
		if c.available >= s.minSize {
			return 0
		}
		deficit = s.size - (c.available + c.leased + c.provisioning)
	case poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE:
		deficit = s.size - (c.available + c.leased + c.provisioning)
	default:
		return 0
	}
	if deficit < 0 {
		return 0
	}
	return int(deficit)
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL expire a Lease whose last heartbeat is older
//# than the Pool's heartbeat expiry threshold and SHALL delete the expired
//# Lease's MicroVM.

// tick is one pass of the control loop: expire and warn on Leases (TD-004),
// retry deletions a Host refused, and top every Pool up (TD-003).
func (p *PoolManager) tick(ctx context.Context) {
	now := p.cfg.Clock.Now()
	var expired, pending []*vmState

	p.mu.Lock()
	for id, ls := range p.leases {
		ps, ok := p.pools[keyOf(ls.rec.Pool)]
		if !ok {
			continue
		}
		if !ls.rec.ExpiresAt.After(now) {
			delete(p.leases, id)
			delete(ps.warned, id)
			if vm := p.vmByUID[ls.rec.VMUID]; vm != nil && vm.phase == poolmgrv1.VMPhase_LEASED {
				expired = append(expired, vm)
			}
			continue
		}
		if ls.rec.ExpiresAt.Sub(now) <= warningWindow(ps.spec) && ps.warned[id] != ls.rec.ExpiresAt.UnixNano() {
			ps.warned[id] = ls.rec.ExpiresAt.UnixNano()
			p.emitLocked(ps.key, ls.rec.VMUID, poolmgrv1.EventType_VM_EXPIRING_SOON,
				map[string]any{"lease_id": id, "expires_at": ls.rec.ExpiresAt.UTC().Format(time.RFC3339Nano)})
		}
	}
	for _, vm := range p.vms {
		if vm.phase == poolmgrv1.VMPhase_DELETING && vm.uid != "" && !vm.deleteInFlight {
			pending = append(pending, vm)
		}
	}
	for _, ps := range p.pools {
		c := p.countsLocked(ps.key)
		deficit := strategyOf(ps.spec).tickDeficit(c)
		if deficit > 0 && !ps.fresh {
			p.emitLocked(ps.key, "", poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET, map[string]any{
				"target": ps.spec.Size, "available": c.available, "leased": c.leased,
				"provisioning": c.provisioning, "quarantined": c.quarantined,
			})
		}
		ps.fresh = false
		p.provisionNLocked(ps, deficit, "tick")
	}
	p.mu.Unlock()

	for _, vm := range expired {
		p.spawn(func() {
			if err := p.deleteVM(ctx, vm, poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY); err != nil {
				p.log.Warn("expired lease: microvm deletion deferred", "uid", vm.uid, "host", vm.host, "error", err)
			}
		})
	}
	for _, vm := range pending {
		p.spawn(func() {
			// Zero, not vm.deleteEvent: the event the first attempt recorded
			// is already on the MicroVM, and reading it here would be a read
			// outside p.mu.
			if err := p.deleteVM(ctx, vm, 0); err != nil {
				p.log.Warn("microvm deletion still pending", "uid", vm.uid, "host", vm.host, "error", err)
			}
		})
	}
}

// warningWindow is how long before expiry VM_EXPIRING_SOON fires: one
// heartbeat interval, or half the threshold when the Pool declares none.
func warningWindow(spec poolmgr.PoolSpec) time.Duration {
	if spec.HeartbeatInterval > 0 {
		return spec.HeartbeatInterval
	}
	return spec.HeartbeatExpiryThreshold / 2
}

// spawn runs fn on a tracked goroutine so that Run can wait for it.
func (p *PoolManager) spawn(fn func()) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		fn()
	}()
}

// provisionNLocked reserves n MicroVMs in ps, placing each (TD-007), and
// starts their provisioning. Nothing happens before Run or after it stops;
// the first tick catches up. It emits POOL_REPLENISHING when it starts any.
func (p *PoolManager) provisionNLocked(ps *poolState, n int, reason string) {
	if n <= 0 || p.runCtx == nil || p.runCtx.Err() != nil {
		return
	}
	var reserved []*vmState
	for i := 0; i < n; i++ {
		host, err := p.pickHostLocked(ps)
		if err != nil {
			p.log.Warn("cannot place microvm", "pool", ps.key, "error", err)
			break
		}
		p.nextVM++
		now := p.cfg.Clock.Now()
		vm := &vmState{id: p.nextVM, pool: ps.key, host: host, phase: poolmgrv1.VMPhase_PROVISIONING, createdAt: now, updated: now}
		p.vms[vm.id] = vm
		reserved = append(reserved, vm)
	}
	if len(reserved) == 0 {
		return
	}
	p.emitLocked(ps.key, "", poolmgrv1.EventType_POOL_REPLENISHING, map[string]any{"count": len(reserved), "reason": reason})
	ctx := p.runCtx
	for _, vm := range reserved {
		p.spawn(func() { p.provision(ctx, vm) })
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL create, place and delete MicroVMs through the
//# same Host client interface the Runner uses, so that it can be pointed at
//# fake Hosts or at real `flintlockd` instances.

// provision is battery's pipeline for one reserved MicroVM: CreateMicroVM
// on the placed Host through its flintlock.PoolHostClient (TD-002), wait for
// CREATED, run the create hooks, mark it AVAILABLE. Every failure after the
// create goes through the Pool's hook failure policy.
func (p *PoolManager) provision(ctx context.Context, vm *vmState) {
	hc, err := p.hostClient(vm.host)
	if err != nil {
		p.dropReserved(vm, err)
		return
	}
	p.mu.Lock()
	ps, ok := p.pools[vm.pool]
	if !ok {
		p.mu.Unlock()
		p.dropReserved(vm, errors.New("pool deleted"))
		return
	}
	spec := proto.Clone(ps.spec.Template).(*types.MicroVMSpec)
	hooks := ps.spec.CreateCommands
	p.mu.Unlock()

	created, err := hc.CreateMicroVM(ctx, spec)
	if err != nil {
		p.dropReserved(vm, fmt.Errorf("create microvm on %s: %w", vm.host, err))
		return
	}
	uid := created.GetSpec().GetUid()
	if uid == "" {
		p.dropReserved(vm, fmt.Errorf("create microvm on %s: host returned no uid", vm.host))
		return
	}

	p.mu.Lock()
	if !p.aliveLocked(vm) {
		p.mu.Unlock()
		p.deleteOrphan(hc, uid)
		return
	}
	vm.uid = uid
	p.vmByUID[uid] = vm
	vm.updated = p.cfg.Clock.Now()
	p.emitLocked(vm.pool, uid, poolmgrv1.EventType_VM_PROVISIONED, map[string]any{"host": vm.host})
	p.mu.Unlock()

	if err := p.waitCreated(ctx, hc, uid); err != nil {
		p.applyHookFailurePolicy(ctx, vm, poolmgr.HookCreate, err)
		return
	}
	if !p.transition(vm, poolmgrv1.VMPhase_CREATE_HOOK_RUNNING) {
		p.deleteOrphan(hc, uid)
		return
	}
	if err := p.runHooks(ctx, vm, poolmgr.HookCreate, hooks); err != nil {
		p.applyHookFailurePolicy(ctx, vm, poolmgr.HookCreate, err)
		return
	}
	p.mu.Lock()
	if !p.aliveLocked(vm) {
		p.mu.Unlock()
		p.deleteOrphan(hc, uid)
		return
	}
	p.setPhaseLocked(vm, poolmgrv1.VMPhase_AVAILABLE)
	p.emitLocked(vm.pool, uid, poolmgrv1.EventType_VM_AVAILABLE, map[string]any{"host": vm.host})
	p.mu.Unlock()
}

// transition moves vm to phase if it is still alive and reports whether it
// was.
func (p *PoolManager) transition(vm *vmState, phase poolmgr.VMPhase) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.aliveLocked(vm) {
		return false
	}
	p.setPhaseLocked(vm, phase)
	return true
}

// dropReserved forgets a reservation whose CreateMicroVM never produced a
// MicroVM. The next tick provisions again, so a Host that keeps failing is
// retried at the reconcile interval rather than in a loop.
func (p *PoolManager) dropReserved(vm *vmState, cause error) {
	p.mu.Lock()
	delete(p.vms, vm.id)
	p.mu.Unlock()
	if !isCtxErr(cause) {
		p.log.Warn("provisioning failed before the microvm existed", "pool", vm.pool, "host", vm.host, "error", cause)
	}
}

// deleteOrphan deletes a MicroVM whose Pool disappeared or whose record was
// removed while it was being provisioned.
func (p *PoolManager) deleteOrphan(hc flintlock.PoolHostClient, uid string) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := hc.DeleteMicroVM(ctx, uid); err != nil && !errors.Is(err, flintlock.ErrNotFound) {
		p.log.Warn("orphaned microvm not deleted", "uid", uid, "host", hc.Name(), "error", err)
	}
}

// waitCreated polls GetMicroVM until the MicroVM is CREATED, or fails when
// it is FAILED or cfg.ReadyTimeout passes on the fake's clock. The first
// check is immediate.
func (p *PoolManager) waitCreated(ctx context.Context, hc flintlock.PoolHostClient, uid string) error {
	deadline := p.cfg.Clock.Now().Add(p.cfg.ReadyTimeout)
	for {
		m, err := hc.GetMicroVM(ctx, uid)
		if err != nil {
			return fmt.Errorf("get microvm %s: %w", uid, err)
		}
		switch m.GetStatus().GetState() {
		case types.MicroVMStatus_CREATED:
			return nil
		case types.MicroVMStatus_FAILED:
			return fmt.Errorf("microvm %s failed to create", uid)
		case types.MicroVMStatus_PENDING, types.MicroVMStatus_DELETING:
		}
		if !p.cfg.Clock.Now().Before(deadline) {
			return fmt.Errorf("microvm %s not created within %s", uid, p.cfg.ReadyTimeout)
		}
		timer := p.cfg.Clock.NewTimer(createPollInterval)
		select {
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// applyHookFailurePolicy is battery's ApplyHookFailurePolicy: QUARANTINE
// keeps the MicroVM for inspection, anything else deletes it; both emit
// VM_HOOK_FAILED. Cleanup runs detached from ctx so a cancelled claim cannot
// strand a MicroVM in a transient phase.
func (p *PoolManager) applyHookFailurePolicy(ctx context.Context, vm *vmState, hook poolmgr.HookKind, cause error) {
	p.mu.Lock()
	if !p.aliveLocked(vm) {
		p.mu.Unlock()
		return
	}
	policy := p.pools[vm.pool].spec.HookFailurePolicy
	vm.leaseID = ""
	p.emitLocked(vm.pool, vm.uid, poolmgrv1.EventType_VM_HOOK_FAILED,
		map[string]any{"hook": string(hook), "error": describe(cause), "policy": policy.String()})
	if policy == poolmgrv1.HookFailurePolicy_QUARANTINE {
		p.setPhaseLocked(vm, poolmgrv1.VMPhase_QUARANTINED)
		p.mu.Unlock()
		p.log.Warn("hook failed; microvm quarantined", "hook", hook, "uid", vm.uid, "pool", vm.pool, "error", cause)
		return
	}
	// Leave the provisioning count in the same critical section as the
	// event, so a tick that runs between here and the Host call already
	// sees the deficit.
	p.setPhaseLocked(vm, poolmgrv1.VMPhase_DELETING)
	p.mu.Unlock()
	p.log.Warn("hook failed; microvm deleted", "hook", hook, "uid", vm.uid, "pool", vm.pool, "error", cause)

	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := p.deleteVM(cleanup, vm, 0); err != nil {
		p.log.Warn("hook failure cleanup deferred", "uid", vm.uid, "host", vm.host, "error", err)
	}
}

// errDeletionInFlight is what deleteVM reports to a caller that asks for a
// deletion the fake has already put on the Host and is still waiting for.
// The caller does nothing: the attempt in flight finishes the deletion, or
// fails and leaves it to the tick.
var errDeletionInFlight = errors.New("fake poolmgr: deletion already in flight")

// deleteVM marks vm DELETING, deletes it on its Host and, when the Host
// confirms, finishes the deletion with event as the VM_DELETED_* event to
// emit (zero to keep the one already recorded). On a Host error the MicroVM
// stays DELETING and the tick retries. Only one attempt per MicroVM is in
// flight at a time, so a Host that never answers cannot accumulate calls or
// goroutines; a caller that arrives while one is in flight gets
// errDeletionInFlight. Not called with p.mu held.
func (p *PoolManager) deleteVM(ctx context.Context, vm *vmState, event poolmgrv1.EventType) error {
	p.mu.Lock()
	if _, ok := p.vms[vm.id]; !ok {
		p.mu.Unlock()
		return nil
	}
	p.setPhaseLocked(vm, poolmgrv1.VMPhase_DELETING)
	if event != 0 {
		vm.deleteEvent = event
	}
	if vm.deleteInFlight {
		p.mu.Unlock()
		return fmt.Errorf("delete microvm %s on %s: %w", vm.uid, vm.host, errDeletionInFlight)
	}
	vm.deleteInFlight = true
	host, uid := vm.host, vm.uid
	p.mu.Unlock()

	err := p.deleteOnHost(ctx, host, uid)

	p.mu.Lock()
	defer p.mu.Unlock()
	vm.deleteInFlight = false
	if err != nil {
		return err
	}
	p.finishDeletionLocked(vm)
	return nil
}

// deleteOnHost is the Host half of a deletion. A MicroVM the Host no longer
// knows about counts as deleted.
func (p *PoolManager) deleteOnHost(ctx context.Context, host, uid string) error {
	if uid == "" {
		return nil
	}
	hc, err := p.hostClient(host)
	if err != nil {
		return err
	}
	if err := hc.DeleteMicroVM(ctx, uid); err != nil && !errors.Is(err, flintlock.ErrNotFound) {
		return fmt.Errorf("delete microvm %s on %s: %w", uid, host, err)
	}
	return nil
}

// finishDeletionLocked removes a deleted MicroVM and its Lease, emits its
// VM_DELETED_* event and starts the strategy's replacement.
func (p *PoolManager) finishDeletionLocked(vm *vmState) {
	if _, ok := p.vms[vm.id]; !ok {
		return
	}
	delete(p.vms, vm.id)
	if vm.uid != "" {
		delete(p.vmByUID, vm.uid)
	}
	payload := map[string]any{"host": vm.host}
	if vm.leaseID != "" {
		delete(p.leases, vm.leaseID)
		payload["lease_id"] = vm.leaseID
	}
	ps, ok := p.pools[vm.pool]
	if !ok {
		return
	}
	if vm.deleteEvent != 0 {
		p.emitLocked(vm.pool, vm.uid, vm.deleteEvent, payload)
	}
	p.provisionNLocked(ps, strategyOf(ps.spec).onDeleted(), "vm deleted")
}
