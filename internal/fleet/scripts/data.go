package scripts

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// Host layout. These names are shared by the scripts, the Inventory entry
// the Provisioner writes and the tests.
const (
	// ThinPool is the LVM volume group holding the containerd devicemapper
	// thin pool; the pool is ThinPool/thinpool, which containerd sees as
	// ThinPool-thinpool. It is flintlock's default name.
	ThinPool = "flintlock"
	// Bridge is the guest bridge TAP interfaces are attached to (FL-040).
	Bridge = "flbr0"
	// ContainerdNamespace is flintlock's containerd namespace (FL-028).
	ContainerdNamespace = "flintlock"
	// ContainerdSocket is where the provisioned containerd listens.
	ContainerdSocket = "/run/containerd/containerd.sock"
	// PoolAgentPort is the Pool Manager host agent port that FL-045 closes
	// to guests. battery no longer ships a host agent and nothing installs
	// one (FL-050 is withdrawn), but the port stays closed as FL-045 says.
	PoolAgentPort = 9091
	// MetricsPort is flintlock's HTTP endpoint, which serves metrics.
	MetricsPort = 8090
	// CacheRoot is where a cache volume device is mounted (FL-107); a cache
	// volume directory is used where it is.
	CacheRoot = "/var/lib/flintlock-runner/cache"
	// TLSDir holds the flintlockd certificate, key and CA on each Host.
	TLSDir = "/etc/flintlock-runner/tls"
	// MetadataAddress is the EC2 instance metadata service (FL-044, SE-030).
	MetadataAddress = "169.254.169.254"
	// MetadataAddressV6 is its IPv6 address on Nitro instances.
	MetadataAddressV6 = "fd00:ec2::254"
)

// Pinned versions of the Host Service software. The configuration pins the
// flintlock stack and the Pool Manager (CF-060); the Host Services are
// pinned here until the configuration grows fields for them.
const (
	BuildkitVersion    = "v0.23.2"
	RootlesskitVersion = "v2.3.5"
	AthensVersion      = "v0.15.4"
	ZotVersion         = "v2.1.5"
	GoToolchainVersion = "1.25.1"
)

// Internal ports of the Host Services that sit behind nginx; they listen on
// the loopback address only.
const (
	athensInternalPort = 3999
)

// The module guest verification fetches through the proxy when no module
// is configured for pre-warming (FL-109).
const (
	defaultVerifyModule  = "golang.org/x/mod"
	defaultVerifyVersion = "v0.17.0"
)

// Secret names in the bundle EncodeSecrets writes to a script's standard
// input (SE-015).
const (
	SecretFlintlockdToken   = "flintlockd_token"
	SecretTLSCA             = "tls_ca"
	SecretTLSCert           = "tls_cert"
	SecretTLSKey            = "tls_key"
	SecretGoProxyCredential = "go_proxy_credential"
	// SecretRegistryPrefix is followed by the index of the upstream in
	// host_services.registry_mirror.upstreams.
	SecretRegistryPrefix = "registry_credential_"
)

