package config

import "time"

// Secret is a configuration value that `config show` redacts (CF-006) and
// that is never written to a log, metric label, Job log or MicroVM metadata
// (SE-010). Each Secret field documents the environment variable that
// overrides it (CF-002); the environment wins over the file.
type Secret string

// Architecture is a CPU architecture as used by Profiles, Hosts and the
// Fleet Controller when selecting binaries and images (FL-027).
type Architecture string

// Architectures the project supports. EC2 metal instances are one of these.
const (
	ArchAMD64 Architecture = "amd64"
	ArchARM64 Architecture = "arm64"
)

// Config is the root of the single YAML configuration file (CF-001).
//
// The minimal valid file contains only the GitLab URL, the runner token, the
// Pool Manager endpoint, one Host and one Profile with a Pool size (CF-005);
// every other field has a documented default.
type Config struct {
	// GitLab configures access to GitLab (CF-010).
	GitLab GitLab `yaml:"gitlab"`
	// Profiles lists the MicroVM Profiles (CF-020). At least one is required.
	Profiles []Profile `yaml:"profiles"`
	// Inventory is the set of Hosts, inline or by file reference (CF-003).
	Inventory Inventory `yaml:"inventory"`
	// PoolManager configures the battery connection (CF-040). Required (CF-041).
	PoolManager PoolManager `yaml:"pool_manager"`
	// Scheduler configures capacity, allocation and health (CF-050).
	Scheduler Scheduler `yaml:"scheduler"`
	// Executor configures the timeouts of the Executor and the Guest
	// Transport. This section is not enumerated in 07-configuration.md; it
	// carries the "configured" values referenced by EX-017, EX-024, EX-051
	// and HO-003.
	Executor Executor `yaml:"executor"`
	// Fleet configures the Fleet Controller (CF-060). Only read by the
	// `fleet` subcommands, optional for `run`.
	Fleet *Fleet `yaml:"fleet,omitempty"`
	// HostServices configures the per-Host services (CF-070).
	HostServices HostServices `yaml:"host_services"`
	// DistributedCache configures the S3-backed GitLab cache (CF-080). When
	// absent, `cache:` is unavailable and Jobs using it fail (CF-083).
	DistributedCache *DistributedCache `yaml:"distributed_cache,omitempty"`
	// Observability configures logging, metrics and health endpoints
	// (OB-001, OB-010, OB-030). Not enumerated in 07-configuration.md.
	Observability Observability `yaml:"observability"`
	// StateDir is the directory holding the persistent system identifier
	// (GL-012) and any cached state; it is created owner-only (SE-013).
	StateDir string `yaml:"state_dir"`
}

// GitLab is the GitLab section (CF-010). It maps onto the gitlab-runner
// RunnerConfig and Config structures (CF-011).
type GitLab struct {
	// URL is the GitLab instance URL. Must be https unless AllowInsecure is
	// set (SE-020).
	URL string `yaml:"url"`
	// Token is the runner authentication token beginning with `glrt-`
	// (GL-010). Environment override: FLINTLOCK_RUNNER_GITLAB_TOKEN.
	Token Secret `yaml:"token"`
	// Name is the runner name reported to GitLab; it is also the default
	// Runner namespace (CF-051) and the value of the
	// gitlab-runner.flintlock.dev/runner Pool label (PL-014).
	Name string `yaml:"name"`
	// Concurrent is the concurrency limit (GL-033). Zero means the default of
	// twice the sum of the declared Pool sizes (CF-012).
	Concurrent int `yaml:"concurrent"`
	// CheckInterval is the interval between job requests when no Reservation
	// is available; maps onto gitlab-runner's check_interval.
	CheckInterval time.Duration `yaml:"check_interval"`
	// OutputLimitKB is the Job log limit in kilobytes (GL-054).
	OutputLimitKB int `yaml:"output_limit_kb"`
	// ShutdownTimeout bounds graceful shutdown (GL-071, GL-072).
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// TLS holds optional CA, client certificate and key paths.
	TLS ClientTLS `yaml:"tls"`
	// AllowInsecure permits a non-https URL (SE-020). Default false.
	AllowInsecure bool `yaml:"allow_insecure"`
}

