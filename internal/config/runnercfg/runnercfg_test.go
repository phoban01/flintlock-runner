package runnercfg

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/cache"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
	"gitlab.com/gitlab-org/gitlab-runner/common"

	// The binary links the shells package in (GL-041); the test does too, so
	// that the shell this package names can be looked up here.
	_ "gitlab.com/gitlab-org/gitlab-runner/shells"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// noEnv is an environment with nothing set, so the tests never depend on the
// process environment for a secret override.
func noEnv(string) (string, bool) { return "", false }

// baseYAML is a valid configuration with two Profiles; a test appends a
// section to it or replaces one line.
const baseYAML = `
gitlab:
  url: https://gitlab.example.com
  token: glrt-runner-token
  name: metal-runner
  check_interval: 4s
  output_limit_kb: 8192
  shutdown_timeout: 45s
  tls:
    ca_file: /etc/flintlock-runner/gitlab-ca.pem
    cert_file: /etc/flintlock-runner/gitlab-client.pem
    key_file: /etc/flintlock-runner/gitlab-client.key
pool_manager:
  endpoint: 10.0.0.5:9091
inventory:
  hosts:
    - name: host-a
      endpoint: 10.0.1.10:9090
      arch: arm64
      vcpu: 8
      memory_mb: 16384
profiles:
  - name: general
    arch: arm64
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    builds_dir: /builds
    cache_dir: /cache
    pool:
      size: 2
  - name: builders
    arch: arm64
    default: true
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    builds_dir: /srv/builds
    cache_dir: /srv/cache
    pool:
      size: 3
`

// cacheYAML is the distributed cache section the cache tests add.
const cacheYAML = `
distributed_cache:
  bucket: ci-cache
  region: eu-west-1
  prefix: metal-runner
`

// load parses yaml the way `run` does, through internal/config, so the
// translation always starts from an effective, validated configuration.
func load(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml), t.TempDir(), config.WithEnv(noEnv))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return cfg
}

