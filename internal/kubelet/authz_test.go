package kubelet

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

func TestVerbOfMapsLikeTheKubelet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, path, want string
	}{
		// The streaming endpoints are create however they are reached, a
		// websocket upgrade over GET included.
		{http.MethodPost, "/exec/ns/pod/microvm", "create"},
		{http.MethodGet, "/exec/ns/pod/microvm", "create"},
		{http.MethodGet, "/attach/ns/pod/microvm", "create"},
		{http.MethodGet, "/portForward/ns/pod", "create"},
		{http.MethodGet, "/portforward/ns/pod", "create"},
		{http.MethodPost, "/run/ns/pod/microvm", "create"},
		{http.MethodGet, "//exec/ns/pod/microvm", "create"},
		{http.MethodGet, "/pods/../exec/ns/pod/microvm", "create"},
		// Everything else by its method.
		{http.MethodGet, "/pods", "get"},
		{http.MethodHead, "/pods", "get"},
		{http.MethodGet, "/containerLogs/ns/pod/microvm", "get"},
		{http.MethodGet, "/stats/summary", "get"},
		{http.MethodPost, "/pods", "create"},
		{http.MethodPut, "/pods", "update"},
		{http.MethodPatch, "/pods", "patch"},
		{http.MethodDelete, "/pods", "delete"},
		{http.MethodOptions, "/pods", ""},
		{"PROPFIND", "/pods", ""},
	} {
		r := httptest.NewRequest(tc.method, "https://host/", nil)
		r.URL.Path = tc.path
		if got := verbOf(r); got != tc.want {
			t.Errorf("%s %s maps to %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestIdentityOfReadsTheCertificateLikeTheX509Authenticator(t *testing.T) {
	t.Parallel()
	who, err := identityOf(&x509.Certificate{Subject: pkix.Name{CommonName: "kube-apiserver-kubelet-client", Organization: []string{"system:masters", "ops"}}})
	if err != nil {
		t.Fatal(err)
	}
	if who.user != "kube-apiserver-kubelet-client" {
		t.Errorf("user = %q", who.user)
	}
	if want := []string{"system:masters", "ops", "system:authenticated"}; !slices.Equal(who.groups, want) {
		t.Errorf("groups = %v, want %v", who.groups, want)
	}
	if _, err := identityOf(&x509.Certificate{Subject: pkix.Name{Organization: []string{"system:masters"}}}); err == nil {
		t.Error("a certificate with no common name was given an identity")
	}
}

// reviewer is a fake API server that answers SubjectAccessReviews with
// decide and counts them.
type reviewer struct {
	client  *fake.Clientset
	reviews atomic.Int64
	last    atomic.Pointer[authorizationv1.SubjectAccessReviewSpec]
}

func newReviewer(decide func(spec authorizationv1.SubjectAccessReviewSpec) (authorizationv1.SubjectAccessReviewStatus, error)) *reviewer {
	r := &reviewer{client: fake.NewClientset()}
	r.client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		r.reviews.Add(1)
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
		spec := review.Spec
		r.last.Store(&spec)
		status, err := decide(spec)
		if err != nil {
			return true, nil, err
		}
		review.Status = status
		return true, review, nil
	})
	return r
}

// serve sends one request with a verified client certificate for cn and
// orgs through the authorizer and reports the status and whether the
// handler behind it ran.
func serve(a *authorizer, method, path, cn string, orgs ...string) (status int, ran bool) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(method, "https://host"+path, nil)
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn, Organization: orgs}}
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	w := httptest.NewRecorder()
	requireClientCertificate(a.wrap(next)).ServeHTTP(w, r)
	return w.Code, ran
}

//= docs/requirements/12-cluster-fleet.md#cluster-hardening
//= type=test
//# The Pod Provider SHALL authorize every kubelet API request with
//# a SubjectAccessReview of the requesting identity for the `nodes/proxy`
//# resource on its Virtual Node and the verb the request maps to, and SHALL
//# refuse the request unless the review allows it.

