package flintlock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	sshv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmsshproxy/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

//= docs/requirements/05-hosts.md#flintlock-client
//# The Runner SHALL communicate with Hosts through the flintlock
//# `microvm.services.api.v1alpha1`, `microvmexec.services.api.v1alpha1` and
//# `microvmsshproxy.services.api.v1alpha1` gRPC APIs using the generated
//# clients from the flintlock `api` module.

//= docs/requirements/05-hosts.md#flintlock-client
//# The Runner SHALL NOT call `CreateMicroVM` or `DeleteMicroVM` on
//# any Host.

// Client is the Runner's client for one Host. It owns one gRPC connection
// (HO-002) and drives the three generated clients over it: MicroVM for
// ServerInfo, GetMicroVM and ListMicroVMs, MicroVMExec for the exec Guest
// Transport and MicroVMSSHProxy for the ssh one. Every stream it opens
// shares that connection (EX-050).
//
// It has no CreateMicroVM and no DeleteMicroVM method and calls neither:
// the MicroVM client it holds is unexported, so nothing outside this file
// can reach those RPCs through it, and nothing in this file invokes them
// (HO-007). Creating and deleting MicroVMs is the Pool Manager's job.
//
// A Client is safe for concurrent use; gRPC's is, and the Client adds no
// mutable state beyond the closed flag.
type Client struct {
	ep   Endpoint
	conn *grpc.ClientConn

	vms  mvmv1.MicroVMClient
	exec execv1.MicroVMExecClient
	ssh  sshv1.MicroVMSSHProxyClient

	closeOnce sync.Once
	closeErr  error
}

// newClient builds the Client over an established connection. It keeps no
// logger of its own: what a Host answers is reported to the caller as an
// error naming the Host, and the components that decide what is worth
// saying about a Host -- Probe and the Registry -- have loggers of their
// own.
func newClient(ep Endpoint, conn *grpc.ClientConn) *Client {
	return &Client{
		ep:   ep,
		conn: conn,
		vms:  mvmv1.NewMicroVMClient(conn),
		exec: execv1.NewMicroVMExecClient(conn),
		ssh:  sshv1.NewMicroVMSSHProxyClient(conn),
	}
}

// Name implements HostClient.
func (c *Client) Name() string { return c.ep.Name }

// Endpoint returns the Endpoint the Client was dialled with.
func (c *Client) Endpoint() Endpoint { return c.ep }

// ServerInfo implements HostClient (HO-011). A Host that predates the RPC
// answers UNIMPLEMENTED, which becomes ErrUnimplemented (HO-013).
func (c *Client) ServerInfo(ctx context.Context) (*HostInfo, error) {
	resp, err := c.vms.ServerInfo(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapErr(c.Name(), "ServerInfo", err)
	}
	return &HostInfo{
		Name:         c.Name(),
		VersionKnown: true,
		Version:      resp.GetVersion().GetVersion(),
		BuildDate:    resp.GetVersion().GetBuildDate(),
		Commit:       resp.GetVersion().GetCommitHash(),
		Uptime:       resp.GetUptime().AsDuration(),
		Exec: GuestService{
			Enabled: resp.GetExec().GetEnabled(),
			Address: resp.GetExec().GetAddress(),
		},
		SSHProxy: GuestService{
			Enabled: resp.GetSshProxy().GetEnabled(),
			Address: resp.GetSshProxy().GetAddress(),
		},
	}, nil
}

// GetMicroVM implements HostClient (SC-031). A Host that does not hold the
// MicroVM answers NOT_FOUND, which becomes ErrNotFound.
func (c *Client) GetMicroVM(ctx context.Context, uid string) (*types.MicroVM, error) {
	resp, err := c.vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: uid})
	if err != nil {
		return nil, mapErr(c.Name(), "GetMicroVM "+uid, err)
	}
	return resp.GetMicrovm(), nil
}

//= docs/requirements/05-hosts.md#flintlock-client
//# The Runner SHALL only call `ListMicroVMs` with its own namespace
//# and only as a health probe fallback.

