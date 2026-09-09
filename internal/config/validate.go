package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// ErrInvalid is matched by every *ValidationError, so callers can write
// errors.Is(err, config.ErrInvalid).
var ErrInvalid = errors.New("config: invalid configuration")

// FieldError is one invalid field: its YAML path and the reason.
type FieldError struct {
	Field  string
	Reason string
}

// Error implements error.
func (e FieldError) Error() string { return e.Field + ": " + e.Reason }

// ValidationError carries every invalid field found in one pass, in schema
// order, so that an operator fixes them all at once (CF-004). Errors is
// never empty.
type ValidationError struct {
	Errors []FieldError
}

// First is the first invalid field in schema order.
func (e *ValidationError) First() FieldError { return e.Errors[0] }

// Error names the first invalid field and its reason on the first line and
// lists every other one on the lines that follow.
func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration: %s", e.Errors[0].Error())
	if len(e.Errors) > 1 {
		fmt.Fprintf(&b, " (and %d more)", len(e.Errors)-1)
		for _, fe := range e.Errors[1:] {
			b.WriteString("\n  ")
			b.WriteString(fe.Error())
		}
	}
	return b.String()
}

// Is reports ErrInvalid so errors.Is works without a type assertion.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalid }

// validator accumulates FieldErrors.
type validator struct {
	errs []FieldError
}

func (v *validator) errorf(field, format string, args ...any) {
	v.errs = append(v.errs, FieldError{Field: field, Reason: fmt.Sprintf(format, args...)})
}

func (v *validator) required(field, value string) bool {
	if strings.TrimSpace(value) == "" {
		v.errorf(field, "is required")
		return false
	}
	return true
}

func (v *validator) positive(field string, d time.Duration) {
	if d <= 0 {
		v.errorf(field, "must be a positive duration, got %s", d)
	}
}

func (v *validator) nonNegative(field string, d time.Duration) {
	if d < 0 {
		v.errorf(field, "must not be negative, got %s", d)
	}
}

func (v *validator) positiveInt(field string, n int) {
	if n <= 0 {
		v.errorf(field, "must be positive, got %d", n)
	}
}

func (v *validator) nonNegativeInt(field string, n int) {
	if n < 0 {
		v.errorf(field, "must not be negative, got %d", n)
	}
}

func (v *validator) port(field string, p int) {
	if p < 1 || p > 65535 {
		v.errorf(field, "must be a port between 1 and 65535, got %d", p)
	}
}

// hostPort checks a host:port address without a scheme.
func (v *validator) hostPort(field, addr string) {
	if !v.required(field, addr) {
		return
	}
	if strings.Contains(addr, "://") {
		v.errorf(field, "must be host:port without a scheme, got %q", addr)
		return
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		v.errorf(field, "must be host:port, got %q", addr)
	}
}

// httpURL checks an absolute http or https URL.
func (v *validator) httpURL(field, raw string) *url.URL {
	if !v.required(field, raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		v.errorf(field, "must be an absolute http or https URL, got %q", raw)
		return nil
	}
	return u
}

func (v *validator) clientTLS(field string, t ClientTLS) {
	if (t.CertFile == "") != (t.KeyFile == "") {
		v.errorf(field, "cert_file and key_file have to be set together")
	}
	if t.Insecure && (t.CAFile != "" || t.CertFile != "") {
		v.errorf(field, "insecure cannot be combined with TLS material")
	}
}

func (v *validator) arch(field string, a Architecture) {
	switch a {
	case ArchAMD64, ArchARM64:
	case "":
		v.errorf(field, "is required (amd64 or arm64)")
	default:
		v.errorf(field, "must be amd64 or arm64, got %q", a)
	}
}

func (v *validator) absPath(field, p string) {
	if !v.required(field, p) {
		return
	}
	if !strings.HasPrefix(p, "/") {
		v.errorf(field, "must be an absolute path, got %q", p)
	}
}

//= docs/requirements/07-configuration.md#file-and-precedence
//# When starting, the Runner SHALL validate the configuration and,
//# if it is invalid, SHALL exit with a non-zero status and a message naming
//# the first invalid field and the reason.

