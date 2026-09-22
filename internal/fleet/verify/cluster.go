package verify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/executor"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// The steps a cluster verification failure names (KF-102), besides
// StepExercise and fleet.StepGuestVerify, which mean what they mean for a
// fleet of EC2 Hosts.
const (
	// StepVirtualNode is the Virtual Node of a Host being ready and having a
	// Profile that fits it.
	StepVirtualNode fleet.Step = "virtual_node"
	// StepPodReady is the verification pod of a Host becoming ready within
	// the verification timeout.
	StepPodReady fleet.Step = "pod_ready"
)

// Labels of the verification ReplicaSets and their pods. Neither carries
// the Runner or Profile label of a Pool, so the Kubernetes pool backend
// never counts or claims a verification pod, and the Pod Provider, which
// acts on the claim state label only, treats it as neither idle nor claimed.
const (
	// LabelVerify is the verification run a ReplicaSet and its pod belong
	// to. `kubectl delete rs -l gitlab-runner.flintlock.dev/verify` removes
	// whatever an interrupted run left behind.
	LabelVerify = kubelabels.Prefix + "verify"
	// LabelVerifyNode tells the verification pods of one run apart: it is a
	// hash of the Virtual Node's name, which may be longer than a label
	// value can be.
	LabelVerifyNode = kubelabels.Prefix + "verify-node"
)

// verifyContainer is the name of the verification pod's one container.
const verifyContainer = "microvm"

// cleanupTimeout bounds the deletion of the verification objects, which
// runs even when the verification itself was cut short.
const cleanupTimeout = 30 * time.Second

// ClusterConfig is what NewCluster needs.
type ClusterConfig struct {
	// Client is the Runner's client of the Kubernetes API. Required. It
	// needs nothing beyond the Runner's Role (deploy/runner/role.yaml):
	// verification pods come from ReplicaSets, which the Role may create,
	// because the Role may not create pods (KF-110).
	Client kubernetes.Interface
	// Namespace is the Runner's namespace, where its Pools live. Required.
	Namespace string
	// Transports builds the kube-exec Guest Transport; it has to be
	// configured with transport.WithKubeExec for Namespace. Required.
	Transports transport.Factory
	// Profiles are the declared Profiles; the first that fits a Virtual
	// Node gives its verification pod its shape. At least one.
	Profiles []config.Profile
	// CloudInitConfigMaps names each Profile's cloud-init ConfigMap, as the
	// Kubernetes pool backend's option of the same name does.
	CloudInitConfigMaps map[string]string
	// HostServices is the Runner's host_services section: the HTTP cache
	// upstreams and the Go module to fetch, and which services it expects
	// every Virtual Node to publish.
	HostServices config.HostServices
	// Scripts renders the guest_verify script of FL-109; nil uses the
	// embedded scripts.
	Scripts fleet.Scripts
	// NodeSelector selects the Virtual Nodes to verify; nil selects every
	// Virtual Node (KF-013).
	NodeSelector labels.Selector
	// Timeout is the verification timeout. It bounds the wait for the
	// verification pods to become ready and, separately, the commands run
	// in each.
	Timeout time.Duration
	// PollInterval defaults to 250ms.
	PollInterval time.Duration
	// Clock measures creation to readiness; nil means the real clock.
	Clock clock.Clock
	// Out receives one progress line per step; nil discards.
	Out io.Writer
}

// Cluster is `fleet verify` for a cluster fleet: it verifies the Hosts
// through their Virtual Nodes, with a pod on each (KF-100 to KF-102).
type Cluster struct {
	cfg ClusterConfig
}

// NewCluster checks cfg and returns a Cluster.
func NewCluster(cfg ClusterConfig) (*Cluster, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("verify: a Kubernetes client is required")
	case cfg.Namespace == "":
		return nil, errors.New("verify: the Runner's namespace is required")
	case cfg.Transports == nil:
		return nil, errors.New("verify: a transport factory is required")
	case len(cfg.Profiles) == 0:
		return nil, errors.New("verify: at least one Profile is required")
	case cfg.Timeout <= 0:
		return nil, errors.New("verify: the verification timeout has to be positive")
	}
	if cfg.Scripts == nil {
		s, err := scripts.New()
		if err != nil {
			return nil, fmt.Errorf("verify: %w", err)
		}
		cfg.Scripts = s
	}
	if cfg.NodeSelector == nil {
		cfg.NodeSelector = labels.SelectorFromSet(labels.Set{kubelabels.LabelVirtualNode: "true"})
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	return &Cluster{cfg: cfg}, nil
}