// ClientTLS is the TLS material a client presents or trusts (HO-005, HO-006,
// PL-003, SE-021, SE-022). There is deliberately no field to skip server
// certificate verification (SE-022).
type ClientTLS struct {
	// CAFile is the certificate authority used to verify the server.
	CAFile string `yaml:"ca_file,omitempty"`
	// CertFile and KeyFile are presented for mutual TLS when both are set.
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
	// Insecure marks the endpoint as plaintext. It has to be explicit
	// (SE-021); the default is TLS.
	Insecure bool `yaml:"insecure,omitempty"`
}

// Profile describes the MicroVM a class of Jobs runs in (CF-020, CF-021)
// and the Pool it is bound to (CF-025). A Profile is the single source of
// truth from which the Scheduler declares its Pool (PL-010).
type Profile struct {
	// Name is unique across Profiles (CF-020) and is the default Pool name
	// (CF-028).
	Name string `yaml:"name"`
	// Arch selects Hosts of that architecture for the Pool (PL-011).
	Arch Architecture `yaml:"arch"`
	// VCPU and MemoryMB size the MicroVM template (PL-020).
	VCPU     int `yaml:"vcpu"`
	MemoryMB int `yaml:"memory_mb"`
	// Kernel is the kernel image and command line (PL-020, PL-022).
	Kernel Kernel `yaml:"kernel"`
	// Initrd is the optional initial ramdisk image (CF-021).
	Initrd *Initrd `yaml:"initrd,omitempty"`
	// RootFS is the root filesystem OCI image. It has to carry a tag or a
	// digest (CF-026); a tag alone produces a startup warning (CF-027).
	RootFS string `yaml:"rootfs"`
	// AdditionalVolumes are attached to the MicroVM (CF-021, SE-003).
	AdditionalVolumes []Volume `yaml:"additional_volumes,omitempty"`
	// Provider is the flintlock hypervisor provider name; empty means the
	// Host's default (PL-020).
	Provider string `yaml:"provider,omitempty"`
	// UserData is a cloud-init user-data template rendered with the Profile
	// name available (PL-025). It never carries Job-specific values (PL-026).
	UserData string `yaml:"user_data,omitempty"`
	// Shell is the shell path executed for every Stage (EX-020). Default
	// /bin/bash.
	Shell string `yaml:"shell"`
	// BuildsDir and CacheDir are the guest directories generated scripts use
	// (EX-016, EX-018, EX-025).
	BuildsDir string `yaml:"builds_dir"`
	CacheDir  string `yaml:"cache_dir"`
	// HelperPath is the path of gitlab-runner-helper inside the guest
	// (EX-018).
	HelperPath string `yaml:"helper_path"`
	// Images are Job Image names that map onto this Profile exactly; unique
	// across Profiles (CF-023, SC-010).
	Images []string `yaml:"images,omitempty"`
	// ImageGlobs are glob patterns tried in configuration order after exact
	// names (SC-010).
	ImageGlobs []string `yaml:"image_globs,omitempty"`
	// Default marks the Default Profile; at most one Profile sets it (CF-022).
	Default bool `yaml:"default,omitempty"`
	// Transport selects and configures the Guest Transport (EX-042).
	Transport Transport `yaml:"transport"`
	// User is the guest user Stages run as; default root (EX-026).
	User string `yaml:"user,omitempty"`
	// ReadyTimeout bounds the wait for guest readiness (EX-014, EX-015).
	ReadyTimeout time.Duration `yaml:"ready_timeout"`
	// HostSelector is a set of label requirements every Pool Host has to
	// satisfy, matched by equality against Inventory labels (CF-024, PL-011).
	HostSelector map[string]string `yaml:"host_selector,omitempty"`
	// MaxConcurrency caps concurrent Allocations for this Profile (SC-006).
	// Zero means unlimited.
	MaxConcurrency int `yaml:"max_concurrency,omitempty"`
	// Pool holds the Pool settings (CF-025).
	Pool PoolSettings `yaml:"pool"`
}

