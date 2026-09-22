package kubelet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// Reasons the provider reports on pods and their container.
const (
	reasonUnsupportedPod  = "UnsupportedPodSpec"
	reasonBooting         = "MicroVMBooting"
	reasonMicroVMFailed   = "MicroVMFailed"
	reasonMicroVMGone     = "MicroVMDisappeared"
	reasonMicroVMDeleted  = "MicroVMDeleted"
	reasonDeadline        = "DeadlineExceeded"
	probeTimeout          = 10 * time.Second
	deletePollInterval    = 100 * time.Millisecond
	defaultDeleteDeadline = 2 * time.Minute
)

// Provider realises the pods bound to one Virtual Node as MicroVMs on the
// local flintlockd. It is virtual kubelet's PodLifecycleHandler and
// PodNotifier; the pod controller calls it for every pod event and it calls
// back whenever a MicroVM changes what its pod's status should say.
//
// All state is a cache of what flintlockd and the API server hold: the
// records are rebuilt from the two by adopt on every start, which is what
// lets the provider stop, restart or lose the API server without touching a
// MicroVM (KF-029).
type Provider struct {
	cfg        *Config
	log        *slog.Logger
	clk        clock.Clock
	host       flintlock.PoolHostClient
	kube       kubernetes.Interface
	transports transport.Factory
	nodeName   string
	hostIP     string
	// deleteDeadline bounds one DeletePod call's wait for flintlockd to stop
	// listing the MicroVM; the pod controller retries after it.
	deleteDeadline time.Duration

	mu     sync.Mutex
	pods   map[string]*podRecord
	notify func(*corev1.Pod)
}

// podRecord is one pod the provider is responsible for.
type podRecord struct {
	// pod is the pod as last given by the pod controller, carrying the
	// status the provider reports for it.
	pod *corev1.Pod
	// vmUID is the flintlock uid of the pod's MicroVM; empty for a pod that
	// was refused and has none.
	vmUID string
	// ready is set once the MicroVM is CREATED and the probe succeeded.
	ready bool
	// terminal is set once the pod is failed; nothing changes it after.
	terminal bool
	// deleting is set while DeletePod works on the record.
	deleting bool
}

func podKey(namespace, name string) string { return namespace + "/" + name }

// NotifyPods implements node.PodNotifier.
func (p *Provider) NotifyPods(_ context.Context, cb func(*corev1.Pod)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notify = cb
}

// publish hands the pod controller a copy of a record's pod. It is called
// without the lock held, because the callback can block.
func (p *Provider) publish(pod *corev1.Pod) {
	p.mu.Lock()
	notify := p.notify
	p.mu.Unlock()
	if notify != nil && pod != nil {
		notify(pod)
	}
}

// CreatePod implements node.PodLifecycleHandler. A pod the provider cannot
// realise is recorded as failed, with no MicroVM, and nil is returned so
// that the pod controller does not retry what will never work (KF-022); an
// error from the API server or flintlockd is returned so that it does.
func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)
	log := p.log.With("pod", key, "pod_uid", string(pod.UID))

	if err := validatePod(pod); err != nil {
		return p.refuse(pod, err)
	}
	userData, err := p.cloudInitUserData(ctx, pod)
	if err != nil {
		return err
	}
	spec, err := buildMicroVMSpec(pod, p.cfg.MicroVMNamespace, userData)
	var refused *unsupportedError
	if errors.As(err, &refused) {
		return p.refuse(pod, err)
	}
	if err != nil {
		return err
	}

	// A MicroVM already labelled with this pod's UID is this pod's: an
	// earlier CreatePod got as far as flintlockd and the provider did not
	// get to record it. Creating a second one would leak the first.
	vm, err := p.findMicroVM(ctx, string(pod.UID))
	if err != nil {
		return err
	}
	if vm == nil && pod.Status.Phase == corev1.PodRunning {
		// The pod ran in a MicroVM that is gone, lost while the provider
		// was not there to see it. Booting another would hand the Job that
		// claimed it an empty machine and call it the same pod (KF-026).
		return p.recordFailed(pod, reasonMicroVMGone, "flintlockd no longer lists the microvm this pod was running in")
	}
	if vm == nil {
		if vm, err = p.host.CreateMicroVM(ctx, spec); err != nil {
			return fmt.Errorf("creating the microvm of pod %s: %w", key, err)
		}
		log.Info("created microvm", "microvm", vm.GetSpec().GetUid(), "vcpu", spec.GetVcpu(), "memory_mb", spec.GetMemoryInMb())
	}

	rec := &podRecord{pod: pod.DeepCopy(), vmUID: vm.GetSpec().GetUid()}
	rec.pod.Status = p.pendingStatus(rec.pod)
	p.mu.Lock()
	p.pods[key] = rec
	snapshot := rec.pod.DeepCopy()
	p.mu.Unlock()
	p.publish(snapshot)
	return nil
}