// target is one Host under verification: its Virtual Node, the Profile its
// pod is shaped by and, once created, the pod's ReplicaSet.
type target struct {
	hv      *fleet.HostVerification
	node    *corev1.Node
	profile *config.Profile
	// nodeHash is the LabelVerifyNode value.
	nodeHash string
	rs       string
	created  time.Time
	// pod is the verification pod once it is ready.
	pod     string
	readyIn time.Duration
}

// clusterRun is the state of one Verify call.
type clusterRun struct {
	c      *Cluster
	id     string
	report *fleet.VerifyReport
}

//= docs/requirements/12-cluster-fleet.md#cluster-verification
//# Where the Kubernetes pool backend is configured, the
//# verification command SHALL create one verification pod bound by name to
//# each ready Virtual Node, run a trivial command in it through the
//# `kube-exec` Guest Transport, report the time from creation to readiness
//# per Host and delete the pod.

// Verify verifies every selected Virtual Node. Each ready one gets a
// verification pod, bound to it by name, which is exercised and then
// deleted; the time from creating it to its readiness is the Host's
// ClaimToReady. Failures are in the report, one per Host and step; the
// error is for a verification that could not run at all, which includes a
// cluster with no Virtual Node to verify.
func (c *Cluster) Verify(ctx context.Context) (*fleet.VerifyReport, error) {
	nodes, err := c.cfg.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: c.cfg.NodeSelector.String()})
	if err != nil {
		return nil, fmt.Errorf("verify: listing the Virtual Nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("verify: no Virtual Node matches %s; is a Pod Provider running on any Host?", c.cfg.NodeSelector)
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	id, err := runID()
	if err != nil {
		return nil, err
	}
	r := &clusterRun{c: c, id: id, report: &fleet.VerifyReport{Hosts: make([]fleet.HostVerification, len(nodes.Items))}}

	var targets []*target
	for i := range nodes.Items {
		node := &nodes.Items[i]
		hv := &r.report.Hosts[i]
		hv.Host = hostOf(node)
		if t := r.admit(hv, node); t != nil {
			targets = append(targets, t)
		}
	}
	defer r.cleanup(ctx, targets)
	for _, t := range targets {
		r.create(ctx, t)
	}
	r.awaitReady(ctx, targets)
	for _, t := range targets {
		if t.pod != "" {
			r.exercise(ctx, t)
		}
	}
	return r.report, nil
}

//= docs/requirements/12-cluster-fleet.md#cluster-verification
//# If a Virtual Node is not ready or its verification pod does not
//# become ready within the verification timeout, then the verification
//# command SHALL exit with a non-zero status naming the Host and the
//# reason.

// admit returns the Host's target, or nil after reporting why its Virtual
// Node cannot be verified: it is not ready, or no Profile fits it.
func (r *clusterRun) admit(hv *fleet.HostVerification, node *corev1.Node) *target {
	if ready, why := nodeReady(node); !ready {
		r.fail(hv.Host, StepVirtualNode, fmt.Errorf("virtual node %s is not ready: %s", node.Name, why))
		return nil
	}
	p := r.c.profileFor(node)
	if p == nil {
		r.fail(hv.Host, StepVirtualNode, fmt.Errorf("no Profile fits virtual node %s (architecture %q and its labels)", node.Name, node.Labels[corev1.LabelArchStable]))
		return nil
	}
	sum := sha256.Sum256([]byte(node.Name))
	return &target{hv: hv, node: node, profile: p, nodeHash: hex.EncodeToString(sum[:5])}
}

// create creates the Host's verification ReplicaSet: one replica, whose pod
// template is bound to the Virtual Node by name.
func (r *clusterRun) create(ctx context.Context, t *target) {
	rs, err := r.c.replicaSet(r.id, t)
	if err == nil {
		t.created = r.c.cfg.Clock.Now()
		_, err = r.c.cfg.Client.AppsV1().ReplicaSets(r.c.cfg.Namespace).Create(ctx, rs, metav1.CreateOptions{})
	}
	if err != nil {
		r.fail(t.hv.Host, StepPodReady, fmt.Errorf("creating the verification pod's ReplicaSet: %w", err))
		return
	}
	t.rs = rs.Name
}

// awaitReady polls the verification pods until each is ready or the
// verification timeout has passed since it was created, and reports each
// that was not with what its status says.
func (r *clusterRun) awaitReady(ctx context.Context, targets []*target) {
	wctx, cancel := context.WithTimeout(ctx, r.c.cfg.Timeout)
	defer cancel()
	last := map[*target]*corev1.Pod{}
	var lastErr error
	for {
		pending := 0
		pods, err := r.c.cfg.Client.CoreV1().Pods(r.c.cfg.Namespace).List(wctx, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{LabelVerify: r.id}).String(),
		})
		if err != nil {
			lastErr = err
		}
		for _, t := range targets {
			if t.rs == "" || t.pod != "" {
				continue
			}
			if pods != nil {
				if pod := podOf(pods.Items, t.nodeHash); pod != nil {
					last[t] = pod
					if podReady(pod) {
						t.pod = pod.Name
						t.readyIn = r.c.cfg.Clock.Now().Sub(t.created)
						fmt.Fprintf(r.c.cfg.Out, "ok   %s %s: pod %s ready in %s\n", t.hv.Host, StepPodReady, pod.Name, t.readyIn.Round(time.Millisecond))
						continue
					}
				}
			}
			pending++
		}
		if pending == 0 || !sleep(wctx, r.c.cfg.PollInterval) {
			break
		}
	}
	for _, t := range targets {
		if t.rs == "" || t.pod != "" {
			continue
		}
		why := "its ReplicaSet created no pod"
		if pod := last[t]; pod != nil {
			why = fmt.Sprintf("pod %s: %s", pod.Name, podStatus(pod))
		} else if lastErr != nil {
			why = fmt.Sprintf("listing the verification pods: %v", lastErr)
		}
		r.fail(t.hv.Host, StepPodReady, fmt.Errorf("the verification pod on virtual node %s was not ready within %s: %s", t.node.Name, r.c.cfg.Timeout, why))
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-verification
//# The verification command SHALL perform the Host Service checks
//# of FL-109 from inside each verification pod.

// exercise runs a trivial command in a ready verification pod through
// kube-exec, then the guest_verify script of FL-109 with the Host Service
// addresses the Virtual Node publishes, and records the outcome per
// service. Both are bounded by the verification timeout.
func (r *clusterRun) exercise(ctx context.Context, t *target) {
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.Timeout)
	defer cancel()
	tr, err := r.c.cfg.Transports.New(ctx, transport.Target{Kind: transport.KindKubeExec, VMUID: t.pod})
	if err != nil {
		r.fail(t.hv.Host, StepExercise, fmt.Errorf("guest transport: %w", err))
		return
	}
	defer func() { _ = tr.Close() }()

	shell := t.profile.Shell
	if shell == "" {
		shell = config.DefaultShell
	}
	var stderr strings.Builder
	status, err := tr.Run(ctx, transport.Command{Path: shell, Args: []string{"-c", "true"}, User: t.profile.User, Stderr: &stderr})
	switch {
	case err != nil:
		r.fail(t.hv.Host, StepExercise, fmt.Errorf("running a trivial command in pod %s: %w", t.pod, err))
		return
	case status != 0:
		r.fail(t.hv.Host, StepExercise, fmt.Errorf("a trivial command in pod %s exited %d: %s", t.pod, status, strings.TrimSpace(stderr.String())))
		return
	}
	t.hv.Exercised = true
	t.hv.ClaimToReady = t.readyIn
	fmt.Fprintf(r.c.cfg.Out, "ok   %s %s: pod %s, creation to ready %s\n", t.hv.Host, StepExercise, t.pod, t.readyIn.Round(time.Millisecond))

	r.checkServices(ctx, t, tr, shell)
}