// ListMicroVMs implements HostClient. The request carries the namespace it
// is given and no name filter, so it lists nothing outside the Runner's own
// namespace; Probe is the only caller in the Runner, and only on the Host
// whose ServerInfo is unimplemented (HO-008, HO-013).
func (c *Client) ListMicroVMs(ctx context.Context, namespace string) ([]*types.MicroVM, error) {
	if namespace == "" {
		return nil, fmt.Errorf("flintlock: host %s: ListMicroVMs needs the Runner's namespace", c.Name())
	}
	resp, err := c.vms.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Namespace: namespace})
	if err != nil {
		return nil, mapErr(c.Name(), "ListMicroVMs "+namespace, err)
	}
	return resp.GetMicrovm(), nil
}

//= docs/requirements/02-executor.md#guest-transport
//# The `exec` Guest Transport SHALL use the flintlock
//# `MicroVMExec.ExecCommand` streaming RPC of the Host that runs the MicroVM.

//= docs/requirements/02-executor.md#guest-transport
//# The Guest Transport SHALL reuse the Host client's gRPC
//# connection rather than opening a new connection per Stage.

// Exec implements HostClient: it opens an ExecCommand stream on the
// Client's own connection, so a Stage costs a stream and never a
// connection. The returned stream reports the Host's errors as the package
// sentinels; io.EOF still means the stream ended normally.
func (c *Client) Exec(ctx context.Context) (ExecStream, error) {
	stream, err := c.exec.ExecCommand(ctx)
	if err != nil {
		return nil, mapErr(c.Name(), "ExecCommand", err)
	}
	return &execStream{host: c.Name(), stream: stream}, nil
}

//= docs/requirements/02-executor.md#guest-transport
//# The `ssh` Guest Transport SHALL connect to the guest's SSH
//# service through the flintlock `MicroVMSSHProxy.SSHProxy` streaming RPC of
//# the Host that runs the MicroVM.

// SSHProxy implements HostClient: it opens an SSHProxy stream on the
// Client's own connection (EX-050), sends the uid message that names the
// MicroVM, and returns the stream as a byte pipe to the guest's sshd. The
// caller closes it; Close half-closes the stream and releases it.
//
// The uid message goes out before SSHProxy returns because flintlockd
// requires it as the first message of the exchange and answers anything
// else with INVALID_ARGUMENT.
func (c *Client) SSHProxy(ctx context.Context, uid string) (io.ReadWriteCloser, error) {
	if uid == "" {
		return nil, fmt.Errorf("flintlock: host %s: SSHProxy needs a microvm uid", c.Name())
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.ssh.SSHProxy(streamCtx)
	if err != nil {
		cancel()
		return nil, mapErr(c.Name(), "SSHProxy", err)
	}
	if err := stream.Send(&sshv1.SSHProxyRequest{
		Payload: &sshv1.SSHProxyRequest_Uid{Uid: uid},
	}); err != nil {
		cancel()
		// A Send that fails on an established stream reports io.EOF and
		// leaves the status on Recv, which is where the Host's reason is.
		if errors.Is(err, io.EOF) {
			_, err = stream.Recv()
		}
		return nil, mapErr(c.Name(), "SSHProxy "+uid, err)
	}
	return &sshConn{host: c.Name(), uid: uid, stream: stream, cancel: cancel}, nil
}

// Close implements HostClient. In-flight streams fail with Unavailable.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if err := c.conn.Close(); err != nil {
			c.closeErr = fmt.Errorf("flintlock: host %s: closing connection: %w", c.Name(), err)
		}
	})
	return c.closeErr
}

// execStream adapts the generated bidi stream so that its errors are the
// package's sentinels. It adds no buffering: Send and Recv go straight to
// gRPC.
type execStream struct {
	host   string
	stream grpc.BidiStreamingClient[execv1.ExecCommandRequest, execv1.ExecCommandResponse]
}

