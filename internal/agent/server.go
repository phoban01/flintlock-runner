package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// server is the exec API: flintlock's own MicroVMExec service, and the two
// calls of its MicroVM service that the Runner's exec Guest Transport makes
// besides (ServerInfo when it builds a transport, HO-012, and GetMicroVM
// as its liveness probe, EX-051), in front of the local flintlockd. Every
// call is authenticated by the interceptors (KF-173); every call that names
// a MicroVM is then authorized against the claims (KF-174). The other
// calls of the MicroVM service, create, delete and list above all, are
// not offered: the Runner makes none of them, and a MicroVM is battery's
// to create and delete.
type server struct {
	hostNode    string
	fl          *Flintlockd
	authn       *authenticator
	claims      ClaimLookup
	clk         clock.Clock
	log         *slog.Logger
	openTimeout time.Duration
	callTimeout time.Duration
}

// userKey carries the authenticated user name from the interceptor.
type userKey struct{}

// userOf is the user the interceptor authenticated.
func userOf(ctx context.Context) string {
	user, _ := ctx.Value(userKey{}).(string)
	return user
}

// register adds the exec API's services to srv, which is built with the
// interceptors below.
func (s *server) register(srv *grpc.Server) {
	mvmv1.RegisterMicroVMServer(srv, &microVMService{s: s})
	execv1.RegisterMicroVMExecServer(srv, &execService{s: s})
}

// unaryInterceptor authenticates every unary call before its handler runs.
func (s *server) unaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	user, err := s.authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return handler(context.WithValue(ctx, userKey{}, user), req)
}

// streamInterceptor authenticates every stream before its handler runs.
func (s *server) streamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	user, err := s.authenticate(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return handler(srv, &userStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), userKey{}, user)})
}

// userStream is a ServerStream whose context carries the user.
type userStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context implements grpc.ServerStream.
func (u *userStream) Context() context.Context { return u.ctx }

// authenticate turns the outcome of the TokenReview into the status the
// caller sees: unauthenticated for a request that is not, unavailable for a
// review that could not be made. Both are refusals (KF-173, KF-175).
func (s *server) authenticate(ctx context.Context, method string) (string, error) {
	user, err := s.authn.authenticate(ctx)
	var review *reviewError
	switch {
	case err == nil:
		return user, nil
	case errors.As(err, &review):
		s.log.Warn("refused a request: its token could not be reviewed", "method", method, "error", err)
		return "", status.Error(codes.Unavailable, "the exec agent could not review the bearer token; the request was refused")
	default:
		s.log.Info("refused an unauthenticated request", "method", method, "error", err)
		return "", status.Error(codes.Unauthenticated, "the request does not authenticate: a valid bearer token is required")
	}
}

// authorize decides whether the authenticated user may use the MicroVM
// (KF-174), and turns the outcome into the status the caller sees:
// permission denied for a refusal, unavailable for a lookup that could not
// be made (KF-175). What exactly failed is logged, not told.
func (s *server) authorize(ctx context.Context, method, vmUID string) error {
	user := userOf(ctx)
	lookupCtx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	err := authorizeClaims(lookupCtx, s.claims, user, vmUID, s.hostNode, s.clk.Now())
	var lookup *lookupError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &lookup):
		s.log.Warn("refused a request: its claims could not be looked up", "method", method, "user", user, "vm", vmUID, "error", err)
		return status.Error(codes.Unavailable, "the exec agent could not look up the claims; the request was refused")
	default:
		s.log.Info("refused a request without a Bound claim", "method", method, "user", user, "vm", vmUID, "reason", err)
		return status.Errorf(codes.PermissionDenied, "no Bound, unexpired claim of %s names microvm %q on host %s", user, vmUID, s.hostNode)
	}
}

// microVMService is the part of the MicroVM service the exec Guest
// Transport uses.
type microVMService struct {
	mvmv1.UnimplementedMicroVMServer
	s *server
}

// ServerInfo relays flintlockd's answer to an authenticated caller. It
// names no MicroVM, so there is no claim to check; it tells the caller
// whether exec is enabled, which is what the transport asks it for.
func (m *microVMService) ServerInfo(ctx context.Context, _ *emptypb.Empty) (*mvmv1.ServerInfoResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, m.s.callTimeout)
	defer cancel()
	return m.s.fl.vms.ServerInfo(ctx, &emptypb.Empty{})
}

// GetMicroVM relays flintlockd's answer for a MicroVM the caller holds a
// Bound claim on, which is the transport's liveness probe (EX-051).
func (m *microVMService) GetMicroVM(ctx context.Context, req *mvmv1.GetMicroVMRequest) (*mvmv1.GetMicroVMResponse, error) {
	if err := m.s.authorize(ctx, "GetMicroVM", req.GetUid()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.s.callTimeout)
	defer cancel()
	return m.s.fl.vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: req.GetUid()})
}

// execService is the MicroVMExec service.
type execService struct {
	execv1.UnimplementedMicroVMExecServer
	s *server
}

// ExecCommand authorizes the exchange by the MicroVM its ExecStart names,
// then relays it to flintlockd. Nothing reaches flintlockd before the claim
// check has passed.
func (e *execService) ExecCommand(stream grpc.BidiStreamingServer[execv1.ExecCommandRequest, execv1.ExecCommandResponse]) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "the exchange ended before its ExecStart")
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "the first message of an exchange has to be its ExecStart")
	}
	if err := e.s.authorize(ctx, "ExecCommand", start.GetUid()); err != nil {
		return err
	}
	return e.s.relayExec(ctx, stream, first)
}

