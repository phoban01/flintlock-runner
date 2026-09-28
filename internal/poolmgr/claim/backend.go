package claim

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/pkg/claimclient"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// defaultCallDeadline bounds every API call, and a claim's wait to bind,
// when Options.Deadline is zero.
const defaultCallDeadline = config.DefaultPoolManagerDeadline

// Options is what New needs. Config, Namespace, RunnerName and Holder are
// required.
type Options struct {
	// Config is the Kubernetes client configuration.
	Config *rest.Config
	// Namespace is the Kubernetes namespace of the Runner's Pools, its
	// claims and the Holder.
	Namespace string
	// RunnerName labels every Pool and claim. Instances of one Runner share
	// a name and so share their Pools.
	RunnerName string
	// RunnerNamespace is the Runner namespace (CF-051) of a Pool that
	// carries no namespace annotation. It is empty when every Pool is one
	// this backend declared.
	RunnerNamespace string
	// Holder is the ServiceAccount, in Namespace, that every claim names
	// and that claim tokens are requested for (KF-150).
	Holder string
	// ServingCA names the ConfigMap that holds battery-operator's serving
	// CA, which the Client Library verifies an Exec Agent against. It is
	// read on the first claim.
	ServingCA config.ServingCAConfigMap
	// Profiles are the Profiles Pools are declared from. A PoolSpec names
	// Hosts, not an architecture and a Host selector, so the backend reads
	// them here; SetProfiles replaces them on a configuration reload.
	Profiles []config.Profile
	// Deadline bounds every API call, as the battery client bounds every
	// unary call (PL-002). It also bounds how long ClaimVM waits for a claim
	// that neither binds nor says why it cannot.
	Deadline time.Duration
	// Clock stamps events.
	Clock clock.Clock
	// Log receives the backend's notices.
	Log *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Deadline <= 0 {
		o.Deadline = defaultCallDeadline
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return o
}

// Backend is the claim pool backend. It is safe for concurrent use.
type Backend struct {
	opts Options
	log  *slog.Logger
	// kube reaches the API server. claimclient's Client gets gated, which
	// refuses the calls a held claim makes on its own once the backend is
	// closed.
	kube  client.WithWatch
	gated *gatedClient

	hub *hub

	// stop ends the watch; done is closed when it has returned.
	stop      context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	profiles map[string]config.Profile
	// claims is the Client Library's client, built on the first claim,
	// once the serving CA has been read.
	claims *claimclient.Client
	// held are the claims the Runner holds, by lease id, which is the
	// claim's name.
	held map[string]*claimclient.Claim
}

//= docs/requirements/12-cluster-fleet.md#battery-claims
//# The claim backend SHALL implement the `poolmgr.Client`
//# interface, so that the Scheduler's requirements in `03-scheduler.md` hold
//# unchanged over it.

// Compile-time interface check: the Scheduler, the Declarer, the Tracker and
// Health take this backend wherever they take the battery client.
var _ poolmgr.Client = (*Backend)(nil)

// Scheme knows client-go's types and battery-operator's v1alpha1, which is
// what the Client Library needs of its client.
func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := batteryv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

// New builds the backend and starts its watch on the Runner's Pools. Like
// the battery client it does not fail because the API server is down: the
// watch keeps retrying, the first call reports poolmgr.ErrUnavailable and
// Health drives the retry (PL-004). Close stops the watch.
func New(opts Options) (*Backend, error) {
	opts = opts.withDefaults()
	switch {
	case opts.Config == nil:
		return nil, errors.New("claim: a Kubernetes client configuration is required")
	case opts.Namespace == "":
		return nil, errors.New("claim: a namespace is required")
	case opts.RunnerName == "":
		return nil, errors.New("claim: a runner name is required")
	case opts.Holder == "":
		return nil, errors.New("claim: a holder service account is required")
	}
	scheme, err := Scheme()
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	kube, err := client.NewWithWatch(opts.Config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	b := &Backend{
		opts:     opts,
		log:      opts.Log.With("component", "poolmgr-claim", "namespace", opts.Namespace),
		kube:     kube,
		gated:    &gatedClient{Client: kube},
		done:     make(chan struct{}),
		profiles: profilesByName(opts.Profiles),
		held:     map[string]*claimclient.Claim{},
	}
	b.hub = newHub(b)

	ctx, stop := context.WithCancel(context.Background())
	b.stop = stop
	go func() {
		defer close(b.done)
		b.hub.watch(ctx)
	}()
	return b, nil
}

// Close implements poolmgr.Client. It stops the watch, ends every open
// EventStream with poolmgr.ErrUnavailable, and stops the renewal of every
// claim still held. It deletes nothing: Pools and Leases outlive the Runner,
// and a claim no longer renewed expires, as a battery Lease does when its
// Runner stops.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		b.gated.closed.Store(true)
		b.stop()
		<-b.done
		b.hub.close()
	})
	return nil
}

