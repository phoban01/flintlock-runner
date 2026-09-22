package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// ClaimResource says where a claim resource is served and in which fields
// a claim records what the Exec Agent reads of it. It exists because
// battery has not settled either; DynamicClaims reads whatever resource one
// describes, as long as each fact is one field.
type ClaimResource struct {
	// Group, Version and Resource name the resource.
	Group    string
	Version  string
	Resource string
	// PhaseField, VMUIDField, HostNodeField and ExpiresAtField are the
	// paths of the phase, the MicroVM's uid, the Host's node name and the
	// lease expiry, an RFC 3339 time, in the object.
	PhaseField     []string
	VMUIDField     []string
	HostNodeField  []string
	ExpiresAtField []string
	// CreatorAnnotation is the annotation that records the user name of
	// the identity that created the claim.
	CreatorAnnotation string
}

// ProvisionalClaimResource is the test definition of the claim resource,
// internal/agent/testdata/crds/microvmclaims.yaml. PROVISIONAL: every name
// in it is this project's stand-in for battery's MicroVMClaim, chosen to
// be unmistakable for the real one, and is replaced when battery publishes
// its resource. The package documentation lists the fields.
var ProvisionalClaimResource = ClaimResource{
	Group:             "claims.test.flintlock-runner.dev",
	Version:           "v1alpha1",
	Resource:          "microvmclaims",
	PhaseField:        []string{"status", "phase"},
	VMUIDField:        []string{"status", "microVM", "uid"},
	HostNodeField:     []string{"status", "host", "nodeName"},
	ExpiresAtField:    []string{"status", "leaseExpiresAt"},
	CreatorAnnotation: "claims.test.flintlock-runner.dev/creator",
}

// GroupVersionResource is the resource's GVR.
func (r ClaimResource) GroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Resource}
}

// Index names of DynamicClaims' cache.
const (
	indexVM   = "vm"
	indexHost = "host"
)

// errNotSynced is a lookup made before the claims were first listed.
var errNotSynced = errors.New("the claims have not been listed yet")

// DynamicClaims is the provisional ClaimLookup. It keeps every claim of the
// cluster in a cache indexed by MicroVM uid and by Host, from one list and
// watch, and reads each candidate claim afresh from the API server before
// it answers ClaimsForVM, so that a claim released a moment ago is not
// authorized from a cache that has not heard yet. RBAC: get, list and watch
// on the resource.
type DynamicClaims struct {
	res      ClaimResource
	client   dynamic.Interface
	informer cache.SharedIndexInformer
}

// NewDynamicClaims builds the lookup. Run starts its cache.
func NewDynamicClaims(client dynamic.Interface, res ClaimResource, resync time.Duration) *DynamicClaims {
	d := &DynamicClaims{res: res, client: client}
	indexers := cache.Indexers{
		indexVM:   d.indexBy(res.VMUIDField),
		indexHost: d.indexBy(res.HostNodeField),
	}
	d.informer = dynamicinformer.NewFilteredDynamicInformer(client, res.GroupVersionResource(),
		metav1.NamespaceAll, resync, indexers, nil).Informer()
	return d
}

// Run keeps the cache until ctx ends.
func (d *DynamicClaims) Run(ctx context.Context) { d.informer.Run(ctx.Done()) }

// HasSynced reports whether the claims have been listed once.
func (d *DynamicClaims) HasSynced() bool { return d.informer.HasSynced() }

// indexBy indexes a claim by the string at path.
func (d *DynamicClaims) indexBy(path []string) cache.IndexFunc {
	return func(obj any) ([]string, error) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return nil, nil
		}
		v, _, _ := unstructured.NestedString(u.Object, path...)
		if v == "" {
			return nil, nil
		}
		return []string{v}, nil
	}
}

// ClaimsForVM implements ClaimLookup: the cache says which claims to read,
// and each is read from the API server.
func (d *DynamicClaims) ClaimsForVM(ctx context.Context, vmUID string) ([]Claim, error) {
	if !d.informer.HasSynced() {
		return nil, errNotSynced
	}
	cached, err := d.informer.GetIndexer().ByIndex(indexVM, vmUID)
	if err != nil {
		return nil, err
	}
	var out []Claim
	for _, obj := range cached {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		live, err := d.client.Resource(d.res.GroupVersionResource()).Namespace(u.GetNamespace()).Get(ctx, u.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading claim %s/%s: %w", u.GetNamespace(), u.GetName(), err)
		}
		c := d.parse(live)
		if c.VMUID == vmUID {
			out = append(out, c)
		}
	}
	return out, nil
}

// BoundOnHost implements ClaimLookup from the cache alone: the drain guard
// it serves is reconciled again a moment later, so a cache a moment behind
// costs nothing.
func (d *DynamicClaims) BoundOnHost(_ context.Context, hostNode string) ([]Claim, error) {
	if !d.informer.HasSynced() {
		return nil, errNotSynced
	}
	cached, err := d.informer.GetIndexer().ByIndex(indexHost, hostNode)
	if err != nil {
		return nil, err
	}
	var out []Claim
	for _, obj := range cached {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if c := d.parse(u); c.Phase == ClaimBound && c.HostNode == hostNode {
			out = append(out, c)
		}
	}
	return out, nil
}

// parse reads a claim's facts. A field that is missing or malformed is
// left zero, which Authorize refuses.
func (d *DynamicClaims) parse(u *unstructured.Unstructured) Claim {
	str := func(path []string) string {
		v, _, _ := unstructured.NestedString(u.Object, path...)
		return v
	}
	c := Claim{
		Namespace: u.GetNamespace(),
		Name:      u.GetName(),
		Phase:     ClaimPhase(str(d.res.PhaseField)),
		VMUID:     str(d.res.VMUIDField),
		HostNode:  str(d.res.HostNodeField),
		Creator:   u.GetAnnotations()[d.res.CreatorAnnotation],
	}
	if t, err := time.Parse(time.RFC3339, str(d.res.ExpiresAtField)); err == nil {
		c.ExpiresAt = t
	}
	return c
}

var _ ClaimLookup = (*DynamicClaims)(nil)
