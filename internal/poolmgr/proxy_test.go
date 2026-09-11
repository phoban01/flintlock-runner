package poolmgr_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// The recording proxy is a gRPC server that speaks the three
// poolmgr.v1alpha1 services and forwards every call to the fake Pool
// Manager behind it. It exists because two of the client's requirements are
// about the connection rather than about an answer: PL-002, the deadline on
// every unary call, and PL-003 and PL-006, TLS and one long-lived
// connection. Neither is visible from the fake's replies, and both are
// visible here: the proxy records the deadline each call arrived with and
// counts the TCP connections it accepted, while the behaviour under the
// call is still the fake Pool Manager's.

// proxy is a recording pass-through in front of a fake Pool Manager.
type proxy struct {
	poolmgrv1.UnimplementedPoolAdminServer
	poolmgrv1.UnimplementedLeaseServer
	poolmgrv1.UnimplementedEventsServer

	admin  poolmgrv1.PoolAdminClient
	lease  poolmgrv1.LeaseClient
	events poolmgrv1.EventsClient

	// addr is the proxy's own listen address.
	addr string

	mu sync.Mutex
	// deadlines records the remaining deadline of every unary call, by
	// method name, in arrival order.
	deadlines map[string][]time.Duration
	// noDeadline names the methods that arrived without one.
	noDeadline []string
	// conns counts the TCP connections the proxy accepted.
	conns int
}

// startProxy serves a recording proxy in front of the Pool Manager at
// upstream. When tlsCert is set, the proxy serves TLS with that key pair.
func startProxy(t *testing.T, upstream string, tlsCert, tlsKey string) *proxy {
	t.Helper()
	conn, err := grpc.NewClient(upstream, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("proxy: dialling %s: %v", upstream, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	p := &proxy{
		admin:     poolmgrv1.NewPoolAdminClient(conn),
		lease:     poolmgrv1.NewLeaseClient(conn),
		events:    poolmgrv1.NewEventsClient(conn),
		deadlines: make(map[string][]time.Duration),
	}

	var opts []grpc.ServerOption
	if tlsCert != "" {
		creds, err := credentials.NewServerTLSFromFile(tlsCert, tlsKey)
		if err != nil {
			t.Fatalf("proxy: server credentials: %v", err)
		}
		opts = append(opts, grpc.Creds(creds))
	}
	opts = append(opts, grpc.ChainUnaryInterceptor(p.record))
	srv := grpc.NewServer(opts...)
	poolmgrv1.RegisterPoolAdminServer(srv, p)
	poolmgrv1.RegisterLeaseServer(srv, p)
	poolmgrv1.RegisterEventsServer(srv, p)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy: listen: %v", err)
	}
	p.addr = lis.Addr().String()
	counting := &countingListener{Listener: lis, proxy: p}
	go func() { _ = srv.Serve(counting) }()
	t.Cleanup(srv.Stop)
	return p
}

// record notes the deadline a unary call arrived with.
func (p *proxy) record(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	p.mu.Lock()
	if deadline, ok := ctx.Deadline(); ok {
		p.deadlines[info.FullMethod] = append(p.deadlines[info.FullMethod], time.Until(deadline))
	} else {
		p.noDeadline = append(p.noDeadline, info.FullMethod)
	}
	p.mu.Unlock()
	return handler(ctx, req)
}

// seen returns the recorded deadlines by method and the methods that
// arrived without one.
func (p *proxy) seen() (map[string][]time.Duration, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string][]time.Duration, len(p.deadlines))
	for method, ds := range p.deadlines {
		out[method] = append([]time.Duration(nil), ds...)
	}
	return out, append([]string(nil), p.noDeadline...)
}

// connections is how many TCP connections the proxy has accepted.
func (p *proxy) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns
}

// countingListener counts accepted connections.
type countingListener struct {
	net.Listener
	proxy *proxy
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.proxy.mu.Lock()
		l.proxy.conns++
		l.proxy.mu.Unlock()
	}
	return conn, err
}

// The forwarding handlers. Each is one RPC of the three services.

func (p *proxy) CreatePool(ctx context.Context, req *poolmgrv1.CreatePoolRequest) (*poolmgrv1.Pool, error) {
	return p.admin.CreatePool(ctx, req)
}

func (p *proxy) UpdatePool(ctx context.Context, req *poolmgrv1.UpdatePoolRequest) (*poolmgrv1.Pool, error) {
	return p.admin.UpdatePool(ctx, req)
}

func (p *proxy) DeletePool(ctx context.Context, req *poolmgrv1.DeletePoolRequest) (*emptypb.Empty, error) {
	return p.admin.DeletePool(ctx, req)
}

func (p *proxy) GetPool(ctx context.Context, req *poolmgrv1.GetPoolRequest) (*poolmgrv1.Pool, error) {
	return p.admin.GetPool(ctx, req)
}

func (p *proxy) ListPools(ctx context.Context, req *poolmgrv1.ListPoolsRequest) (*poolmgrv1.ListPoolsResponse, error) {
	return p.admin.ListPools(ctx, req)
}

func (p *proxy) ClaimVM(ctx context.Context, req *poolmgrv1.ClaimVMRequest) (*poolmgrv1.ClaimVMResponse, error) {
	return p.lease.ClaimVM(ctx, req)
}

func (p *proxy) Heartbeat(ctx context.Context, req *poolmgrv1.HeartbeatRequest) (*poolmgrv1.HeartbeatResponse, error) {
	return p.lease.Heartbeat(ctx, req)
}

func (p *proxy) ReleaseVM(ctx context.Context, req *poolmgrv1.ReleaseVMRequest) (*emptypb.Empty, error) {
	return p.lease.ReleaseVM(ctx, req)
}

// Subscribe forwards the server stream, so that a client of the proxy sees
// the fake Pool Manager's events.
func (p *proxy) Subscribe(req *poolmgrv1.SubscribeRequest, out grpc.ServerStreamingServer[poolmgrv1.Event]) error {
	ctx, cancel := context.WithCancel(out.Context())
	defer cancel()
	in, err := p.events.Subscribe(ctx, req)
	if err != nil {
		return err
	}
	for {
		event, err := in.Recv()
		if err != nil {
			return err
		}
		if err := out.Send(event); err != nil {
			return err
		}
	}
}