// SetProfiles replaces the Profiles on a configuration reload. It is called
// before the Scheduler declares the Pools again, so that a Profile whose
// architecture or Host selector changed is declared with the new one. A
// Profile that is gone keeps its Pool, as a Pool at battery is kept
// (PL-015).
func (b *Backend) SetProfiles(profiles []config.Profile) {
	next := profilesByName(profiles)
	b.mu.Lock()
	b.profiles = next
	b.mu.Unlock()
}

// Held returns the claim the Runner holds under a lease id. The Guest
// Transport dials the claim's Exec Agent through it.
func (b *Backend) Held(leaseID string) (*claimclient.Claim, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, ok := b.held[leaseID]
	return cl, ok
}

// profile returns the Profile of that name.
func (b *Backend) profile(name string) (config.Profile, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.profiles[name]
	return p, ok
}

func profilesByName(profiles []config.Profile) map[string]config.Profile {
	out := make(map[string]config.Profile, len(profiles))
	for _, p := range profiles {
		out[p.Name] = p
	}
	return out
}

// claimClient returns the Client Library's client, building it on first
// use. It is built late because it needs the serving CA, and reading the CA
// needs the API server, which New does not.
func (b *Backend) claimClient(ctx context.Context) (*claimclient.Client, error) {
	b.mu.Lock()
	cl := b.claims
	b.mu.Unlock()
	if cl != nil {
		return cl, nil
	}
	pool, err := b.servingCA(ctx)
	if err != nil {
		return nil, err
	}
	cl, err = claimclient.New(claimclient.Config{
		Client:    b.gated,
		Namespace: b.opts.Namespace,
		ServingCA: pool,
	})
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claims == nil {
		b.claims = cl
	}
	return b.claims, nil
}

// servingCA reads battery-operator's serving CA from the ConfigMap the
// configuration names.
func (b *Backend) servingCA(ctx context.Context) (*x509.CertPool, error) {
	ref := b.opts.ServingCA
	ctx, cancel := b.call(ctx)
	defer cancel()
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := b.kube.Get(ctx, key, cm); err != nil {
		op := fmt.Sprintf("reading the serving CA from ConfigMap %s", key)
		if apierrors.IsNotFound(err) {
			// Not ErrNotFound: that would make the Scheduler declare the
			// Pool again, and the Pool is not what is missing.
			return nil, fmt.Errorf("claim: %s: %w", op, err)
		}
		return nil, translate(op, err)
	}
	data := cm.Data[ref.Key]
	pool := x509.NewCertPool()
	if data == "" || !pool.AppendCertsFromPEM([]byte(data)) {
		return nil, fmt.Errorf("claim: ConfigMap %s holds no PEM certificate under %q", key, ref.Key)
	}
	return pool, nil
}

// call bounds one API call with the configured deadline (PL-002).
func (b *Backend) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, b.opts.Deadline)
}

// translate maps a Kubernetes API error onto the package's sentinel errors,
// as the battery client maps gRPC status codes, so that no caller imports
// apimachinery. A request the API server refuses for lack of permission is
// left as it is, because retrying it cannot help; anything else, a timeout
// or a connection error among them, is the API server being unavailable.
func translate(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("claim: %s: %w: %w", op, poolmgr.ErrNotFound, err)
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("claim: %s: %w: %w", op, poolmgr.ErrAlreadyExists, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("claim: %s: %w: %w", op, poolmgr.ErrInvalid, err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return fmt.Errorf("claim: %s: %w", op, err)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("claim: %s: %w", op, err)
	default:
		return fmt.Errorf("claim: %s: %w: %w", op, poolmgr.ErrUnavailable, err)
	}
}

// errClosed is what a held claim's own calls get once the backend is
// closed.
var errClosed = errors.New("claim: the backend is closed")

// gatedClient is the client the Client Library gets. A held claim renews
// itself and refreshes its token in the background, and the Client Library
// stops that only by deleting the claim. Once the backend is closed, the
// gate refuses those two calls, so a closed backend renews nothing, as a
// closed battery client heartbeats nothing.
type gatedClient struct {
	client.Client
	closed atomic.Bool
}

// Patch implements client.Client.
func (g *gatedClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if g.closed.Load() {
		return errClosed
	}
	return g.Client.Patch(ctx, obj, patch, opts...)
}

// SubResource implements client.Client.
func (g *gatedClient) SubResource(subResource string) client.SubResourceClient {
	return gatedSubResource{SubResourceClient: g.Client.SubResource(subResource), closed: &g.closed}
}

// gatedSubResource refuses to create a subresource, a claim token, once the
// backend is closed.
type gatedSubResource struct {
	client.SubResourceClient
	closed *atomic.Bool
}

// Create implements client.SubResourceClient.
func (g gatedSubResource) Create(ctx context.Context, obj, sub client.Object, opts ...client.SubResourceCreateOption) error {
	if g.closed.Load() {
		return errClosed
	}
	return g.SubResourceClient.Create(ctx, obj, sub, opts...)
}