// refuse records a pod as failed without a MicroVM (KF-022).
func (p *Provider) refuse(pod *corev1.Pod, why error) error {
	p.log.Warn("refusing a pod the provider cannot realise as a microvm",
		"pod", podKey(pod.Namespace, pod.Name), "reason", why.Error())
	return p.recordFailed(pod, reasonUnsupportedPod, why.Error())
}

// recordFailed records a pod as failed with no MicroVM behind it.
func (p *Provider) recordFailed(pod *corev1.Pod, reason, message string) error {
	rec := &podRecord{pod: pod.DeepCopy(), terminal: true}
	rec.pod.Status = p.failedStatus(rec.pod, reason, message)
	p.mu.Lock()
	p.pods[podKey(pod.Namespace, pod.Name)] = rec
	snapshot := rec.pod.DeepCopy()
	p.mu.Unlock()
	p.publish(snapshot)
	return nil
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# Where the pod names a cloud-init ConfigMap in its annotations,
//# the Pod Provider SHALL pass that ConfigMap's user data to the MicroVM.

// cloudInitUserData reads the user data of the ConfigMap a pod names, in the
// pod's own namespace. It is a single get of a named object, so the
// provider needs no list or watch on ConfigMaps (KF-111). A ConfigMap that
// is missing, or has no user data, is an error the pod controller retries:
// the ConfigMap can still be created, and a MicroVM booted without the
// cloud-init it was meant to have is worse than one that boots late.
func (p *Provider) cloudInitUserData(ctx context.Context, pod *corev1.Pod) (string, error) {
	name := pod.Annotations[kubelabels.AnnotationCloudInitConfigMap]
	if name == "" {
		return "", nil
	}
	cm, err := p.kube.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading cloud-init configmap %s/%s: %w", pod.Namespace, name, err)
	}
	userData, ok := cm.Data[kubelabels.CloudInitUserDataKey]
	if !ok {
		return "", fmt.Errorf("cloud-init configmap %s/%s has no %q key", pod.Namespace, name, kubelabels.CloudInitUserDataKey)
	}
	return userData, nil
}

// findMicroVM returns the MicroVM labelled with a pod UID, or nil.
func (p *Provider) findMicroVM(ctx context.Context, podUID string) (*types.MicroVM, error) {
	vms, err := p.host.ListMicroVMs(ctx, p.cfg.MicroVMNamespace)
	if err != nil {
		return nil, fmt.Errorf("listing microvms: %w", err)
	}
	for _, vm := range vms {
		if vm.GetSpec().GetLabels()[vmLabelPodUID] == podUID {
			return vm, nil
		}
	}
	return nil, nil
}

