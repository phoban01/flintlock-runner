package kubelet

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Defaults of the Pod Provider's configuration.
const (
	// DefaultFlintlockd is where the Host Image's flintlockd listens
	// (HI-042).
	DefaultFlintlockd = "unix:///run/flintlock/flintlockd.sock"
	// DefaultMicroVMNamespace is the flintlock namespace the provider
	// creates its MicroVMs in and sweeps for orphans (KF-028).
	DefaultMicroVMNamespace = "flintlock-runner"
	// DefaultNotReadyDir is the directory of the not-ready-reason contract
	// (KF-016); see ReadNotReadyReasons.
	DefaultNotReadyDir = "/run/flr/not-ready.d"
	// DefaultListen is the kubelet API port.
	DefaultListen = ":10250"
	// DefaultLeaseDuration is how old a claimed pod's lease may grow before
	// the pod is deleted (KF-032).
	DefaultLeaseDuration = 2 * time.Minute
	// DefaultDrainTimeout bounds how long claimed pods hold a drain open
	// (KF-093).
	DefaultDrainTimeout = time.Hour
	// DefaultSyncInterval is how often MicroVM state, leases, readiness and
	// the drain state are reconciled.
	DefaultSyncInterval = 2 * time.Second
	// DefaultGuardImage is the image of the drain guard pod, which only has
	// to stay running.
	DefaultGuardImage = "registry.k8s.io/pause:3.10"
	// DefaultGuestAddressCommand prints the guest's addresses; the first
	// word of its output is reported as the pod's address (KF-025).
	DefaultGuestAddressCommand = "hostname -I"
)

// Config is the configuration of `flr kubelet`, read from a YAML file of its
// own: the Pod Provider runs on a Host, where the Runner's configuration
// does not exist.
type Config struct {
	// HostNode is the name of the Host's own Node. The Virtual Node is named
	// after it (KF-010). The Host Agent sets it from the downward API.
	HostNode string `yaml:"host_node"`
	// Kubeconfig is the path of a kubeconfig; empty uses the in-cluster
	// configuration.
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
	// Flintlockd is the local endpoint of flintlockd: `unix://` and a socket
	// path, or a loopback `address:port` (KF-018).
	Flintlockd string `yaml:"flintlockd"`
	// MicroVMNamespace is the flintlock namespace of this provider's
	// MicroVMs.
	MicroVMNamespace string `yaml:"microvm_namespace"`
	// HostReserve is what is held back from the Virtual Node's capacity for
	// the Host itself (KF-012, HI-061).
	HostReserve Reserve `yaml:"host_reserve"`
	// MaxMicroVMs is the Virtual Node's pod limit (KF-012).
	MaxMicroVMs int `yaml:"max_microvms"`
	// BridgeGateway is the guest bridge gateway address, where the Host
	// Services listen (KF-015, KF-017).
	BridgeGateway string `yaml:"bridge_gateway"`
	// HostServices are the Host Services of FL-100 by name. Only enabled
	// ones are probed and published.
	HostServices map[string]HostService `yaml:"host_services,omitempty"`
	// NotReadyDir is the directory the Host Image's units write not ready
	// reasons to (KF-016).
	NotReadyDir string `yaml:"not_ready_dir"`
	// Listen is the address of the kubelet API (KF-030).
	Listen string `yaml:"listen"`
	// TLS is the kubelet API's serving material (KF-031). All three files
	// are required: there is no mode that serves without client
	// certificate verification.
	TLS ServerTLS `yaml:"tls"`
	// LeaseDuration is the age at which a claimed pod's lease expires
	// (KF-032).
	LeaseDuration time.Duration `yaml:"lease_duration"`
	// DrainTimeout bounds how long claimed pods hold a drain open (KF-093).
	DrainTimeout time.Duration `yaml:"drain_timeout"`
	// Guard configures the drain guard pod (KF-091).
	Guard Guard `yaml:"guard"`
	// SyncInterval is the reconcile period.
	SyncInterval time.Duration `yaml:"sync_interval"`
	// GuestAddressCommand is run in a guest, through its shell, to learn
	// the guest's bridge address (KF-025).
	GuestAddressCommand string `yaml:"guest_address_command"`
}

// Reserve is CPU and memory as Kubernetes quantities, for example `2` and
// `4Gi`.
type Reserve struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// HostService is one Host Service as the provider needs to know it.
type HostService struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

// ServerTLS is the serving certificate of the kubelet API and the
// certificate authority its clients have to be issued by.
type ServerTLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ClientCAFile is the cluster's kubelet client certificate authority.
	ClientCAFile string `yaml:"client_ca_file"`
}

// Guard is where and from what the drain guard pod is made.
type Guard struct {
	// Namespace is the namespace of the guard pod and its
	// PodDisruptionBudget, normally the Host Agent's own.
	Namespace string `yaml:"namespace"`
	Image     string `yaml:"image"`
}

// LoadConfig reads, defaults and validates a configuration file. Unknown
// keys are errors, as they are in the Runner's configuration.
func LoadConfig(path string) (*Config, error) {
	return LoadConfigWith(path, nil)
}

