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
// with.
type hostEntry struct {
	ep     Endpoint
	client HostClient
}

// registry is the production Registry.
type registry struct {
	dialer Dialer
	log    *slog.Logger

	mu sync.RWMutex
	// hosts is the live Inventory, keyed by the Host name the Pool Manager
	// uses in flintlock_hosts (HO-010).
	hosts map[string]*hostEntry
	// retired holds the clients of Hosts a reload removed. They are not
	// closed, because a Job holding one has to finish on it (HO-014); Close
	// releases them with everything else.
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
// stops the Scheduler probing it. Neither case closes a client: a Job that
// already holds one keeps running on it, and every client is released by
// Close. Unchanged entries keep the connection they have, so a reload costs
// nothing for the Hosts that did not change.
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
	r.retired = append(r.retired, retired...)
	r.mu.Unlock()

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
	r.mu.Unlock()

	var errs []error
	for _, e := range entries {
		if err := e.client.Close(); err != nil {
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

// Compile-time check.
var _ Registry = (*registry)(nil)