// UpdatePod implements node.PodLifecycleHandler. What can change on a bound
// pod is its labels, annotations and active deadline, none of which changes
// the MicroVM; the record takes the new object and keeps its status, so
// that a claim (KF-044) is seen by the lease and drain checks.
func (p *Provider) UpdatePod(_ context.Context, pod *corev1.Pod) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.pods[podKey(pod.Namespace, pod.Name)]
	if !ok {
		return errdefs.NotFoundf("pod %s/%s is not known to the provider", pod.Namespace, pod.Name)
	}
	status := rec.pod.Status
	rec.pod = pod.DeepCopy()
	rec.pod.Status = status
	return nil
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# When a pod is deleted, the Pod Provider SHALL delete its
//# MicroVM and SHALL report the pod terminated only after `flintlockd`
//# no longer lists the MicroVM.

// DeletePod implements node.PodLifecycleHandler. It returns, and reports
// the pod's container terminated, only once flintlockd answers NotFound for
// the MicroVM; the pod controller removes the pod from the API server after
// that and not before. flintlockd deletes asynchronously, so the MicroVM is
// polled; when it is still listed at the deadline the call fails and the pod
// controller calls again. A flintlockd that cannot be reached is a failure
// too: not being told is not the same as the MicroVM being gone.
func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	rec, ok := p.pods[key]
	if ok {
		rec.deleting = true
	}
	p.mu.Unlock()
	if !ok {
		return errdefs.NotFoundf("pod %s is not known to the provider", key)
	}

	if rec.vmUID != "" {
		if err := p.deleteMicroVM(ctx, rec.vmUID); err != nil {
			p.mu.Lock()
			rec.deleting = false
			p.mu.Unlock()
			return fmt.Errorf("pod %s: %w", key, err)
		}
		p.log.Info("deleted microvm", "pod", key, "microvm", rec.vmUID)
	}

	p.mu.Lock()
	rec.pod.Status = p.terminatedStatus(rec.pod)
	snapshot := rec.pod.DeepCopy()
	delete(p.pods, key)
	p.mu.Unlock()
	p.publish(snapshot)
	return nil
}

// deleteMicroVM deletes a MicroVM and waits until flintlockd no longer has
// it. A MicroVM that is already gone is deleted.
func (p *Provider) deleteMicroVM(ctx context.Context, uid string) error {
	if err := p.host.DeleteMicroVM(ctx, uid); err != nil && !errors.Is(err, flintlock.ErrNotFound) {
		return fmt.Errorf("deleting microvm %s: %w", uid, err)
	}
	ctx, cancel := context.WithTimeout(ctx, p.deleteDeadline)
	defer cancel()
	for {
		_, err := p.host.GetMicroVM(ctx, uid)
		if errors.Is(err, flintlock.ErrNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			if err == nil {
				err = errors.New("flintlockd still lists it")
			}
			return fmt.Errorf("waiting for microvm %s to be gone: %w", uid, err)
		case <-p.clk.After(deletePollInterval):
		}
	}
}

// GetPod implements node.PodLifecycleHandler.
func (p *Provider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.pods[podKey(namespace, name)]
	if !ok {
		return nil, errdefs.NotFoundf("pod %s/%s is not known to the provider", namespace, name)
	}
	return rec.pod.DeepCopy(), nil
}

// GetPodStatus implements node.PodLifecycleHandler.
func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return &pod.Status, nil
}

// GetPods implements node.PodLifecycleHandler.
func (p *Provider) GetPods(context.Context) ([]*corev1.Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*corev1.Pod, 0, len(p.pods))
	for _, rec := range p.pods {
		out = append(out, rec.pod.DeepCopy())
	}
	sort.Slice(out, func(i, j int) bool {
		return podKey(out[i].Namespace, out[i].Name) < podKey(out[j].Namespace, out[j].Name)
	})
	return out, nil
}

