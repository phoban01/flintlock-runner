package poolmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// Defaults for the connection settings a ClientConfig leaves zero. The
// keepalive values are the ones a long-lived control-plane connection
// wants: often enough to notice a dead peer well inside a Job, rare enough
// not to trip a server's minimum ping interval.
const (
	defaultKeepaliveTime    = 30 * time.Second
	defaultKeepaliveTimeout = 10 * time.Second
	defaultReconnectBase    = time.Second
	defaultReconnectMax     = 30 * time.Second
	defaultCallDeadline     = 10 * time.Second
)

// ClientConfig is what NewClient needs to reach one Pool Manager. It is
// built from the pool_manager section of the configuration (CF-040).
type ClientConfig struct {
	// Endpoint is the battery gRPC address, host:port. Required.
	Endpoint string
	// TLS is the client TLS material. TLS is used unless TLS.Insecure marks
	// the endpoint plaintext (PL-003, SE-021); there is no way to skip
	// certificate verification (SE-022).
	TLS config.ClientTLS
	// Deadline is applied to every unary call (PL-002). Zero takes
	// defaultCallDeadline; the Subscribe stream is not a unary call and is
	// bounded by its caller's context instead.
	Deadline time.Duration
	// KeepaliveTime and KeepaliveTimeout are the HTTP/2 keepalive settings
	// of the single long-lived connection (PL-006).
	KeepaliveTime    time.Duration
	KeepaliveTimeout time.Duration
	// ReconnectBase and ReconnectMax bound the exponential backoff gRPC
	// uses to re-establish the connection after it is lost (PL-006).
	ReconnectBase time.Duration
	ReconnectMax  time.Duration
	// DialOptions are appended to the options NewClient builds. They are
	// how a test points the client at an in-process listener; nothing in
	// the Runner sets them.
	DialOptions []grpc.DialOption
}

// withDefaults returns cfg with its zero fields filled in.
func (c ClientConfig) withDefaults() ClientConfig {
	if c.Deadline <= 0 {
		c.Deadline = defaultCallDeadline
	}
	if c.KeepaliveTime <= 0 {
		c.KeepaliveTime = defaultKeepaliveTime
	}
	if c.KeepaliveTimeout <= 0 {
		c.KeepaliveTimeout = defaultKeepaliveTimeout
	}
	if c.ReconnectBase <= 0 {
		c.ReconnectBase = defaultReconnectBase
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = defaultReconnectMax
	}
	return c
}

//= docs/requirements/04-pool-manager.md#client
//# The Scheduler SHALL communicate with the Pool Manager through the
//# `poolmgr.v1alpha1` gRPC services using the generated Go client from the
//# battery module.

// client is the Runner's Pool Manager client: one gRPC connection carrying
// the three generated service clients, one method per RPC. Every method
// translates a gRPC status into the package's sentinel errors, so no caller
// imports gRPC codes.
type client struct {
	conn   *grpc.ClientConn
	admin  poolmgrv1.PoolAdminClient
	lease  poolmgrv1.LeaseClient
	events poolmgrv1.EventsClient
}

//= docs/requirements/04-pool-manager.md#client
//# The Scheduler SHALL maintain one long-lived connection to the Pool
//# Manager with keepalive enabled and SHALL reconnect with exponential
//# backoff when it is lost.

// NewClient builds the Pool Manager client. It creates one gRPC
// ClientConn with keepalive and exponential reconnect backoff (PL-006), the
// configured transport credentials (PL-003) and an interceptor that puts
// the configured deadline on every unary call (PL-002). The connection is
// established lazily: NewClient does not fail because the Pool Manager is
// down, the first call does, and Health drives the retry (PL-004).
func NewClient(cfg ClientConfig) (Client, error) {
	cfg = cfg.withDefaults()
	if cfg.Endpoint == "" {
		return nil, errors.New("poolmgr: endpoint is required")
	}
	creds, err := transportCredentials(cfg.TLS)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cfg.KeepaliveTime,
			Timeout:             cfg.KeepaliveTimeout,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  cfg.ReconnectBase,
				Multiplier: backoff.DefaultConfig.Multiplier,
				Jitter:     backoff.DefaultConfig.Jitter,
				MaxDelay:   cfg.ReconnectMax,
			},
			MinConnectTimeout: cfg.ReconnectMax,
		}),
		grpc.WithChainUnaryInterceptor(deadlineInterceptor(cfg.Deadline)),
	}
	opts = append(opts, cfg.DialOptions...)

	conn, err := grpc.NewClient(cfg.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("poolmgr: dial %s: %w", cfg.Endpoint, err)
	}
	return &client{
		conn:   conn,
		admin:  poolmgrv1.NewPoolAdminClient(conn),
		lease:  poolmgrv1.NewLeaseClient(conn),
		events: poolmgrv1.NewEventsClient(conn),
	}, nil
}

