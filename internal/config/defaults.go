package config

import (
	"os"
	"runtime"
	"time"
)

// Documented defaults (CF-005). Every optional field of the schema takes one
// of these when it is left unset; the doc comments in types.go name the
// same values. Change both together.
const (
	// DefaultCheckInterval is the gitlab-runner check_interval.
	DefaultCheckInterval = 3 * time.Second
	// DefaultOutputLimitKB is gitlab-runner's 4 MiB Job log limit in KB.
	DefaultOutputLimitKB = 4096
	// DefaultShutdownTimeout is gitlab-runner's graceful shutdown bound.
	DefaultShutdownTimeout = 30 * time.Second
	// DefaultConcurrencyFactor multiplies the sum of Pool sizes (CF-012).
	DefaultConcurrencyFactor = 2

	DefaultProfileVCPU     = 2
	DefaultProfileMemoryMB = 2048
	DefaultShell           = "/bin/bash"
	DefaultBuildsDir       = "/builds"
	DefaultCacheDir        = "/cache"
	DefaultHelperPath      = "/usr/local/bin/gitlab-runner-helper"
	DefaultUser            = "root"
	DefaultReadyTimeout    = 60 * time.Second

	DefaultHeartbeatInterval = 10 * time.Second
	DefaultHeartbeatExpiry   = 30 * time.Second

	DefaultPoolManagerDeadline                 = 10 * time.Second
	DefaultPoolManagerHealthBackoff            = 30 * time.Second
	DefaultPoolManagerHealthInterval           = 10 * time.Second
	DefaultPoolManagerHealthThreshold          = 3
	DefaultPoolManagerEventsPoll               = 5 * time.Second
	DefaultPoolManagerDeclareRetry             = 30 * time.Second
	DefaultPoolManagerReleaseRetry             = 5
	DefaultAllocationTimeout                   = 5 * time.Minute
	DefaultHostHealthInterval                  = 10 * time.Second
	DefaultHostUnhealthyThreshold              = 3
	DefaultHostCallDeadline                    = 10 * time.Second
	DefaultKeepDuration                        = time.Hour
	DefaultPrepareTimeout                      = 5 * time.Minute
	DefaultGracefulKillTimeout                 = 30 * time.Second
	DefaultTransportDeadline                   = 30 * time.Second
	DefaultFleetParallelism                    = 4
	DefaultFlintlockdPort                      = 9090
	DefaultGuestSubnet                         = "172.31.0.0/16"
	DefaultHostReserveVCPU                     = 2
	DefaultHostReserveMemoryMB                 = 4096
	DefaultInventoryPath                       = "/etc/flintlock-runner/inventory.yaml"
	DefaultRunnerConfigPath                    = "/etc/flintlock-runner/config.yaml"
	DefaultDrainTimeout                        = 30 * time.Minute
	DefaultVerificationTimeout                 = 10 * time.Minute
	DefaultSSHPort                             = 22
	DefaultBuildkitPort                        = 1234
	DefaultGoProxyPort                         = 3000
	DefaultRegistryMirrorPort                  = 5000
	DefaultHTTPCachePort                       = 3128
	DefaultGoProxyUpstream                     = "https://proxy.golang.org"
	DefaultPrivateRevalidateInterval           = 24 * time.Hour
	DefaultRegistryUpstream                    = "https://registry-1.docker.io"
	DefaultHTTPCacheTTL                        = 24 * time.Hour
	DefaultCacheVolumeDirectory                = "/var/lib/flintlock-runner/cache"
	DefaultCacheVolumeSizeCap         ByteSize = 200 << 30
	DefaultS3Region                            = "us-east-1"
	DefaultLogFormat                           = LogJSON
	DefaultLogLevel                            = "info"
	DefaultListenAddress                       = ":9252"
	DefaultStateDir                            = "/var/lib/flintlock-runner"
)

// hostname is the fallback runner name; a variable so tests can pin it.
var hostname = os.Hostname

