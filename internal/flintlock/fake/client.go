package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// memStreamBuffer is how many messages an in-memory exec stream buffers in
// each direction, standing in for gRPC's flow-control window so that a
// client can send a few stdin chunks before the handler reads them.
const memStreamBuffer = 64

// Client is the in-memory flintlock.PoolHostClient over a Host. It goes
// through the same store and the same exec handler as the gRPC services, so
// what it returns is what a real client would map a Host's answer onto,
// including the sentinel errors.
type Client struct {
	host  *Host
	token string

	ctx    context.Context
	cancel context.CancelFunc
}

// newClient binds a client to h presenting token.
func (h *Host) newClient(token string) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{host: h, token: token, ctx: ctx, cancel: cancel}
}

// Name implements flintlock.HostClient.
func (c *Client) Name() string { return c.host.cfg.Name }

// admit is the in-process counterpart of the gRPC interceptors: it fails
// once the client or the Host is closed, blocks while the Host is
// Unresponsive and then checks the token (TD-024, TD-025). Errors wrap the
// flintlock sentinels; a wait that ends at the caller's deadline wraps both
// flintlock.ErrUnavailable and the context error.
func (c *Client) admit(ctx context.Context) error {
	if c.ctx.Err() != nil {
		return fmt.Errorf("fake host %s: client closed: %w", c.Name(), flintlock.ErrUnavailable)
	}
	if err := c.host.awaitResponsive(ctx); err != nil {
		if errors.Is(err, errClosed) {
			return fmt.Errorf("fake host %s: %w", c.Name(), flintlock.ErrUnavailable)
		}
		return fmt.Errorf("fake host %s: unresponsive: %w", c.Name(), errors.Join(flintlock.ErrUnavailable, err))
	}
	if c.host.cfg.Token != "" && c.token != c.host.cfg.Token {
		return fmt.Errorf("fake host %s: invalid auth token: %w", c.Name(), flintlock.ErrUnauthenticated)
	}
	return nil
}

// ServerInfo implements flintlock.HostClient (TD-026).
func (c *Client) ServerInfo(ctx context.Context) (*flintlock.HostInfo, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	resp, err := c.host.serverInfo()
	if err != nil {
		return nil, statusToSentinel(c.Name(), err)
	}
	return &flintlock.HostInfo{
		Name:         c.Name(),
		VersionKnown: true,
		Version:      resp.GetVersion().GetVersion(),
		BuildDate:    resp.GetVersion().GetBuildDate(),
		Commit:       resp.GetVersion().GetCommitHash(),
		Uptime:       resp.GetUptime().AsDuration(),
		Exec: flintlock.GuestService{
			Enabled: resp.GetExec().GetEnabled(),
			Address: resp.GetExec().GetAddress(),
		},
		SSHProxy: flintlock.GuestService{
			Enabled: resp.GetSshProxy().GetEnabled(),
			Address: resp.GetSshProxy().GetAddress(),
		},
	}, nil
}

// GetMicroVM implements flintlock.HostClient.
func (c *Client) GetMicroVM(ctx context.Context, uid string) (*types.MicroVM, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	vm, err := c.host.getMicroVM(uid)
	if err != nil {
		return nil, statusToSentinel(c.Name(), err)
	}
	return vm, nil
}

// ListMicroVMs implements flintlock.HostClient.
func (c *Client) ListMicroVMs(ctx context.Context, namespace string) ([]*types.MicroVM, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	vms, err := c.host.listMicroVMs(namespace, nil)
	if err != nil {
		return nil, statusToSentinel(c.Name(), err)
	}
	return vms, nil
}

