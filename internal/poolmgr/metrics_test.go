package poolmgr_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/04-pool-manager.md#capacity-tracking
//= type=test
//# The Scheduler SHALL expose each Pool's available count as a metric.

// TestAvailableCountIsExposedAsAMetric registers the collector with a
// Prometheus registry and gathers it as a scrape would, at three points: a
// full Pool, the same Pool after a claim, and after the Pool stops being
// tracked.
func TestAvailableCountIsExposedAsAMetric(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	spec := specFor(t, testProfile("small", 2), "host-a")
	pm.fillPool(c, spec)

	tracker := newTracker(t, c, nil, trackerPollInterval, nil)
	tracker.run(t)
	tracker.Track(spec.Ref, true)
	tracker.await(t, ctx, "both warm microvms to be counted", func() bool {
		return tracker.Available(spec.Ref) == 2
	})

	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(poolmgr.NewAvailableCollector(tracker)); err != nil {
		t.Fatalf("registering the collector: %v", err)
	}

	const name = "flintlock_runner_pool_available_microvms"
	full := `
# HELP flintlock_runner_pool_available_microvms Warm MicroVMs available in a Pool, as the Runner currently counts them.
# TYPE flintlock_runner_pool_available_microvms gauge
flintlock_runner_pool_available_microvms{namespace="runner-ns",pool="small"} 2
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(full), name); err != nil {
		t.Errorf("scrape of a full pool: %v", err)
	}

	pm.host("host-a").SetFaults(flintlock.HostFaults{CreateFails: true})
	if _, err := c.ClaimVM(ctx, spec.Ref); err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	tracker.await(t, ctx, "the claim to be counted", func() bool {
		return tracker.Available(spec.Ref) == 1
	})
	claimed := `
# HELP flintlock_runner_pool_available_microvms Warm MicroVMs available in a Pool, as the Runner currently counts them.
# TYPE flintlock_runner_pool_available_microvms gauge
flintlock_runner_pool_available_microvms{namespace="runner-ns",pool="small"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(claimed), name); err != nil {
		t.Errorf("scrape after a claim: %v", err)
	}

	// A Pool that is no longer tracked stops being reported rather than
	// being left at its last value.
	tracker.Untrack(spec.Ref)
	if got := testutil.CollectAndCount(poolmgr.NewAvailableCollector(tracker), name); got != 0 {
		t.Errorf("%d series after untracking the only pool, want none", got)
	}
}
