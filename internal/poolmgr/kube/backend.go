package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// Defaults for the settings an Options leaves zero.
const (
	defaultCallDeadline    = 10 * time.Second
	defaultLeaseExpiry     = config.DefaultHeartbeatExpiry
	defaultJobTimeout      = config.DefaultKubernetesJobTimeout
	defaultCleanupMargin   = config.DefaultKubernetesCleanupMargin
	defaultRolloutInterval = config.DefaultKubernetesRolloutInterval
	// informerResync is how often the informers replay their cache, which
	// bounds how long a missed transition can stay missed.
	informerResync = 10 * time.Minute
)

// Options is what New needs. Client, Namespace and RunnerName are required.
type Options struct {
	// Client is the Kubernetes API client. The backend uses it only within
	// the Role of deploy/runner/role.yaml (KF-110).
	Client kubernetes.Interface
	// Namespace is the Kubernetes namespace that holds the Runner's
	// ReplicaSets and pods (KF-040).
	Namespace string
	// RunnerName labels every ReplicaSet and pod (KF-043). Instances of one
	// Runner share a name and so share their Pools.
	RunnerName string
	// Profiles are the Profiles Pools are declared from. A PoolSpec carries
	// no architecture and no Host selector, so the backend reads them here
	// (KF-041); SetProfiles replaces them on a configuration reload.
	Profiles []config.Profile
	// CloudInitConfigMaps names, per Profile, the ConfigMap with its
	// cloud-init user data (KF-023).
	CloudInitConfigMaps map[string]string
	// JobTimeout is the active deadline of a claimed pod when the claim's
	// context carries no Job timeout (KF-044, KF-127).
	JobTimeout time.Duration
	// CleanupMargin is added to the Job timeout a claim's context carries,
	// so that the active deadline outlasts the Job (KF-127).
	CleanupMargin time.Duration
	// RolloutInterval is the least time between two deletions of idle pods
	// of a previous template, per Pool (KF-050).
	RolloutInterval time.Duration
	// Deadline is applied to every API call, as the battery client applies
	// one to every unary call (PL-002).
	Deadline time.Duration
	// Clock stamps leases and paces the rollout.
	Clock clock.Clock
	// Log receives the backend's notices.
	Log *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.JobTimeout <= 0 {
		o.JobTimeout = defaultJobTimeout
	}
	if o.CleanupMargin <= 0 {
		o.CleanupMargin = defaultCleanupMargin
	}
	if o.RolloutInterval <= 0 {
		o.RolloutInterval = defaultRolloutInterval
	}
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

// Backend is the Kubernetes pool backend. It is safe for concurrent use.
type Backend struct {
	opts Options
	log  *slog.Logger

	pods        corelisters.PodNamespaceLister
	replicaSets appslisters.ReplicaSetNamespaceLister
	synced      []cache.InformerSynced

	hub *hub

	// stop ends the informers and the rollout loop; done is closed when the
	// rollout loop has returned.
	stop      context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	profiles map[string]config.Profile
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# The Kubernetes pool backend SHALL implement the same
//# `poolmgr.Client` interface as the battery client, so that the Scheduler's
//# requirements in `03-scheduler.md` hold unchanged over either.

// Compile-time interface check: the Scheduler, the Declarer, the Tracker and
// Health take this backend wherever they take the battery client.
var _ poolmgr.Client = (*Backend)(nil)

// New builds the backend and starts its watch on the Runner's ReplicaSets
// and pods and its rollout loop. Like the battery client it does not fail
// because the API server is down: the informers keep retrying, the first
// call reports poolmgr.ErrUnavailable and Health drives the retry (PL-004).
// Close stops everything New started.
func New(opts Options) (*Backend, error) {
	opts = opts.withDefaults()
	switch {
	case opts.Client == nil:
		return nil, errors.New("kube: a Kubernetes client is required")
	case opts.Namespace == "":
		return nil, errors.New("kube: a namespace is required")
	case opts.RunnerName == "":
		return nil, errors.New("kube: a runner name is required")
	}
	b := &Backend{
		opts: opts,
		log:  opts.Log.With("component", "poolmgr-kube", "namespace", opts.Namespace),
		done: make(chan struct{}),
	}
	b.hub = newHub(b)
	b.profiles = profilesByName(opts.Profiles)

	ctx, stop := context.WithCancel(context.Background())
	b.stop = stop

	selector := b.runnerSelector().String()
	factory := informers.NewSharedInformerFactoryWithOptions(opts.Client, informerResync,
		informers.WithNamespace(opts.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = selector }),
	)
	podInformer := factory.Core().V1().Pods()
	rsInformer := factory.Apps().V1().ReplicaSets()
	b.pods = podInformer.Lister().Pods(opts.Namespace)
	b.replicaSets = rsInformer.Lister().ReplicaSets(opts.Namespace)
	b.synced = []cache.InformerSynced{podInformer.Informer().HasSynced, rsInformer.Informer().HasSynced}

	if _, err := podInformer.Informer().AddEventHandler(b.hub.podHandler()); err != nil {
		stop()
		return nil, fmt.Errorf("kube: watching pods: %w", err)
	}
	if _, err := rsInformer.Informer().AddEventHandler(b.hub.replicaSetHandler()); err != nil {
		stop()
		return nil, fmt.Errorf("kube: watching replicasets: %w", err)
	}
	factory.Start(ctx.Done())

	go func() {
		defer close(b.done)
		b.runRollout(ctx)
		factory.Shutdown()
	}()
	return b, nil
}

// WaitForSync blocks until the first list of the Runner's ReplicaSets and
// pods has arrived, or ctx ends. Nothing needs it for correctness: a claim
// that finds nothing in an unsynced cache asks the API server before it
// reports exhaustion.
func (b *Backend) WaitForSync(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), b.synced...) {
		return fmt.Errorf("%w: the watch on %s has not synced: %w", poolmgr.ErrUnavailable, b.opts.Namespace, ctx.Err())
	}
	return nil
}

