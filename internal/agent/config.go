package agent

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/phoban01/flintlock-runner/internal/hostcheck"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// Defaults of the Exec Agent's configuration.
const (
	// DefaultFlintlockd is where the Host Image's flintlockd listens
	// (HI-042): the loopback port the Host Image's firewall admits only the
	// Exec Agent's user id to (HI-063).
	DefaultFlintlockd = "127.0.0.1:9090"
	// DefaultFlintlockdUserID is the user id HI-063 admits to flintlockd
	// when the Host configuration file names none, and so the one the Exec
	// Agent has to run as (KF-171).
	DefaultFlintlockdUserID = 10250
	// DefaultPort is the port of the exec API. It is neither the Host's
	// kubelet (10250) nor the Pod Provider's kubelet API (10260), which may
	// run on the same Host while the two designs coexist.
	DefaultPort = 10270
	// DefaultExecOpenTimeout bounds how long flintlockd has to open the
	// exec stream of a request (KF-177).
	DefaultExecOpenTimeout = 10 * time.Second
	// DefaultCallTimeout bounds every unary call the agent relays or makes
	// to flintlockd, and every TokenReview.
	DefaultCallTimeout = 10 * time.Second
	// DefaultDrainTimeout bounds how long Bound claims hold a drain of the
	// Host's Node open (KF-181).
	DefaultDrainTimeout = time.Hour
	// DefaultSyncInterval is how often readiness, the annotations and the
	// drain guard are reconciled.
	DefaultSyncInterval = 2 * time.Second
	// DefaultGuardImage is the image of the drain guard pod, which only has
	// to stay running.
	DefaultGuardImage = "registry.k8s.io/pause:3.10"
)

// Config is the configuration of `flr agent`, read from a YAML file of its
// own: the Exec Agent runs on a Host, where the Runner's configuration does
// not exist.
type Config struct {
	// HostNode is the name of the Host's own Node. The Host Agent sets it
	// from the downward API.
	HostNode string `yaml:"host_node"`
	// Kubeconfig is the path of a kubeconfig; empty uses the in-cluster
	// configuration.
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
	// Flintlockd is the local endpoint of flintlockd: `unix://` and a socket
	// path, or a loopback `address:port` (KF-171, HI-042).
	Flintlockd string `yaml:"flintlockd"`
	// FlintlockdUserID is the user id the Host Image admits to flintlockd
	// (HI-063). `flr agent` refuses to start as any other; -1 skips the
	// check, for running the agent off a Host.
	FlintlockdUserID int `yaml:"flintlockd_user_id"`
	// Address is the address the exec API is served on. Empty means the
	// Host's internal address, read from its Node (KF-172).
	Address string `yaml:"address,omitempty"`
	// Port is the port of the exec API.
	Port int `yaml:"port"`
	// TLS is the serving certificate of the exec API (KF-172). It is
	// required: there is no mode that serves in the clear.
	TLS ServerTLS `yaml:"tls"`
	// TokenAudiences, when set, are the audiences a caller's token has to
	// carry (KF-173). Empty accepts the API server's own.
	TokenAudiences []string `yaml:"token_audiences,omitempty"`
	// ExecOpenTimeout bounds how long flintlockd has to open the exec
	// stream of a request (KF-177).
	ExecOpenTimeout time.Duration `yaml:"exec_open_timeout"`
	// CallTimeout bounds every unary call to flintlockd and every
	// TokenReview (KF-175).
	CallTimeout time.Duration `yaml:"call_timeout"`
	// BridgeGateway is the guest bridge gateway address, where the Host
	// Services listen (KF-178, KF-179).
	BridgeGateway string `yaml:"bridge_gateway"`
	// HostServices are the Host Services of FL-100 by name. Only enabled
	// ones are probed and published.
	HostServices map[string]HostService `yaml:"host_services,omitempty"`
	// NotReadyDir is the directory the Host Image's units write not ready
	// reasons to (KF-178, HI-011).
	NotReadyDir string `yaml:"not_ready_dir"`
	// DrainTimeout bounds how long Bound claims hold a drain open (KF-181).
	DrainTimeout time.Duration `yaml:"drain_timeout"`
	// Guard configures the drain guard pod (KF-181).
	Guard Guard `yaml:"guard"`
	// SyncInterval is the reconcile period.
	SyncInterval time.Duration `yaml:"sync_interval"`
	// Claims names the claim resource the agent authorizes against. Empty
	// fields take the provisional resource; see ProvisionalClaimResource.
	Claims ClaimResourceConfig `yaml:"claims,omitempty"`
}

