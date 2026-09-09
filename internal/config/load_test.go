package config

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// noEnv is an environment with nothing set.
func noEnv(string) (string, bool) { return "", false }

// mapEnv is an environment backed by a map.
func mapEnv(m map[string]string) LookupEnv {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// loadTestdata loads testdata/<name> with no environment and a discarding
// logger, failing the test on error.
func loadTestdata(t *testing.T, name string, opts ...Option) *Config {
	t.Helper()
	opts = append([]Option{WithEnv(noEnv), WithLogger(slog.New(slog.DiscardHandler))}, opts...)
	cfg, err := Load(filepath.Join("testdata", name), opts...)
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	return cfg
}

// captureLogger returns a text logger writing to buf.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# The Runner SHALL read its configuration from a single YAML file
//# whose path is given by the `--config` flag or the `FLINTLOCK_RUNNER_CONFIG`
//# environment variable.

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# A Profile MAY declare image names and image glob patterns that
//# map Job Images onto it, a kernel command line, an initrd image, additional
//# volumes, a hypervisor provider, cloud-init user-data, a Guest Transport, a
//# user, a ready timeout, a Host selector and a maximum concurrency.

// TestLoadFull reads the complete example and checks that every section,
// including every optional Profile field of CF-021, arrives as written.
func TestLoadFull(t *testing.T) {
	t.Parallel()
	cfg := loadTestdata(t, "full.yaml")

	if cfg.GitLab.URL != "https://gitlab.example.com" || string(cfg.GitLab.Token) != "glrt-file-token" {
		t.Errorf("gitlab section = %+v", cfg.GitLab)
	}
	if cfg.GitLab.Concurrent != 8 || cfg.GitLab.CheckInterval != 5*time.Second || cfg.GitLab.OutputLimitKB != 8192 {
		t.Errorf("gitlab limits = %+v", cfg.GitLab)
	}
	if len(cfg.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(cfg.Profiles))
	}
	p := cfg.Profiles[0]
	checks := []struct {
		name string
		ok   bool
	}{
		{"images", len(p.Images) == 2 && p.Images[1] == "ubuntu:24.04"},
		{"image_globs", len(p.ImageGlobs) == 1 && p.ImageGlobs[0] == "ubuntu:*"},
		{"kernel cmdline", p.Kernel.Cmdline["console"] == "ttyS0"},
		{"initrd", p.Initrd != nil && p.Initrd.Image == "ghcr.io/example/initrd:6.1"},
		{"volumes", len(p.AdditionalVolumes) == 2 && p.AdditionalVolumes[1].SizeMB == 20480 && p.AdditionalVolumes[0].ReadOnly},
		{"provider", p.Provider == "firecracker"},
		{"user_data", strings.HasPrefix(p.UserData, "#cloud-config")},
		{"transport", p.Transport.Kind == TransportExec},
		{"user", p.User == "root"},
		{"ready_timeout", p.ReadyTimeout == 90*time.Second},
		{"host_selector", p.HostSelector["tier"] == "general"},
		{"max_concurrency", p.MaxConcurrency == 6},
		{"default", p.Default},
		{"pool", p.Pool.Strategy == ReplenishMinSizeThreshold && p.Pool.MinSize != nil && *p.Pool.MinSize == 1 && p.Pool.HookFailurePolicy == HookFailureQuarantine},
		{"ssh transport", cfg.Profiles[1].Transport.Kind == TransportSSH && cfg.Profiles[1].Transport.SSH.User == "ci"},
		{"inventory", len(cfg.Inventory.Hosts) == 2 && cfg.Inventory.Hosts[0].Services.HTTPCache["npm"] != ""},
		{"pool manager", cfg.PoolManager.Endpoint == "10.0.0.5:9091" && cfg.PoolManager.ReleaseRetryLimit == 6},
		{"scheduler", cfg.Scheduler.KeepOnFailure && cfg.Scheduler.KeepDuration == 30*time.Minute},
		{"executor", cfg.Executor.PrepareTimeout == 6*time.Minute},
		{"fleet", cfg.Fleet != nil && cfg.Fleet.LaunchTemplate != nil && cfg.Fleet.LaunchTemplate.Parameters.TLSKey == "/flintlock-runner/tls-key"},
		{"host services", cfg.HostServices.Buildkit.StorageLimit == 40<<30 && len(cfg.HostServices.HTTPCache.Upstreams) == 2},
		{"cache volume", cfg.HostServices.CacheVolume.Device == "/dev/nvme2n1" && cfg.HostServices.CacheVolume.SizeCap == 200<<30},
		{"distributed cache", cfg.DistributedCache != nil && cfg.DistributedCache.Prefix == "metal-runner"},
		{"observability", cfg.Observability.LogFormat == LogJSON && cfg.Observability.ListenAddress == ":9252"},
		{"state dir", cfg.StateDir == "/var/lib/flintlock-runner"},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s not loaded as written", c.name)
		}
	}
}

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# The Runner SHALL apply documented defaults to every optional
//# field so that a minimal configuration contains only the GitLab URL, the
//# runner token, the Pool Manager endpoint, one Host and one Profile with a
//# Pool size.

