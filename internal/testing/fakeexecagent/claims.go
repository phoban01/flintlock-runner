package fakeexecagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/pkg/claimclient"
)

// pollInterval is how often Claim looks for the claim it has to bind.
const pollInterval = 10 * time.Millisecond

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//# The `agent-exec` Guest Transport SHALL be tested with
//# battery-operator's Client Library against a test double of
//# battery-operator's Exec Agent in front of the fake Host, with no KVM and
//# no battery.

// World is the API server of the claim design, for tests: controller-
// runtime's fake client, serving battery-operator's Pools and
// MicroVMClaims, the claims' Secrets and TokenRequests, which is what
// battery-operator's Client Library needs to claim, hold, dial and release
// a MicroVM. It binds claims itself, in battery's place, and it is the
// Authorizer of the Agents that serve them: it accepts a token only as the
// Exec Agent does (EA-010 to EA-013), a claim token of the claim's Holder,
// bound to the claim's Secret, of a Bound, unexpired claim that names the
// MicroVM and the Host.
type World struct {
	// Namespace is the namespace of the claims, their Pools, their Secrets
	// and their Holders.
	Namespace string
	// Kube is the client battery-operator's Client Library is given.
	Kube client.Client

	mu     sync.Mutex
	uids   int
	grants map[string]grant
}

// grant is one claim token the World issued.
type grant struct {
	holder    k8stypes.NamespacedName
	secret    string
	secretUID k8stypes.UID
	audiences []string
	expires   time.Time
}

// NewWorld returns a World in namespace that serves the named Pools, each
// with the heartbeat interval of battery-operator's CRD default.
func NewWorld(namespace string, pools ...string) (*World, error) {
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := batteryv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	w := &World{Namespace: namespace, grants: map[string]grant{}}
	objs := make([]client.Object, 0, len(pools))
	for _, name := range pools {
		objs = append(objs, &batteryv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}})
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		Build()
	w.Kube = interceptor.NewClient(base, interceptor.Funcs{
		Create:            w.create,
		SubResourceCreate: w.subResourceCreate,
	})
	return w, nil
}

// create gives an object the uid an API server would.
func (w *World) create(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		w.mu.Lock()
		w.uids++
		obj.SetUID(k8stypes.UID(fmt.Sprintf("uid-%d", w.uids)))
		w.mu.Unlock()
	}
	return c.Create(ctx, obj, opts...)
}

// subResourceCreate answers a TokenRequest with a token of its own, bound
// to the Secret the request names, as the API server does.
func (w *World) subResourceCreate(ctx context.Context, c client.Client, sub string, obj, subObj client.Object,
	opts ...client.SubResourceCreateOption,
) error {
	tr, ok := subObj.(*authenticationv1.TokenRequest)
	if sub != "token" || !ok {
		return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
	}
	ref := tr.Spec.BoundObjectRef
	if ref == nil || ref.Kind != "Secret" {
		return apierrors.NewBadRequest("a claim token is bound to the claim's Secret")
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: ref.Name}, secret); err != nil {
		return err
	}
	if secret.UID != ref.UID {
		return apierrors.NewConflict(corev1.Resource("secrets"), ref.Name, errors.New("the bound object's uid differs"))
	}
	lifetime := 10 * time.Minute
	if s := tr.Spec.ExpirationSeconds; s != nil && *s > 0 {
		lifetime = time.Duration(*s) * time.Second
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	token := fmt.Sprintf("claim-token-%d", len(w.grants)+1)
	expires := time.Now().Add(lifetime)
	w.grants[token] = grant{
		holder:    client.ObjectKeyFromObject(obj),
		secret:    ref.Name,
		secretUID: ref.UID,
		audiences: slices.Clone(tr.Spec.Audiences),
		expires:   expires,
	}
	tr.Status.Token = token
	tr.Status.ExpirationTimestamp = metav1.NewTime(expires)
	return nil
}

// Tokens returns every claim token the World issued for the claim name,
// oldest first.
func (w *World) Tokens(name string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for token, g := range w.grants {
		if g.secret == batteryv1alpha1.ExecSecretName(name) {
			out = append(out, token)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		var x, y int
		_, _ = fmt.Sscanf(a, "claim-token-%d", &x)
		_, _ = fmt.Sscanf(b, "claim-token-%d", &y)
		return x - y
	})
	return out
}

// Authorize implements Authorizer, as battery-operator's Exec Agent
// decides: the token has to be one the World issued for the Exec Agent's
// audience, not expired, bound to a Secret that still exists with the same
// uid (EA-010, EA-012); the Secret's claim has to name the token's
// ServiceAccount as its Holder (EA-011); and, for a MicroVM, the claim has
// to be Bound, unexpired and name that MicroVM and this Host (EA-013).
func (w *World) Authorize(ctx context.Context, token, node, vmUID string) error {
	w.mu.Lock()
	g, ok := w.grants[token]
	w.mu.Unlock()
	if !ok || time.Now().After(g.expires) || !slices.Contains(g.audiences, batteryv1alpha1.ExecAgentTokenAudience) {
		return ErrUnauthenticated
	}
	secret := &corev1.Secret{}
	switch err := w.Kube.Get(ctx, client.ObjectKey{Namespace: g.holder.Namespace, Name: g.secret}, secret); {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: the token's Secret %s is gone", ErrUnauthenticated, g.secret)
	case err != nil:
		return err
	case secret.UID != g.secretUID:
		return fmt.Errorf("%w: the token's Secret %s was replaced", ErrUnauthenticated, g.secret)
	}
	return claimOpens(ctx, w.Kube, g.holder, g.secret, node, vmUID)
}