// syncPods brings every record's status up to date with its MicroVM and
// publishes the ones that changed.
func (p *Provider) syncPods(ctx context.Context) {
	p.mu.Lock()
	keys := make([]string, 0, len(p.pods))
	for key := range p.pods {
		keys = append(keys, key)
	}
	p.mu.Unlock()
	sort.Strings(keys)
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		p.publish(p.syncPod(ctx, key))
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# The Pod Provider SHALL report the pod running and ready only
//# when `flintlockd` reports the MicroVM created and a command run through
//# `MicroVMExec` in the guest succeeds.

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# If the MicroVM fails or disappears from `flintlockd`, then the
//# Pod Provider SHALL mark its pod failed with the reason `flintlockd`
//# gives.

// syncPod reconciles one record and returns the pod to publish, or nil when
// nothing changed. A flintlockd that cannot be reached changes nothing: the
// MicroVMs run on without it (HI-040), and the Virtual Node's Ready
// condition is what reports the outage.
func (p *Provider) syncPod(ctx context.Context, key string) *corev1.Pod {
	p.mu.Lock()
	rec, ok := p.pods[key]
	if !ok || rec.terminal || rec.deleting || rec.vmUID == "" {
		p.mu.Unlock()
		return nil
	}
	uid, ready := rec.vmUID, rec.ready
	deadline := activeDeadline(rec.pod)
	p.mu.Unlock()

	if !deadline.IsZero() && !p.clk.Now().Before(deadline) {
		// The kubelet enforces a pod's active deadline, and on a Virtual
		// Node the provider is the kubelet. It is the backstop for a Runner
		// that dies holding a claim (KF-044).
		if err := p.deleteMicroVM(ctx, uid); err != nil {
			p.log.Warn("could not delete a microvm past its pod's active deadline", "pod", key, "microvm", uid, "error", err)
			return nil
		}
		return p.fail(key, reasonDeadline, "the pod was active longer than its active deadline")
	}

	vm, err := p.host.GetMicroVM(ctx, uid)
	if errors.Is(err, flintlock.ErrNotFound) {
		return p.fail(key, reasonMicroVMGone, err.Error())
	}
	if err != nil {
		p.log.Debug("could not read a microvm; leaving its pod as it is", "pod", key, "microvm", uid, "error", err)
		return nil
	}

	switch state := vm.GetStatus().GetState(); state {
	case types.MicroVMStatus_PENDING:
		return nil
	case types.MicroVMStatus_CREATED:
		if ready {
			return nil
		}
		return p.probe(ctx, key, uid)
	default:
		return p.fail(key, reasonMicroVMFailed, fmt.Sprintf("flintlockd reports microvm %s in state %s after %d retries",
			uid, state, vm.GetStatus().GetRetry()))
	}
}

// probe runs the readiness command in a CREATED MicroVM and, when it
// succeeds, marks the pod running and ready (KF-024). A guest that does not
// answer yet is still booting; the next sync tries again.
func (p *Provider) probe(ctx context.Context, key, uid string) *corev1.Pod {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	guest, err := p.transports.New(ctx, transport.Target{Kind: transport.KindExec, Host: p.host, VMUID: uid, Deadline: probeTimeout})
	if err != nil {
		p.log.Debug("could not build the guest transport", "pod", key, "microvm", uid, "error", err)
		return nil
	}
	defer func() { _ = guest.Close() }()
	if err := guest.Ready(ctx); err != nil {
		p.log.Debug("guest is not ready yet", "pod", key, "microvm", uid, "error", err)
		return nil
	}
	address := p.guestAddress(ctx, guest, key)

	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.pods[key]
	if !ok || rec.terminal || rec.deleting {
		return nil
	}
	rec.ready = true
	rec.pod.Status = p.runningStatus(rec.pod, address)
	p.log.Info("microvm is ready", "pod", key, "microvm", uid, "address", address)
	return rec.pod.DeepCopy()
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# The Pod Provider SHALL report the Host's internal address as
//# the pod's host address and the guest's bridge address as the pod's
//# address.

// guestAddress asks the guest for its address on the bridge. flintlockd does
// not report one, because the guest gets it from the Host's DHCP service
// (HI-031), so the guest is asked, with the configured command. The address
// is informational, so a guest that does not answer leaves it empty rather
// than keeping the pod from being ready.
func (p *Provider) guestAddress(ctx context.Context, guest transport.Transport, key string) string {
	var out bytes.Buffer
	status, err := guest.Run(ctx, transport.Command{
		Path:   "sh",
		Args:   []string{"-c", p.cfg.GuestAddressCommand},
		Stdout: &out,
	})
	fields := strings.Fields(out.String())
	if err != nil || status != 0 || len(fields) == 0 {
		p.log.Warn("could not learn the guest's address; the pod reports none", "pod", key, "exit_status", status, "error", err)
		return ""
	}
	return fields[0]
}

// fail marks a record failed, once, and returns the pod to publish.
func (p *Provider) fail(key, reason, message string) *corev1.Pod {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.pods[key]
	if !ok || rec.terminal || rec.deleting {
		return nil
	}
	rec.terminal, rec.ready = true, false
	rec.pod.Status = p.failedStatus(rec.pod, reason, message)
	p.log.Warn("pod failed", "pod", key, "microvm", rec.vmUID, "reason", reason, "message", message)
	return rec.pod.DeepCopy()
}

// activeDeadline is when a pod's active deadline passes, or zero.
func activeDeadline(pod *corev1.Pod) time.Time {
	if pod.Spec.ActiveDeadlineSeconds == nil || pod.Status.StartTime == nil {
		return time.Time{}
	}
	return pod.Status.StartTime.Add(time.Duration(*pod.Spec.ActiveDeadlineSeconds) * time.Second)
}

// Status construction. Every status carries the Host's address (KF-025) and
// keeps the start time of the one before it.

func (p *Provider) baseStatus(pod *corev1.Pod) corev1.PodStatus {
	start := pod.Status.StartTime
	if start == nil {
		now := metav1.NewTime(p.clk.Now())
		start = &now
	}
	status := corev1.PodStatus{HostIP: p.hostIP, StartTime: start}
	if p.hostIP != "" {
		status.HostIPs = []corev1.HostIP{{IP: p.hostIP}}
	}
	return status
}

func (p *Provider) conditions(ready bool) []corev1.PodCondition {
	now := metav1.NewTime(p.clk.Now())
	state := corev1.ConditionFalse
	if ready {
		state = corev1.ConditionTrue
	}
	return []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.ContainersReady, Status: state, LastTransitionTime: now},
		{Type: corev1.PodReady, Status: state, LastTransitionTime: now},
	}
}

