package poolmgr_test

import (
	"context"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/04-pool-manager.md#client
//= type=test
//# If the Pool Manager is unreachable at startup, then the Scheduler SHALL
//# keep retrying the connection with exponential backoff and the Runner
//# SHALL NOT report itself ready until it succeeds.

// TestUnreachableAtStartupIsRetriedWithBackoff starts the health monitor
// against a Pool Manager that cannot be reached and checks the three things
// that follow: it keeps trying, the waits between the attempts grow
// exponentially, and Contacted -- the channel the readiness endpoint waits
// on before the Runner reports itself ready (OB-031) -- stays open until an
// attempt succeeds.
func TestUnreachableAtStartupIsRetriedWithBackoff(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	gate := newGateAdmin(pm.client(), true)
	clk := clock.NewFake(testEpoch)
	health := newHealth(t, gate, clk, func(cfg *poolmgr.HealthConfig) {
		cfg.Backoff = clock.Exponential{Base: time.Second, Max: 4 * time.Second}
	})
	runInBackground(t, "Health.Run", health.Run)

	for _, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		gate.awaitCall(t, ctx)
		if err := clk.BlockUntil(ctx, 1); err != nil {
			t.Fatalf("waiting for the backoff timer: %v", err)
		}
		deadlines := clk.Deadlines()
		if len(deadlines) != 1 {
			t.Fatalf("the health monitor armed %d timers, want one", len(deadlines))
		}
		if got := deadlines[0].Sub(clk.Now()); got != wait {
			t.Errorf("backoff = %s, want %s", got, wait)
		}
		select {
		case <-health.Contacted():
			t.Fatal("Contacted was closed although no call has succeeded")
		default:
		}
		if health.Healthy() {
			t.Error("the pool manager counts as healthy before it has ever answered")
		}
		clk.Advance(wait)
	}

	// The moment the Pool Manager answers, the Runner may report itself
	// ready.
	gate.set(false)
	awaitContact(t, ctx, health)
	if !health.Healthy() {
		t.Error("the pool manager is not healthy after a successful call")
	}
}

//= docs/requirements/04-pool-manager.md#client
//= type=test
//# The Scheduler SHALL probe the Pool Manager at the configured health
//# interval by calling `ListPools` and SHALL mark it unhealthy after the
//# configured number of consecutive failures and healthy again after a
//# successful probe.

// TestProbesMarkUnhealthyAfterTheThresholdAndHealthyAgain drives the probe
// loop over a real ListPools: the probes run at the configured interval,
// two failures are not enough to make the Pool Manager unhealthy when the
// threshold is three, the third one is, a success makes it healthy again,
// and the count starts from zero after it.
func TestProbesMarkUnhealthyAfterTheThresholdAndHealthyAgain(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	const threshold = 3
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	gate := newGateAdmin(pm.client(), false)
	clk := clock.NewFake(testEpoch)
	health := newHealth(t, gate, clk, func(cfg *poolmgr.HealthConfig) {
		cfg.FailureThreshold = threshold
		cfg.Interval = healthInterval
	})
	runInBackground(t, "Health.Run", health.Run)
	awaitContact(t, ctx, health)

	// Every probe is one interval after the last, and ListPools is the
	// probe: the gate counts the calls that reached the Pool Manager.
	gate.set(true)
	for probe := 1; probe <= threshold; probe++ {
		probeOnce(t, ctx, clk, gate)
		wantHealthy := probe < threshold
		if got := health.Healthy(); got != wantHealthy {
			t.Errorf("after %d consecutive failures healthy = %v, want %v", probe, got, wantHealthy)
		}
	}

	// One good probe is enough to come back.
	gate.set(false)
	probeOnce(t, ctx, clk, gate)
	if !health.Healthy() {
		t.Fatal("the pool manager is still unhealthy after a successful probe")
	}

	// The failure count started again, so two failures are not enough.
	gate.set(true)
	for probe := 1; probe < threshold; probe++ {
		probeOnce(t, ctx, clk, gate)
		if !health.Healthy() {
			t.Errorf("the pool manager is unhealthy after %d failures since the last success, want the count to have restarted", probe)
		}
	}
}

// probeOnce fires the health monitor's timer and waits until the probe it
// triggered has been answered and the next one armed.
func probeOnce(t *testing.T, ctx context.Context, clk *clock.Fake, gate *gateAdmin) {
	t.Helper()
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the probe timer: %v", err)
	}
	clk.Advance(healthInterval)
	gate.awaitCall(t, ctx)
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the probe to finish: %v", err)
	}
}

// TestContactedClosesOnceAndStaysClosed covers the contract the Scheduler
// relies on (SC-008): the channel is closed by the first success and never
// reopens, however unhealthy the Pool Manager becomes afterwards.
func TestContactedClosesOnceAndStaysClosed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	gate := newGateAdmin(pm.client(), false)
	clk := clock.NewFake(testEpoch)
	health := newHealth(t, gate, clk, func(cfg *poolmgr.HealthConfig) {
		cfg.FailureThreshold = 1
	})
	runInBackground(t, "Health.Run", health.Run)
	awaitContact(t, ctx, health)
	first := health.Contacted()

	gate.set(true)
	probeOnce(t, ctx, clk, gate)
	if health.Healthy() {
		t.Error("the pool manager is healthy after the threshold was reached")
	}
	select {
	case <-health.Contacted():
	default:
		t.Error("Contacted reopened when the pool manager became unhealthy")
	}
	if health.Contacted() != first {
		t.Error("Contacted returned a different channel on the second call")
	}

	health.MarkUnavailable()
	select {
	case <-health.Contacted():
	default:
		t.Error("Contacted reopened after MarkUnavailable")
	}
}

// TestHealthRejectsAnIncompleteConfiguration covers the one dependency the
// health monitor cannot do without.
func TestHealthRejectsAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := poolmgr.NewHealth(poolmgr.HealthConfig{}); err == nil {
		t.Error("NewHealth accepted a configuration with no PoolAdmin client")
	}
}
