package executor

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// tripwireRegistry is a Host Registry that fails the test whenever a Host
// client is asked of it: under the claim backend the Runner holds no
// connection to any flintlockd (KF-063), so nothing may borrow or lease one.
type tripwireRegistry struct{ t *testing.T }

func (r tripwireRegistry) Get(name string) (flintlock.HostClient, error) {
	r.t.Errorf("the executor asked the host registry for host %s", name)
	return nil, errors.New("tripwire")
}

func (r tripwireRegistry) Lease(name string) (flintlock.HostClient, func(), error) {
	r.t.Errorf("the executor leased host %s from the host registry", name)
	return nil, nil, errors.New("tripwire")
}

func (tripwireRegistry) Endpoint(string) (flintlock.Endpoint, bool) {
	return flintlock.Endpoint{}, false
}
func (tripwireRegistry) Names() []string                                   { return nil }
func (tripwireRegistry) Apply(context.Context, []flintlock.Endpoint) error { return nil }
func (tripwireRegistry) Close() error                                      { return nil }

// hostNode is a Host's Node as the Host Agent publishes it (KF-194): one
// annotation per enabled Host Service, `address:port` on the bridge
// gateway, beside annotations of other owners.
func hostNode(name string, services map[string]string) *corev1.Node {
	annotations := map[string]string{"unrelated.example.com/note": "x"}
	for service, addr := range services {
		annotations[kubelabels.HostServiceAnnotation(service)] = addr
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The Executor SHALL read the Host Service addresses for a Job
//# from the annotations of KF-194 that the Host Agent publishes on the Node
//# of the Job's Host.

// TestHostNodeAddressForms reads the Host Services of the named Node and of
// no other, in the forms a provisioned Host's Inventory entry has: buildkit
// over tcp, the rest over http, and the HTTP cache once per configured
// upstream. A service whose annotation is malformed gets no address.
func TestHostNodeAddressForms(t *testing.T) {
	t.Parallel()
	cfg := hostServicesConfig()
	nodes := kubefake.NewClientset(
		hostNode("host-7", map[string]string{
			ServiceBuildkit:       "10.200.0.1:1234",
			ServiceGoProxy:        "10.200.0.1:3000",
			ServiceRegistryMirror: "not an address",
			ServiceHTTPCache:      "10.200.0.1:3128",
		}),
		hostNode("host-8", map[string]string{ServiceBuildkit: "10.200.0.9:1234"}),
	).CoreV1().Nodes()
	inv := NewHostNodeInventory(nodes, cfg.HTTPCache.Upstreams, nil)

	entry, ok := inv.Host("host-7")
	if !ok {
		t.Fatal("host-7's node was not read")
	}
	s := entry.Services
	if s.Buildkit != "tcp://10.200.0.1:1234" || s.GoProxy != "http://10.200.0.1:3000" {
		t.Errorf("buildkit, go proxy = %q, %q", s.Buildkit, s.GoProxy)
	}
	if s.RegistryMirror != "" {
		t.Errorf("registry mirror = %q from a malformed annotation, want none", s.RegistryMirror)
	}
	if s.HTTPCache["nodejs"] != "http://10.200.0.1:3128/nodejs" || s.HTTPCache["pypi"] != "http://10.200.0.1:3128/pypi" || len(s.HTTPCache) != 2 {
		t.Errorf("http cache = %v, want one URL per configured upstream", s.HTTPCache)
	}

	entry, ok = inv.Host("host-8")
	if !ok || entry.Services.Buildkit != "tcp://10.200.0.9:1234" || entry.Services.GoProxy != "" {
		t.Errorf("host-8 = %+v, %t; want only host-8's buildkit", entry, ok)
	}
}