//= docs/requirements/07-configuration.md#scheduler-section
//= type=test
//# The Runner SHALL default the Runner namespace to the runner
//# name so that two Runners sharing one Pool Manager declare their Pools in
//# separate namespaces.

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# The Runner SHALL default a Pool's name to the Profile name and
//# its namespace to the Runner namespace.

// TestLoadMinimalAppliesDefaults loads the minimal file and checks that
// every optional field took its documented default, including the runner
// name from the hostname, the namespace from the runner name (CF-051) and
// the Pool name and namespace (CF-028).
func TestLoadMinimalAppliesDefaults(t *testing.T) {
	// Not parallel: it pins the package-level hostname function.
	hostname = func() (string, error) { return "control-node", nil }
	t.Cleanup(func() { hostname = os.Hostname })

	cfg := loadTestdata(t, "minimal.yaml")
	p := cfg.Profiles[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"gitlab.name", cfg.GitLab.Name, "control-node"},
		{"gitlab.concurrent", cfg.GitLab.Concurrent, 4},
		{"gitlab.check_interval", cfg.GitLab.CheckInterval, DefaultCheckInterval},
		{"gitlab.output_limit_kb", cfg.GitLab.OutputLimitKB, DefaultOutputLimitKB},
		{"gitlab.shutdown_timeout", cfg.GitLab.ShutdownTimeout, DefaultShutdownTimeout},
		{"scheduler.namespace", cfg.Scheduler.Namespace, "control-node"},
		{"scheduler.allocation_timeout", cfg.Scheduler.AllocationTimeout, DefaultAllocationTimeout},
		{"scheduler.host_health_interval", cfg.Scheduler.HostHealthInterval, DefaultHostHealthInterval},
		{"scheduler.host_unhealthy_threshold", cfg.Scheduler.HostUnhealthyThreshold, DefaultHostUnhealthyThreshold},
		{"scheduler.host_call_deadline", cfg.Scheduler.HostCallDeadline, DefaultHostCallDeadline},
		{"scheduler.keep_duration", cfg.Scheduler.KeepDuration, time.Duration(0)},
		{"profile.arch", p.Arch, ArchARM64},
		{"profile.vcpu", p.VCPU, DefaultProfileVCPU},
		{"profile.memory_mb", p.MemoryMB, DefaultProfileMemoryMB},
		{"profile.shell", p.Shell, DefaultShell},
		{"profile.builds_dir", p.BuildsDir, DefaultBuildsDir},
		{"profile.cache_dir", p.CacheDir, DefaultCacheDir},
		{"profile.helper_path", p.HelperPath, DefaultHelperPath},
		{"profile.transport.kind", p.Transport.Kind, TransportExec},
		{"profile.user", p.User, DefaultUser},
		{"profile.ready_timeout", p.ReadyTimeout, DefaultReadyTimeout},
		{"profile.default", p.Default, true},
		{"profile.pool.name", p.Pool.Name, "default"},
		{"profile.pool.namespace", p.Pool.Namespace, "control-node"},
		{"profile.pool.strategy", p.Pool.Strategy, ReplenishImmediateOnLease},
		{"profile.pool.heartbeat_interval", p.Pool.HeartbeatInterval, DefaultHeartbeatInterval},
		{"profile.pool.heartbeat_expiry", p.Pool.HeartbeatExpiry, DefaultHeartbeatExpiry},
		{"pool_manager.deadline", cfg.PoolManager.Deadline, DefaultPoolManagerDeadline},
		{"pool_manager.health_backoff", cfg.PoolManager.HealthBackoff, DefaultPoolManagerHealthBackoff},
		{"pool_manager.health_interval", cfg.PoolManager.HealthInterval, DefaultPoolManagerHealthInterval},
		{"pool_manager.health_failure_threshold", cfg.PoolManager.HealthFailureThreshold, DefaultPoolManagerHealthThreshold},
		{"pool_manager.events_poll_interval", cfg.PoolManager.EventsPollInterval, DefaultPoolManagerEventsPoll},
		{"pool_manager.declare_retry_interval", cfg.PoolManager.DeclareRetryInterval, DefaultPoolManagerDeclareRetry},
		{"pool_manager.release_retry_limit", cfg.PoolManager.ReleaseRetryLimit, DefaultPoolManagerReleaseRetry},
		{"executor.prepare_timeout", cfg.Executor.PrepareTimeout, DefaultPrepareTimeout},
		{"executor.graceful_kill_timeout", cfg.Executor.GracefulKillTimeout, DefaultGracefulKillTimeout},
		{"executor.transport_deadline", cfg.Executor.TransportDeadline, DefaultTransportDeadline},
		{"fleet", cfg.Fleet == nil, true},
		{"host_services.buildkit.enabled", cfg.HostServices.Buildkit.IsEnabled(), true},
		{"host_services.buildkit.port", cfg.HostServices.Buildkit.Port, DefaultBuildkitPort},
		{"host_services.go_proxy.port", cfg.HostServices.GoProxy.Port, DefaultGoProxyPort},
		{"host_services.go_proxy.upstream", cfg.HostServices.GoProxy.Upstream, DefaultGoProxyUpstream},
		{"host_services.registry_mirror.port", cfg.HostServices.RegistryMirror.Port, DefaultRegistryMirrorPort},
		{"host_services.registry_mirror.upstreams", len(cfg.HostServices.RegistryMirror.Upstreams), 1},
		{"host_services.http_cache.port", cfg.HostServices.HTTPCache.Port, DefaultHTTPCachePort},
		{"host_services.cache_volume.directory", cfg.HostServices.CacheVolume.Directory, DefaultCacheVolumeDirectory},
		{"host_services.cache_volume.size_cap", cfg.HostServices.CacheVolume.SizeCap, DefaultCacheVolumeSizeCap},
		{"distributed_cache", cfg.DistributedCache == nil, true},
		{"observability.log_format", cfg.Observability.LogFormat, DefaultLogFormat},
		{"observability.log_level", cfg.Observability.LogLevel, DefaultLogLevel},
		{"observability.listen_address", cfg.Observability.ListenAddress, DefaultListenAddress},
		{"state_dir", cfg.StateDir, DefaultStateDir},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestApplyDefaultsIsIdempotent applies the defaults twice and expects no
// change the second time, and never a change to a value the operator set.
func TestApplyDefaultsIsIdempotent(t *testing.T) {
	t.Parallel()
	full := loadTestdata(t, "full.yaml")
	before := full.Redacted()
	ApplyDefaults(full)
	if got := full.Redacted(); !equalYAML(t, got, before) {
		t.Error("ApplyDefaults changed an already-defaulted configuration")
	}
	if full.GitLab.Concurrent != 8 || full.Profiles[0].Pool.HeartbeatInterval != 15*time.Second {
		t.Error("ApplyDefaults overwrote operator values")
	}
}

// equalYAML compares two configurations by their Show output.
func equalYAML(t *testing.T, a, b *Config) bool {
	t.Helper()
	var ba, bb bytes.Buffer
	if err := Show(&ba, a); err != nil {
		t.Fatal(err)
	}
	if err := Show(&bb, b); err != nil {
		t.Fatal(err)
	}
	return ba.String() == bb.String()
}

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# The Runner SHALL allow every secret value in the configuration
//# to be supplied by an environment variable named in the field's
//# documentation, with the environment variable taking precedence over the
//# file.

// TestLoadEnvOverridesSecrets sets every documented variable and checks the
// environment wins over the file for each Secret: the GitLab token, one
// Host's own token, the fleet-wide Host token for the other Host, and the
// fleet flintlockd token.
func TestLoadEnvOverridesSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want map[string]string // secret name -> expected value
	}{
		{
			name: "file values when nothing is set",
			env:  map[string]string{},
			want: map[string]string{
				"gitlab": "glrt-file-token", "host-a": "host-a-file-token",
				"host-b": "host-b-file-token", "fleet": "fleet-file-token",
			},
		},
		{
			name: "every variable set",
			env: map[string]string{
				EnvGitLabToken:                          "glrt-env",
				HostTokenEnv("host-a"):                  "a-env",
				EnvHostToken:                            "fleet-wide-env",
				EnvFleetHostToken:                       "fleet-env",
				"FLINTLOCK_RUNNER_HOST_TOKEN_UNRELATED": "ignored",
			},
			want: map[string]string{
				"gitlab": "glrt-env", "host-a": "a-env", "host-b": "fleet-wide-env", "fleet": "fleet-env",
			},
		},
		{
			name: "per-host variable beats the fleet-wide one",
			env: map[string]string{
				EnvHostToken:           "fleet-wide-env",
				HostTokenEnv("host-b"): "b-env",
			},
			want: map[string]string{
				"gitlab": "glrt-file-token", "host-a": "fleet-wide-env", "host-b": "b-env", "fleet": "fleet-file-token",
			},
		},
		{
			name: "a variable set to the empty string blanks the file value",
			env:  map[string]string{HostTokenEnv("host-a"): ""},
			want: map[string]string{
				"gitlab": "glrt-file-token", "host-a": "", "host-b": "host-b-file-token", "fleet": "fleet-file-token",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := loadTestdata(t, "full.yaml", WithEnv(mapEnv(tt.env)))
			got := map[string]string{
				"gitlab": string(cfg.GitLab.Token),
				"host-a": string(cfg.Inventory.Hosts[0].Token),
				"host-b": string(cfg.Inventory.Hosts[1].Token),
				"fleet":  string(cfg.Fleet.Flintlockd.Token),
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("%s token = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestHostTokenEnv(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"host-a":        "FLINTLOCK_RUNNER_HOST_TOKEN_HOST_A",
		"ip-10-0-1-10":  "FLINTLOCK_RUNNER_HOST_TOKEN_IP_10_0_1_10",
		"Metal.eu-west": "FLINTLOCK_RUNNER_HOST_TOKEN_METAL_EU_WEST",
		"h1":            "FLINTLOCK_RUNNER_HOST_TOKEN_H1",
	}
	for in, want := range tests {
		if got := HostTokenEnv(in); got != want {
			t.Errorf("HostTokenEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# The Runner SHALL allow an Inventory to be supplied inline in the
//# configuration file or by reference to a separate Inventory file.

// TestLoadInventoryFile covers the file reference: a relative path resolved
// against the configuration file, both forms given at once, and a missing
// file. The inline form is what every other test uses.
func TestLoadInventoryFile(t *testing.T) {
	t.Parallel()

	t.Run("relative reference is resolved and read", func(t *testing.T) {
		t.Parallel()
		cfg := loadTestdata(t, "with-inventory-file.yaml")
		if len(cfg.Inventory.Hosts) != 2 || cfg.Inventory.Hosts[1].Name != "host-b" || cfg.Inventory.Hosts[1].Arch != ArchAMD64 {
			t.Errorf("hosts = %+v", cfg.Inventory.Hosts)
		}
		if cfg.Inventory.File != "inventory.yaml" {
			t.Errorf("inventory.file = %q, want kept as provenance", cfg.Inventory.File)
		}
	})

	t.Run("absolute reference", func(t *testing.T) {
		t.Parallel()
		abs, err := filepath.Abs(filepath.Join("testdata", "inventory.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join("testdata", "with-inventory-file.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.Replace(data, []byte("file: inventory.yaml"), []byte("file: "+abs), 1)
		cfg, err := Parse(data, t.TempDir(), WithEnv(noEnv))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Inventory.Hosts) != 2 {
			t.Errorf("hosts = %d, want 2", len(cfg.Inventory.Hosts))
		}
	})

	t.Run("file and inline together are rejected", func(t *testing.T) {
		t.Parallel()
		data, err := os.ReadFile(filepath.Join("testdata", "with-inventory-file.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, []byte("\ninventory:\n  file: inventory.yaml\n  hosts:\n    - name: x\n      endpoint: 1.2.3.4:9090\n      arch: arm64\n      vcpu: 1\n      memory_mb: 1\n")...)
		// The YAML above redefines the inventory key; drop the first one.
		data = bytes.Replace(data, []byte("inventory:\n  file: inventory.yaml\nprofiles:"), []byte("profiles:"), 1)
		_, err = Parse(data, "testdata", WithEnv(noEnv))
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.First().Field != "inventory" {
			t.Fatalf("error = %v, want a validation error on inventory", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		data := []byte("inventory:\n  file: nope.yaml\n")
		_, err := Parse(data, t.TempDir(), WithEnv(noEnv))
		if !IsNotExist(err) || !strings.Contains(err.Error(), "nope.yaml") {
			t.Fatalf("error = %v, want not-exist naming the file", err)
		}
	})

	t.Run("LoadInventoryFile", func(t *testing.T) {
		t.Parallel()
		inv, err := LoadInventoryFile(filepath.Join("testdata", "inventory.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.Hosts) != 2 || inv.GeneratedAt.IsZero() {
			t.Errorf("inventory file = %+v", inv)
		}
	})
}

// TestLoadRejectsMalformedFiles covers the failures before validation:
// a missing file, unknown keys, a type mismatch and two documents.
func TestLoadRejectsMalformedFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{"unknown top-level key", "gitlab: {url: https://x}\nbogus: 1\n", "field bogus not found"},
		{"unknown nested key", "gitlab:\n  url: https://x\n  tokn: y\n", "field tokn not found"},
		{"type mismatch", "gitlab:\n  concurrent: many\n", "cannot unmarshal"},
		{"bad duration", "gitlab:\n  check_interval: soon\n", "soon"},
		{"bad size", "host_services:\n  buildkit:\n    storage_limit: 10XB\n", "invalid byte size"},
		{"two documents", "gitlab: {url: https://x}\n---\ngitlab: {url: https://y}\n", "more than one YAML document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.data), t.TempDir(), WithEnv(noEnv))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"), WithEnv(noEnv))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("error = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("empty file fails validation, not parsing", func(t *testing.T) {
		t.Parallel()
		_, err := Parse(nil, t.TempDir(), WithEnv(noEnv))
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
	})
}

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# Where a Profile image is specified by tag rather than by digest,
//# the Runner SHALL log a warning at startup naming the Profile and the image.

// TestLoadWarnsForTaggedImages checks that Load logs one warning per image
// pinned by tag, naming the Profile and the image, and none for digests.
func TestLoadWarnsForTaggedImages(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	loadTestdata(t, "full.yaml", WithLogger(captureLogger(&buf)))
	logs := buf.String()
	for _, want := range []string{
		`profile=ubuntu-arm64 field=profiles[0].initrd.image image=ghcr.io/example/initrd:6.1`,
		`profile=ubuntu-arm64 field=profiles[0].rootfs image=ghcr.io/example/ubuntu-ci:24.04`,
		`profile=ubuntu-arm64 field=profiles[0].additional_volumes[0].image image=ghcr.io/example/tools:1.2.3`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	if strings.Count(logs, "level=WARN") != 3 {
		t.Errorf("want exactly 3 warnings, got:\n%s", logs)
	}
	if strings.Contains(logs, "builder-ssh") {
		t.Errorf("digest-pinned profile was warned about:\n%s", logs)
	}

	buf.Reset()
	loadTestdata(t, "minimal.yaml", WithLogger(captureLogger(&buf)))
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("minimal config (all digests) produced a warning:\n%s", buf.String())
	}
}

//= docs/requirements/07-configuration.md#distributed-cache-section
//= type=test
//# If the Distributed cache section is absent, then the Runner
//# SHALL log at startup that the `cache:` keyword is unavailable and SHALL
//# fail Jobs that use it with the failure reason `runner_unsupported`.

// TestLoadLogsWhenDistributedCacheAbsent checks the startup line and the
// CacheConfigured flag the Executor uses to fail `cache:` Jobs with
// runner_unsupported.
func TestLoadLogsWhenDistributedCacheAbsent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := loadTestdata(t, "minimal.yaml", WithLogger(captureLogger(&buf)))
	if cfg.CacheConfigured() {
		t.Error("CacheConfigured() = true without a distributed_cache section")
	}
	if !strings.Contains(buf.String(), "cache: keyword is unavailable") || !strings.Contains(buf.String(), "runner_unsupported") {
		t.Errorf("startup log lacks the cache notice:\n%s", buf.String())
	}

	buf.Reset()
	cfg = loadTestdata(t, "full.yaml", WithLogger(captureLogger(&buf)))
	if !cfg.CacheConfigured() {
		t.Error("CacheConfigured() = false with a distributed_cache section")
	}
	if strings.Contains(buf.String(), "cache: keyword is unavailable") {
		t.Errorf("cache notice logged although the section is present:\n%s", buf.String())
	}
}
