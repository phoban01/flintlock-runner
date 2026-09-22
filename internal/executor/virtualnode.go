package executor

import (
	"context"
	"log/slog"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// virtualNodeLookupTimeout bounds the read of one Virtual Node. Prepare has
// its own timeout around it, but the lookup has no context of its own to
// inherit that from.
const virtualNodeLookupTimeout = 10 * time.Second

// NodeGetter reads a Node by name. The Nodes client of a Kubernetes
// clientset is one.
type NodeGetter interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error)
}

// virtualNodes is the InventoryLookup of a cluster fleet, which has no
// Inventory: the Placement names the Virtual Node of the claimed pod
// (KF-126), and the Pod Provider has published the Host Services of its
// Host on that node (KF-017).
type virtualNodes struct {
	nodes     NodeGetter
	upstreams []config.HTTPCacheUpstream
	log       *slog.Logger
}

// NewVirtualNodeInventory returns the InventoryLookup of a cluster fleet.
// Each lookup reads the named Virtual Node and turns its Host Service
// annotations into the Host Service addresses of an Inventory entry, in the
// forms the Fleet Controller records for a Host it provisioned, so that the
// HostServiceEnvResolver treats both alike. upstreams are the configured
// HTTP cache upstreams, each of which the Host's HTTP cache serves under
// its own name. A nil logger discards.
func NewVirtualNodeInventory(nodes NodeGetter, upstreams []config.HTTPCacheUpstream, log *slog.Logger) InventoryLookup {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &virtualNodes{nodes: nodes, upstreams: upstreams, log: log}
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//# The Executor SHALL read the Host Service addresses for a Job
//# from the annotations of the Virtual Node named in the Placement.

// Host implements InventoryLookup. A Virtual Node that cannot be read is
// reported as unknown, which leaves the Job without Host Service variables
// rather than failing it (EX-062); the reason is logged.
func (v *virtualNodes) Host(name string) (*config.HostEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), virtualNodeLookupTimeout)
	defer cancel()
	node, err := v.nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		v.log.Warn("could not read the virtual node for its host services; the job gets none",
			"virtual_node", name, "error", err)
		return nil, false
	}
	return &config.HostEntry{Name: node.Name, Services: v.services(node.Annotations)}, true
}

// services reads the Host Service annotations. Each is `address:port` on
// the guest bridge gateway; one that is missing or malformed leaves its
// service without an address, and so without variables (EX-062).
func (v *virtualNodes) services(annotations map[string]string) config.HostServiceAddresses {
	addr := func(service string) string {
		a := annotations[kubelabels.HostServiceAnnotation(service)]
		if _, _, err := net.SplitHostPort(a); err != nil {
			return ""
		}
		return a
	}
	var out config.HostServiceAddresses
	if a := addr(ServiceBuildkit); a != "" {
		out.Buildkit = "tcp://" + a
	}
	if a := addr(ServiceGoProxy); a != "" {
		out.GoProxy = "http://" + a
	}
	if a := addr(ServiceRegistryMirror); a != "" {
		out.RegistryMirror = "http://" + a
	}
	if a := addr(ServiceHTTPCache); a != "" && len(v.upstreams) > 0 {
		out.HTTPCache = make(map[string]string, len(v.upstreams))
		for _, u := range v.upstreams {
			out.HTTPCache[u.Name] = "http://" + a + "/" + u.Name
		}
	}
	return out
}
