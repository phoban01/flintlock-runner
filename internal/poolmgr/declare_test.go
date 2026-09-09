package poolmgr_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// declareRetryInterval is the configured interval a failed declaration is
// retried at in these tests. Nothing sleeps for it: the tests fire the
// Declaration's fake clock.
const declareRetryInterval = 30 * time.Second

// declaration builds a Declaration over a client with a fake clock. The
// tests that watch what the Tracker is told build their own.
func declaration(t *testing.T, c poolmgr.Client, clk clock.Clock) *poolmgr.Declaration {
	t.Helper()
	d, err := poolmgr.NewDeclaration(poolmgr.DeclarationConfig{
		Builder:       poolmgr.NewSpecBuilder(),
		Selector:      poolmgr.NewHostSelector(),
		Declarer:      poolmgr.NewDeclarer(c),
		RunnerName:    testRunner,
		Namespace:     testNamespace,
		Clock:         clk,
		RetryInterval: declareRetryInterval,
		Log:           testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewDeclaration: %v", err)
	}
	return d
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//= type=test
//# When starting and on every configuration reload, the Scheduler SHALL
//# create or update one Pool per Profile, deriving the Pool's MicroVM
//# template from the Profile and the Pool's size, replenishment strategy,
//# hooks and heartbeat settings from the Profile's pool settings.

// TestPoolsAreDeclaredForEveryProfile declares two Profiles at startup and
// checks the Pools the Pool Manager ends up holding against the Profiles
// they came from, then reloads with one Profile changed and checks that the
// existing Pool is updated rather than left alone or recreated.
func TestPoolsAreDeclaredForEveryProfile(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "arm-fast", "arm-slow")
	c := pm.client()
	d := declaration(t, c, clock.NewFake(testEpoch))

	small := testProfile("small", 1)
	large := testProfile("large", 2)
	large.VCPU, large.MemoryMB = 8, 16384
	large.Pool.HeartbeatInterval, large.Pool.HeartbeatExpiry = 5*time.Second, 15*time.Second
	large.HostSelector = map[string]string{"disk": "nvme"}

	if err := d.Sync(ctx, []config.Profile{small, large}, testInventory()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	pools, err := c.ListPools(ctx, testNamespace)
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("the pool manager holds %d pools, want one per profile", len(pools))
	}
	byName := map[string]*poolmgr.Pool{}
	for _, pool := range pools {
		byName[pool.Spec.Ref.Name] = pool
	}
	declared, ok := byName["large"]
	if !ok {
		t.Fatalf("pools = %v, want one named large", byName)
	}
	if declared.Spec.Size != 2 {
		t.Errorf("large pool size = %d, want 2", declared.Spec.Size)
	}
	if !slices.Equal(declared.Spec.FlintlockHosts, []string{"arm-fast"}) {
		t.Errorf("large pool hosts = %v, want just arm-fast", declared.Spec.FlintlockHosts)
	}
	if got := declared.Spec.Template.GetVcpu(); got != 8 {
		t.Errorf("large pool template vcpu = %d, want 8", got)
	}
	if got := declared.Spec.HeartbeatExpiryThreshold; got != 15*time.Second {
		t.Errorf("large pool heartbeat expiry = %s, want 15s", got)
	}
	if got := declared.Spec.Template.GetLabels()["gitlab-runner.flintlock.dev/profile"]; got != "large" {
		t.Errorf("large pool profile label = %q, want large", got)
	}

	// A reload that changes a Profile updates the Pool it already declared.
	large.Pool.Size = 3
	if err := d.Sync(ctx, []config.Profile{small, large}, testInventory()); err != nil {
		t.Fatalf("Sync after reload: %v", err)
	}
	updated, err := c.GetPool(ctx, ref("large"))
	if err != nil {
		t.Fatalf("GetPool after reload: %v", err)
	}
	if updated.Spec.Size != 3 {
		t.Errorf("large pool size after reload = %d, want 3", updated.Spec.Size)
	}
	if pools, err = c.ListPools(ctx, testNamespace); err != nil || len(pools) != 2 {
		t.Errorf("after reload the pool manager holds %d pools (%v), want two", len(pools), err)
	}
}

// TestDeclaringAPoolThatExistsWithADifferentSpecUpdatesIt is the create
// then update path of the Declarer on its own: a Pool declared by an
// earlier run of the Runner, with a different spec, is brought to the
// current one, and the warm MicroVMs it already holds are kept.
func TestDeclaringAPoolThatExistsWithADifferentSpecUpdatesIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()

	old := specFor(t, testProfile("small", 1), "host-a")
	pm.fillPool(c, old)
	before := pm.vms()
	if len(before) != 1 {
		t.Fatalf("the pool holds %d microvms, want one", len(before))
	}

	changed := old
	changed.Size = 2
	changed.HeartbeatInterval = 4 * time.Second
	declarer := poolmgr.NewDeclarer(c)
	pool, err := declarer.Declare(ctx, changed)
	if err != nil {
		t.Fatalf("Declare over an existing pool: %v", err)
	}
	if pool.Spec.Size != 2 || pool.Spec.HeartbeatInterval != 4*time.Second {
		t.Errorf("declared pool = %+v, want the new size and heartbeat interval", pool.Spec)
	}
	for _, vm := range before {
		if !slices.ContainsFunc(pm.vms(), func(got poolmgr.VMRecord) bool { return got.UID == vm.UID }) {
			t.Errorf("microvm %s was destroyed by the update", vm.UID)
		}
	}
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//= type=test
//# When a Profile is removed by a configuration reload, the Scheduler SHALL
//# NOT delete its Pool and SHALL log that the Pool is no longer referenced.

// TestRemovedProfileLeavesItsPoolAlone reloads without one of the two
// Profiles and checks that its Pool and its warm MicroVMs are still at the
// Pool Manager, that the Runner stopped tracking it, and that the removal
// was logged.
func TestRemovedProfileLeavesItsPoolAlone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	tracker := newRecordingTracker()
	logs := newLogRecorder(t)
	d, err := poolmgr.NewDeclaration(poolmgr.DeclarationConfig{
		Builder:       poolmgr.NewSpecBuilder(),
		Selector:      poolmgr.NewHostSelector(),
		Declarer:      poolmgr.NewDeclarer(c),
		Tracker:       tracker,
		RunnerName:    testRunner,
		Namespace:     testNamespace,
		Clock:         clock.NewFake(testEpoch),
		RetryInterval: declareRetryInterval,
		Log:           logs.logger(),
	})
	if err != nil {
		t.Fatalf("NewDeclaration: %v", err)
	}

	keep := testProfile("keep", 1)
	drop := testProfile("drop", 1)
	if err := d.Sync(ctx, []config.Profile{keep, drop}, testInventory()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := d.Sync(ctx, []config.Profile{keep}, testInventory()); err != nil {
		t.Fatalf("Sync after the profile was removed: %v", err)
	}

	pool, err := c.GetPool(ctx, ref("drop"))
	if err != nil {
		t.Fatalf("the pool of the removed profile is gone: %v", err)
	}
	if pool.Spec.Size != 1 {
		t.Errorf("the pool of the removed profile = %+v, want it unchanged", pool.Spec)
	}
	if _, known := d.Spec(ref("drop")); known {
		t.Error("the declaration still declares the removed profile's pool")
	}
	if !slices.Contains(tracker.untracked(), ref("drop")) {
		t.Errorf("untracked pools = %v, want the removed profile's pool", tracker.untracked())
	}
	if !logs.contains("no longer referenced") || !logs.contains("runner-ns/drop") {
		t.Errorf("the removal was not logged; log was:\n%s", logs.text())
	}
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//= type=test
//# If `CreatePool` or `UpdatePool` fails for a Profile, then the Scheduler
//# SHALL log the error, treat that Pool as empty and retry the declaration
//# at the configured interval.

// TestFailedDeclarationIsLoggedCountedEmptyAndRetried takes the Pool
// Manager away, declares, and checks the three consequences: the error is
// logged, the Tracker is told the Pool is not declared so that it counts as
// empty, and the declaration is retried when the configured interval
// elapses -- not before, and successfully once the Pool Manager is back.
func TestFailedDeclarationIsLoggedCountedEmptyAndRetried(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	clk := clock.NewFake(testEpoch)
	tracker := newRecordingTracker()
	logs := newLogRecorder(t)
	d, err := poolmgr.NewDeclaration(poolmgr.DeclarationConfig{
		Builder:       poolmgr.NewSpecBuilder(),
		Selector:      poolmgr.NewHostSelector(),
		Declarer:      poolmgr.NewDeclarer(c),
		Tracker:       tracker,
		RunnerName:    testRunner,
		Namespace:     testNamespace,
		Clock:         clk,
		RetryInterval: declareRetryInterval,
		Log:           logs.logger(),
	})
	if err != nil {
		t.Fatalf("NewDeclaration: %v", err)
	}

	pm.setFaults(poolmgr.Faults{UnavailableFor: time.Hour})
	profile := testProfile("small", 1)
	if err := d.Sync(ctx, []config.Profile{profile}, testInventory()); err == nil {
		t.Fatal("Sync succeeded while the pool manager was unavailable")
	} else if !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Errorf("Sync error = %v, want one wrapping ErrUnavailable", err)
	}
	if d.Declared(ref("small")) {
		t.Error("the pool counts as declared after the declaration failed")
	}
	if declared, ok := tracker.state(ref("small")); !ok || declared {
		t.Errorf("tracker was told declared=%v (tracked=%v), want it told the pool is not declared", declared, ok)
	}
	if !logs.contains("pool not declared") {
		t.Errorf("the failure was not logged; log was:\n%s", logs.text())
	}

	stop := runInBackground(t, "Declaration.Run", d.Run)
	defer stop()
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the retry timer: %v", err)
	}

	// The Pool Manager is still down: the retry that fires now fails too.
	clk.Advance(declareRetryInterval)
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the second retry timer: %v", err)
	}
	if d.Declared(ref("small")) {
		t.Error("the pool counts as declared while the pool manager is still down")
	}

	// With the Pool Manager back, the next retry declares it. Waiting for
	// the timer to be armed again is waiting for that retry to finish.
	pm.setFaults(poolmgr.Faults{})
	clk.Advance(declareRetryInterval)
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the retry to finish: %v", err)
	}
	if !d.Declared(ref("small")) {
		t.Fatal("the pool is still not declared after a retry with the pool manager back")
	}
	if _, err := c.GetPool(ctx, ref("small")); err != nil {
		t.Errorf("GetPool after the retry: %v", err)
	}
	if declared, ok := tracker.state(ref("small")); !ok || !declared {
		t.Errorf("tracker was told declared=%v (tracked=%v), want it told the pool is declared", declared, ok)
	}
}