// Kernel is a Profile's kernel image (PL-020, PL-022).
type Kernel struct {
	// Image is the OCI image holding the kernel.
	Image string `yaml:"image"`
	// Filename is the kernel file inside the image; empty uses flintlock's
	// default.
	Filename string `yaml:"filename,omitempty"`
	// Cmdline is added to the provider's default kernel command line.
	Cmdline map[string]string `yaml:"cmdline,omitempty"`
}

// Initrd is a Profile's optional initial ramdisk (CF-021, PL-020).
type Initrd struct {
	Image    string `yaml:"image"`
	Filename string `yaml:"filename,omitempty"`
}

// Volume is an additional volume attached to a MicroVM (CF-021, PL-020).
// Only volumes declared here are mounted (SE-003).
type Volume struct {
	// ID is the volume identifier inside the MicroVM spec.
	ID string `yaml:"id"`
	// Image is the OCI image the volume is sourced from.
	Image string `yaml:"image"`
	// MountPoint is where cloud-init mounts it in the guest.
	MountPoint string `yaml:"mount_point,omitempty"`
	// ReadOnly mounts the volume read-only.
	ReadOnly bool `yaml:"read_only,omitempty"`
	// SizeMB optionally resizes the volume.
	SizeMB int `yaml:"size_mb,omitempty"`
}

// TransportKind names a Guest Transport implementation (EX-042).
type TransportKind string

// The Guest Transport implementations. Exec is the default.
const (
	TransportExec TransportKind = "exec"
	TransportSSH  TransportKind = "ssh"
)

// Transport is a Profile's Guest Transport selection (EX-042).
type Transport struct {
	// Kind is exec or ssh; default exec.
	Kind TransportKind `yaml:"kind"`
	// SSH is read only when Kind is ssh.
	SSH SSHTransport `yaml:"ssh,omitempty"`
}

// SSHTransport configures the ssh Guest Transport (EX-047, EX-049). It always
// goes through the flintlock MicroVMSSHProxy RPC; there is no direct mode
// (EX-048).
type SSHTransport struct {
	// User is the SSH user; default is the Profile user.
	User string `yaml:"user,omitempty"`
	// PrivateKeyFile is the private key used to authenticate (EX-049).
	PrivateKeyFile string `yaml:"private_key_file"`
	// KnownHostKey is the guest host public key; when empty the host key is
	// not verified (EX-049).
	KnownHostKey string `yaml:"known_host_key,omitempty"`
}

// ReplenishmentStrategy names a battery replenishment strategy (PL-013,
// TD-003). The values are the snake_case forms of the proto enum.
type ReplenishmentStrategy string

// Replenishment strategies. ImmediateOnLease is the default (PL-013).
const (
	ReplenishImmediateOnLease ReplenishmentStrategy = "immediate_on_lease"
	ReplenishMinSizeThreshold ReplenishmentStrategy = "min_size_threshold"
	ReplenishReplaceOnDelete  ReplenishmentStrategy = "replace_on_delete"
)

// HookFailurePolicy names what battery does with a MicroVM whose hook fails.
type HookFailurePolicy string

// Hook failure policies, mirroring the proto enum.
const (
	HookFailureDeleteAndReplace HookFailurePolicy = "delete_and_replace"
	HookFailureQuarantine       HookFailurePolicy = "quarantine"
)

