package config

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// kubernetesConfig is testdata/minimal.yaml turned into the smallest
// configuration of a cluster fleet: the Kubernetes pool backend, no battery
// endpoint and no Inventory.
func kubernetesConfig(t *testing.T, section string) string {
	t.Helper()
	data := strings.ReplaceAll(minimalConfig(t), inlineInventory, "")
	return strings.ReplaceAll(data, "pool_manager:\n  endpoint: 10.0.0.5:9091\n", "pool_manager:\n  backend: kubernetes\n"+section)
}

// TestBatteryIsTheDefaultBackend checks that a configuration written before
// the backend selector existed still means battery, and that defaulting it
// leaves no Kubernetes section behind.
func TestBatteryIsTheDefaultBackend(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(minimalConfig(t)), t.TempDir(), WithEnv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolManager.IsKubernetes() {
		t.Error("a configuration with no backend selects the Kubernetes pool backend")
	}
	if cfg.PoolManager.Kubernetes != nil {
		t.Errorf("pool_manager.kubernetes = %+v, want it left unset for battery", cfg.PoolManager.Kubernetes)
	}

	explicit := strings.ReplaceAll(minimalConfig(t), "pool_manager:\n", "pool_manager:\n  backend: battery\n")
	if _, err := Parse([]byte(explicit), t.TempDir(), WithEnv(noEnv)); err != nil {
		t.Errorf("backend: battery is rejected: %v", err)
	}
}

// TestKubernetesBackendDefaults checks that selecting the backend is enough:
// no endpoint, no Inventory and no kubernetes section are needed, and every
// setting takes its default.
func TestKubernetesBackendDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(kubernetesConfig(t, "")), t.TempDir(), WithEnv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PoolManager.IsKubernetes() {
		t.Fatal("backend: kubernetes does not select the Kubernetes pool backend")
	}
	k := cfg.PoolManager.Kubernetes
	if k == nil {
		t.Fatal("pool_manager.kubernetes is nil after defaulting")
	}
	if k.JobTimeout != DefaultKubernetesJobTimeout {
		t.Errorf("job_timeout = %s, want %s", k.JobTimeout, DefaultKubernetesJobTimeout)
	}
	if k.CleanupMargin != DefaultKubernetesCleanupMargin {
		t.Errorf("cleanup_margin = %s, want %s", k.CleanupMargin, DefaultKubernetesCleanupMargin)
	}
	if k.RolloutInterval != DefaultKubernetesRolloutInterval {
		t.Errorf("rollout_interval = %s, want %s", k.RolloutInterval, DefaultKubernetesRolloutInterval)
	}
	if k.Kubeconfig != "" || k.Namespace != "" {
		t.Errorf("kubeconfig = %q, namespace = %q, want both empty for the in-cluster configuration", k.Kubeconfig, k.Namespace)
	}
}

