package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/kubelet"
	"github.com/phoban01/flintlock-runner/internal/kubelet/kubelettest"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube/kubetest"
)

// Backend selects the Pool backend a Stack runs the Runner over.
type Backend string

const (
	// BackendBattery is the fake Pool Manager over gRPC and fake Hosts the
	// Runner dials (04-pool-manager.md). It is the zero value.
	BackendBattery Backend = ""
	// BackendKubernetes is the cluster stack (KF-122): the Runner with
	// pool_manager.backend kubernetes and the kube-exec Guest Transport, an
	// API server test environment with a test reconciler for the ReplicaSet
	// controller and a stand-in for the scheduler, and on each fake Host the
	// real Pod Provider under the Host Agent's shipped RBAC and admission
	// policy.
	BackendKubernetes Backend = "kubernetes"
)

// String names the backend as a scenario's subtest does.
func (b Backend) String() string {
	if b == BackendKubernetes {
		return "kubernetes"
	}
	return "battery"
}

// Settings of the Kubernetes stack.
const (
	// DefaultProviderLeaseDuration is the Pod Providers' lease duration
	// (KF-032) when Options leaves it zero: long enough that a heartbeating
	// Runner never loses a Lease to it.
	DefaultProviderLeaseDuration = time.Minute
	// kubeHostAddress is the internal address of every Host's Node, which
	// the API server dials the Pod Provider's kubelet endpoint on, and the
	// bridge gateway the Host Services listen on.
	kubeHostAddress = "127.0.0.1"
	// kubeSyncInterval paces the Pod Providers and the stand-ins.
	kubeSyncInterval = 50 * time.Millisecond
	// providerStopTimeout bounds a Pod Provider's return after its context
	// ends.
	providerStopTimeout = 30 * time.Second
	// poolDrainTimeout bounds the wait, at shutdown, for the pods of the
	// deleted Pools to go, which is the Pod Providers deleting their
	// MicroVMs (KF-027).
	poolDrainTimeout = 60 * time.Second
)

// KubeHost is one Host of a Kubernetes stack: its Node, the fake flintlockd
// on it and the Pod Provider that registers its Virtual Node. The Pod
// Provider runs in the test process, as its Host's Host Agent: with the
// shipped RBAC, under the shipped admission policy, with an identity that
// names its Host (KF-111, KF-133, KF-134).
type KubeHost struct {
	// Node is the Host's own Node and VirtualNode the Pod Provider's.
	Node        string
	VirtualNode string
	// Fake is the fake flintlockd, also in Stack.Hosts.
	Fake *hostfake.Host

	cfg    *kubelet.Config
	agent  kubernetes.Interface
	logs   *syncBuffer
	logger *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
	conns  *trackingListener
}

// trackingListener remembers every connection it accepts, so that they can
// all be closed at once.
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

// Accept implements net.Listener.
func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

// closeAll closes the listener and every connection it accepted.
func (l *trackingListener) closeAll() {
	_ = l.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		_ = c.Close()
	}
	l.conns = nil
}

// Log is what the Pod Provider has logged so far, across restarts.
func (h *KubeHost) Log() string { return h.logs.String() }

// start runs the Pod Provider and waits until its Virtual Node is
// registered. The kubelet endpoint keeps its address across restarts, so
// that the Virtual Node's daemon endpoint stays right from the first moment
// of the next run.
func (h *KubeHost) start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		return fmt.Errorf("harness: the pod provider of %s is already running", h.Node)
	}
	var (
		listener net.Listener
		err      error
	)
	// The previous run's port can take a moment to be free again.
	for range 50 {
		if listener, err = net.Listen("tcp", h.cfg.Listen); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("harness: the pod provider of %s: %w", h.Node, err)
	}
	h.cfg.Listen = listener.Addr().String()
	tracked := &trackingListener{Listener: listener}
	listener = tracked
	h.conns = tracked
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- kubelet.Run(ctx, kubelet.Options{
			Config: h.cfg, Kube: h.agent, Host: h.Fake.Client(), Version: "harness",
			Listener: listener, Ready: ready, Logger: h.logger,
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		return fmt.Errorf("harness: the pod provider of %s stopped before it was ready: %w\n%s", h.Node, err, h.logs.String())
	case <-time.After(providerStopTimeout):
		cancel()
		<-done
		return fmt.Errorf("harness: the pod provider of %s was not ready in %s\n%s", h.Node, providerStopTimeout, h.logs.String())
	}
	h.cancel, h.done = cancel, done
	return nil
}