func TestAuthorizerAsksAboutTheVirtualNodeAndRefusesUnlessAllowed(t *testing.T) {
	t.Parallel()
	rev := newReviewer(func(spec authorizationv1.SubjectAccessReviewSpec) (authorizationv1.SubjectAccessReviewStatus, error) {
		switch spec.User {
		case "allowed":
			return authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil
		case "denied":
			return authorizationv1.SubjectAccessReviewStatus{Denied: true, Reason: "no"}, nil
		case "contradictory":
			return authorizationv1.SubjectAccessReviewStatus{Allowed: true, Denied: true}, nil
		}
		return authorizationv1.SubjectAccessReviewStatus{}, nil // no opinion
	})
	a := newAuthorizer(rev.client.AuthorizationV1().SubjectAccessReviews(), "host-a-microvms", slog.New(slog.DiscardHandler), clock.NewFake(time.Unix(0, 0)))

	status, ran := serve(a, http.MethodGet, "/exec/ns/pod/microvm", "allowed", "execers")
	if status != http.StatusOK || !ran {
		t.Errorf("allowed identity: status %d, ran %v", status, ran)
	}
	spec := rev.last.Load()
	want := authorizationv1.ResourceAttributes{Verb: "create", Version: "v1", Resource: "nodes", Subresource: "proxy", Name: "host-a-microvms"}
	if spec.ResourceAttributes == nil || *spec.ResourceAttributes != want {
		t.Errorf("reviewed %+v, want %+v", spec.ResourceAttributes, want)
	}
	if spec.User != "allowed" || !slices.Equal(spec.Groups, []string{"execers", "system:authenticated"}) {
		t.Errorf("reviewed user %q groups %v", spec.User, spec.Groups)
	}

	for _, user := range []string{"denied", "no-opinion", "contradictory"} {
		if status, ran := serve(a, http.MethodPost, "/exec/ns/pod/microvm", user); status != http.StatusForbidden || ran {
			t.Errorf("%s identity: status %d, ran %v; want 403 and nothing run", user, status, ran)
		}
	}
	if status, ran := serve(a, http.MethodGet, "/pods", "ungranted"); status != http.StatusForbidden || ran {
		t.Errorf("GET /pods by an identity with no grant: status %d, ran %v", status, ran)
	}
	if status, ran := serve(a, http.MethodGet, "/pods", ""); status != http.StatusUnauthorized || ran {
		t.Errorf("a certificate with no common name: status %d, ran %v; want 401", status, ran)
	}
	before := rev.reviews.Load()
	if status, ran := serve(a, http.MethodOptions, "/pods", "allowed"); status != http.StatusMethodNotAllowed || ran {
		t.Errorf("OPTIONS: status %d, ran %v; want 405", status, ran)
	}
	if rev.reviews.Load() != before {
		t.Error("a method with no verb was reviewed")
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-hardening
//= type=test
//# If the SubjectAccessReview of a request cannot be completed,
//# then the Pod Provider SHALL refuse the request.

func TestAuthorizerRefusesWhenTheReviewFails(t *testing.T) {
	t.Parallel()
	var failing atomic.Bool
	failing.Store(true)
	rev := newReviewer(func(authorizationv1.SubjectAccessReviewSpec) (authorizationv1.SubjectAccessReviewStatus, error) {
		if failing.Load() {
			return authorizationv1.SubjectAccessReviewStatus{}, errors.New("the API server is unreachable")
		}
		return authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil
	})
	a := newAuthorizer(rev.client.AuthorizationV1().SubjectAccessReviews(), "host-a-microvms", slog.New(slog.DiscardHandler), clock.NewFake(time.Unix(0, 0)))

	if status, ran := serve(a, http.MethodPost, "/exec/ns/pod/microvm", "apiserver"); status != http.StatusInternalServerError || ran {
		t.Errorf("failed review: status %d, ran %v; want 500 and nothing run", status, ran)
	}
	// A failure is not remembered: the next request is reviewed afresh.
	failing.Store(false)
	if status, ran := serve(a, http.MethodPost, "/exec/ns/pod/microvm", "apiserver"); status != http.StatusOK || !ran {
		t.Errorf("after the API server came back: status %d, ran %v", status, ran)
	}
	if n := rev.reviews.Load(); n != 2 {
		t.Errorf("%d reviews, want 2", n)
	}
}

func TestAuthorizerCachesDecisionsBriefly(t *testing.T) {
	t.Parallel()
	var allow atomic.Bool
	allow.Store(true)
	rev := newReviewer(func(authorizationv1.SubjectAccessReviewSpec) (authorizationv1.SubjectAccessReviewStatus, error) {
		return authorizationv1.SubjectAccessReviewStatus{Allowed: allow.Load()}, nil
	})
	clk := clock.NewFake(time.Unix(0, 0))
	a := newAuthorizer(rev.client.AuthorizationV1().SubjectAccessReviews(), "host-a-microvms", slog.New(slog.DiscardHandler), clk)
	exec := func() int {
		status, _ := serve(a, http.MethodPost, "/exec/ns/pod/microvm", "apiserver", "b", "a")
		return status
	}

	if exec() != http.StatusOK || exec() != http.StatusOK {
		t.Fatal("allowed identity refused")
	}
	if n := rev.reviews.Load(); n != 1 {
		t.Errorf("%d reviews for two requests inside the allow TTL, want 1", n)
	}
	// Groups in another order are the same identity.
	if status, _ := serve(a, http.MethodPost, "/exec/ns/pod/microvm", "apiserver", "a", "b"); status != http.StatusOK || rev.reviews.Load() != 1 {
		t.Error("the same groups in another order were reviewed again")
	}
	// Another verb is another question.
	if status, _ := serve(a, http.MethodGet, "/pods", "apiserver", "a", "b"); status != http.StatusOK || rev.reviews.Load() != 2 {
		t.Error("a get was answered from the create decision")
	}

	// A revoked grant takes effect when the allow TTL runs out, no later.
	allow.Store(false)
	clk.Advance(allowTTL - time.Second)
	if exec() != http.StatusOK {
		t.Error("the cached grant was not used inside its TTL")
	}
	clk.Advance(time.Second)
	if exec() != http.StatusForbidden {
		t.Error("a revoked grant was still used after the allow TTL")
	}
	// A refusal is cached for the shorter deny TTL.
	allow.Store(true)
	clk.Advance(denyTTL - time.Second)
	if exec() != http.StatusForbidden {
		t.Error("the cached refusal was not used inside its TTL")
	}
	clk.Advance(time.Second)
	if exec() != http.StatusOK {
		t.Error("a new grant was not seen after the deny TTL")
	}
}

func TestAuthorizerCacheIsBounded(t *testing.T) {
	t.Parallel()
	rev := newReviewer(func(authorizationv1.SubjectAccessReviewSpec) (authorizationv1.SubjectAccessReviewStatus, error) {
		return authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil
	})
	clk := clock.NewFake(time.Unix(0, 0))
	a := newAuthorizer(rev.client.AuthorizationV1().SubjectAccessReviews(), "host-a-microvms", slog.New(slog.DiscardHandler), clk)
	for i := range 3 * maxCachedDecisions {
		clk.Advance(time.Millisecond)
		if status, _ := serve(a, http.MethodGet, "/pods", fmt.Sprintf("client-%d", i)); status != http.StatusOK {
			t.Fatalf("client-%d refused", i)
		}
		a.mu.Lock()
		n := len(a.cache)
		a.mu.Unlock()
		if n > maxCachedDecisions {
			t.Fatalf("the cache holds %d decisions, more than %d", n, maxCachedDecisions)
		}
	}
	// The newest decision survived the evictions.
	before := rev.reviews.Load()
	serve(a, http.MethodGet, "/pods", fmt.Sprintf("client-%d", 3*maxCachedDecisions-1))
	if rev.reviews.Load() != before {
		t.Error("the most recent decision was evicted")
	}
}
