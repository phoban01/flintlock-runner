package kubelet

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// The authorization of the kubelet API, as the real kubelet's webhook
// authorizer does it.
const (
	// reviewTimeout bounds one SubjectAccessReview. A review that takes
	// longer is a review that could not be completed (KF-131).
	reviewTimeout = 10 * time.Second
	// allowTTL is how long an allowed decision is reused. The kubelet's
	// default is five minutes; here the endpoint is a shell in every Job on
	// the Host, so a grant that is taken away stops opening shells within
	// half a minute. A Job opens one exec session per Stage, all as the
	// API server's kubelet client identity, so even this short cache spares
	// the API server most of a Job's reviews.
	allowTTL = 30 * time.Second
	// denyTTL is how long a refusal is reused: long enough to absorb a
	// refused client retrying in a loop, short enough that an identity
	// granted access a moment ago is not kept waiting. The kubelet uses 30
	// seconds.
	denyTTL = 10 * time.Second
	// maxCachedDecisions bounds the cache. The kubelet API has a handful of
	// callers (the API server, perhaps a metrics scraper, an operator), so
	// the bound is only ever reached by a client cycling through
	// certificates, and then the entries closest to expiry make room.
	maxCachedDecisions = 256
)

// The resource every request to the kubelet API is authorized against. The
// kubelet checks nodes/proxy for every path it has no finer subresource for;
// the Pod Provider serves no path that has one, so it checks nodes/proxy
// always.
const (
	authzResource    = "nodes"
	authzSubresource = "proxy"
)

// identity is who a verified client certificate says the client is, read the
// way the API server's and the kubelet's x509 authenticator reads it: the
// common name is the user and each organization is a group, and every
// authenticated client is in system:authenticated.
type identity struct {
	user   string
	groups []string
}

// identityOf reads the identity of a verified certificate. A certificate
// with no common name names no user, and the x509 authenticator refuses it.
func identityOf(cert *x509.Certificate) (identity, error) {
	if cert.Subject.CommonName == "" {
		return identity{}, fmt.Errorf("the client certificate has no common name")
	}
	groups := slices.Clone(cert.Subject.Organization)
	if !slices.Contains(groups, "system:authenticated") {
		groups = append(groups, "system:authenticated")
	}
	return identity{user: cert.Subject.CommonName, groups: groups}, nil
}

// verbOf is the API verb a request to the kubelet API maps to. The streaming
// endpoints are "create" however they are reached: exec, attach and port
// forwarding accept a websocket upgrade over GET, and mapping that GET to
// "get" would let an identity that may only read the node open a shell.
// Everything else maps by its method, as in the kubelet. An empty verb is a
// method the kubelet API has no use for.
func verbOf(r *http.Request) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/"), "/")
	for _, streaming := range []string{"exec", "attach", "portForward", "run"} {
		if strings.EqualFold(first, streaming) {
			return "create"
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return "get"
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		return "delete"
	}
	return ""
}

// authorizer authorizes kubelet API requests with SubjectAccessReviews.
type authorizer struct {
	reviews authorizationv1client.SubjectAccessReviewInterface
	// node is the Virtual Node, the name the reviews are about.
	node string
	log  *slog.Logger
	clk  clock.Clock

	mu    sync.Mutex
	cache map[string]cachedDecision
}

type cachedDecision struct {
	allowed bool
	reason  string
	expires time.Time
}

func newAuthorizer(reviews authorizationv1client.SubjectAccessReviewInterface, node string, log *slog.Logger, clk clock.Clock) *authorizer {
	return &authorizer{reviews: reviews, node: node, log: log, clk: clk, cache: map[string]cachedDecision{}}
}

//= docs/requirements/12-cluster-fleet.md#cluster-hardening
//# The Pod Provider SHALL authorize every kubelet API request with
//# a SubjectAccessReview of the requesting identity for the `nodes/proxy`
//# resource on its Virtual Node and the verb the request maps to, and SHALL
//# refuse the request unless the review allows it.