// Validate checks c against every rule of 07-configuration.md and returns a
// *ValidationError listing every violation in schema order, or nil. It
// expects ApplyDefaults to have run; a field with a default is still checked
// so that a Config built as a literal is validated the same way. Validate
// never modifies c.
func Validate(c *Config) error {
	v := &validator{}
	v.gitlab(&c.GitLab)
	v.profiles(c)
	v.inventory(c)
	v.poolManager(&c.PoolManager)
	v.scheduler(&c.Scheduler)
	v.executor(&c.Executor)
	v.fleet(c.Fleet)
	v.hostServices(&c.HostServices)
	v.distributedCache(c.DistributedCache)
	v.observability(&c.Observability)
	v.absPath("state_dir", c.StateDir)
	if len(v.errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: v.errs}
}

//= docs/requirements/07-configuration.md#gitlab-section
//# The GitLab section SHALL contain the GitLab URL, the runner
//# authentication token, the runner name, the concurrency limit, the check
//# interval, the output limit, the shutdown timeout and optional TLS
//# certificate authority, client certificate and client key paths.

func (v *validator) gitlab(g *GitLab) {
	if u := v.httpURL("gitlab.url", g.URL); u != nil && u.Scheme != "https" && !g.AllowInsecure {
		v.errorf("gitlab.url", "must use https unless gitlab.allow_insecure is true (SE-020)")
	}
	v.required("gitlab.token", string(g.Token))
	v.required("gitlab.name", g.Name)
	v.positiveInt("gitlab.concurrent", g.Concurrent)
	v.positive("gitlab.check_interval", g.CheckInterval)
	v.positiveInt("gitlab.output_limit_kb", g.OutputLimitKB)
	v.positive("gitlab.shutdown_timeout", g.ShutdownTimeout)
	v.clientTLS("gitlab.tls", g.TLS)
	if g.TLS.Insecure {
		v.errorf("gitlab.tls.insecure", "does not apply to GitLab; use gitlab.allow_insecure for an http URL")
	}
}

//= docs/requirements/07-configuration.md#profiles-section
//# A Profile SHALL have a unique name, an architecture, a vCPU
//# count, a memory size in megabytes, a kernel image, a root filesystem image,
//# a shell path, a builds directory, a cache directory, a helper binary path
//# and pool settings.

func (v *validator) profiles(c *Config) {
	if len(c.Profiles) == 0 {
		v.errorf("profiles", "at least one Profile is required")
		return
	}
	names := map[string]int{}
	images := map[string]int{}
	pools := map[string]int{}
	defaults := 0
	for i := range c.Profiles {
		p := &c.Profiles[i]
		f := fmt.Sprintf("profiles[%d]", i)
		if v.required(f+".name", p.Name) {
			if j, dup := names[p.Name]; dup {
				v.errorf(f+".name", "duplicates profiles[%d].name %q", j, p.Name)
			} else {
				names[p.Name] = i
			}
		}
		v.arch(f+".arch", p.Arch)
		v.positiveInt(f+".vcpu", p.VCPU)
		v.positiveInt(f+".memory_mb", p.MemoryMB)
		v.profileImages(f, p)
		v.absPath(f+".shell", p.Shell)
		v.absPath(f+".builds_dir", p.BuildsDir)
		v.absPath(f+".cache_dir", p.CacheDir)
		v.absPath(f+".helper_path", p.HelperPath)
		v.profileOptional(f, p, images, i)
		if p.Default {
			defaults++
		}
		v.poolSettings(f+".pool", &p.Pool)
		//= docs/requirements/07-configuration.md#profiles-section
		//# If two Profiles resolve to the same Pool name and namespace,
		//# then the Runner SHALL reject the configuration.
		//
		// The key is the resolved pair, so a collision reached through the
		// defaults of CF-028 is caught as well as an explicit one.
		key := p.Pool.Namespace + "/" + p.Pool.Name
		if j, dup := pools[key]; dup {
			v.errorf(f+".pool", "resolves to pool %q in namespace %q, the same as profiles[%d]", p.Pool.Name, p.Pool.Namespace, j)
		} else {
			pools[key] = i
		}
		if p.Arch != "" && len(c.Inventory.Hosts) > 0 && len(SelectHosts(p, c.Inventory.Hosts)) == 0 {
			v.errorf(f+".host_selector", "matches no Inventory Host with arch %s and labels %v", p.Arch, p.HostSelector)
		}
	}
	//= docs/requirements/07-configuration.md#profiles-section
	//# Exactly one Profile MAY be marked as the Default Profile.
	if defaults > 1 {
		v.errorf("profiles", "%d Profiles are marked default; at most one may be", defaults)
	}
}

