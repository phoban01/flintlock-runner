package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

func TestNewRequiresItsDependencies(t *testing.T) {
	t.Parallel()
	full := Deps{
		PoolManager:  newStubClient(),
		Specs:        stubSpecs{},
		HostSelector: stubSelector{},
		Declarer:     &stubDeclarer{},
		Tracker:      newStubTracker(),
		Health:       newStubHealth(),
		Hosts:        newTestRegistry(),
	}
	settings := Settings{Slots: 1, Namespace: testNamespace}

	tests := []struct {
		name   string
		break_ func(*Deps)
	}{
		{name: "pool manager", break_: func(d *Deps) { d.PoolManager = nil }},
		{name: "specs", break_: func(d *Deps) { d.Specs = nil }},
		{name: "host selector", break_: func(d *Deps) { d.HostSelector = nil }},
		{name: "declarer", break_: func(d *Deps) { d.Declarer = nil }},
		{name: "tracker", break_: func(d *Deps) { d.Tracker = nil }},
		{name: "health", break_: func(d *Deps) { d.Health = nil }},
		{name: "hosts", break_: func(d *Deps) { d.Hosts = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deps := full
			tt.break_(&deps)
			if _, err := New(deps, settings); err == nil {
				t.Fatalf("New without %s returned no error", tt.name)
			}
		})
	}

	if _, err := New(full, Settings{Slots: 0}); err == nil {
		t.Fatal("New with no slots returned no error")
	}
	s, err := New(full, settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s == nil {
		t.Fatal("New returned no scheduler")
	}
}

func TestRunDeclaresOnePoolPerProfileAndWarnsAboutUnknownHosts(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{profiles: profileFixtures()})
	// The Pool Manager reports a Host the Inventory does not have.
	e.declarer.admin = nil
	e.health.contact()
	e.run(ctx)

	waitFor(t, ctx, func() bool { return len(e.declarer.declared()) == 3 })
	seen := map[poolmgr.PoolRef]bool{}
	for _, spec := range e.declarer.declared() {
		seen[spec.Ref] = true
		if len(spec.FlintlockHosts) != 1 || spec.FlintlockHosts[0] != "host-1" {
			t.Fatalf("pool %s declared on %v, want the selected inventory hosts", spec.Ref, spec.FlintlockHosts)
		}
	}
	for _, name := range []string{"golang", "rust", "default"} {
		if !seen[poolmgr.PoolRef{Name: name, Namespace: testNamespace}] {
			t.Fatalf("no pool declared for profile %q", name)
		}
	}
	select {
	case <-e.sched.Ready():
	case <-ctx.Done():
		t.Fatal("Ready was not closed after the pool manager answered")
	}
}

func TestRunWarnsAboutAPoolHostMissingFromTheInventory(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{})
	e.health.contact()

	// The Pool Manager holds the Pool on a Host the Inventory does not name.
	e.declarer.admin = &poolWithHosts{hosts: []string{"host-1", "host-99"}}
	e.run(ctx)

	waitFor(t, ctx, func() bool { return len(e.logs.find("not in the inventory")) == 1 })
	record := e.logs.find("not in the inventory")[0]
	if record.attrs["host"] != "host-99" {
		t.Fatalf("warning names host %q, want host-99", record.attrs["host"])
	}
}

// poolWithHosts is a PoolAdmin that answers CreatePool with a Pool whose
// flintlock_hosts are the ones it was built with.
type poolWithHosts struct {
	poolmgr.PoolAdmin
	hosts []string
}

func (p *poolWithHosts) CreatePool(_ context.Context, spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	spec.FlintlockHosts = p.hosts
	return &poolmgr.Pool{Spec: spec}, nil
}

func TestUndeclaredPoolCountsAsEmptyAndIsRetried(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{tune: func(s *Settings) { s.PoolManager.DeclareRetryInterval = 30 * time.Second }})
	e.declarer.setErr(errors.New("pool manager is down"))
	e.health.contact()
	e.run(ctx)

	waitFor(t, ctx, func() bool { return len(e.declarer.declared()) == 1 })
	waitFor(t, ctx, func() bool { return !e.sched.poolDeclared(e.poolOf("default")) })

	// An undeclared Pool has no capacity, whatever the Tracker last saw.
	e.tracker.setAvailable(e.poolOf("default"), 5)
	if _, err := e.sched.Reserve(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("Reserve against an undeclared pool = %v, want a refusal", err)
	}

	// The declaration is retried at the configured interval.
	e.declarer.setErr(nil)
	waitFor(t, ctx, func() bool { return e.clk.Timers() > 0 })
	e.clk.Advance(31 * time.Second)
	waitFor(t, ctx, func() bool { return len(e.declarer.declared()) >= 2 })
	waitFor(t, ctx, func() bool { return e.sched.poolDeclared(e.poolOf("default")) })
}

