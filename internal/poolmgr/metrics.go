package poolmgr

import "github.com/prometheus/client_golang/prometheus"

// availableDesc is the descriptor of the available warm MicroVM gauge. The
// labels are the Pool's name and namespace, which together identify it.
var availableDesc = prometheus.NewDesc(
	"flintlock_runner_pool_available_microvms",
	"Warm MicroVMs available in a Pool, as the Runner currently counts them.",
	[]string{"pool", "namespace"},
	nil,
)

//= docs/requirements/04-pool-manager.md#capacity-tracking
//# The Scheduler SHALL expose each Pool's available count as a metric.

// AvailableCollector exposes the available count of every tracked Pool as a
// Prometheus gauge. It collects from the Tracker on scrape rather than
// mirroring its state into a GaugeVec, so the number a scrape reports is
// the number the capacity computation would use at that moment, and a Pool
// that stops being tracked stops being reported instead of being left at
// its last value. Register it with the Runner's registry (OB-010).
type AvailableCollector struct {
	tracker Tracker
}

// NewAvailableCollector returns a collector over t.
func NewAvailableCollector(t Tracker) *AvailableCollector {
	return &AvailableCollector{tracker: t}
}

// Describe implements prometheus.Collector.
func (c *AvailableCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- availableDesc
}

// Collect implements prometheus.Collector.
func (c *AvailableCollector) Collect(ch chan<- prometheus.Metric) {
	for _, pool := range c.tracker.Pools() {
		ch <- prometheus.MustNewConstMetric(
			availableDesc,
			prometheus.GaugeValue,
			float64(pool.Status.Available),
			pool.Pool.Name,
			pool.Pool.Namespace,
		)
	}
}

// Compile-time interface check.
var _ prometheus.Collector = (*AvailableCollector)(nil)
