package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fullWith loads testdata/full.yaml, which is valid, and applies mutate to
// the effective configuration. Validation tests work on the defaulted
// configuration Load produces, which is what Validate sees at startup.
func fullWith(t *testing.T, mutate func(*Config)) *Config {
	t.Helper()
	cfg := loadTestdata(t, "full.yaml")
	mutate(cfg)
	return cfg
}

// fieldErrors runs Validate and returns the field errors it reports, or nil
// when the configuration is valid.
func fieldErrors(t *testing.T, cfg *Config) []FieldError {
	t.Helper()
	err := Validate(cfg)
	if err == nil {
		return nil
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate returned %T (%v), want *ValidationError", err, err)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Error("Validate error does not match ErrInvalid")
	}
	if len(verr.Errors) == 0 {
		t.Fatal("*ValidationError carries no field errors")
	}
	return verr.Errors
}

// hasFieldError reports whether errs names field with a reason containing
// want.
func hasFieldError(errs []FieldError, field, want string) bool {
	for _, e := range errs {
		if e.Field == field && strings.Contains(e.Reason, want) {
			return true
		}
	}
	return false
}

// rejectCase is one configuration the Runner has to reject: mutate breaks a
// valid configuration and Validate has to name field with a reason
// containing reason.
type rejectCase struct {
	name   string
	mutate func(*Config)
	field  string
	reason string
}

// runRejectCases checks that each case is rejected with the expected field
// error, and that full.yaml on its own is accepted.
func runRejectCases(t *testing.T, cases []rejectCase) {
	t.Helper()
	t.Run("the unmutated configuration is valid", func(t *testing.T) {
		t.Parallel()
		if errs := fieldErrors(t, fullWith(t, func(*Config) {})); errs != nil {
			t.Fatalf("testdata/full.yaml is not valid: %v", errs)
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := fieldErrors(t, fullWith(t, tc.mutate))
			if !hasFieldError(errs, tc.field, tc.reason) {
				t.Errorf("errors = %v\nwant one on %q whose reason contains %q", errs, tc.field, tc.reason)
			}
		})
	}
}

//= docs/requirements/07-configuration.md#gitlab-section
//= type=test
//# The GitLab section SHALL contain the GitLab URL, the runner
//# authentication token, the runner name, the concurrency limit, the check
//# interval, the output limit, the shutdown timeout and optional TLS
//# certificate authority, client certificate and client key paths.

// TestGitLabSection checks that every field the GitLab section has to carry
// is read from the file and is required (or, for the TLS paths, optional and
// read when present).
func TestGitLabSection(t *testing.T) {
	t.Parallel()
	cfg := loadTestdata(t, "full.yaml")
	g := cfg.GitLab
	got := map[string]any{
		"url":              g.URL,
		"token":            string(g.Token),
		"name":             g.Name,
		"concurrent":       g.Concurrent,
		"check_interval":   g.CheckInterval,
		"output_limit_kb":  g.OutputLimitKB,
		"shutdown_timeout": g.ShutdownTimeout,
		"tls.ca_file":      g.TLS.CAFile,
		"tls.cert_file":    g.TLS.CertFile,
		"tls.key_file":     g.TLS.KeyFile,
	}
	want := map[string]any{
		"url":              "https://gitlab.example.com",
		"token":            "glrt-file-token",
		"name":             "metal-runner",
		"concurrent":       8,
		"check_interval":   5 * time.Second,
		"output_limit_kb":  8192,
		"shutdown_timeout": 45 * time.Second,
		"tls.ca_file":      "/etc/flintlock-runner/gitlab-ca.pem",
		"tls.cert_file":    "/etc/flintlock-runner/gitlab-client.pem",
		"tls.key_file":     "/etc/flintlock-runner/gitlab-client.key",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("gitlab.%s = %v, want %v", k, got[k], w)
		}
	}

	runRejectCases(t, []rejectCase{
		{"no url", func(c *Config) { c.GitLab.URL = "" }, "gitlab.url", "is required"},
		{"url is not a URL", func(c *Config) { c.GitLab.URL = "gitlab.example.com" }, "gitlab.url", "absolute http or https URL"},
		{"http url without allow_insecure", func(c *Config) { c.GitLab.URL = "http://gitlab.example.com" }, "gitlab.url", "https"},
		{"no token", func(c *Config) { c.GitLab.Token = "" }, "gitlab.token", "is required"},
		{"no name", func(c *Config) { c.GitLab.Name = "" }, "gitlab.name", "is required"},
		{"concurrency limit of zero", func(c *Config) { c.GitLab.Concurrent = 0 }, "gitlab.concurrent", "must be positive"},
		{"no check interval", func(c *Config) { c.GitLab.CheckInterval = 0 }, "gitlab.check_interval", "positive duration"},
		{"no output limit", func(c *Config) { c.GitLab.OutputLimitKB = 0 }, "gitlab.output_limit_kb", "must be positive"},
		{"no shutdown timeout", func(c *Config) { c.GitLab.ShutdownTimeout = 0 }, "gitlab.shutdown_timeout", "positive duration"},
		{"client certificate without key", func(c *Config) { c.GitLab.TLS.KeyFile = "" }, "gitlab.tls", "set together"},
	})

	t.Run("an http url is accepted with allow_insecure", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.GitLab.URL = "http://gitlab.example.com"
			c.GitLab.AllowInsecure = true
		})
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# A Profile SHALL have a unique name, an architecture, a vCPU
//# count, a memory size in megabytes, a kernel image, a root filesystem image,
//# a shell path, a builds directory, a cache directory, a helper binary path
//# and pool settings.

// TestProfileRequiredFields removes each field a Profile has to have, one at
// a time, and expects that Profile's field to be named; the last case makes
// two Profiles share a name, which the uniqueness rule rejects.
func TestProfileRequiredFields(t *testing.T) {
	t.Parallel()
	runRejectCases(t, []rejectCase{
		{"no name", func(c *Config) { c.Profiles[0].Name = "" }, "profiles[0].name", "is required"},
		{"no arch", func(c *Config) { c.Profiles[0].Arch = "" }, "profiles[0].arch", "is required"},
		{"unknown arch", func(c *Config) { c.Profiles[0].Arch = "riscv64" }, "profiles[0].arch", "amd64 or arm64"},
		{"no vcpu", func(c *Config) { c.Profiles[0].VCPU = 0 }, "profiles[0].vcpu", "must be positive"},
		{"no memory", func(c *Config) { c.Profiles[0].MemoryMB = 0 }, "profiles[0].memory_mb", "must be positive"},
		{"no kernel image", func(c *Config) { c.Profiles[0].Kernel.Image = "" }, "profiles[0].kernel.image", "is required"},
		{"no rootfs", func(c *Config) { c.Profiles[0].RootFS = "" }, "profiles[0].rootfs", "is required"},
		{"no shell", func(c *Config) { c.Profiles[0].Shell = "" }, "profiles[0].shell", "is required"},
		{"relative shell", func(c *Config) { c.Profiles[0].Shell = "bin/bash" }, "profiles[0].shell", "absolute path"},
		{"no builds dir", func(c *Config) { c.Profiles[0].BuildsDir = "" }, "profiles[0].builds_dir", "is required"},
		{"no cache dir", func(c *Config) { c.Profiles[0].CacheDir = "" }, "profiles[0].cache_dir", "is required"},
		{"no helper path", func(c *Config) { c.Profiles[0].HelperPath = "" }, "profiles[0].helper_path", "is required"},
		{"no pool settings", func(c *Config) { c.Profiles[0].Pool = PoolSettings{} }, "profiles[0].pool.size", "must be positive"},
		{"no profiles at all", func(c *Config) { c.Profiles = nil }, "profiles", "at least one Profile"},
		{
			"two profiles with the same name",
			func(c *Config) { c.Profiles[1].Name = c.Profiles[0].Name },
			"profiles[1].name", "duplicates profiles[0].name",
		},
	})
}

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# Exactly one Profile MAY be marked as the Default Profile.

