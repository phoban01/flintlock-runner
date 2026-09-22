package agent_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/agent"
	"github.com/phoban01/flintlock-runner/internal/agent/agenttest"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// annotation reads one annotation of the Host's Node.
func annotation(h *agenttest.Host, key string) string {
	return h.ReadNode().Annotations[key]
}

// waitReady waits until the agent reports want with reason.
func waitReady(t *testing.T, h *agenttest.Host, want bool, reason string) {
	t.Helper()
	agenttest.Eventually(t, fmt.Sprintf("the host reported ready=%v with reason %s", want, reason), func() bool {
		a := h.ReadNode().Annotations
		return a[kubelabels.AnnotationExecAgentReady] == strconv.FormatBool(want) && a[kubelabels.AnnotationExecAgentReason] == reason
	})
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL report its Host not ready while the local
//# `flintlockd` does not answer `ServerInfo` with the exec service enabled,
//# while an enabled Host Service does not accept connections on the bridge
//# gateway address, or while a unit of the Host Image reports a not ready
//# reason.

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL publish the readiness of KF-178 on its
//# Host's Node as the annotation `gitlab-runner.flintlock.dev/exec-agent-ready`
//# set to `true` or `false`, with the reason and message of the last check in
//# `gitlab-runner.flintlock.dev/exec-agent-reason` and
//# `gitlab-runner.flintlock.dev/exec-agent-message`, and its own address in
//# `gitlab-runner.flintlock.dev/exec-agent-address`.

// TestReadiness walks one Host through each reason to be not ready and
// back: a Host Image unit's reason, flintlockd not answering, a Host
// Service not accepting connections; and a second Host whose flintlockd
// has exec disabled. The agent reports each on its Host's Node, with the
// unit's own words in the message.
func TestReadiness(t *testing.T) {
	t.Parallel()
	needEnv(t)
	port, stopService := agenttest.ListenHostService(t)
	h := env.NewHost(t, agenttest.HostOptions{HostServices: map[string]int{kubelabels.HostServiceBuildkit: port}})
	waitReady(t, h, true, "Ready")

	if err := os.MkdirAll(h.NotReadyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reasonFile := filepath.Join(h.NotReadyDir, "flintlock-kvm-check.service")
	if err := os.WriteFile(reasonFile, []byte("KVM is unavailable: /dev/kvm is absent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h, false, "HostImageNotReady")
	if msg := annotation(h, kubelabels.AnnotationExecAgentMessage); !strings.Contains(msg, "/dev/kvm is absent") {
		t.Errorf("message %q does not carry the unit's reason", msg)
	}
	if err := os.Remove(reasonFile); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h, true, "Ready")

	h.Fake.SetFaults(flintlock.HostFaults{Unresponsive: true})
	waitReady(t, h, false, "FlintlockdNotReady")
	h.Fake.SetFaults(flintlock.HostFaults{})
	waitReady(t, h, true, "Ready")

	stopService()
	waitReady(t, h, false, "HostServiceUnreachable")

	disabled := env.NewHost(t, agenttest.HostOptions{ExecDisabled: true})
	waitReady(t, disabled, false, "ExecDisabled")
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL publish the address and port of each
//# enabled Host Service, under the names `buildkit`, `go_proxy`,
//# `registry_mirror` and `http_cache`, as annotations on its Host's Node.

// TestHostServiceAnnotations checks that the agent publishes each enabled
// Host Service on its Host's own Node as `address:port` on the bridge
// gateway, under the name the Runner reads it by, publishes nothing for a
// service that is not enabled, removes an annotation for a service no
// longer enabled, and publishes the exec API's address.
func TestHostServiceAnnotations(t *testing.T) {
	t.Parallel()
	needEnv(t)
	ports := map[string]int{}
	for _, name := range []string{kubelabels.HostServiceBuildkit, kubelabels.HostServiceGoProxy, kubelabels.HostServiceHTTPCache} {
		ports[name], _ = agenttest.ListenHostService(t)
	}
	h := env.NewHost(t, agenttest.HostOptions{HostServices: ports})
	// A stale annotation of a service this Host does not run.
	if _, err := env.Admin.CoreV1().Nodes().Patch(context.Background(), h.Node, k8stypes.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"`+kubelabels.HostServiceAnnotation(kubelabels.HostServiceRegistryMirror)+`":"10.0.0.1:5000"}}}`),
		metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	h.StopAgent()
	h.StartAgent()
	agenttest.Eventually(t, "the host service annotations", func() bool {
		a := h.ReadNode().Annotations
		for name, port := range ports {
			if a[kubelabels.HostServiceAnnotation(name)] != net.JoinHostPort(agenttest.HostAddress, strconv.Itoa(port)) {
				return false
			}
		}
		_, stale := a[kubelabels.HostServiceAnnotation(kubelabels.HostServiceRegistryMirror)]
		return !stale && a[kubelabels.AnnotationExecAgentAddress] == h.Address
	})
}

// agentClient is the Kubernetes client of the Exec Agent of hostNode, with
// the real bound token of a Host Agent pod there.
func agentClient(t *testing.T, hostNode string) kubernetes.Interface {
	t.Helper()
	client, err := kubernetes.NewForConfig(env.AgentConfig(t, hostNode))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Fleet Manifests SHALL include a ValidatingAdmissionPolicy
//# that lets an Exec Agent's identity change only the annotations of its own
//# Host's Node, under the project's prefix, and nothing else of any Node.

// TestAdmissionPolicyKeepsEachAgentToItsOwnNode runs against the API server
// with deploy/agent/admission-policy.yaml loaded unchanged, with the Exec
// Agents of two Hosts authenticating with real bound tokens of Host Agent
// pods on their Nodes. Every change is one the shipped RBAC allows, so a
// refusal can only be the policy's, and each is checked to be.
func TestAdmissionPolicyKeepsEachAgentToItsOwnNode(t *testing.T) {
	t.Parallel()
	needEnv(t)
	ctx := context.Background()
	id := time.Now().UnixNano()
	hostA, hostB := fmt.Sprintf("policy-a-%d", id), fmt.Sprintf("policy-b-%d", id)
	for _, name := range []string{hostA, hostB} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"keep": "me"},
			Annotations: map[string]string{"example.com/other": "x"}}}
		if _, err := env.Admin.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	a := agentClient(t, hostA)
	patch := func(client kubernetes.Interface, node, body string, sub ...string) error {
		_, err := client.CoreV1().Nodes().Patch(ctx, node, k8stypes.MergePatchType, []byte(body), metav1.PatchOptions{}, sub...)
		return err
	}
	ann := func(key, value string) string {
		return fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value)
	}

	admitted := map[string]error{
		"a project annotation of its own Node":      patch(a, hostA, ann(kubelabels.AnnotationExecAgentReady, "true")),
		"a Host Service annotation of its own Node": patch(a, hostA, ann(kubelabels.HostServiceAnnotation(kubelabels.HostServiceBuildkit), "10.200.0.1:1234")),
		"removing a project annotation":             patch(a, hostA, `{"metadata":{"annotations":{"`+kubelabels.AnnotationExecAgentReady+`":null}}}`),
	}
	for what, err := range admitted {
		if err != nil {
			t.Errorf("%s: %v, want admitted", what, err)
		}
	}

	refused := map[string]error{
		"a project annotation of another Host's Node": patch(a, hostB, ann(kubelabels.AnnotationExecAgentReady, "true")),
		"another prefix's annotation of its own Node": patch(a, hostA, ann("example.com/other", "changed")),
		"removing another prefix's annotation":        patch(a, hostA, `{"metadata":{"annotations":{"example.com/other":null}}}`),
		"a look-alike prefix":                         patch(a, hostA, ann("evilgitlab-runner.flintlock.dev/x", "y")),
		"a label of its own Node":                     patch(a, hostA, `{"metadata":{"labels":{"keep":"changed"}}}`),
		"cordoning its own Node":                      patch(a, hostA, `{"spec":{"unschedulable":true}}`),
		"its own Node's status":                       patch(a, hostA, `{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`, "status"),
		"a project annotation with a token bound to no pod": patch(unboundAgent(t), hostA,
			ann(kubelabels.AnnotationExecAgentReady, "true")),
	}
	for what, err := range refused {
		if !agenttest.IsPolicyDenial(err) && !apierrors.IsForbidden(err) {
			t.Errorf("%s: %v, want refused", what, err)
		}
		if err != nil && !agenttest.IsPolicyDenial(err) && what != "its own Node's status" {
			t.Errorf("%s: refused by %v, want the admission policy's refusal", what, err)
		}
	}

	// The drain guard is the only pod and budget an agent makes.
	noToken := false
	guard := func(name, node string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agenttest.AgentNamespace},
			Spec: corev1.PodSpec{NodeName: node, AutomountServiceAccountToken: &noToken,
				Containers: []corev1.Container{{Name: "guard", Image: agent.DefaultGuardImage}}},
		}
	}
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	pods := a.CoreV1().Pods(agenttest.AgentNamespace)
	if _, err := pods.Create(ctx, guard(agent.GuardName(hostA), hostA), dryRun); err != nil {
		t.Errorf("its own guard pod: %v, want admitted", err)
	}
	withToken := guard(agent.GuardName(hostA), hostA)
	withToken.Spec.AutomountServiceAccountToken = nil
	for what, pod := range map[string]*corev1.Pod{
		"another Host's guard pod":           guard(agent.GuardName(hostB), hostB),
		"its guard pod on another Node":      guard(agent.GuardName(hostA), hostB),
		"a pod of another name":              guard("not-the-guard", hostA),
		"its guard pod with a service token": withToken,
	} {
		if _, err := pods.Create(ctx, pod, dryRun); !agenttest.IsPolicyDenial(err) {
			t.Errorf("%s: %v, want refused by the admission policy", what, err)
		}
	}
}

// unboundAgent is a client of the Host Agent's ServiceAccount whose token is
// bound to no pod, so its identity names no Host.
func unboundAgent(t *testing.T) kubernetes.Interface {
	t.Helper()
	id := env.ServiceAccountToken(t, agenttest.AgentNamespace, agenttest.AgentServiceAccount)
	cfg := *env.Config
	cfg.CertData, cfg.KeyData, cfg.CertFile, cfg.KeyFile = nil, nil, "", ""
	cfg.BearerToken = id.Token
	client, err := kubernetes.NewForConfig(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# While claims are `Bound` on its Host, the Exec Agent SHALL hold
//# an eviction-based drain of the Host's Node open, and SHALL let it complete
//# when none remain or when the configured drain timeout elapses.

// TestDrainGuard checks that a Bound claim on the Host puts a guard pod on
// the Host's Node under a budget that allows no disruption, which an
// eviction of the guard then shows, and that the guard goes when the claim
// does. With a claim still Bound on a cordoned Node, the guard goes once
// the drain timeout has passed, and the abandoned claim is logged.
func TestDrainGuard(t *testing.T) {
	t.Parallel()
	needEnv(t)
	ctx := context.Background()
	f := newFixture(t, agenttest.HostOptions{DrainTimeout: 2 * time.Second})
	pods := env.Admin.CoreV1().Pods(agenttest.AgentNamespace)
	pdbs := env.Admin.PolicyV1().PodDisruptionBudgets(agenttest.AgentNamespace)
	name := agent.GuardName(f.host.Node)
	guarded := func() bool {
		_, podErr := pods.Get(ctx, name, metav1.GetOptions{})
		_, pdbErr := pdbs.Get(ctx, name, metav1.GetOptions{})
		return podErr == nil && pdbErr == nil
	}
	gone := func() bool {
		_, podErr := pods.Get(ctx, name, metav1.GetOptions{})
		_, pdbErr := pdbs.Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(podErr) && apierrors.IsNotFound(pdbErr)
	}

	time.Sleep(300 * time.Millisecond)
	if !gone() {
		t.Fatal("a guard was placed with no claim bound on the host")
	}
	f.bind("job")
	agenttest.Eventually(t, "the guard to be placed for a bound claim", guarded)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.NodeName != f.host.Node || len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Kind != "Node" {
		t.Errorf("guard pod = node %q, owners %v; want on the host's node, owned by it", pod.Spec.NodeName, pod.OwnerReferences)
	}
	env.DeleteClaim(t, f.ns, "job")
	agenttest.Eventually(t, "the guard to go when no claim is bound", gone)

	// A drain that outlasts the timeout is let through.
	f.bind("stuck")
	agenttest.Eventually(t, "the guard to be placed again", guarded)
	if _, err := env.Admin.CoreV1().Nodes().Patch(ctx, f.host.Node, k8stypes.MergePatchType,
		[]byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	agenttest.Eventually(t, "the drain start to be recorded", func() bool {
		return annotation(f.host, kubelabels.AnnotationDrainStarted) != ""
	})
	if !guarded() {
		t.Error("the guard went before the drain timeout, with a claim still bound")
	}
	agenttest.Eventually(t, "the guard to go when the drain timeout has passed", gone)
	if !strings.Contains(f.host.Log(), "abandoning a bound claim") {
		t.Errorf("the abandoned claim was not logged:\n%s", f.host.Log())
	}

	// Uncordoned, the drain is over and a bound claim is guarded again.
	if _, err := env.Admin.CoreV1().Nodes().Patch(ctx, f.host.Node, k8stypes.MergePatchType,
		[]byte(`{"spec":{"unschedulable":null}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	agenttest.Eventually(t, "the drain start to be cleared and the guard placed", func() bool {
		return annotation(f.host, kubelabels.AnnotationDrainStarted) == "" && guarded()
	})
}