// stop ends the Pod Provider, which leaves every MicroVM, pod and guard as
// they are (KF-029).
func (h *KubeHost) stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel == nil {
		return nil
	}
	h.cancel()
	h.cancel = nil
	// A Pod Provider that stops is a process that exits: every connection
	// to its kubelet endpoint goes with it, the exec streams the API server
	// relays included, which an in-process server's shutdown would leave
	// open.
	h.conns.closeAll()
	select {
	case err := <-h.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("harness: the pod provider of %s: %w", h.Node, err)
		}
		return nil
	case <-time.After(providerStopTimeout):
		return fmt.Errorf("harness: the pod provider of %s did not stop in %s", h.Node, providerStopTimeout)
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The harness SHALL include a scenario in which the Pod Provider
//# restarts while a Job runs, and SHALL assert that the Job's MicroVM is
//# adopted and the Job finishes.

// Restart stops the Pod Provider and starts it again, as a rollout of the
// Host Agent's DaemonSet or a crash and restart of its container does.
func (h *KubeHost) Restart() error {
	if err := h.stop(); err != nil {
		return err
	}
	return h.start()
}

// kubeStack is what a Stack on BackendKubernetes runs besides the Runner
// and the fake GitLab.
type kubeStack struct {
	cluster *Cluster
	// namespace is where the Runner keeps its Pools (KF-040).
	namespace  string
	kubeconfig string
	hosts      []*KubeHost
	binder     *binder
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// KubeNamespace is the Kubernetes namespace the Runner's Pools live in; empty on
// the battery stack.
func (s *Stack) KubeNamespace() string {
	if s.kube == nil {
		return ""
	}
	return s.kube.namespace
}

// KubeHosts are the Hosts of a Kubernetes stack, in the order of Hosts.
func (s *Stack) KubeHosts() []*KubeHost {
	if s.kube == nil {
		return nil
	}
	return append([]*KubeHost(nil), s.kube.hosts...)
}

// Cluster is the API server test environment of a Kubernetes stack.
func (s *Stack) Cluster() *Cluster {
	if s.kube == nil {
		return nil
	}
	return s.kube.cluster
}

// PauseScheduling stops the scheduler stand-in from binding pods, so that a
// Pool's replacement pods stay pending.
func (s *Stack) PauseScheduling() { s.kube.binder.pause(true) }

// ResumeScheduling lets the pods PauseScheduling held back be bound.
func (s *Stack) ResumeScheduling() { s.kube.binder.pause(false) }

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The harness SHALL run every scenario of TD-051 over the
//# Kubernetes pool backend, the `kube-exec` Guest Transport, the Pod Provider
//# and the fake Host.

// startKube brings up the Kubernetes side of the stack on opts.Cluster: a
// namespace, the Runner's identity bound to its shipped Role there and
// written as a kubeconfig, the Hosts with their Pod Providers, the test
// reconciler that stands in for the ReplicaSet controller (KF-121) and a
// stand-in for the scheduler.
func (s *Stack) startKube(ctx context.Context) error {
	c := s.opts.Cluster
	if c == nil {
		return errors.New("harness: the kubernetes stack needs Options.Cluster")
	}
	if s.opts.Hardware() || s.opts.PoolManagerEndpoint != "" {
		return errors.New("harness: the kubernetes stack runs on fake Hosts and no Pool Manager")
	}
	id := c.next()
	k := &kubeStack{cluster: c, namespace: fmt.Sprintf("harness-%d", id)}
	s.kube = k
	if err := c.createNamespace(ctx, k.namespace); err != nil {
		return err
	}
	runner, err := c.runnerUser(ctx, k.namespace)
	if err != nil {
		return err
	}
	k.kubeconfig = filepath.Join(s.Root, "kubeconfig")
	if err := writeKubeconfig(k.kubeconfig, runner); err != nil {
		return err
	}
	s.logf("namespace %s on the API server at %s; the runner's kubeconfig is %s", k.namespace, runner.Host, k.kubeconfig)

	for i := 1; i <= s.opts.Hosts; i++ {
		h, err := s.newKubeHost(ctx, fmt.Sprintf("k%d-host-%d", id, i))
		if err != nil {
			return err
		}
		k.hosts = append(k.hosts, h)
		s.Hosts = append(s.Hosts, h.Fake)
		s.logf("Host %s: fake flintlockd (sandboxes under %s) and pod provider serving virtual node %s on %s",
			h.Node, h.Fake.SandboxRoot(), h.VirtualNode, h.cfg.Listen)
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	k.cancel = cancel
	nodes := make([]string, 0, len(k.hosts))
	for _, h := range k.hosts {
		nodes = append(nodes, h.VirtualNode)
	}
	k.binder = &binder{client: c.Admin(), namespace: k.namespace, nodes: nodes}
	replicaSets := &kubetest.ReplicaSets{Client: c.Admin(), Namespace: k.namespace, Interval: kubeSyncInterval}
	k.wg.Add(2)
	go func() { defer k.wg.Done(); replicaSets.Run(runCtx) }()
	go func() { defer k.wg.Done(); k.binder.run(runCtx) }()
	return nil
}

// newKubeHost creates a Host's Node at kubeHostAddress, a fake flintlockd
// behind it and a Pod Provider serving its kubelet endpoint on that
// address, and waits until the provider has registered the Virtual Node.
func (s *Stack) newKubeHost(ctx context.Context, name string) (*KubeHost, error) {
	c := s.kube.cluster
	admin := c.Admin()
	node, err := admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{kubelabels.LabelHost: "true", corev1.LabelArchStable: runtime.GOARCH},
	}}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("harness: creating the node of %s: %w", name, err)
	}
	node.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: kubeHostAddress}}
	if _, err := admin.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("harness: setting the node status of %s: %w", name, err)
	}

	h := &KubeHost{Node: name, VirtualNode: kubelet.VirtualNodeName(name), logs: &syncBuffer{}}
	h.logger = slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.Fake = hostfake.New(flintlock.FakeHostConfig{
		Name: name, SandboxRoot: filepath.Join(s.Root, "hosts", name), ExecEnabled: true, BootDelay: s.opts.BootDelay,
		Version: "v0.0.0-harness",
	})
	services := map[string]kubelet.HostService{}
	for service, port := range s.serviceBackends {
		services[service] = kubelet.HostService{Enabled: true, Port: port}
	}
	lease := s.opts.ProviderLeaseDuration
	if lease <= 0 {
		lease = DefaultProviderLeaseDuration
	}
	h.cfg = &kubelet.Config{
		HostNode:            name,
		MicroVMNamespace:    "flr-" + name,
		HostReserve:         kubelet.Reserve{CPU: "2", Memory: "4Gi"},
		MaxMicroVMs:         8,
		BridgeGateway:       kubeHostAddress,
		HostServices:        services,
		NotReadyDir:         filepath.Join(s.Root, "hosts", name+"-not-ready.d"),
		Listen:              net.JoinHostPort(kubeHostAddress, "0"),
		TLS:                 kubelet.ServerTLS{CertFile: c.certs.ServerCertFile, KeyFile: c.certs.ServerKeyFile, ClientCAFile: c.certs.CAFile},
		LeaseDuration:       lease,
		DrainTimeout:        time.Hour,
		Guard:               kubelet.Guard{Namespace: kubelettest.AgentNamespace},
		SyncInterval:        kubeSyncInterval,
		GuestAddressCommand: "echo 10.200.0.7",
	}
	h.cfg.ApplyDefaults()
	if h.agent, err = c.env.AgentFor(name); err != nil {
		return nil, err
	}
	if err := h.start(); err != nil {
		_ = h.Fake.Close()
		return nil, err
	}
	return h, nil
}