//= docs/requirements/07-configuration.md#file-and-precedence
//# The Runner SHALL apply documented defaults to every optional
//# field so that a minimal configuration contains only the GitLab URL, the
//# runner token, the Pool Manager endpoint, one Host and one Profile with a
//# Pool size.

// ApplyDefaults fills every unset optional field with its documented
// default. It is idempotent and never overwrites a value the operator set.
// Load calls it after the Inventory is resolved and before Validate, so the
// fields Validate requires are exactly those without a default: the GitLab
// URL and token, the Pool Manager endpoint, each Host's name, endpoint,
// architecture and capacity, and each Profile's name, kernel image, root
// filesystem and Pool size.
func ApplyDefaults(c *Config) {
	applyGitLabDefaults(c)
	applySchedulerDefaults(c)
	applyProfileDefaults(c)
	applyPoolManagerDefaults(&c.PoolManager)
	applyExecutorDefaults(&c.Executor)
	applyFleetDefaults(c.Fleet)
	applyHostServicesDefaults(&c.HostServices)
	applyDistributedCacheDefaults(c.DistributedCache)
	applyObservabilityDefaults(&c.Observability)
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
}

func applyGitLabDefaults(c *Config) {
	g := &c.GitLab
	if g.Name == "" {
		if h, err := hostname(); err == nil {
			g.Name = h
		}
	}
	if g.Concurrent == 0 {
		g.Concurrent = defaultConcurrency(c.Profiles)
	}
	if g.CheckInterval == 0 {
		g.CheckInterval = DefaultCheckInterval
	}
	if g.OutputLimitKB == 0 {
		g.OutputLimitKB = DefaultOutputLimitKB
	}
	if g.ShutdownTimeout == 0 {
		g.ShutdownTimeout = DefaultShutdownTimeout
	}
}

//= docs/requirements/07-configuration.md#gitlab-section
//# The Runner SHALL default the concurrency limit to twice the sum
//# of the declared Pool sizes, because immediate-on-lease replenishment keeps
//# Jobs flowing beyond the idle Pool size.

// defaultConcurrency is twice the sum of the declared Pool sizes.
func defaultConcurrency(profiles []Profile) int {
	sum := 0
	for _, p := range profiles {
		sum += p.Pool.Size
	}
	return DefaultConcurrencyFactor * sum
}

//= docs/requirements/07-configuration.md#scheduler-section
//# The Runner SHALL default the Runner namespace to the runner
//# name so that two Runners sharing one Pool Manager declare their Pools in
//# separate namespaces.

func applySchedulerDefaults(c *Config) {
	s := &c.Scheduler
	if s.Namespace == "" {
		s.Namespace = c.GitLab.Name
	}
	if s.AllocationTimeout == 0 {
		s.AllocationTimeout = DefaultAllocationTimeout
	}
	if s.HostHealthInterval == 0 {
		s.HostHealthInterval = DefaultHostHealthInterval
	}
	if s.HostUnhealthyThreshold == 0 {
		s.HostUnhealthyThreshold = DefaultHostUnhealthyThreshold
	}
	if s.HostCallDeadline == 0 {
		s.HostCallDeadline = DefaultHostCallDeadline
	}
	if s.KeepOnFailure && s.KeepDuration == 0 {
		s.KeepDuration = DefaultKeepDuration
	}
}

//= docs/requirements/07-configuration.md#profiles-section
//# The Runner SHALL default a Pool's name to the Profile name and
//# its namespace to the Runner namespace.

