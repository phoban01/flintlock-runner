package kubelet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// virtualNodeSuffix is appended to the name of the Host's Node (KF-010).
const virtualNodeSuffix = "-microvms"

// Well-known label keys copied or set on the Virtual Node.
const (
	labelArch     = "kubernetes.io/arch"
	labelOS       = "kubernetes.io/os"
	labelHostname = "kubernetes.io/hostname"
	labelNodeType = "type"
	nodeTypeValue = "virtual-kubelet"
)

// Reasons of the Virtual Node's Ready condition.
const (
	reasonReady              = "MicroVMsReady"
	reasonHostImageNotReady  = "HostImageNotReady"
	reasonFlintlockdNotReady = "FlintlockdNotReady"
	reasonExecDisabled       = "ExecDisabled"
	reasonHostServiceDown    = "HostServiceUnreachable"
)

// hostServiceDialTimeout bounds one Host Service probe. The service is on
// the Host's own bridge, so anything slower than this is not working.
const hostServiceDialTimeout = time.Second

// serverInfoTimeout bounds the ServerInfo call of one readiness check, so
// that a flintlockd that accepts and never answers is not ready rather than
// a check that never ends.
const serverInfoTimeout = 5 * time.Second

// maxReasonLength caps one not ready reason in the condition's message.
const maxReasonLength = 256

// VirtualNodeName is the name of the Virtual Node of a Host (KF-010).
func VirtualNodeName(hostNode string) string { return hostNode + virtualNodeSuffix }

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL register one Virtual Node for its Host,
//# named after the Host's Node with the suffix `-microvms`, and SHALL renew
//# its node lease while it runs.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL set the Host's Node as the owner of the
//# Virtual Node, so that the Virtual Node is removed when the Host's Node is.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL taint the Virtual Node with
//# `gitlab-runner.flintlock.dev/microvm=true:NoSchedule`, so that only pods
//# meant to be MicroVMs are bound to it.

// buildVirtualNode derives the Virtual Node from the Host's Node and the
// configuration. The node controller of virtual kubelet registers it and
// renews its lease; the object starts not ready and is made ready by the
// first readiness check.
func buildVirtualNode(cfg *Config, host *corev1.Node, kubeletPort int32, version string) (*corev1.Node, error) {
	capacity, err := virtualCapacity(cfg, host)
	if err != nil {
		return nil, err
	}
	name := VirtualNodeName(host.Name)
	arch := host.Labels[labelArch]
	if arch == "" {
		arch = runtime.GOARCH
	}
	now := metav1.Now()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      virtualNodeLabels(host, name, arch),
			Annotations: hostServiceAnnotations(cfg),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       host.Name,
				UID:        host.UID,
			}},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{microVMTaint()},
		},
		Status: corev1.NodeStatus{
			Phase:       corev1.NodeRunning,
			Capacity:    capacity,
			Allocatable: capacity.DeepCopy(),
			Addresses:   virtualNodeAddresses(host),
			DaemonEndpoints: corev1.NodeDaemonEndpoints{
				KubeletEndpoint: corev1.DaemonEndpoint{Port: kubeletPort},
			},
			NodeInfo: corev1.NodeSystemInfo{
				Architecture:    arch,
				OperatingSystem: "linux",
				KubeletVersion:  "flr-kubelet-" + version,
			},
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionFalse,
					Reason:             reasonFlintlockdNotReady,
					Message:            "the Pod Provider has not checked the Host yet",
					LastTransitionTime: now,
				},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, Reason: "NotApplicable", LastTransitionTime: now},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, Reason: "NotApplicable", LastTransitionTime: now},
				{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse, Reason: "NotApplicable", LastTransitionTime: now},
				{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionFalse, Reason: "NotApplicable", LastTransitionTime: now},
			},
		},
	}
	return node, nil
}

