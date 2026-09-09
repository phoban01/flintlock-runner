package scheduler

import (
	"context"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/03-scheduler.md#capacity
//# The Scheduler SHALL maintain a count of held Reservations and
//# active Allocations and SHALL treat their sum as the number of Slots in use.

// slotsInUseLocked is the number of Slots in use: one per held Reservation
// that has not been converted plus one per live Allocation. A Reservation
// that Allocate converted is no longer in the Reservation map and its
// Allocation is in the Allocation map, so the sum does not change across a
// conversion (SC-026).
func (s *impl) slotsInUseLocked() int {
	return len(s.reservations) + len(s.allocations)
}

//= docs/requirements/03-scheduler.md#capacity
//# The Scheduler SHALL count a Pool's MicroVMs that are being
//# provisioned or running create hooks as unavailable when computing
//# capacity.

// warmAvailable is the sum of the available warm MicroVMs across every Pool.
// Only PoolStatus.Available is summed: a MicroVM the Pool Manager reports as
// provisioning, which is also how it reports one that is still running its
// create hooks, and one that is quarantined are both left out, because
// neither can be claimed. A Pool that is not declared counts as empty
// (PL-016), and while the Pool Manager is unhealthy every Pool counts as
// empty (PL-035).
func (s *impl) warmAvailable() int {
	if !s.deps.Health.Healthy() {
		return 0
	}
	total := 0
	for _, pa := range s.deps.Tracker.Pools() {
		if !pa.Declared {
			continue
		}
		if pa.Status.Available > 0 {
			total += int(pa.Status.Available)
		}
	}
	return total
}

//= docs/requirements/03-scheduler.md#capacity
//# The Scheduler SHALL compute available capacity as the lesser of
//# the number of unused Slots and the sum of the number of available warm
//# MicroVMs across all Pools.

//= docs/requirements/03-scheduler.md#host-health
//# The Scheduler SHALL NOT refuse Reservations because a Host is
//# unhealthy, because the Pool Manager decides where warm MicroVMs live.

// availableCapacityLocked is the number of Reservations that may still be
// granted: the lesser of the unused Slots and the warm MicroVMs the Pool
// Manager has. Host health is deliberately not an input, because the Pool
// Manager places warm MicroVMs and a Host being unhealthy neither frees nor
// removes a Slot and does not make a warm MicroVM unclaimable. The refusal
// reason names whichever of the two terms was zero.
func (s *impl) availableCapacityLocked(warm int) (int, RefusalReason) {
	free := s.set.Slots - s.slotsInUseLocked()
	if free <= 0 {
		return 0, RefusalNoFreeSlot
	}
	if warm <= 0 {
		return 0, RefusalNoWarmMicroVM
	}
	if warm < free {
		return warm, ""
	}
	return free, ""
}

//= docs/requirements/03-scheduler.md#capacity
//# When a Reservation is requested and available capacity is
//# greater than zero, the Scheduler SHALL grant a Reservation and count it as
//# a Slot in use.

//= docs/requirements/03-scheduler.md#capacity
//# When a Reservation is requested and available capacity is zero,
//# the Scheduler SHALL refuse the Reservation.

// Reserve implements Reserver. It does not block: capacity comes from the
// Tracker's last known counts, and the Slot is taken under the same hold of
// the lock that counted it, so two workers cannot both take the last Slot.
func (s *impl) Reserve(ctx context.Context) (*Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch s.runningState() {
	case stateStopping:
		return nil, s.refuse(RefusalShuttingDown)
	case stateNew, stateStopped:
		return nil, ErrNotRunning
	case stateRunning:
	}
	if !s.contacted() {
		return nil, s.refuse(RefusalPoolManagerNotContacted)
	}
	r, reason := s.grant(s.warmAvailable())
	if r == nil {
		return nil, s.refuse(reason)
	}
	return r, nil
}

// grant takes a Slot when available capacity is greater than zero and
// returns the Reservation, or the reason it was refused.
func (s *impl) grant(warm int) (*Reservation, RefusalReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, reason := s.availableCapacityLocked(warm); n <= 0 {
		return nil, reason
	}
	s.nextID++
	r := &Reservation{ID: s.nextID, GrantedAt: s.clk.Now()}
	s.reservations[r.ID] = r.GrantedAt
	return r, ""
}

// runningState reads the lifecycle state.
func (s *impl) runningState() runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// refuse counts and logs a refusal and returns it (OB-003, OB-015).
func (s *impl) refuse(reason RefusalReason) error {
	if reason == "" {
		reason = RefusalNoWarmMicroVM
	}
	s.metrics.ReservationRefused(reason)
	if !s.throttled("refusal:" + string(reason)) {
		s.log.Info("reservation refused", "reason", string(reason))
	}
	return &Refusal{Reason: reason}
}

//= docs/requirements/03-scheduler.md#capacity
//# When a Reservation is released without an Allocation, the
//# Scheduler SHALL return its Slot immediately.

// ReleaseReservation implements Reserver. The Slot is returned before the
// call returns, with no I/O in between. Releasing a Reservation that was
// already released, or that Allocate converted into an Allocation, is a
// no-op: the Slot then belongs to the Allocation and Release returns it.
func (s *impl) ReleaseReservation(r *Reservation) {
	if r == nil {
		return
	}
	s.mu.Lock()
	_, held := s.reservations[r.ID]
	delete(s.reservations, r.ID)
	s.mu.Unlock()
	if held {
		s.log.Debug("reservation released without an allocation", "reservation", r.ID)
	}
}

//= docs/requirements/03-scheduler.md#allocation
//# The Scheduler SHALL convert a Reservation into an Allocation
//# without releasing the Slot in between.

// convert exchanges the Reservation for the Allocation under one hold of the
// lock, so the sum of held Reservations and live Allocations that
// slotsInUseLocked reports never dips and no other worker can take the Slot
// in between. It reports false when the Reservation is no longer held, which
// happens when the run loop released it while the allocation was in flight;
// the caller then hands the MicroVM back.
func (s *impl) convert(r *Reservation, h *handle) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reservations[r.ID]; !ok {
		return false
	}
	delete(s.reservations, r.ID)
	s.allocations[h.id] = h
	s.placements[h.vmUID] = h.placement
	return true
}