//= docs/requirements/04-pool-manager.md#client
//# The Scheduler SHALL apply the configured deadline to every unary call to
//# the Pool Manager.

// deadlineInterceptor bounds every unary call with d. A caller that already
// set an earlier deadline keeps it, because context.WithTimeout never
// extends one.
func deadlineInterceptor(d time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if d > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

//= docs/requirements/04-pool-manager.md#client
//# The Scheduler SHALL connect to the Pool Manager with TLS unless the
//# configuration explicitly marks the endpoint insecure.

// transportCredentials builds the connection's credentials from the
// configured TLS material: plaintext only when the endpoint is explicitly
// marked insecure (SE-021), otherwise TLS 1.2 or better with the configured
// certificate authority, or the system roots when none is given, and a
// client certificate for mutual TLS when both files are set. Server
// certificates are always verified (SE-022).
func transportCredentials(t config.ClientTLS) (credentials.TransportCredentials, error) {
	if t.Insecure {
		return insecure.NewCredentials(), nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("poolmgr: reading ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("poolmgr: ca_file %s holds no certificate", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	switch {
	case t.CertFile != "" && t.KeyFile != "":
		pair, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("poolmgr: loading client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	case t.CertFile != "" || t.KeyFile != "":
		return nil, errors.New("poolmgr: cert_file and key_file have to be set together")
	}
	return credentials.NewTLS(cfg), nil
}

// CreatePool implements PoolAdmin.
func (c *client) CreatePool(ctx context.Context, spec PoolSpec) (*Pool, error) {
	resp, err := c.admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: specToProto(spec)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp), nil
}

// UpdatePool implements PoolAdmin.
func (c *client) UpdatePool(ctx context.Context, spec PoolSpec) (*Pool, error) {
	resp, err := c.admin.UpdatePool(ctx, &poolmgrv1.UpdatePoolRequest{Spec: specToProto(spec)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp), nil
}

// DeletePool implements PoolAdmin.
func (c *client) DeletePool(ctx context.Context, ref PoolRef) error {
	_, err := c.admin.DeletePool(ctx, &poolmgrv1.DeletePoolRequest{Ref: refToProto(ref)})
	return mapErr(ctx, err)
}

// GetPool implements PoolAdmin.
func (c *client) GetPool(ctx context.Context, ref PoolRef) (*Pool, error) {
	resp, err := c.admin.GetPool(ctx, &poolmgrv1.GetPoolRequest{Ref: refToProto(ref)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp), nil
}

// ListPools implements PoolAdmin. An empty namespace lists every namespace.
func (c *client) ListPools(ctx context.Context, namespace string) ([]*Pool, error) {
	req := &poolmgrv1.ListPoolsRequest{}
	if namespace != "" {
		req.Namespace = &namespace
	}
	resp, err := c.admin.ListPools(ctx, req)
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	out := make([]*Pool, 0, len(resp.GetPools()))
	for _, pool := range resp.GetPools() {
		out = append(out, poolFromProto(pool))
	}
	return out, nil
}

//= docs/requirements/04-pool-manager.md#claiming
//# When `ClaimVM` succeeds, the Scheduler SHALL record the returned
//# `lease_id`, `vm_uid`, `network_interfaces` and the `host` name and
//# address on the Allocation.

// ClaimVM implements Lease. Every field of ClaimVMResponse the Scheduler
// needs for an Allocation is carried over: the lease id it heartbeats and
// releases with, the MicroVM uid, the guest's network interfaces and the
// Host that runs it. Host is left zero by a Pool Manager that predates the
// field, which is what sends the Scheduler to the fan-out of SC-031.
func (c *client) ClaimVM(ctx context.Context, pool PoolRef) (*Claim, error) {
	resp, err := c.lease.ClaimVM(ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(pool)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return &Claim{
		LeaseID:           resp.GetLeaseId(),
		VMUID:             resp.GetVmUid(),
		NetworkInterfaces: resp.GetNetworkInterfaces(),
		Host: HostRef{
			Name:    resp.GetHost().GetName(),
			Address: resp.GetHost().GetAddress(),
		},
	}, nil
}

// Heartbeat implements Lease. It returns the expiry the Pool Manager
// answered with; ErrNotFound means the lease is gone (SC-061).
func (c *client) Heartbeat(ctx context.Context, leaseID string) (time.Time, error) {
	resp, err := c.lease.Heartbeat(ctx, &poolmgrv1.HeartbeatRequest{LeaseId: leaseID})
	if err != nil {
		return time.Time{}, mapErr(ctx, err)
	}
	return resp.GetExpiresAt().AsTime(), nil
}

// ReleaseVM implements Lease.
func (c *client) ReleaseVM(ctx context.Context, leaseID string) error {
	_, err := c.lease.ReleaseVM(ctx, &poolmgrv1.ReleaseVMRequest{LeaseId: leaseID})
	return mapErr(ctx, err)
}

// Subscribe implements Events. The stream lives until Close, until the
// caller's context ends or until the server drops it.
func (c *client) Subscribe(ctx context.Context, filter EventFilter) (EventStream, error) {
	req := &poolmgrv1.SubscribeRequest{}
	if filter.Pool != nil {
		req.Pool = refToProto(*filter.Pool)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.events.Subscribe(streamCtx, req)
	if err != nil {
		cancel()
		return nil, mapErr(streamCtx, err)
	}
	return newEventStream(streamCtx, cancel, stream), nil
}

// Close implements Client.
func (c *client) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("poolmgr: closing connection: %w", err)
	}
	return nil
}

// eventStream adapts the generated server-streaming client to EventStream.
// One reader goroutine, started at Subscribe, owns the gRPC stream and hands
// results to Recv, so a Recv abandoned on its own context does not lose the
// event it was waiting for. Recv is not for concurrent use.
type eventStream struct {
	cancel context.CancelFunc
	// streamCtx ends on Close; the reader goroutine exits with it.
	streamCtx context.Context
	results   chan streamResult
	// terminal is the error the stream ended with, once Recv has returned
	// it; every later Recv repeats it.
	terminal error
}

type streamResult struct {
	event *poolmgrv1.Event
	err   error
}

func newEventStream(ctx context.Context, cancel context.CancelFunc, stream grpc.ServerStreamingClient[poolmgrv1.Event]) *eventStream {
	s := &eventStream{cancel: cancel, streamCtx: ctx, results: make(chan streamResult)}
	go func() {
		for {
			e, err := stream.Recv()
			select {
			case s.results <- streamResult{event: e, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

// Recv implements EventStream. Any end of the stream the caller did not ask
// for is reported as ErrUnavailable, so that the Tracker falls back to
// polling and re-subscribes (PL-051).
func (s *eventStream) Recv(ctx context.Context) (*Event, error) {
	if s.terminal != nil {
		return nil, s.terminal
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.streamCtx.Done():
		s.terminal = fmt.Errorf("%w: events stream closed", ErrUnavailable)
		return nil, s.terminal
	case r := <-s.results:
		if r.err != nil {
			s.terminal = fmt.Errorf("%w: events stream ended: %s", ErrUnavailable, status.Convert(r.err).Message())
			return nil, s.terminal
		}
		return eventFromProto(r.event), nil
	}
}

// Close implements EventStream. It is safe to call more than once.
func (s *eventStream) Close() error {
	s.cancel()
	return nil
}

// mapErr translates a gRPC status into the package's sentinel errors. A
// cancellation or deadline the caller asked for comes back as the context
// error; one it did not, such as a connection that went away, is
// ErrUnavailable.
func mapErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("poolmgr: %w", err)
	}
	var sentinel error
	switch st.Code() {
	case codes.ResourceExhausted:
		sentinel = ErrExhausted
	case codes.NotFound:
		sentinel = ErrNotFound
	case codes.AlreadyExists:
		sentinel = ErrAlreadyExists
	case codes.Unavailable:
		sentinel = ErrUnavailable
	case codes.InvalidArgument:
		sentinel = ErrInvalid
	case codes.Canceled, codes.DeadlineExceeded:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		sentinel = ErrUnavailable
	default:
		return fmt.Errorf("poolmgr: %s: %s", st.Code(), st.Message())
	}
	return fmt.Errorf("%w: %s", sentinel, st.Message())
}

// eventFromProto converts one wire Event.
func eventFromProto(e *poolmgrv1.Event) *Event {
	out := &Event{
		ID:    e.GetId(),
		Pool:  PoolRef{Name: e.GetPoolName(), Namespace: e.GetPoolNamespace()},
		VMUID: e.GetVmUid(),
		Type:  e.GetType(),
		At:    e.GetCreatedAt().AsTime(),
	}
	if e.GetPayloadJson() != "" {
		out.Payload = json.RawMessage(e.GetPayloadJson())
	}
	return out
}

// Compile-time interface checks.
var (
	_ Client      = (*client)(nil)
	_ EventStream = (*eventStream)(nil)
)