// PoolSettings are a Profile's Pool settings (CF-025). Name defaults to the
// Profile name and Namespace to the Runner namespace (CF-028); two Profiles
// resolving to the same name and namespace are rejected (CF-029).
type PoolSettings struct {
	// Size is the target number of warm MicroVMs. Required.
	Size int `yaml:"size"`
	// Name and Namespace identify the Pool at the Pool Manager (CF-028).
	Name      string `yaml:"name,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`
	// Strategy is the replenishment strategy (PL-013).
	Strategy ReplenishmentStrategy `yaml:"strategy,omitempty"`
	// MinSize is the threshold for min_size_threshold.
	MinSize *int `yaml:"min_size,omitempty"`
	// CreateHooks run once per MicroVM after boot; PreLeaseHooks run before
	// each claim is handed out.
	CreateHooks   []string `yaml:"create_hooks,omitempty"`
	PreLeaseHooks []string `yaml:"pre_lease_hooks,omitempty"`
	// HookFailurePolicy decides what happens to a MicroVM whose hook fails.
	HookFailurePolicy HookFailurePolicy `yaml:"hook_failure_policy,omitempty"`
	// HeartbeatInterval and HeartbeatExpiry are the Pool's lease settings
	// (PL-010, PL-040, SC-060).
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval,omitempty"`
	HeartbeatExpiry   time.Duration `yaml:"heartbeat_expiry,omitempty"`
}

// Inventory is the Inventory section (CF-003). Exactly one of File and Hosts
// is set; when File is set the referenced file holds a `hosts:` list of
// HostEntry.
type Inventory struct {
	// File is the path of a separate Inventory file, as written by the Fleet
	// Controller (FL-060).
	File string `yaml:"file,omitempty"`
	// Hosts is the inline Inventory.
	Hosts []HostEntry `yaml:"hosts,omitempty"`
}

// HostEntry is one Inventory entry (CF-030). Names and endpoints are unique
// (CF-031); the name is the one the Pool Manager uses in flintlock_hosts
// (CF-032, FL-052), because the Runner joins Placement to a Host by name.
type HostEntry struct {
	// Name is the Host name shared with the Pool Manager.
	Name string `yaml:"name"`
	// Endpoint is the flintlockd gRPC address, host:port. It is what the
	// `host.address` of a claim response is checked against (SC-030).
	Endpoint string `yaml:"endpoint"`
	// Arch is the Host architecture.
	Arch Architecture `yaml:"arch"`
	// VCPU and MemoryMB are the capacity after the Host reserve (FL-061).
	VCPU     int `yaml:"vcpu"`
	MemoryMB int `yaml:"memory_mb"`
	// Labels are matched by Profile Host selectors (CF-024).
	Labels map[string]string `yaml:"labels,omitempty"`
	// Token is the flintlockd basic auth token (HO-004). Environment
	// override: FLINTLOCK_RUNNER_HOST_TOKEN_<NAME> with the name upper-cased
	// and non-alphanumerics replaced by underscores, then the fleet-wide
	// FLINTLOCK_RUNNER_HOST_TOKEN.
	Token Secret `yaml:"token,omitempty"`
	// TLS is the client TLS material for this Host (HO-005, HO-006).
	TLS ClientTLS `yaml:"tls,omitempty"`
	// Services holds the Host Service addresses (FL-110, EX-060).
	Services HostServiceAddresses `yaml:"services,omitempty"`
	// Versions records what the Fleet Controller installed (FL-029).
	Versions InstalledVersions `yaml:"versions,omitempty"`
}

// HostServiceAddresses are the addresses of a Host's Host Services as
// reachable from guests on that Host (FL-101, FL-110). An empty address means
// the service is not available on that Host and its variables are omitted
// (EX-062).
type HostServiceAddresses struct {
	// Buildkit is the value injected as BUILDKIT_HOST.
	Buildkit string `yaml:"buildkit,omitempty"`
	// GoProxy is the value injected as GOPROXY.
	GoProxy string `yaml:"go_proxy,omitempty"`
	// RegistryMirror is the value injected as CI_REGISTRY_MIRROR.
	RegistryMirror string `yaml:"registry_mirror,omitempty"`
	// HTTPCache maps an HTTP cache upstream name to its base URL; the
	// upstream's configured environment variable receives it (CF-074).
	HTTPCache map[string]string `yaml:"http_cache,omitempty"`
}