//= docs/requirements/07-configuration.md#profiles-section
//# The Runner SHALL reject a Profile whose images are specified by
//# neither a digest nor a tag.

func (v *validator) profileImages(f string, p *Profile) {
	for _, img := range p.images(f) {
		if !v.required(img.field, img.ref) {
			continue
		}
		ref, err := ParseImageRef(img.ref)
		if err != nil {
			v.errorf(img.field, "%v", err)
			continue
		}
		if !ref.Pinned() {
			v.errorf(img.field, "image %q is specified by neither a digest nor a tag", img.ref)
		}
	}
	for i, vol := range p.AdditionalVolumes {
		vf := fmt.Sprintf("%s.additional_volumes[%d]", f, i)
		v.required(vf+".id", vol.ID)
		if vol.MountPoint != "" {
			v.absPath(vf+".mount_point", vol.MountPoint)
		}
		v.nonNegativeInt(vf+".size_mb", vol.SizeMB)
	}
}

//= docs/requirements/07-configuration.md#profiles-section
//# A Profile MAY declare image names and image glob patterns that
//# map Job Images onto it, a kernel command line, an initrd image, additional
//# volumes, a hypervisor provider, cloud-init user-data, a Guest Transport, a
//# user, a ready timeout, a Host selector and a maximum concurrency.

func (v *validator) profileOptional(f string, p *Profile, images map[string]int, i int) {
	//= docs/requirements/07-configuration.md#profiles-section
	//# If two Profiles declare the same image name, then the Runner
	//# SHALL reject the configuration.
	for k, name := range p.Images {
		nf := fmt.Sprintf("%s.images[%d]", f, k)
		if !v.required(nf, name) {
			continue
		}
		if j, dup := images[name]; dup {
			v.errorf(nf, "image name %q is also declared by profiles[%d]", name, j)
		} else {
			images[name] = i
		}
	}
	for k, glob := range p.ImageGlobs {
		gf := fmt.Sprintf("%s.image_globs[%d]", f, k)
		if !v.required(gf, glob) {
			continue
		}
		if _, err := path.Match(glob, ""); err != nil {
			v.errorf(gf, "bad glob pattern %q: %v", glob, err)
		}
	}
	for k := range p.Kernel.Cmdline {
		if strings.TrimSpace(k) == "" {
			v.errorf(f+".kernel.cmdline", "keys must not be empty")
		}
	}
	switch p.Transport.Kind {
	case TransportExec:
	case TransportSSH:
		v.required(f+".transport.ssh.private_key_file", p.Transport.SSH.PrivateKeyFile)
		v.required(f+".transport.ssh.user", p.Transport.SSH.User)
	case "":
		v.errorf(f+".transport.kind", "is required (exec or ssh)")
	default:
		v.errorf(f+".transport.kind", "must be exec or ssh, got %q", p.Transport.Kind)
	}
	v.required(f+".user", p.User)
	v.positive(f+".ready_timeout", p.ReadyTimeout)
	for k := range p.HostSelector {
		if strings.TrimSpace(k) == "" {
			v.errorf(f+".host_selector", "label keys must not be empty")
		}
	}
	v.nonNegativeInt(f+".max_concurrency", p.MaxConcurrency)
}

//= docs/requirements/07-configuration.md#profiles-section
//# A Profile's pool settings SHALL contain the Pool size and MAY
//# contain the Pool name and namespace, replenishment strategy, minimum size,
//# create hooks, pre-lease hooks, hook failure policy, heartbeat interval and
//# heartbeat expiry threshold.