// build is Build with the error checked.
func build(t *testing.T, cfg *config.Config, systemID string) *common.Config {
	t.Helper()
	out, err := Build(cfg, systemID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return out
}

//= docs/requirements/07-configuration.md#gitlab-section
//= type=test
//# The Runner SHALL map the GitLab section onto the `RunnerConfig`
//# and `Config` structures of the gitlab-runner `common` package.

// TestBuildMapsTheGitLabSection checks every field of the GitLab section
// against the gitlab-runner structure that reads it: the credentials the
// network client dials with, the limits the run loop applies, and the
// executor and shell the build picks up.
func TestBuildMapsTheGitLabSection(t *testing.T) {
	t.Parallel()
	cfg := load(t, baseYAML)
	out := build(t, cfg, "s_0123456789ab")

	if len(out.Runners) != 1 {
		t.Fatalf("Runners = %d, want exactly one", len(out.Runners))
	}
	runner := out.Runners[0]

	got := map[string]any{
		"Config.Concurrent":                 out.Concurrent,
		"Config.CheckInterval":              out.CheckInterval,
		"Config.ShutdownTimeout":            out.ShutdownTimeout,
		"Config.ListenAddress":              out.ListenAddress,
		"Config.Loaded":                     out.Loaded,
		"RunnerConfig.Name":                 runner.Name,
		"RunnerConfig.Limit":                runner.Limit,
		"RunnerConfig.OutputLimit":          runner.OutputLimit,
		"RunnerConfig.SystemID":             runner.SystemID,
		"RunnerCredentials.URL":             runner.URL,
		"RunnerCredentials.Token":           runner.Token,
		"RunnerCredentials.TLSCAFile":       runner.TLSCAFile,
		"RunnerCredentials.TLSCertFile":     runner.TLSCertFile,
		"RunnerCredentials.TLSKeyFile":      runner.TLSKeyFile,
		"RunnerSettings.Executor":           runner.Executor,
		"RunnerSettings.Shell":              runner.Shell,
		"RunnerSettings.BuildsDir":          runner.BuildsDir,
		"RunnerSettings.CacheDir":           runner.CacheDir,
		"RunnerConfig.RequestConcurrency":   runner.RequestConcurrency,
		"Config.SessionServer.SessionTimeo": out.SessionServer.SessionTimeout != 0,
	}
	want := map[string]any{
		"Config.Concurrent":                 cfg.GitLab.Concurrent,
		"Config.CheckInterval":              4,
		"Config.ShutdownTimeout":            45,
		"Config.ListenAddress":              cfg.Observability.ListenAddress,
		"Config.Loaded":                     true,
		"RunnerConfig.Name":                 "metal-runner",
		"RunnerConfig.Limit":                cfg.GitLab.Concurrent,
		"RunnerConfig.OutputLimit":          8192,
		"RunnerConfig.SystemID":             "s_0123456789ab",
		"RunnerCredentials.URL":             "https://gitlab.example.com",
		"RunnerCredentials.Token":           "glrt-runner-token",
		"RunnerCredentials.TLSCAFile":       "/etc/flintlock-runner/gitlab-ca.pem",
		"RunnerCredentials.TLSCertFile":     "/etc/flintlock-runner/gitlab-client.pem",
		"RunnerCredentials.TLSKeyFile":      "/etc/flintlock-runner/gitlab-client.key",
		"RunnerSettings.Executor":           ExecutorName,
		"RunnerSettings.Shell":              ShellName,
		"RunnerSettings.BuildsDir":          "/srv/builds",
		"RunnerSettings.CacheDir":           "/srv/cache",
		"RunnerConfig.RequestConcurrency":   1,
		"Config.SessionServer.SessionTimeo": true,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}

	// gitlab-runner has to be able to work with what it was handed: the
	// short description it labels metrics and cache paths with, the shell it
	// generates scripts through, and a deep copy of the whole structure.
	if runner.ShortDescription() == "" {
		t.Error("RunnerCredentials.ShortDescription() is empty; the token did not arrive")
	}
	if common.GetShell(runner.Shell) == nil {
		t.Errorf("gitlab-runner has no shell named %q", runner.Shell)
	}
	if _, err := out.DeepCopy(); err != nil {
		t.Errorf("gitlab-runner cannot copy the configuration: %v", err)
	}
}

// TestBuildTakesTheDirectoriesFromTheDefaultProfile checks which Profile the
// runner-wide builds and cache directories come from, and the errors Build
// reports when there is no Profile at all.
func TestBuildTakesTheDirectoriesFromTheDefaultProfile(t *testing.T) {
	t.Parallel()

	t.Run("the Default Profile", func(t *testing.T) {
		t.Parallel()
		out := build(t, load(t, baseYAML), "")
		if out.Runners[0].BuildsDir != "/srv/builds" {
			t.Errorf("BuildsDir = %q, want the Default Profile's", out.Runners[0].BuildsDir)
		}
	})

	t.Run("the first Profile when none is marked", func(t *testing.T) {
		t.Parallel()
		cfg := load(t, baseYAML)
		cfg.Profiles[1].Default = false
		out := build(t, cfg, "")
		if out.Runners[0].BuildsDir != "/builds" || out.Runners[0].CacheDir != "/cache" {
			t.Errorf("dirs = %q, %q; want the first Profile's", out.Runners[0].BuildsDir, out.Runners[0].CacheDir)
		}
	})

	t.Run("no profiles", func(t *testing.T) {
		t.Parallel()
		cfg := load(t, baseYAML)
		cfg.Profiles = nil
		if _, err := Build(cfg, ""); !errors.Is(err, ErrNoDefaultProfile) {
			t.Errorf("Build = %v, want ErrNoDefaultProfile", err)
		}
	})

	t.Run("no configuration", func(t *testing.T) {
		t.Parallel()
		if _, err := Build(nil, ""); err == nil {
			t.Error("Build(nil) returned no error")
		}
	})
}

//= docs/requirements/07-configuration.md#gitlab-section
//= type=test
//# The Runner SHALL default the concurrency limit to twice the sum
//# of the declared Pool sizes, because immediate-on-lease replenishment keeps
//# Jobs flowing beyond the idle Pool size.

// TestConcurrencyReachesGitLabRunner checks that the derived limit is what
// the run loop and the runner worker actually see: twice the sum of the Pool
// sizes in Config.Concurrent and in RunnerConfig.Limit, and an explicit
// gitlab.concurrent carried through unchanged.
func TestConcurrencyReachesGitLabRunner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		yaml  string
		want  int
		sizes string
	}{
		{"twice the sum of two pools", baseYAML, 10, "2 + 3"},
		{"twice a single pool", strings.Replace(baseYAML, "      size: 3\n", "      size: 7\n", 1), 18, "2 + 7"},
		{"an explicit limit is kept", strings.Replace(baseYAML, "  name: metal-runner", "  name: metal-runner\n  concurrent: 3", 1), 3, "explicit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := build(t, load(t, tt.yaml), "")
			if out.Concurrent != tt.want {
				t.Errorf("Config.Concurrent = %d, want %d (%s)", out.Concurrent, tt.want, tt.sizes)
			}
			if out.Runners[0].Limit != tt.want {
				t.Errorf("RunnerConfig.Limit = %d, want %d", out.Runners[0].Limit, tt.want)
			}
		})
	}
}