// TestKubernetesBackendSettings checks that every setting is read from the
// file.
func TestKubernetesBackendSettings(t *testing.T) {
	t.Parallel()
	section := `  kubernetes:
    kubeconfig: /etc/flintlock-runner/kubeconfig
    context: fleet
    namespace: ci-runners
    job_timeout: 90m
    cleanup_margin: 20m
    rollout_interval: 3s
    cloud_init_config_maps:
      default: default-cloud-init
`
	cfg, err := Parse([]byte(kubernetesConfig(t, section)), t.TempDir(), WithEnv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	k := cfg.PoolManager.Kubernetes
	if k.Kubeconfig != "/etc/flintlock-runner/kubeconfig" || k.Context != "fleet" || k.Namespace != "ci-runners" {
		t.Errorf("kubeconfig, context, namespace = %q, %q, %q", k.Kubeconfig, k.Context, k.Namespace)
	}
	if k.JobTimeout != 90*time.Minute || k.CleanupMargin != 20*time.Minute || k.RolloutInterval != 3*time.Second {
		t.Errorf("job_timeout, cleanup_margin, rollout_interval = %s, %s, %s", k.JobTimeout, k.CleanupMargin, k.RolloutInterval)
	}
	if got := k.CloudInitConfigMaps["default"]; got != "default-cloud-init" {
		t.Errorf("cloud_init_config_maps[default] = %q", got)
	}
}

// TestKubernetesBackendRejected checks the validation of the backend
// selector and of the Kubernetes section.
func TestKubernetesBackendRejected(t *testing.T) {
	t.Parallel()
	kube := func(mutate func(*KubernetesPools)) func(*Config) {
		return func(c *Config) {
			c.PoolManager.Backend = PoolBackendKubernetes
			for i := range c.Profiles {
				c.Profiles[i].Transport = Transport{Kind: TransportKubeExec}
			}
			c.PoolManager.Kubernetes = &KubernetesPools{
				JobTimeout:      time.Hour,
				CleanupMargin:   time.Minute,
				RolloutInterval: time.Second,
			}
			mutate(c.PoolManager.Kubernetes)
		}
	}
	const f = "pool_manager.kubernetes"
	runRejectCases(t, []rejectCase{
		{"unknown backend", func(c *Config) { c.PoolManager.Backend = "nomad" }, "pool_manager.backend", "must be"},
		{
			"kubernetes section under battery",
			func(c *Config) { c.PoolManager.Kubernetes = &KubernetesPools{} },
			f, "is only read with",
		},
		{
			"battery without an inventory",
			func(c *Config) { c.Inventory = Inventory{} },
			"inventory", "at least one Host is required",
		},
		{"relative kubeconfig", kube(func(k *KubernetesPools) { k.Kubeconfig = "kubeconfig" }), f + ".kubeconfig", "absolute path"},
		{"context without kubeconfig", kube(func(k *KubernetesPools) { k.Context = "fleet" }), f + ".context", "needs"},
		{"namespace that is no label", kube(func(k *KubernetesPools) { k.Namespace = "CI_Runners" }), f + ".namespace", "RFC 1123 label"},
		{"job timeout of zero", kube(func(k *KubernetesPools) { k.JobTimeout = 0 }), f + ".job_timeout", "positive duration"},
		{"job timeout under a second", kube(func(k *KubernetesPools) { k.JobTimeout = time.Millisecond }), f + ".job_timeout", "at least one second"},
		{"negative cleanup margin", kube(func(k *KubernetesPools) { k.CleanupMargin = -time.Minute }), f + ".cleanup_margin", "positive duration"},
		{"rollout interval of zero", kube(func(k *KubernetesPools) { k.RolloutInterval = 0 }), f + ".rollout_interval", "positive duration"},
		{
			"cloud-init ConfigMap for no profile",
			kube(func(k *KubernetesPools) { k.CloudInitConfigMaps = map[string]string{"no-such-profile": "cm"} }),
			f + ".cloud_init_config_maps[no-such-profile]", "names no profile",
		},
	})

	// The Kubernetes backend needs neither an endpoint nor an Inventory, but
	// an endpoint that is given is still checked.
	cfg := fullWith(t, kube(func(*KubernetesPools) {}))
	cfg.PoolManager.Endpoint = ""
	cfg.Inventory = Inventory{}
	if errs := fieldErrors(t, cfg); errs != nil {
		t.Errorf("kubernetes backend without endpoint and inventory: %v", errs)
	}
	cfg.PoolManager.Endpoint = "https://10.0.0.5:9091"
	if errs := fieldErrors(t, cfg); !hasFieldError(errs, "pool_manager.endpoint", "without a scheme") {
		t.Errorf("errors = %v, want pool_manager.endpoint checked", errs)
	}

	cfg = fullWith(t, kube(func(*KubernetesPools) {}))
	cfg.PoolManager.Kubernetes.CloudInitConfigMaps = map[string]string{cfg.Profiles[0].Name: "Not_A_Name"}
	field := f + ".cloud_init_config_maps[" + cfg.Profiles[0].Name + "]"
	if errs := fieldErrors(t, cfg); !hasFieldError(errs, field, "RFC 1123 subdomain") {
		t.Errorf("errors = %v, want %s rejected", errs, field)
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-allocation
//= type=test
//# Where the Kubernetes pool backend is configured, the Executor
//# SHALL use the `kube-exec` Guest Transport for every Profile, and the
//# Runner SHALL reject a configuration that names any other Guest Transport.

// TestKubernetesBackendUsesKubeExec checks the configuration's half of
// KF-128: with the Kubernetes pool backend, a Profile that names no Guest
// Transport gets kube-exec, one that names kube-exec keeps it, and one that
// names exec or ssh is refused rather than run over something else. The
// other way round, kube-exec is refused under battery, whose MicroVMs are
// no pods.
func TestKubernetesBackendUsesKubeExec(t *testing.T) {
	t.Parallel()
	named := strings.ReplaceAll(kubernetesConfig(t, ""), "profiles:\n  - name: default\n",
		"profiles:\n  - name: default\n    transport:\n      kind: kube-exec\n")
	for what, data := range map[string]string{"unnamed": kubernetesConfig(t, ""), "named": named} {
		cfg, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
		if err != nil {
			t.Fatalf("%s transport: %v", what, err)
		}
		if got := cfg.Profiles[0].Transport.Kind; got != TransportKubeExec {
			t.Errorf("%s transport: kind = %q, want %q", what, got, TransportKubeExec)
		}
	}

	for _, kind := range []TransportKind{TransportExec, TransportSSH} {
		data := strings.ReplaceAll(kubernetesConfig(t, ""), "profiles:\n  - name: default\n",
			"profiles:\n  - name: default\n    transport:\n      kind: "+string(kind)+"\n      ssh:\n        private_key_file: /etc/key\n")
		_, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
		var verr *ValidationError
		if !errors.As(err, &verr) || !hasFieldError(verr.Errors, "profiles[0].transport.kind", "must be kube-exec") {
			t.Errorf("transport %s with the kubernetes backend: %v, want profiles[0].transport.kind rejected", kind, err)
		}
	}

	runRejectCases(t, []rejectCase{
		{
			"kube-exec under battery",
			func(c *Config) { c.Profiles[0].Transport = Transport{Kind: TransportKubeExec} },
			"profiles[0].transport.kind", "needs pool_manager.backend",
		},
		{
			"an unknown transport",
			func(c *Config) { c.Profiles[0].Transport = Transport{Kind: "telnet"} },
			"profiles[0].transport.kind", "must be exec or ssh",
		},
	})
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# Where the `kube-exec` Guest Transport is configured, the Runner
//# SHALL NOT open any connection to a Host.

// TestKubernetesBackendRefusesAnInventory checks that a cluster fleet's
// configuration cannot list a Host to dial: with the Kubernetes pool
// backend an Inventory is an error rather than a set of flintlockd
// endpoints quietly ignored or, worse, connected to.
func TestKubernetesBackendRefusesAnInventory(t *testing.T) {
	t.Parallel()
	cfg := fullWith(t, func(c *Config) {
		c.PoolManager.Backend = PoolBackendKubernetes
		c.PoolManager.Kubernetes = &KubernetesPools{JobTimeout: time.Hour, CleanupMargin: time.Minute, RolloutInterval: time.Second}
		for i := range c.Profiles {
			c.Profiles[i].Transport = Transport{Kind: TransportKubeExec}
		}
	})
	if len(cfg.Inventory.Hosts) == 0 {
		t.Fatal("testdata/full.yaml has no inventory to refuse")
	}
	if errs := fieldErrors(t, cfg); !hasFieldError(errs, "inventory", "reaches no Host") {
		t.Errorf("errors = %v, want the inventory refused", errs)
	}
}

// TestHostServiceKeysAreTheCanonicalNames checks that the keys of the
// host_services section are the Host Service names of kubelabels, which the
// Pod Provider publishes under and the Executor reads a Virtual Node by
// (KF-017, KF-062): a key renamed here without them would leave a cluster
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
