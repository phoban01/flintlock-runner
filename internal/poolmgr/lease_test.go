package poolmgr_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// claimOne fills a Pool with one MicroVM and claims it.
func claimOne(t *testing.T, ctx context.Context, pm *poolManager, c poolmgr.Client) (poolmgr.PoolSpec, *poolmgr.Claim) {
	t.Helper()
	spec := specFor(t, testProfile("small", 1), "host-a")
	pm.fillPool(c, spec)
	claim, err := c.ClaimVM(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	return spec, claim
}

// leaseOf finds a lease record at the fake Pool Manager.
func leaseOf(pm *poolManager, leaseID string) (poolmgr.LeaseRecord, bool) {
	for _, lease := range pm.leases() {
		if lease.LeaseID == leaseID {
			return lease, true
		}
	}
	return poolmgr.LeaseRecord{}, false
}

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//= type=test
//# While an Allocation holds a Lease, the Scheduler SHALL call `Heartbeat`
//# with the `lease_id` and SHALL update the Lease expiry from the returned
//# `expires_at`.

// TestLeaseIsHeartbeatedAndTheExpiryFollows runs the keep-alive over a real
// Lease and checks both halves: the Pool Manager sees heartbeats for that
// lease id, and the expiry the keeper holds is the one the Pool Manager
// answered with, not one it made up.
func TestLeaseIsHeartbeatedAndTheExpiryFollows(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	_, claim := claimOne(t, ctx, pm, c)

	// The expiry comes from the Pool Manager's clock, so the keeper's fake
	// clock starts at the wall clock: the two are then comparable and the
	// heartbeat interval is computed from a real remaining lifetime.
	clk := clock.NewFake(time.Now())
	keeper, err := poolmgr.NewLeaseKeeper(poolmgr.LeaseKeeperConfig{
		Lease:    c,
		LeaseID:  claim.LeaseID,
		Interval: 10 * time.Second,
		Clock:    clk,
		Log:      testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewLeaseKeeper: %v", err)
	}
	runInBackground(t, "LeaseKeeper.Run", keeper.Run)

	before, ok := leaseOf(pm, claim.LeaseID)
	if !ok {
		t.Fatalf("the pool manager has no lease %s", claim.LeaseID)
	}
	if got := keeper.ExpiresAt(); !got.IsZero() {
		t.Errorf("ExpiresAt = %s before the first heartbeat, want the zero time", got)
	}

	for beat := 1; beat <= 3; beat++ {
		if err := clk.BlockUntil(ctx, 1); err != nil {
			t.Fatalf("waiting for the heartbeat timer: %v", err)
		}
		clk.Advance(10 * time.Second)
		if err := clk.BlockUntil(ctx, 1); err != nil {
			t.Fatalf("waiting for heartbeat %d to be answered: %v", beat, err)
		}
		if got := keeper.Heartbeats(); got < beat {
			t.Fatalf("keeper counted %d heartbeats, want at least %d", got, beat)
		}
		after, ok := leaseOf(pm, claim.LeaseID)
		if !ok {
			t.Fatalf("the lease disappeared after heartbeat %d", beat)
		}
		if !after.LastHeartbeatAt.After(before.LastHeartbeatAt) {
			t.Errorf("heartbeat %d did not reach the pool manager: last heartbeat still %s",
				beat, after.LastHeartbeatAt)
		}
		if !keeper.ExpiresAt().Equal(after.ExpiresAt) {
			t.Errorf("keeper expiry = %s, want the pool manager's %s", keeper.ExpiresAt(), after.ExpiresAt)
		}
		before = after
	}
}

// TestHeartbeatOnALeaseThatIsGoneStops covers the answer the Scheduler
// turns into an aborted Job (SC-061): a Pool Manager that no longer has
// the Lease ends the keep-alive with ErrLeaseLost rather than retrying
// forever.
func TestHeartbeatOnALeaseThatIsGoneStops(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	_, claim := claimOne(t, ctx, pm, c)
	pm.setFaults(poolmgr.Faults{RefuseHeartbeats: true})

	clk := clock.NewFake(time.Now())
	keeper, err := poolmgr.NewLeaseKeeper(poolmgr.LeaseKeeperConfig{
		Lease:    c,
		LeaseID:  claim.LeaseID,
		Interval: 10 * time.Second,
		Clock:    clk,
		Log:      testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewLeaseKeeper: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- keeper.Run(ctx) }()

	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the heartbeat timer: %v", err)
	}
	clk.Advance(10 * time.Second)
	select {
	case err := <-done:
		if !errors.Is(err, poolmgr.ErrLeaseLost) {
			t.Errorf("Run = %v, want ErrLeaseLost", err)
		}
		if !errors.Is(err, poolmgr.ErrNotFound) {
			t.Errorf("Run = %v, want it to say the lease was not found", err)
		}
	case <-ctx.Done():
		t.Fatal("the keeper did not stop when the lease was gone")
	}
}

// TestHeartbeatsThatKeepFailingEndAtTheExpiry covers the other way a Lease
// is lost (SC-062): the Pool Manager cannot be reached at all, the keeper
// retries, and it gives up once the expiry it last received has passed,
// because by then the Pool Manager has swept the Lease.
func TestHeartbeatsThatKeepFailingEndAtTheExpiry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	_, claim := claimOne(t, ctx, pm, c)

	start := time.Now()
	clk := clock.NewFake(start)
	keeper, err := poolmgr.NewLeaseKeeper(poolmgr.LeaseKeeperConfig{
		Lease:     c,
		LeaseID:   claim.LeaseID,
		Interval:  10 * time.Second,
		ExpiresAt: start.Add(30 * time.Second),
		Clock:     clk,
		Log:       testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewLeaseKeeper: %v", err)
	}
	pm.setFaults(poolmgr.Faults{UnavailableFor: time.Hour})
	done := make(chan error, 1)
	go func() { done <- keeper.Run(ctx) }()

	// Two failed heartbeats before the expiry: the keeper keeps trying.
	for beat := 0; beat < 2; beat++ {
		if err := clk.BlockUntil(ctx, 1); err != nil {
			t.Fatalf("waiting for the heartbeat timer: %v", err)
		}
		clk.Advance(10 * time.Second)
		select {
		case err := <-done:
			t.Fatalf("the keeper gave up before the expiry: %v", err)
		default:
		}
	}
	// The third failure is past the expiry.
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the heartbeat timer: %v", err)
	}
	clk.Advance(20 * time.Second)
	select {
	case err := <-done:
		if !errors.Is(err, poolmgr.ErrLeaseLost) {
			t.Errorf("Run = %v, want ErrLeaseLost", err)
		}
	case <-ctx.Done():
		t.Fatal("the keeper did not give up after the lease expired")
	}
}

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//= type=test
//# When a Job holding a Lease finishes, the Scheduler SHALL call
//# `ReleaseVM` with the `lease_id`.

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//= type=test
//# The Scheduler SHALL NOT call `DeleteMicroVM` on any Host.

// TestReleaseGivesTheLeaseBackAndDeletesNothing releases a real Lease and
// checks what happened at the Pool Manager and at the Host: the Lease is
// gone, the MicroVM was deleted by the Pool Manager as part of the release,
// and the Runner did not delete it, which it could not do in any case --
// TestPackageNeverCallsDeleteMicroVM checks that no code in this package
// can reach a Host's delete.
func TestReleaseGivesTheLeaseBackAndDeletesNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	stream := pm.events(c)
	spec, claim := claimOne(t, ctx, pm, c)

	releaser, err := poolmgr.NewReleaser(poolmgr.ReleaserConfig{
		Lease:      c,
		Clock:      clock.NewFake(testEpoch),
		Backoff:    clock.Exponential{Base: time.Second, Max: time.Minute},
		RetryLimit: 2,
		Log:        testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewReleaser: %v", err)
	}
	if err := releaser.Release(ctx, claim.LeaseID); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, ok := leaseOf(pm, claim.LeaseID); ok {
		t.Errorf("the pool manager still holds lease %s", claim.LeaseID)
	}
	deleted := awaitEvent(t, ctx, stream, poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
	if deleted.VMUID != claim.VMUID || deleted.Pool != spec.Ref {
		t.Errorf("VM_DELETED_ON_RELEASE = %+v, want the released microvm %s of %s",
			deleted, claim.VMUID, spec.Ref)
	}
	sandboxes, err := pm.host("host-a").Sandboxes()
	if err != nil {
		t.Fatalf("listing the host's sandboxes: %v", err)
	}
	for _, uid := range sandboxes {
		if uid == claim.VMUID {
			t.Errorf("the microvm %s is still on the host after the release", uid)
		}
	}
}

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//= type=test
//# If `ReleaseVM` returns `NOT_FOUND`, then the Scheduler SHALL treat the
//# release as complete.

// TestReleasingALeaseThatIsGoneSucceeds covers the Lease the expiry sweeper
// has already taken away: there is nothing left to release, so the release
// is complete rather than an error the Scheduler would retry.
func TestReleasingALeaseThatIsGoneSucceeds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	_, claim := claimOne(t, ctx, pm, c)

	releaser, err := poolmgr.NewReleaser(poolmgr.ReleaserConfig{
		Lease:      c,
		Clock:      clock.NewFake(testEpoch),
		Backoff:    clock.Zero{},
		RetryLimit: 2,
		Log:        testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewReleaser: %v", err)
	}
	if err := releaser.Release(ctx, claim.LeaseID); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	// The second release finds the lease gone.
	if err := releaser.Release(ctx, claim.LeaseID); err != nil {
		t.Errorf("releasing a lease that is already gone = %v, want it treated as complete", err)
	}
	if err := releaser.Release(ctx, "never-existed"); err != nil {
		t.Errorf("releasing an unknown lease = %v, want it treated as complete", err)
	}
}

//= docs/requirements/04-pool-manager.md#heartbeat-and-release
//= type=test
//# If `ReleaseVM` fails for any other reason, then the Scheduler SHALL
//# retry with exponential backoff up to the configured retry limit and
//# SHALL then log the Lease id and rely on Lease expiry.

// TestReleaseRetriesWithBackoffThenLogsTheLeaseID takes the Pool Manager
// away and checks the retries: each one waits the backoff the policy gives,
// there are as many as the configured limit allows, and the Lease id is
// logged when they run out. The second half brings the Pool Manager back
// between two retries and checks that the release then succeeds.
func TestReleaseRetriesWithBackoffThenLogsTheLeaseID(t *testing.T) {
	t.Parallel()

	t.Run("retries run out", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
		c := pm.client()
		_, claim := claimOne(t, ctx, pm, c)
		pm.setFaults(poolmgr.Faults{UnavailableFor: time.Hour})

		clk := clock.NewFake(testEpoch)
		logs := newLogRecorder(t)
		const limit = 3
		releaser, err := poolmgr.NewReleaser(poolmgr.ReleaserConfig{
			Lease:      c,
			Clock:      clk,
			Backoff:    clock.Exponential{Base: time.Second, Max: 4 * time.Second},
			RetryLimit: limit,
			Log:        logs.logger(),
		})
		if err != nil {
			t.Fatalf("NewReleaser: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- releaser.Release(ctx, claim.LeaseID) }()

		// One wait per retry, each the backoff for that attempt.
		for _, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			if err := clk.BlockUntil(ctx, 1); err != nil {
				t.Fatalf("waiting for the backoff timer: %v", err)
			}
			deadlines := clk.Deadlines()
			if len(deadlines) != 1 {
				t.Fatalf("the releaser armed %d timers, want one", len(deadlines))
			}
			if got := deadlines[0].Sub(clk.Now()); got != wait {
				t.Errorf("backoff = %s, want %s", got, wait)
			}
			clk.Advance(wait)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("Release succeeded while the pool manager was away")
			}
			if !errors.Is(err, poolmgr.ErrUnavailable) {
				t.Errorf("Release = %v, want it to wrap ErrUnavailable", err)
			}
			if !strings.Contains(err.Error(), claim.LeaseID) {
				t.Errorf("Release error = %v, want it to name lease %s", err, claim.LeaseID)
			}
		case <-ctx.Done():
			t.Fatal("Release did not give up")
		}
		if !logs.atLevel("ERROR", claim.LeaseID, "lease expiry") {
			t.Errorf("the lease id was not logged when the retries ran out; log was:\n%s", logs.text())
		}
	})

	t.Run("a retry succeeds", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
		c := pm.client()
		_, claim := claimOne(t, ctx, pm, c)
		pm.setFaults(poolmgr.Faults{UnavailableFor: time.Hour})

		clk := clock.NewFake(testEpoch)
		releaser, err := poolmgr.NewReleaser(poolmgr.ReleaserConfig{
			Lease:      c,
			Clock:      clk,
			Backoff:    clock.Exponential{Base: time.Second, Max: 4 * time.Second},
			RetryLimit: 5,
			Log:        testLogger(t),
		})
		if err != nil {
			t.Fatalf("NewReleaser: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- releaser.Release(ctx, claim.LeaseID) }()

		if err := clk.BlockUntil(ctx, 1); err != nil {
			t.Fatalf("waiting for the backoff timer: %v", err)
		}
		pm.setFaults(poolmgr.Faults{})
		clk.Advance(time.Second)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Release after the pool manager came back = %v, want it to succeed", err)
			}
		case <-ctx.Done():
			t.Fatal("Release did not finish")
		}
		if _, ok := leaseOf(pm, claim.LeaseID); ok {
			t.Errorf("the pool manager still holds lease %s", claim.LeaseID)
		}
	})
}