// TestExactlyOneDefaultProfile checks that one Default Profile is accepted
// and a second one is rejected. A configuration with no Default Profile is
// also valid; SC-012 then has no fallback to offer.
func TestExactlyOneDefaultProfile(t *testing.T) {
	t.Parallel()

	t.Run("one is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := loadTestdata(t, "full.yaml")
		if !cfg.Profiles[0].Default || cfg.Profiles[1].Default {
			t.Fatalf("defaults = %v, %v; want exactly the first", cfg.Profiles[0].Default, cfg.Profiles[1].Default)
		}
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})

	t.Run("none is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) { c.Profiles[0].Default = false })
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})

	t.Run("two are rejected", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) { c.Profiles[1].Default = true })
		errs := fieldErrors(t, cfg)
		if !hasFieldError(errs, "profiles", "2 Profiles are marked default") {
			t.Errorf("errors = %v, want a rejection of the second default Profile", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# If two Profiles declare the same image name, then the Runner
//# SHALL reject the configuration.

// TestDuplicateImageNamesRejected checks that an image name claimed by two
// Profiles is rejected, naming both Profiles, while a name declared once and
// a glob shared with another Profile's exact name are accepted.
func TestDuplicateImageNamesRejected(t *testing.T) {
	t.Parallel()

	t.Run("the same image name in two profiles", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.Profiles[1].Images = append(c.Profiles[1].Images, c.Profiles[0].Images[0])
		})
		errs := fieldErrors(t, cfg)
		if !hasFieldError(errs, "profiles[1].images[1]", `"ubuntu" is also declared by profiles[0]`) {
			t.Errorf("errors = %v, want the duplicate image name rejected", errs)
		}
	})

	t.Run("the same image name twice in one profile", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.Profiles[0].Images = append(c.Profiles[0].Images, c.Profiles[0].Images[0])
		})
		if errs := fieldErrors(t, cfg); !hasFieldError(errs, "profiles[0].images[2]", "is also declared by profiles[0]") {
			t.Errorf("errors = %v, want the duplicate image name rejected", errs)
		}
	})

	t.Run("distinct image names are accepted", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.Profiles[1].Images = append(c.Profiles[1].Images, "debian:12")
		})
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# A Profile's Host selector SHALL be expressed as label
//# requirements matched against Inventory labels and SHALL determine the
//# Pool's host list.

// TestHostSelectorDeterminesPoolHosts checks the label matching and the host
// list it produces: every requirement has to be present on the Host with the
// same value, the architectures have to agree, an empty selector takes every
// Host of the Profile's architecture, and a selector that matches nothing is
// rejected because the Pool would have no Hosts.
func TestHostSelectorDeterminesPoolHosts(t *testing.T) {
	t.Parallel()
	inventory := []HostEntry{
		{Name: "arm-general", Arch: ArchARM64, Labels: map[string]string{"tier": "general", "az": "a"}},
		{Name: "arm-builder", Arch: ArchARM64, Labels: map[string]string{"tier": "builders", "az": "b"}},
		{Name: "arm-general-2", Arch: ArchARM64, Labels: map[string]string{"tier": "general", "az": "b"}},
		{Name: "x86-general", Arch: ArchAMD64, Labels: map[string]string{"tier": "general", "az": "a"}},
	}
	tests := []struct {
		name     string
		arch     Architecture
		selector map[string]string
		want     []string
	}{
		{"no selector takes every host of the architecture", ArchARM64, nil, []string{"arm-general", "arm-builder", "arm-general-2"}},
		{"one requirement", ArchARM64, map[string]string{"tier": "general"}, []string{"arm-general", "arm-general-2"}},
		{"two requirements are combined", ArchARM64, map[string]string{"tier": "general", "az": "b"}, []string{"arm-general-2"}},
		{"the architecture also has to match", ArchAMD64, map[string]string{"tier": "general"}, []string{"x86-general"}},
		{"a value that differs excludes the host", ArchARM64, map[string]string{"tier": "gpu"}, nil},
		{"a label the host lacks excludes it", ArchARM64, map[string]string{"gpu": "true"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &Profile{Name: "p", Arch: tt.arch, HostSelector: tt.selector}
			got := SelectHosts(p, inventory)
			if len(got) != len(tt.want) {
				t.Fatalf("SelectHosts = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("SelectHosts = %v, want %v", got, tt.want)
				}
			}
		})
	}

	t.Run("a selector that matches no host is rejected", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.Profiles[0].HostSelector = map[string]string{"tier": "gpu"}
		})
		if errs := fieldErrors(t, cfg); !hasFieldError(errs, "profiles[0].host_selector", "matches no Inventory Host") {
			t.Errorf("errors = %v, want the empty host list rejected", errs)
		}
	})

	t.Run("the selector picks the pool's hosts out of the inventory", func(t *testing.T) {
		t.Parallel()
		cfg := loadTestdata(t, "full.yaml")
		got := SelectHosts(&cfg.Profiles[0], cfg.Inventory.Hosts)
		if len(got) != 1 || got[0] != "host-a" {
			t.Errorf("pool hosts = %v, want [host-a] from host_selector tier=general", got)
		}
		got = SelectHosts(&cfg.Profiles[1], cfg.Inventory.Hosts)
		if len(got) != 1 || got[0] != "host-b" {
			t.Errorf("pool hosts = %v, want [host-b] from host_selector tier=builders", got)
		}
	})
}

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# A Profile's pool settings SHALL contain the Pool size and MAY
//# contain the Pool name and namespace, replenishment strategy, minimum size,
//# create hooks, pre-lease hooks, hook failure policy, heartbeat interval and
//# heartbeat expiry threshold.

