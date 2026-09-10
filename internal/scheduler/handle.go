package scheduler

import (
	"context"
	"sync"
)

// handle is the live Allocation behind the Handle interface. Done is closed
// exactly once, by fail, and Err is set before it closes so that a reader
// woken by Done always sees the cause.
type handle struct {
	id   uint64
	done chan struct{}
	once sync.Once

	// The identifiers of an Allocation never change once it exists, so they
	// are copied out of alloc at newHandle and read without any lock. The
	// Scheduler's own accounting needs them while it holds s.mu, and reaching
	// into alloc for them there would be a read of mu-guarded state under the
	// wrong lock.
	vmUID     string
	profile   string
	placement Placement

	mu    sync.Mutex
	alloc Allocation
	err   error
	// leaseGone is set when the Lease no longer exists at the Pool Manager,
	// so that no release call is made for it (SC-061, SC-062).
	leaseGone bool
	// stopHeartbeat ends the keep-alive loop.
	stopHeartbeat context.CancelFunc
}

// newHandle builds a handle for an Allocation.
func newHandle(id uint64, alloc Allocation) *handle {
	return &handle{
		id:        id,
		done:      make(chan struct{}),
		vmUID:     alloc.VMUID,
		profile:   alloc.Profile,
		placement: alloc.Placement,
		alloc:     alloc,
	}
}

// Allocation implements Handle. The value is a copy; the Lease expiry moves
// as heartbeats are answered, so callers that care read it again.
func (h *handle) Allocation() Allocation {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.alloc
}

// Done implements Handle.
func (h *handle) Done() <-chan struct{} { return h.done }

// Err implements Handle.
func (h *handle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// fail records why the Job can no longer run and closes Done. Only the first
// call has an effect, so the first cause wins.
func (h *handle) fail(err error) {
	h.once.Do(func() {
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
		close(h.done)
	})
}

// setLease records the Lease expiry a heartbeat returned (PL-040).
func (h *handle) setLease(l Lease) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alloc.Lease = l
}

// lease reads the current Lease.
func (h *handle) lease() Lease {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.alloc.Lease
}

// markLeaseGone records that the Lease no longer exists, so that Release
// makes no call for it.
func (h *handle) markLeaseGone() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leaseGone = true
}

// leaseIsGone reports whether the Lease is known to be gone.
func (h *handle) leaseIsGone() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.leaseGone
}

// placementHost is the Host of the Allocation's Placement. The Placement is
// fixed at newHandle, so this is safe to call with the Scheduler's lock held.
func (h *handle) placementHost() string { return h.placement.Host }
