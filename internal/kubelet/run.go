package kubelet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

const (
	// informerResync is the full resync period of the pod and node
	// informers.
	informerResync = time.Minute
	// drainResync is how often the drainer forgets what it did and checks
	// the API server again.
	drainResync = time.Minute
	// podWorkers is the number of pods reconciled at once. A Host runs tens
	// of MicroVMs, and creating one is a quick call to a local socket.
	podWorkers = 4
	// startRetry is the wait between attempts at the steps of starting that
	// need the API server or flintlockd to answer.
	startRetry = 2 * time.Second
)

// Options are everything Run needs. Config, Kube and Host are required.
type Options struct {
	Config *Config
	// Kube is the API server client, holding no more than
	// deploy/host-agent/rbac.yaml grants (KF-111).
	Kube kubernetes.Interface
	// Host is the local flintlockd (KF-018); DialLocal makes one.
	Host flintlock.PoolHostClient
	// Logger defaults to discarding.
	Logger *slog.Logger
	// Clock defaults to the real one.
	Clock clock.Clock
	// Version is reported as the Virtual Node's kubelet version.
	Version string
	// Listener, when set, is served instead of listening on Config.Listen.
	// It is plain TCP; Run wraps it in TLS. Tests use it to learn the port.
	Listener net.Listener
	// Ready, when set, is closed once the Virtual Node is registered and
	// both controllers run.
	Ready chan<- struct{}
}

// Run is the Pod Provider: it registers the Host's Virtual Node, serves the
// kubelet API and realises the pods bound to the node as MicroVMs until ctx
// ends. Stopping leaves every MicroVM, pod and the guard exactly as they
// are (KF-029); the next Run adopts them.
func Run(ctx context.Context, opts Options) error {
	cfg := opts.Config
	if cfg == nil || opts.Kube == nil || opts.Host == nil {
		return errors.New("kubelet: Run needs a configuration, a Kubernetes client and a flintlockd client")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tlsConfig, err := kubeletTLSConfig(cfg.TLS)
	if err != nil {
		return err
	}
	listener := opts.Listener
	if listener == nil {
		if listener, err = net.Listen("tcp", cfg.Listen); err != nil {
			return fmt.Errorf("kubelet: listening on %s: %w", cfg.Listen, err)
		}
	}
	defer func() { _ = listener.Close() }()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil {
		return err
	}

	hostNode, err := awaitHostNode(ctx, opts.Kube, cfg.HostNode, log)
	if err != nil {
		return err
	}
	vnode, err := buildVirtualNode(cfg, hostNode, int32(port), opts.Version)
	if err != nil {
		return err
	}

	provider := &Provider{
		cfg:            cfg,
		log:            log,
		clk:            clk,
		host:           opts.Host,
		kube:           opts.Kube,
		transports:     transport.NewFactory(transport.WithClock(clk), transport.WithLogger(log)),
		nodeName:       vnode.Name,
		hostIP:         hostInternalIP(hostNode),
		deleteDeadline: defaultDeleteDeadline,
		pods:           map[string]*podRecord{},
	}

	// Adoption comes before the pod controller, and is retried until both
	// flintlockd and the API server have answered: while either is silent
	// nothing is known, so nothing is created and nothing deleted (KF-028,
	// KF-029).
	for {
		err := provider.adopt(ctx)
		if err == nil {
			break
		}
		log.Warn("could not adopt the host's microvms yet", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clk.After(startRetry):
		}
	}

	// The pod informer sees only the pods bound to the Virtual Node, and the
	// node informer only the Host's Node. The pod controller insists on
	// informers for Secrets, ConfigMaps and Services; they come from a
	// factory that is never started, so they list nothing and the provider
	// needs no permission on them (KF-111). Downward API resolution, their
	// only use, is off: a MicroVM has no environment to resolve into.
	podInformers := informers.NewSharedInformerFactoryWithOptions(opts.Kube, informerResync, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", vnode.Name).String()
	}))
	unused := informers.NewSharedInformerFactory(opts.Kube, 0)
	nodeInformers := informers.NewSharedInformerFactoryWithOptions(opts.Kube, informerResync,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", cfg.HostNode).String()
		}))
	podInformer := podInformers.Core().V1().Pods()
	hostInformer := nodeInformers.Core().V1().Nodes()

	broadcaster := record.NewBroadcaster()
	defer broadcaster.Shutdown()
	broadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: opts.Kube.CoreV1().Events(corev1.NamespaceAll)})

	podController, err := node.NewPodController(node.PodControllerConfig{
		PodClient:                 opts.Kube.CoreV1(),
		PodInformer:               podInformer,
		EventRecorder:             broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: path.Join(vnode.Name, "pod-controller")}),
		Provider:                  provider,
		ConfigMapInformer:         unused.Core().V1().ConfigMaps(),
		SecretInformer:            unused.Core().V1().Secrets(),
		ServiceInformer:           unused.Core().V1().Services(),
		SkipDownwardAPIResolution: true,
	})
	if err != nil {
		return fmt.Errorf("kubelet: building the pod controller: %w", err)
	}

	nodes := &nodeProvider{node: vnode.DeepCopy()}
	nodeController, err := node.NewNodeController(nodes, vnode, opts.Kube.CoreV1().Nodes(),
		node.WithNodeEnableLeaseV1(opts.Kube.CoordinationV1().Leases(corev1.NamespaceNodeLease), node.DefaultLeaseDuration))
	if err != nil {
		return fmt.Errorf("kubelet: building the node controller: %w", err)
	}

	server := &http.Server{
		Handler:           provider.kubeletHandler(podInformer.Lister()),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go func() { _ = server.Serve(tls.NewListener(listener, tlsConfig)) }()
	defer func() { _ = server.Close() }()

	kick := make(chan struct{}, 1)
	if _, err := hostInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { wake(kick) },
		UpdateFunc: func(any, any) { wake(kick) },
	}); err != nil {
		return fmt.Errorf("kubelet: watching the host's node: %w", err)
	}

	podInformers.Start(ctx.Done())
	nodeInformers.Start(ctx.Done())
	go func() { _ = podController.Run(ctx, podWorkers) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-podController.Done():
		return fmt.Errorf("kubelet: the pod controller stopped: %w", podController.Err())
	case <-podController.Ready():
	}
	go func() { _ = nodeController.Run(ctx) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-nodeController.Done():
		return fmt.Errorf("kubelet: the node controller stopped: %w", nodeController.Err())
	case <-nodeController.Ready():
	}
	if err := ensureVirtualNodeSpec(ctx, opts.Kube, vnode); err != nil {
		return err
	}
	log.Info("registered the virtual node", "virtual_node", vnode.Name, "host_node", cfg.HostNode,
		"cpu", vnode.Status.Capacity.Cpu().String(), "memory", vnode.Status.Capacity.Memory().String(), "pods", cfg.MaxMicroVMs)
	if opts.Ready != nil {
		close(opts.Ready)
	}

	drain := &drainer{
		cfg: cfg, log: log, clk: clk, kube: opts.Kube, provider: provider,
		hostNode: cfg.HostNode, nodeName: vnode.Name, resync: drainResync,
		host: func() (*corev1.Node, error) { return hostInformer.Lister().Get(cfg.HostNode) },
	}
	for {
		nodes.setReady(checkReadiness(ctx, cfg, opts.Host))
		provider.syncPods(ctx)
		provider.expireLeases(ctx)
		provider.sweepOrphans(ctx)
		if err := drain.reconcile(ctx); err != nil && ctx.Err() == nil {
			log.Warn("could not reconcile the drain state", "error", err)
		}
		select {
		case <-ctx.Done():
			// Stopping is just stopping: no MicroVM, pod or guard is
			// touched on the way out (KF-029).
			return nil
		case <-podController.Done():
			return fmt.Errorf("kubelet: the pod controller stopped: %w", podController.Err())
		case <-nodeController.Done():
			return fmt.Errorf("kubelet: the node controller stopped: %w", nodeController.Err())
		case <-kick:
		case <-clk.After(cfg.SyncInterval):
		}
	}
}