// finish removes an Allocation from the accounting: the Slot and the
// Profile's concurrency unit are returned and the cached Placement is
// dropped. It reports whether this call was the one that ended the
// Allocation, so that a Lease is never released twice.
func (s *impl) finish(h *handle) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.allocations[h.id]; !ok {
		return false
	}
	delete(s.allocations, h.id)
	delete(s.placements, h.vmUID)
	s.releaseProfileLocked(h.profile)
	if len(s.allocations) == 0 && s.drain != nil {
		close(s.drain)
		s.drain = nil
	}
	return true
}

// drained returns a channel that is closed when the last Allocation ends. It
// is already closed when there is no Allocation.
func (s *impl) drained() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.allocations) == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if s.drain == nil {
		s.drain = make(chan struct{})
	}
	return s.drain
}

//= docs/requirements/03-scheduler.md#capacity
//# Where a Profile has a configured maximum concurrency, the
//# Scheduler SHALL NOT hold more Allocations for that Profile than the
//# maximum.

// enterProfile takes one of the Profile's concurrency units when the Profile
// has a maximum and it is not reached. The unit is taken before the claim, so
// two allocations for the same Profile can never both claim a MicroVM past
// the maximum, and it is returned by leaveProfile when the allocation fails
// or by finish when the Allocation ends. The second return value is a channel
// closed the next time any unit of any Profile is returned, so a caller that
// was refused waits rather than polls. A Profile with no configured maximum
// always admits.
func (s *impl) enterProfile(p *Profile) (bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	free := s.profileFree
	if p.MaxConcurrency > 0 && s.profileUse[p.Name] >= p.MaxConcurrency {
		return false, free
	}
	s.profileUse[p.Name]++
	return true, free
}

// leaveProfile returns a concurrency unit taken by enterProfile for an
// allocation that did not become an Allocation.
func (s *impl) leaveProfile(p *Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseProfileLocked(p.Name)
}

// releaseProfileLocked returns one concurrency unit of a Profile and wakes
// everything waiting on the limit.
func (s *impl) releaseProfileLocked(name string) {
	if n := s.profileUse[name]; n > 1 {
		s.profileUse[name] = n - 1
	} else {
		delete(s.profileUse, name)
	}
	close(s.profileFree)
	s.profileFree = make(chan struct{})
}

// waitOnPool adds delta to the number of allocations waiting on a Pool and
// returns the new count (OB-004, OB-021).
func (s *impl) waitOnPool(ref poolmgr.PoolRef, delta int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.waiting[ref] + delta
	if n <= 0 {
		delete(s.waiting, ref)
		return 0
	}
	s.waiting[ref] = n
	return n
}