// Exec implements flintlock.HostClient. The returned stream is served by
// the same handler as the gRPC service (TD-021, TD-023); it ends when ctx
// is cancelled, the exit code has been received or the client is closed.
func (c *Client) Exec(ctx context.Context) (flintlock.ExecStream, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	c.host.mu.Lock()
	closed := c.host.closed
	if !closed {
		c.host.wg.Add(1)
	}
	c.host.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("fake host %s: %w", c.Name(), flintlock.ErrUnavailable)
	}

	s := newMemExecStream(ctx, c.ctx, c.host.ctx, c.Name())
	go func() {
		defer c.host.wg.Done()
		err := c.host.execCommand(s.server())
		s.finish(statusToSentinel(c.Name(), err))
	}()
	return s, nil
}

// SSHProxy implements flintlock.HostClient. The fake serves no SSH proxy:
// this always fails with flintlock.ErrUnimplemented and Serve registers no
// MicroVMSSHProxy server.
//
// FakeHostConfig.SSHProxyEnabled is therefore a reporting knob and nothing
// more. It decides what ServerInfo advertises, including an address, so that
// a test can drive the Runner's capability handling (TD-026); it does not
// make the service answer, so no SSH scenario can be pointed at it.
func (c *Client) SSHProxy(ctx context.Context, _ string) (io.ReadWriteCloser, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("fake host %s: ssh proxy: %w", c.Name(), flintlock.ErrUnimplemented)
}

// Close implements flintlock.HostClient. In-flight exec streams fail.
func (c *Client) Close() error {
	c.cancel()
	return nil
}

// CreateMicroVM implements flintlock.HostAdminClient (TD-022).
func (c *Client) CreateMicroVM(ctx context.Context, spec *types.MicroVMSpec) (*types.MicroVM, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	vm, err := c.host.createMicroVM(spec)
	if err != nil {
		return nil, statusToSentinel(c.Name(), err)
	}
	return vm, nil
}

// DeleteMicroVM implements flintlock.HostAdminClient.
func (c *Client) DeleteMicroVM(ctx context.Context, uid string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	return statusToSentinel(c.Name(), c.host.deleteMicroVM(uid))
}

// memExecStream is an in-memory bidirectional stream: the client half is a
// flintlock.ExecStream, the server half an execStream. Each direction is a
// buffered channel; the stream context ends when the caller's context, the
// client value, the Host or the handler ends.
type memExecStream struct {
	host   string
	ctx    context.Context
	cancel context.CancelFunc

	reqs  chan *execv1.ExecCommandRequest
	resps chan *execv1.ExecCommandResponse

	mu         sync.Mutex
	sendClosed bool
	closeSend  chan struct{}
	done       chan struct{}
	err        error
}

// newMemExecStream builds a stream on ctx that also ends with the Client
// (the HostClient.Close contract) and with the Host. The Host has to be in
// there: Close waits for the handler behind the stream, so a stream nobody
// speaks on must not be able to outlive it (TD-021). host names the Host in
// the errors Recv reports.
func newMemExecStream(ctx, clientCtx, hostCtx context.Context, host string) *memExecStream {
	ctx, cancel := context.WithCancel(ctx)
	s := &memExecStream{
		host:      host,
		ctx:       ctx,
		cancel:    cancel,
		reqs:      make(chan *execv1.ExecCommandRequest, memStreamBuffer),
		resps:     make(chan *execv1.ExecCommandResponse, memStreamBuffer),
		closeSend: make(chan struct{}),
		done:      make(chan struct{}),
	}
	stopClient := context.AfterFunc(clientCtx, cancel)
	stopHost := context.AfterFunc(hostCtx, cancel)
	go func() {
		<-s.done
		stopClient()
		stopHost()
	}()
	return s
}

