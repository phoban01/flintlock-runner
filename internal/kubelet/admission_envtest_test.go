package kubelet

import (
	"context"
	"fmt"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/phoban01/flintlock-runner/internal/kubelet/kubelettest"
)

// hostAgentToken plays the kubelet of hostNode starting the Host Agent: it
// creates a Host Agent pod bound to that Node and asks the API server for a
// ServiceAccount token bound to the pod, which is what the kubelet projects
// into the pod and what `flr kubelet` authenticates with in-cluster. It
// returns a client that authenticates with that token and nothing else.
func hostAgentToken(t *testing.T, hostNode string) kubernetes.Interface {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "host-agent-" + hostNode, Namespace: agentNamespace},
		Spec: corev1.PodSpec{
			NodeName:           hostNode,
			ServiceAccountName: kubelettest.AgentServiceAccount,
			Containers:         []corev1.Container{{Name: "pod-provider", Image: "flr"}},
		},
	}
	pod, err := suite.Admin.CoreV1().Pods(agentNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the host agent pod on %s: %v", hostNode, err)
	}
	return tokenClient(t, &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID})
}

// tokenClient requests a token of the Host Agent's ServiceAccount, bound to
// the object when one is given, and returns a client that uses only it.
func tokenClient(t *testing.T, bound *authenticationv1.BoundObjectReference) kubernetes.Interface {
	t.Helper()
	ctx := context.Background()
	expiry := int64(600)
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry, BoundObjectRef: bound}}
	tok, err := suite.Admin.CoreV1().ServiceAccounts(agentNamespace).CreateToken(ctx, kubelettest.AgentServiceAccount, req, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("requesting a token: %v", err)
	}
	cfg := rest.AnonymousClientConfig(suite.Config)
	cfg.BearerToken = tok.Status.Token
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// ensureAgentServiceAccount creates the Host Agent's ServiceAccount, which
// the Fleet Manifests would; no controller manager runs to do it here.
func ensureAgentServiceAccount(t *testing.T) {
	t.Helper()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: kubelettest.AgentServiceAccount, Namespace: agentNamespace}}
	if _, err := suite.Admin.CoreV1().ServiceAccounts(agentNamespace).Create(context.Background(), sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-hardening
//= type=test
//# The Fleet Manifests SHALL give each Pod Provider an identity
//# that names its Host, so that the policy of KF-133 can tell one Host's Pod
//# Provider from another's.

// TestHostAgentTokenNamesItsHost shows, on a real API server, that the
// identity of a bound token from a Host Agent pod carries the pod's node in
// the extra the admission policy reads, that CheckIdentity, which `flr
// kubelet` runs at start, accepts it for that Host only, and that a token
// bound to no pod names no Host.
func TestHostAgentTokenNamesItsHost(t *testing.T) {
	t.Parallel()
	if suite == nil {
		t.Skip("no API server test environment: run `make envtest` and set KUBEBUILDER_ASSETS")
	}
	ensureAgentServiceAccount(t)
	ctx := context.Background()
	id := fixtureSeq.Add(1)
	hostA, hostB := fmt.Sprintf("identity-a-%d", id), fmt.Sprintf("identity-b-%d", id)
	for _, name := range []string{hostA, hostB} {
		if _, err := suite.Admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	agentA := hostAgentToken(t, hostA)

	review, err := agentA.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info := review.Status.UserInfo
	t.Logf("a bound token of a Host Agent pod on %s authenticates as %s with extra %v", hostA, info.Username, info.Extra)
	if info.Username != kubelettest.AgentUser {
		t.Errorf("user = %q, want %q", info.Username, kubelettest.AgentUser)
	}
	if got := info.Extra[NodeNameExtra]; len(got) != 1 || got[0] != hostA {
		t.Errorf("extra %s = %v, want [%s]", NodeNameExtra, got, hostA)
	}

	if err := CheckIdentity(ctx, agentA, hostA); err != nil {
		t.Errorf("CheckIdentity on its own Host: %v", err)
	}
	if err := CheckIdentity(ctx, agentA, hostB); err == nil {
		t.Error("CheckIdentity accepted a token of a pod on another Host")
	}
	if err := CheckIdentity(ctx, tokenClient(t, nil), hostA); err == nil {
		t.Error("CheckIdentity accepted a token bound to no pod")
	}
}

// providerChanges makes, as client, every change a Pod Provider makes to the
// objects of the Host whose Node is host, and returns the outcome of each by
// name. The Virtual Node is created by the first step; the pod bound to it,
// `job` in namespace, has to exist beforehand. Admission comes before the
// store, so a refused create of a Virtual Node that exists is refused by
// the policy rather than found to exist.
func providerChanges(client kubernetes.Interface, host, namespace string) (steps []string, outcome map[string]error) {
	ctx := context.Background()
	vnode := VirtualNodeName(host)
	outcome = map[string]error{}
	do := func(step string, err error) {
		steps = append(steps, step)
		outcome[step] = err
	}
	nodes, pods := client.CoreV1().Nodes(), client.CoreV1().Pods(namespace)
	guards := client.CoreV1().Pods(agentNamespace)

	_, err := nodes.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: vnode}}, metav1.CreateOptions{})
	do("create the Virtual Node", err)
	_, err = nodes.Patch(ctx, vnode, k8stypes.MergePatchType, []byte(`{"metadata":{"labels":{"policy-test":"patched"}}}`), metav1.PatchOptions{})
	do("patch the Virtual Node", err)
	_, err = nodes.Patch(ctx, vnode, k8stypes.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{})
	do("cordon the Virtual Node", err)
	_, err = nodes.PatchStatus(ctx, vnode, []byte(`{"status":{"conditions":[{"type":"Ready","status":"True","reason":"PolicyTest"}]}}`))
	do("update the Virtual Node's status", err)
	_, err = pods.Patch(ctx, "job", k8stypes.MergePatchType, []byte(`{"status":{"phase":"Running"}}`), metav1.PatchOptions{}, "status")
	do("update the status of a pod bound to the Virtual Node", err)

	noToken := false
	guard := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: guardNamePrefix + host, Namespace: agentNamespace},
		Spec: corev1.PodSpec{
			NodeName:                     host,
			AutomountServiceAccountToken: &noToken,
			Containers:                   []corev1.Container{{Name: "guard", Image: DefaultGuardImage}},
		},
	}
	_, err = guards.Create(ctx, guard, metav1.CreateOptions{})
	do("create the guard pod", err)
	err = guards.Delete(ctx, guard.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	do("delete the guard pod", err)
	err = pods.Delete(ctx, "job", metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	do("delete a pod bound to the Virtual Node", err)
	return steps, outcome
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The Fleet Manifests SHALL include a test, run against a
//# Kubernetes API server test environment, in which the policy of KF-133
//# admits a Pod Provider's change to its own Host's Virtual Node and pods and
//# refuses the same change to another Host's.
//
//= docs/requirements/12-cluster-fleet.md#cluster-hardening
//= type=test
//# The Fleet Manifests SHALL include a ValidatingAdmissionPolicy
//# that rejects any request by a Pod Provider to create, update, patch or
//# delete a Node other than its own Virtual Node, or a pod that is neither
//# bound to its own Virtual Node nor its own guard pod.

// TestAdmissionPolicyKeepsEachProviderToItsOwnHost runs against the API
// server of this package, which has deploy/host-agent/admission-policy.yaml
// loaded from disk unchanged, with the Pod Providers of two Hosts
// authenticating with real bound tokens of Host Agent pods on their Nodes.
// Every change is one the shipped RBAC allows, so a refusal can only be the
// policy's, and each is checked to be.
func TestAdmissionPolicyKeepsEachProviderToItsOwnHost(t *testing.T) {
	t.Parallel()
	if suite == nil {
		t.Skip("no API server test environment: run `make envtest` and set KUBEBUILDER_ASSETS")
	}
	ensureAgentServiceAccount(t)
	ctx := context.Background()
	admin := suite.Admin.CoreV1()
	id := fixtureSeq.Add(1)
	hostA, hostB := fmt.Sprintf("policy-a-%d", id), fmt.Sprintf("policy-b-%d", id)
	namespace := fmt.Sprintf("runners-policy-%d", id)
	if _, err := admin.Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{hostA, hostB} {
		if _, err := admin.Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: host}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	agentA, agentB := hostAgentToken(t, hostA), hostAgentToken(t, hostB)
	jobOn := func(host string) {
		t.Helper()
		pod := microVMPodForTest()
		pod.ObjectMeta = metav1.ObjectMeta{Name: "job", Namespace: namespace, Annotations: pod.Annotations}
		pod.Spec.NodeName = VirtualNodeName(host)
		if _, err := admin.Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	noToken := false
	guardPod := func(name, node string, automount *bool) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentNamespace},
			Spec: corev1.PodSpec{
				NodeName: node, AutomountServiceAccountToken: automount,
				Containers: []corev1.Container{{Name: "guard", Image: DefaultGuardImage}},
			},
		}
	}

	// Each provider may make every change it makes on its own Host. This
	// also creates both Virtual Nodes.
	for _, own := range []struct {
		name   string
		client kubernetes.Interface
		host   string
	}{{"A on A", agentA, hostA}, {"B on B", agentB, hostB}} {
		jobOn(own.host)
		steps, outcome := providerChanges(own.client, own.host, namespace)
		for _, step := range steps {
			if err := outcome[step]; err != nil {
				t.Errorf("%s: %s: %v, want admitted", own.name, step, err)
			}
		}
	}

	// Host A's provider may make none of them on Host B, and Host B's none
	// on Host A. Every object exists, placed by the administrator, so each
	// refusal is about whose object it is, not whether it is there.
	for _, cross := range []struct {
		name   string
		client kubernetes.Interface
		target string
	}{{"A on B", agentA, hostB}, {"B on A", agentB, hostA}} {
		jobOn(cross.target)
		guard := guardPod(guardNamePrefix+cross.target, cross.target, &noToken)
		if _, err := admin.Pods(agentNamespace).Create(ctx, guard, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		steps, outcome := providerChanges(cross.client, cross.target, namespace)
		for _, step := range steps {
			if err := outcome[step]; !kubelettest.IsPolicyDenial(err) {
				t.Errorf("%s: %s: %v, want refused by the admission policy", cross.name, step, err)
			}
		}
		for _, del := range []struct{ namespace, name string }{{namespace, "job"}, {agentNamespace, guard.Name}} {
			if err := admin.Pods(del.namespace).Delete(ctx, del.name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Nor may a provider change its Host's own Node, or pods that are not
	// its own under the right names.
	refused := map[string]error{}
	_, refused["patch the Host's own Node"] = agentA.CoreV1().Nodes().Patch(ctx, hostA, k8stypes.MergePatchType,
		[]byte(`{"metadata":{"labels":{"policy-test":"patched"}}}`), metav1.PatchOptions{})
	guards := agentA.CoreV1().Pods(agentNamespace)
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	_, refused["create its guard pod on another Node"] = guards.Create(ctx, guardPod(guardNamePrefix+hostA, hostB, &noToken), dryRun)
	_, refused["create a pod of another name on its Host's Node"] = guards.Create(ctx, guardPod("not-the-guard", hostA, &noToken), dryRun)
	_, refused["create its guard pod with a ServiceAccount token"] = guards.Create(ctx, guardPod(guardNamePrefix+hostA, hostA, nil), dryRun)
	withVolume := guardPod(guardNamePrefix+hostA, hostA, &noToken)
	withVolume.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
	_, refused["create its guard pod with a volume"] = guards.Create(ctx, withVolume, dryRun)
	// A token of the Host Agent's ServiceAccount bound to no pod names no
	// Host, and may change nothing at all.
	_, refused["patch a Virtual Node with a token bound to no pod"] = tokenClient(t, nil).CoreV1().Nodes().Patch(ctx, VirtualNodeName(hostA),
		k8stypes.MergePatchType, []byte(`{"metadata":{"labels":{"policy-test":"unbound"}}}`), metav1.PatchOptions{})
	for step, err := range refused {
		if !kubelettest.IsPolicyDenial(err) {
			t.Errorf("%s: %v, want refused by the admission policy", step, err)
		}
	}

	// The policy binds the Pod Provider only: the same change by anyone
	// else is RBAC's business alone.
	if _, err := admin.Nodes().Patch(ctx, VirtualNodeName(hostB), k8stypes.MergePatchType,
		[]byte(`{"metadata":{"labels":{"policy-test":"admin"}}}`), metav1.PatchOptions{}); err != nil {
		t.Errorf("the administrator's change to a Virtual Node: %v", err)
	}
}