// wake requests a reconcile without blocking.
func wake(kick chan<- struct{}) {
	select {
	case kick <- struct{}{}:
	default:
	}
}

// awaitHostNode reads the Host's Node, waiting for it to exist: the provider
// can start before the Host's kubelet has registered.
func awaitHostNode(ctx context.Context, kube kubernetes.Interface, name string, log *slog.Logger) (*corev1.Node, error) {
	for {
		host, err := kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return host, nil
		}
		log.Warn("could not read the host's node yet", "host_node", name, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(startRetry):
		}
	}
}

// ensureVirtualNodeSpec gives a Virtual Node that already existed the owner
// and the taint a new one is created with (KF-011, KF-014). The node
// controller only maintains status, labels and annotations, so a node
// registered by an earlier version, or edited since, is repaired here.
// Taints others put there, such as the cordon's, are kept.
func ensureVirtualNodeSpec(ctx context.Context, kube kubernetes.Interface, want *corev1.Node) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		have, err := kube.CoreV1().Nodes().Get(ctx, want.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("kubelet: reading the virtual node: %w", err)
		}
		changed := false
		if !ownedBy(have, want.OwnerReferences[0]) {
			have.OwnerReferences = want.OwnerReferences
			changed = true
		}
		if !tainted(have, want.Spec.Taints[0]) {
			have.Spec.Taints = append(have.Spec.Taints, want.Spec.Taints[0])
			changed = true
		}
		if !changed {
			return nil
		}
		_, err = kube.CoreV1().Nodes().Update(ctx, have, metav1.UpdateOptions{})
		return err
	})
}

func ownedBy(n *corev1.Node, owner metav1.OwnerReference) bool {
	for _, ref := range n.OwnerReferences {
		if ref.UID == owner.UID {
			return true
		}
	}
	return false
}

func tainted(n *corev1.Node, taint corev1.Taint) bool {
	for _, t := range n.Spec.Taints {
		if t.MatchTaint(&taint) {
			return true
		}
	}
	return false
}