func containerStatus(pod *corev1.Pod, state corev1.ContainerState, ready bool) []corev1.ContainerStatus {
	if len(pod.Spec.Containers) == 0 {
		return nil
	}
	c := pod.Spec.Containers[0]
	started := ready
	return []corev1.ContainerStatus{{Name: c.Name, Image: c.Image, State: state, Ready: ready, Started: &started}}
}

func (p *Provider) pendingStatus(pod *corev1.Pod) corev1.PodStatus {
	status := p.baseStatus(pod)
	status.Phase = corev1.PodPending
	status.Conditions = p.conditions(false)
	status.ContainerStatuses = containerStatus(pod, corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reasonBooting, Message: "the microvm is booting"},
	}, false)
	return status
}

func (p *Provider) runningStatus(pod *corev1.Pod, address string) corev1.PodStatus {
	status := p.baseStatus(pod)
	status.Phase = corev1.PodRunning
	status.Conditions = p.conditions(true)
	status.PodIP = address
	if address != "" {
		status.PodIPs = []corev1.PodIP{{IP: address}}
	}
	status.ContainerStatuses = containerStatus(pod, corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(p.clk.Now())},
	}, true)
	return status
}

func (p *Provider) failedStatus(pod *corev1.Pod, reason, message string) corev1.PodStatus {
	status := p.baseStatus(pod)
	status.Phase = corev1.PodFailed
	status.Reason = reason
	status.Message = message
	status.Conditions = p.conditions(false)
	status.ContainerStatuses = containerStatus(pod, corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: reason, Message: message, FinishedAt: metav1.NewTime(p.clk.Now())},
	}, false)
	return status
}