// DescribeCluster renders the Stack's pods and Virtual Nodes, one line
// each, for a failing scenario's log. It is empty on the battery stack.
func (s *Stack) DescribeCluster(ctx context.Context) string {
	if s.kube == nil {
		return ""
	}
	admin := s.kube.cluster.Admin()
	var b strings.Builder
	pods, err := admin.CoreV1().Pods(s.kube.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err.Error()
	}
	for _, p := range pods.Items {
		ready := "not-ready"
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = "ready"
			}
		}
		fmt.Fprintf(&b, "pod %s state=%s node=%q phase=%s %s deleting=%v reason=%q %s\n", p.Name, p.Labels[kubelabels.LabelState],
			p.Spec.NodeName, p.Status.Phase, ready, p.DeletionTimestamp != nil, p.Status.Reason, p.Status.Message)
	}
	for _, h := range s.kube.hosts {
		n, err := admin.CoreV1().Nodes().Get(ctx, h.VirtualNode, metav1.GetOptions{})
		if err != nil {
			fmt.Fprintf(&b, "node %s: %v\n", h.VirtualNode, err)
			continue
		}
		fmt.Fprintf(&b, "node %s ready=%v unschedulable=%v labels=%v taints=%v\n", n.Name, nodeReady(n), n.Spec.Unschedulable, n.Labels, n.Spec.Taints)
	}
	return b.String()
}

