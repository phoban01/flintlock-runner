package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// ResolvePlacement implements PlacementResolver. A claim that names its Host
// is resolved without a call to anything (SC-030); one that does not is
// resolved by asking the Pool's Hosts for the MicroVM (SC-031).
func (s *impl) ResolvePlacement(ctx context.Context, p *Profile, claim *poolmgr.Claim) (Placement, error) {
	if claim == nil {
		return Placement{}, &AllocationError{
			Profile: p.Name, Pool: p.PoolRef, Err: ErrPlacementUnresolved,
		}
	}
	if cached, ok := s.cachedPlacement(claim.VMUID); ok {
		return cached, nil
	}
	if claim.Host.Name != "" {
		return s.placementFromClaim(p, claim)
	}
	return s.placementFromLookup(ctx, p, claim)
}

//= docs/requirements/03-scheduler.md#placement
//# The Scheduler SHALL cache the Placement of a MicroVM for the
//# lifetime of its Lease.

// cachedPlacement returns the Placement already resolved for a MicroVM. The
// cache is written when the Allocation is created and dropped when the Lease
// is handed back, so a Placement is resolved once per Lease however often it
// is asked for, and the fan-out of SC-031 never runs twice for one MicroVM.
func (s *impl) cachedPlacement(uid string) (Placement, bool) {
	if uid == "" {
		return Placement{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pl, ok := s.placements[uid]
	return pl, ok
}

//= docs/requirements/03-scheduler.md#placement
//# When a claim succeeds and the response carries a `host` with a
//# name, the Scheduler SHALL record that name as the Placement, and if the
//# `host.address` differs from the Inventory entry of that name, then the
//# Scheduler SHALL log a warning with both values, count the mismatch and
//# continue using the Inventory endpoint.

// placementFromClaim records the Host the claim named. The address on the
// response is only checked against the Inventory: the Runner reaches Hosts
// through the Registry, which was dialled from the Inventory endpoint, so a
// disagreement is a configuration inconsistency to be reported rather than a
// reason to fail the Job or to dial the claimed address.
func (s *impl) placementFromClaim(p *Profile, claim *poolmgr.Claim) (Placement, error) {
	name := claim.Host.Name
	ep, known := s.deps.Hosts.Endpoint(name)
	if !known {
		return Placement{}, &AllocationError{
			Profile: p.Name, Pool: p.PoolRef, Host: name,
			Err: fmt.Errorf("%w: %q", ErrHostNotInInventory, name),
		}
	}
	if claim.Host.Address != "" && claim.Host.Address != ep.Address {
		s.metrics.FailureCounted(FailureAddressMismatch)
		s.log.Warn("claimed host address differs from the inventory endpoint, using the inventory endpoint",
			"host", name,
			"claimed_address", claim.Host.Address,
			"inventory_endpoint", ep.Address,
			"vm", claim.VMUID)
	}
	return Placement{Host: name, Source: PlacementFromClaim, ResolvedAt: s.clk.Now()}, nil
}

//= docs/requirements/03-scheduler.md#placement
//# When a claim succeeds and the response does not name the Host,
//# the Scheduler SHALL resolve the Placement by calling `GetMicroVM` with the
//# MicroVM's uid on each Host the Pool Manager reports for that Pool until
//# one returns it.

// placementFromLookup is the fallback for a Pool Manager that predates the
// host field on the claim response. The Hosts to ask are the Pool's
// flintlock_hosts as the Pool Manager reports them; each is asked for the
// MicroVM by uid in turn and the first Host that has it is the Placement. A
// Host that answers "not found" is simply the wrong one; a Host that cannot
// be reached is logged and skipped, because another Host may still have it.
func (s *impl) placementFromLookup(ctx context.Context, p *Profile, claim *poolmgr.Claim) (Placement, error) {
	hosts := s.poolHosts(ctx, p)
	for _, name := range hosts {
		client, err := s.deps.Hosts.Get(name)
		if err != nil {
			s.log.Warn("pool host is not in the inventory, skipping it in the placement lookup",
				"pool", p.PoolRef.String(), "host", name, "error", err)
			continue
		}
		vm, err := client.GetMicroVM(ctx, claim.VMUID)
		switch {
		case err == nil && vm != nil:
			s.log.Debug("placement resolved by asking the pool's hosts",
				"pool", p.PoolRef.String(), "host", name, "vm", claim.VMUID)
			return Placement{Host: name, Source: PlacementFromLookup, ResolvedAt: s.clk.Now()}, nil
		case errors.Is(err, flintlock.ErrNotFound):
		case err != nil:
			s.log.Warn("host could not be asked for the leased microvm",
				"pool", p.PoolRef.String(), "host", name, "vm", claim.VMUID, "error", err)
		}
		if ctx.Err() != nil {
			return Placement{}, &AllocationError{
				Profile: p.Name, Pool: p.PoolRef, Err: ctx.Err(),
			}
		}
	}
	return Placement{}, &AllocationError{
		Profile: p.Name, Pool: p.PoolRef,
		Err: fmt.Errorf("%w: no host of %d asked has microvm %s", ErrPlacementUnresolved, len(hosts), claim.VMUID),
	}
}

// poolHosts is the Host list the Pool Manager reports for a Pool. When the
// Pool Manager cannot be asked, or answers with no Host at all, the whole
// Inventory is used instead: it is a superset of the Pool's Hosts, so the
// fan-out still finds the MicroVM, and failing an otherwise good allocation
// because GetPool was unavailable would cost the Job a retry. Both fallbacks
// are logged, because both widen the fan-out beyond the Pool.
func (s *impl) poolHosts(ctx context.Context, p *Profile) []string {
	pool, err := s.deps.PoolManager.GetPool(ctx, p.PoolRef)
	if err == nil && pool != nil && len(pool.Spec.FlintlockHosts) > 0 {
		return pool.Spec.FlintlockHosts
	}
	names := s.deps.Hosts.Names()
	switch {
	case err != nil:
		s.log.Warn("pool host list could not be read, falling back to the whole inventory",
			"pool", p.PoolRef.String(), "hosts", len(names), "error", err)
	default:
		// A Pool the Pool Manager holds on no Host at all, which the claim
		// that got here contradicts. The fan-out widens to every Host in the
		// Inventory, other architectures and other Pools included, so it is
		// worth an operator knowing about.
		s.log.Warn("pool manager reports no hosts for the pool, falling back to the whole inventory",
			"pool", p.PoolRef.String(), "hosts", len(names))
	}
	return names
}