func (v *validator) poolSettings(f string, ps *PoolSettings) {
	v.positiveInt(f+".size", ps.Size)
	v.required(f+".name", ps.Name)
	v.required(f+".namespace", ps.Namespace)
	switch ps.Strategy {
	case ReplenishImmediateOnLease, ReplenishReplaceOnDelete:
	case ReplenishMinSizeThreshold:
		if ps.MinSize == nil {
			v.errorf(f+".min_size", "is required for the %s strategy", ReplenishMinSizeThreshold)
		}
	case "":
		v.errorf(f+".strategy", "is required")
	default:
		v.errorf(f+".strategy", "must be one of %s, %s, %s; got %q",
			ReplenishImmediateOnLease, ReplenishMinSizeThreshold, ReplenishReplaceOnDelete, ps.Strategy)
	}
	if ps.MinSize != nil {
		if *ps.MinSize < 0 {
			v.errorf(f+".min_size", "must not be negative, got %d", *ps.MinSize)
		} else if *ps.MinSize > ps.Size {
			v.errorf(f+".min_size", "must not exceed size %d, got %d", ps.Size, *ps.MinSize)
		}
	}
	for i, h := range ps.CreateHooks {
		v.required(fmt.Sprintf("%s.create_hooks[%d]", f, i), h)
	}
	for i, h := range ps.PreLeaseHooks {
		v.required(fmt.Sprintf("%s.pre_lease_hooks[%d]", f, i), h)
	}
	switch ps.HookFailurePolicy {
	case "", HookFailureDeleteAndReplace, HookFailureQuarantine:
	default:
		v.errorf(f+".hook_failure_policy", "must be %s or %s, got %q",
			HookFailureDeleteAndReplace, HookFailureQuarantine, ps.HookFailurePolicy)
	}
	v.positive(f+".heartbeat_interval", ps.HeartbeatInterval)
	v.positive(f+".heartbeat_expiry", ps.HeartbeatExpiry)
	if ps.HeartbeatInterval > 0 && ps.HeartbeatExpiry > 0 && ps.HeartbeatExpiry <= ps.HeartbeatInterval {
		v.errorf(f+".heartbeat_expiry", "must be longer than heartbeat_interval %s, got %s", ps.HeartbeatInterval, ps.HeartbeatExpiry)
	}
}

//= docs/requirements/07-configuration.md#inventory-section
//# An Inventory entry SHALL contain a Host name, a `flintlockd`
//# gRPC endpoint, an architecture, a vCPU capacity and a memory capacity, and
//# MAY contain labels, a basic auth token, TLS settings, Host Service
//# addresses and installed version information.

func (v *validator) inventory(c *Config) {
	hosts := c.Inventory.Hosts
	if len(hosts) == 0 {
		v.errorf("inventory", "at least one Host is required, inline under inventory.hosts or in the file named by inventory.file")
		return
	}
	upstreams := map[string]bool{}
	for _, u := range c.HostServices.HTTPCache.Upstreams {
		upstreams[u.Name] = true
	}
	names := map[string]int{}
	endpoints := map[string]int{}
	for i := range hosts {
		h := &hosts[i]
		f := fmt.Sprintf("inventory.hosts[%d]", i)
		if v.required(f+".name", h.Name) {
			if strings.ContainsAny(h.Name, " \t\n/") {
				v.errorf(f+".name", "must be the Host name the Pool Manager uses in flintlock_hosts, without whitespace or slashes; got %q", h.Name)
			}
			//= docs/requirements/07-configuration.md#inventory-section
			//# The Runner SHALL reject an Inventory with duplicate Host names or
			//# duplicate endpoints.
			if j, dup := names[h.Name]; dup {
				v.errorf(f+".name", "duplicates inventory.hosts[%d].name %q", j, h.Name)
			} else {
				names[h.Name] = i
			}
		}
		v.hostPort(f+".endpoint", h.Endpoint)
		if h.Endpoint != "" {
			if j, dup := endpoints[h.Endpoint]; dup {
				v.errorf(f+".endpoint", "duplicates inventory.hosts[%d].endpoint %q", j, h.Endpoint)
			} else {
				endpoints[h.Endpoint] = i
			}
		}
		v.arch(f+".arch", h.Arch)
		v.positiveInt(f+".vcpu", h.VCPU)
		v.positiveInt(f+".memory_mb", h.MemoryMB)
		for k := range h.Labels {
			if strings.TrimSpace(k) == "" {
				v.errorf(f+".labels", "label keys must not be empty")
			}
		}
		v.clientTLS(f+".tls", h.TLS)
		for name := range h.Services.HTTPCache {
			if !upstreams[name] {
				v.errorf(f+".services.http_cache", "names HTTP cache upstream %q which host_services.http_cache.upstreams does not declare", name)
			}
		}
	}
}

//= docs/requirements/07-configuration.md#pool-manager-section
//# The Pool Manager section SHALL contain an endpoint, TLS
//# settings, a request deadline, a health backoff period, a health probe
//# interval, an events poll interval, a pool declaration retry interval and a
//# release retry limit.

