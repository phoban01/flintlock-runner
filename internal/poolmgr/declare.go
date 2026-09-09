package poolmgr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
)

// defaultDeclareRetryInterval is how often a failed Pool declaration is
// retried when the configuration gives no interval (PL-016).
const defaultDeclareRetryInterval = 30 * time.Second

// declarer is the Declarer implementation.
type declarer struct {
	admin PoolAdmin
}

// NewDeclarer returns the Declarer that creates a Pool, or updates it when
// it already exists.
func NewDeclarer(admin PoolAdmin) Declarer { return &declarer{admin: admin} }

// Declare implements Declarer. It creates the Pool and falls back to
// UpdatePool when the Pool Manager already holds one of that name and
// namespace, so that a Pool whose Profile has changed is brought to the
// spec the Runner wants rather than left as it was. The rare inverse race,
// a Pool deleted between the create and the update, is retried as a create.
// No retry loop lives here: PL-016 puts that at the configured interval,
// which is Declaration's job.
func (d *declarer) Declare(ctx context.Context, spec PoolSpec) (*Pool, error) {
	pool, err := d.admin.CreatePool(ctx, spec)
	switch {
	case err == nil:
		return pool, nil
	case !errors.Is(err, ErrAlreadyExists):
		return nil, fmt.Errorf("creating pool %s: %w", spec.Ref, err)
	}
	pool, err = d.admin.UpdatePool(ctx, spec)
	if errors.Is(err, ErrNotFound) {
		if pool, err = d.admin.CreatePool(ctx, spec); err != nil {
			return nil, fmt.Errorf("creating pool %s: %w", spec.Ref, err)
		}
		return pool, nil
	}
	if err != nil {
		return nil, fmt.Errorf("updating pool %s: %w", spec.Ref, err)
	}
	return pool, nil
}

// DeclarationConfig is what NewDeclaration needs. Builder, Selector and
// Declarer are required; the rest have defaults.
type DeclarationConfig struct {
	// Builder derives a PoolSpec from a Profile and Selector picks the
	// Pool's Hosts out of the Inventory.
	Builder  SpecBuilder
	Selector HostSelector
	// Declarer talks to the Pool Manager.
	Declarer Declarer
	// Tracker, when set, is told which Pools exist and whether their
	// declaration succeeded, so that a Pool the Runner could not declare
	// counts as empty (PL-016).
	Tracker Tracker
	// RunnerName and Namespace identify this Runner's Pools (PL-014,
	// PL-017).
	RunnerName string
	Namespace  string
	// Clock and RetryInterval pace the retry of a failed declaration
	// (PL-016).
	Clock         clock.Clock
	RetryInterval time.Duration
	// Log receives the declaration errors and the notice that a Pool is no
	// longer referenced (PL-015, PL-016).
	Log *slog.Logger
}

// Declaration declares one Pool per Profile and keeps trying until each of
// them exists (PL-010, PL-015, PL-016). Sync is called at startup and on
// every configuration reload; Run retries in between. It is safe for
// concurrent use.
type Declaration struct {
	cfg   DeclarationConfig
	log   *slog.Logger
	clk   clock.Clock
	kick  chan struct{}
	mu    sync.Mutex
	pools map[PoolRef]*declaredPool
}

// declaredPool is what Declaration remembers about one Pool: the spec it
// last built for it and whether the Pool Manager has that spec.
type declaredPool struct {
	profile string
	spec    PoolSpec
	// ok is false while CreatePool or UpdatePool is failing; err is why.
	ok  bool
	err error
}