// InstalledVersions are the component versions found on a Host (FL-029).
type InstalledVersions struct {
	Flintlock       string `yaml:"flintlock,omitempty"`
	Firecracker     string `yaml:"firecracker,omitempty"`
	CloudHypervisor string `yaml:"cloud_hypervisor,omitempty"`
	Containerd      string `yaml:"containerd,omitempty"`
}

// PoolManager is the Pool Manager section (CF-040). The endpoint is required
// (CF-041).
type PoolManager struct {
	// Endpoint is the battery gRPC address, host:port.
	Endpoint string `yaml:"endpoint"`
	// TLS is the client TLS material (PL-003).
	TLS ClientTLS `yaml:"tls,omitempty"`
	// Deadline is applied to every unary call (PL-002).
	Deadline time.Duration `yaml:"deadline"`
	// HealthBackoff is how long the Pool Manager stays marked unhealthy
	// after an UNAVAILABLE claim (PL-034).
	HealthBackoff time.Duration `yaml:"health_backoff"`
	// HealthInterval is the ListPools probe interval and
	// HealthFailureThreshold the consecutive failures before unhealthy
	// (PL-005).
	HealthInterval         time.Duration `yaml:"health_interval"`
	HealthFailureThreshold int           `yaml:"health_failure_threshold"`
	// EventsPollInterval is the GetPool poll interval while the Events
	// stream is down (PL-051).
	EventsPollInterval time.Duration `yaml:"events_poll_interval"`
	// DeclareRetryInterval is the retry interval for a failed Pool
	// declaration (PL-016).
	DeclareRetryInterval time.Duration `yaml:"declare_retry_interval"`
	// ReleaseRetryLimit bounds ReleaseVM retries (PL-043, SC-051).
	ReleaseRetryLimit int `yaml:"release_retry_limit"`
}

// Scheduler is the Scheduler section (CF-050).
type Scheduler struct {
	// Namespace is the Runner namespace used for Pools and MicroVM templates
	// (PL-017, PL-021). Defaults to the runner name (CF-051).
	Namespace string `yaml:"namespace,omitempty"`
	// AllocationTimeout bounds a claim with backoff (SC-021, SC-022).
	AllocationTimeout time.Duration `yaml:"allocation_timeout"`
	// HostHealthInterval and HostUnhealthyThreshold drive Host probing
	// (SC-040, SC-041).
	HostHealthInterval     time.Duration `yaml:"host_health_interval"`
	HostUnhealthyThreshold int           `yaml:"host_unhealthy_threshold"`
	// HostCallDeadline is applied to every unary flintlock call (HO-003).
	HostCallDeadline time.Duration `yaml:"host_call_deadline"`
	// KeepOnFailure retains the MicroVM of a failed Job for KeepDuration
	// (SC-054, EX-033).
	KeepOnFailure bool          `yaml:"keep_on_failure,omitempty"`
	KeepDuration  time.Duration `yaml:"keep_duration,omitempty"`
	// AllowDefaultForUnknownImages lets a Job Image that matches no Profile
	// fall back to the Default Profile (SC-012). Default false.
	AllowDefaultForUnknownImages bool `yaml:"allow_default_for_unknown_images,omitempty"`
}

// Executor holds the Executor and Guest Transport timeouts.
type Executor struct {
	// PrepareTimeout bounds Prepare as a whole (EX-017).
	PrepareTimeout time.Duration `yaml:"prepare_timeout"`
	// GracefulKillTimeout bounds Run after the Job context is cancelled
	// (EX-024).
	GracefulKillTimeout time.Duration `yaml:"graceful_kill_timeout"`
	// TransportDeadline bounds an in-flight Guest Transport operation once
	// the Host stops answering (EX-051).
	TransportDeadline time.Duration `yaml:"transport_deadline"`
}

// RemoteMode selects the Fleet Controller's remote execution channel
// (FL-010, FL-011).
type RemoteMode string

// Remote execution modes. SSM is the default.
const (
	RemoteSSM RemoteMode = "ssm"
	RemoteSSH RemoteMode = "ssh"
)