// execStream is the client side of an exchange with flintlockd.
type execStream = grpc.BidiStreamingClient[execv1.ExecCommandRequest, execv1.ExecCommandResponse]

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# The Exec Agent SHALL relay a request's streams to
//# `MicroVMExec.ExecCommand` on the local `flintlockd` and SHALL end every
//# response with an exit status frame, sent only after `flintlockd` has
//# reported the command's exit.

// relayExec relays one authorized exchange to flintlockd, message for
// message in both directions, and decides how the response ends.
//
// The exit status frame is flintlockd's own exit_code message. It is
// relayed when, and only when, flintlockd sends it, and the response then
// ends with an OK status. Every other end -- flintlockd closing its stream
// without an exit_code, the stream to it failing, the agent shutting down
// -- ends the response with an error status and no exit_code, so that the
// caller sees a stream failure (EX-023) and never a clean end it could
// mistake for a finished command. The caller's own cancellation cancels
// the stream to flintlockd, which is what stops the command in the guest
// (EX-024).
func (s *server) relayExec(ctx context.Context, down grpc.BidiStreamingServer[execv1.ExecCommandRequest, execv1.ExecCommandResponse], first *execv1.ExecCommandRequest) error {
	upCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	up, err := s.openExec(upCtx, cancel, first)
	if err != nil {
		s.log.Warn("flintlockd did not open an exec stream", "user", userOf(ctx), "vm", first.GetStart().GetUid(), "error", err)
		return status.Errorf(codes.Unavailable, "flintlockd did not open the exec stream: %v", err)
	}

	// The caller's messages go to flintlockd as they come. The end of the
	// caller's input is passed on as a half-close; a caller that goes away
	// cancels the exchange. A failed send to flintlockd shows up on the
	// receiving side, where it is reported.
	//
	// Only standard input follows the ExecStart. The claim was checked for
	// the MicroVM the first message named, so a second ExecStart, naming that
	// MicroVM or another, would ask flintlockd for something no one
	// authorized; it is never passed on, and it ends the exchange.
	var secondStart atomic.Bool
	go func() {
		for {
			req, err := down.Recv()
			if errors.Is(err, io.EOF) {
				_ = up.CloseSend()
				return
			}
			if err != nil {
				cancel()
				return
			}
			if req.GetStart() != nil {
				secondStart.Store(true)
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
			if secondStart.Load() {
				s.log.Warn("refused an exchange that sent a second ExecStart", "user", userOf(ctx), "vm", first.GetStart().GetUid())
				return status.Error(codes.InvalidArgument, "an exchange carries one ExecStart, first; a second one ends it")
			}
			if ctx.Err() != nil {
				return status.FromContextError(ctx.Err()).Err()
			}
			if errors.Is(err, io.EOF) {
				s.log.Warn("flintlockd ended an exec stream without an exit status", "vm", first.GetStart().GetUid())
				return status.Error(codes.Unavailable, "flintlockd ended the exec stream before the command's exit status")
			}
			s.log.Warn("the exec stream to flintlockd failed before the exit status", "vm", first.GetStart().GetUid(), "error", err)
			// flintlockd's own code is kept where it says why, a deadline
			// or a cancellation above all, so that the caller sees the
			// same failure it would see from flintlockd itself; it is
			// never OK.
			code := status.Code(err)
			if code == codes.OK || code == codes.Unknown {
				code = codes.Unavailable
			}
			return status.Errorf(code, "the exec stream to flintlockd failed before the command's exit status: %s", status.Convert(err).Message())
		}
		if err := down.Send(resp); err != nil {
			return err
		}
		if _, ok := resp.GetPayload().(*execv1.ExecCommandResponse_ExitCode); ok {
			return nil
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# If `flintlockd` has not answered for the requested MicroVM and
//# accepted the request's exec stream within the configured deadline, then
//# the Exec Agent SHALL end the response as a stream failure.

// openExec opens the exchange with flintlockd within the configured
// deadline, and counts it open once flintlockd has answered for the
// MicroVM, the stream exists and the ExecStart is on it. The GetMicroVM
// comes first because a stream is created on the client's side alone: a
// flintlockd that accepts connections and answers nothing would take a
// stream and an ExecStart without a word, and the exchange would then wait
// for output that never comes. A call that has to be answered is what
// shows that flintlockd is there. Whatever has not happened by the
// deadline is abandoned by cancelling the exchange, which the stream lives
// under, so that nothing is left waiting behind the failure.
func (s *server) openExec(ctx context.Context, cancel context.CancelFunc, first *execv1.ExecCommandRequest) (execStream, error) {
	type opened struct {
		stream execStream
		err    error
	}
	done := make(chan opened, 1)
	go func() {
		probeCtx, probeCancel := context.WithCancel(ctx)
		defer probeCancel()
		if _, err := s.fl.vms.GetMicroVM(probeCtx, &mvmv1.GetMicroVMRequest{Uid: first.GetStart().GetUid()}); err != nil {
			done <- opened{err: fmt.Errorf("asking flintlockd for the microvm: %s", status.Convert(err).Message())}
			return
		}
		stream, err := s.fl.exec.ExecCommand(ctx)
		if err != nil {
			done <- opened{err: fmt.Errorf("opening the exec stream: %s", status.Convert(err).Message())}
			return
		}
		if err := stream.Send(first); err != nil {
			done <- opened{err: fmt.Errorf("sending the exec start: %w", err)}
			return
		}
		done <- opened{stream: stream}
	}()

	timer := s.clk.NewTimer(s.openTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.stream, r.err
	case <-timer.C():
		cancel()
		return nil, fmt.Errorf("not open within the deadline of %s", s.openTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