// TestPackageNeverCallsDeleteMicroVM is the structural half of PL-044: no
// source file of this package names a Host's delete, and none of them so
// much as mentions the Host client interface that has one. The Runner side
// releases Leases; deleting the MicroVM is the Pool Manager's job, and the
// only code here that holds a flintlock.PoolHostClient is the fake Pool
// Manager in the subpackage, which is a Pool Manager, not the Runner.
func TestPackageNeverCallsDeleteMicroVM(t *testing.T) {
	t.Parallel()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package's sources: %v", err)
	}
	// No source may name a Host's create or delete at all. The Host client
	// interface that has them may only be named by interfaces.go, and there
	// only as the HostSource seam the fake Pool Manager in the subpackage
	// takes: a Pool Manager creates and deletes MicroVMs, the Runner does
	// not (HO-007).
	forbidden := []string{"DeleteMicroVM", "CreateMicroVM", "HostAdminClient"}
	hostClientAllowedIn := map[string]bool{"interfaces.go": true}
	checked := 0
	fset := token.NewFileSet()
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			for _, bad := range forbidden {
				if ident.Name == bad {
					t.Errorf("%s names %s; the runner never creates or deletes a microvm", path, bad)
				}
			}
			if ident.Name == "PoolHostClient" && !hostClientAllowedIn[filepath.Base(path)] {
				t.Errorf("%s holds a flintlock.PoolHostClient; nothing on the runner side may (HO-007)", path)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source file was parsed; the guard would pass vacuously")
	}
}