func applyProfileDefaults(c *Config) {
	arch := defaultArch(c.Inventory.Hosts)
	for i := range c.Profiles {
		p := &c.Profiles[i]
		if p.Arch == "" {
			p.Arch = arch
		}
		if p.VCPU == 0 {
			p.VCPU = DefaultProfileVCPU
		}
		if p.MemoryMB == 0 {
			p.MemoryMB = DefaultProfileMemoryMB
		}
		if p.Shell == "" {
			p.Shell = DefaultShell
		}
		if p.BuildsDir == "" {
			p.BuildsDir = DefaultBuildsDir
		}
		if p.CacheDir == "" {
			p.CacheDir = DefaultCacheDir
		}
		if p.HelperPath == "" {
			p.HelperPath = DefaultHelperPath
		}
		if p.Transport.Kind == "" {
			p.Transport.Kind = TransportExec
		}
		if p.User == "" {
			p.User = DefaultUser
		}
		if p.Transport.Kind == TransportSSH && p.Transport.SSH.User == "" {
			p.Transport.SSH.User = p.User
		}
		if p.ReadyTimeout == 0 {
			p.ReadyTimeout = DefaultReadyTimeout
		}
		if p.Pool.Name == "" {
			p.Pool.Name = p.Name
		}
		if p.Pool.Namespace == "" {
			p.Pool.Namespace = c.Scheduler.Namespace
		}
		if p.Pool.Strategy == "" {
			p.Pool.Strategy = ReplenishImmediateOnLease
		}
		if p.Pool.HookFailurePolicy == "" && (len(p.Pool.CreateHooks) > 0 || len(p.Pool.PreLeaseHooks) > 0) {
			p.Pool.HookFailurePolicy = HookFailureDeleteAndReplace
		}
		if p.Pool.HeartbeatInterval == 0 {
			p.Pool.HeartbeatInterval = DefaultHeartbeatInterval
		}
		if p.Pool.HeartbeatExpiry == 0 {
			p.Pool.HeartbeatExpiry = DefaultHeartbeatExpiry
		}
	}
	if len(c.Profiles) == 1 {
		// A single Profile is the Default Profile (CF-022 allows one).
		c.Profiles[0].Default = true
	}
}

// defaultArch is the architecture every Inventory Host shares, or the
// Runner's own when the Hosts disagree or there are none.
func defaultArch(hosts []HostEntry) Architecture {
	var shared Architecture
	for _, h := range hosts {
		if shared == "" {
			shared = h.Arch
			continue
		}
		if h.Arch != shared {
			shared = ""
			break
		}
	}
	if shared != "" {
		return shared
	}
	return Architecture(runtime.GOARCH)
}

func applyPoolManagerDefaults(pm *PoolManager) {
	if pm.Deadline == 0 {
		pm.Deadline = DefaultPoolManagerDeadline
	}
	if pm.HealthBackoff == 0 {
		pm.HealthBackoff = DefaultPoolManagerHealthBackoff
	}
	if pm.HealthInterval == 0 {
		pm.HealthInterval = DefaultPoolManagerHealthInterval
	}
	if pm.HealthFailureThreshold == 0 {
		pm.HealthFailureThreshold = DefaultPoolManagerHealthThreshold
	}
	if pm.EventsPollInterval == 0 {
		pm.EventsPollInterval = DefaultPoolManagerEventsPoll
	}
	if pm.DeclareRetryInterval == 0 {
		pm.DeclareRetryInterval = DefaultPoolManagerDeclareRetry
	}
	if pm.ReleaseRetryLimit == 0 {
		pm.ReleaseRetryLimit = DefaultPoolManagerReleaseRetry
	}
}

func applyExecutorDefaults(e *Executor) {
	if e.PrepareTimeout == 0 {
		e.PrepareTimeout = DefaultPrepareTimeout
	}
	if e.GracefulKillTimeout == 0 {
		e.GracefulKillTimeout = DefaultGracefulKillTimeout
	}
	if e.TransportDeadline == 0 {
		e.TransportDeadline = DefaultTransportDeadline
	}
}