// wrap puts next behind the authorizer. It has to sit behind
// requireClientCertificate, which guarantees the verified chain it reads.
// A refusal is 403, a review that could not be completed 500, as in the
// kubelet, and in neither case does next see the request.
func (a *authorizer) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
			http.Error(w, "a client certificate issued by the kubelet client certificate authority is required", http.StatusUnauthorized)
			return
		}
		who, err := identityOf(r.TLS.VerifiedChains[0][0])
		if err != nil {
			http.Error(w, "Unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		verb := verbOf(r)
		if verb == "" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		attrs := fmt.Sprintf("user=%s, verb=%s, resource=%s, subresource=%s", who.user, verb, authzResource, authzSubresource)
		allowed, reason, err := a.authorize(r.Context(), who, verb)
		if err != nil {
			//= docs/requirements/12-cluster-fleet.md#cluster-hardening
			//# If the SubjectAccessReview of a request cannot be completed,
			//# then the Pod Provider SHALL refuse the request.
			a.log.Warn("refused a kubelet API request: its authorization could not be completed",
				"user", who.user, "verb", verb, "path", r.URL.Path, "error", err)
			http.Error(w, "Authorization error ("+attrs+")", http.StatusInternalServerError)
			return
		}
		if !allowed {
			a.log.Info("refused a kubelet API request", "user", who.user, "groups", who.groups, "verb", verb, "path", r.URL.Path, "reason", reason)
			http.Error(w, "Forbidden ("+attrs+")", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorize returns the decision for who and verb on the Virtual Node, from
// the cache while it is fresh and from a SubjectAccessReview otherwise. An
// error is never cached: the next request asks again.
func (a *authorizer) authorize(ctx context.Context, who identity, verb string) (bool, string, error) {
	key := decisionKey(who, verb)
	now := a.clk.Now()
	a.mu.Lock()
	if d, ok := a.cache[key]; ok && now.Before(d.expires) {
		a.mu.Unlock()
		return d.allowed, d.reason, nil
	}
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()
	review, err := a.reviews.Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   who.user,
			Groups: who.groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:        verb,
				Version:     "v1",
				Resource:    authzResource,
				Subresource: authzSubresource,
				Name:        a.node,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, "", fmt.Errorf("creating a SubjectAccessReview: %w", err)
	}
	// Denied is only a stronger no, and no opinion is a no as well: anything
	// but Allowed is a refusal. An evaluation error beside a decision is
	// what went wrong with some other binding; the decision stands, as it
	// does in the kubelet.
	allowed := review.Status.Allowed && !review.Status.Denied
	reason := review.Status.Reason
	if review.Status.EvaluationError != "" {
		reason = strings.TrimSpace(reason + " (evaluation error: " + review.Status.EvaluationError + ")")
	}
	a.remember(key, cachedDecision{allowed: allowed, reason: reason}, now)
	return allowed, reason, nil
}

// remember caches a decision, making room when the cache is full: expired
// entries go first, then the ones closest to expiry.
func (a *authorizer) remember(key string, d cachedDecision, now time.Time) {
	ttl := denyTTL
	if d.allowed {
		ttl = allowTTL
	}
	d.expires = now.Add(ttl)
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.cache[key]; !ok && len(a.cache) >= maxCachedDecisions {
		for k, v := range a.cache {
			if !now.Before(v.expires) {
				delete(a.cache, k)
			}
		}
		for len(a.cache) >= maxCachedDecisions {
			oldest := ""
			for k, v := range a.cache {
				if oldest == "" || v.expires.Before(a.cache[oldest].expires) {
					oldest = k
				}
			}
			delete(a.cache, oldest)
		}
	}
	a.cache[key] = d
}

// decisionKey identifies a decision: the user, the groups in any order and
// the verb. The resource and the name are the same for every request.
func decisionKey(who identity, verb string) string {
	groups := slices.Clone(who.groups)
	slices.Sort(groups)
	return strings.Join([]string{who.user, strings.Join(groups, "\x01"), verb}, "\x00")
}
