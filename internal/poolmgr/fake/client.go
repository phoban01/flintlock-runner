package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// loopbackBuffer is the in-memory listener's buffer size.
const loopbackBuffer = 1 << 20

// loopback is the in-memory gRPC server behind PoolManager.Client.
type loopback struct {
	lis *bufconn.Listener
	srv *grpc.Server
}

// newLoopback starts the in-memory gRPC server behind Client. New calls it,
// so that p.loop is written once before any goroutine can see the
// PoolManager and read without synchronisation afterwards; stop shuts the
// server down.
func (p *PoolManager) newLoopback() *loopback {
	lis := bufconn.Listen(loopbackBuffer)
	srv := p.newGRPCServer()
	go func() { _ = srv.Serve(lis) }()
	return &loopback{lis: lis, srv: srv}
}

// loopbackConn returns a new connection to the in-memory server, or
// ErrStopped once the fake has shut down and the server is gone.
func (p *PoolManager) loopbackConn() (*grpc.ClientConn, error) {
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return nil, ErrStopped
	}
	lis := p.loop.lis
	return grpc.NewClient("passthrough:///fake-poolmgr",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

// Client is a poolmgr.Client over a gRPC connection to a fake Pool Manager:
// the loopback connection from PoolManager.Client, or, through Dial, a TCP
// connection to a served one. It maps status codes onto the poolmgr
// sentinel errors the way the Runner's client does, so the two are
// interchangeable to the Scheduler.
type Client struct {
	conn   *grpc.ClientConn
	admin  poolmgrv1.PoolAdminClient
	lease  poolmgrv1.LeaseClient
	events poolmgrv1.EventsClient
	// err, when set, is returned by every call; it records a failure to
	// build the connection.
	err error
}

func newClient(conn *grpc.ClientConn) *Client {
	return &Client{
		conn:   conn,
		admin:  poolmgrv1.NewPoolAdminClient(conn),
		lease:  poolmgrv1.NewLeaseClient(conn),
		events: poolmgrv1.NewEventsClient(conn),
	}
}

// Dial connects to a fake Pool Manager served at addr in plaintext, for
// tests that run the standalone binary. The Runner's own client, with TLS
// and deadlines, is package poolmgr's.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("fake poolmgr: dial %s: %w", addr, err)
	}
	return newClient(conn), nil
}

// CreatePool implements poolmgr.PoolAdmin.
func (c *Client) CreatePool(ctx context.Context, spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	if c.err != nil {
		return nil, c.err
	}
	resp, err := c.admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: specToProto(spec)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp)
}