// checkServices is FL-109 from inside the verification pod: the
// guest_verify script, rendered for the Host Service addresses of the
// Virtual Node, on the shell's standard input. A service the Runner's
// configuration enables and the Virtual Node does not publish is a failure
// too, because the Jobs on that Host get no address for it (KF-062).
func (r *clusterRun) checkServices(ctx context.Context, t *target, tr transport.Transport, shell string) {
	entry := r.c.hostEntry(t.node)
	for _, name := range r.c.unpublished(entry.Services) {
		r.fail(t.hv.Host, fleet.StepGuestVerify, fmt.Errorf("%s: enabled in host_services but virtual node %s publishes no address for it", name, t.node.Name))
	}
	script, err := r.c.cfg.Scripts.Render(fleet.StepGuestVerify, fleet.RenderInput{
		HostServices: r.c.cfg.HostServices,
		Profiles:     r.c.cfg.Profiles,
		Instance:     fleet.Instance{ID: entry.Name},
		Inventory:    fleet.Inventory{Hosts: []config.HostEntry{*entry}},
	})
	if err != nil {
		r.fail(t.hv.Host, fleet.StepGuestVerify, fmt.Errorf("rendering the Host Service checks: %w", err))
		return
	}
	var stdout, stderr strings.Builder
	status, err := tr.Run(ctx, transport.Command{
		Path: shell, Dir: "/", User: t.profile.User,
		Stdin: strings.NewReader(script.Content), Stdout: &stdout, Stderr: &stderr,
	})
	switch {
	case err != nil:
		r.fail(t.hv.Host, fleet.StepGuestVerify, fmt.Errorf("running the Host Service checks in pod %s: %w", t.pod, err))
		return
	case status != 0:
		r.fail(t.hv.Host, fleet.StepGuestVerify, fmt.Errorf("the Host Service checks in pod %s exited %d: %s", t.pod, status, strings.TrimSpace(stderr.String())))
		return
	}
	checks := scripts.ParseOutput(stdout.String()).Services
	t.hv.Services = checks
	for _, s := range checks {
		if s.Err != nil {
			r.fail(t.hv.Host, fleet.StepGuestVerify, fmt.Errorf("%s: %w", s.Service, s.Err))
			continue
		}
		fmt.Fprintf(r.c.cfg.Out, "ok   %s %s: %s\n", t.hv.Host, fleet.StepGuestVerify, s.Service)
	}
	if len(checks) == 0 {
		fmt.Fprintf(r.c.cfg.Out, "note %s %s: virtual node %s publishes no Host Service\n", t.hv.Host, fleet.StepGuestVerify, t.node.Name)
	}
}

