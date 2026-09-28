// Package fakeexecagent is a test double of battery-operator's Exec Agent
// (battery-operator v0.1.0, docs/requirements/05-exec-agent.md), for the
// tests of the `agent-exec` Guest Transport (12-cluster-fleet.md, KF-190).
// battery-operator's own test doubles are internal to it, so this one
// speaks the same protocol here.
//
// Agent is the Exec Agent of one Host: it serves flintlock's MicroVMExec
// service and the ServerInfo and GetMicroVM calls of its MicroVM service
// over TLS (EA-002, EA-003), admits a call only with a bearer token that
// its Authorizer accepts for the MicroVM the call names (EA-010 to
// EA-013), and relays it to the Host's flintlockd, the fake Host's client,
// ending every exec response with the exit status frame or an error status
// (EA-020). World, in claims.go, is the API server side: the claims and the
// claim tokens that battery-operator's Client Library makes and the
// Authorizer checks. Reviewer, in review.go, is the Authorizer for a real
// API server instead, such as envtest's, whose claim tokens are real.
//
// It does not reach a real flintlockd, check the Host or hold a drain; the
// agent's own suite in battery-operator covers those.
package fakeexecagent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

var (
	// ErrUnauthenticated is what an Authorizer returns for a token that is
	// no claim token at all (EA-010). The Agent answers Unauthenticated.
	ErrUnauthenticated = errors.New("fakeexecagent: the token does not authenticate")
	// ErrPermissionDenied is what an Authorizer returns for a claim token
	// that does not open the MicroVM a call names (EA-011 to EA-013). The
	// Agent answers PermissionDenied.
	ErrPermissionDenied = errors.New("fakeexecagent: no Bound claim of the token names the microvm on this host")
)

// Authorizer decides whether a bearer token opens a MicroVM on a Host.
type Authorizer interface {
	// Authorize returns nil when token is a claim token of a Bound,
	// unexpired claim that names vmUID on the Host whose Node is node, and
	// ErrUnauthenticated or ErrPermissionDenied, or a wrap of either,
	// otherwise. An empty vmUID asks only whether token is a claim token.
	Authorize(ctx context.Context, token, node, vmUID string) error
}

// Config is one Agent.
type Config struct {
	// Node is the name of its Host's Node, which the claims it admits
	// name.
	Node string
	// Listen is where it serves; empty is a free loopback port.
	Listen string
	// CertFile and KeyFile are its serving certificate and key. They are
	// required: the exec API is never served in the clear (EA-002).
	CertFile string
	KeyFile  string
	// Upstream is the Host's flintlockd, normally a fake Host's client.
	Upstream flintlock.HostClient
	// Authorizer decides every call.
	Authorizer Authorizer
}

// Call is one call the Agent was asked to make, as it saw it.
type Call struct {
	// Method is the gRPC method's name, such as ExecCommand.
	Method string
	// Token is the bearer token the call carried, empty for none.
	Token string
	// VMUID is the MicroVM the call named, empty for ServerInfo.
	VMUID string
	// Admitted says whether the call was let through to flintlockd.
	Admitted bool
}

// Agent is a running test double of one Host's Exec Agent.
type Agent struct {
	cfg  Config
	lis  net.Listener
	srv  *grpc.Server
	done chan struct{}

	mu    sync.Mutex
	calls []Call
}

// Start serves an Agent until Close.
func Start(cfg Config) (*Agent, error) {
	switch {
	case cfg.Node == "":
		return nil, errors.New("fakeexecagent: Config.Node is required")
	case cfg.CertFile == "" || cfg.KeyFile == "":
		return nil, errors.New("fakeexecagent: a serving certificate and key are required")
	case cfg.Upstream == nil:
		return nil, errors.New("fakeexecagent: Config.Upstream is required")
	case cfg.Authorizer == nil:
		return nil, errors.New("fakeexecagent: Config.Authorizer is required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("fakeexecagent: loading the serving certificate: %w", err)
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("fakeexecagent: listening: %w", err)
	}
	a := &Agent{cfg: cfg, lis: lis, done: make(chan struct{})}
	a.srv = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})),
		// The Runner keeps its connections alive with pings (HO-002).
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: time.Second, PermitWithoutStream: true}),
	)
	mvmv1.RegisterMicroVMServer(a.srv, &microVMService{a: a})
	execv1.RegisterMicroVMExecServer(a.srv, &execService{a: a})
	go func() {
		defer close(a.done)
		_ = a.srv.Serve(lis)
	}()
	return a, nil
}

// Addr is the `address:port` the Agent serves on, which a claim's status
// gives as its agent address.
func (a *Agent) Addr() string { return a.lis.Addr().String() }

// Node is the name of the Agent's Host's Node.
func (a *Agent) Node() string { return a.cfg.Node }

// Calls returns every call the Agent has seen, in order.
func (a *Agent) Calls() []Call {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Call(nil), a.calls...)
}

// Close stops the Agent at once: every open exchange ends without its exit
// status, as it does when a real Exec Agent restarts.
func (a *Agent) Close() error {
	a.srv.Stop()
	<-a.done
	return nil
}