// TestPoolSettings checks that every optional Pool setting is read from the
// file, that the size is required, and that each optional setting is
// validated: a strategy outside the enum, a min_size that is missing for
// min_size_threshold or larger than the size, an unknown hook failure
// policy, an empty hook and a heartbeat expiry that is not longer than the
// interval are all rejected.
func TestPoolSettings(t *testing.T) {
	t.Parallel()
	cfg := loadTestdata(t, "full.yaml")
	p := cfg.Profiles[0].Pool
	if p.Size != 3 || p.Name != "ubuntu-arm64" || p.Namespace != "metal-runner" {
		t.Errorf("pool identity = %+v", p)
	}
	if p.Strategy != ReplenishMinSizeThreshold || p.MinSize == nil || *p.MinSize != 1 {
		t.Errorf("pool strategy = %+v", p)
	}
	if len(p.CreateHooks) != 1 || p.CreateHooks[0] != "/usr/local/bin/warm-caches" {
		t.Errorf("create_hooks = %v", p.CreateHooks)
	}
	if len(p.PreLeaseHooks) != 1 || p.PreLeaseHooks[0] != "/usr/local/bin/check-agent" {
		t.Errorf("pre_lease_hooks = %v", p.PreLeaseHooks)
	}
	if p.HookFailurePolicy != HookFailureQuarantine {
		t.Errorf("hook_failure_policy = %q", p.HookFailurePolicy)
	}
	if p.HeartbeatInterval != 15*time.Second || p.HeartbeatExpiry != 60*time.Second {
		t.Errorf("heartbeat = %s/%s", p.HeartbeatInterval, p.HeartbeatExpiry)
	}

	zero := 0
	four := 4
	runRejectCases(t, []rejectCase{
		{"size of zero", func(c *Config) { c.Profiles[0].Pool.Size = 0 }, "profiles[0].pool.size", "must be positive"},
		{"unknown strategy", func(c *Config) { c.Profiles[0].Pool.Strategy = "on_tuesdays" }, "profiles[0].pool.strategy", "must be one of"},
		{
			"min_size_threshold without a minimum",
			func(c *Config) { c.Profiles[0].Pool.MinSize = nil },
			"profiles[0].pool.min_size", "is required for the min_size_threshold strategy",
		},
		{
			"minimum larger than the size",
			func(c *Config) { c.Profiles[0].Pool.MinSize = &four },
			"profiles[0].pool.min_size", "must not exceed size 3",
		},
		{"empty create hook", func(c *Config) { c.Profiles[0].Pool.CreateHooks = []string{""} }, "profiles[0].pool.create_hooks[0]", "is required"},
		{"empty pre-lease hook", func(c *Config) { c.Profiles[0].Pool.PreLeaseHooks = []string{" "} }, "profiles[0].pool.pre_lease_hooks[0]", "is required"},
		{
			"unknown hook failure policy",
			func(c *Config) { c.Profiles[0].Pool.HookFailurePolicy = "ignore" },
			"profiles[0].pool.hook_failure_policy", "must be delete_and_replace or quarantine",
		},
		{
			"heartbeat interval of zero",
			func(c *Config) { c.Profiles[0].Pool.HeartbeatInterval = 0 },
			"profiles[0].pool.heartbeat_interval", "positive duration",
		},
		{
			"expiry no longer than the interval",
			func(c *Config) { c.Profiles[0].Pool.HeartbeatExpiry = c.Profiles[0].Pool.HeartbeatInterval },
			"profiles[0].pool.heartbeat_expiry", "must be longer than heartbeat_interval",
		},
		{"minimum below zero", func(c *Config) { n := -1; c.Profiles[0].Pool.MinSize = &n }, "profiles[0].pool.min_size", "must not be negative"},
	})

	t.Run("a zero minimum is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) { c.Profiles[0].Pool.MinSize = &zero })
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# The Runner SHALL reject a Profile whose images are specified by
//# neither a digest nor a tag.

// TestUnpinnedImagesRejected checks every image reference of a Profile: the
// kernel, the initrd, the root filesystem and each additional volume are
// rejected when they carry neither a digest nor a tag, and accepted with
// either. A registry port is not mistaken for a tag.
func TestUnpinnedImagesRejected(t *testing.T) {
	t.Parallel()
	runRejectCases(t, []rejectCase{
		{
			"kernel image without digest or tag",
			func(c *Config) { c.Profiles[0].Kernel.Image = "ghcr.io/example/kernel" },
			"profiles[0].kernel.image", "neither a digest nor a tag",
		},
		{
			"rootfs without digest or tag",
			func(c *Config) { c.Profiles[0].RootFS = "ghcr.io/example/ubuntu-ci" },
			"profiles[0].rootfs", "neither a digest nor a tag",
		},
		{
			"initrd without digest or tag",
			func(c *Config) { c.Profiles[0].Initrd = &Initrd{Image: "ghcr.io/example/initrd"} },
			"profiles[0].initrd.image", "neither a digest nor a tag",
		},
		{
			"additional volume without digest or tag",
			func(c *Config) { c.Profiles[0].AdditionalVolumes[0].Image = "ghcr.io/example/tools" },
			"profiles[0].additional_volumes[0].image", "neither a digest nor a tag",
		},
		{
			"a registry port is not a tag",
			func(c *Config) { c.Profiles[0].RootFS = "localhost:5000/ubuntu-ci" },
			"profiles[0].rootfs", "neither a digest nor a tag",
		},
		{
			"malformed digest",
			func(c *Config) { c.Profiles[0].RootFS = "ghcr.io/example/ubuntu-ci@sha256" },
			"profiles[0].rootfs", "malformed digest",
		},
	})

	for _, ref := range []string{
		"ghcr.io/example/ubuntu-ci:24.04",
		"ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567",
		"ghcr.io/example/ubuntu-ci:24.04@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567",
		"localhost:5000/ubuntu-ci:24.04",
	} {
		t.Run("pinned by "+ref, func(t *testing.T) {
			t.Parallel()
			cfg := fullWith(t, func(c *Config) { c.Profiles[0].RootFS = ref })
			if errs := fieldErrors(t, cfg); errs != nil {
				t.Errorf("errors = %v, want %s accepted", errs, ref)
			}
		})
	}
}

//= docs/requirements/07-configuration.md#profiles-section
//= type=test
//# If two Profiles resolve to the same Pool name and namespace,
//# then the Runner SHALL reject the configuration.

// TestPoolNameCollisionRejected checks the collision after defaulting: two
// Profiles that name the same Pool in the same namespace are rejected,
// whether they say so explicitly or arrive there through the default Pool
// name, while the same Pool name in different namespaces is accepted.
func TestPoolNameCollisionRejected(t *testing.T) {
	t.Parallel()

	t.Run("explicit collision", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) { c.Profiles[1].Pool.Name = c.Profiles[0].Pool.Name })
		if errs := fieldErrors(t, cfg); !hasFieldError(errs, "profiles[1].pool", "resolves to pool") {
			t.Errorf("errors = %v, want the pool collision rejected", errs)
		}
	})

	t.Run("collision through the defaulted pool name", func(t *testing.T) {
		t.Parallel()
		// Two Profiles cannot share a name, but a Profile may name its Pool
		// after another Profile, which the default gives the other Pool.
		cfg := fullWith(t, func(c *Config) { c.Profiles[1].Pool.Name = "ubuntu-arm64" })
		if errs := fieldErrors(t, cfg); !hasFieldError(errs, "profiles[1].pool", `resolves to pool "ubuntu-arm64"`) {
			t.Errorf("errors = %v, want the pool collision rejected", errs)
		}
	})

	t.Run("the same name in another namespace is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.Profiles[1].Pool.Name = c.Profiles[0].Pool.Name
			c.Profiles[1].Pool.Namespace = "other-runner"
		})
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#inventory-section
//= type=test
//# An Inventory entry SHALL contain a Host name, a `flintlockd`
//# gRPC endpoint, an architecture, a vCPU capacity and a memory capacity, and
//# MAY contain labels, a basic auth token, TLS settings, Host Service
//# addresses and installed version information.

// TestInventoryEntry checks that the optional parts of an entry are read
// from the file and that each required part is rejected when it is missing
// or malformed.
func TestInventoryEntry(t *testing.T) {
	t.Parallel()
	cfg := loadTestdata(t, "full.yaml")
	h := cfg.Inventory.Hosts[0]
	if h.Name != "host-a" || h.Endpoint != "10.0.1.10:9090" || h.Arch != ArchARM64 || h.VCPU != 62 || h.MemoryMB != 253952 {
		t.Errorf("required fields = %+v", h)
	}
	if h.Labels["tier"] != "general" || h.Labels["az"] != "eu-west-1a" {
		t.Errorf("labels = %v", h.Labels)
	}
	if string(h.Token) != "host-a-file-token" {
		t.Errorf("token = %q", h.Token)
	}
	if h.TLS.CAFile == "" || h.TLS.CertFile == "" || h.TLS.KeyFile == "" {
		t.Errorf("tls = %+v", h.TLS)
	}
	if h.Services.Buildkit != "tcp://172.31.0.1:1234" || h.Services.GoProxy == "" ||
		h.Services.RegistryMirror == "" || h.Services.HTTPCache["npm"] == "" {
		t.Errorf("services = %+v", h.Services)
	}
	if h.Versions.Flintlock != "v0.12.1" || h.Versions.Firecracker != "v1.10.1" ||
		h.Versions.CloudHypervisor != "v41.0" || h.Versions.Containerd != "v1.7.22" {
		t.Errorf("versions = %+v", h.Versions)
	}

	runRejectCases(t, []rejectCase{
		{"no name", func(c *Config) { c.Inventory.Hosts[0].Name = "" }, "inventory.hosts[0].name", "is required"},
		{"no endpoint", func(c *Config) { c.Inventory.Hosts[0].Endpoint = "" }, "inventory.hosts[0].endpoint", "is required"},
		{
			"endpoint with a scheme",
			func(c *Config) { c.Inventory.Hosts[0].Endpoint = "grpc://10.0.1.10:9090" },
			"inventory.hosts[0].endpoint", "without a scheme",
		},
		{
			"endpoint without a port",
			func(c *Config) { c.Inventory.Hosts[0].Endpoint = "10.0.1.10" },
			"inventory.hosts[0].endpoint", "must be host:port",
		},
		{"no arch", func(c *Config) { c.Inventory.Hosts[0].Arch = "" }, "inventory.hosts[0].arch", "is required"},
		{"no vcpu capacity", func(c *Config) { c.Inventory.Hosts[0].VCPU = 0 }, "inventory.hosts[0].vcpu", "must be positive"},
		{"no memory capacity", func(c *Config) { c.Inventory.Hosts[0].MemoryMB = 0 }, "inventory.hosts[0].memory_mb", "must be positive"},
		{"no hosts at all", func(c *Config) { c.Inventory.Hosts = nil }, "inventory", "at least one Host is required"},
		{
			"a host service address with no upstream declared",
			func(c *Config) { c.Inventory.Hosts[0].Services.HTTPCache["maven"] = "http://172.31.0.1:3128/maven" },
			"inventory.hosts[0].services.http_cache", "which host_services.http_cache.upstreams does not declare",
		},
	})
}

//= docs/requirements/07-configuration.md#inventory-section
//= type=test
//# The Runner SHALL reject an Inventory with duplicate Host names or
//# duplicate endpoints.