// claimOpens decides, as battery-operator's Exec Agent does once a token
// has authenticated, whether a claim token of holder, bound to the Secret
// secret in the holder's namespace, opens the MicroVM vmUID on the Host
// whose Node is node: the Secret has to be a claim's, the claim has to name
// holder as its Holder (EA-011) and, for a MicroVM, be Bound, unexpired and
// name that MicroVM and this Host (EA-013). An empty vmUID asks only
// whether the token is a claim token.
func claimOpens(ctx context.Context, kube client.Client, holder k8stypes.NamespacedName, secret, node, vmUID string) error {
	name, ok := strings.CutSuffix(secret, batteryv1alpha1.ExecSecretSuffix)
	if !ok {
		return fmt.Errorf("%w: the token is bound to %s, which is no claim's Secret", ErrPermissionDenied, secret)
	}
	claim := &batteryv1alpha1.MicroVMClaim{}
	switch err := kube.Get(ctx, client.ObjectKey{Namespace: holder.Namespace, Name: name}, claim); {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: claim %s is gone", ErrPermissionDenied, name)
	case err != nil:
		return err
	}
	if claim.Spec.ServiceAccountName != holder.Name {
		return fmt.Errorf("%w: %s is not the Holder of claim %s", ErrPermissionDenied, holder.Name, name)
	}
	if vmUID == "" {
		return nil
	}
	st := claim.Status
	switch {
	case st.Phase != batteryv1alpha1.MicroVMClaimBound:
		return fmt.Errorf("%w: claim %s is %q", ErrPermissionDenied, name, st.Phase)
	case st.LeaseExpiresAt != nil && time.Now().After(st.LeaseExpiresAt.Time):
		return fmt.Errorf("%w: the lease of claim %s has expired", ErrPermissionDenied, name)
	case st.MicroVM == nil || st.MicroVM.UID != vmUID:
		return fmt.Errorf("%w: claim %s does not name microvm %s", ErrPermissionDenied, name, vmUID)
	case st.Host == nil || st.Host.NodeName != node:
		return fmt.Errorf("%w: claim %s does not name host %s", ErrPermissionDenied, name, node)
	}
	return nil
}

// Bind binds the claim name to the MicroVM vmUID behind agent, as battery
// would: Bound, with the lease id, the MicroVM, the Host's Node and the
// agent's address, and a lease an hour long.
func (w *World) Bind(ctx context.Context, name, vmUID string, agent *Agent) error {
	claim := &batteryv1alpha1.MicroVMClaim{}
	if err := w.Kube.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, claim); err != nil {
		return err
	}
	now := metav1.Now()
	expires := metav1.NewTime(now.Add(time.Hour))
	claim.Status.Phase = batteryv1alpha1.MicroVMClaimBound
	claim.Status.LeaseID = "lease-" + name
	claim.Status.MicroVM = &batteryv1alpha1.MicroVMReference{UID: vmUID}
	claim.Status.Host = &batteryv1alpha1.HostReference{NodeName: agent.Node(), AgentAddress: agent.Addr()}
	claim.Status.BoundTime = &now
	claim.Status.LeaseExpiresAt = &expires
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type: batteryv1alpha1.ConditionBound, Status: metav1.ConditionTrue,
		Reason: batteryv1alpha1.ReasonBound, Message: "bound by the test's World",
	})
	return w.Kube.Status().Update(ctx, claim)
}

// Claim claims a MicroVM with cl, the Client Library over w.Kube, for req,
// which has to name its claim, and binds the claim to vmUID behind agent
// as soon as it exists. It returns what cl.Claim returns.
func (w *World) Claim(ctx context.Context, cl *claimclient.Client, req claimclient.Request, vmUID string, agent *Agent) (*claimclient.Claim, error) {
	if req.Name == "" {
		return nil, errors.New("fakeexecagent: the request has to name its claim")
	}
	type result struct {
		claim *claimclient.Claim
		err   error
	}
	out := make(chan result, 1)
	go func() {
		claim, err := cl.Claim(ctx, req)
		out <- result{claim, err}
	}()
	for {
		// The Client Library creates the claim, then its Secret; bind once
		// both exist, as battery binds after the Secret is there.
		secret := &corev1.Secret{}
		err := w.Kube.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: batteryv1alpha1.ExecSecretName(req.Name)}, secret)
		if err == nil {
			if err := w.Bind(ctx, req.Name, vmUID, agent); err != nil {
				return nil, err
			}
			r := <-out
			return r.claim, r.err
		}
		select {
		case r := <-out:
			return r.claim, r.err
		case <-ctx.Done():
			r := <-out
			return r.claim, r.err
		case <-time.After(pollInterval):
		}
	}
}