// admit authorizes one call and records it. It returns a status error.
func (a *Agent) admit(ctx context.Context, method, vmUID string) error {
	token := bearer(ctx)
	err := a.cfg.Authorizer.Authorize(ctx, token, a.cfg.Node, vmUID)
	a.mu.Lock()
	a.calls = append(a.calls, Call{Method: method, Token: token, VMUID: vmUID, Admitted: err == nil})
	a.mu.Unlock()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnauthenticated):
		return status.Error(codes.Unauthenticated, "the request does not authenticate: a valid bearer token is required")
	case errors.Is(err, ErrPermissionDenied):
		return status.Errorf(codes.PermissionDenied, "no Bound, unexpired claim names microvm %q on host %s", vmUID, a.cfg.Node)
	default:
		// EA-014: a lookup that cannot be completed refuses the request.
		return status.Errorf(codes.Unavailable, "the exec agent could not look up the claim: %v", err)
	}
}

// bearer is the bearer token of a call, or empty.
func bearer(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if scheme, token, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "bearer") {
			return token
		}
	}
	return ""
}

// upstreamStatus turns an error of the upstream Host client into a status.
func upstreamStatus(err error) error {
	switch {
	case errors.Is(err, flintlock.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, flintlock.ErrUnimplemented):
		return status.Error(codes.Unimplemented, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	default:
		return status.Error(codes.Unavailable, err.Error())
	}
}

// microVMService is the MicroVM service's ServerInfo and GetMicroVM
// (EA-003); every other call of it is unimplemented.
type microVMService struct {
	mvmv1.UnimplementedMicroVMServer
	a *Agent
}

// ServerInfo answers for the Host's flintlockd to any claim token.
func (m *microVMService) ServerInfo(ctx context.Context, _ *emptypb.Empty) (*mvmv1.ServerInfoResponse, error) {
	if err := m.a.admit(ctx, "ServerInfo", ""); err != nil {
		return nil, err
	}
	info, err := m.a.cfg.Upstream.ServerInfo(ctx)
	if err != nil {
		return nil, upstreamStatus(err)
	}
	return &mvmv1.ServerInfoResponse{
		Version:  &mvmv1.VersionInfo{Version: info.Version, BuildDate: info.BuildDate, CommitHash: info.Commit},
		Uptime:   durationpb.New(info.Uptime),
		Exec:     &mvmv1.GuestAgentServiceInfo{Enabled: info.Exec.Enabled, Address: info.Exec.Address},
		SshProxy: &mvmv1.GuestAgentServiceInfo{Enabled: info.SSHProxy.Enabled, Address: info.SSHProxy.Address},
	}, nil
}

// GetMicroVM answers for a MicroVM the token's claim names.
func (m *microVMService) GetMicroVM(ctx context.Context, req *mvmv1.GetMicroVMRequest) (*mvmv1.GetMicroVMResponse, error) {
	if err := m.a.admit(ctx, "GetMicroVM", req.GetUid()); err != nil {
		return nil, err
	}
	vm, err := m.a.cfg.Upstream.GetMicroVM(ctx, req.GetUid())
	if err != nil {
		return nil, upstreamStatus(err)
	}
	return &mvmv1.GetMicroVMResponse{Microvm: vm}, nil
}

// execService is the MicroVMExec service.
type execService struct {
	execv1.UnimplementedMicroVMExecServer
	a *Agent
}

// ExecCommand admits the exchange by the MicroVM its ExecStart names and
// relays it to the Host's flintlockd message for message. The response
// ends with flintlockd's exit status frame, or with an error status when
// flintlockd's stream ended without one (EA-020).
func (e *execService) ExecCommand(down grpc.BidiStreamingServer[execv1.ExecCommandRequest, execv1.ExecCommandResponse]) error {
	ctx := down.Context()
	first, err := down.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "the exchange ended before its ExecStart")
	}
	if first.GetStart() == nil {
		return status.Error(codes.InvalidArgument, "the first message of an exchange has to be its ExecStart")
	}
	if err := e.a.admit(ctx, "ExecCommand", first.GetStart().GetUid()); err != nil {
		return err
	}

	upCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	up, err := e.a.cfg.Upstream.Exec(upCtx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "flintlockd did not open the exec stream: %v", err)
	}
	if err := up.Send(first); err != nil {
		return status.Errorf(codes.Unavailable, "flintlockd did not take the ExecStart: %v", err)
	}

	// Standard input goes up as it comes, and the half-close with it.
	go func() {
		for {
			req, err := down.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					_ = up.CloseSend()
				}
				return
			}
			if req.GetStart() != nil {
				cancel()
				return
			}
			if err := up.Send(req); err != nil {
				return
			}
		}
	}()

	for {
		resp, err := up.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return status.Error(codes.Unavailable, "flintlockd ended the exec stream before the command's exit status")
			}
			if ctx.Err() != nil {
				return status.FromContextError(ctx.Err()).Err()
			}
			return status.Errorf(codes.Unavailable, "the exec stream to flintlockd failed before the command's exit status: %v", err)
		}
		if err := down.Send(resp); err != nil {
			return err
		}
		if _, ok := resp.GetPayload().(*execv1.ExecCommandResponse_ExitCode); ok {
			return nil
		}
	}
}