//= docs/requirements/07-configuration.md#distributed-cache-section
//= type=test
//# The Runner SHALL map the Distributed cache section onto the
//# gitlab-runner cache configuration so that the `cache:` keyword in a
//# pipeline stores and restores through that bucket.

// TestDistributedCacheMapping checks the cache configuration a `cache:` step
// reads: the type gitlab-runner resolves to a factory, the bucket and region
// that factory dials, the prefix the object path is built under, and that
// gitlab-runner can in fact build an adapter from it. Without a
// distributed_cache section the RunnerConfig carries no cache at all, which
// is what makes gitlab-runner fall back to its no-op adapter.
func TestDistributedCacheMapping(t *testing.T) {
	t.Parallel()

	t.Run("the section maps onto the cache configuration", func(t *testing.T) {
		t.Parallel()
		out := build(t, load(t, baseYAML+cacheYAML), "")
		cc := out.Runners[0].Cache
		if cc == nil {
			t.Fatal("RunnerConfig.Cache is nil although distributed_cache is configured")
		}
		if cc.Type != "s3" {
			t.Errorf("Cache.Type = %q, want s3", cc.Type)
		}
		if cc.Path != "metal-runner" {
			t.Errorf("Cache.Path = %q, want the configured prefix", cc.Path)
		}
		if !cc.GetShared() {
			t.Error("Cache.Shared = false; the prefix would gain a per-token namespace and a re-registered Runner would lose its cache")
		}
		if cc.S3 == nil {
			t.Fatal("Cache.S3 is nil")
		}
		if cc.S3.BucketName != "ci-cache" || cc.S3.BucketLocation != "eu-west-1" {
			t.Errorf("S3 = %+v, want the configured bucket and region", cc.S3)
		}
		if cc.S3.ServerAddress != "" {
			t.Errorf("S3.ServerAddress = %q, want empty for AWS S3", cc.S3.ServerAddress)
		}

		// gitlab-runner has to find a factory for the type and build a real
		// adapter from it; otherwise every `cache:` step silently no-ops.
		if _, err := cache.Factories().Find(cc.Type); err != nil {
			t.Fatalf("gitlab-runner has no cache factory for %q: %v", cc.Type, err)
		}
		adapter := cache.GetAdapter(cc, time.Minute, "shorttok", "42", "cache-key", false)
		if name := fmt.Sprintf("%T", adapter); !strings.HasPrefix(name, "*s3.") {
			t.Errorf("cache adapter = %s, want gitlab-runner's S3 adapter; `cache:` would not reach the bucket", name)
		}
	})

	t.Run("an S3-compatible endpoint becomes the server address", func(t *testing.T) {
		t.Parallel()
		yaml := baseYAML + cacheYAML + "  endpoint: https://minio.example.com:9000\n"
		out := build(t, load(t, yaml), "")
		if got := out.Runners[0].Cache.S3.ServerAddress; got != "minio.example.com:9000" {
			t.Errorf("S3.ServerAddress = %q, want host:port without the scheme", got)
		}
	})

	// gitlab-runner reads CacheS3Config.Insecure only on its access-key path
	// (cache/s3/minio.go): the IAM path this mapping selects builds its client
	// with Secure: true and instance-metadata credentials and never looks at
	// the field. Carrying distributed_cache.insecure through would produce a
	// configuration that says http and dials https, so the value is rejected
	// at validation and never reaches the mapping.
	t.Run("an insecure endpoint is rejected before it reaches the mapping", func(t *testing.T) {
		t.Parallel()
		yaml := baseYAML + cacheYAML + "  endpoint: http://minio.example.com:9000\n  insecure: true\n"
		if _, err := config.Parse([]byte(yaml), t.TempDir(), config.WithEnv(noEnv)); err == nil {
			t.Fatal("config.Parse accepted an http distributed_cache endpoint; the IAM client would dial https")
		}
	})

	t.Run("insecure is not silently honoured", func(t *testing.T) {
		t.Parallel()
		cfg := load(t, baseYAML+cacheYAML+"  endpoint: https://minio.example.com:9000\n")
		// Set the field behind validation's back: the mapping must not carry
		// it into a configuration that reads as plaintext.
		cfg.DistributedCache.Insecure = true
		out := build(t, cfg, "")
		s3 := out.Runners[0].Cache.S3
		if s3.Insecure {
			t.Error("S3.Insecure = true; the IAM client ignores it and dials TLS, so the cache configuration would misrepresent the connection")
		}
		if s3.AuthType() != cacheconfig.S3AuthTypeIAM {
			t.Fatalf("S3.AuthType() = %q, want IAM; the reason Insecure cannot be honoured", s3.AuthType())
		}
	})

	t.Run("no section means no cache configuration", func(t *testing.T) {
		t.Parallel()
		out := build(t, load(t, baseYAML), "")
		if out.Runners[0].Cache != nil {
			t.Errorf("Cache = %+v, want nil without a distributed_cache section", out.Runners[0].Cache)
		}
		if adapter := cache.GetAdapter(out.Runners[0].Cache, time.Minute, "shorttok", "42", "cache-key", false); adapter == nil {
			t.Error("gitlab-runner returned no adapter at all")
		}
	})
}