// Fleet is the Fleet section (CF-060, CF-061).
type Fleet struct {
	// Region is the AWS region.
	Region string `yaml:"region"`
	// Discovery selects how instances are found.
	Discovery Discovery `yaml:"discovery"`
	// Remote selects the remote execution mode and its SSH settings.
	Remote Remote `yaml:"remote"`
	// Parallelism bounds concurrent provisioning (FL-012).
	Parallelism int `yaml:"parallelism"`
	// Versions pins every installed component (FL-020, FL-050, FL-051).
	Versions PinnedVersions `yaml:"versions"`
	// ThinPoolDevice is the block device for the containerd thin pool
	// (FL-022, FL-023).
	ThinPoolDevice string `yaml:"thin_pool_device"`
	// GuestSubnet is the host-local guest bridge subnet in CIDR form (FL-040).
	GuestSubnet string `yaml:"guest_subnet"`
	// Flintlockd configures the daemon's listener and auth (FL-024 to FL-026).
	Flintlockd Flintlockd `yaml:"flintlockd"`
	// HostReserve is subtracted from instance capacity (FL-061).
	HostReserve HostReserve `yaml:"host_reserve"`
	// InventoryPath and RunnerConfigPath are where the generated files are
	// written (FL-060, FL-062).
	InventoryPath    string `yaml:"inventory_path"`
	RunnerConfigPath string `yaml:"runner_config_path"`
	// EndpointOverrides maps an instance id to the Host address to use
	// instead of the private IP (FL-005).
	EndpointOverrides map[string]string `yaml:"endpoint_overrides,omitempty"`
	// LaunchTemplate enables launch template mode (CF-061, FL-090).
	LaunchTemplate *LaunchTemplate `yaml:"launch_template,omitempty"`
	// EgressAllowList restricts guest outbound traffic when set (SE-032).
	EgressAllowList []string `yaml:"egress_allow_list,omitempty"`
	// DrainTimeout and VerificationTimeout bound drain (FL-080) and verify
	// (FL-072).
	DrainTimeout        time.Duration `yaml:"drain_timeout"`
	VerificationTimeout time.Duration `yaml:"verification_timeout"`
}

// Discovery selects how the Fleet Controller finds instances. Exactly one of
// tag discovery (FL-001), explicit InstanceIDs (FL-002) or Static (TD-044)
// is used; Static takes precedence, then InstanceIDs, then the tag.
type Discovery struct {
	TagKey      string   `yaml:"tag_key,omitempty"`
	TagValue    string   `yaml:"tag_value,omitempty"`
	InstanceIDs []string `yaml:"instance_ids,omitempty"`
	// Static lists machines directly, for provisioning over SSH without an
	// AWS account (TD-044).
	Static []StaticHost `yaml:"static,omitempty"`
}

// StaticHost is a machine listed for the static discovery provider (TD-044).
type StaticHost struct {
	Name     string            `yaml:"name"`
	Address  string            `yaml:"address"`
	Arch     Architecture      `yaml:"arch"`
	VCPU     int               `yaml:"vcpu"`
	MemoryMB int               `yaml:"memory_mb"`
	Labels   map[string]string `yaml:"labels,omitempty"`
}

// Remote configures remote execution (FL-010, FL-011).
type Remote struct {
	// Mode is ssm (default) or ssh.
	Mode RemoteMode `yaml:"mode"`
	// SSH is read only when Mode is ssh.
	SSH SSHRemote `yaml:"ssh,omitempty"`
}

// SSHRemote holds the SSH settings for remote execution (FL-011).
type SSHRemote struct {
	User    string `yaml:"user"`
	KeyFile string `yaml:"key_file"`
	Port    int    `yaml:"port,omitempty"`
}