func (v *validator) poolManager(pm *PoolManager) {
	//= docs/requirements/07-configuration.md#pool-manager-section
	//# If the Pool Manager section is absent or has no endpoint, then the
	//# Runner SHALL reject the configuration.
	if strings.TrimSpace(pm.Endpoint) == "" {
		v.errorf("pool_manager.endpoint", "is required; the pool_manager section has to name the battery endpoint")
	} else {
		v.hostPort("pool_manager.endpoint", pm.Endpoint)
	}
	v.clientTLS("pool_manager.tls", pm.TLS)
	v.positive("pool_manager.deadline", pm.Deadline)
	v.positive("pool_manager.health_backoff", pm.HealthBackoff)
	v.positive("pool_manager.health_interval", pm.HealthInterval)
	v.positiveInt("pool_manager.health_failure_threshold", pm.HealthFailureThreshold)
	v.positive("pool_manager.events_poll_interval", pm.EventsPollInterval)
	v.positive("pool_manager.declare_retry_interval", pm.DeclareRetryInterval)
	v.nonNegativeInt("pool_manager.release_retry_limit", pm.ReleaseRetryLimit)
}

//= docs/requirements/07-configuration.md#scheduler-section
//# The Scheduler section SHALL contain the Runner namespace, the
//# allocation timeout, the Host health probe interval, the unhealthy probe
//# threshold, the keep-on-failure flag and the keep duration.

func (v *validator) scheduler(s *Scheduler) {
	if v.required("scheduler.namespace", s.Namespace) && strings.ContainsAny(s.Namespace, " \t\n/") {
		v.errorf("scheduler.namespace", "must not contain whitespace or slashes, got %q", s.Namespace)
	}
	v.positive("scheduler.allocation_timeout", s.AllocationTimeout)
	v.positive("scheduler.host_health_interval", s.HostHealthInterval)
	v.positiveInt("scheduler.host_unhealthy_threshold", s.HostUnhealthyThreshold)
	v.positive("scheduler.host_call_deadline", s.HostCallDeadline)
	if s.KeepOnFailure {
		v.positive("scheduler.keep_duration", s.KeepDuration)
	} else {
		v.nonNegative("scheduler.keep_duration", s.KeepDuration)
	}
}

func (v *validator) executor(e *Executor) {
	v.positive("executor.prepare_timeout", e.PrepareTimeout)
	v.positive("executor.graceful_kill_timeout", e.GracefulKillTimeout)
	v.positive("executor.transport_deadline", e.TransportDeadline)
}

//= docs/requirements/07-configuration.md#fleet-section
//# The Fleet section SHALL contain the AWS region, the discovery
//# tag key and value or explicit instance ids, the remote execution mode and
//# its SSH settings, the provisioning parallelism, the pinned versions of
//# flintlock, Firecracker, Cloud Hypervisor, containerd and the Pool Manager,
//# the thin pool device, the guest subnet, the `flintlockd`
//# port and auth settings, the Host reserve, and the paths for the generated
//# Inventory and Runner configuration.

func (v *validator) fleet(f *Fleet) {
	if f == nil {
		return
	}
	staticOnly := v.discovery(&f.Discovery)
	if !staticOnly {
		v.required("fleet.region", f.Region)
	}
	switch f.Remote.Mode {
	case RemoteSSM:
	case RemoteSSH:
		v.required("fleet.remote.ssh.user", f.Remote.SSH.User)
		v.required("fleet.remote.ssh.key_file", f.Remote.SSH.KeyFile)
		v.port("fleet.remote.ssh.port", f.Remote.SSH.Port)
	default:
		v.errorf("fleet.remote.mode", "must be ssm or ssh, got %q", f.Remote.Mode)
	}
	if staticOnly && f.Remote.Mode == RemoteSSM {
		v.errorf("fleet.remote.mode", "static discovery has no Systems Manager agent; set ssh")
	}
	v.positiveInt("fleet.parallelism", f.Parallelism)
	v.required("fleet.versions.flintlock", f.Versions.Flintlock)
	if f.Versions.Firecracker == "" && f.Versions.CloudHypervisor == "" {
		v.errorf("fleet.versions", "at least one of firecracker and cloud_hypervisor has to be pinned")
	}
	v.required("fleet.versions.containerd", f.Versions.Containerd)
	v.required("fleet.versions.pool_manager", f.Versions.PoolManager)
	v.absPath("fleet.thin_pool_device", f.ThinPoolDevice)
	if v.required("fleet.guest_subnet", f.GuestSubnet) {
		if _, _, err := net.ParseCIDR(f.GuestSubnet); err != nil {
			v.errorf("fleet.guest_subnet", "must be a CIDR such as 172.31.0.0/16, got %q", f.GuestSubnet)
		}
	}
	v.port("fleet.flintlockd.port", f.Flintlockd.Port)
	tls := f.Flintlockd.TLS
	if (tls.CertFile == "") != (tls.KeyFile == "") || (tls.CAFile == "") != (tls.CertFile == "") {
		v.errorf("fleet.flintlockd.tls", "ca_file, cert_file and key_file have to be set together or all left empty to generate them")
	}
	if f.Flintlockd.Insecure && tls.CAFile != "" {
		v.errorf("fleet.flintlockd.insecure", "cannot be combined with TLS material")
	}
	v.nonNegativeInt("fleet.host_reserve.vcpu", f.HostReserve.VCPU)
	v.nonNegativeInt("fleet.host_reserve.memory_mb", f.HostReserve.MemoryMB)
	v.absPath("fleet.inventory_path", f.InventoryPath)
	v.absPath("fleet.runner_config_path", f.RunnerConfigPath)
	for id, addr := range f.EndpointOverrides {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(addr) == "" {
			v.errorf("fleet.endpoint_overrides", "instance ids and addresses must not be empty")
			break
		}
	}
	v.launchTemplate(f)
	for i, e := range f.EgressAllowList {
		v.required(fmt.Sprintf("fleet.egress_allow_list[%d]", i), e)
	}
	v.positive("fleet.drain_timeout", f.DrainTimeout)
	v.positive("fleet.verification_timeout", f.VerificationTimeout)
}

