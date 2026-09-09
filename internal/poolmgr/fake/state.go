package fake

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// poolKey identifies a Pool by namespace and name.
type poolKey struct {
	name, namespace string
}

func keyOf(ref poolmgr.PoolRef) poolKey { return poolKey{name: ref.Name, namespace: ref.Namespace} }

func (k poolKey) ref() poolmgr.PoolRef { return poolmgr.PoolRef{Name: k.name, Namespace: k.namespace} }

func (k poolKey) String() string { return k.namespace + "/" + k.name }

// poolState is one declared Pool.
type poolState struct {
	key  poolKey
	spec poolmgr.PoolSpec
	// nextEvent is the per-Pool monotonic event id (proto Event.id).
	nextEvent int64
	// rr is the round-robin cursor (PlacementRoundRobin).
	rr int
	// fresh is true between a create or update and the next tick; the
	// initial fill of a fresh Pool is not reported as POOL_SIZE_BELOW_TARGET.
	fresh bool
	// warned records, per lease id, the expiry for which VM_EXPIRING_SOON
	// was emitted; a heartbeat moves the expiry and re-arms the warning.
	warned map[string]int64
}

// vmState is one MicroVM the fake manages.
type vmState struct {
	id      int64
	uid     string
	pool    poolKey
	host    string
	phase   poolmgr.VMPhase
	leaseID string
	// deleteEvent is the VM_DELETED_* event to emit once the Host confirms
	// the deletion, or zero for a deletion that has no event of its own.
	deleteEvent poolmgrv1.EventType
	// released is set once VM_RELEASED has been emitted for this MicroVM, so
	// a retried ReleaseVM does not emit it twice.
	released           bool
	createdAt, updated time.Time
}

// leaseState is one outstanding Lease.
type leaseState struct {
	rec poolmgr.LeaseRecord
}

// counts is a Pool's population by phase, as PoolStatus reports it.
type counts struct {
	available, leased, provisioning, quarantined int32
}

func (c counts) status() poolmgr.PoolStatus {
	return poolmgr.PoolStatus{Available: c.available, Leased: c.leased, Provisioning: c.provisioning, Quarantined: c.quarantined}
}

// countsLocked summarises a Pool's MicroVMs the way battery's CountVMs does:
// PRE_LEASE_HOOK_RUNNING counts as leased, CREATE_HOOK_RUNNING as
// provisioning, and DELETING and FAILED count nowhere.
func (p *PoolManager) countsLocked(key poolKey) counts {
	var c counts
	for _, vm := range p.vms {
		if vm.pool != key {
			continue
		}
		switch vm.phase {
		case poolmgrv1.VMPhase_AVAILABLE:
			c.available++
		case poolmgrv1.VMPhase_LEASED, poolmgrv1.VMPhase_PRE_LEASE_HOOK_RUNNING:
			c.leased++
		case poolmgrv1.VMPhase_PROVISIONING, poolmgrv1.VMPhase_CREATE_HOOK_RUNNING:
			c.provisioning++
		case poolmgrv1.VMPhase_QUARANTINED:
			c.quarantined++
		case poolmgrv1.VMPhase_DELETING, poolmgrv1.VMPhase_FAILED:
		}
	}
	return c
}

// poolLocked resolves ref, enforcing the configured namespace restriction.
func (p *PoolManager) poolLocked(ref poolmgr.PoolRef) (*poolState, error) {
	if err := p.checkNamespace(ref.Namespace); err != nil {
		return nil, err
	}
	ps, ok := p.pools[keyOf(ref)]
	if !ok {
		return nil, errNotFound("pool %s not found", ref)
	}
	return ps, nil
}

// checkNamespace rejects a namespace other than cfg.Namespace when one is
// configured (PL-017 in the harness).
func (p *PoolManager) checkNamespace(ns string) error {
	if p.cfg.Namespace != "" && ns != p.cfg.Namespace {
		return errInvalid("namespace %q is not served by this pool manager (only %q)", ns, p.cfg.Namespace)
	}
	return nil
}