// PinnedVersions are the component versions the Fleet Controller installs
// (CF-060). FL-021 chooses flintlock-provision or provision.sh from the
// flintlock version; FL-030 compares these against what is installed.
type PinnedVersions struct {
	Flintlock       string `yaml:"flintlock"`
	Firecracker     string `yaml:"firecracker"`
	CloudHypervisor string `yaml:"cloud_hypervisor"`
	Containerd      string `yaml:"containerd"`
	PoolManager     string `yaml:"pool_manager"`
}

// Flintlockd configures the daemon on every Host (FL-024 to FL-026).
type Flintlockd struct {
	// Port is the gRPC listen port on the private address.
	Port int `yaml:"port"`
	// Token is the basic auth token required of clients. Environment
	// override: FLINTLOCK_RUNNER_FLEET_HOST_TOKEN. In launch template mode
	// it comes from a Systems Manager parameter instead (FL-091).
	Token Secret `yaml:"token,omitempty"`
	// TLS names the CA, certificate and key to install; when empty the Fleet
	// Controller generates them (SE-023).
	TLS ServerTLSFiles `yaml:"tls,omitempty"`
	// Insecure starts flintlockd without TLS. Explicit opt-in only (FL-026).
	Insecure bool `yaml:"insecure,omitempty"`
}

