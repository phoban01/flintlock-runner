package fake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// HostAddresser is implemented by a poolmgr.HostSource that knows each
// Host's flintlockd address. The fake reports it as HostInfo.address on
// ClaimVMResponse (TD-008); a HostSource without it leaves the address
// empty and the Scheduler falls back to the Inventory entry for the name.
type HostAddresser interface {
	Address(name string) (string, bool)
}

// Hosts is a static poolmgr.HostSource: a set of named
// flintlock.PoolHostClient values, each with the address it was dialled
// with. The harness fills it with fake Hosts and the standalone binary with
// real flintlockd endpoints (TD-002); the fake sees no difference. It is
// safe for concurrent use.
type Hosts struct {
	mu      sync.RWMutex
	entries map[string]hostEntry
}

type hostEntry struct {
	client  flintlock.PoolHostClient
	address string
}

// NewHosts returns an empty Hosts.
func NewHosts() *Hosts { return &Hosts{entries: make(map[string]hostEntry)} }

// HostsFromDialer dials every endpoint through d and returns the resulting
// Hosts. On failure it closes the clients it already has.
func HostsFromDialer(ctx context.Context, d flintlock.AdminDialer, endpoints []flintlock.Endpoint) (*Hosts, error) {
	h := NewHosts()
	for _, ep := range endpoints {
		c, err := d.DialAdmin(ctx, ep)
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("host %q: %w", ep.Name, err)
		}
		h.Add(ep.Name, c, ep.Address)
	}
	return h, nil
}

// Add registers or replaces a Host. The address may be empty.
func (h *Hosts) Add(name string, client flintlock.PoolHostClient, address string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries[name] = hostEntry{client: client, address: address}
}

// Remove forgets a Host without closing its client.
func (h *Hosts) Remove(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.entries, name)
}

// Host implements poolmgr.HostSource.
func (h *Hosts) Host(name string) (flintlock.PoolHostClient, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", flintlock.ErrUnknownHost, name)
	}
	return e.client, nil
}

// Names implements poolmgr.HostSource; the result is sorted.
func (h *Hosts) Names() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, 0, len(h.entries))
	for n := range h.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Address implements HostAddresser.
func (h *Hosts) Address(name string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	return e.address, ok
}

// Close closes every client and returns the first error.
func (h *Hosts) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var errs []error
	for name, e := range h.entries {
		if err := e.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("host %q: %w", name, err))
		}
		delete(h.entries, name)
	}
	return errors.Join(errs...)
}

// Compile-time interface checks.
var (
	_ poolmgr.HostSource = (*Hosts)(nil)
	_ HostAddresser      = (*Hosts)(nil)
)