// discovery validates the discovery selection and reports whether only the
// static provider is configured, in which case no AWS settings are needed.
func (v *validator) discovery(d *Discovery) (staticOnly bool) {
	hasTag := d.TagKey != "" || d.TagValue != ""
	if hasTag && (d.TagKey == "" || d.TagValue == "") {
		v.errorf("fleet.discovery", "tag_key and tag_value have to be set together")
	}
	for i, id := range d.InstanceIDs {
		v.required(fmt.Sprintf("fleet.discovery.instance_ids[%d]", i), id)
	}
	names := map[string]int{}
	for i := range d.Static {
		s := &d.Static[i]
		f := fmt.Sprintf("fleet.discovery.static[%d]", i)
		if v.required(f+".name", s.Name) {
			if j, dup := names[s.Name]; dup {
				v.errorf(f+".name", "duplicates fleet.discovery.static[%d].name %q", j, s.Name)
			} else {
				names[s.Name] = i
			}
		}
		v.required(f+".address", s.Address)
		v.arch(f+".arch", s.Arch)
		v.positiveInt(f+".vcpu", s.VCPU)
		v.positiveInt(f+".memory_mb", s.MemoryMB)
	}
	if !hasTag && len(d.InstanceIDs) == 0 && len(d.Static) == 0 {
		v.errorf("fleet.discovery", "one of tag_key/tag_value, instance_ids or static is required")
	}
	return len(d.Static) > 0 && !hasTag && len(d.InstanceIDs) == 0
}

//= docs/requirements/07-configuration.md#fleet-section
//# The Fleet section MAY contain launch template settings naming
//# the Systems Manager parameters that hold secrets.

func (v *validator) launchTemplate(f *Fleet) {
	lt := f.LaunchTemplate
	if lt == nil {
		return
	}
	v.required("fleet.launch_template.parameters.host_token", lt.Parameters.HostToken)
	if !f.Flintlockd.Insecure {
		v.required("fleet.launch_template.parameters.tls_ca", lt.Parameters.TLSCA)
		v.required("fleet.launch_template.parameters.tls_cert", lt.Parameters.TLSCert)
		v.required("fleet.launch_template.parameters.tls_key", lt.Parameters.TLSKey)
	}
	v.nonNegative("fleet.launch_template.inventory_refresh_interval", lt.InventoryRefreshInterval)
	if lt.InventoryRefreshInterval > 0 && f.Discovery.TagKey == "" {
		v.errorf("fleet.launch_template.inventory_refresh_interval", "needs tag discovery (fleet.discovery.tag_key and tag_value) to refresh the Inventory")
	}
}

var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

//= docs/requirements/07-configuration.md#host-services-section
//# The Host services section SHALL contain the cache volume device
//# or directory and its total size cap.