// kubeLeases lists the Leases still held: the Runner's claimed pods that
// nobody has deleted (KF-044, KF-048).
func (s *Stack) kubeLeases(ctx context.Context) ([]corev1.Pod, error) {
	pods, err := s.kube.cluster.Admin().CoreV1().Pods(s.kube.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{kubelabels.LabelState: kubelabels.StateClaimed}).String(),
	})
	if err != nil {
		return nil, err
	}
	var held []corev1.Pod
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase != corev1.PodFailed && p.Status.Phase != corev1.PodSucceeded {
			held = append(held, p)
		}
	}
	return held, nil
}

// checkKubeLeases fails when a claimed pod is left after the Runner has gone
// (TD-054).
func (s *Stack) checkKubeLeases(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkTimeout)
	defer cancel()
	held, err := s.kubeLeases(callCtx)
	if err != nil {
		return fmt.Errorf("harness: checking for claimed pods: %w", err)
	}
	if len(held) == 0 {
		return nil
	}
	names := make([]string, 0, len(held))
	for _, p := range held {
		names = append(names, fmt.Sprintf("%s (node %s)", p.Name, p.Spec.NodeName))
	}
	return fmt.Errorf("harness: %d Lease(s) still held after the Runner shut down: claimed pods %s", len(names), strings.Join(names, ", "))
}

// stopKube takes the cluster side down in the order that makes the sandbox
// check mean something. The Runner leaves its Pools behind on purpose
// (KF-051), so the harness deletes them as an operator removing the fleet
// would; the stand-in for the ReplicaSet controller deletes their pods, and
// each Pod Provider deletes the MicroVM of each pod and reports it gone
// (KF-027). Only then do the Pod Providers stop and the fake Hosts close,
// after which a sandbox left on a Host is a MicroVM no provider deleted.
func (s *Stack) stopKube(ctx context.Context) error {
	k := s.kube
	if k == nil {
		return nil
	}
	admin := k.cluster.Admin()
	bg := context.WithoutCancel(ctx)
	var errs []error
	if k.cancel != nil {
		// The stand-ins stop first, so that nothing replaces a pod deleted
		// here.
		k.cancel()
		k.wg.Wait()
		err := admin.AppsV1().ReplicaSets(k.namespace).DeleteCollection(bg, metav1.DeleteOptions{}, metav1.ListOptions{})
		if err != nil {
			errs = append(errs, fmt.Errorf("harness: deleting the pools: %w", err))
		}
		// Every pod - a claimed one the Runner did not release included -
		// is deleted, so that its MicroVM goes and the sandbox check reports
		// only what a provider failed to delete. The grace period is one
		// second rather than the template's default thirty, which the Pod
		// Provider waits out before the pod leaves the API server even
		// though its MicroVM is gone at once.
		var left []corev1.Pod
		grace := int64(1)
		err = poll(bg, poolDrainTimeout, func() bool {
			pods, err := admin.CoreV1().Pods(k.namespace).List(bg, metav1.ListOptions{})
			if err != nil {
				return false
			}
			left = pods.Items
			for _, p := range left {
				if p.DeletionGracePeriodSeconds == nil || *p.DeletionGracePeriodSeconds > grace {
					_ = admin.CoreV1().Pods(k.namespace).Delete(bg, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
				}
			}
			return len(left) == 0
		})
		if err != nil {
			names := make([]string, 0, len(left))
			for _, p := range left {
				names = append(names, p.Name)
			}
			errs = append(errs, fmt.Errorf("harness: pods %v were not gone %s after their pools were deleted", names, poolDrainTimeout))
		}
		// A pod the API server removed at once, as it does one that never
		// ran, leaves its MicroVM to the provider's next sync. Give them
		// that; whatever is left afterwards the sandbox check reports.
		_ = poll(bg, poolDrainTimeout, func() bool {
			for _, h := range k.hosts {
				if left, err := h.Fake.Sandboxes(); err != nil || len(left) > 0 {
					return false
				}
			}
			return true
		})
	}
	for _, h := range k.hosts {
		if err := h.stop(); err != nil {
			errs = append(errs, err)
		}
		if err := h.Fake.Close(); err != nil {
			errs = append(errs, fmt.Errorf("harness: fake host %s: %w", h.Node, err))
		}
		for _, name := range []string{h.VirtualNode, h.Node} {
			if err := admin.CoreV1().Nodes().Delete(bg, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("harness: deleting node %s: %w", name, err))
			}
		}
	}
	if err := k.cluster.removeRunnerUser(bg, k.namespace); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The harness SHALL include a scenario in which a Host's Node is
//# cordoned while it runs a Job, and SHALL assert that the Job finishes, that
//# the idle pods leave the Host and that the drain completes afterwards.

// binder stands in for the scheduler in one namespace: it binds each pending
// pod to the least loaded of the Stack's Virtual Nodes that is ready and
// schedulable, carries every label of the pod's node selector and has no
// NoSchedule taint the pod does not tolerate. The Pod Provider does the
// rest, as the kubelet would: it runs the pod, reports its status and
// finishes its deletion. The binder polls rather than watches, so that what
// it sees is always current.
type binder struct {
	client    kubernetes.Interface
	namespace string
	nodes     []string

	mu     sync.Mutex
	paused bool
}

func (b *binder) pause(p bool) {
	b.mu.Lock()
	b.paused = p
	b.mu.Unlock()
}

func (b *binder) isPaused() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paused
}

