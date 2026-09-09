// Package runnercfg translates a validated config.Config into the
// common.Config and common.RunnerConfig structures that the gitlab-runner
// run loop reads (CF-011), including the S3 cache configuration for the
// `cache:` keyword (CF-081, CF-082). It is the one place besides
// internal/executor that imports gitlab-runner's common package; it lives
// apart from internal/config so that the schema package, which every other
// package imports, stays free of that dependency.
//
// Only the `run` subcommand calls Build. Nothing here reads files or the
// environment; the input is the effective configuration after Load.
package runnercfg

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
	"gitlab.com/gitlab-org/gitlab-runner/common"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// ExecutorName is the executor the RunnerConfig selects. It equals
// internal/executor.Name; the executor's tests check the two agree so that
// this package does not import the executor.
const ExecutorName = "flintlock"

// ShellName is the gitlab-runner shell the RunnerConfig selects (GL-021,
// GL-041).
const ShellName = "bash"

// ErrNoDefaultProfile is returned when the configuration has no Default
// Profile to take the runner-wide builds and cache directories from.
var ErrNoDefaultProfile = errors.New("runnercfg: no default profile")

//= docs/requirements/07-configuration.md#gitlab-section
//# The Runner SHALL map the GitLab section onto the `RunnerConfig`
//# and `Config` structures of the gitlab-runner `common` package.

// Build returns the gitlab-runner configuration for cfg: one common.Config
// carrying the global limits and exactly one common.RunnerConfig, the
// flintlock executor, under it. systemID is the persistent system
// identifier from the state directory (GL-012); it may be empty when the
// caller lets gitlab-runner generate one.
//
// The mapping of the GitLab section is:
//
//	url, token, tls.*        -> RunnerCredentials
//	name                     -> RunnerConfig.Name
//	concurrent               -> Config.Concurrent and RunnerConfig.Limit
//	check_interval           -> Config.CheckInterval (whole seconds)
//	output_limit_kb          -> RunnerConfig.OutputLimit
//	shutdown_timeout         -> Config.ShutdownTimeout (whole seconds)
//	observability.listen_address -> Config.ListenAddress (OB-010, OB-011)
//
// The runner-wide builds and cache directories come from the Default
// Profile, or from the first Profile when none is marked; the Executor sets
// the per-Job values from the resolved Profile (EX-018).
func Build(cfg *config.Config, systemID string) (*common.Config, error) {
	if cfg == nil {
		return nil, errors.New("runnercfg: nil config")
	}
	if len(cfg.Profiles) == 0 {
		return nil, ErrNoDefaultProfile
	}
	dirs := cfg.Profiles[0]
	for _, p := range cfg.Profiles {
		if p.Default {
			dirs = p
			break
		}
	}
	g := cfg.GitLab

	runner := &common.RunnerConfig{
		Name:               g.Name,
		Limit:              g.Concurrent,
		OutputLimit:        g.OutputLimitKB,
		RequestConcurrency: 1,
		SystemID:           systemID,
		ConfigLoadedAt:     time.Now(),
		RunnerCredentials: common.RunnerCredentials{
			URL:         g.URL,
			Token:       string(g.Token),
			TLSCAFile:   g.TLS.CAFile,
			TLSCertFile: g.TLS.CertFile,
			TLSKeyFile:  g.TLS.KeyFile,
		},
		RunnerSettings: common.RunnerSettings{
			Executor:  ExecutorName,
			Shell:     ShellName,
			BuildsDir: dirs.BuildsDir,
			CacheDir:  dirs.CacheDir,
		},
	}
	cache, err := cacheConfig(cfg.DistributedCache)
	if err != nil {
		return nil, err
	}
	runner.Cache = cache

	out := common.NewConfig()
	out.ListenAddress = cfg.Observability.ListenAddress
	out.Concurrent = g.Concurrent
	out.CheckInterval = wholeSeconds(g.CheckInterval)
	out.ShutdownTimeout = wholeSeconds(g.ShutdownTimeout)
	out.Runners = []*common.RunnerConfig{runner}
	out.Loaded = true
	return out, nil
}

// wholeSeconds rounds d up to whole seconds, never below one, because
// gitlab-runner stores these intervals as integer seconds and treats zero
// as "use the built-in default".
func wholeSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

//= docs/requirements/07-configuration.md#distributed-cache-section
//# The Runner SHALL map the Distributed cache section onto the
//# gitlab-runner cache configuration so that the `cache:` keyword in a
//# pipeline stores and restores through that bucket.

// cacheConfig maps the Distributed cache section onto gitlab-runner's S3
// cache configuration. A nil section yields nil, which makes gitlab-runner's
// cache adapter a no-op; the Executor separately fails Jobs that use
// `cache:` in that case (CF-083).
func cacheConfig(dc *config.DistributedCache) (*cacheconfig.Config, error) {
	if dc == nil {
		return nil, nil //nolint:nilnil // nil is the documented "no cache" value gitlab-runner expects
	}
	s3 := &cacheconfig.CacheS3Config{
		BucketName:     dc.Bucket,
		BucketLocation: dc.Region,
		Insecure:       dc.Insecure,
		//= docs/requirements/07-configuration.md#distributed-cache-section
		//# The Runner SHALL configure the distributed cache so that
		//# pre-signed URLs are generated on the Control Node and no AWS credentials
		//# are passed into the guest.
		//
		// IAM authentication makes gitlab-runner sign URLs with the Control
		// Node's own credential chain (instance role, environment, shared
		// config) and hand the guest only the pre-signed URLs. No access key
		// or secret key is set, so the credentials adapter has nothing to
		// forward, and no RoleARN or UploadRoleARN is set, so the go-cloud
		// path that passes temporary credentials into the Job stays off.
		AuthenticationType: cacheconfig.S3AuthTypeIAM,
	}
	if dc.Endpoint != "" {
		addr, err := serverAddress(dc.Endpoint)
		if err != nil {
			return nil, err
		}
		s3.ServerAddress = addr
	}
	return &cacheconfig.Config{
		Type: "s3",
		Path: dc.Prefix,
		// Shared makes the prefix the only namespace, so a re-registered
		// Runner or a second Runner configured with the same bucket and
		// prefix keeps reading the same cache.
		Shared: true,
		S3:     s3,
	}, nil
}

// serverAddress turns the endpoint URL into the host[:port] gitlab-runner
// expects in ServerAddress; the scheme is carried by Insecure.
func serverAddress(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("runnercfg: distributed_cache.endpoint %q is not a URL", endpoint)
	}
	return u.Host, nil
}