func (v *validator) hostServices(hs *HostServices) {
	ports := map[int]string{}
	for _, s := range []struct {
		name string
		svc  Service
	}{
		{"buildkit", hs.Buildkit.Service},
		{"go_proxy", hs.GoProxy.Service},
		{"registry_mirror", hs.RegistryMirror.Service},
		{"http_cache", hs.HTTPCache.Service},
	} {
		f := "host_services." + s.name + ".port"
		v.port(f, s.svc.Port)
		if !s.svc.IsEnabled() {
			continue
		}
		if other, dup := ports[s.svc.Port]; dup {
			v.errorf(f, "port %d is also used by host_services.%s", s.svc.Port, other)
		} else {
			ports[s.svc.Port] = s.name
		}
	}
	v.buildkit(&hs.Buildkit)
	v.goProxy(&hs.GoProxy)
	v.registryMirror(&hs.RegistryMirror)
	v.httpCache(&hs.HTTPCache)

	cv := &hs.CacheVolume
	switch {
	case cv.Device != "" && cv.Directory != "":
		v.errorf("host_services.cache_volume", "device and directory are mutually exclusive")
	case cv.Device != "":
		v.absPath("host_services.cache_volume.device", cv.Device)
	case cv.Directory != "":
		v.absPath("host_services.cache_volume.directory", cv.Directory)
	default:
		v.errorf("host_services.cache_volume", "one of device or directory is required")
	}
	if cv.SizeCap <= 0 {
		v.errorf("host_services.cache_volume.size_cap", "must be a positive size, got %s", cv.SizeCap)
	}
	var limits ByteSize
	if hs.Buildkit.IsEnabled() {
		limits += hs.Buildkit.StorageLimit
	}
	if hs.GoProxy.IsEnabled() {
		limits += hs.GoProxy.StorageLimit
	}
	if hs.RegistryMirror.IsEnabled() {
		limits += hs.RegistryMirror.StorageLimit
	}
	if hs.HTTPCache.IsEnabled() {
		for _, u := range hs.HTTPCache.Upstreams {
			limits += u.SizeLimit
		}
	}
	if cv.SizeCap > 0 && limits > cv.SizeCap {
		v.errorf("host_services.cache_volume.size_cap", "%s is smaller than the %s the enabled services' storage limits add up to", cv.SizeCap, limits)
	}
}

//= docs/requirements/07-configuration.md#host-services-section
//# The `buildkit` entry MAY contain a storage limit and a garbage
//# collection policy.

func (v *validator) buildkit(b *Buildkit) {
	if b.StorageLimit < 0 {
		v.errorf("host_services.buildkit.storage_limit", "must not be negative")
	}
	if b.GCPolicy != "" && !b.IsEnabled() {
		v.errorf("host_services.buildkit.gc_policy", "is set but buildkit is disabled")
	}
}

//= docs/requirements/07-configuration.md#host-services-section
//# The `go_proxy` entry MAY contain the upstream proxy, a list of
//# private module patterns, the version control host those patterns resolve
//# to, the Systems Manager parameter holding the read-only credential for
//# them, a private module revalidation interval, a storage limit and a list of
//# modules to pre-warm.

func (v *validator) goProxy(g *GoProxy) {
	v.httpURL("host_services.go_proxy.upstream", g.Upstream)
	for i, p := range g.PrivatePatterns {
		v.required(fmt.Sprintf("host_services.go_proxy.private_patterns[%d]", i), p)
	}
	//= docs/requirements/07-configuration.md#host-services-section
	//# If the `go_proxy` entry lists private module patterns without a
	//# credential parameter, then the Runner SHALL reject the configuration.
	if len(g.PrivatePatterns) > 0 && strings.TrimSpace(g.CredentialParameter) == "" {
		v.errorf("host_services.go_proxy.credential_parameter", "is required when private_patterns are set; name the Systems Manager parameter holding the read-only credential")
	}
	if len(g.PrivatePatterns) == 0 && g.PrivateVCSHost != "" {
		v.errorf("host_services.go_proxy.private_vcs_host", "is set without private_patterns")
	}
	v.nonNegative("host_services.go_proxy.private_revalidate_interval", g.PrivateRevalidateInterval)
	if g.StorageLimit < 0 {
		v.errorf("host_services.go_proxy.storage_limit", "must not be negative")
	}
	for i, m := range g.Prewarm {
		f := fmt.Sprintf("host_services.go_proxy.prewarm[%d]", i)
		if v.required(f, m) && !strings.Contains(m, "@") {
			v.errorf(f, "must be module@version, got %q", m)
		}
	}
}