// TestDuplicateInventoryEntriesRejected checks both duplicates, inline and
// in a referenced Inventory file, and that distinct entries are accepted.
func TestDuplicateInventoryEntriesRejected(t *testing.T) {
	t.Parallel()

	t.Run("duplicate name", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) { c.Inventory.Hosts[1].Name = c.Inventory.Hosts[0].Name })
		if errs := fieldErrors(t, cfg); !hasFieldError(errs, "inventory.hosts[1].name", "duplicates inventory.hosts[0].name") {
			t.Errorf("errors = %v, want the duplicate name rejected", errs)
		}
	})

	t.Run("duplicate endpoint", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) { c.Inventory.Hosts[1].Endpoint = c.Inventory.Hosts[0].Endpoint })
		if errs := fieldErrors(t, cfg); !hasFieldError(errs, "inventory.hosts[1].endpoint", "duplicates inventory.hosts[0].endpoint") {
			t.Errorf("errors = %v, want the duplicate endpoint rejected", errs)
		}
	})

	t.Run("a third entry duplicating the first", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.Inventory.Hosts = append(c.Inventory.Hosts, c.Inventory.Hosts[0])
		})
		errs := fieldErrors(t, cfg)
		if !hasFieldError(errs, "inventory.hosts[2].name", "duplicates inventory.hosts[0].name") ||
			!hasFieldError(errs, "inventory.hosts[2].endpoint", "duplicates inventory.hosts[0].endpoint") {
			t.Errorf("errors = %v, want both duplicates rejected", errs)
		}
	})

	t.Run("duplicates in a referenced inventory file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "inventory.yaml"), `
hosts:
  - {name: host-a, endpoint: 10.0.1.10:9090, arch: arm64, vcpu: 8, memory_mb: 1024}
  - {name: host-a, endpoint: 10.0.1.11:9090, arch: arm64, vcpu: 8, memory_mb: 1024}
`)
		data := strings.Replace(minimalConfig(t), inlineInventory, "inventory:\n  file: inventory.yaml\n", 1)
		_, err := Parse([]byte(data), dir, WithEnv(noEnv))
		var verr *ValidationError
		if !errors.As(err, &verr) || !hasFieldError(verr.Errors, "inventory.hosts[1].name", "duplicates") {
			t.Fatalf("error = %v, want the duplicate host name rejected", err)
		}
	})
}

//= docs/requirements/07-configuration.md#inventory-section
//= type=test
//# The Host name in an Inventory entry SHALL be the name the Pool
//# Manager uses for that Host in `flintlock_hosts`.

// TestHostNameIsThePoolManagerName checks the identity the Runner relies on:
// the names SelectHosts declares in a Pool's flintlock_hosts are exactly the
// Inventory names, and HostByName joins a Placement's Host name back to the
// entry that carries the endpoint. A name the Inventory does not carry does
// not resolve, and a name that could not be a flintlock_hosts entry is
// rejected.
func TestHostNameIsThePoolManagerName(t *testing.T) {
	t.Parallel()
	cfg := loadTestdata(t, "full.yaml")

	declared := SelectHosts(&cfg.Profiles[0], cfg.Inventory.Hosts)
	if len(declared) == 0 {
		t.Fatal("the Profile declares no hosts")
	}
	for _, name := range declared {
		h, ok := HostByName(cfg.Inventory.Hosts, name)
		if !ok {
			t.Fatalf("flintlock_hosts entry %q does not resolve to an Inventory Host", name)
		}
		if h.Name != name {
			t.Errorf("HostByName(%q).Name = %q", name, h.Name)
		}
		if h.Endpoint == "" {
			t.Errorf("Host %q has no endpoint to dial", name)
		}
	}

	if _, ok := HostByName(cfg.Inventory.Hosts, "host-z"); ok {
		t.Error("a Host name the Inventory does not carry resolved")
	}
	if _, ok := HostByName(cfg.Inventory.Hosts, "HOST-A"); ok {
		t.Error("HostByName matched a name that differs in case; the Pool Manager's name is exact")
	}

	runRejectCases(t, []rejectCase{
		{
			"a name with a slash",
			func(c *Config) { c.Inventory.Hosts[0].Name = "eu-west-1a/host-a" },
			"inventory.hosts[0].name", "the Host name the Pool Manager uses in flintlock_hosts",
		},
		{
			"a name with whitespace",
			func(c *Config) { c.Inventory.Hosts[0].Name = "host a" },
			"inventory.hosts[0].name", "the Host name the Pool Manager uses in flintlock_hosts",
		},
	})
}

//= docs/requirements/07-configuration.md#pool-manager-section
//= type=test
//# The Pool Manager section SHALL contain an endpoint, TLS
//# settings, a request deadline, a health backoff period, a health probe
//# interval, an events poll interval, a pool declaration retry interval and a
//# release retry limit.

// TestPoolManagerSection checks that every setting is read from the file and
// that each one is validated.
func TestPoolManagerSection(t *testing.T) {
	t.Parallel()
	pm := loadTestdata(t, "full.yaml").PoolManager
	got := map[string]any{
		"endpoint":                 pm.Endpoint,
		"tls.ca_file":              pm.TLS.CAFile,
		"tls.cert_file":            pm.TLS.CertFile,
		"tls.key_file":             pm.TLS.KeyFile,
		"deadline":                 pm.Deadline,
		"health_backoff":           pm.HealthBackoff,
		"health_interval":          pm.HealthInterval,
		"health_failure_threshold": pm.HealthFailureThreshold,
		"events_poll_interval":     pm.EventsPollInterval,
		"declare_retry_interval":   pm.DeclareRetryInterval,
		"release_retry_limit":      pm.ReleaseRetryLimit,
	}
	want := map[string]any{
		"endpoint":                 "10.0.0.5:9091",
		"tls.ca_file":              "/etc/flintlock-runner/poolmgr-ca.pem",
		"tls.cert_file":            "/etc/flintlock-runner/poolmgr-client.pem",
		"tls.key_file":             "/etc/flintlock-runner/poolmgr-client.key",
		"deadline":                 15 * time.Second,
		"health_backoff":           20 * time.Second,
		"health_interval":          8 * time.Second,
		"health_failure_threshold": 4,
		"events_poll_interval":     3 * time.Second,
		"declare_retry_interval":   20 * time.Second,
		"release_retry_limit":      6,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("pool_manager.%s = %v, want %v", k, got[k], w)
		}
	}

	runRejectCases(t, []rejectCase{
		{"endpoint with a scheme", func(c *Config) { c.PoolManager.Endpoint = "https://10.0.0.5:9091" }, "pool_manager.endpoint", "without a scheme"},
		{"certificate without key", func(c *Config) { c.PoolManager.TLS.KeyFile = "" }, "pool_manager.tls", "set together"},
		{"insecure with TLS material", func(c *Config) { c.PoolManager.TLS.Insecure = true }, "pool_manager.tls", "insecure cannot be combined"},
		{"deadline of zero", func(c *Config) { c.PoolManager.Deadline = 0 }, "pool_manager.deadline", "positive duration"},
		{"health backoff of zero", func(c *Config) { c.PoolManager.HealthBackoff = 0 }, "pool_manager.health_backoff", "positive duration"},
		{"health interval of zero", func(c *Config) { c.PoolManager.HealthInterval = 0 }, "pool_manager.health_interval", "positive duration"},
		{
			"health failure threshold of zero",
			func(c *Config) { c.PoolManager.HealthFailureThreshold = 0 },
			"pool_manager.health_failure_threshold", "must be positive",
		},
		{"events poll interval of zero", func(c *Config) { c.PoolManager.EventsPollInterval = 0 }, "pool_manager.events_poll_interval", "positive duration"},
		{
			"declare retry interval of zero",
			func(c *Config) { c.PoolManager.DeclareRetryInterval = 0 },
			"pool_manager.declare_retry_interval", "positive duration",
		},
		{
			"negative release retry limit",
			func(c *Config) { c.PoolManager.ReleaseRetryLimit = -1 },
			"pool_manager.release_retry_limit", "must not be negative",
		},
	})
}

//= docs/requirements/07-configuration.md#pool-manager-section
//= type=test
//# If the Pool Manager section is absent or has no endpoint, then the
//# Runner SHALL reject the configuration.

// TestPoolManagerEndpointRequired checks that a configuration file without a
// pool_manager section at all, and one whose section has no endpoint, are
// both rejected naming pool_manager.endpoint.
func TestPoolManagerEndpointRequired(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"section absent": strings.ReplaceAll(minimalConfig(t), "pool_manager:\n  endpoint: 10.0.0.5:9091\n", ""),
		"endpoint empty": strings.ReplaceAll(minimalConfig(t), "endpoint: 10.0.0.5:9091", `endpoint: ""`),
		"endpoint blank": strings.ReplaceAll(minimalConfig(t), "endpoint: 10.0.0.5:9091", `endpoint: "   "`),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %v, want a validation error", err)
			}
			if !hasFieldError(verr.Errors, "pool_manager.endpoint", "is required") {
				t.Errorf("errors = %v, want pool_manager.endpoint required", verr.Errors)
			}
		})
	}
}

//= docs/requirements/07-configuration.md#scheduler-section
//= type=test
//# The Scheduler section SHALL contain the Runner namespace, the
//# allocation timeout, the Host health probe interval, the unhealthy probe
//# threshold, the keep-on-failure flag and the keep duration.

// TestSchedulerSection checks that every setting is read from the file and
// validated, including the keep duration, which is required only when
// keep_on_failure is set.
func TestSchedulerSection(t *testing.T) {
	t.Parallel()
	s := loadTestdata(t, "full.yaml").Scheduler
	if s.Namespace != "metal-runner" || s.AllocationTimeout != 4*time.Minute {
		t.Errorf("scheduler = %+v", s)
	}
	if s.HostHealthInterval != 12*time.Second || s.HostUnhealthyThreshold != 2 || s.HostCallDeadline != 8*time.Second {
		t.Errorf("scheduler health = %+v", s)
	}
	if !s.KeepOnFailure || s.KeepDuration != 30*time.Minute {
		t.Errorf("scheduler keep = %+v", s)
	}

	runRejectCases(t, []rejectCase{
		{"no namespace", func(c *Config) { c.Scheduler.Namespace = "" }, "scheduler.namespace", "is required"},
		{"namespace with a slash", func(c *Config) { c.Scheduler.Namespace = "a/b" }, "scheduler.namespace", "whitespace or slashes"},
		{"allocation timeout of zero", func(c *Config) { c.Scheduler.AllocationTimeout = 0 }, "scheduler.allocation_timeout", "positive duration"},
		{"host health interval of zero", func(c *Config) { c.Scheduler.HostHealthInterval = 0 }, "scheduler.host_health_interval", "positive duration"},
		{
			"unhealthy threshold of zero",
			func(c *Config) { c.Scheduler.HostUnhealthyThreshold = 0 },
			"scheduler.host_unhealthy_threshold", "must be positive",
		},
		{
			"keep_on_failure without a keep duration",
			func(c *Config) { c.Scheduler.KeepDuration = 0 },
			"scheduler.keep_duration", "positive duration",
		},
		{
			"negative keep duration",
			func(c *Config) { c.Scheduler.KeepOnFailure = false; c.Scheduler.KeepDuration = -time.Second },
			"scheduler.keep_duration", "must not be negative",
		},
	})
}

//= docs/requirements/07-configuration.md#fleet-section
//= type=test
//# The Fleet section SHALL contain the AWS region, the discovery
//# tag key and value or explicit instance ids, the remote execution mode and
//# its SSH settings, the provisioning parallelism, the pinned versions of
//# flintlock, Firecracker, Cloud Hypervisor, containerd and the Pool Manager,
//# the thin pool device, the guest subnet, the `flintlockd`
//# port and auth settings, the Host reserve, and the paths for the generated
//# Inventory and Runner configuration.

// TestFleetSection checks that every Fleet setting is read from the file,
// that the discovery alternatives are accepted, and that each setting is
// validated.
func TestFleetSection(t *testing.T) {
	t.Parallel()
	f := loadTestdata(t, "full.yaml").Fleet
	if f == nil {
		t.Fatal("fleet section not loaded")
	}
	got := map[string]any{
		"region":                    f.Region,
		"discovery.tag_key":         f.Discovery.TagKey,
		"discovery.tag_value":       f.Discovery.TagValue,
		"remote.mode":               f.Remote.Mode,
		"parallelism":               f.Parallelism,
		"versions.flintlock":        f.Versions.Flintlock,
		"versions.firecracker":      f.Versions.Firecracker,
		"versions.cloud_hypervisor": f.Versions.CloudHypervisor,
		"versions.containerd":       f.Versions.Containerd,
		"versions.pool_manager":     f.Versions.PoolManager,
		"thin_pool_device":          f.ThinPoolDevice,
		"guest_subnet":              f.GuestSubnet,
		"flintlockd.port":           f.Flintlockd.Port,
		"flintlockd.token":          string(f.Flintlockd.Token),
		"flintlockd.tls.ca_file":    f.Flintlockd.TLS.CAFile,
		"host_reserve.vcpu":         f.HostReserve.VCPU,
		"host_reserve.memory_mb":    f.HostReserve.MemoryMB,
		"inventory_path":            f.InventoryPath,
		"runner_config_path":        f.RunnerConfigPath,
	}
	want := map[string]any{
		"region":                    "eu-west-1",
		"discovery.tag_key":         "flintlock-runner:fleet",
		"discovery.tag_value":       "metal",
		"remote.mode":               RemoteSSM,
		"parallelism":               6,
		"versions.flintlock":        "v0.12.1",
		"versions.firecracker":      "v1.10.1",
		"versions.cloud_hypervisor": "v41.0",
		"versions.containerd":       "v1.7.22",
		"versions.pool_manager":     "v0.1.0",
		"thin_pool_device":          "/dev/nvme1n1",
		"guest_subnet":              "172.31.0.0/16",
		"flintlockd.port":           9090,
		"flintlockd.token":          "fleet-file-token",
		"flintlockd.tls.ca_file":    "/etc/flintlock-runner/fleet-ca.pem",
		"host_reserve.vcpu":         2,
		"host_reserve.memory_mb":    8192,
		"inventory_path":            "/etc/flintlock-runner/inventory.yaml",
		"runner_config_path":        "/etc/flintlock-runner/config.yaml",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("fleet.%s = %v, want %v", k, got[k], w)
		}
	}

	runRejectCases(t, []rejectCase{
		{"no region", func(c *Config) { c.Fleet.Region = "" }, "fleet.region", "is required"},
		{
			"a tag key without a value",
			func(c *Config) { c.Fleet.Discovery.TagValue = "" },
			"fleet.discovery", "tag_key and tag_value have to be set together",
		},
		{
			"no discovery at all",
			func(c *Config) { c.Fleet.Discovery = Discovery{} },
			"fleet.discovery", "one of tag_key/tag_value, instance_ids or static is required",
		},
		{"unknown remote mode", func(c *Config) { c.Fleet.Remote.Mode = "telnet" }, "fleet.remote.mode", "must be ssm or ssh"},
		{
			"ssh mode without a user",
			func(c *Config) { c.Fleet.Remote.Mode = RemoteSSH; c.Fleet.Remote.SSH.KeyFile = "/k" },
			"fleet.remote.ssh.user", "is required",
		},
		{
			"ssh mode without a key file",
			func(c *Config) { c.Fleet.Remote.Mode = RemoteSSH; c.Fleet.Remote.SSH.User = "ec2-user" },
			"fleet.remote.ssh.key_file", "is required",
		},
		{"parallelism of zero", func(c *Config) { c.Fleet.Parallelism = 0 }, "fleet.parallelism", "must be positive"},
		{"no flintlock version", func(c *Config) { c.Fleet.Versions.Flintlock = "" }, "fleet.versions.flintlock", "is required"},
		{
			"neither hypervisor pinned",
			func(c *Config) { c.Fleet.Versions.Firecracker = ""; c.Fleet.Versions.CloudHypervisor = "" },
			"fleet.versions", "at least one of firecracker and cloud_hypervisor",
		},
		{"no containerd version", func(c *Config) { c.Fleet.Versions.Containerd = "" }, "fleet.versions.containerd", "is required"},
		{"no pool manager version", func(c *Config) { c.Fleet.Versions.PoolManager = "" }, "fleet.versions.pool_manager", "is required"},
		{"no thin pool device", func(c *Config) { c.Fleet.ThinPoolDevice = "" }, "fleet.thin_pool_device", "is required"},
		{"guest subnet that is not a CIDR", func(c *Config) { c.Fleet.GuestSubnet = "172.31.0.1" }, "fleet.guest_subnet", "must be a CIDR"},
		{"flintlockd port out of range", func(c *Config) { c.Fleet.Flintlockd.Port = 70000 }, "fleet.flintlockd.port", "port between 1 and 65535"},
		{
			"partial flintlockd TLS material",
			func(c *Config) { c.Fleet.Flintlockd.TLS.KeyFile = "" },
			"fleet.flintlockd.tls", "have to be set together or all left empty",
		},
		{
			"negative host reserve",
			func(c *Config) { c.Fleet.HostReserve.VCPU = -1 },
			"fleet.host_reserve.vcpu", "must not be negative",
		},
		{"relative inventory path", func(c *Config) { c.Fleet.InventoryPath = "inventory.yaml" }, "fleet.inventory_path", "absolute path"},
		{"no runner config path", func(c *Config) { c.Fleet.RunnerConfigPath = "" }, "fleet.runner_config_path", "is required"},
	})

	t.Run("explicit instance ids replace the tag", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.Fleet.Discovery = Discovery{InstanceIDs: []string{"i-0123456789abcdef0"}}
			c.Fleet.LaunchTemplate = nil // its refresh interval needs tag discovery
		})
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#fleet-section
//= type=test
//# The Fleet section MAY contain launch template settings naming
//# the Systems Manager parameters that hold secrets.

// TestFleetLaunchTemplate checks that the launch template settings are read
// from the file, that they are optional, and that a template has to name the
// Systems Manager parameter for every secret it needs.
func TestFleetLaunchTemplate(t *testing.T) {
	t.Parallel()
	lt := loadTestdata(t, "full.yaml").Fleet.LaunchTemplate
	if lt == nil {
		t.Fatal("launch_template not loaded")
	}
	if lt.Parameters.HostToken != "/flintlock-runner/host-token" ||
		lt.Parameters.TLSCA != "/flintlock-runner/tls-ca" ||
		lt.Parameters.TLSCert != "/flintlock-runner/tls-cert" ||
		lt.Parameters.TLSKey != "/flintlock-runner/tls-key" {
		t.Errorf("parameters = %+v", lt.Parameters)
	}
	if lt.InventoryRefreshInterval != 5*time.Minute {
		t.Errorf("inventory_refresh_interval = %s", lt.InventoryRefreshInterval)
	}

	runRejectCases(t, []rejectCase{
		{
			"no host token parameter",
			func(c *Config) { c.Fleet.LaunchTemplate.Parameters.HostToken = "" },
			"fleet.launch_template.parameters.host_token", "is required",
		},
		{
			"no TLS CA parameter",
			func(c *Config) { c.Fleet.LaunchTemplate.Parameters.TLSCA = "" },
			"fleet.launch_template.parameters.tls_ca", "is required",
		},
		{
			"no TLS certificate parameter",
			func(c *Config) { c.Fleet.LaunchTemplate.Parameters.TLSCert = "" },
			"fleet.launch_template.parameters.tls_cert", "is required",
		},
		{
			"no TLS key parameter",
			func(c *Config) { c.Fleet.LaunchTemplate.Parameters.TLSKey = "" },
			"fleet.launch_template.parameters.tls_key", "is required",
		},
		{
			"a refresh interval without tag discovery",
			func(c *Config) { c.Fleet.Discovery.TagKey = ""; c.Fleet.Discovery.TagValue = "" },
			"fleet.launch_template.inventory_refresh_interval", "needs tag discovery",
		},
	})

	t.Run("the section is optional", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) { c.Fleet.LaunchTemplate = nil })
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})

	t.Run("TLS parameters are unnecessary for an insecure flintlockd", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.Fleet.Flintlockd.Insecure = true
			c.Fleet.Flintlockd.TLS = ServerTLSFiles{}
			c.Fleet.LaunchTemplate.Parameters = LaunchTemplateParameters{HostToken: "/flintlock-runner/host-token"}
		})
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#host-services-section
//= type=test
//# The Host services section SHALL contain, for each of
//# `buildkit`, `go_proxy`, `registry_mirror` and `http_cache`, an enabled flag
//# that defaults to true and a port.

// TestHostServiceEnabledAndPort checks the enabled flag and the port of each
// of the four services: unset means enabled, false is honoured, the port
// takes a documented default and is validated, and two enabled services
// cannot share a port.
func TestHostServiceEnabledAndPort(t *testing.T) {
	t.Parallel()

	t.Run("unset means enabled, with the default port", func(t *testing.T) {
		t.Parallel()
		hs := loadTestdata(t, "minimal.yaml").HostServices
		services := []struct {
			name    string
			svc     Service
			defPort int
		}{
			{"buildkit", hs.Buildkit.Service, DefaultBuildkitPort},
			{"go_proxy", hs.GoProxy.Service, DefaultGoProxyPort},
			{"registry_mirror", hs.RegistryMirror.Service, DefaultRegistryMirrorPort},
			{"http_cache", hs.HTTPCache.Service, DefaultHTTPCachePort},
		}
		for _, s := range services {
			if !s.svc.IsEnabled() {
				t.Errorf("host_services.%s is not enabled by default", s.name)
			}
			if s.svc.Port != s.defPort {
				t.Errorf("host_services.%s.port = %d, want %d", s.name, s.svc.Port, s.defPort)
			}
		}
	})

	t.Run("an explicit enabled flag and port are honoured", func(t *testing.T) {
		t.Parallel()
		data := minimalConfig(t) + `
host_services:
  buildkit:
    enabled: false
  go_proxy:
    enabled: true
    port: 3100
  registry_mirror:
    enabled: false
  http_cache:
    port: 3200
`
		cfg, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
		if err != nil {
			t.Fatal(err)
		}
		hs := cfg.HostServices
		if hs.Buildkit.IsEnabled() || hs.RegistryMirror.IsEnabled() {
			t.Error("enabled: false was not honoured")
		}
		if !hs.GoProxy.IsEnabled() || hs.GoProxy.Port != 3100 {
			t.Errorf("go_proxy = %+v", hs.GoProxy.Service)
		}
		if !hs.HTTPCache.IsEnabled() || hs.HTTPCache.Port != 3200 {
			t.Errorf("http_cache = %+v", hs.HTTPCache.Service)
		}
		// A disabled service keeps its default port so nothing else claims it.
		if hs.Buildkit.Port != DefaultBuildkitPort {
			t.Errorf("buildkit.port = %d, want the default even when disabled", hs.Buildkit.Port)
		}
	})

	runRejectCases(t, []rejectCase{
		{"port out of range", func(c *Config) { c.HostServices.GoProxy.Port = 70000 }, "host_services.go_proxy.port", "port between 1 and 65535"},
		{"port of zero on a literal", func(c *Config) { c.HostServices.Buildkit.Port = 0 }, "host_services.buildkit.port", "port between 1 and 65535"},
		{
			"two enabled services on one port",
			func(c *Config) { c.HostServices.GoProxy.Port = c.HostServices.Buildkit.Port },
			"host_services.go_proxy.port", "is also used by host_services.buildkit",
		},
	})

	t.Run("a disabled service may share a port", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			disabled := false
			c.HostServices.GoProxy.Enabled = &disabled
			c.HostServices.GoProxy.Port = c.HostServices.Buildkit.Port
		})
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#host-services-section
//= type=test
//# The `buildkit` entry MAY contain a storage limit and a garbage
//# collection policy.

// TestBuildkitEntry checks that the storage limit and the garbage collection
// policy are read from the file, that both are optional, and that they are
// validated.
func TestBuildkitEntry(t *testing.T) {
	t.Parallel()
	b := loadTestdata(t, "full.yaml").HostServices.Buildkit
	if b.StorageLimit != 40<<30 {
		t.Errorf("storage_limit = %s, want 40GiB", b.StorageLimit)
	}
	if b.GCPolicy != "keepBytes=30GB" {
		t.Errorf("gc_policy = %q", b.GCPolicy)
	}

	t.Run("both are optional", func(t *testing.T) {
		t.Parallel()
		hs := loadTestdata(t, "minimal.yaml").HostServices
		if hs.Buildkit.StorageLimit != 0 || hs.Buildkit.GCPolicy != "" {
			t.Errorf("buildkit = %+v, want no storage limit and no policy", hs.Buildkit)
		}
	})

	runRejectCases(t, []rejectCase{
		{
			"negative storage limit",
			func(c *Config) { c.HostServices.Buildkit.StorageLimit = -1 },
			"host_services.buildkit.storage_limit", "must not be negative",
		},
		{
			"a policy for a disabled buildkit",
			func(c *Config) { disabled := false; c.HostServices.Buildkit.Enabled = &disabled },
			"host_services.buildkit.gc_policy", "buildkit is disabled",
		},
	})
}

//= docs/requirements/07-configuration.md#host-services-section
//= type=test
//# The `go_proxy` entry MAY contain the upstream proxy, a list of
//# private module patterns, the version control host those patterns resolve
//# to, the Systems Manager parameter holding the read-only credential for
//# them, a private module revalidation interval, a storage limit and a list of
//# modules to pre-warm.

// TestGoProxyEntry checks that every optional go_proxy setting is read from
// the file and validated, and that the upstream and the revalidation
// interval take their documented defaults.
func TestGoProxyEntry(t *testing.T) {
	t.Parallel()
	g := loadTestdata(t, "full.yaml").HostServices.GoProxy
	if g.Upstream != "https://proxy.golang.org" {
		t.Errorf("upstream = %q", g.Upstream)
	}
	if len(g.PrivatePatterns) != 1 || g.PrivatePatterns[0] != "gitlab.example.com/*" {
		t.Errorf("private_patterns = %v", g.PrivatePatterns)
	}
	if g.PrivateVCSHost != "gitlab.example.com" {
		t.Errorf("private_vcs_host = %q", g.PrivateVCSHost)
	}
	if g.CredentialParameter != "/flintlock-runner/goproxy-token" {
		t.Errorf("credential_parameter = %q", g.CredentialParameter)
	}
	if g.PrivateRevalidateInterval != 12*time.Hour {
		t.Errorf("private_revalidate_interval = %s", g.PrivateRevalidateInterval)
	}
	if g.StorageLimit != 20<<30 {
		t.Errorf("storage_limit = %s", g.StorageLimit)
	}
	if len(g.Prewarm) != 1 || g.Prewarm[0] != "golang.org/x/tools@v0.30.0" {
		t.Errorf("prewarm = %v", g.Prewarm)
	}

	t.Run("the defaults", func(t *testing.T) {
		t.Parallel()
		m := loadTestdata(t, "minimal.yaml").HostServices.GoProxy
		if m.Upstream != DefaultGoProxyUpstream {
			t.Errorf("upstream = %q, want %q", m.Upstream, DefaultGoProxyUpstream)
		}
		if len(m.PrivatePatterns) != 0 || m.PrivateRevalidateInterval != 0 {
			t.Errorf("private settings = %+v, want none without patterns", m)
		}
	})

	t.Run("a revalidation interval is defaulted once patterns are declared", func(t *testing.T) {
		t.Parallel()
		data := minimalConfig(t) + `
host_services:
  go_proxy:
    private_patterns: ["gitlab.example.com/*"]
    credential_parameter: /flintlock-runner/goproxy-token
`
		cfg, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.HostServices.GoProxy.PrivateRevalidateInterval != DefaultPrivateRevalidateInterval {
			t.Errorf("private_revalidate_interval = %s, want %s",
				cfg.HostServices.GoProxy.PrivateRevalidateInterval, DefaultPrivateRevalidateInterval)
		}
	})

	runRejectCases(t, []rejectCase{
		{"upstream that is not a URL", func(c *Config) { c.HostServices.GoProxy.Upstream = "proxy.golang.org" }, "host_services.go_proxy.upstream", "absolute http or https URL"},
		{"empty private pattern", func(c *Config) { c.HostServices.GoProxy.PrivatePatterns = []string{""} }, "host_services.go_proxy.private_patterns[0]", "is required"},
		{
			"a version control host without patterns",
			func(c *Config) { c.HostServices.GoProxy.PrivatePatterns = nil },
			"host_services.go_proxy.private_vcs_host", "is set without private_patterns",
		},
		{
			"negative revalidation interval",
			func(c *Config) { c.HostServices.GoProxy.PrivateRevalidateInterval = -time.Hour },
			"host_services.go_proxy.private_revalidate_interval", "must not be negative",
		},
		{"negative storage limit", func(c *Config) { c.HostServices.GoProxy.StorageLimit = -1 }, "host_services.go_proxy.storage_limit", "must not be negative"},
		{
			"a pre-warm entry without a version",
			func(c *Config) { c.HostServices.GoProxy.Prewarm = []string{"golang.org/x/tools"} },
			"host_services.go_proxy.prewarm[0]", "must be module@version",
		},
	})
}

//= docs/requirements/07-configuration.md#host-services-section
//= type=test
//# If the `go_proxy` entry lists private module patterns without a
//# credential parameter, then the Runner SHALL reject the configuration.

// TestGoProxyPrivatePatternsNeedCredential checks that private patterns
// without a credential parameter are rejected, whether the parameter is
// missing or blank, and that the same patterns are accepted with one.
func TestGoProxyPrivatePatternsNeedCredential(t *testing.T) {
	t.Parallel()
	for name, param := range map[string]string{"missing": "", "blank": "   "} {
		t.Run(name+" credential parameter", func(t *testing.T) {
			t.Parallel()
			cfg := fullWith(t, func(c *Config) { c.HostServices.GoProxy.CredentialParameter = param })
			errs := fieldErrors(t, cfg)
			if !hasFieldError(errs, "host_services.go_proxy.credential_parameter", "is required when private_patterns are set") {
				t.Errorf("errors = %v, want the private patterns rejected", errs)
			}
		})
	}

	t.Run("no patterns needs no credential parameter", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.HostServices.GoProxy.PrivatePatterns = nil
			c.HostServices.GoProxy.PrivateVCSHost = ""
			c.HostServices.GoProxy.CredentialParameter = ""
		})
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})
}

//= docs/requirements/07-configuration.md#host-services-section
//= type=test
//# The `registry_mirror` entry MAY contain a list of upstream
//# registries with optional credential parameter names, a storage limit and a
//# list of images to pre-warm.

// TestRegistryMirrorEntry checks that the upstreams, their optional
// credential parameters, the storage limit and the pre-warm list are read
// from the file and validated.
func TestRegistryMirrorEntry(t *testing.T) {
	t.Parallel()
	r := loadTestdata(t, "full.yaml").HostServices.RegistryMirror
	if len(r.Upstreams) != 2 {
		t.Fatalf("upstreams = %+v, want 2", r.Upstreams)
	}
	if r.Upstreams[0].URL != "https://registry-1.docker.io" || r.Upstreams[0].CredentialParameter != "" {
		t.Errorf("upstreams[0] = %+v, want an anonymous upstream", r.Upstreams[0])
	}
	if r.Upstreams[1].URL != "https://ghcr.io" || r.Upstreams[1].CredentialParameter != "/flintlock-runner/ghcr-token" {
		t.Errorf("upstreams[1] = %+v", r.Upstreams[1])
	}
	if r.StorageLimit != 60<<30 {
		t.Errorf("storage_limit = %s", r.StorageLimit)
	}
	if len(r.Prewarm) != 1 || r.Prewarm[0] != "docker.io/library/alpine:3.20" {
		t.Errorf("prewarm = %v", r.Prewarm)
	}

	runRejectCases(t, []rejectCase{
		{
			"an upstream that is not a URL",
			func(c *Config) { c.HostServices.RegistryMirror.Upstreams[0].URL = "registry-1.docker.io" },
			"host_services.registry_mirror.upstreams[0].url", "absolute http or https URL",
		},
		{
			"the same upstream twice",
			func(c *Config) {
				c.HostServices.RegistryMirror.Upstreams[1].URL = c.HostServices.RegistryMirror.Upstreams[0].URL
			},
			"host_services.registry_mirror.upstreams[1].url", "duplicates upstreams[0]",
		},
		{
			"negative storage limit",
			func(c *Config) { c.HostServices.RegistryMirror.StorageLimit = -1 },
			"host_services.registry_mirror.storage_limit", "must not be negative",
		},
		{
			"a pre-warm image that is not a reference",
			func(c *Config) { c.HostServices.RegistryMirror.Prewarm = []string{"alpine@sha256"} },
			"host_services.registry_mirror.prewarm[0]", "malformed digest",
		},
	})
}

//= docs/requirements/07-configuration.md#host-services-section
//= type=test
//# The `http_cache` entry MAY contain a list of upstreams, each
//# with a name, an upstream URL, a size limit, a time-to-live and the
//# environment variable name the Executor injects for it.

// TestHTTPCacheEntry checks that each upstream carries its name, URL, size
// limit, time-to-live and environment variable, that the time-to-live takes
// a default, and that names and environment variables are unique and
// well-formed.
func TestHTTPCacheEntry(t *testing.T) {
	t.Parallel()
	h := loadTestdata(t, "full.yaml").HostServices.HTTPCache
	if len(h.Upstreams) != 2 {
		t.Fatalf("upstreams = %+v, want 2", h.Upstreams)
	}
	npm := h.Upstreams[0]
	if npm.Name != "npm" || npm.URL != "https://registry.npmjs.org" ||
		npm.SizeLimit != 10<<30 || npm.TTL != 48*time.Hour || npm.EnvVar != "NPM_CONFIG_REGISTRY" {
		t.Errorf("upstreams[0] = %+v", npm)
	}
	if h.Upstreams[1].EnvVar != "PIP_INDEX_URL" {
		t.Errorf("upstreams[1] = %+v", h.Upstreams[1])
	}

	t.Run("the time-to-live takes a default", func(t *testing.T) {
		t.Parallel()
		data := minimalConfig(t) + `
host_services:
  http_cache:
    upstreams:
      - name: npm
        url: https://registry.npmjs.org
        env_var: NPM_CONFIG_REGISTRY
`
		cfg, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.HostServices.HTTPCache.Upstreams[0].TTL; got != DefaultHTTPCacheTTL {
			t.Errorf("ttl = %s, want %s", got, DefaultHTTPCacheTTL)
		}
	})

	runRejectCases(t, []rejectCase{
		{"an upstream without a name", func(c *Config) { c.HostServices.HTTPCache.Upstreams[0].Name = "" }, "host_services.http_cache.upstreams[0].name", "is required"},
		{
			"two upstreams with one name",
			func(c *Config) { c.HostServices.HTTPCache.Upstreams[1].Name = "npm" },
			"host_services.http_cache.upstreams[1].name", `duplicates upstreams[0].name "npm"`,
		},
		{"an upstream without a URL", func(c *Config) { c.HostServices.HTTPCache.Upstreams[0].URL = "" }, "host_services.http_cache.upstreams[0].url", "is required"},
		{
			"a negative size limit",
			func(c *Config) { c.HostServices.HTTPCache.Upstreams[0].SizeLimit = -1 },
			"host_services.http_cache.upstreams[0].size_limit", "must not be negative",
		},
		{
			"a negative time-to-live",
			func(c *Config) { c.HostServices.HTTPCache.Upstreams[0].TTL = -time.Hour },
			"host_services.http_cache.upstreams[0].ttl", "must not be negative",
		},
		{"an upstream without an environment variable", func(c *Config) { c.HostServices.HTTPCache.Upstreams[0].EnvVar = "" }, "host_services.http_cache.upstreams[0].env_var", "is required"},
		{
			"an environment variable that is not a name",
			func(c *Config) { c.HostServices.HTTPCache.Upstreams[0].EnvVar = "NPM-REGISTRY" },
			"host_services.http_cache.upstreams[0].env_var", "must be an environment variable name",
		},
		{
			"two upstreams injecting one variable",
			func(c *Config) { c.HostServices.HTTPCache.Upstreams[1].EnvVar = "NPM_CONFIG_REGISTRY" },
			"host_services.http_cache.upstreams[1].env_var", "duplicates upstreams[0].env_var",
		},
	})
}

//= docs/requirements/07-configuration.md#host-services-section
//= type=test
//# The Host services section SHALL contain the cache volume device
//# or directory and its total size cap.

// TestCacheVolume checks the storage the Host Services share: a device or a
// directory but not both, a directory and a size cap by default, and a size
// cap that has to hold the storage limits of the enabled services.
func TestCacheVolume(t *testing.T) {
	t.Parallel()
	cv := loadTestdata(t, "full.yaml").HostServices.CacheVolume
	if cv.Device != "/dev/nvme2n1" || cv.Directory != "" || cv.SizeCap != 200<<30 {
		t.Errorf("cache_volume = %+v", cv)
	}

	t.Run("the defaults", func(t *testing.T) {
		t.Parallel()
		cv := loadTestdata(t, "minimal.yaml").HostServices.CacheVolume
		if cv.Directory != DefaultCacheVolumeDirectory || cv.Device != "" {
			t.Errorf("cache_volume = %+v, want the default directory", cv)
		}
		if cv.SizeCap != DefaultCacheVolumeSizeCap {
			t.Errorf("size_cap = %s, want %s", cv.SizeCap, DefaultCacheVolumeSizeCap)
		}
	})

	t.Run("a directory is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := fullWith(t, func(c *Config) {
			c.HostServices.CacheVolume = CacheVolume{Directory: "/var/lib/host-services", SizeCap: 300 << 30}
		})
		if errs := fieldErrors(t, cfg); errs != nil {
			t.Errorf("errors = %v, want none", errs)
		}
	})

	runRejectCases(t, []rejectCase{
		{
			"a device and a directory",
			func(c *Config) { c.HostServices.CacheVolume.Directory = "/var/lib/host-services" },
			"host_services.cache_volume", "device and directory are mutually exclusive",
		},
		{
			"neither a device nor a directory",
			func(c *Config) { c.HostServices.CacheVolume.Device = "" },
			"host_services.cache_volume", "one of device or directory is required",
		},
		{
			"a relative directory",
			func(c *Config) { c.HostServices.CacheVolume = CacheVolume{Directory: "cache", SizeCap: 1 << 30} },
			"host_services.cache_volume.directory", "absolute path",
		},
		{
			"no size cap",
			func(c *Config) { c.HostServices.CacheVolume.SizeCap = 0 },
			"host_services.cache_volume.size_cap", "must be a positive size",
		},
		{
			"a size cap smaller than the storage limits",
			func(c *Config) { c.HostServices.CacheVolume.SizeCap = 1 << 30 },
			"host_services.cache_volume.size_cap", "is smaller than the",
		},
	})
}

//= docs/requirements/07-configuration.md#distributed-cache-section
//= type=test
//# The Distributed cache section SHALL contain an S3 bucket name,
//# region and optional prefix, and MAY contain an endpoint for S3-compatible
//# stores.

// TestDistributedCacheSection checks that the bucket, region, prefix and
// endpoint are read from the file, that the section as a whole and the
// prefix and endpoint within it are optional, and that each field is
// validated.
func TestDistributedCacheSection(t *testing.T) {
	t.Parallel()
	dc := loadTestdata(t, "full.yaml").DistributedCache
	if dc == nil {
		t.Fatal("distributed_cache not loaded")
	}
	if dc.Bucket != "ci-cache" || dc.Region != "eu-west-1" || dc.Prefix != "metal-runner" {
		t.Errorf("distributed_cache = %+v", dc)
	}
	if dc.Endpoint != "https://s3.eu-west-1.amazonaws.com" {
		t.Errorf("endpoint = %q", dc.Endpoint)
	}

	t.Run("bucket and region alone are enough", func(t *testing.T) {
		t.Parallel()
		data := minimalConfig(t) + "\ndistributed_cache:\n  bucket: ci-cache\n  region: eu-west-1\n"
		cfg, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DistributedCache.Prefix != "" || cfg.DistributedCache.Endpoint != "" {
			t.Errorf("distributed_cache = %+v, want no prefix or endpoint", cfg.DistributedCache)
		}
	})

	t.Run("an S3-compatible endpoint gets a region by default", func(t *testing.T) {
		t.Parallel()
		data := minimalConfig(t) + "\ndistributed_cache:\n  bucket: ci-cache\n  endpoint: https://minio.example.com\n"
		cfg, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DistributedCache.Region != DefaultS3Region {
			t.Errorf("region = %q, want %q", cfg.DistributedCache.Region, DefaultS3Region)
		}
	})

	runRejectCases(t, []rejectCase{
		{"no bucket", func(c *Config) { c.DistributedCache.Bucket = "" }, "distributed_cache.bucket", "is required"},
		{"no region", func(c *Config) { c.DistributedCache.Region = "" }, "distributed_cache.region", "is required"},
		{"a prefix starting with a slash", func(c *Config) { c.DistributedCache.Prefix = "/metal" }, "distributed_cache.prefix", "must not start with a slash"},
		{
			"an endpoint that is not a URL",
			func(c *Config) { c.DistributedCache.Endpoint = "s3.eu-west-1.amazonaws.com" },
			"distributed_cache.endpoint", "absolute http or https URL",
		},
		{
			"an endpoint with a path",
			func(c *Config) { c.DistributedCache.Endpoint = "https://minio.example.com/bucket" },
			"distributed_cache.endpoint", "must not have a path",
		},
		{
			"an http endpoint without insecure",
			func(c *Config) { c.DistributedCache.Endpoint = "http://minio.example.com" },
			"distributed_cache.endpoint", "set distributed_cache.insecure",
		},
		{
			"insecure without an endpoint",
			func(c *Config) { c.DistributedCache.Endpoint = ""; c.DistributedCache.Insecure = true },
			"distributed_cache.insecure", "applies only to an S3-compatible endpoint",
		},
	})
}

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# When starting, the Runner SHALL validate the configuration and,
//# if it is invalid, SHALL exit with a non-zero status and a message naming
//# the first invalid field and the reason.

// TestValidationReportsEveryFieldAtOnce checks the error a failed start
// carries: it names the first invalid field in schema order and its reason,
// and it lists every other invalid field so that an operator fixes them in
// one pass instead of one restart per field.
func TestValidationReportsEveryFieldAtOnce(t *testing.T) {
	t.Parallel()
	cfg := fullWith(t, func(c *Config) {
		c.GitLab.Token = ""                              // gitlab.token
		c.Profiles[0].VCPU = 0                           // profiles[0].vcpu
		c.Inventory.Hosts[1].Endpoint = "not-an-address" // inventory.hosts[1].endpoint
		c.PoolManager.Deadline = 0                       // pool_manager.deadline
		c.Observability.LogLevel = "chatty"              // observability.log_level
	})
	err := Validate(cfg)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate = %v, want a *ValidationError", err)
	}
	for _, field := range []string{
		"gitlab.token", "profiles[0].vcpu", "inventory.hosts[1].endpoint",
		"pool_manager.deadline", "observability.log_level",
	} {
		if !hasFieldError(verr.Errors, field, "") {
			t.Errorf("errors do not name %s: %v", field, verr.Errors)
		}
	}

	// The first invalid field in schema order leads the message, with its
	// reason, and the rest follow it.
	first := verr.First()
	if first.Field != "gitlab.token" {
		t.Errorf("First() = %q, want gitlab.token, the first in schema order", first.Field)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "invalid configuration: gitlab.token: is required") {
		t.Errorf("message does not lead with the first invalid field and reason:\n%s", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("(and %d more)", len(verr.Errors)-1)) {
		t.Errorf("message does not say how many more fields are invalid:\n%s", msg)
	}
	for _, field := range []string{"profiles[0].vcpu", "pool_manager.deadline", "observability.log_level"} {
		if !strings.Contains(msg, field) {
			t.Errorf("message does not list %s:\n%s", field, msg)
		}
	}
}

// TestLoadFailsWithTheValidationError checks that Load, the path a start
// takes, returns the same error wrapped with the file name, so the process
// exits non-zero with a message naming the first invalid field.
func TestLoadFailsWithTheValidationError(t *testing.T) {
	t.Parallel()
	data := strings.ReplaceAll(minimalConfig(t), "token: glrt-minimal", `token: ""`)
	_, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "gitlab.token: is required") {
		t.Errorf("error = %v, want it to name the field and the reason", err)
	}
}
