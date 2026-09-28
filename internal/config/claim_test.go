package config

import (
	"strings"
	"testing"
)

// claimConfig is testdata/minimal.yaml turned into the smallest
// configuration of the claim backend: no battery endpoint, no Inventory,
// and the claim section given.
func claimConfig(t *testing.T, section string) string {
	t.Helper()
	data := strings.ReplaceAll(minimalConfig(t), inlineInventory, "")
	return strings.ReplaceAll(data, "pool_manager:\n  endpoint: 10.0.0.5:9091\n", "pool_manager:\n  backend: claim\n"+section)
}

// TestClaimBackendDefaults checks that the backend and its Holder are
// enough: no endpoint and no Inventory are needed, the serving CA takes
// battery-operator's names, and the namespace is left for the Runner to
// resolve where it runs.
func TestClaimBackendDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(claimConfig(t, "  claim:\n    holder_service_account: runner\n")), t.TempDir(), WithEnv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PoolManager.IsClaim() || cfg.PoolManager.IsKubernetes() {
		t.Fatal("backend: claim does not select the claim pool backend alone")
	}
	if cfg.PoolManager.Kubernetes != nil {
		t.Errorf("pool_manager.kubernetes = %+v, want it left unset for the claim backend", cfg.PoolManager.Kubernetes)
	}
	k := cfg.PoolManager.Claim
	if k.HolderServiceAccount != "runner" {
		t.Errorf("holder_service_account = %q, want runner", k.HolderServiceAccount)
	}
	want := ServingCAConfigMap{Namespace: "battery-operator-system", Name: "flintlockd-ca", Key: "serving-ca.crt"}
	if k.ServingCA != want {
		t.Errorf("serving_ca = %+v, want %+v", k.ServingCA, want)
	}
	if k.Kubeconfig != "" || k.Context != "" || k.Namespace != "" {
		t.Errorf("kubeconfig, context, namespace = %q, %q, %q; want all empty", k.Kubeconfig, k.Context, k.Namespace)
	}
	// Profiles keep the exec default: the Executor runs them over
	// agent-exec whatever they name (KF-185).
	if got := cfg.Profiles[0].Transport.Kind; got != TransportExec {
		t.Errorf("transport kind = %q, want %q", got, TransportExec)
	}
}

