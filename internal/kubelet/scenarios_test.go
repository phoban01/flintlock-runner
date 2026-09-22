package kubelet

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL register one Virtual Node for its Host,
//# named after the Host's Node with the suffix `-microvms`, and SHALL renew
//# its node lease while it runs.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL set the Host's Node as the owner of the
//# Virtual Node, so that the Virtual Node is removed when the Host's Node is.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL advertise as the Virtual Node's capacity
//# the Host's CPU and memory minus the configured Host reserve, and a pod
//# limit equal to the configured maximum number of MicroVMs per Host.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL copy the architecture and the
//# `gitlab-runner.flintlock.dev` labels of the Host's Node to the Virtual
//# Node and SHALL add the label `gitlab-runner.flintlock.dev/virtual-node`
//# set to `true` and the label `gitlab-runner.flintlock.dev/host-node` set to
//# the name of the Host's Node.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL taint the Virtual Node with
//# `gitlab-runner.flintlock.dev/microvm=true:NoSchedule`, so that only pods
//# meant to be MicroVMs are bound to it.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL publish the address and port of each
//# enabled Host Service as annotations on the Virtual Node.

func TestVirtualNodeRegistration(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	services := listenOn(t)
	f.cfg.BridgeGateway = "127.0.0.1"
	f.cfg.HostServices = map[string]HostService{
		kubelabels.HostServiceGoProxy:        {Enabled: true, Port: services.port},
		kubelabels.HostServiceRegistryMirror: {Enabled: false, Port: 5000},
	}
	f.start()
	ctx := context.Background()

	node := f.virtualNode()
	if node.Name != f.hostNode+"-microvms" {
		t.Errorf("name = %q", node.Name)
	}

	host, err := suite.Admin.CoreV1().Nodes().Get(ctx, f.hostNode, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(node.OwnerReferences) != 1 || node.OwnerReferences[0].Kind != "Node" ||
		node.OwnerReferences[0].Name != f.hostNode || node.OwnerReferences[0].UID != host.UID {
		t.Errorf("owner references = %+v, want the host's node %s (%s)", node.OwnerReferences, f.hostNode, host.UID)
	}

	for resourceName, want := range map[corev1.ResourceName]string{
		corev1.ResourceCPU: "14", corev1.ResourceMemory: "60Gi", corev1.ResourcePods: "8",
	} {
		wantQ := resource.MustParse(want)
		if got := node.Status.Capacity[resourceName]; got.Cmp(wantQ) != 0 {
			t.Errorf("capacity %s = %s, want %s", resourceName, got.String(), want)
		}
		if got := node.Status.Allocatable[resourceName]; got.Cmp(wantQ) != 0 {
			t.Errorf("allocatable %s = %s, want %s", resourceName, got.String(), want)
		}
	}

	// The only address is the Host's internal one, where the API server
	// dials the kubelet API: the Virtual Node's own name resolves nowhere,
	// so a hostname address would break exec on an API server that prefers
	// it.
	wantAddrs := []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: hostAddress}}
	if !reflect.DeepEqual(node.Status.Addresses, wantAddrs) {
		t.Errorf("addresses = %+v, want only the host's internal address %+v", node.Status.Addresses, wantAddrs)
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeHostName {
			t.Errorf("the virtual node reports the hostname address %q", addr.Address)
		}
	}

	wantLabels := map[string]string{
		labelArch:                   "amd64",
		kubelabels.LabelHost:        "true",
		kubelabels.Prefix + "image": "sha-0a1b2c",
		kubelabels.LabelVirtualNode: "true",
		kubelabels.LabelHostNode:    f.hostNode,
	}
	for key, want := range wantLabels {
		if node.Labels[key] != want {
			t.Errorf("label %s = %q, want %q", key, node.Labels[key], want)
		}
	}
	for _, key := range []string{"example.com/not-this-one", "node.kubernetes.io/instance"} {
		if _, copied := node.Labels[key]; copied {
			t.Errorf("label %s was copied and is not one of the project's", key)
		}
	}

	// The API server adds taints of its own to a new node; the provider's
	// has to be among them.
	want := corev1.Taint{Key: "gitlab-runner.flintlock.dev/microvm", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	found := false
	for _, taint := range node.Spec.Taints {
		found = found || (taint.Key == want.Key && taint.Value == want.Value && taint.Effect == want.Effect)
	}
	if !found {
		t.Errorf("taints = %+v, want %+v among them", node.Spec.Taints, want)
	}

	if got := node.Annotations[kubelabels.HostServiceAnnotation(kubelabels.HostServiceGoProxy)]; got != services.addr {
		t.Errorf("go_proxy annotation = %q, want %q", got, services.addr)
	}
	if _, published := node.Annotations[kubelabels.HostServiceAnnotation(kubelabels.HostServiceRegistryMirror)]; published {
		t.Error("a disabled Host Service was published")
	}
	if node.Status.DaemonEndpoints.KubeletEndpoint.Port == 0 {
		t.Error("the kubelet endpoint port is not published")
	}

	leases := suite.Admin.CoordinationV1().Leases(corev1.NamespaceNodeLease)
	var first *metav1.MicroTime
	f.eventually("the node lease is held", func() bool {
		lease, err := leases.Get(ctx, f.vnode, metav1.GetOptions{})
		if err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != f.vnode || lease.Spec.RenewTime == nil {
			return false
		}
		first = lease.Spec.RenewTime
		return true
	})
	f.eventually("the node lease is renewed", func() bool {
		lease, err := leases.Get(ctx, f.vnode, metav1.GetOptions{})
		return err == nil && lease.Spec.RenewTime != nil && lease.Spec.RenewTime.After(first.Time)
	})
}