// aliveLocked reports whether vm is still tracked and its Pool still exists.
// Provisioning re-checks it after every Host call, because the Pool may have
// been deleted or the fake stopped in the meantime.
func (p *PoolManager) aliveLocked(vm *vmState) bool {
	if _, ok := p.vms[vm.id]; !ok {
		return false
	}
	_, ok := p.pools[vm.pool]
	return ok
}

// oldestAvailableLocked picks the AVAILABLE MicroVM of a Pool that has been
// available longest, for deterministic claims.
func (p *PoolManager) oldestAvailableLocked(key poolKey) *vmState {
	var best *vmState
	for _, vm := range p.vms {
		if vm.pool != key || vm.phase != poolmgrv1.VMPhase_AVAILABLE {
			continue
		}
		if best == nil || vm.updated.Before(best.updated) || (vm.updated.Equal(best.updated) && vm.id < best.id) {
			best = vm
		}
	}
	return best
}

// pickHostLocked chooses the Host for a new MicroVM in ps (TD-007). Only
// names the HostSource knows are candidates. PlacementLeastVMs picks the
// candidate with the fewest MicroVMs of this Pool that are not DELETING or
// FAILED, ties broken by the order of flintlock_hosts, which is battery's
// PickHost; PlacementRoundRobin cycles through the candidates.
func (p *PoolManager) pickHostLocked(ps *poolState) (string, error) {
	var candidates []string
	for _, name := range ps.spec.FlintlockHosts {
		if _, err := p.cfg.Hosts.Host(name); err == nil {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("pool %s has no known host among flintlock_hosts %v: %w", ps.key, ps.spec.FlintlockHosts, flintlock.ErrUnknownHost)
	}
	if p.cfg.Placement == poolmgr.PlacementRoundRobin {
		host := candidates[ps.rr%len(candidates)]
		ps.rr++
		return host, nil
	}
	perHost := make(map[string]int, len(candidates))
	for _, vm := range p.vms {
		if vm.pool != ps.key {
			continue
		}
		switch vm.phase {
		case poolmgrv1.VMPhase_DELETING, poolmgrv1.VMPhase_FAILED:
			continue
		default:
			perHost[vm.host]++
		}
	}
	best := candidates[0]
	for _, h := range candidates[1:] {
		if perHost[h] < perHost[best] {
			best = h
		}
	}
	return best, nil
}

// hostClient resolves a Host name through the HostSource.
func (p *PoolManager) hostClient(name string) (flintlock.PoolHostClient, error) {
	hc, err := p.cfg.Hosts.Host(name)
	if err != nil {
		return nil, fmt.Errorf("host %q: %w", name, err)
	}
	return hc, nil
}

// hostAddress returns the flintlockd address of a Host when the HostSource
// knows it (HostAddresser), for HostInfo.address on ClaimVMResponse (TD-008).
func (p *PoolManager) hostAddress(name string) string {
	if a, ok := p.cfg.Hosts.(HostAddresser); ok {
		if addr, known := a.Address(name); known {
			return addr
		}
	}
	return ""
}

// vmRecordsLocked converts every MicroVM to its Inspector record in creation
// order.
func (p *PoolManager) vmRecordsLocked() []poolmgr.VMRecord {
	vms := make([]*vmState, 0, len(p.vms))
	for _, vm := range p.vms {
		vms = append(vms, vm)
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].id < vms[j].id })
	out := make([]poolmgr.VMRecord, 0, len(vms))
	for _, vm := range vms {
		out = append(out, poolmgr.VMRecord{
			UID:       vm.uid,
			Pool:      vm.pool.ref(),
			Host:      vm.host,
			Phase:     vm.phase,
			LeaseID:   vm.leaseID,
			CreatedAt: vm.createdAt,
			UpdatedAt: vm.updated,
		})
	}
	return out
}

func sortLeases(ls []poolmgr.LeaseRecord) {
	sort.Slice(ls, func(i, j int) bool {
		if !ls[i].ClaimedAt.Equal(ls[j].ClaimedAt) {
			return ls[i].ClaimedAt.Before(ls[j].ClaimedAt)
		}
		return ls[i].LeaseID < ls[j].LeaseID
	})
}

// newLeaseID returns an opaque lease token.
func newLeaseID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("fake poolmgr: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}