// terminatedStatus is the last status of a deleted pod: a failed pod stays
// failed, any other ends succeeded, with its container terminated.
func (p *Provider) terminatedStatus(pod *corev1.Pod) corev1.PodStatus {
	if pod.Status.Phase == corev1.PodFailed {
		return pod.Status
	}
	status := p.baseStatus(pod)
	status.Phase = corev1.PodSucceeded
	status.Reason = reasonMicroVMDeleted
	status.PodIP, status.PodIPs = pod.Status.PodIP, pod.Status.PodIPs
	status.Conditions = p.conditions(false)
	status.ContainerStatuses = containerStatus(pod, corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: reasonMicroVMDeleted, FinishedAt: metav1.NewTime(p.clk.Now())},
	}, false)
	return status
}

// boundPods lists, from the API server, the pods bound to the Virtual Node.
func (p *Provider) boundPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := p.kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + p.nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing the pods bound to %s: %w", p.nodeName, err)
	}
	return list.Items, nil
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# When starting, the Pod Provider SHALL adopt every MicroVM whose
//# labelled pod still exists and is bound to its Virtual Node, without
//# restarting it, and SHALL delete every MicroVM in its namespace whose pod
//# does not.

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# The Pod Provider SHALL NOT delete or restart a MicroVM because
//# the Pod Provider itself stops, restarts or loses its connection to the
//# Kubernetes API.

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The harness SHALL include a scenario in which the Pod Provider
//# restarts while a Job runs, and SHALL assert that the Job's MicroVM is
//# adopted and the Job finishes.

// adopt rebuilds the records from flintlockd and the API server. It is what
// the restart scenario of KF-125 exercises: TestRestartAdoptsRunningMicroVMs
// runs it in this package, and the harness runs it under a Runner. It runs
// before the pod controller, so that when the controller then offers every
// bound pod the provider already knows the ones with a MicroVM and creates
// nothing for them.
//
// A MicroVM is deleted here only on a positive answer: the API server
// returned the list of bound pods and the MicroVM's pod, by UID, is not in
// it. When either flintlockd or the API server cannot be asked adopt fails
// and deletes nothing, and Run retries it; there is no path on which not
// knowing deletes a MicroVM. Nothing in the provider deletes one on the way
// down either: stopping is just stopping.
func (p *Provider) adopt(ctx context.Context) error {
	vms, err := p.host.ListMicroVMs(ctx, p.cfg.MicroVMNamespace)
	if err != nil {
		return fmt.Errorf("listing microvms to adopt: %w", err)
	}
	pods, err := p.boundPods(ctx)
	if err != nil {
		return err
	}
	byUID := make(map[string]*corev1.Pod, len(pods))
	for i := range pods {
		byUID[string(pods[i].UID)] = &pods[i]
	}

	for _, vm := range vms {
		uid := vm.GetSpec().GetUid()
		pod, ok := byUID[vm.GetSpec().GetLabels()[vmLabelPodUID]]
		if !ok {
			p.log.Info("deleting a microvm whose pod no longer exists on this virtual node", "microvm", uid,
				"pod_namespace", vm.GetSpec().GetLabels()[vmLabelPodNamespace], "pod_name", vm.GetSpec().GetLabels()[vmLabelPodName])
			if err := p.host.DeleteMicroVM(ctx, uid); err != nil && !errors.Is(err, flintlock.ErrNotFound) {
				p.log.Warn("could not delete an orphaned microvm; the next sweep tries again", "microvm", uid, "error", err)
			}
			continue
		}
		rec := &podRecord{pod: pod.DeepCopy(), vmUID: uid}
		// The status the API server holds is the one this provider wrote
		// before it restarted. A running pod stays running: it is not
		// probed again, let alone restarted.
		rec.ready = pod.Status.Phase == corev1.PodRunning
		rec.terminal = pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
		if pod.Status.Phase == "" || pod.Status.StartTime == nil {
			rec.pod.Status = p.pendingStatus(rec.pod)
		}
		p.mu.Lock()
		p.pods[podKey(pod.Namespace, pod.Name)] = rec
		p.mu.Unlock()
		p.log.Info("adopted microvm", "microvm", uid, "pod", podKey(pod.Namespace, pod.Name), "phase", string(pod.Status.Phase))
	}
	return nil
}