// Option keys read from RenderInput.Options.
const (
	// OptionPoolManagerListen is the address the Pool Manager daemon's API
	// listens on, host:port (control_node).
	OptionPoolManagerListen = "pool_manager_listen"
	// OptionPoolManagerCert and OptionPoolManagerKey are the daemon's server
	// certificate and key on the Control Node; empty means insecure.
	OptionPoolManagerCert = "pool_manager_tls_cert"
	OptionPoolManagerKey  = "pool_manager_tls_key"
	// OptionHostCAFile is the CA the daemon verifies flintlockd with.
	OptionHostCAFile = "host_ca_file"
	// OptionStopFlintlockd lets drain stop flintlockd once no MicroVM runs.
	OptionStopFlintlockd = "stop_flintlockd"
	// OptionPurge and OptionTerminate mirror the teardown flags.
	OptionPurge = "purge"

	// OptionRunnerConfig is the absolute path of the generated Runner
	// configuration the Runner's service starts with (runner, FL-122).
	OptionRunnerConfig = "runner_config"
	// OptionRunnerBinary is the flintlock-runner binary the step installs
	// as RunnerBinary; empty keeps the one installed.
	OptionRunnerBinary = "runner_binary"
	// OptionRunnerStateDir is the Runner's state directory, which its user
	// owns.
	OptionRunnerStateDir = "runner_state_dir"
	// OptionRunnerReads lists, one per line, the absolute paths of the
	// files the Runner reads: its configuration, the Inventory and TLS
	// material. The step lets the Runner's user read each.
	OptionRunnerReads = "runner_reads"
	// OptionRunnerStopTimeout is how long systemd waits for the Runner's
	// graceful shutdown, as a Go duration.
	OptionRunnerStopTimeout = "runner_stop_timeout"
	// OptionRunnerRemove makes the runner step stop and disable the
	// service instead, for teardown, when it is "true".
	OptionRunnerRemove = "runner_remove"
)

// The Runner's service on the Control Node (runner).
const (
	// RunnerUnit is the systemd unit, and RunnerUser the unprivileged user
	// it runs as.
	RunnerUnit = "flintlock-runner"
	RunnerUser = "flintlock-runner"
	// RunnerBinary is where the step installs the binary the unit runs.
	RunnerBinary = "/usr/local/bin/flintlock-runner"
	// runnerSettle is how long the Runner has to stay active after a start
	// before the step reports it active.
	runnerSettle = 10 * time.Second
)

// data is what the templates see.
type data struct {
	Step     fleet.Step
	Arch     string
	Versions config.PinnedVersions
	Instance fleet.Instance

	ThinPool       string
	ThinPoolDevice string

	// Address is the Host's private address flintlockd listens on; empty
	// (user-data) means detect at run time.
	Address string
	Port    int
	// Insecure starts flintlockd without TLS (FL-026).
	Insecure bool
	// SSHProxy enables the SSH proxy API (FL-025).
	SSHProxy bool

	Bridge    string
	Subnet    string
	Gateway   string
	PrefixLen int
	Netmask   string
	DHCPStart string
	DHCPEnd   string
	// ControlPorts are the Host's own ports guests may never reach (FL-045).
	ControlPorts []int
	// Peers are the other Hosts' private addresses (FL-046).
	Peers []string
	// EgressAllow is the guest egress allow-list, empty when unrestricted
	// (SE-032).
	EgressAllow []string

	Params config.LaunchTemplateParameters

	Services services
	Images   []image
	// GoPrewarm and MirrorPrewarm drive the prewarm step (FL-112).
	GoPrewarm     []goModule
	MirrorPrewarm []string

	// PoolManagerConfig is the daemon's JSON config without secrets
	// (control_node).
	PoolManagerHosts []pmHost
	PMListen         string
	PMCert           string
	PMKey            string
	HostCAFile       string

	// Runner is the Runner's service on the Control Node (runner).
	Runner runnerService

	// Guest verification (guest_verify): the Host's Inventory entry and the
	// module fetched through its proxy, as a proxy URL path without the
	// extension.
	Entry        *config.HostEntry
	VerifyModule string

	Options map[string]string
	Steps   []renderedStep

	// The host layout constants, for the templates.
	ContainerdSocket    string
	ContainerdNamespace string
	TLSDir              string
	MetadataAddress     string
	MetadataAddressV6   string
	BuildkitVersion     string
	RootlesskitVersion  string
	AthensVersion       string
	ZotVersion          string
	GoToolchainVersion  string
}

type goModule struct {
	Module  string
	Version string
	// Path is the proxy URL path of the module, escaped as the Go module
	// proxy protocol requires.
	Path string
}