// finish records the handler's result and ends the stream.
//
// A handler error that wraps io.EOF is flattened to its text first. Recv
// reports io.EOF only for a stream that ended normally, and the handler
// wraps the io.EOF it gets when a client half-closes before the start
// message, exactly as flintlockd does; over gRPC that becomes an Unknown
// status carrying the text, so the in-memory stream must not hand it back
// as something errors.Is(err, io.EOF) accepts.
func (s *memExecStream) finish(err error) {
	if err != nil && errors.Is(err, io.EOF) {
		err = errors.New(err.Error())
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	close(s.done)
	s.cancel()
}

// Send implements flintlock.ExecStream.
func (s *memExecStream) Send(req *execv1.ExecCommandRequest) error {
	s.mu.Lock()
	closed := s.sendClosed
	s.mu.Unlock()
	if closed {
		return errors.New("fake exec stream: Send after CloseSend")
	}
	select {
	case <-s.done:
		// The stream is over; like gRPC, report EOF and let Recv carry the
		// status.
		return io.EOF
	case <-s.ctx.Done():
		return s.ctx.Err()
	case s.reqs <- req:
		return nil
	}
}

// Recv implements flintlock.ExecStream.
//
// The three end conditions are ranked, because finish closes done and then
// cancels the stream context, so at the end of a healthy exchange both are
// ready at once and a single select over them would pick at random. A
// buffered response always wins, the handler's own result comes next, and
// the context error is only reached while the handler is still running,
// which is when the caller or the Host, rather than the command, ended the
// stream. The ranking cannot rest on the checks alone: finish can land
// between them and the select below, so the context branch re-checks done
// and yields to it.
func (s *memExecStream) Recv() (*execv1.ExecCommandResponse, error) {
	for {
		select {
		case resp := <-s.resps:
			return resp, nil
		default:
		}
		select {
		case <-s.done:
			return s.result()
		default:
		}
		select {
		case resp := <-s.resps:
			return resp, nil
		case <-s.done:
			// Loop so that anything the handler sent before finishing is
			// delivered ahead of the result.
		case <-s.ctx.Done():
			// The ranking above is a hint, not a guarantee: finish closes
			// done and then cancels, so both can become ready while this
			// select is being entered, and the poll then picks at random.
			// A cancellation that merely accompanies the handler's result
			// must not be reported in its place.
			select {
			case <-s.done:
				continue
			default:
			}
			return nil, statusToSentinel(s.host, status.FromContextError(s.ctx.Err()).Err())
		}
	}
}

// result reports what the finished handler left behind: the next buffered
// response if there is one, then its error, then io.EOF for a stream that
// ended normally.
func (s *memExecStream) result() (*execv1.ExecCommandResponse, error) {
	select {
	case resp := <-s.resps:
		return resp, nil
	default:
	}
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err == nil {
		return nil, io.EOF
	}
	return nil, err
}

// CloseSend implements flintlock.ExecStream.
func (s *memExecStream) CloseSend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sendClosed {
		s.sendClosed = true
		close(s.closeSend)
	}
	return nil
}

// server returns the handler-side view of the stream.
func (s *memExecStream) server() execStream { return memServerStream{s} }

// memServerStream is the execStream the handler drives.
type memServerStream struct{ s *memExecStream }

// Context implements execStream.
func (m memServerStream) Context() context.Context { return m.s.ctx }

// Send implements execStream.
func (m memServerStream) Send(resp *execv1.ExecCommandResponse) error {
	select {
	case m.s.resps <- resp:
		return nil
	case <-m.s.ctx.Done():
		return status.FromContextError(m.s.ctx.Err()).Err()
	}
}

// Recv implements execStream.
func (m memServerStream) Recv() (*execv1.ExecCommandRequest, error) {
	// Deliver buffered requests before reporting the half-close, as gRPC
	// does.
	select {
	case req := <-m.s.reqs:
		return req, nil
	default:
	}
	select {
	case req := <-m.s.reqs:
		return req, nil
	case <-m.s.closeSend:
		select {
		case req := <-m.s.reqs:
			return req, nil
		default:
			return nil, io.EOF
		}
	case <-m.s.ctx.Done():
		return nil, status.Error(codes.Canceled, m.s.ctx.Err().Error())
	}
}

// Compile-time checks.
var (
	_ flintlock.ExecStream = (*memExecStream)(nil)
	_ execStream           = memServerStream{}
)
