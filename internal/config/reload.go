package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
)

// ReloadFunc is notified after a successful reload with the new effective
// configuration. main registers the Scheduler's Reload and the Executor's
// Inventory swap here.
type ReloadFunc func(ctx context.Context, cfg *Config) error

// Reloader holds the current configuration and replaces its Profiles and
// Inventory from disk on demand (CF-007) or on SIGHUP. The swap is atomic:
// Current returns either the old or the new *Config, never a mix, and the
// returned value is never mutated afterwards, so holders may read it without
// locking. A reload that fails validation leaves the current configuration
// in place (CF-008).
type Reloader struct {
	path   string
	opts   []Option
	logger *slog.Logger

	current atomic.Pointer[Config]

	mu   sync.Mutex // serialises Reload and guards subs
	subs []ReloadFunc
}

// NewReloader starts from initial, as returned by Load(path, opts...), and
// re-reads path with the same options on every reload.
func NewReloader(initial *Config, path string, opts ...Option) *Reloader {
	l := newLoader(opts)
	r := &Reloader{path: path, opts: opts, logger: l.logger}
	r.current.Store(initial)
	return r
}

// Current returns the configuration in effect. The result is immutable.
func (r *Reloader) Current() *Config { return r.current.Load() }

// Subscribe registers fn to run after every successful reload, in
// registration order. Errors from fn are logged and returned by Reload but
// do not undo the swap.
func (r *Reloader) Subscribe(fn ReloadFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs = append(r.subs, fn)
}

//= docs/requirements/07-configuration.md#file-and-precedence
//# When the Runner receives `SIGHUP`, the Runner SHALL reload the
//# Profiles and the Inventory from disk without interrupting running Jobs.

//= docs/requirements/07-configuration.md#file-and-precedence
//# If a reload produces an invalid configuration, then the Runner
//# SHALL keep the previous configuration and log the validation error.

// Reload re-reads the configuration file, validates it as a whole and, when
// it is valid, swaps in a configuration that is the current one with the
// Profiles and the Inventory replaced (CF-007). Nothing else changes at run
// time: the GitLab section, the Pool Manager connection and the limits keep
// their values until a restart, and a difference there is logged. Running
// Jobs are untouched because the swap only publishes a new pointer;
// subscribers such as the Scheduler re-declare Pools without touching held
// Leases. When the file fails to load or validate, the current configuration
// stays in place and the error is logged and returned (CF-008).
// Subscribers run after the lock is released, so a subscriber may call back
// into Subscribe, Current or Reload without deadlocking, and a slow one does
// not hold up the next SIGHUP.
func (r *Reloader) Reload(ctx context.Context) error {
	next, subs, err := r.swap(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, fn := range subs {
		if err := fn(ctx, next); err != nil {
			r.logger.Error("reload subscriber failed", "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// swap is the serialised half of Reload: it re-reads and validates the file
// and publishes the new configuration, or leaves the current one in place
// (CF-008). It returns the published configuration together with the
// subscribers to notify, snapshotted under the same lock so that the
// notification loop runs outside the critical section.
func (r *Reloader) swap(ctx context.Context) (*Config, []ReloadFunc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	loaded, err := Load(r.path, r.opts...)
	if err != nil {
		r.logger.Error("configuration reload rejected; keeping the previous configuration",
			"path", r.path, "error", err)
		return nil, nil, fmt.Errorf("config: reload %s: %w", r.path, err)
	}
	old := r.current.Load()
	next := *old
	next.Profiles = loaded.Profiles
	next.Inventory = loaded.Inventory
	if !onlyReloadableSectionsDiffer(old, loaded) {
		r.logger.Warn("configuration changed outside profiles and inventory; those changes take effect at the next restart",
			"path", r.path)
	}
	r.current.Store(&next)
	r.logger.Info("configuration reloaded",
		"path", r.path, "profiles", len(next.Profiles), "hosts", len(next.Inventory.Hosts))

	subs := make([]ReloadFunc, len(r.subs))
	copy(subs, r.subs)
	return &next, subs, nil
}

// onlyReloadableSectionsDiffer reports whether old and loaded agree on every
// section other than Profiles and Inventory.
func onlyReloadableSectionsDiffer(old, loaded *Config) bool {
	compare := *loaded
	compare.Profiles = old.Profiles
	compare.Inventory = old.Inventory
	// Defaults derived from the Profiles (the concurrency limit) follow them;
	// they are not something the operator changed.
	compare.GitLab.Concurrent = old.GitLab.Concurrent
	return reflect.DeepEqual(compare, *old)
}

// Serve reloads once per value received on signals until ctx is done or
// signals is closed. Reload failures are already logged, so Serve only
// returns when it stops, and then always nil, which keeps shutdown clean.
func (r *Reloader) Serve(ctx context.Context, signals <-chan os.Signal) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-signals:
			if !ok {
				return nil
			}
			_ = r.Reload(ctx) // logged inside
		}
	}
}

// ServeSIGHUP subscribes to SIGHUP and calls Serve. It returns when ctx is
// done, after unsubscribing, so no signal handler outlives the caller.
func (r *Reloader) ServeSIGHUP(ctx context.Context) error {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	return r.Serve(ctx, ch)
}