// NewDeclaration builds a Declaration.
func NewDeclaration(cfg DeclarationConfig) (*Declaration, error) {
	switch {
	case cfg.Builder == nil:
		return nil, errors.New("poolmgr: declaration needs a SpecBuilder")
	case cfg.Selector == nil:
		return nil, errors.New("poolmgr: declaration needs a HostSelector")
	case cfg.Declarer == nil:
		return nil, errors.New("poolmgr: declaration needs a Declarer")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = defaultDeclareRetryInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Declaration{
		cfg:   cfg,
		log:   cfg.Log.With("component", "poolmgr-declaration"),
		clk:   cfg.Clock,
		kick:  make(chan struct{}, 1),
		pools: make(map[PoolRef]*declaredPool),
	}, nil
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//# When starting and on every configuration reload, the Scheduler SHALL
//# create or update one Pool per Profile, deriving the Pool's MicroVM
//# template from the Profile and the Pool's size, replenishment strategy,
//# hooks and heartbeat settings from the Profile's pool settings.

//= docs/requirements/04-pool-manager.md#pool-declaration
//# When a Profile is removed by a configuration reload, the Scheduler SHALL
//# NOT delete its Pool and SHALL log that the Pool is no longer referenced.

// Sync declares one Pool per Profile: it builds each spec from the Profile
// and the Hosts of the Inventory that the Profile selects, then creates or
// updates the Pool. It is called once at startup and again on every
// configuration reload. A Pool that was declared for a Profile the reload
// removed is left alone at the Pool Manager and only logged: the Runner
// does not delete Pools, because another Runner or a later reload may want
// it and because deleting one would destroy warm MicroVMs somebody else is
// using (PL-015). Sync returns the declaration errors joined; each is also
// logged and retried by Run (PL-016).
func (d *Declaration) Sync(ctx context.Context, profiles []config.Profile, inventory []config.HostEntry) error {
	specs := make(map[PoolRef]*declaredPool, len(profiles))
	var errs []error
	for _, p := range profiles {
		spec, err := d.cfg.Builder.Build(p, SpecInput{
			RunnerName: d.cfg.RunnerName,
			Namespace:  d.cfg.Namespace,
			Hosts:      d.cfg.Selector.Select(p, inventory),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("profile %q: %w", p.Name, err))
			d.log.Error("pool spec not built", "profile", p.Name, "error", err)
			continue
		}
		if existing, ok := specs[spec.Ref]; ok {
			errs = append(errs, fmt.Errorf("profiles %q and %q both declare pool %s", existing.profile, p.Name, spec.Ref))
			continue
		}
		specs[spec.Ref] = &declaredPool{profile: p.Name, spec: spec}
	}

	d.mu.Lock()
	for ref, state := range d.pools {
		if _, kept := specs[ref]; kept {
			continue
		}
		delete(d.pools, ref)
		d.log.Info("pool no longer referenced by any profile; leaving it at the pool manager",
			"pool", ref.String(), "profile", state.profile)
		if d.cfg.Tracker != nil {
			d.cfg.Tracker.Untrack(ref)
		}
	}
	for ref, state := range specs {
		if existing, ok := d.pools[ref]; ok {
			existing.profile, existing.spec = state.profile, state.spec
			continue
		}
		d.pools[ref] = state
	}
	d.mu.Unlock()

	for _, ref := range d.refs() {
		if err := d.declare(ctx, ref); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//# If `CreatePool` or `UpdatePool` fails for a Profile, then the Scheduler
//# SHALL log the error, treat that Pool as empty and retry the declaration
//# at the configured interval.

// Run retries the declaration of every Pool that is not declared, at the
// configured interval, until ctx is cancelled. A Pool whose declaration is
// failing is marked undeclared on the Tracker, which counts it as empty for
// capacity, so the Runner refuses work it could not start rather than
// accepting a Job for a Pool that does not exist.
func (d *Declaration) Run(ctx context.Context) error {
	timer := d.clk.NewTimer(d.cfg.RetryInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.kick:
		case <-timer.C():
		}
		d.retryFailed(ctx)
		timer.Reset(d.cfg.RetryInterval)
	}
}

// retryFailed re-declares every Pool whose last declaration failed.
func (d *Declaration) retryFailed(ctx context.Context) {
	for _, ref := range d.refs() {
		d.mu.Lock()
		state, ok := d.pools[ref]
		retry := ok && !state.ok
		d.mu.Unlock()
		if !retry {
			continue
		}
		if err := d.declare(ctx, ref); err != nil && ctx.Err() != nil {
			return
		}
	}
}

//= docs/requirements/04-pool-manager.md#claiming
//# If `ClaimVM` returns `NOT_FOUND`, then the Scheduler SHALL re-declare
//# the Pool from its Profile and treat the Pool as having no warm MicroVM
//# available.

// Redeclare declares the Pool again from the spec its Profile produced. A
// claim that found the Pool gone calls it, so that the Pool exists again
// before the next claim; the claim itself is answered as an exhausted Pool
// (PL-032), because re-declaring a Pool does not make a warm MicroVM
// appear. It reports an error for a Pool no Profile declares.
func (d *Declaration) Redeclare(ctx context.Context, ref PoolRef) error {
	d.mu.Lock()
	_, known := d.pools[ref]
	d.mu.Unlock()
	if !known {
		return fmt.Errorf("poolmgr: pool %s is not declared by any profile", ref)
	}
	return d.declare(ctx, ref)
}

// declare declares one Pool and records the outcome. The Tracker learns
// whether the Pool is declared, so that an undeclared Pool counts as empty.
func (d *Declaration) declare(ctx context.Context, ref PoolRef) error {
	d.mu.Lock()
	state, ok := d.pools[ref]
	var spec PoolSpec
	if ok {
		spec = state.spec
	}
	d.mu.Unlock()
	if !ok {
		return nil
	}

	pool, err := d.cfg.Declarer.Declare(ctx, spec)

	d.mu.Lock()
	if current, still := d.pools[ref]; still {
		current.ok, current.err = err == nil, err
	}
	d.mu.Unlock()

	if err != nil {
		d.log.Error("pool not declared; treating it as empty and retrying",
			"pool", ref.String(), "profile", state.profile,
			"retry_in", d.cfg.RetryInterval, "error", err)
		if d.cfg.Tracker != nil {
			d.cfg.Tracker.Track(ref, false)
		}
		d.requestRetry()
		return err
	}
	d.log.Info("pool declared", "pool", ref.String(), "profile", state.profile,
		"size", pool.Spec.Size, "hosts", pool.Spec.FlintlockHosts)
	if d.cfg.Tracker != nil {
		d.cfg.Tracker.Track(ref, true)
	}
	return nil
}

// requestRetry wakes Run without blocking.
func (d *Declaration) requestRetry() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// refs returns the declared Pools in a stable order.
func (d *Declaration) refs() []PoolRef {
	d.mu.Lock()
	defer d.mu.Unlock()
	refs := make([]PoolRef, 0, len(d.pools))
	for ref := range d.pools {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	return refs
}

// Spec returns the spec last built for a Pool and whether it is known.
func (d *Declaration) Spec(ref PoolRef) (PoolSpec, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.pools[ref]
	if !ok {
		return PoolSpec{}, false
	}
	return state.spec, true
}

// Declared reports whether a Pool's last declaration succeeded. An unknown
// Pool is not declared.
func (d *Declaration) Declared(ref PoolRef) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.pools[ref]
	return ok && state.ok
}

// Compile-time interface check.
var _ Declarer = (*declarer)(nil)