// image is one Profile image and the architecture of its Profile; an empty
// architecture matches every Host.
type image struct {
	Arch string
	Ref  string
}

type pmHost struct {
	Name     string
	Endpoint string
}

type renderedStep struct {
	Name    string
	Content string
}

type services struct {
	CacheDevice string
	CacheDir    string
	// Root is where Host Service storage lives: the mounted cache volume.
	Root       string
	CacheBytes int64
	CacheMB    int64

	Buildkit      bool
	BuildkitPort  int
	BuildkitGCMB  int64
	BuildkitGC    []kv
	GoProxy       bool
	GoProxyPort   int
	AthensPort    int
	GoUpstream    string
	Private       []string
	PrivateRegex  string
	PrivateVCS    string
	CredParam     string
	RevalidateSec int64
	Mirror        bool
	MirrorPort    int
	Registries    []registry
	ZotConfig     string
	HTTPCache     bool
	HTTPCachePort int
	HTTPUpstreams []httpUpstream
	Ports         []int
	Units         []string
}

type kv struct{ Key, Value string }

type registry struct {
	Index int
	URL   string
	Host  string
	Param string
}

type httpUpstream struct {
	Name     string
	URL      string
	Host     string
	MaxBytes int64
	TTLSec   int64
}

func newData(step fleet.Step, in fleet.RenderInput, atBoot bool) (*data, error) {
	d := &data{
		Step:     step,
		Instance: in.Instance,
		ThinPool: ThinPool,
		Bridge:   Bridge,
		Options:  in.Options,

		ContainerdSocket:    ContainerdSocket,
		ContainerdNamespace: ContainerdNamespace,
		TLSDir:              TLSDir,
		MetadataAddress:     MetadataAddress,
		MetadataAddressV6:   MetadataAddressV6,
		BuildkitVersion:     BuildkitVersion,
		RootlesskitVersion:  RootlesskitVersion,
		AthensVersion:       AthensVersion,
		ZotVersion:          ZotVersion,
		GoToolchainVersion:  GoToolchainVersion,
	}
	if d.Options == nil {
		d.Options = map[string]string{}
	}
	f := in.Fleet
	d.Versions = f.Versions
	d.ThinPoolDevice = f.ThinPoolDevice
	d.Port = f.Flintlockd.Port
	d.Insecure = f.Flintlockd.Insecure
	if f.LaunchTemplate != nil {
		d.Params = f.LaunchTemplate.Parameters
	}

	//= docs/requirements/06-fleet.md#host-provisioning
	//# The Fleet Controller SHALL select binaries and images matching
	//# the instance's architecture.
	switch {
	case atBoot || step == fleet.StepUserData:
		// Launch template mode: the instance does not exist yet, so the
		// script detects its architecture and address on the Host.
	case step == fleet.StepControlNode || step == fleet.StepGuestVerify || step == fleet.StepRunner:
		// These run on the Control Node and in a guest, not on the Host.
	case in.Instance.Arch == config.ArchAMD64 || in.Instance.Arch == config.ArchARM64:
		d.Arch = string(in.Instance.Arch)
		d.Address = in.Instance.PrivateIP
		if d.Address == "" {
			return nil, fmt.Errorf("instance %s has no private address", in.Instance.ID)
		}
	default:
		return nil, fmt.Errorf("instance %s: unsupported architecture %q", in.Instance.ID, in.Instance.Arch)
	}

	//= docs/requirements/06-fleet.md#host-provisioning
	//# The Fleet Controller SHALL enable the `flintlockd` exec API on
	//# every Host and SHALL enable the SSH proxy API where any Profile uses the
	//# proxied `ssh` Guest Transport.
	for _, p := range in.Profiles {
		if p.Transport.Kind == config.TransportSSH {
			d.SSHProxy = true
		}
	}

	if err := d.network(f, in); err != nil {
		return nil, err
	}
	if err := d.hostServices(in.HostServices); err != nil {
		return nil, err
	}
	d.images(in.Profiles)
	d.poolManager(in)
	if step == fleet.StepRunner {
		if err := d.runner(); err != nil {
			return nil, err
		}
	}
	d.Entry = findEntry(in.Inventory, in.Instance)
	d.VerifyModule = escapeModulePath(defaultVerifyModule) + "/@v/" + defaultVerifyVersion
	if len(d.GoPrewarm) > 0 {
		d.VerifyModule = d.GoPrewarm[0].Path
	}
	return d, nil
}

