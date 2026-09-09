package flintlock

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

// Defaults for a dialled Host connection. The call deadline is the fallback
// for a Dialer built without one; the keepalive and backoff values are the
// Runner's policy for HO-002 and are not part of the configuration surface.
const (
	// DefaultCallDeadline bounds a unary call when no deadline is
	// configured (HO-003).
	DefaultCallDeadline = 10 * time.Second
	// keepaliveTime is how often an idle connection is pinged and
	// keepaliveTimeout how long a ping may go unanswered before the
	// connection is torn down and re-established (HO-002).
	keepaliveTime    = 30 * time.Second
	keepaliveTimeout = 10 * time.Second
	// reconnectBaseDelay and reconnectMaxDelay bound the exponential
	// backoff gRPC applies between reconnection attempts (HO-002).
	reconnectBaseDelay = 500 * time.Millisecond
	reconnectMaxDelay  = 30 * time.Second
	// reconnectMultiplier and reconnectJitter shape that backoff. The
	// jitter keeps a fleet of Hosts from reconnecting in lockstep.
	reconnectMultiplier = 1.6
	reconnectJitter     = 0.2
)

// DialerOption configures a Dialer built by NewDialer.
type DialerOption func(*grpcDialer)

// WithCallDeadline sets the deadline applied to every unary call (HO-003).
// A zero or negative value keeps DefaultCallDeadline.
func WithCallDeadline(d time.Duration) DialerOption {
	return func(g *grpcDialer) {
		if d > 0 {
			g.deadline = d
		}
	}
}

// WithLogger sets the logger the connections report on. The default
// discards.
func WithLogger(log *slog.Logger) DialerOption {
	return func(g *grpcDialer) {
		if log != nil {
			g.log = log
		}
	}
}

// WithKeepalive sets how often an idle connection is pinged and how long a
// ping may go unanswered before the connection is re-established (HO-002).
// Non-positive values keep the defaults. Production uses the defaults; it
// is an option so that a test can watch the pings without waiting half a
// minute for one.
func WithKeepalive(interval, timeout time.Duration) DialerOption {
	return func(g *grpcDialer) {
		if interval > 0 {
			g.keepaliveTime = interval
		}
		if timeout > 0 {
			g.keepaliveTimeout = timeout
		}
	}
}

// WithReconnectBackoff sets the first and the longest delay of the
// exponential backoff between reconnection attempts (HO-002). Non-positive
// values keep the defaults.
func WithReconnectBackoff(base, max time.Duration) DialerOption {
	return func(g *grpcDialer) {
		if base > 0 {
			g.backoffBase = base
		}
		if max > 0 {
			g.backoffMax = max
		}
	}
}

// NewDialer returns the production Dialer: one long-lived gRPC connection
// per Host (HO-002), the configured deadline on every unary call (HO-003),
// basic auth (HO-004) and TLS (HO-005, HO-006) from the Endpoint.
func NewDialer(opts ...DialerOption) Dialer {
	g := &grpcDialer{
		deadline:         DefaultCallDeadline,
		log:              discardLogger(),
		keepaliveTime:    keepaliveTime,
		keepaliveTimeout: keepaliveTimeout,
		backoffBase:      reconnectBaseDelay,
		backoffMax:       reconnectMaxDelay,
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// grpcDialer is the production Dialer.
type grpcDialer struct {
	deadline         time.Duration
	log              *slog.Logger
	keepaliveTime    time.Duration
	keepaliveTimeout time.Duration
	backoffBase      time.Duration
	backoffMax       time.Duration
}

//= docs/requirements/05-hosts.md#flintlock-client
//# The Runner SHALL maintain one long-lived gRPC connection per
//# Host, with keepalive enabled, and SHALL reconnect with exponential backoff
//# when a connection is lost.

// Dial implements Dialer. It builds one connection for the Endpoint and
// hands back the Client that owns it for its whole life; the connection is
// established lazily and gRPC re-establishes it with the exponential
// backoff configured here whenever it is lost, so Dial does not block on a
// Host that is down.
func (g *grpcDialer) Dial(_ context.Context, ep Endpoint) (HostClient, error) {
	if ep.Name == "" {
		return nil, fmt.Errorf("flintlock: dialling %q: host name is required", ep.Address)
	}
	if ep.Address == "" {
		return nil, fmt.Errorf("flintlock: dialling host %s: address is required", ep.Name)
	}
	creds, err := transportCredentials(ep)
	if err != nil {
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                g.keepaliveTime,
			Timeout:             g.keepaliveTimeout,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  g.backoffBase,
				Multiplier: reconnectMultiplier,
				Jitter:     reconnectJitter,
				MaxDelay:   g.backoffMax,
			},
			MinConnectTimeout: g.backoffMax,
		}),
		grpc.WithChainUnaryInterceptor(deadlineInterceptor(g.deadline)),
	}
	if header := basicAuthHeader(ep.Token); header != "" {
		opts = append(opts,
			grpc.WithChainUnaryInterceptor(authUnaryInterceptor(header)),
			grpc.WithChainStreamInterceptor(authStreamInterceptor(header)),
		)
	}

	conn, err := grpc.NewClient(ep.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("flintlock: dialling host %s at %s: %w", ep.Name, ep.Address, err)
	}
	return newClient(ep, conn, g.log), nil
}