// TestRedeclareBringsBackADeletedPool covers the seam PL-033 uses: a Pool
// that disappeared from the Pool Manager is declared again from the Profile
// the Runner still holds.
func TestRedeclareBringsBackADeletedPool(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	d := declaration(t, c, clock.NewFake(testEpoch))

	if err := d.Sync(ctx, []config.Profile{testProfile("small", 1)}, testInventory()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := c.DeletePool(ctx, ref("small")); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	if _, err := c.GetPool(ctx, ref("small")); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Fatalf("GetPool after DeletePool = %v, want ErrNotFound", err)
	}

	if err := d.Redeclare(ctx, ref("small")); err != nil {
		t.Fatalf("Redeclare: %v", err)
	}
	if _, err := c.GetPool(ctx, ref("small")); err != nil {
		t.Errorf("GetPool after Redeclare: %v", err)
	}
	if err := d.Redeclare(ctx, ref("never-declared")); err == nil {
		t.Error("Redeclare accepted a pool no profile declares")
	}
}

// TestDeclarationRejectsTwoProfilesSharingAPool covers the CF-029 case
// reaching the Declaration: two Profiles resolving to one Pool are not
// silently declared twice.
func TestDeclarationRejectsTwoProfilesSharingAPool(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	d := declaration(t, c, clock.NewFake(testEpoch))

	first := testProfile("one", 1)
	second := testProfile("two", 1)
	second.Pool.Name = first.Pool.Name

	err := d.Sync(ctx, []config.Profile{first, second}, testInventory())
	if err == nil {
		t.Fatal("Sync accepted two profiles declaring one pool")
	}
	pools, listErr := c.ListPools(ctx, testNamespace)
	if listErr != nil {
		t.Fatalf("ListPools: %v", listErr)
	}
	if len(pools) != 1 {
		t.Errorf("the pool manager holds %d pools, want the one that was not in conflict", len(pools))
	}
}