// listening is a TCP listener standing in for a Host Service.
type listening struct {
	net.Listener
	addr string
	port int
}

func listenOn(t *testing.T) *listening {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return &listening{Listener: l, addr: l.Addr().String(), port: l.Addr().(*net.TCPAddr).Port}
}

func nodeReady(n *corev1.Node) (bool, corev1.NodeCondition) {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue, c
		}
	}
	return false, corev1.NodeCondition{}
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL report the Virtual Node ready only while
//# the local `flintlockd` answers `ServerInfo` with the exec service enabled
//# and every enabled Host Service accepts connections on the bridge gateway
//# address.

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# If a unit of the Host Image reports a not ready reason, then the
//# Pod Provider SHALL report the Virtual Node not ready with that reason in
//# the condition's message.

func TestVirtualNodeReadinessFollowsTheHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	service := listenOn(t)
	f.cfg.BridgeGateway = "127.0.0.1"
	f.cfg.HostServices = map[string]HostService{kubelabels.HostServiceGoProxy: {Enabled: true, Port: service.port}}
	f.start()

	waitReady := func(want bool, reason, inMessage string) {
		t.Helper()
		f.eventually("virtual node ready="+reason, func() bool {
			ready, cond := nodeReady(f.virtualNode())
			return ready == want && cond.Reason == reason && strings.Contains(cond.Message, inMessage)
		})
	}
	waitReady(true, reasonReady, "")

	_ = service.Close()
	waitReady(false, reasonHostServiceDown, kubelabels.HostServiceGoProxy)
	again, err := net.Listen("tcp", service.addr)
	if err != nil {
		t.Fatalf("listening on %s again: %v", service.addr, err)
	}
	t.Cleanup(func() { _ = again.Close() })
	waitReady(true, reasonReady, "")

	if err := os.MkdirAll(f.cfg.NotReadyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reason := filepath.Join(f.cfg.NotReadyDir, "flr-kvm-check.service")
	if err := os.WriteFile(reason, []byte("KVM is unavailable: /dev/kvm is absent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitReady(false, reasonHostImageNotReady, "flr-kvm-check.service: KVM is unavailable: /dev/kvm is absent")
	if err := os.Remove(reason); err != nil {
		t.Fatal(err)
	}
	waitReady(true, reasonReady, "")

	// flintlockd going away: the fake Host is closed for good, so this is
	// the last step.
	_ = f.fake.Close()
	waitReady(false, reasonFlintlockdNotReady, "ServerInfo")
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# When a pod is bound to the Virtual Node, the Pod Provider SHALL
//# create one MicroVM for it through `flintlockd`, using the image of the
//# pod's only container as the root filesystem image, the container's CPU
//# and memory limits as the MicroVM's vCPU count and memory, and the pod's
//# `gitlab-runner.flintlock.dev` annotations for the kernel image, the
//# kernel command line and the hypervisor.

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# Where the pod names a cloud-init ConfigMap in its annotations,
//# the Pod Provider SHALL pass that ConfigMap's user data to the MicroVM.

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# The Pod Provider SHALL report the pod running and ready only
//# when `flintlockd` reports the MicroVM created and a command run through
//# `MicroVMExec` in the guest succeeds.

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# The Pod Provider SHALL report the Host's internal address as
//# the pod's host address and the guest's bridge address as the pod's
//# address.

func TestPodBecomesRunningMicroVM(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{BootDelay: 300 * time.Millisecond})
	f.host.set(func(g *gatedHost) { g.execDown = true })
	f.start()
	ctx := context.Background()

	const userData = "#cloud-config\nruncmd: [true]\n"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cloud-init", Namespace: f.namespace},
		Data:       map[string]string{kubelabels.CloudInitUserDataKey: userData},
	}
	if _, err := suite.Admin.CoreV1().ConfigMaps(f.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := f.createPod("warm-1", withState(kubelabels.StateIdle), func(p *corev1.Pod) {
		p.Annotations[kubelabels.AnnotationCloudInitConfigMap] = "cloud-init"
	})

	var vm *types.MicroVM
	f.eventually("the microvm is created", func() bool {
		vm = f.microVMOf(pod)
		return vm != nil && vm.GetStatus().GetState() == types.MicroVMStatus_CREATED
	})
	spec := vm.GetSpec()
	if spec.GetRootVolume().GetSource().GetContainerSource() != "ghcr.io/example/rootfs:ubuntu" ||
		spec.GetVcpu() != 2 || spec.GetMemoryInMb() != 2048 ||
		spec.GetKernel().GetImage() != "ghcr.io/example/kernel:6.1" ||
		spec.GetKernel().GetCmdline()["console"] != "ttyS0" || spec.GetProvider() != "firecracker" {
		t.Errorf("microvm spec = %v", spec)
	}
	if got, _ := base64.StdEncoding.DecodeString(spec.GetMetadata()["user-data"]); string(got) != userData {
		t.Errorf("user-data = %q, want the ConfigMap's", got)
	}
	if len(f.microVMs()) != 1 {
		t.Errorf("%d microvms for one pod", len(f.microVMs()))
	}

	// CREATED is not enough: while no command can run in the guest the pod
	// is pending.
	f.consistently("the pod is not running before a command succeeds in the guest", time.Second, func() bool {
		p, err := f.getPod(pod.Name)
		return err == nil && p.Status.Phase != corev1.PodRunning && !podReady(p)
	})
	f.host.set(func(g *gatedHost) { g.execDown = false })
	running := f.waitRunning(pod.Name)

	if running.Status.HostIP != hostAddress {
		t.Errorf("hostIP = %q, want the host's internal address %s", running.Status.HostIP, hostAddress)
	}
	if running.Status.PodIP != guestAddress {
		t.Errorf("podIP = %q, want the guest's address %s", running.Status.PodIP, guestAddress)
	}
	if len(running.Status.ContainerStatuses) != 1 || running.Status.ContainerStatuses[0].State.Running == nil {
		t.Errorf("container statuses = %+v", running.Status.ContainerStatuses)
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# If a pod has more than one container, an init container, a
//# volume, a host namespace or a mounted service account token, then the Pod
//# Provider SHALL mark the pod failed with a reason naming the unsupported
//# field and SHALL NOT create a MicroVM for it.

func TestUnsupportedPodsFailWithoutAMicroVM(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.start()

	names := map[string]string{}
	for field, breakIt := range unsupportedShapes() {
		name := "bad-" + strings.ToLower(strings.TrimPrefix(field, "spec."))
		names[name] = field
		f.createPod(name, podOption(breakIt))
	}
	for name, field := range names {
		pod := f.waitPod(name, "failed", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodFailed })
		if pod.Status.Reason != reasonUnsupportedPod || !strings.Contains(pod.Status.Message, field) {
			t.Errorf("%s: reason %q message %q, want %s naming %s", name, pod.Status.Reason, pod.Status.Message, reasonUnsupportedPod, field)
		}
	}
	if vms := f.microVMs(); len(vms) != 0 || f.host.created() != 0 {
		t.Errorf("%d microvms exist and %d were created for pods that were refused", len(vms), f.host.created())
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# If the MicroVM fails or disappears from `flintlockd`, then the
//# Pod Provider SHALL mark its pod failed with the reason `flintlockd`
//# gives.

func TestMicroVMFailureFailsThePod(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.start()

	healthy := f.createPod("healthy")
	f.waitRunning(healthy.Name)

	f.fake.SetFaults(flintlock.HostFaults{CreateFails: true})
	failing := f.createPod("fails-to-boot")
	got := f.waitPod(failing.Name, "failed", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodFailed })
	if got.Status.Reason != reasonMicroVMFailed || !strings.Contains(got.Status.Message, "FAILED") {
		t.Errorf("reason %q message %q, want %s with flintlockd's state", got.Status.Reason, got.Status.Message, reasonMicroVMFailed)
	}
	f.fake.SetFaults(flintlock.HostFaults{})

	// The running MicroVM disappears from under its pod.
	vm := f.microVMOf(healthy)
	if err := f.fake.Client().DeleteMicroVM(context.Background(), vm.GetSpec().GetUid()); err != nil {
		t.Fatal(err)
	}
	got = f.waitPod(healthy.Name, "failed", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodFailed })
	if got.Status.Reason != reasonMicroVMGone || !strings.Contains(got.Status.Message, "not found") {
		t.Errorf("reason %q message %q, want %s with flintlockd's answer", got.Status.Reason, got.Status.Message, reasonMicroVMGone)
	}
	if f.host.created() != 2 {
		t.Errorf("%d microvms were created; a lost microvm is not replaced under the same pod", f.host.created())
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# When a pod is deleted, the Pod Provider SHALL delete its
//# MicroVM and SHALL report the pod terminated only after `flintlockd`
//# no longer lists the MicroVM.

func TestPodDeletionWaitsForTheMicroVM(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.start()
	pod := f.createPod("doomed")
	f.waitRunning(pod.Name)

	f.host.set(func(g *gatedHost) { g.holdDelete = true })
	if err := suite.Admin.CoreV1().Pods(f.namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.eventually("flintlockd was asked to delete the microvm", func() bool {
		f.host.mu.Lock()
		defer f.host.mu.Unlock()
		return len(f.host.held) == 1
	})
	// flintlockd still lists the MicroVM, well past the pod's grace period
	// of one second: the pod is neither gone nor reported terminated.
	f.consistently("the pod is not terminated while flintlockd lists its microvm", 3*time.Second, func() bool {
		p, err := f.getPod(pod.Name)
		if err != nil || f.microVMOf(pod) == nil {
			return false
		}
		return p.Status.Phase == corev1.PodRunning && p.Status.ContainerStatuses[0].State.Terminated == nil
	})

	f.host.releaseDeletes(t)
	f.waitGone(pod.Name)
	if vm := f.microVMOf(pod); vm != nil {
		t.Errorf("the pod is gone and its microvm %s is not", vm.GetSpec().GetUid())
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# When starting, the Pod Provider SHALL adopt every MicroVM whose
//# labelled pod still exists and is bound to its Virtual Node, without
//# restarting it, and SHALL delete every MicroVM in its namespace whose pod
//# does not.

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# The Pod Provider SHALL NOT delete or restart a MicroVM because
//# the Pod Provider itself stops, restarts or loses its connection to the
//# Kubernetes API.

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The harness SHALL include a scenario in which the Pod Provider
//# restarts while a Job runs, and SHALL assert that the Job's MicroVM is
//# adopted and the Job finishes.

// TestRestartAdoptsRunningMicroVMs is KF-125 at the level of this package:
// the real provider against the API server and the fake Host, with the Job
// played by exec sessions through the kubelet API. Running it under the
// Runner, in internal/testing/harness, belongs to the kube-harness work
// package.
func TestRestartAdoptsRunningMicroVMs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.start()
	ctx := context.Background()

	job := f.createPod("job", withState(kubelabels.StateClaimed), withLease(time.Now()))
	f.waitRunning(job.Name)
	before := f.microVMOf(job)

	// The Job's first stage leaves state in the guest that only the same
	// MicroVM can still have afterwards.
	if code, _, err := f.exec(job.Name, f.certs, nil, "sh", "-c", "echo stage-one > marker"); err != nil || code != 0 {
		t.Fatalf("first stage: exit %d, %v", code, err)
	}

	f.stop()
	if got := f.microVMOf(job); got == nil || got.GetSpec().GetUid() != before.GetSpec().GetUid() {
		t.Fatal("stopping the provider deleted or replaced the job's microvm")
	}

	// While the provider is down: a MicroVM is left behind by a pod that no
	// longer exists, and another Host namespace has a MicroVM of its own.
	admin := f.fake.Client()
	orphan, err := admin.CreateMicroVM(ctx, &types.MicroVMSpec{
		Id: "orphan", Namespace: f.cfg.MicroVMNamespace,
		Labels: map[string]string{vmLabelPodUID: "00000000-dead-4000-8000-000000000000", vmLabelPodNamespace: f.namespace, vmLabelPodName: "long-gone"},
	})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := admin.CreateMicroVM(ctx, &types.MicroVMSpec{Id: "foreign", Namespace: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("an unreachable API server deletes nothing", func(t *testing.T) {
		nowhere, err := kubernetes.NewForConfig(&rest.Config{Host: "https://127.0.0.1:1", Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		p := &Provider{cfg: f.cfg, log: slog.New(slog.DiscardHandler), clk: clock.Real{}, host: f.host, kube: nowhere, nodeName: f.vnode, pods: map[string]*podRecord{}}
		if err := p.adopt(ctx); err == nil {
			t.Fatal("adopt succeeded without the API server")
		}
		p.sweepOrphans(ctx)
		if n := len(f.microVMs()); n != 2 {
			t.Fatalf("%d microvms are left of 2: not knowing deleted one", n)
		}
	})

	created := f.host.created()
	f.start()

	f.eventually("the orphan is deleted", func() bool {
		_, err := admin.GetMicroVM(ctx, orphan.GetSpec().GetUid())
		return errors.Is(err, flintlock.ErrNotFound)
	})
	if _, err := admin.GetMicroVM(ctx, foreign.GetSpec().GetUid()); err != nil {
		t.Errorf("a microvm in another namespace was touched: %v", err)
	}
	if !strings.Contains(f.logs.String(), "adopted microvm") {
		t.Error("the provider did not log the adoption")
	}

	// The Job's next stage runs in the same machine, and the Job finishes.
	code, out, err := f.exec(job.Name, f.certs, nil, "cat", "marker")
	if err != nil || code != 0 || strings.TrimSpace(out) != "stage-one" {
		t.Fatalf("second stage after the restart: exit %d, output %q, %v", code, out, err)
	}
	after := f.microVMOf(job)
	if after == nil || after.GetSpec().GetUid() != before.GetSpec().GetUid() ||
		!after.GetSpec().GetCreatedAt().AsTime().Equal(before.GetSpec().GetCreatedAt().AsTime()) {
		t.Errorf("the job's microvm changed across the restart: %v -> %v", before.GetSpec().GetUid(), after.GetSpec().GetUid())
	}
	if f.host.created() != created {
		t.Errorf("the restart created %d microvms; an adopted microvm is not created again", f.host.created()-created)
	}
	if p, _ := f.getPod(job.Name); p == nil || p.Status.Phase != corev1.PodRunning || !podReady(p) {
		t.Errorf("the job's pod is not running and ready after the restart: %+v", p)
	}
}

// exec runs a command in a pod through the provider's kubelet API, as the
// API server does for `pods/exec`, with the given client certificate.
func (f *fixture) exec(pod string, certs *hostfake.TestCerts, stdin []byte, command ...string) (exit int, stdout string, err error) {
	f.t.Helper()
	cfg := &rest.Config{Host: "https://" + f.addr}
	cfg.CAFile = f.certs.CAFile
	if certs != nil {
		cfg.CertFile, cfg.KeyFile = certs.ClientCertFile, certs.ClientKeyFile
	}
	query := url.Values{"command": command, "output": {"1"}, "error": {"1"}}
	if stdin != nil {
		query.Set("input", "1")
	}
	target := &url.URL{Scheme: "https", Host: f.addr, Path: "/exec/" + f.namespace + "/" + pod + "/microvm", RawQuery: query.Encode()}
	executor, err := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, target)
	if err != nil {
		return -1, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	opts := remotecommand.StreamOptions{Stdout: &out, Stderr: &errOut}
	if stdin != nil {
		opts.Stdin = bytes.NewReader(stdin)
	}
	err = executor.StreamWithContext(ctx, opts)
	var exitErr utilexec.CodeExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code, out.String() + errOut.String(), nil
	}
	if err != nil {
		return -1, "", err
	}
	return 0, out.String() + errOut.String(), nil
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# The Pod Provider SHALL serve the pod exec endpoint of the
//# kubelet API by relaying the streams to `MicroVMExec.ExecCommand` on the
//# local `flintlockd` and SHALL return the command's exit status.

func TestExecRelaysStreamsAndExitStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.start()
	pod := f.createPod("job")
	f.waitRunning(pod.Name)

	code, out, err := f.exec(pod.Name, f.certs, []byte("from stdin\n"), "sh", "-c", "cat; echo to-stderr >&2; exit 7")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code != 7 {
		t.Errorf("exit status = %d, want 7", code)
	}
	if !strings.Contains(out, "from stdin") || !strings.Contains(out, "to-stderr") {
		t.Errorf("output = %q, want stdin echoed and stderr relayed", out)
	}

	if code, out, err = f.exec(pod.Name, f.certs, nil, "echo", "hello"); err != nil || code != 0 || strings.TrimSpace(out) != "hello" {
		t.Errorf("plain command: exit %d, output %q, %v", code, out, err)
	}
	// The command really ran in the pod's MicroVM: its sandbox is where the
	// fake Host roots every process of that MicroVM.
	if _, _, err := f.exec(pod.Name, f.certs, nil, "touch", "was-here"); err != nil {
		t.Fatal(err)
	}
	sandbox, _ := f.fake.SandboxPath(f.microVMOf(pod).GetSpec().GetUid())
	if _, err := os.Stat(filepath.Join(sandbox, "was-here")); err != nil {
		t.Errorf("the command did not run in the pod's microvm: %v", err)
	}

	if _, _, err := f.exec("no-such-pod", f.certs, nil, "true"); err == nil {
		t.Error("exec in a pod the provider does not have succeeded")
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# The Pod Provider SHALL serve its kubelet API over TLS and SHALL
//# reject every request that does not authenticate with a client certificate
//# issued by the cluster's kubelet client certificate authority.

func TestKubeletAPIRequiresAClientCertificateOfTheConfiguredCA(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.start()
	pod := f.createPod("job")
	f.waitRunning(pod.Name)

	otherCA, err := hostfake.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := func(name string) []string { return []string{"touch", name} }
	sandbox, _ := f.fake.SandboxPath(f.microVMOf(pod).GetSpec().GetUid())
	ran := func(name string) bool {
		_, err := os.Stat(filepath.Join(sandbox, name))
		return err == nil
	}

	if _, _, err := f.exec(pod.Name, nil, nil, marker("no-cert")...); err == nil {
		t.Error("exec without a client certificate succeeded")
	}
	if _, _, err := f.exec(pod.Name, otherCA, nil, marker("wrong-ca")...); err == nil {
		t.Error("exec with a client certificate of another authority succeeded")
	}
	if ran("no-cert") || ran("wrong-ca") {
		t.Fatal("a rejected request ran its command in the guest")
	}

	// Every route is behind the same door, not only exec.
	for name, certs := range map[string]*hostfake.TestCerts{"no certificate": nil, "another authority's certificate": otherCA} {
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS(t, f.certs, certs)}}
		resp, err := client.Get("https://" + f.addr + "/pods")
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("GET /pods with %s: status %d, want the handshake refused", name, resp.StatusCode)
		}
	}
	// And it is TLS only.
	plain := &http.Client{Timeout: 5 * time.Second}
	if resp, err := plain.Get("http://" + f.addr + "/pods"); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("the kubelet API answered a plaintext request")
		}
	}

	// The handler refuses on its own as well, should a listener ever be
	// built without the handshake check.
	rec := &statusRecorder{header: http.Header{}}
	requireClientCertificate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(rec, &http.Request{TLS: &tls.ConnectionState{}})
	if rec.status != http.StatusUnauthorized {
		t.Errorf("a request with no verified chain got status %d, want 401", rec.status)
	}

	if code, _, err := f.exec(pod.Name, f.certs, nil, marker("right-cert")...); err != nil || code != 0 || !ran("right-cert") {
		t.Errorf("exec with the right certificate: exit %d, %v", code, err)
	}
}

type statusRecorder struct {
	header http.Header
	status int
}

func (r *statusRecorder) Header() http.Header         { return r.header }
func (r *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *statusRecorder) WriteHeader(status int)      { r.status = status }

// clientTLS trusts the server's CA and presents client's certificate, if any.
func clientTLS(t *testing.T, server, client *hostfake.TestCerts) *tls.Config {
	t.Helper()
	cfg, err := rest.TLSConfigFor(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAFile: server.CAFile}})
	if err != nil {
		t.Fatal(err)
	}
	if client != nil {
		cert, err := tls.LoadX509KeyPair(client.ClientCertFile, client.ClientKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# If a claimed pod's lease annotation is older than the
//# configured lease duration, then the Pod Provider SHALL delete that pod.

func TestExpiredLeaseDeletesTheClaimedPod(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.cfg.LeaseDuration = 3 * time.Second
	f.start()
	ctx := context.Background()

	idle := f.createPod("idle", withState(kubelabels.StateIdle))
	abandoned := f.createPod("abandoned", withState(kubelabels.StateClaimed), withLease(time.Now()))
	renewed := f.createPod("renewed", withState(kubelabels.StateClaimed), withLease(time.Now()))
	for _, p := range []*corev1.Pod{idle, abandoned, renewed} {
		f.waitRunning(p.Name)
	}

	// One Runner keeps its heartbeat going (KF-047); the other has died.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
			patch := `{"metadata":{"annotations":{"` + kubelabels.AnnotationLease + `":"` + kubelabels.FormatLease(time.Now()) + `"}}}`
			_, _ = suite.Admin.CoreV1().Pods(f.namespace).Patch(ctx, renewed.Name, k8stypes.MergePatchType, []byte(patch), metav1.PatchOptions{})
		}
	}()

	f.waitGone(abandoned.Name)
	if vm := f.microVMOf(abandoned); vm != nil {
		t.Error("the expired pod's microvm is still there")
	}
	for _, p := range []*corev1.Pod{idle, renewed} {
		if got, err := f.getPod(p.Name); err != nil || got.DeletionTimestamp != nil {
			t.Errorf("pod %s was deleted or is being deleted: %v", p.Name, err)
		}
	}
	if !strings.Contains(f.logs.String(), "lease expired") {
		t.Error("the expiry was not logged")
	}
}

// guardObjects reports whether the drain guard pod and its budget exist.
func (f *fixture) guardObjects() (pod *corev1.Pod, budget bool) {
	f.t.Helper()
	ctx := context.Background()
	name := guardNamePrefix + f.hostNode
	p, err := suite.Admin.CoreV1().Pods(agentNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		f.t.Fatal(err)
	}
	if err != nil {
		p = nil
	}
	_, err = suite.Admin.PolicyV1().PodDisruptionBudgets(agentNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		f.t.Fatal(err)
	}
	return p, err == nil
}

func (f *fixture) cordonHost(unschedulable bool) {
	f.t.Helper()
	patch := `{"spec":{"unschedulable":true}}`
	if !unschedulable {
		patch = `{"spec":{"unschedulable":null}}`
	}
	if _, err := suite.Admin.CoreV1().Nodes().Patch(context.Background(), f.hostNode, k8stypes.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-drain
//= type=test
//# When the Host's Node becomes unschedulable, the Pod Provider
//# SHALL mark the Virtual Node unschedulable and SHALL delete the idle pods
//# bound to it, so that their ReplicaSets replace them on other Hosts.

//= docs/requirements/12-cluster-fleet.md#cluster-drain
//= type=test
//# While a claimed pod is bound to the Virtual Node, the Pod
//# Provider SHALL prevent an eviction-based drain of the Host's Node from
//# completing.

//= docs/requirements/12-cluster-fleet.md#cluster-drain
//= type=test
//# When no claimed pod remains on an unschedulable Host, the Pod
//# Provider SHALL let the drain of the Host's Node complete.

//= docs/requirements/12-cluster-fleet.md#cluster-drain
//= type=test
//# When the Host's Node becomes schedulable again, the Pod
//# Provider SHALL mark the Virtual Node schedulable.

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The harness SHALL include a scenario in which a Host's Node is
//# cordoned while it runs a Job, and SHALL assert that the Job finishes, that
//# the idle pods leave the Host and that the drain completes afterwards.

// TestCordonDrainsAroundARunningJob is KF-124 at the level of this package.
// The test is the Runner (it claims, runs stages and releases), the
// operator (it cordons and evicts) and the Host's kubelet (it reports the
// guard pod running). Running it under the Runner, in
// internal/testing/harness, belongs to the kube-harness work package.
func TestCordonDrainsAroundARunningJob(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.start()
	ctx := context.Background()

	idle := f.createPod("idle", withState(kubelabels.StateIdle))
	job := f.createPod("job", withState(kubelabels.StateIdle))
	f.waitRunning(idle.Name)
	f.waitRunning(job.Name)
	if pod, budget := f.guardObjects(); pod != nil || budget {
		t.Fatal("a guard exists with no claimed pod")
	}

	// The Runner claims one pod (KF-044). The guard appears with the claim,
	// before any drain, so that a drain never finds the Host unguarded.
	claim := `{"metadata":{"labels":{"` + kubelabels.LabelState + `":"claimed"},"annotations":{"` + kubelabels.AnnotationLease + `":"` + kubelabels.FormatLease(time.Now()) + `"}}}`
	if _, err := suite.Admin.CoreV1().Pods(f.namespace).Patch(ctx, job.Name, k8stypes.MergePatchType, []byte(claim), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	var guard *corev1.Pod
	f.eventually("the guard pod and its budget exist", func() bool {
		pod, budget := f.guardObjects()
		guard = pod
		return pod != nil && budget
	})
	if guard.Spec.NodeName != f.hostNode {
		t.Errorf("the guard is on %q, want the host's own node %s", guard.Spec.NodeName, f.hostNode)
	}
	// The Host's kubelet runs the guard.
	guard.Status.Phase = corev1.PodRunning
	guard.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := suite.Admin.CoreV1().Pods(agentNamespace).UpdateStatus(ctx, guard, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	f.cordonHost(true)
	f.eventually("the virtual node is cordoned", func() bool { return f.virtualNode().Spec.Unschedulable })
	f.waitGone(idle.Name)
	if vm := f.microVMOf(idle); vm != nil {
		t.Error("the idle pod left and its microvm did not")
	}

	// The drain evicts what is on the Host's Node; the guard refuses.
	// A refusal is a 429, which client-go would retry for a minute; one
	// attempt is the answer wanted here.
	evict := func() error {
		return suite.Admin.CoreV1().RESTClient().Post().
			Namespace(agentNamespace).Resource("pods").Name(guard.Name).SubResource("eviction").
			Body(&policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: guard.Name, Namespace: agentNamespace}}).
			MaxRetries(0).Do(ctx).Error()
	}
	if err := evict(); !apierrors.IsTooManyRequests(err) {
		t.Fatalf("evicting the guard = %v, want it refused by the disruption budget", err)
	}

	// Meanwhile the Job is untouched and runs to its end.
	f.consistently("the claimed pod keeps running on a cordoned host", time.Second, func() bool {
		p, err := f.getPod(job.Name)
		return err == nil && p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning
	})
	if code, out, err := f.exec(job.Name, f.certs, nil, "echo", "job done"); err != nil || code != 0 || !strings.Contains(out, "job done") {
		t.Fatalf("the job's stage on a cordoned host: exit %d, %q, %v", code, out, err)
	}
	if err := evict(); !apierrors.IsTooManyRequests(err) {
		t.Fatalf("evicting the guard while the job runs = %v, want it refused", err)
	}

	// The Runner releases the Lease (KF-048); nothing holds the drain now.
	if err := suite.Admin.CoreV1().Pods(f.namespace).Delete(ctx, job.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.eventually("the guard and its budget are removed, so the drain completes", func() bool {
		pod, budget := f.guardObjects()
		return pod == nil && !budget
	})
	f.waitGone(job.Name)
	if n := len(f.microVMs()); n != 0 {
		t.Errorf("%d microvms are left on a drained host", n)
	}

	f.cordonHost(false)
	f.eventually("the virtual node is schedulable again", func() bool { return !f.virtualNode().Spec.Unschedulable })
}

//= docs/requirements/12-cluster-fleet.md#cluster-drain
//= type=test
//# If the configured drain timeout elapses while claimed pods
//# remain, then the Pod Provider SHALL let the drain complete and SHALL log
//# each pod it abandoned.

func TestDrainTimeoutAbandonsClaimedPods(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	f.cfg.DrainTimeout = 2 * time.Second
	f.start()

	first := f.createPod("job-a", withState(kubelabels.StateClaimed), withLease(time.Now()))
	second := f.createPod("job-b", withState(kubelabels.StateClaimed), withLease(time.Now()))
	f.waitRunning(first.Name)
	f.waitRunning(second.Name)
	f.eventually("the guard exists", func() bool { pod, budget := f.guardObjects(); return pod != nil && budget })

	f.cordonHost(true)
	f.eventually("the virtual node is cordoned", func() bool { return f.virtualNode().Spec.Unschedulable })
	if pod, _ := f.guardObjects(); pod == nil {
		t.Fatal("the guard was removed as soon as the host was cordoned")
	}

	f.eventually("the guard is removed at the drain timeout", func() bool {
		pod, budget := f.guardObjects()
		return pod == nil && !budget
	})
	logs := f.logs.String()
	for _, p := range []*corev1.Pod{first, second} {
		if !strings.Contains(logs, "abandoning a claimed pod") || !strings.Contains(logs, "pod="+f.namespace+"/"+p.Name) {
			t.Errorf("abandoning %s was not logged", p.Name)
		}
		if got, err := f.getPod(p.Name); err != nil || got.DeletionTimestamp != nil {
			t.Errorf("the provider deleted the claimed pod %s itself: %v", p.Name, err)
		}
	}
	if n := strings.Count(logs, "abandoning a claimed pod"); n != 2 {
		t.Errorf("%d abandonment lines for 2 pods", n)
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-least-privilege
//= type=test
//# The Pod Provider SHALL operate with permissions limited to its
//# own Virtual Node and node lease, the pods bound to that Virtual Node, the
//# ConfigMaps those pods name, reading its Host's Node and managing its guard
//# pod.

// TestProviderHoldsOnlyTheShippedRBAC covers the half of KF-111 this test
// can see directly: what the shipped manifest refuses. The other half, that
// the code stays within it, is every other scenario of this package: they
// all run the provider as the Host Agent's ServiceAccount under exactly
// deploy/host-agent/rbac.yaml, on an API server that enforces RBAC, so a
// call outside the manifest fails the scenario that needs it.
func TestProviderHoldsOnlyTheShippedRBAC(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	ctx := context.Background()
	agent := suite.Agent

	denied := map[string]error{}
	_, denied["list secrets"] = agent.CoreV1().Secrets(f.namespace).List(ctx, metav1.ListOptions{})
	_, denied["list configmaps"] = agent.CoreV1().ConfigMaps(f.namespace).List(ctx, metav1.ListOptions{})
	_, denied["list services"] = agent.CoreV1().Services(f.namespace).List(ctx, metav1.ListOptions{})
	_, denied["create a pool pod"] = agent.CoreV1().Pods(f.namespace).Create(ctx, microVMPodForTest(), metav1.CreateOptions{})
	_, denied["edit a pod"] = agent.CoreV1().Pods(f.namespace).Patch(ctx, "any", k8stypes.MergePatchType, []byte(`{}`), metav1.PatchOptions{})
	denied["delete a node"] = agent.CoreV1().Nodes().Delete(ctx, f.hostNode, metav1.DeleteOptions{})
	_, denied["list replicasets"] = agent.AppsV1().ReplicaSets(f.namespace).List(ctx, metav1.ListOptions{})
	_, denied["list leases outside kube-node-lease"] = agent.CoordinationV1().Leases(f.namespace).List(ctx, metav1.ListOptions{})
	_, denied["read rbac"] = agent.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
	_, denied["create a budget outside the agent's namespace"] = agent.PolicyV1().PodDisruptionBudgets(f.namespace).Create(ctx,
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "x"}}, metav1.CreateOptions{})
	for what, err := range denied {
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s: %v, want forbidden", what, err)
		}
	}
}
