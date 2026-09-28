// Package hostservices publishes where a Host's Host Services listen, as
// annotations on the Host's Node, for the Executor of the claim design to
// read (docs/requirements/12-cluster-fleet.md, KF-189 and KF-194). It is
// `flr host-services`, a container of the Host Agent.
//
// The Exec Agent of flintlock-runner published these annotations until
// battery-operator's Exec Agent replaced it. battery-operator's agent knows
// nothing of the Host Services, so the Host Agent that runs them publishes
// where they are.
package hostservices

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

const (
	// DefaultResyncInterval is how often the annotations are written again,
	// in case something else changed or removed them.
	DefaultResyncInterval = time.Minute
	// retryInterval is how soon a write that failed is tried again.
	retryInterval = 5 * time.Second
	// callTimeout bounds one write to the API server.
	callTimeout = 10 * time.Second
)

// Config is the configuration of `flr host-services`, a YAML file that the
// Host Agent's render.sh makes for each Host.
type Config struct {
	// HostNode is the name of the Host's own Node. The Host Agent sets it
	// from the downward API.
	HostNode string `yaml:"host_node,omitempty"`
	// Kubeconfig is the path of a kubeconfig; empty uses the in-cluster
	// configuration.
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
	// BridgeGateway is the guest bridge gateway address, where every Host
	// Service listens (KF-071).
	BridgeGateway string `yaml:"bridge_gateway"`
	// HostServices are the Host Services of FL-100 by name. Only enabled
	// ones are published.
	HostServices map[string]HostService `yaml:"host_services,omitempty"`
	// ResyncInterval is how often the annotations are written again.
	ResyncInterval time.Duration `yaml:"resync_interval,omitempty"`
}

// HostService is one Host Service: whether it runs, and its port on the
// bridge gateway.
type HostService struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

// LoadConfig reads, defaults and validates a configuration file, applying
// override, such as command-line flags, after the file is read. Unknown
// keys are errors, as they are in the Runner's configuration.
func LoadConfig(path string, override func(*Config)) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("host-services: reading configuration: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("host-services: parsing %s: %w", path, err)
	}
	if override != nil {
		override(&cfg)
	}
	if cfg.ResyncInterval == 0 {
		cfg.ResyncInterval = DefaultResyncInterval
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate reports every invalid field, sorted.
func (c *Config) Validate() error {
	var errs []error
	fail := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if c.HostNode == "" {
		fail("host_node", "is required")
	}
	// The Executor reads a Host Service by its name (KF-189), so a name it
	// does not know would be published and never read; refusing it makes a
	// typo fail at start.
	known := kubelabels.HostServiceNames()
	for _, name := range slices.Sorted(maps.Keys(c.HostServices)) {
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
	if c.ResyncInterval <= 0 {
		fail("resync_interval", "has to be positive")
	}
	if len(errs) == 0 {
		return nil
	}
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return fmt.Errorf("host-services: invalid configuration: %w", errors.Join(errs...))
}

// EnabledHostServices lists the enabled Host Services by name, sorted.
func (c *Config) EnabledHostServices() []string {
	var names []string
	for name, svc := range c.HostServices {
		if svc.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

//= docs/requirements/12-cluster-fleet.md#cluster-host-agent
//# The Host Agent SHALL publish the address and port of each
//# enabled Host Service on the guest bridge gateway, under the names
//# `buildkit`, `go_proxy`, `registry_mirror` and `http_cache`, as the
//# annotations `host-service.gitlab-runner.flintlock.dev/<name>` on its Host's
//# Node.

// Annotations are the annotations the Host Agent keeps on its Host's Node:
// one per enabled Host Service, keyed by kubelabels.HostServiceAnnotation
// with the service's name and valued `address:port` on the bridge gateway,
// which is what the Executor reads for a Job (KF-189). A Host Service that
// is not enabled has its annotation removed, a null in the merge patch, so
// that a service switched off stops being offered to Jobs.
func (c *Config) Annotations() map[string]any {
	out := map[string]any{}
	for _, name := range kubelabels.HostServiceNames() {
		out[kubelabels.HostServiceAnnotation(name)] = nil
	}
	for _, name := range c.EnabledHostServices() {
		out[kubelabels.HostServiceAnnotation(name)] = net.JoinHostPort(c.BridgeGateway, fmt.Sprint(c.HostServices[name].Port))
	}
	return out
}

// Publish applies a JSON merge patch of annotations, and of nothing else,
// to the Node node. The admission policy of the Host Agent refuses it any
// other change (deploy/host-agent/host-services-admission-policy.yaml).
func Publish(ctx context.Context, kube kubernetes.Interface, node string, annotations map[string]any) error {
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	if _, err := kube.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, data, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("host-services: annotating the host's node %s: %w", node, err)
	}
	return nil
}

// Run publishes the annotations of cfg on the Host's Node at once, and
// again every resync interval, until ctx ends. A write that fails is
// logged and tried again soon. Run returns nil when ctx ends.
func Run(ctx context.Context, cfg *Config, kube kubernetes.Interface, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	annotations := cfg.Annotations()
	for {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		err := Publish(callCtx, kube, cfg.HostNode, annotations)
		cancel()
		wait := cfg.ResyncInterval
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			log.Warn("Could not publish the Host Services on the host's Node", "node", cfg.HostNode, "error", err)
			wait = min(wait, retryInterval)
		default:
			log.Debug("Published the Host Services on the host's Node", "node", cfg.HostNode, "services", cfg.EnabledHostServices())
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