// LoadConfigWith is LoadConfig with overrides, such as command-line flags,
// applied after the file is read and before it is defaulted and validated.
func LoadConfigWith(path string, override func(*Config)) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("kubelet: reading configuration: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("kubelet: parsing %s: %w", path, err)
	}
	if override != nil {
		override(&cfg)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults fills every unset field that has a default.
func (c *Config) ApplyDefaults() {
	setDefault(&c.Flintlockd, DefaultFlintlockd)
	setDefault(&c.MicroVMNamespace, DefaultMicroVMNamespace)
	setDefault(&c.NotReadyDir, DefaultNotReadyDir)
	setDefault(&c.Listen, DefaultListen)
	setDefault(&c.Guard.Image, DefaultGuardImage)
	setDefault(&c.GuestAddressCommand, DefaultGuestAddressCommand)
	setDefault(&c.HostReserve.CPU, "0")
	setDefault(&c.HostReserve.Memory, "0")
	if c.LeaseDuration == 0 {
		c.LeaseDuration = DefaultLeaseDuration
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.SyncInterval == 0 {
		c.SyncInterval = DefaultSyncInterval
	}
}

func setDefault(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

// Validate reports every invalid field, first one first, in the manner of
// the Runner's configuration (CF-004).
func (c *Config) Validate() error {
	var errs []error
	fail := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if c.HostNode == "" {
		fail("host_node", "is required")
	}
	if err := ValidateLocalEndpoint(c.Flintlockd); err != nil {
		fail("flintlockd", "%v", err)
	}
	if c.MicroVMNamespace == "" {
		fail("microvm_namespace", "is required")
	}
	if _, err := resource.ParseQuantity(c.HostReserve.CPU); err != nil {
		fail("host_reserve.cpu", "%v", err)
	}
	if _, err := resource.ParseQuantity(c.HostReserve.Memory); err != nil {
		fail("host_reserve.memory", "%v", err)
	}
	if c.MaxMicroVMs <= 0 {
		fail("max_microvms", "has to be positive")
	}
	enabled := c.EnabledHostServices()
	if len(enabled) > 0 && net.ParseIP(c.BridgeGateway) == nil {
		fail("bridge_gateway", "%q is not an IP address, and enabled Host Services are reached on it", c.BridgeGateway)
	}
	for _, name := range enabled {
		if port := c.HostServices[name].Port; port <= 0 || port > 65535 {
			fail("host_services."+name+".port", "%d is not a port", port)
		}
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		fail("listen", "%v", err)
	}
	// KF-031 has no opt-out, so the material for it is required rather
	// than defaulted.
	if c.TLS.CertFile == "" {
		fail("tls.cert_file", "is required")
	}
	if c.TLS.KeyFile == "" {
		fail("tls.key_file", "is required")
	}
	if c.TLS.ClientCAFile == "" {
		fail("tls.client_ca_file", "is required: the kubelet API is a shell in every job and is never served without client certificate verification")
	}
	if c.LeaseDuration <= 0 {
		fail("lease_duration", "has to be positive")
	}
	if c.DrainTimeout <= 0 {
		fail("drain_timeout", "has to be positive")
	}
	if c.SyncInterval <= 0 {
		fail("sync_interval", "has to be positive")
	}
	if c.Guard.Namespace == "" {
		fail("guard.namespace", "is required")
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("kubelet: invalid configuration: %w", errors.Join(errs...))
}

// EnabledHostServices lists the enabled Host Services by name, sorted.
func (c *Config) EnabledHostServices() []string {
	names := make([]string, 0, len(c.HostServices))
	for name, svc := range c.HostServices {
		if svc.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// unixScheme prefixes a unix socket endpoint, as gRPC writes them.
const unixScheme = "unix://"

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL reach `flintlockd` only through the
//# local endpoint of HI-042.

// ValidateLocalEndpoint accepts exactly the two shapes HI-042 allows: a unix
// socket, written `unix://` and an absolute path, or a literal loopback IP
// address and a port. A host name is refused even when it is `localhost`,
// because what it resolves to is not this function's to know; so is every
// other scheme, the unspecified address and any address of another machine.
// The flintlockd of a Host has no authentication in a cluster fleet, so a
// provider pointed anywhere else would be talking to it in the clear.
func ValidateLocalEndpoint(endpoint string) error {
	if endpoint == "" {
		return errors.New("is required")
	}
	if path, ok := strings.CutPrefix(endpoint, unixScheme); ok {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("%q: a unix socket endpoint needs an absolute path", endpoint)
		}
		return nil
	}
	if strings.Contains(endpoint, "://") || strings.HasPrefix(endpoint, "unix:") || strings.HasPrefix(endpoint, "dns:") {
		return fmt.Errorf("%q: only unix:// and a loopback address:port are local endpoints", endpoint)
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("%q: not unix:// and not an address:port: %w", endpoint, err)
	}
	if port == "" {
		return fmt.Errorf("%q: the port is missing", endpoint)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%q: the host has to be a literal loopback address, not a name", endpoint)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("%q: %s is not a loopback address; flintlockd is reached only on the Host itself", endpoint, host)
	}
	return nil
}