// network derives the guest bridge addressing from the configured subnet
// (FL-040, FL-041) and the firewall sets (FL-045, FL-046, SE-032).
func (d *data) network(f config.Fleet, in fleet.RenderInput) error {
	subnet := f.GuestSubnet
	if subnet == "" {
		subnet = config.DefaultGuestSubnet
	}
	p, err := netip.ParsePrefix(subnet)
	if err != nil || !p.Addr().Is4() || p.Bits() > 29 {
		return fmt.Errorf("guest subnet %q: need an IPv4 CIDR of /29 or larger", subnet)
	}
	p = p.Masked()
	gw := p.Addr().Next()
	base := binary.BigEndian.Uint32(p.Addr().AsSlice())
	size := uint32(1) << (32 - p.Bits())
	last := uint32ToAddr(base + size - 2)
	start := gw.Next()
	if size > 64 {
		start = uint32ToAddr(base + 10)
	}
	d.Subnet = p.String()
	d.Gateway = gw.String()
	d.PrefixLen = p.Bits()
	d.Netmask = net.IP(net.CIDRMask(p.Bits(), 32)).String()
	d.DHCPStart = start.String()
	d.DHCPEnd = last.String()

	d.ControlPorts = []int{d.Port, PoolAgentPort, MetricsPort}
	if d.Port == 0 {
		d.ControlPorts[0] = config.DefaultFlintlockdPort
	}

	self := in.Instance.PrivateIP
	seen := map[string]bool{}
	for _, h := range in.Inventory.Hosts {
		host, _, err := net.SplitHostPort(h.Endpoint)
		if err != nil {
			host = h.Endpoint
		}
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is4() {
			// A name cannot go into an nftables set; it is resolved by
			// nobody, so refuse rather than leave a peer reachable.
			return fmt.Errorf("inventory host %s: endpoint %q is not an IPv4 address", h.Name, h.Endpoint)
		}
		if a.String() == self || h.Name == in.Instance.ID || seen[a.String()] {
			continue
		}
		seen[a.String()] = true
		d.Peers = append(d.Peers, a.String())
	}
	sort.Strings(d.Peers)

	for _, e := range f.EgressAllowList {
		if a, err := netip.ParseAddr(e); err == nil && a.Is4() {
			d.EgressAllow = append(d.EgressAllow, a.String())
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil && p.Addr().Is4() {
			d.EgressAllow = append(d.EgressAllow, p.Masked().String())
			continue
		}
		return fmt.Errorf("egress allow-list entry %q is not an IPv4 address or CIDR", e)
	}
	return nil
}

func uint32ToAddr(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func (d *data) hostServices(hs config.HostServices) error {
	s := &d.Services
	s.CacheDevice = hs.CacheVolume.Device
	s.CacheDir = hs.CacheVolume.Directory
	if s.CacheDevice == "" && s.CacheDir == "" {
		s.CacheDir = config.DefaultCacheVolumeDirectory
	}
	s.CacheBytes = int64(hs.CacheVolume.SizeCap)
	if s.CacheBytes == 0 {
		s.CacheBytes = int64(config.DefaultCacheVolumeSizeCap)
	}
	s.CacheMB = s.CacheBytes >> 20
	s.Root = s.CacheDir
	if s.CacheDevice != "" {
		s.Root = CacheRoot
	}

	if hs.Buildkit.IsEnabled() {
		s.Buildkit = true
		s.BuildkitPort = hs.Buildkit.Port
		s.BuildkitGCMB = int64(hs.Buildkit.StorageLimit) >> 20
		gc, err := parseGCPolicy(hs.Buildkit.GCPolicy)
		if err != nil {
			return err
		}
		s.BuildkitGC = gc
		s.Ports = append(s.Ports, s.BuildkitPort)
		s.Units = append(s.Units, "flintlock-runner-buildkitd")
	}
	if hs.GoProxy.IsEnabled() {
		s.GoProxy = true
		s.GoProxyPort = hs.GoProxy.Port
		s.AthensPort = athensInternalPort
		s.GoUpstream = hs.GoProxy.Upstream
		if s.GoUpstream == "" {
			s.GoUpstream = config.DefaultGoProxyUpstream
		}
		s.Private = hs.GoProxy.PrivatePatterns
		s.PrivateVCS = hs.GoProxy.PrivateVCSHost
		s.CredParam = hs.GoProxy.CredentialParameter
		s.RevalidateSec = int64(hs.GoProxy.PrivateRevalidateInterval / time.Second)
		if len(s.Private) > 0 {
			if s.PrivateVCS == "" {
				return fmt.Errorf("go_proxy: private_patterns need private_vcs_host")
			}
			if s.RevalidateSec == 0 {
				s.RevalidateSec = int64(config.DefaultPrivateRevalidateInterval / time.Second)
			}
			s.PrivateRegex = privateRegex(s.Private)
		}
		s.Ports = append(s.Ports, s.GoProxyPort)
		s.Units = append(s.Units, "flintlock-runner-athens")
		for _, m := range hs.GoProxy.Prewarm {
			mod, ver, ok := strings.Cut(m, "@")
			if !ok || mod == "" || ver == "" {
				return fmt.Errorf("go_proxy prewarm entry %q is not module@version", m)
			}
			d.GoPrewarm = append(d.GoPrewarm, goModule{Module: mod, Version: ver, Path: escapeModulePath(mod) + "/@v/" + escapeModulePath(ver)})
		}
	}
	if hs.RegistryMirror.IsEnabled() {
		s.Mirror = true
		s.MirrorPort = hs.RegistryMirror.Port
		ups := hs.RegistryMirror.Upstreams
		if len(ups) == 0 {
			ups = []config.RegistryUpstream{{URL: config.DefaultRegistryUpstream}}
		}
		for i, u := range ups {
			host := u.URL
			if _, rest, ok := strings.Cut(host, "://"); ok {
				host = rest
			}
			host = strings.TrimSuffix(host, "/")
			s.Registries = append(s.Registries, registry{Index: i, URL: u.URL, Host: host, Param: u.CredentialParameter})
		}
		zc, err := zotConfig(s.Root, d.Gateway, s.MirrorPort, s.Registries)
		if err != nil {
			return err
		}
		s.ZotConfig = zc
		s.Ports = append(s.Ports, s.MirrorPort)
		s.Units = append(s.Units, "flintlock-runner-zot")
		d.MirrorPrewarm = append(d.MirrorPrewarm, hs.RegistryMirror.Prewarm...)
	}
	if hs.HTTPCache.IsEnabled() {
		s.HTTPCache = true
		s.HTTPCachePort = hs.HTTPCache.Port
		for _, u := range hs.HTTPCache.Upstreams {
			host := u.URL
			if _, rest, ok := strings.Cut(host, "://"); ok {
				host = rest
			}
			host, _, _ = strings.Cut(host, "/")
			ttl := u.TTL
			if ttl == 0 {
				ttl = config.DefaultHTTPCacheTTL
			}
			s.HTTPUpstreams = append(s.HTTPUpstreams, httpUpstream{
				Name: u.Name, URL: strings.TrimSuffix(u.URL, "/"), Host: host,
				MaxBytes: int64(u.SizeLimit), TTLSec: int64(ttl / time.Second),
			})
		}
		s.Ports = append(s.Ports, s.HTTPCachePort)
	}
	if s.HTTPCache || s.GoProxy {
		s.Units = append(s.Units, "nginx")
	}
	return nil
}

// parseGCPolicy reads the buildkit GC policy, "key=value,key=value", into
// the keys of a [[worker.oci.gcpolicy]] table.
func parseGCPolicy(p string) ([]kv, error) {
	if strings.TrimSpace(p) == "" {
		return nil, nil
	}
	var out []kv
	for _, part := range strings.Split(p, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k == "" || v == "" || strings.ContainsAny(k+v, "\"\n\\") {
			return nil, fmt.Errorf("buildkit gc_policy %q: want key=value[,key=value]", p)
		}
		if _, err := strconv.ParseInt(v, 10, 64); err != nil && v != "true" && v != "false" {
			v = strconv.Quote(v)
		}
		out = append(out, kv{Key: k, Value: v})
	}
	return out, nil
}

// escapeModulePath applies the module proxy protocol's case encoding: an
// upper-case letter becomes '!' followed by its lower case.
func escapeModulePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// privateRegex turns GOPRIVATE-style glob patterns into one regular
// expression over escaped module paths for the nginx location that caches
// private module listings (FL-115). A pattern matches a path prefix of
// whole elements, as GOPRIVATE does.
func privateRegex(patterns []string) string {
	alts := make([]string, 0, len(patterns))
	for _, p := range patterns {
		var b strings.Builder
		for _, r := range escapeModulePath(strings.TrimSuffix(p, "/")) {
			switch r {
			case '*':
				b.WriteString("[^/]*")
			case '?':
				b.WriteString("[^/]")
			case '.', '+', '(', ')', '|', '[', ']', '{', '}', '^', '$', '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			default:
				b.WriteRune(r)
			}
		}
		alts = append(alts, b.String())
	}
	return "^/(" + strings.Join(alts, "|") + ")(/.*)?/@(v/list|latest)$"
}

// zotConfig is the registry mirror's configuration: a pull-through cache
// with on-demand sync from every configured upstream (FL-104), listening
// on the bridge gateway only (FL-101), storing under the cache volume
// (FL-107). Credentials are in a separate file the step writes from the
// secrets (SE-052).
func zotConfig(root, gw string, port int, regs []registry) (string, error) {
	type content struct {
		Prefix string `json:"prefix"`
	}
	type reg struct {
		URLs      []string  `json:"urls"`
		OnDemand  bool      `json:"onDemand"`
		TLSVerify bool      `json:"tlsVerify"`
		Content   []content `json:"content"`
	}
	cfg := map[string]any{
		"distSpecVersion": "1.1.0",
		"storage": map[string]any{
			"rootDirectory": root + "/registry",
			"gc":            true,
			"dedupe":        true,
		},
		"http": map[string]any{
			"address": gw,
			"port":    strconv.Itoa(port),
		},
		"log": map[string]any{"level": "info"},
	}
	var rs []reg
	for _, r := range regs {
		rs = append(rs, reg{URLs: []string{r.URL}, OnDemand: true, TLSVerify: true, Content: []content{{Prefix: "**"}}})
	}
	cfg["extensions"] = map[string]any{
		"sync": map[string]any{
			"enable":          true,
			"credentialsFile": "/etc/flintlock-runner/zot/credentials.json",
			"registries":      rs,
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if strings.Contains(string(b), "FLR_EOF") {
		return "", fmt.Errorf("registry mirror configuration contains the heredoc delimiter")
	}
	return string(b), nil
}

// images lists the kernel, initrd, root filesystem and volume images of
// every Profile of the instance's architecture (FL-027, FL-028).
func (d *data) images(profiles []config.Profile) {
	seen := map[image]bool{}
	for _, p := range profiles {
		if d.Arch != "" && p.Arch != "" && string(p.Arch) != d.Arch {
			continue
		}
		add := func(ref string) {
			im := image{Arch: string(p.Arch), Ref: ref}
			if ref != "" && !seen[im] {
				seen[im] = true
				d.Images = append(d.Images, im)
			}
		}
		add(p.Kernel.Image)
		if p.Initrd != nil {
			add(p.Initrd.Image)
		}
		add(p.RootFS)
		for _, v := range p.AdditionalVolumes {
			add(v.Image)
		}
	}
}

func (d *data) poolManager(in fleet.RenderInput) {
	for _, h := range in.Inventory.Hosts {
		d.PoolManagerHosts = append(d.PoolManagerHosts, pmHost{Name: h.Name, Endpoint: h.Endpoint})
	}
	d.PMListen = d.Options[OptionPoolManagerListen]
	if d.PMListen == "" {
		d.PMListen = ":9443"
	}
	d.PMCert = d.Options[OptionPoolManagerCert]
	d.PMKey = d.Options[OptionPoolManagerKey]
	d.HostCAFile = d.Options[OptionHostCAFile]
}

// runnerService is what the runner template sees.
type runnerService struct {
	Unit   string
	User   string
	Binary string
	// Source is the binary installed as Binary; empty keeps Binary.
	Source   string
	Config   string
	StateDir string
	// Reads are the files the Runner reads.
	Reads          []string
	StopTimeoutSec int64
	SettleSec      int64
	Remove         bool
}

// runner reads the runner step's options. Every path ends up on the unit's
// ExecStart line or in a shell word, so each has to be absolute and free of
// what systemd or a here-document would interpret.
func (d *data) runner() error {
	r := runnerService{
		Unit:      RunnerUnit,
		User:      RunnerUser,
		Binary:    RunnerBinary,
		Source:    d.Options[OptionRunnerBinary],
		Config:    d.Options[OptionRunnerConfig],
		StateDir:  d.Options[OptionRunnerStateDir],
		SettleSec: int64(runnerSettle / time.Second),
		Remove:    d.Options[OptionRunnerRemove] == "true",
	}
	if r.Config == "" {
		r.Config = config.DefaultRunnerConfigPath
	}
	if r.StateDir == "" {
		r.StateDir = config.DefaultStateDir
	}
	stop := config.DefaultShutdownTimeout + 30*time.Second
	if v := d.Options[OptionRunnerStopTimeout]; v != "" {
		var err error
		if stop, err = time.ParseDuration(v); err != nil || stop <= 0 {
			return fmt.Errorf("runner: %s %q is not a positive duration", OptionRunnerStopTimeout, v)
		}
	}
	r.StopTimeoutSec = int64((stop + time.Second - 1) / time.Second)
	for _, f := range strings.Split(d.Options[OptionRunnerReads], "\n") {
		if f = strings.TrimSpace(f); f != "" {
			r.Reads = append(r.Reads, f)
		}
	}
	for _, p := range append([]string{r.Config, r.StateDir, r.Source}, r.Reads...) {
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") || strings.ContainsFunc(p, func(c rune) bool {
			return unicode.IsSpace(c) || !unicode.IsPrint(c) || strings.ContainsRune("\"'`$\\%;", c)
		}) {
			return fmt.Errorf("runner: %q has to be an absolute path without spaces, quotes, $, %%, ; or backslashes", p)
		}
	}
	if filepath.Clean(r.StateDir) == "/" {
		return fmt.Errorf("runner: the state directory cannot be /")
	}
	d.Runner = r
	return nil
}

func findEntry(inv fleet.Inventory, inst fleet.Instance) *config.HostEntry {
	for i := range inv.Hosts {
		if inv.Hosts[i].Name == inst.ID {
			return &inv.Hosts[i]
		}
	}
	return nil
}