// microVMTaint is the taint of KF-014.
func microVMTaint() corev1.Taint {
	return corev1.Taint{Key: kubelabels.TaintMicroVM, Value: "true", Effect: corev1.TaintEffectNoSchedule}
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL copy the architecture and the
//# `gitlab-runner.flintlock.dev` labels of the Host's Node to the Virtual
//# Node and SHALL add the label `gitlab-runner.flintlock.dev/virtual-node`
//# set to `true` and the label `gitlab-runner.flintlock.dev/host-node` set to
//# the name of the Host's Node.

// virtualNodeLabels are the Virtual Node's labels. The project's labels on
// the Host's Node are facts about the machine, such as the image and the
// hypervisor versions of HI-060, which a Pool's node selector places
// against; they are as true of the Virtual Node, which is the same machine.
func virtualNodeLabels(host *corev1.Node, name, arch string) map[string]string {
	labels := map[string]string{
		labelArch:     arch,
		labelOS:       "linux",
		labelHostname: name,
		labelNodeType: nodeTypeValue,
	}
	for key, value := range host.Labels {
		if strings.HasPrefix(key, kubelabels.Prefix) {
			labels[key] = value
		}
	}
	labels[kubelabels.LabelVirtualNode] = "true"
	labels[kubelabels.LabelHostNode] = host.Name
	return labels
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL advertise as the Virtual Node's capacity
//# the Host's CPU and memory minus the configured Host reserve, and a pod
//# limit equal to the configured maximum number of MicroVMs per Host.

// virtualCapacity is the Host's capacity less the Host reserve. It reads the
// Host Node's capacity and not its allocatable, which HI-061 has already
// shrunk to the reserve alone. A reserve larger than the machine leaves
// nothing, rather than a negative quantity the API server would refuse.
func virtualCapacity(cfg *Config, host *corev1.Node) (corev1.ResourceList, error) {
	reserveCPU, err := resource.ParseQuantity(cfg.HostReserve.CPU)
	if err != nil {
		return nil, fmt.Errorf("kubelet: host_reserve.cpu: %w", err)
	}
	reserveMemory, err := resource.ParseQuantity(cfg.HostReserve.Memory)
	if err != nil {
		return nil, fmt.Errorf("kubelet: host_reserve.memory: %w", err)
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    subtract(host.Status.Capacity[corev1.ResourceCPU], reserveCPU),
		corev1.ResourceMemory: subtract(host.Status.Capacity[corev1.ResourceMemory], reserveMemory),
		corev1.ResourcePods:   *resource.NewQuantity(int64(cfg.MaxMicroVMs), resource.DecimalSI),
	}, nil
}

// subtract returns total less reserve, floored at zero.
func subtract(total, reserve resource.Quantity) resource.Quantity {
	out := total.DeepCopy()
	out.Sub(reserve)
	if out.Sign() < 0 {
		return *resource.NewQuantity(0, total.Format)
	}
	return out
}

// virtualNodeAddresses gives the Virtual Node the Host's internal address,
// which is where the API server reaches the kubelet API for an exec, and
// nothing else. In particular it has no hostname address: its name, the
// Host's with a suffix, resolves nowhere, and an API server whose preferred
// address types put Hostname first, as the default order does, would dial
// that name for every exec and fail.
func virtualNodeAddresses(host *corev1.Node) []corev1.NodeAddress {
	if ip := hostInternalIP(host); ip != "" {
		return []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
	}
	return nil
}

// hostInternalIP is the Host's internal address (KF-025).
func hostInternalIP(host *corev1.Node) string {
	for _, addr := range host.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL publish the address and port of each
//# enabled Host Service as annotations on the Virtual Node.

// hostServiceAnnotations is one annotation per enabled Host Service, keyed
// by kubelabels.HostServiceAnnotation and valued `address:port` on the
// bridge gateway, which is what the Executor reads for a Job (KF-062).
func hostServiceAnnotations(cfg *Config) map[string]string {
	out := map[string]string{}
	for _, name := range cfg.EnabledHostServices() {
		out[kubelabels.HostServiceAnnotation(name)] = hostServiceAddress(cfg, name)
	}
	return out
}

// hostServiceAddress is where a Host Service listens.
func hostServiceAddress(cfg *Config, name string) string {
	return net.JoinHostPort(cfg.BridgeGateway, strconv.Itoa(cfg.HostServices[name].Port))
}

// nodeProvider is the node.NodeProvider of the Virtual Node. It keeps the
// node object virtual kubelet patches into the API server and pushes it
// whenever the readiness check changes the Ready condition.
type nodeProvider struct {
	mu     sync.Mutex
	node   *corev1.Node
	notify func(*corev1.Node)
}

// Ping implements node.NodeProvider. Not being able to run MicroVMs is
// reported through the Ready condition with its reason (KF-015, KF-016); a
// failing ping would instead stop status updates and hide the reason, so
// the ping only fails with its context.
func (n *nodeProvider) Ping(ctx context.Context) error { return ctx.Err() }

// NotifyNodeStatus implements node.NodeProvider.
func (n *nodeProvider) NotifyNodeStatus(_ context.Context, cb func(*corev1.Node)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notify = cb
}

// setReady records the outcome of a readiness check and, when it changed
// the condition, hands virtual kubelet a copy of the node to publish.
func (n *nodeProvider) setReady(r readiness) {
	n.mu.Lock()
	status := corev1.ConditionFalse
	if r.ready {
		status = corev1.ConditionTrue
	}
	changed := false
	for i := range n.node.Status.Conditions {
		cond := &n.node.Status.Conditions[i]
		if cond.Type != corev1.NodeReady {
			continue
		}
		if cond.Status != status {
			cond.LastTransitionTime = metav1.Now()
		}
		changed = cond.Status != status || cond.Reason != r.reason || cond.Message != r.message
		cond.Status, cond.Reason, cond.Message = status, r.reason, r.message
	}
	notify := n.notify
	snapshot := n.node.DeepCopy()
	n.mu.Unlock()
	if changed && notify != nil {
		notify(snapshot)
	}
}

// readiness is the outcome of one readiness check.
type readiness struct {
	ready   bool
	reason  string
	message string
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL report the Virtual Node ready only while
//# the local `flintlockd` answers `ServerInfo` with the exec service enabled
//# and every enabled Host Service accepts connections on the bridge gateway
//# address.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# If a unit of the Host Image reports a not ready reason, then the
//# Pod Provider SHALL report the Virtual Node not ready with that reason in
//# the condition's message.

// checkReadiness decides the Ready condition. Every check runs, so that the
// message lists everything that is wrong at once; the reason is that of the
// first failing check, in the order an operator would want to fix them: a
// Host Image unit that gave up (without KVM flintlockd is not even started,
// HI-011), then flintlockd, then the Host Services.
//
// A flintlockd that does not implement ServerInfo is not ready here, unlike
// for the Runner (HO-013): the Host Image pins a flintlockd that has it, and
// without an answer the provider cannot know that exec is enabled.
func checkReadiness(ctx context.Context, cfg *Config, host flintlock.HostClient) readiness {
	var problems []string
	reason := ""
	fail := func(why, message string) {
		if reason == "" {
			reason = why
		}
		problems = append(problems, message)
	}

	reasons, err := ReadNotReadyReasons(cfg.NotReadyDir)
	if err != nil {
		fail(reasonHostImageNotReady, err.Error())
	}
	for _, r := range reasons {
		fail(reasonHostImageNotReady, r.Unit+": "+r.Reason)
	}

	infoCtx, cancel := context.WithTimeout(ctx, serverInfoTimeout)
	info, err := host.ServerInfo(infoCtx)
	cancel()
	switch {
	case err != nil:
		fail(reasonFlintlockdNotReady, "flintlockd does not answer ServerInfo: "+err.Error())
	case !info.Exec.Enabled:
		fail(reasonExecDisabled, "flintlockd reports the exec service disabled")
	}

	for _, name := range cfg.EnabledHostServices() {
		addr := hostServiceAddress(cfg, name)
		if err := dialTCP(ctx, addr); err != nil {
			fail(reasonHostServiceDown, fmt.Sprintf("host service %s does not accept connections on %s: %v", name, addr, err))
		}
	}

	if reason != "" {
		return readiness{reason: reason, message: strings.Join(problems, "; ")}
	}
	return readiness{ready: true, reason: reasonReady, message: "flintlockd answers with exec enabled and every enabled Host Service accepts connections"}
}

// dialTCP succeeds when addr accepts a connection.
func dialTCP(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, hostServiceDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// NotReadyReason is one reason a unit of the Host Image reported.
type NotReadyReason struct {
	// Unit is the name of the file, by convention the reporting unit.
	Unit string
	// Reason is the first line of the file.
	Reason string
}

// ReadNotReadyReasons reads the not-ready-reason contract between the Host
// Image and the Pod Provider (KF-016, HI-011):
//
//   - The directory is Config.NotReadyDir, by default
//     /run/flr/not-ready.d. It is on a tmpfs, so a reboot clears
//     it, and the Host Agent mounts it read-only.
//   - Every regular file in it is one reason for the Host not being ready.
//     The file's name says who reports it, by convention the systemd unit,
//     for example `flintlock-kvm-check.service`; names starting with a dot
//     are ignored so that a writer can create a temporary file and rename it
//     into place.
//   - The first line of the file is the reason in words, for example `KVM is
//     unavailable: /dev/kvm is absent`. An empty file reports `not ready`.
//   - A unit removes its file when the condition clears. An absent or empty
//     directory means no unit has anything to report.
//
// A directory that exists and cannot be read is an error, which the caller
// reports as not ready: the provider does not guess that a Host is fine.
func ReadNotReadyReasons(dir string) ([]NotReadyReason, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the not ready reasons: %w", err)
	}
	var out []NotReadyReason
	for _, entry := range entries {
		if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // removed between the listing and the read
		}
		if err != nil {
			return nil, fmt.Errorf("reading the not ready reason of %s: %w", entry.Name(), err)
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
		line = strings.TrimSpace(line)
		if line == "" {
			line = "not ready"
		}
		if len(line) > maxReasonLength {
			line = line[:maxReasonLength]
		}
		out = append(out, NotReadyReason{Unit: entry.Name(), Reason: line})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Unit < out[j].Unit })
	return out, nil
}