// sweepOrphans deletes MicroVMs that no record owns and whose pod the API
// server says is gone. It covers the pod that was removed from the API
// server before its MicroVM could be deleted, which is what happens to a pod
// deleted before it ever ran. Like adopt, it deletes on a positive answer
// only (KF-029).
func (p *Provider) sweepOrphans(ctx context.Context) {
	vms, err := p.host.ListMicroVMs(ctx, p.cfg.MicroVMNamespace)
	if err != nil {
		return
	}
	p.mu.Lock()
	owned := make(map[string]bool, len(p.pods))
	for _, rec := range p.pods {
		owned[rec.vmUID] = true
	}
	p.mu.Unlock()

	for _, vm := range vms {
		uid := vm.GetSpec().GetUid()
		if owned[uid] {
			continue
		}
		labels := vm.GetSpec().GetLabels()
		namespace, name := labels[vmLabelPodNamespace], labels[vmLabelPodName]
		if namespace != "" && name != "" {
			pod, err := p.kube.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
			if err == nil && string(pod.UID) == labels[vmLabelPodUID] && pod.Spec.NodeName == p.nodeName {
				continue // the pod controller is about to offer it
			}
			if err != nil && !apierrors.IsNotFound(err) {
				continue // not knowing is not a reason to delete
			}
		}
		p.log.Info("deleting an orphaned microvm", "microvm", uid, "pod_namespace", namespace, "pod_name", name)
		if err := p.host.DeleteMicroVM(ctx, uid); err != nil && !errors.Is(err, flintlock.ErrNotFound) {
			p.log.Warn("could not delete an orphaned microvm", "microvm", uid, "error", err)
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# If a claimed pod's lease annotation is older than the
//# configured lease duration, then the Pod Provider SHALL delete that pod.

// expireLeases deletes every claimed pod whose lease the Runner has stopped
// renewing. Deleting the pod is all it does: the MicroVM follows through
// DeletePod like any other deletion. A claimed pod whose lease annotation
// is missing or unreadable is left to its active deadline, and logged,
// because a pod is not deleted on a guess.
func (p *Provider) expireLeases(ctx context.Context) {
	p.mu.Lock()
	var claimed []*corev1.Pod
	for _, rec := range p.pods {
		if rec.pod.Labels[kubelabels.LabelState] == kubelabels.StateClaimed && rec.pod.DeletionTimestamp == nil && !rec.deleting {
			claimed = append(claimed, rec.pod.DeepCopy())
		}
	}
	p.mu.Unlock()

	for _, pod := range claimed {
		key := podKey(pod.Namespace, pod.Name)
		lease, err := kubelabels.ParseLease(pod.Annotations[kubelabels.AnnotationLease])
		if err != nil {
			p.log.Warn("claimed pod has no readable lease; leaving it to its active deadline", "pod", key, "error", err)
			continue
		}
		age := p.clk.Now().Sub(lease)
		if age <= p.cfg.LeaseDuration {
			continue
		}
		p.log.Warn("deleting a claimed pod whose lease expired", "pod", key, "lease_age", age.String(), "lease_duration", p.cfg.LeaseDuration.String())
		uid := pod.UID
		err = p.kube.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			p.log.Warn("could not delete a pod whose lease expired", "pod", key, "error", err)
		}
	}
}

// podsByState returns the provider's live pods with the given state label.
func (p *Provider) podsByState(state string) []*corev1.Pod {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*corev1.Pod
	for _, rec := range p.pods {
		if rec.pod.Labels[kubelabels.LabelState] == state && !rec.terminal && !rec.deleting {
			out = append(out, rec.pod.DeepCopy())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return podKey(out[i].Namespace, out[i].Name) < podKey(out[j].Namespace, out[j].Name)
	})
	return out
}