// Close implements poolmgr.Client. It stops the watch and the rollout loop
// and ends every open EventStream with poolmgr.ErrUnavailable. It deletes
// nothing: Pools and Leases outlive the Runner.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		b.stop()
		<-b.done
		b.hub.close()
	})
	return nil
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# When a Profile is removed by a configuration reload, the
//# Scheduler SHALL NOT delete its ReplicaSet and SHALL log that the Pool is
//# no longer referenced.

// SetProfiles replaces the Profiles on a configuration reload. It is called
// before the Scheduler declares the Pools again, so that a Profile whose
// architecture or Host selector changed is declared with the new one. A
// Profile that is gone keeps its ReplicaSet, as a Pool at battery is kept
// (PL-015): deleting it would delete warm MicroVMs another instance of the
// Runner, still on the previous configuration, may be about to claim, and an
// operator who wants it gone deletes one ReplicaSet. The backend only says
// so, once per removal, and stops rolling the Pool's pods.
func (b *Backend) SetProfiles(profiles []config.Profile) {
	next := profilesByName(profiles)
	b.mu.Lock()
	previous := b.profiles
	b.profiles = next
	b.mu.Unlock()

	for name := range previous {
		if _, kept := next[name]; kept {
			continue
		}
		b.log.Info("pool is no longer referenced by any profile, its replicaset is left in place",
			"profile", name, "selector", labelsString(b.poolLabels(name)))
	}
}

// profile returns the Profile of that name.
func (b *Backend) profile(name string) (config.Profile, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.profiles[name]
	return p, ok
}

// referenced reports whether a Profile label value belongs to a Profile of
// the current configuration.
func (b *Backend) referenced(profileLabel string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name := range b.profiles {
		if labelValue(name) == profileLabel {
			return true
		}
	}
	return false
}

func profilesByName(profiles []config.Profile) map[string]config.Profile {
	out := make(map[string]config.Profile, len(profiles))
	for _, p := range profiles {
		out[p.Name] = p
	}
	return out
}

//= docs/requirements/12-cluster-fleet.md#kube-allocation
//# the backend SHALL set the
//# active deadline of KF-044 to that timeout plus the configured cleanup
//# margin, using its configured default only for a Job that has none.

// jobTimeout is the active deadline of a claim: the Job timeout the claim's
// context carries (poolmgr.WithJobTimeout) plus the cleanup margin, or the
// configured default for a claim that carries none. The margin is added
// because the deadline must outlast the Job: the claim is made before the
// Job's own clock starts, and a Job that times out still runs its
// after_script and failure uploads in the same MicroVM.
func (b *Backend) jobTimeout(ctx context.Context) time.Duration {
	if d, ok := poolmgr.JobTimeoutFrom(ctx); ok {
		return d + b.opts.CleanupMargin
	}
	return b.opts.JobTimeout
}

// call bounds one API call with the configured deadline (PL-002).
func (b *Backend) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, b.opts.Deadline)
}

// translate maps a Kubernetes API error onto the package's sentinel errors,
// as the battery client maps gRPC status codes, so that no caller imports
// apimachinery. Conflicts are not translated: the claim and the rollout
// handle them where they arise. A request the API server refuses for lack
// of permission is left as it is, because retrying it cannot help; anything
// else, a timeout or a connection error among them, is the API server being
// unavailable.
func translate(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("kube: %s: %w: %w", op, poolmgr.ErrNotFound, err)
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("kube: %s: %w: %w", op, poolmgr.ErrAlreadyExists, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("kube: %s: %w: %w", op, poolmgr.ErrInvalid, err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return fmt.Errorf("kube: %s: %w", op, err)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("kube: %s: %w", op, err)
	default:
		return fmt.Errorf("kube: %s: %w: %w", op, poolmgr.ErrUnavailable, err)
	}
}