// ServerTLSFiles are paths on the Control Node to TLS material to install on
// Hosts (FL-024, SE-023).
type ServerTLSFiles struct {
	CAFile   string `yaml:"ca_file,omitempty"`
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

// HostReserve is the capacity kept back for the Host itself (FL-061).
type HostReserve struct {
	VCPU     int `yaml:"vcpu"`
	MemoryMB int `yaml:"memory_mb"`
}

// LaunchTemplate configures launch template mode (CF-061, FL-090 to FL-092).
type LaunchTemplate struct {
	// Parameters names the Systems Manager parameters holding secrets, read
	// by the emitted script at first boot rather than embedded (FL-091,
	// SE-014).
	Parameters LaunchTemplateParameters `yaml:"parameters"`
	// InventoryRefreshInterval is how often the Runner re-discovers Hosts by
	// tag (FL-092, SE-041). Zero disables refresh.
	InventoryRefreshInterval time.Duration `yaml:"inventory_refresh_interval,omitempty"`
}

// LaunchTemplateParameters are Systems Manager parameter names (FL-091).
type LaunchTemplateParameters struct {
	HostToken string `yaml:"host_token"`
	TLSCA     string `yaml:"tls_ca"`
	TLSCert   string `yaml:"tls_cert"`
	TLSKey    string `yaml:"tls_key"`
}

// HostServices is the Host services section (CF-070 to CF-075).
type HostServices struct {
	Buildkit       Buildkit       `yaml:"buildkit"`
	GoProxy        GoProxy        `yaml:"go_proxy"`
	RegistryMirror RegistryMirror `yaml:"registry_mirror"`
	HTTPCache      HTTPCache      `yaml:"http_cache"`
	// CacheVolume is the shared storage for every Host Service (CF-075,
	// FL-107).
	CacheVolume CacheVolume `yaml:"cache_volume"`
}

// Service is the common part of every Host Service entry (CF-070).
type Service struct {
	// Enabled defaults to true; nil means unset. A disabled service is not
	// installed and its port stays closed (FL-111, EX-062).
	Enabled *bool `yaml:"enabled,omitempty"`
	// Port is the listen port on the guest bridge gateway (FL-101, FL-108).
	Port int `yaml:"port"`
}

// Buildkit configures buildkitd (CF-071, FL-102, FL-103).
type Buildkit struct {
	Service `yaml:",inline"`
	// StorageLimit caps the buildkitd cache.
	StorageLimit ByteSize `yaml:"storage_limit,omitempty"`
	// GCPolicy is the buildkitd garbage collection policy in its own syntax.
	GCPolicy string `yaml:"gc_policy,omitempty"`
}

// GoProxy configures the Go module proxy (CF-072, CF-076, FL-105, FL-113 to
// FL-115, EX-065).
type GoProxy struct {
	Service `yaml:",inline"`
	// Upstream is the public proxy; default https://proxy.golang.org.
	Upstream string `yaml:"upstream,omitempty"`
	// PrivatePatterns are GOPRIVATE-style module path patterns served from
	// PrivateVCSHost with CredentialParameter (CF-076 requires the parameter
	// when patterns are set).
	PrivatePatterns     []string `yaml:"private_patterns,omitempty"`
	PrivateVCSHost      string   `yaml:"private_vcs_host,omitempty"`
	CredentialParameter string   `yaml:"credential_parameter,omitempty"`
	// PrivateRevalidateInterval is how long private modules are served from
	// cache before the VCS host is consulted again (FL-115).
	PrivateRevalidateInterval time.Duration `yaml:"private_revalidate_interval,omitempty"`
	// StorageLimit caps the module store.
	StorageLimit ByteSize `yaml:"storage_limit,omitempty"`
	// Prewarm lists module@version strings fetched after provisioning
	// (FL-112).
	Prewarm []string `yaml:"prewarm,omitempty"`
}

// RegistryMirror configures the pull-through registry mirror (CF-073,
// FL-104).
type RegistryMirror struct {
	Service `yaml:",inline"`
	// Upstreams are the mirrored registries.
	Upstreams []RegistryUpstream `yaml:"upstreams,omitempty"`
	// StorageLimit caps the mirror store.
	StorageLimit ByteSize `yaml:"storage_limit,omitempty"`
	// Prewarm lists image references pulled after provisioning (FL-112).
	Prewarm []string `yaml:"prewarm,omitempty"`
}

// RegistryUpstream is one mirrored registry (CF-073). Credentials stay on
// the Host side (SE-052).
type RegistryUpstream struct {
	URL                 string `yaml:"url"`
	CredentialParameter string `yaml:"credential_parameter,omitempty"`
}

// HTTPCache configures the generic HTTP cache (CF-074, FL-106).
type HTTPCache struct {
	Service   `yaml:",inline"`
	Upstreams []HTTPCacheUpstream `yaml:"upstreams,omitempty"`
}

// HTTPCacheUpstream is one cached upstream (CF-074). The Executor injects
// the upstream's URL on the Host under EnvVar (EX-060).
type HTTPCacheUpstream struct {
	Name      string        `yaml:"name"`
	URL       string        `yaml:"url"`
	SizeLimit ByteSize      `yaml:"size_limit,omitempty"`
	TTL       time.Duration `yaml:"ttl,omitempty"`
	EnvVar    string        `yaml:"env_var"`
}

// CacheVolume is the Host Service storage (CF-075, FL-107). Exactly one of
// Device and Directory is set.
type CacheVolume struct {
	Device    string `yaml:"device,omitempty"`
	Directory string `yaml:"directory,omitempty"`
	// SizeCap caps the total Host Service storage.
	SizeCap ByteSize `yaml:"size_cap"`
}

// DistributedCache is the Distributed cache section (CF-080 to CF-082).
// Pre-signed URLs are generated on the Control Node; no AWS credential enters
// a guest (CF-082).
type DistributedCache struct {
	Bucket string `yaml:"bucket"`
	Region string `yaml:"region"`
	Prefix string `yaml:"prefix,omitempty"`
	// Endpoint is set for S3-compatible stores and for the fake object store
	// in the end-to-end harness (TD-034).
	Endpoint string `yaml:"endpoint,omitempty"`
	// Insecure allows an http Endpoint; harness use only.
	Insecure bool `yaml:"insecure,omitempty"`
}

// LogFormat is the structured log encoding (OB-001).
type LogFormat string

// Log formats.
const (
	LogJSON LogFormat = "json"
	LogText LogFormat = "text"
)

// Observability configures logs, metrics and health (OB-001, OB-010, OB-030,
// OB-031).
type Observability struct {
	LogFormat LogFormat `yaml:"log_format"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `yaml:"log_level"`
	// ListenAddress serves /metrics, /healthz and /readyz.
	ListenAddress string `yaml:"listen_address"`
}