//= docs/requirements/05-hosts.md#flintlock-client
//# Where a Host is configured with a basic auth token, the Runner
//# SHALL send an `authorization` header with the value `Basic` followed by the
//# base64 encoding of the token on every call to that Host.

// basicAuthHeader is the authorization header value for a token, empty when
// the Host is configured without one. flintlockd's basic auth takes the
// token as the whole credential rather than as a user:password pair, so the
// token is base64-encoded as it stands.
func basicAuthHeader(token string) string {
	if token == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(token))
}

// authUnaryInterceptor adds the authorization header to every unary call
// (HO-004).
func authUnaryInterceptor(header string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(withAuth(ctx, header), method, req, reply, cc, opts...)
	}
}

// authStreamInterceptor adds the authorization header to every stream
// (HO-004): the exec and SSH proxy streams are calls to the Host too.
func authStreamInterceptor(header string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(withAuth(ctx, header), desc, cc, method, opts...)
	}
}

// withAuth returns ctx carrying the authorization header.
func withAuth(ctx context.Context, header string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", header)
}

//= docs/requirements/05-hosts.md#flintlock-client
//# The Runner SHALL apply the configured deadline to every unary
//# flintlock call.

// deadlineInterceptor bounds every unary call with the configured deadline.
// It is an interceptor rather than a wrapper inside each method so that a
// method added later cannot forget it; a caller that already holds a
// shorter deadline keeps it, because context.WithTimeout takes the earlier
// of the two.
func deadlineInterceptor(d time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if d <= 0 {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

//= docs/requirements/05-hosts.md#flintlock-client
//# The Runner SHALL connect to Hosts with TLS, verifying the server
//# certificate against the configured certificate authority, unless the Host
//# is explicitly marked insecure.

//= docs/requirements/05-hosts.md#flintlock-client
//# Where client certificate and key files are configured for a
//# Host, the Runner SHALL present them for mutual TLS.

// transportCredentials builds the Endpoint's transport credentials. TLS is
// the default and verification is never disabled (SE-022): a Host is
// reached in plaintext only when its entry sets Insecure (SE-021). An empty
// CAFile verifies against the system roots; a certificate and key together
// are presented for mutual TLS, and one without the other is a
// configuration error rather than a silently one-sided connection.
func transportCredentials(ep Endpoint) (credentials.TransportCredentials, error) {
	if ep.TLS.Insecure {
		return insecure.NewCredentials(), nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if ep.TLS.CAFile != "" {
		pem, err := os.ReadFile(ep.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("flintlock: host %s: reading certificate authority: %w", ep.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("flintlock: host %s: no certificates in %s", ep.Name, ep.TLS.CAFile)
		}
		cfg.RootCAs = pool
	}
	switch {
	case ep.TLS.CertFile != "" && ep.TLS.KeyFile != "":
		cert, err := tls.LoadX509KeyPair(ep.TLS.CertFile, ep.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("flintlock: host %s: loading client certificate: %w", ep.Name, err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	case ep.TLS.CertFile != "" || ep.TLS.KeyFile != "":
		return nil, fmt.Errorf("flintlock: host %s: mutual TLS needs both a client certificate and a key", ep.Name)
	}
	return credentials.NewTLS(cfg), nil
}

// discardLogger is the logger used when none is configured.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// Compile-time check.
var _ Dialer = (*grpcDialer)(nil)