//= docs/requirements/07-configuration.md#distributed-cache-section
//= type=test
//# The Runner SHALL configure the distributed cache so that
//# pre-signed URLs are generated on the Control Node and no AWS credentials
//# are passed into the guest.

// TestDistributedCachePassesNoCredentialsIntoTheGuest checks the two halves
// of the requirement against gitlab-runner's own cache code: the S3
// configuration carries no credential a Job could be handed and authenticates
// as IAM, so signing happens here with the Control Node's credential chain;
// and the adapter offers no Go Cloud URL, which is the only path by which
// gitlab-runner exports credentials into the build environment. What is left
// for the guest is the pre-signed URL.
func TestDistributedCachePassesNoCredentialsIntoTheGuest(t *testing.T) {
	t.Parallel()
	for _, yaml := range []string{
		baseYAML + cacheYAML,
		baseYAML + cacheYAML + "  endpoint: https://minio.example.com:9000\n",
	} {
		out := build(t, load(t, yaml), "")
		s3 := out.Runners[0].Cache.S3

		for name, value := range map[string]string{
			"AccessKey":                 s3.AccessKey,
			"SecretKey":                 s3.SecretKey,
			"SessionToken":              s3.SessionToken,
			"RoleARN":                   s3.RoleARN,
			"UploadRoleARN":             s3.UploadRoleARN,
			"ServerSideEncryptionKeyID": s3.ServerSideEncryptionKeyID,
		} {
			if value != "" {
				t.Errorf("S3.%s = %q; a credential in the configuration can reach a Job", name, value)
			}
		}
		if s3.AuthType() != cacheconfig.S3AuthTypeIAM {
			t.Errorf("S3.AuthType() = %q, want %q so gitlab-runner signs with the Control Node's credential chain",
				s3.AuthType(), cacheconfig.S3AuthTypeIAM)
		}

		adapter := cache.GetAdapter(out.Runners[0].Cache, time.Minute, "shorttok", "42", "cache-key", false)
		for _, upload := range []bool{false, true} {
			goCloud, err := adapter.GetGoCloudURL(t.Context(), upload)
			if err != nil {
				t.Fatalf("GetGoCloudURL(upload=%v): %v", upload, err)
			}
			if goCloud.URL != nil {
				t.Errorf("the adapter offers a Go Cloud URL (upload=%v); gitlab-runner would export credentials into the build environment", upload)
			}
			if len(goCloud.Environment) != 0 {
				t.Errorf("the adapter exports %v into the build environment (upload=%v)", goCloud.Environment, upload)
			}
		}
	}
}

// TestWholeSeconds covers the rounding of the intervals gitlab-runner stores
// as integer seconds, where zero means "use the built-in default".
func TestWholeSeconds(t *testing.T) {
	t.Parallel()
	tests := map[time.Duration]int{
		0:                       0,
		-time.Second:            0,
		time.Nanosecond:         1,
		500 * time.Millisecond:  1,
		time.Second:             1,
		1500 * time.Millisecond: 2,
		3 * time.Second:         3,
		90 * time.Second:        90,
	}
	for in, want := range tests {
		if got := wholeSeconds(in); got != want {
			t.Errorf("wholeSeconds(%s) = %d, want %d", in, got, want)
		}
	}
}

// TestServerAddress covers the endpoint-to-host translation, including the
// malformed input Build reports as an error.
func TestServerAddress(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"https://s3.eu-west-1.amazonaws.com": "s3.eu-west-1.amazonaws.com",
		"http://minio.example.com:9000":      "minio.example.com:9000",
	}
	for in, want := range tests {
		got, err := serverAddress(in)
		if err != nil {
			t.Fatalf("serverAddress(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("serverAddress(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := serverAddress("not a url"); err == nil {
		t.Error("serverAddress accepted a value that is not a URL")
	}

	cfg := load(t, baseYAML+cacheYAML)
	cfg.DistributedCache.Endpoint = "://bad"
	if _, err := Build(cfg, ""); err == nil {
		t.Error("Build accepted a distributed_cache endpoint that is not a URL")
	}
}