// UpdatePool implements poolmgr.PoolAdmin.
func (c *Client) UpdatePool(ctx context.Context, spec poolmgr.PoolSpec) (*poolmgr.Pool, error) {
	if c.err != nil {
		return nil, c.err
	}
	resp, err := c.admin.UpdatePool(ctx, &poolmgrv1.UpdatePoolRequest{Spec: specToProto(spec)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp)
}

// DeletePool implements poolmgr.PoolAdmin.
func (c *Client) DeletePool(ctx context.Context, ref poolmgr.PoolRef) error {
	if c.err != nil {
		return c.err
	}
	_, err := c.admin.DeletePool(ctx, &poolmgrv1.DeletePoolRequest{Ref: refToProto(ref)})
	return mapErr(ctx, err)
}

// GetPool implements poolmgr.PoolAdmin.
func (c *Client) GetPool(ctx context.Context, ref poolmgr.PoolRef) (*poolmgr.Pool, error) {
	if c.err != nil {
		return nil, c.err
	}
	resp, err := c.admin.GetPool(ctx, &poolmgrv1.GetPoolRequest{Ref: refToProto(ref)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp)
}

// ListPools implements poolmgr.PoolAdmin.
func (c *Client) ListPools(ctx context.Context, namespace string) ([]*poolmgr.Pool, error) {
	if c.err != nil {
		return nil, c.err
	}
	req := &poolmgrv1.ListPoolsRequest{}
	if namespace != "" {
		req.Namespace = &namespace
	}
	resp, err := c.admin.ListPools(ctx, req)
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	out := make([]*poolmgr.Pool, 0, len(resp.GetPools()))
	for _, pool := range resp.GetPools() {
		p, err := poolFromProto(pool)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ClaimVM implements poolmgr.Lease.
func (c *Client) ClaimVM(ctx context.Context, pool poolmgr.PoolRef) (*poolmgr.Claim, error) {
	if c.err != nil {
		return nil, c.err
	}
	resp, err := c.lease.ClaimVM(ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(pool)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return &poolmgr.Claim{
		LeaseID:           resp.GetLeaseId(),
		VMUID:             resp.GetVmUid(),
		NetworkInterfaces: resp.GetNetworkInterfaces(),
		Host:              poolmgr.HostRef{Name: resp.GetHost().GetName(), Address: resp.GetHost().GetAddress()},
	}, nil
}

// Heartbeat implements poolmgr.Lease.
func (c *Client) Heartbeat(ctx context.Context, leaseID string) (time.Time, error) {
	if c.err != nil {
		return time.Time{}, c.err
	}
	resp, err := c.lease.Heartbeat(ctx, &poolmgrv1.HeartbeatRequest{LeaseId: leaseID})
	if err != nil {
		return time.Time{}, mapErr(ctx, err)
	}
	return resp.GetExpiresAt().AsTime(), nil
}

// ReleaseVM implements poolmgr.Lease.
func (c *Client) ReleaseVM(ctx context.Context, leaseID string) error {
	if c.err != nil {
		return c.err
	}
	_, err := c.lease.ReleaseVM(ctx, &poolmgrv1.ReleaseVMRequest{LeaseId: leaseID})
	return mapErr(ctx, err)
}

// Subscribe implements poolmgr.Events. The stream lives until Close, the
// context ends, or the server drops it.
func (c *Client) Subscribe(ctx context.Context, filter poolmgr.EventFilter) (poolmgr.EventStream, error) {
	if c.err != nil {
		return nil, c.err
	}
	req := &poolmgrv1.SubscribeRequest{}
	if filter.Pool != nil {
		req.Pool = refToProto(*filter.Pool)
	}
	ctx, cancel := context.WithCancel(ctx)
	stream, err := c.events.Subscribe(ctx, req)
	if err != nil {
		cancel()
		return nil, mapErr(ctx, err)
	}
	return newEventStream(ctx, cancel, stream), nil
}

// Close implements poolmgr.Client.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// eventStream adapts the generated server-streaming client to
// poolmgr.EventStream. One reader goroutine, started at Subscribe, owns the
// gRPC stream and hands results to Recv, so a Recv abandoned on its context
// never loses the event it was waiting for. Recv is not for concurrent use.
type eventStream struct {
	cancel context.CancelFunc
	// streamCtx ends on Close; the reader exits with it.
	streamCtx context.Context
	results   chan streamResult
	// terminal is the error the stream ended with, once Recv has returned it.
	terminal error
}

type streamResult struct {
	e   *poolmgrv1.Event
	err error
}

func newEventStream(ctx context.Context, cancel context.CancelFunc, stream grpc.ServerStreamingClient[poolmgrv1.Event]) *eventStream {
	s := &eventStream{cancel: cancel, streamCtx: ctx, results: make(chan streamResult)}
	go func() {
		for {
			e, err := stream.Recv()
			select {
			case s.results <- streamResult{e, err}:
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

// Recv implements poolmgr.EventStream. Any end of the stream that the
// caller did not ask for is reported as ErrUnavailable so the Tracker
// re-subscribes (PL-051, TD-010).
func (s *eventStream) Recv(ctx context.Context) (*poolmgr.Event, error) {
	if s.terminal != nil {
		return nil, s.terminal
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.streamCtx.Done():
		s.terminal = fmt.Errorf("%w: events stream closed", poolmgr.ErrUnavailable)
		return nil, s.terminal
	case r := <-s.results:
		if r.err != nil {
			s.terminal = fmt.Errorf("%w: events stream ended: %s", poolmgr.ErrUnavailable, status.Convert(r.err).Message())
			return nil, s.terminal
		}
		return eventFromProto(r.e), nil
	}
}

// Close implements poolmgr.EventStream.
func (s *eventStream) Close() error {
	s.cancel()
	return nil
}

func eventFromProto(e *poolmgrv1.Event) *poolmgr.Event {
	out := &poolmgr.Event{
		ID:    e.GetId(),
		Pool:  poolmgr.PoolRef{Name: e.GetPoolName(), Namespace: e.GetPoolNamespace()},
		VMUID: e.GetVmUid(),
		Type:  e.GetType(),
		At:    e.GetCreatedAt().AsTime(),
	}
	if e.GetPayloadJson() != "" {
		out.Payload = json.RawMessage(e.GetPayloadJson())
	}
	return out
}

// mapErr translates a gRPC status into the poolmgr sentinel errors. A
// cancellation the caller asked for comes back as the context error; one
// the caller did not ask for, such as a closed connection, is
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
		sentinel = poolmgr.ErrExhausted
	case codes.NotFound:
		sentinel = poolmgr.ErrNotFound
	case codes.AlreadyExists:
		sentinel = poolmgr.ErrAlreadyExists
	case codes.Unavailable:
		sentinel = poolmgr.ErrUnavailable
	case codes.InvalidArgument:
		sentinel = poolmgr.ErrInvalid
	case codes.Canceled, codes.DeadlineExceeded:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		sentinel = poolmgr.ErrUnavailable
	default:
		return fmt.Errorf("poolmgr: %s: %s", st.Code(), st.Message())
	}
	return fmt.Errorf("%w: %s", sentinel, st.Message())
}

// Compile-time interface checks.
var (
	_ poolmgr.Client      = (*Client)(nil)
	_ poolmgr.EventStream = (*eventStream)(nil)
)
