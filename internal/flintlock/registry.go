package flintlock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// RegistryOption configures a Registry built by NewRegistry.
type RegistryOption func(*registry)

// WithRegistryLogger sets the logger the Registry reports reloads on. The
// default discards.
func WithRegistryLogger(log *slog.Logger) RegistryOption {
	return func(r *registry) {
		if log != nil {
			r.log = log
		}
	}
}

// NewRegistry dials every Endpoint and returns the live set of Hosts keyed
// by Host name (HO-010). A Host that cannot be dialled at all, because its
// TLS material is unreadable or its entry is incomplete, fails the call:
// dialling does not wait for the Host to answer, so the only failures here
// are local configuration ones. Close releases every connection.
func NewRegistry(ctx context.Context, dialer Dialer, endpoints []Endpoint, opts ...RegistryOption) (Registry, error) {
	if dialer == nil {
		return nil, errors.New("flintlock: registry needs a dialer")
	}
	r := &registry{
		dialer: dialer,
		log:    discardLogger(),
		hosts:  make(map[string]*hostEntry, len(endpoints)),
	}
	for _, o := range opts {
		o(r)
	}
	if err := r.Apply(ctx, endpoints); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// hostEntry is one Host's client together with the Endpoint it was dialled
// with, and the bookkeeping that decides when the client may be closed.
//
// leases counts the callers that took the client with Lease and have not
// released it: they are the Jobs a reload must not cut off (HO-014).
// retired says a reload has dropped this entry from the Inventory, so the
// client is wanted only for as long as those leases last.
type hostEntry struct {
	ep     Endpoint
	client HostClient

	leases  int
	retired bool
	closed  bool
}

// releasableLocked returns the client to close when a retired entry has run
// out of holders, and nil otherwise. The caller holds r.mu and closes the
// returned client after releasing it, because Close talks to the network.
func (e *hostEntry) releasableLocked() HostClient {
	if !e.retired || e.closed || e.leases > 0 {
		return nil
	}
	e.closed = true
	return e.client
}

// registry is the production Registry.
type registry struct {
	dialer Dialer
	log    *slog.Logger

	mu sync.RWMutex
	// hosts is the live Inventory, keyed by the Host name the Pool Manager
	// uses in flintlock_hosts (HO-010).
	hosts map[string]*hostEntry
	// retired holds the entries of Hosts a reload removed that still have a
	// lease on them: they are not closed, because a Job holding one has to
	// finish on it (HO-014). An entry leaves this list when its last lease
	// is released, and Close releases whatever is left.
	retired []*hostEntry
	closed  bool
}

// Get implements Registry.
func (r *registry) Get(name string) (HostClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.hosts[name]
	if !ok {
		return nil, fmt.Errorf("flintlock: %q: %w", name, ErrUnknownHost)
	}
	return e.client, nil
}

//= docs/requirements/05-hosts.md#inventory
//# When the configuration is reloaded, the Runner SHALL add new
//# Hosts, stop probing removed Hosts and keep serving Jobs on removed Hosts
//# until they finish.

// Lease implements Registry: it hands out the client and counts the caller
// as a holder of it until the returned function is called. A reload that
// removes the Host leaves a leased client open, so the Job running on it
// finishes there; the connection goes when the last holder lets go, which
// is what stops reloads accumulating connections to Hosts that have left
// the Inventory.
func (r *registry) Lease(name string) (HostClient, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.hosts[name]
	if !ok {
		return nil, nil, fmt.Errorf("flintlock: %q: %w", name, ErrUnknownHost)
	}
	e.leases++
	var once sync.Once
	return e.client, func() { once.Do(func() { r.release(e) }) }, nil
}

// release gives up one lease on an entry and closes its client when that
// was the last thing holding a retired entry open.
func (r *registry) release(e *hostEntry) {
	r.mu.Lock()
	e.leases--
	client := e.releasableLocked()
	if client != nil {
		r.forgetRetiredLocked(e)
	}
	r.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

// forgetRetiredLocked drops a closed entry from the retired list so that it
// is not closed twice by Close. The caller holds r.mu.
func (r *registry) forgetRetiredLocked(e *hostEntry) {
	for i, other := range r.retired {
		if other == e {
			r.retired = append(r.retired[:i], r.retired[i+1:]...)
			return
		}
	}
}

// Endpoint implements Registry.
func (r *registry) Endpoint(name string) (Endpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.hosts[name]
	if !ok {
		return Endpoint{}, false
	}
	return e.ep, true
}

// Names implements Registry.
func (r *registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.hosts))
	for name := range r.hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

//= docs/requirements/05-hosts.md#inventory
//# When the configuration is reloaded, the Runner SHALL add new
//# Hosts, stop probing removed Hosts and keep serving Jobs on removed Hosts
//# until they finish.

// Apply implements Registry. A Host that is new, or whose endpoint,
// token or TLS material changed, is dialled and takes its name; a Host that
// the new Inventory no longer lists leaves Names and Get, which is what
// stops the Scheduler probing it. Unchanged entries keep the connection
// they have, so a reload costs nothing for the Hosts that did not change.
//
// A replaced or removed entry is retired rather than closed where a Job
// holds a Lease on it, so that the Job finishes on the Host it started on
// (HO-014); its connection is closed as soon as the last lease is released,
// and immediately when there was none. A Runner whose Inventory is
// regenerated therefore does not accumulate connections to Hosts that have
// left it.
func (r *registry) Apply(ctx context.Context, endpoints []Endpoint) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("flintlock: registry is closed")
	}
	current := make(map[string]*hostEntry, len(r.hosts))
	for name, e := range r.hosts {
		current[name] = e
	}
	r.mu.Unlock()

	next := make(map[string]*hostEntry, len(endpoints))
	var (
		retired []*hostEntry
		added   []string
		dialled []*hostEntry
	)
	for _, ep := range endpoints {
		if ep.Name == "" {
			return fmt.Errorf("flintlock: inventory entry for %q has no host name", ep.Address)
		}
		if _, duplicate := next[ep.Name]; duplicate {
			return fmt.Errorf("flintlock: inventory lists host %s twice", ep.Name)
		}
		if existing, ok := current[ep.Name]; ok && existing.ep == ep {
			next[ep.Name] = existing
			continue
		}
		client, err := r.dialer.Dial(ctx, ep)
		if err != nil {
			// Nothing has been published yet, so the Registry keeps the
			// Inventory it had; the caller reports the configuration error.
			// Everything dialled on the way to the failure is closed, so a
			// refused reload leaks no connection.
			closeAll(dialled)
			return err
		}
		if existing, ok := current[ep.Name]; ok {
			retired = append(retired, existing)
		}
		entry := &hostEntry{ep: ep, client: client}
		next[ep.Name] = entry
		dialled = append(dialled, entry)
		added = append(added, ep.Name)
	}
	for name, e := range current {
		if _, kept := next[name]; !kept {
			retired = append(retired, e)
		}
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		closeAll(dialled)
		return errors.New("flintlock: registry is closed")
	}
	r.hosts = next
	// A retired entry nothing holds is closed here; one a Job still leases
	// waits in r.retired for its last holder.
	var idle []HostClient
	for _, e := range retired {
		e.retired = true
		if client := e.releasableLocked(); client != nil {
			idle = append(idle, client)
			continue
		}
		r.retired = append(r.retired, e)
	}
	r.mu.Unlock()
	closeClients(idle)

	if len(added) > 0 || len(retired) > 0 {
		removed := make([]string, 0, len(retired))
		for _, e := range retired {
			removed = append(removed, e.ep.Name)
		}
		sort.Strings(removed)
		sort.Strings(added)
		r.log.Info("host inventory applied", "added", added, "removed", removed, "hosts", len(next))
	}
	return nil
}

// Close implements Registry: every client, including those of Hosts a
// reload removed, is closed and the errors are joined.
func (r *registry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	entries := make([]*hostEntry, 0, len(r.hosts)+len(r.retired))
	for _, e := range r.hosts {
		entries = append(entries, e)
	}
	entries = append(entries, r.retired...)
	r.hosts = make(map[string]*hostEntry)
	r.retired = nil
	var clients []HostClient
	for _, e := range entries {
		if e.closed {
			continue
		}
		e.closed = true
		clients = append(clients, e.client)
	}
	r.mu.Unlock()

	var errs []error
	for _, c := range clients {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// closeAll closes the clients of entries whose reload never took effect.
func closeAll(entries []*hostEntry) {
	for _, e := range entries {
		_ = e.client.Close()
	}
}

// closeClients closes clients the Registry no longer needs, outside the
// lock.
func closeClients(clients []HostClient) {
	for _, c := range clients {
		_ = c.Close()
	}
}

// Compile-time check.
var _ Registry = (*registry)(nil)