// hostEntry is the Inventory entry the guest_verify script is rendered
// for: the Host Service addresses of the Virtual Node, read exactly as the
// Executor reads them for a Job (KF-062). A cluster fleet has no Inventory,
// and the renderer only needs the entry's endpoint to be an IPv4 address,
// so it is the Virtual Node's internal address, or the unspecified address
// when it has none; nothing dials it.
func (c *Cluster) hostEntry(node *corev1.Node) *config.HostEntry {
	lookup := executor.NewVirtualNodeInventory(fixedNode{node}, c.cfg.HostServices.HTTPCache.Upstreams, nil)
	entry, _ := lookup.Host(node.Name)
	entry.Endpoint = "0.0.0.0"
	for _, a := range node.Status.Addresses {
		if ip, err := netip.ParseAddr(a.Address); err == nil && ip.Is4() && a.Type == corev1.NodeInternalIP {
			entry.Endpoint = ip.String()
			break
		}
	}
	return entry
}

// unpublished lists the Host Services the configuration enables that the
// Virtual Node gives no address for.
func (c *Cluster) unpublished(a config.HostServiceAddresses) []string {
	hs := c.cfg.HostServices
	var out []string
	if hs.Buildkit.IsEnabled() && a.Buildkit == "" {
		out = append(out, kubelabels.HostServiceBuildkit)
	}
	if hs.GoProxy.IsEnabled() && a.GoProxy == "" {
		out = append(out, kubelabels.HostServiceGoProxy)
	}
	if hs.RegistryMirror.IsEnabled() && a.RegistryMirror == "" {
		out = append(out, kubelabels.HostServiceRegistryMirror)
	}
	if hs.HTTPCache.IsEnabled() && len(hs.HTTPCache.Upstreams) > 0 && len(a.HTTPCache) == 0 {
		out = append(out, kubelabels.HostServiceHTTPCache)
	}
	return out
}