// ServerTLS is the serving certificate of the exec API.
type ServerTLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// HostService is one Host Service as the agent needs to know it.
type HostService struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

// Guard is where and from what the drain guard pod is made.
type Guard struct {
	// Namespace is the namespace of the guard pod and its
	// PodDisruptionBudget, normally the Host Agent's own.
	Namespace string `yaml:"namespace"`
	Image     string `yaml:"image"`
}

// ClaimResourceConfig names the claim resource by group, version and
// resource. The field paths are not configurable: a claim resource whose
// fields differ gets a ClaimLookup of its own.
type ClaimResourceConfig struct {
	Group    string `yaml:"group,omitempty"`
	Version  string `yaml:"version,omitempty"`
	Resource string `yaml:"resource,omitempty"`
}

// LoadConfig reads, defaults and validates a configuration file, applying
// override, such as command-line flags, after the file is read and before
// it is defaulted and validated. Unknown keys are errors, as they are in
// the Runner's configuration.
func LoadConfig(path string, override func(*Config)) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent: reading configuration: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("agent: parsing %s: %w", path, err)
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
	setDefault(&c.NotReadyDir, hostcheck.DefaultNotReadyDir)
	setDefault(&c.Guard.Image, DefaultGuardImage)
	setDefault(&c.Claims.Group, ProvisionalClaimResource.Group)
	setDefault(&c.Claims.Version, ProvisionalClaimResource.Version)
	setDefault(&c.Claims.Resource, ProvisionalClaimResource.Resource)
	if c.FlintlockdUserID == 0 {
		c.FlintlockdUserID = DefaultFlintlockdUserID
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.ExecOpenTimeout == 0 {
		c.ExecOpenTimeout = DefaultExecOpenTimeout
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = DefaultCallTimeout
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

// Validate reports every invalid field, sorted by field, in the manner of
// the Runner's configuration (CF-004).
func (c *Config) Validate() error {
	var errs []error
	fail := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if c.HostNode == "" {
		fail("host_node", "is required")
	}
	if err := hostcheck.ValidateLocalEndpoint(c.Flintlockd); err != nil {
		fail("flintlockd", "%v", err)
	}
	if c.FlintlockdUserID < -1 {
		fail("flintlockd_user_id", "%d is not a user id, nor -1 to skip the check", c.FlintlockdUserID)
	}
	if c.Address != "" && net.ParseIP(c.Address) == nil {
		fail("address", "%q is not an IP address", c.Address)
	}
	if c.Port <= 0 || c.Port > 65535 {
		fail("port", "%d is not a port", c.Port)
	}
	if c.TLS.CertFile == "" {
		fail("tls.cert_file", "is required: the exec API is never served in the clear")
	}
	if c.TLS.KeyFile == "" {
		fail("tls.key_file", "is required")
	}
	// A Host Service is published under its name and the Runner reads it by
	// the same name (KF-179, KF-189), so a name the Runner does not know
	// would be published and never read; refusing it makes a typo fail at
	// start.
	known := kubelabels.HostServiceNames()
	names := make([]string, 0, len(c.HostServices))
	for name := range c.HostServices {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !slices.Contains(known, name) {
			fail("host_services."+name, "is not a Host Service; the names are %s", strings.Join(known, ", "))
		}
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
	for field, d := range map[string]time.Duration{
		"exec_open_timeout": c.ExecOpenTimeout,
		"call_timeout":      c.CallTimeout,
		"drain_timeout":     c.DrainTimeout,
		"sync_interval":     c.SyncInterval,
	} {
		if d <= 0 {
			fail(field, "has to be positive")
		}
	}
	if c.Guard.Namespace == "" {
		fail("guard.namespace", "is required")
	}
	if c.Claims.Group == "" || c.Claims.Version == "" || c.Claims.Resource == "" {
		fail("claims", "group, version and resource are all required")
	}
	if len(errs) == 0 {
		return nil
	}
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return fmt.Errorf("agent: invalid configuration: %w", errors.Join(errs...))
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

// hostServiceAddress is where a Host Service listens.
func (c *Config) hostServiceAddress(name string) string {
	return net.JoinHostPort(c.BridgeGateway, fmt.Sprint(c.HostServices[name].Port))
}