func TestShutdownReleasesReservationsAndLetsJobsFinish(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client, tune: func(s *Settings) { s.ShutdownTimeout = time.Minute }})
	e.health.contact()
	stop := e.run(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 5)

	h := e.allocate(ctx, JobInfo{ID: 51}, "default")
	unconverted := e.reserve(ctx)
	_ = unconverted

	e.cancel()
	waitFor(t, ctx, func() bool { return e.sched.runningState() == stateStopping })

	// Every unconverted Reservation is returned, and the running Job keeps
	// its Slot and its Lease.
	waitFor(t, ctx, func() bool { return e.sched.Snapshot().SlotsInUse == 1 })
	if h.Err() != nil {
		t.Fatalf("the running job was failed during the grace period: %v", h.Err())
	}
	if _, err := e.sched.Reserve(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("Reserve while shutting down = %v, want a refusal", err)
	}

	// The Job finishes normally and Run returns without waiting out the
	// shutdown timeout.
	e.sched.Release(h)
	stop()
	waitFor(t, ctx, func() bool { return client.releaseCount() == 1 })
	if h.Err() != nil {
		t.Fatalf("handle error after a clean release = %v, want none", h.Err())
	}
}

func TestShutdownTimeoutAbortsTheJobsThatAreLeft(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client, tune: func(s *Settings) { s.ShutdownTimeout = time.Minute }})
	e.health.contact()
	stop := e.run(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 5)

	h := e.allocate(ctx, JobInfo{ID: 52}, "default")
	lease := h.Allocation().Lease.ID

	e.cancel()
	waitFor(t, ctx, func() bool { return e.sched.runningState() == stateStopping })
	waitFor(t, ctx, func() bool { return e.clk.Timers() > 0 })
	e.clk.Advance(2 * time.Minute)

	select {
	case <-h.Done():
	case <-ctx.Done():
		t.Fatal("the job was not aborted when the shutdown timeout elapsed")
	}
	if !errors.Is(h.Err(), ErrShutdown) {
		t.Fatalf("handle error = %v, want ErrShutdown", h.Err())
	}
	waitFor(t, ctx, func() bool { return client.releaseCount() == 1 })
	if got := client.releasedLeases(); got[0] != lease {
		t.Fatalf("released lease = %v, want %q", got, lease)
	}
	stop()
	if got := e.sched.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("slots in use after shutdown = %d, want 0", got)
	}
}

func TestReloadReplacesProfilesAndKeepsTheOldPool(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{profiles: profileFixtures()})
	e.health.contact()
	e.run(ctx)
	waitFor(t, ctx, func() bool { return len(e.declarer.declared()) == 3 })

	next := []config.Profile{testProfile("default", 3), testProfile("zig", 1)}
	inventory := []config.HostEntry{testHostEntry("host-1"), testHostEntry("host-2")}
	if err := e.sched.Reload(ctx, next, inventory); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if p := e.sched.profileByName("zig"); p == nil {
		t.Fatal("the new profile is not resolvable after the reload")
	}
	if p := e.sched.profileByName("golang"); p != nil {
		t.Fatal("a removed profile is still resolvable after the reload")
	}
	// A removed Profile's Pool is left in place, not deleted.
	if len(e.logs.find("no longer referenced")) != 2 {
		t.Fatalf("removed-pool log records = %d, want one per removed profile",
			len(e.logs.find("no longer referenced")))
	}
	if got := e.sched.Snapshot().Hosts; len(got) != 2 {
		t.Fatalf("hosts after the reload = %d, want 2", len(got))
	}
}

func TestRunTwiceIsRefused(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{})
	e.health.contact()
	e.run(ctx)
	waitFor(t, ctx, func() bool { return e.sched.runningState() == stateRunning })
	if err := e.sched.Run(ctx); !errors.Is(err, ErrAlreadyRun) {
		t.Fatalf("the second Run returned %v, want ErrAlreadyRun", err)
	}
}

func TestSnapshotReportsPoolsHostsAndWaiting(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	e := newEnv(t, envConfig{
		inventory: []config.HostEntry{testHostEntry("host-1"), testHostEntry("host-2")},
	})
	e.startBare(ctx)
	pool := e.poolOf("default")
	e.tracker.setStatus(pool, poolmgr.PoolStatus{Available: 2, Leased: 1, Provisioning: 3})

	got := e.sched.Snapshot()
	if !got.PoolManagerContacted || !got.PoolManagerHealthy {
		t.Fatalf("snapshot = %+v, want a contacted and healthy pool manager", got)
	}
	if got.Slots != 4 {
		t.Fatalf("slots = %d, want 4", got.Slots)
	}
	if len(got.Pools) != 1 || got.Pools[0].Status.Available != 2 {
		t.Fatalf("pools = %+v, want the tracker's counts", got.Pools)
	}
	if len(got.Hosts) != 2 || got.Hosts[0].Name != "host-1" || got.Hosts[1].Name != "host-2" {
		t.Fatalf("hosts = %+v, want both inventory hosts in name order", got.Hosts)
	}
	if !got.Ready() {
		t.Fatal("the scheduler is not ready with a contacted pool manager and healthy hosts")
	}
}
