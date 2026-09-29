package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// TestBatteryIsTheDefaultBackend checks that a configuration written before
// the backend selector existed still means battery, and that defaulting it
// leaves no claim section behind.
func TestBatteryIsTheDefaultBackend(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(minimalConfig(t)), t.TempDir(), WithEnv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolManager.IsClaim() {
		t.Error("a configuration with no backend selects the claim pool backend")
	}
	if cfg.PoolManager.Claim != nil {
		t.Errorf("pool_manager.claim = %+v, want it left unset for battery", cfg.PoolManager.Claim)
	}

	explicit := strings.ReplaceAll(minimalConfig(t), "pool_manager:\n", "pool_manager:\n  backend: battery\n")
	if _, err := Parse([]byte(explicit), t.TempDir(), WithEnv(noEnv)); err != nil {
		t.Errorf("backend: battery is rejected: %v", err)
	}
}

// TestKubernetesBackendIsGone checks that the Kubernetes pool backend of the
// Virtual Node design, and its kube-exec Guest Transport, are refused like
// any other unknown value.
func TestKubernetesBackendIsGone(t *testing.T) {
	t.Parallel()
	runRejectCases(t, []rejectCase{
		{"backend kubernetes", func(c *Config) { c.PoolManager.Backend = "kubernetes" }, "pool_manager.backend", `must be "battery" or "claim"`},
		{"transport kube-exec", func(c *Config) { c.Profiles[0].Transport = Transport{Kind: "kube-exec"} }, "profiles[0].transport.kind", "must be exec or ssh"},
	})
}

// TestHostServiceKeysAreTheCanonicalNames checks that the keys of the
// host_services section are the Host Service names of kubelabels, which the
// Host Agent publishes under and the Executor reads a Host's Node by
// (KF-189, KF-194): a key renamed here without them would leave a cluster
// fleet's Jobs without that service.
func TestHostServiceKeysAreTheCanonicalNames(t *testing.T) {
	t.Parallel()
	var keys []string
	typ := reflect.TypeFor[HostServices]()
	for i := range typ.NumField() {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if key != "cache_volume" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if want := kubelabels.HostServiceNames(); !reflect.DeepEqual(keys, want) {
		t.Errorf("host_services keys = %v, want the canonical names %v", keys, want)
	}
}