// cleanup deletes every verification ReplicaSet and its pod. It runs on a
// context of its own so that a verification cut short still removes them.
// The pod is deleted as well as its ReplicaSet so that it goes whether or
// not a garbage collector runs.
func (r *clusterRun) cleanup(ctx context.Context, targets []*target) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	background := metav1.DeletePropagationBackground
	for _, t := range targets {
		if t.rs == "" {
			continue
		}
		err := r.c.cfg.Client.AppsV1().ReplicaSets(r.c.cfg.Namespace).Delete(ctx, t.rs, metav1.DeleteOptions{PropagationPolicy: &background})
		if err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintf(r.c.cfg.Out, "warning: deleting ReplicaSet %s/%s: %v\n", r.c.cfg.Namespace, t.rs, err)
		}
	}
	pods, err := r.c.cfg.Client.CoreV1().Pods(r.c.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{LabelVerify: r.id}).String(),
	})
	if err != nil {
		fmt.Fprintf(r.c.cfg.Out, "warning: listing the verification pods to delete them: %v\n", err)
		return
	}
	for _, pod := range pods.Items {
		err := r.c.cfg.Client.CoreV1().Pods(r.c.cfg.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintf(r.c.cfg.Out, "warning: deleting pod %s/%s: %v\n", r.c.cfg.Namespace, pod.Name, err)
		}
	}
}

func (r *clusterRun) fail(host string, step fleet.Step, err error) {
	r.report.Failures = append(r.report.Failures, fleet.Failure{Host: host, Step: step, Err: err})
	fmt.Fprintf(r.c.cfg.Out, "FAIL %s %s: %v\n", host, step, err)
}

// replicaSet renders a Host's verification ReplicaSet. Its pod has the
// shape of a Pool pod of the Profile (internal/poolmgr/kube's podTemplate),
// which is what the Pod Provider accepts (KF-020, KF-022): one container
// whose image is the root filesystem image and whose limits are the vCPU
// count and the memory, the kernel and hypervisor in annotations, no service
// account token and a toleration of the Virtual Node's taint. Instead of a
// node selector and a spread it names its Virtual Node, and instead of the
// Pool labels it carries the verification labels.
func (c *Cluster) replicaSet(id string, t *target) (*appsv1.ReplicaSet, error) {
	p := t.profile
	if p.RootFS == "" || p.Kernel.Image == "" {
		return nil, fmt.Errorf("profile %s needs a root filesystem image and a kernel image", p.Name)
	}
	if p.VCPU <= 0 || p.MemoryMB <= 0 {
		return nil, fmt.Errorf("profile %s needs a vcpu count and a memory size", p.Name)
	}
	annotations := map[string]string{kubelabels.AnnotationKernelImage: p.Kernel.Image}
	setIf := func(key, value string) {
		if value != "" {
			annotations[key] = value
		}
	}
	setIf(kubelabels.AnnotationKernelFilename, p.Kernel.Filename)
	setIf(kubelabels.AnnotationKernelCmdline, cmdline(p.Kernel.Cmdline))
	setIf(kubelabels.AnnotationHypervisor, p.Provider)
	setIf(kubelabels.AnnotationCloudInitConfigMap, c.cfg.CloudInitConfigMaps[p.Name])

	selector := map[string]string{LabelVerify: id, LabelVerifyNode: t.nodeHash}
	podLabels := map[string]string{LabelVerify: id, LabelVerifyNode: t.nodeHash}
	one := int32(1)
	no := false
	grace := int64(1)
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("flr-verify-%s-%s", id, t.nodeHash),
			Namespace: c.cfg.Namespace,
			Labels:    map[string]string{LabelVerify: id, LabelVerifyNode: t.nodeHash},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: annotations},
				Spec: corev1.PodSpec{
					NodeName: t.node.Name,
					Containers: []corev1.Container{{
						Name:  verifyContainer,
						Image: p.RootFS,
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(int64(p.VCPU), resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(int64(p.MemoryMB)<<20, resource.BinarySI),
						}},
					}},
					RestartPolicy: corev1.RestartPolicyAlways,
					// Nothing in a verification guest needs a graceful stop,
					// and the Pod Provider finishes a deletion only once the
					// grace period is over, so a short one lets cleanup
					// complete promptly.
					TerminationGracePeriodSeconds: &grace,
					AutomountServiceAccountToken:  &no,
					EnableServiceLinks:            &no,
					Tolerations: []corev1.Toleration{{
						Key:      kubelabels.TaintMicroVM,
						Operator: corev1.TolerationOpEqual,
						Value:    "true",
						Effect:   corev1.TaintEffectNoSchedule,
					}},
				},
			},
		},
	}, nil
}