// Send implements ExecStream. gRPC reports a stream that has already failed
// as io.EOF and keeps the status for Recv, so io.EOF is passed through
// unchanged and the caller reads the reason from Recv.
func (s *execStream) Send(req *execv1.ExecCommandRequest) error {
	err := s.stream.Send(req)
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return mapErr(s.host, "ExecCommand send", err)
}

// Recv implements ExecStream. io.EOF is the normal end of a stream and is
// returned as it is; anything else is mapped onto a sentinel and names the
// Host.
func (s *execStream) Recv() (*execv1.ExecCommandResponse, error) {
	resp, err := s.stream.Recv()
	if err == nil {
		return resp, nil
	}
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	return nil, mapErr(s.host, "ExecCommand receive", err)
}

// CloseSend implements ExecStream.
func (s *execStream) CloseSend() error {
	if err := s.stream.CloseSend(); err != nil {
		return mapErr(s.host, "ExecCommand close send", err)
	}
	return nil
}

// sshConn is the byte pipe over an SSHProxy stream. Reads and writes may
// each be driven by their own goroutine, as an SSH client does; gRPC allows
// that as long as neither direction is used from two goroutines at once,
// which the mutexes below enforce.
type sshConn struct {
	host   string
	uid    string
	stream grpc.BidiStreamingClient[sshv1.SSHProxyRequest, sshv1.SSHProxyResponse]
	cancel context.CancelFunc

	readMu  sync.Mutex
	pending []byte
	readErr error

	writeMu sync.Mutex

	closeOnce sync.Once
}

// Read implements io.Reader, handing out one response message at a time and
// keeping what does not fit in p for the next call.
func (c *sshConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.pending) == 0 {
		if c.readErr != nil {
			return 0, c.readErr
		}
		resp, err := c.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.readErr = io.EOF
			} else {
				c.readErr = mapErr(c.host, "SSHProxy "+c.uid+" receive", err)
			}
			return 0, c.readErr
		}
		c.pending = resp.GetData()
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// Write implements io.Writer. The bytes are copied because the caller owns
// p and the message travels to gRPC's marshaller.
func (c *sshConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	data := make([]byte, len(p))
	copy(data, p)
	if err := c.stream.Send(&sshv1.SSHProxyRequest{
		Payload: &sshv1.SSHProxyRequest_Data{Data: data},
	}); err != nil {
		return 0, mapErr(c.host, "SSHProxy "+c.uid+" send", err)
	}
	return len(p), nil
}

// Close implements io.Closer: it half-closes the stream so that the Host
// sees the end of the session and then cancels it, which releases the
// stream's resources without touching the Client's connection (EX-050).
func (c *sshConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stream.CloseSend()
		c.cancel()
	})
	return nil
}

// mapErr turns a gRPC status onto the package's sentinel errors so that
// callers never import gRPC codes. op names the call for the message; the
// Host is always named, because an error that reaches a Job log or an
// operator has to say which Host produced it.
func mapErr(host, op string, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("flintlock: host %s: %s: %w", host, op, err)
	}
	var sentinel error
	switch st.Code() {
	case codes.NotFound:
		sentinel = ErrNotFound
	case codes.Unimplemented:
		sentinel = ErrUnimplemented
	case codes.Unauthenticated:
		sentinel = ErrUnauthenticated
	case codes.Unavailable:
		sentinel = ErrUnavailable
	case codes.DeadlineExceeded:
		sentinel = errors.Join(ErrUnavailable, context.DeadlineExceeded)
	case codes.Canceled:
		sentinel = errors.Join(ErrUnavailable, context.Canceled)
	default:
		return fmt.Errorf("flintlock: host %s: %s: %s: %w", host, op, st.Code(), err)
	}
	return fmt.Errorf("flintlock: host %s: %s: %s: %w", host, op, st.Message(), sentinel)
}

// Compile-time checks.
var (
	_ HostClient         = (*Client)(nil)
	_ ExecStream         = (*execStream)(nil)
	_ io.ReadWriteCloser = (*sshConn)(nil)
)