func (b *binder) run(ctx context.Context) {
	ticker := time.NewTicker(kubeSyncInterval)
	defer ticker.Stop()
	for {
		if !b.isPaused() {
			_ = b.bind(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// bind makes one pass.
func (b *binder) bind(ctx context.Context) error {
	pods, err := b.client.CoreV1().Pods(b.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	load := map[string]int{}
	var pending []*corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		switch {
		case p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded:
		case p.Spec.NodeName == "":
			pending = append(pending, p)
		default:
			load[p.Spec.NodeName]++
		}
	}
	if len(pending) == 0 {
		return nil
	}
	var nodes []*corev1.Node
	for _, name := range b.nodes {
		n, err := b.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil && nodeReady(n) && !n.Spec.Unschedulable {
			nodes = append(nodes, n)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		return pending[i].CreationTimestamp.Before(&pending[j].CreationTimestamp)
	})
	for _, p := range pending {
		var best *corev1.Node
		for _, n := range nodes {
			if !fits(p, n, load[n.Name]) {
				continue
			}
			if best == nil || load[n.Name] < load[best.Name] {
				best = n
			}
		}
		if best == nil {
			continue
		}
		err := b.client.CoreV1().Pods(b.namespace).Bind(ctx, &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: b.namespace},
			Target:     corev1.ObjectReference{Kind: "Node", Name: best.Name},
		}, metav1.CreateOptions{})
		if err == nil {
			load[best.Name]++
		}
	}
	return nil
}

// fits reports whether pod may be bound to node, which already has count
// pods of the namespace.
func fits(pod *corev1.Pod, node *corev1.Node, count int) bool {
	if !labels.SelectorFromSet(pod.Spec.NodeSelector).Matches(labels.Set(node.Labels)) {
		return false
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		// The node lifecycle controller, which envtest does not run, keeps
		// these two in step with the Ready condition; the binder reads the
		// condition instead.
		if taint.Key == corev1.TaintNodeNotReady || taint.Key == corev1.TaintNodeUnreachable {
			continue
		}
		tolerated := false
		for _, tol := range pod.Spec.Tolerations {
			if tolerates(tol, taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	if limit, ok := node.Status.Allocatable[corev1.ResourcePods]; ok && int64(count) >= limit.Value() {
		return false
	}
	return true
}

// tolerates is the scheduler's matching of a toleration to a taint.
func tolerates(tol corev1.Toleration, taint corev1.Taint) bool {
	if tol.Effect != "" && tol.Effect != taint.Effect {
		return false
	}
	if tol.Key != "" && tol.Key != taint.Key {
		return false
	}
	switch tol.Operator {
	case corev1.TolerationOpExists:
		return true
	case "", corev1.TolerationOpEqual:
		return tol.Key != "" && tol.Value == taint.Value
	}
	return false
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// syncBuffer is a log sink safe to read while the provider writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