// profileFor is the first Profile whose Pool pods could be placed on the
// Virtual Node: of its architecture and carrying every label of its Host
// selector, read as internal/poolmgr/kube's node selector reads it.
func (c *Cluster) profileFor(node *corev1.Node) *config.Profile {
	for i := range c.cfg.Profiles {
		p := &c.cfg.Profiles[i]
		if p.Arch != "" && string(p.Arch) != node.Labels[corev1.LabelArchStable] {
			continue
		}
		fits := true
		for key, value := range p.HostSelector {
			if !strings.Contains(key, "/") {
				key = kubelabels.Prefix + key
			}
			if node.Labels[key] != value {
				fits = false
				break
			}
		}
		if fits {
			return p
		}
	}
	return nil
}

// hostOf names a Virtual Node's Host: its Host's Node (KF-013), or the
// Virtual Node itself where that label is missing.
func hostOf(node *corev1.Node) string {
	if h := node.Labels[kubelabels.LabelHostNode]; h != "" {
		return h
	}
	return node.Name
}

// nodeReady reports whether a Node's Ready condition is true, and the
// condition's reason and message where it is not.
func nodeReady(node *corev1.Node) (bool, string) {
	for _, c := range node.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		if c.Status == corev1.ConditionTrue {
			return true, ""
		}
		return false, conditionText(string(c.Status), c.Reason, c.Message)
	}
	return false, "it reports no Ready condition"
}

func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podStatus says why a pod is not ready: its phase, its Ready condition and
// what its container is waiting for.
func podStatus(pod *corev1.Pod) string {
	parts := []string{"phase " + string(pod.Status.Phase)}
	if pod.Status.Phase == "" {
		parts[0] = "no status reported by the Pod Provider"
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue {
			parts = append(parts, "ready "+conditionText(string(c.Status), c.Reason, c.Message))
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			parts = append(parts, "container "+conditionText("waiting", w.Reason, w.Message))
		}
	}
	if pod.Status.Message != "" {
		parts = append(parts, pod.Status.Message)
	}
	return strings.Join(parts, ", ")
}

func conditionText(status, reason, message string) string {
	out := status
	if reason != "" {
		out += " (" + reason + ")"
	}
	if message != "" {
		out += ": " + message
	}
	return out
}

// podOf is the live pod of a Host's verification ReplicaSet.
func podOf(pods []corev1.Pod, nodeHash string) *corev1.Pod {
	var found *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Labels[LabelVerifyNode] != nodeHash || p.DeletionTimestamp != nil {
			continue
		}
		if found == nil || podReady(p) {
			found = p
		}
	}
	return found
}

// cmdline renders kernel arguments as internal/poolmgr/kube does: space
// separated, in key order, a bare key where the value is empty.
func cmdline(args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	words := make([]string, 0, len(keys))
	for _, k := range keys {
		if args[k] == "" {
			words = append(words, k)
			continue
		}
		words = append(words, k+"="+args[k])
	}
	return strings.Join(words, " ")
}

// runID names one verification run in its objects' names and labels.
func runID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("verify: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// fixedNode is an executor.NodeGetter that answers with a Node already
// read, so that the Virtual Node listing is not read again per Host.
type fixedNode struct{ node *corev1.Node }

// Get implements executor.NodeGetter.
func (f fixedNode) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.Node, error) {
	if name != f.node.Name {
		return nil, apierrors.NewNotFound(corev1.Resource("nodes"), name)
	}
	return f.node, nil
}
