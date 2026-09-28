package fakeexecagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

// serviceAccountPrefix begins the user name of a ServiceAccount token.
const serviceAccountPrefix = "system:serviceaccount:"

// Reviewer is the Authorizer of an Agent in front of a real API server,
// such as envtest's, where the claim tokens are real ServiceAccount tokens
// that battery-operator's Client Library requested with TokenRequest. It
// decides as battery-operator's Exec Agent does: a TokenReview for the
// Exec Agent's audience authenticates the token (EA-010), and the API
// server refuses a token whose bound Secret is gone or was replaced
// (EA-012). The token's own claims name that Secret, and the claim it
// belongs to has to name the token's ServiceAccount as its Holder (EA-011)
// and, for a MicroVM, be Bound, unexpired and name that MicroVM and this
// Host (EA-013).
type Reviewer struct {
	// Kube is a client of the API server with the right to create
	// TokenReviews and read MicroVMClaims, and a scheme that knows
	// battery-operator's v1alpha1.
	Kube client.Client
}

// Authorize implements Authorizer.
func (r Reviewer) Authorize(ctx context.Context, token, node, vmUID string) error {
	if token == "" {
		return fmt.Errorf("%w: no bearer token", ErrUnauthenticated)
	}
	review := &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{
		Token:     token,
		Audiences: []string{batteryv1alpha1.ExecAgentTokenAudience},
	}}
	if err := r.Kube.Create(ctx, review); err != nil {
		return fmt.Errorf("reviewing the bearer token: %w", err)
	}
	st := review.Status
	if !st.Authenticated || !slices.Contains(st.Audiences, batteryv1alpha1.ExecAgentTokenAudience) {
		return fmt.Errorf("%w: %s", ErrUnauthenticated, st.Error)
	}
	holder, ok := serviceAccount(st.User.Username)
	if !ok {
		return fmt.Errorf("%w: %s is no ServiceAccount", ErrPermissionDenied, st.User.Username)
	}
	secret, ok := boundSecret(token, st.User.Username)
	if !ok || secret.Namespace != holder.Namespace {
		return fmt.Errorf("%w: the token is bound to no Secret of %s", ErrPermissionDenied, holder)
	}
	return claimOpens(ctx, r.Kube, holder, secret.Name, node, vmUID)
}

// serviceAccount is the ServiceAccount a user name names.
func serviceAccount(user string) (k8stypes.NamespacedName, bool) {
	rest, ok := strings.CutPrefix(user, serviceAccountPrefix)
	if !ok {
		return k8stypes.NamespacedName{}, false
	}
	namespace, name, ok := strings.Cut(rest, ":")
	return k8stypes.NamespacedName{Namespace: namespace, Name: name}, ok && namespace != "" && name != ""
}

// boundSecret reads the Secret a ServiceAccount token is bound to from the
// token's own claims, which a TokenReview does not report. The API server
// has just checked the token's signature, which covers the claims, so
// they are read without checking it again; they have to be for user.
func boundSecret(token, user string) (k8stypes.NamespacedName, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return k8stypes.NamespacedName{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return k8stypes.NamespacedName{}, false
	}
	var claims struct {
		Subject    string `json:"sub"`
		Kubernetes struct {
			Namespace string `json:"namespace"`
			Secret    *struct {
				Name string `json:"name"`
			} `json:"secret"`
		} `json:"kubernetes.io"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Subject != user || claims.Kubernetes.Secret == nil {
		return k8stypes.NamespacedName{}, false
	}
	return k8stypes.NamespacedName{Namespace: claims.Kubernetes.Namespace, Name: claims.Kubernetes.Secret.Name}, true
}