// TestClaimBackendSettings checks that every setting is read from the file.
func TestClaimBackendSettings(t *testing.T) {
	t.Parallel()
	section := `  claim:
    kubeconfig: /etc/flintlock-runner/kubeconfig
    context: fleet
    namespace: ci-runners
    holder_service_account: flintlock-runner
    serving_ca:
      namespace: operators
      name: exec-agent-ca
      key: ca.pem
`
	cfg, err := Parse([]byte(claimConfig(t, section)), t.TempDir(), WithEnv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	k := cfg.PoolManager.Claim
	if k.Kubeconfig != "/etc/flintlock-runner/kubeconfig" || k.Context != "fleet" || k.Namespace != "ci-runners" {
		t.Errorf("kubeconfig, context, namespace = %q, %q, %q", k.Kubeconfig, k.Context, k.Namespace)
	}
	if k.HolderServiceAccount != "flintlock-runner" {
		t.Errorf("holder_service_account = %q", k.HolderServiceAccount)
	}
	if want := (ServingCAConfigMap{Namespace: "operators", Name: "exec-agent-ca", Key: "ca.pem"}); k.ServingCA != want {
		t.Errorf("serving_ca = %+v, want %+v", k.ServingCA, want)
	}
}

// TestClaimBackendRejected checks the validation of the claim section, and
// that it is read only with its backend.
func TestClaimBackendRejected(t *testing.T) {
	t.Parallel()
	claim := func(mutate func(*ClaimPools)) func(*Config) {
		return func(c *Config) {
			c.PoolManager.Backend = PoolBackendClaim
			c.Inventory = Inventory{}
			c.PoolManager.Claim = &ClaimPools{
				HolderServiceAccount: "runner",
				ServingCA:            ServingCAConfigMap{Namespace: "battery-operator-system", Name: "flintlockd-ca", Key: "serving-ca.crt"},
			}
			// full.yaml has an ssh Profile, which the claim backend
			// refuses (KF-195).
			execOnly(c)
			mutate(c.PoolManager.Claim)
		}
	}
	const f = "pool_manager.claim"
	runRejectCases(t, []rejectCase{
		{"claim section under battery", func(c *Config) { c.PoolManager.Claim = &ClaimPools{} }, f, "is only read with"},
		{
			"claim section under kubernetes",
			func(c *Config) {
				c.PoolManager.Backend = PoolBackendKubernetes
				c.PoolManager.Claim = &ClaimPools{}
			},
			f, "is only read with",
		},
		{
			"kubernetes section under claim",
			func(c *Config) {
				claim(func(*ClaimPools) {})(c)
				c.PoolManager.Kubernetes = &KubernetesPools{}
			},
			"pool_manager.kubernetes", "is only read with",
		},
		{
			"no claim section",
			func(c *Config) {
				claim(func(*ClaimPools) {})(c)
				c.PoolManager.Claim = nil
			},
			f + ".holder_service_account", "is required",
		},
		{"no holder", claim(func(k *ClaimPools) { k.HolderServiceAccount = "" }), f + ".holder_service_account", "is required"},
		{"holder that is no name", claim(func(k *ClaimPools) { k.HolderServiceAccount = "Runner_SA" }), f + ".holder_service_account", "RFC 1123 subdomain"},
		{"relative kubeconfig", claim(func(k *ClaimPools) { k.Kubeconfig = "kubeconfig" }), f + ".kubeconfig", "absolute path"},
		{"context without kubeconfig", claim(func(k *ClaimPools) { k.Context = "fleet" }), f + ".context", "needs"},
		{"namespace that is no label", claim(func(k *ClaimPools) { k.Namespace = "CI_Runners" }), f + ".namespace", "RFC 1123 label"},
		{"no serving CA namespace", claim(func(k *ClaimPools) { k.ServingCA.Namespace = "" }), f + ".serving_ca.namespace", "is required"},
		{"serving CA namespace that is no label", claim(func(k *ClaimPools) { k.ServingCA.Namespace = "a.b" }), f + ".serving_ca.namespace", "RFC 1123 label"},
		{"serving CA ConfigMap that is no name", claim(func(k *ClaimPools) { k.ServingCA.Name = "Not_A_Name" }), f + ".serving_ca.name", "RFC 1123 subdomain"},
		{"serving CA key that is no key", claim(func(k *ClaimPools) { k.ServingCA.Key = "ca/crt" }), f + ".serving_ca.key", "ConfigMap key"},
		{
			"an inventory under claim",
			func(c *Config) {
				inv := c.Inventory
				claim(func(*ClaimPools) {})(c)
				c.Inventory = inv
			},
			"inventory", "name their Hosts",
		},
		{
			"kube-exec under claim",
			func(c *Config) {
				claim(func(*ClaimPools) {})(c)
				c.Profiles[0].Transport = Transport{Kind: TransportKubeExec}
			},
			"profiles[0].transport.kind", "needs pool_manager.backend",
		},
	})

	// The claim backend needs neither an endpoint nor an Inventory, but an
	// endpoint that is given is still checked.
	cfg := fullWith(t, claim(func(*ClaimPools) {}))
	cfg.PoolManager.Endpoint = ""
	if errs := fieldErrors(t, cfg); errs != nil {
		t.Errorf("claim backend without endpoint and inventory: %v", errs)
	}
	cfg.PoolManager.Endpoint = "https://10.0.0.5:9091"
	if errs := fieldErrors(t, cfg); !hasFieldError(errs, "pool_manager.endpoint", "without a scheme") {
		t.Errorf("errors = %v, want pool_manager.endpoint checked", errs)
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# Where the claim backend is configured, the Runner SHALL
//# reject a configuration in which a Profile names the `ssh` Guest
//# Transport.

// TestClaimBackendRefusesSSH checks that a Profile that names ssh is
// refused under the claim backend, because battery-operator's Exec Agent
// relays exec only, and that exec is still accepted there. The same ssh
// Profile loads under battery.
func TestClaimBackendRefusesSSH(t *testing.T) {
	t.Parallel()
	ssh := Transport{Kind: TransportSSH, SSH: SSHTransport{PrivateKeyFile: "/etc/flintlock-runner/id_ed25519", User: "runner"}}
	claim := func(c *Config) {
		c.PoolManager.Backend = PoolBackendClaim
		c.PoolManager.Endpoint = ""
		c.Inventory = Inventory{}
		c.PoolManager.Claim = &ClaimPools{
			HolderServiceAccount: "runner",
			ServingCA:            ServingCAConfigMap{Namespace: "battery-operator-system", Name: "flintlockd-ca", Key: "serving-ca.crt"},
		}
		execOnly(c)
	}
	runRejectCases(t, []rejectCase{{
		"ssh under claim",
		func(c *Config) {
			claim(c)
			c.Profiles[0].Transport = ssh
		},
		"profiles[0].transport.kind", "relays exec only",
	}})

	cfg := fullWith(t, func(c *Config) {
		claim(c)
		c.Profiles[0].Transport = Transport{Kind: TransportExec}
	})
	if errs := fieldErrors(t, cfg); errs != nil {
		t.Errorf("exec under claim: %v", errs)
	}
	cfg = fullWith(t, func(c *Config) { c.Profiles[0].Transport = ssh })
	if errs := fieldErrors(t, cfg); errs != nil {
		t.Errorf("ssh under battery: %v", errs)
	}
}

// execOnly moves every ssh Profile of c to exec, for a configuration of the
// claim backend.
func execOnly(c *Config) {
	for i := range c.Profiles {
		if c.Profiles[i].Transport.Kind == TransportSSH {
			c.Profiles[i].Transport = Transport{Kind: TransportExec}
		}
	}
}