//= docs/requirements/07-configuration.md#host-services-section
//# The `registry_mirror` entry MAY contain a list of upstream
//# registries with optional credential parameter names, a storage limit and a
//# list of images to pre-warm.

func (v *validator) registryMirror(r *RegistryMirror) {
	seen := map[string]int{}
	for i, u := range r.Upstreams {
		f := fmt.Sprintf("host_services.registry_mirror.upstreams[%d]", i)
		if v.httpURL(f+".url", u.URL) == nil {
			continue
		}
		if j, dup := seen[u.URL]; dup {
			v.errorf(f+".url", "duplicates upstreams[%d]", j)
		} else {
			seen[u.URL] = i
		}
	}
	if r.StorageLimit < 0 {
		v.errorf("host_services.registry_mirror.storage_limit", "must not be negative")
	}
	for i, img := range r.Prewarm {
		f := fmt.Sprintf("host_services.registry_mirror.prewarm[%d]", i)
		if !v.required(f, img) {
			continue
		}
		if _, err := ParseImageRef(img); err != nil {
			v.errorf(f, "%v", err)
		}
	}
}

//= docs/requirements/07-configuration.md#host-services-section
//# The `http_cache` entry MAY contain a list of upstreams, each
//# with a name, an upstream URL, a size limit, a time-to-live and the
//# environment variable name the Executor injects for it.

func (v *validator) httpCache(h *HTTPCache) {
	names := map[string]int{}
	envs := map[string]int{}
	for i, u := range h.Upstreams {
		f := fmt.Sprintf("host_services.http_cache.upstreams[%d]", i)
		if v.required(f+".name", u.Name) {
			if j, dup := names[u.Name]; dup {
				v.errorf(f+".name", "duplicates upstreams[%d].name %q", j, u.Name)
			} else {
				names[u.Name] = i
			}
		}
		v.httpURL(f+".url", u.URL)
		if u.SizeLimit < 0 {
			v.errorf(f+".size_limit", "must not be negative")
		}
		v.nonNegative(f+".ttl", u.TTL)
		if v.required(f+".env_var", u.EnvVar) {
			if !envVarName.MatchString(u.EnvVar) {
				v.errorf(f+".env_var", "must be an environment variable name, got %q", u.EnvVar)
			} else if j, dup := envs[u.EnvVar]; dup {
				v.errorf(f+".env_var", "duplicates upstreams[%d].env_var %q", j, u.EnvVar)
			} else {
				envs[u.EnvVar] = i
			}
		}
	}
}

//= docs/requirements/07-configuration.md#distributed-cache-section
//# The Distributed cache section SHALL contain an S3 bucket name,
//# region and optional prefix, and MAY contain an endpoint for S3-compatible
//# stores.

func (v *validator) distributedCache(dc *DistributedCache) {
	if dc == nil {
		return
	}
	v.required("distributed_cache.bucket", dc.Bucket)
	v.required("distributed_cache.region", dc.Region)
	if strings.HasPrefix(dc.Prefix, "/") {
		v.errorf("distributed_cache.prefix", "must not start with a slash, got %q", dc.Prefix)
	}
	if dc.Endpoint == "" {
		if dc.Insecure {
			v.errorf("distributed_cache.insecure", "applies only to an S3-compatible endpoint")
		}
		return
	}
	u := v.httpURL("distributed_cache.endpoint", dc.Endpoint)
	if u == nil {
		return
	}
	if u.Path != "" && u.Path != "/" {
		v.errorf("distributed_cache.endpoint", "must not have a path, got %q", dc.Endpoint)
	}
	if u.Scheme == "http" && !dc.Insecure {
		v.errorf("distributed_cache.endpoint", "is http; set distributed_cache.insecure to allow it")
	}
}

func (v *validator) observability(o *Observability) {
	switch o.LogFormat {
	case LogJSON, LogText:
	default:
		v.errorf("observability.log_format", "must be json or text, got %q", o.LogFormat)
	}
	switch strings.ToLower(o.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		v.errorf("observability.log_level", "must be one of debug, info, warn, error; got %q", o.LogLevel)
	}
	if v.required("observability.listen_address", o.ListenAddress) {
		if _, _, err := net.SplitHostPort(o.ListenAddress); err != nil {
			v.errorf("observability.listen_address", "must be host:port or :port, got %q", o.ListenAddress)
		}
	}
}