func applyFleetDefaults(f *Fleet) {
	if f == nil {
		return
	}
	if f.Remote.Mode == "" {
		// The static provider (TD-044) lists machines that have no Systems
		// Manager agent, so it defaults to SSH; everything else to SSM.
		f.Remote.Mode = RemoteSSM
		d := &f.Discovery
		if len(d.Static) > 0 && d.TagKey == "" && d.TagValue == "" && len(d.InstanceIDs) == 0 {
			f.Remote.Mode = RemoteSSH
		}
	}
	if f.Remote.Mode == RemoteSSH && f.Remote.SSH.Port == 0 {
		f.Remote.SSH.Port = DefaultSSHPort
	}
	if f.Parallelism == 0 {
		f.Parallelism = DefaultFleetParallelism
	}
	if f.GuestSubnet == "" {
		f.GuestSubnet = DefaultGuestSubnet
	}
	if f.Flintlockd.Port == 0 {
		f.Flintlockd.Port = DefaultFlintlockdPort
	}
	if f.HostReserve.VCPU == 0 {
		f.HostReserve.VCPU = DefaultHostReserveVCPU
	}
	if f.HostReserve.MemoryMB == 0 {
		f.HostReserve.MemoryMB = DefaultHostReserveMemoryMB
	}
	if f.InventoryPath == "" {
		f.InventoryPath = DefaultInventoryPath
	}
	if f.RunnerConfigPath == "" {
		f.RunnerConfigPath = DefaultRunnerConfigPath
	}
	if f.DrainTimeout == 0 {
		f.DrainTimeout = DefaultDrainTimeout
	}
	if f.VerificationTimeout == 0 {
		f.VerificationTimeout = DefaultVerificationTimeout
	}
}

//= docs/requirements/07-configuration.md#host-services-section
//# The Host services section SHALL contain, for each of
//# `buildkit`, `go_proxy`, `registry_mirror` and `http_cache`, an enabled flag
//# that defaults to true and a port.

func applyHostServicesDefaults(hs *HostServices) {
	applyServiceDefaults(&hs.Buildkit.Service, DefaultBuildkitPort)
	applyServiceDefaults(&hs.GoProxy.Service, DefaultGoProxyPort)
	applyServiceDefaults(&hs.RegistryMirror.Service, DefaultRegistryMirrorPort)
	applyServiceDefaults(&hs.HTTPCache.Service, DefaultHTTPCachePort)
	if hs.GoProxy.Upstream == "" {
		hs.GoProxy.Upstream = DefaultGoProxyUpstream
	}
	if len(hs.GoProxy.PrivatePatterns) > 0 && hs.GoProxy.PrivateRevalidateInterval == 0 {
		hs.GoProxy.PrivateRevalidateInterval = DefaultPrivateRevalidateInterval
	}
	if len(hs.RegistryMirror.Upstreams) == 0 {
		hs.RegistryMirror.Upstreams = []RegistryUpstream{{URL: DefaultRegistryUpstream}}
	}
	for i := range hs.HTTPCache.Upstreams {
		if hs.HTTPCache.Upstreams[i].TTL == 0 {
			hs.HTTPCache.Upstreams[i].TTL = DefaultHTTPCacheTTL
		}
	}
	if hs.CacheVolume.Device == "" && hs.CacheVolume.Directory == "" {
		hs.CacheVolume.Directory = DefaultCacheVolumeDirectory
	}
	if hs.CacheVolume.SizeCap == 0 {
		hs.CacheVolume.SizeCap = DefaultCacheVolumeSizeCap
	}
}

func applyServiceDefaults(s *Service, port int) {
	if s.Enabled == nil {
		enabled := true
		s.Enabled = &enabled
	}
	if s.Port == 0 {
		s.Port = port
	}
}

func applyDistributedCacheDefaults(dc *DistributedCache) {
	if dc == nil {
		return
	}
	if dc.Region == "" && dc.Endpoint != "" {
		// S3-compatible stores accept any region; the AWS SDK needs one.
		dc.Region = DefaultS3Region
	}
}

func applyObservabilityDefaults(o *Observability) {
	if o.LogFormat == "" {
		o.LogFormat = DefaultLogFormat
	}
	if o.LogLevel == "" {
		o.LogLevel = DefaultLogLevel
	}
	if o.ListenAddress == "" {
		o.ListenAddress = DefaultListenAddress
	}
}

// IsEnabled reports the effective enabled flag of a Host Service: unset
// means enabled (CF-070).
func (s Service) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }
