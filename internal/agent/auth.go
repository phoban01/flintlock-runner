package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

// errUnauthenticated is wrapped by every request that does not authenticate.
var errUnauthenticated = errors.New("the request does not authenticate")

// reviewError is a TokenReview that could not be completed (KF-175).
type reviewError struct{ err error }

func (e *reviewError) Error() string { return "reviewing the bearer token: " + e.err.Error() }
func (e *reviewError) Unwrap() error { return e.err }

// authenticator authenticates requests by their bearer token.
type authenticator struct {
	reviews   authenticationv1client.TokenReviewInterface
	audiences []string
	timeout   time.Duration
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# The Exec Agent SHALL authenticate every request with a
//# TokenReview of the bearer token it carries, and SHALL refuse a request
//# that does not authenticate.

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# If the TokenReview or the claim lookup of a request cannot be
//# completed, then the Exec Agent SHALL refuse the request.

// authenticate returns the user name of the identity a request's bearer
// token belongs to, asking the API server with a TokenReview every time:
// there is no cache, so a token that stops being valid stops working at
// once. A request with no bearer token, a token the review does not
// authenticate, or one without the configured audiences is refused as
// unauthenticated; a review that cannot be made is refused as well, as a
// reviewError, because a request the agent cannot vouch for is not run.
func (a *authenticator) authenticate(ctx context.Context) (string, error) {
	token, err := bearerToken(ctx)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	review, err := a.reviews.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: a.audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", &reviewError{err: err}
	}
	st := review.Status
	switch {
	case st.Error != "" && !st.Authenticated:
		return "", fmt.Errorf("%w: %s", errUnauthenticated, st.Error)
	case !st.Authenticated:
		return "", fmt.Errorf("%w: the token is not valid", errUnauthenticated)
	case st.User.Username == "":
		return "", fmt.Errorf("%w: the token names no user", errUnauthenticated)
	}
	for _, want := range a.audiences {
		if !slices.Contains(st.Audiences, want) {
			return "", fmt.Errorf("%w: the token is not for audience %q", errUnauthenticated, want)
		}
	}
	return st.User.Username, nil
}

// bearerToken reads the token of an `authorization: Bearer <token>` header.
func bearerToken(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return "", fmt.Errorf("%w: no bearer token", errUnauthenticated)
	}
	if len(values) > 1 {
		return "", fmt.Errorf("%w: more than one authorization header", errUnauthenticated)
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
		return "", fmt.Errorf("%w: the authorization header is not a bearer token", errUnauthenticated)
	}
	return token, nil
}
