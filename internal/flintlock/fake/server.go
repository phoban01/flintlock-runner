package fake

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// gracefulStopTimeout bounds how long Serve lets in-flight RPCs finish after
// every exec process has been killed before it tears the connections down.
const gracefulStopTimeout = 5 * time.Second

//= docs/requirements/10-test-doubles.md#fake-host
//# The project SHALL provide a fake Host that serves the flintlock
//# `MicroVM` service, including `ServerInfo`, and the `MicroVMExec` service
//# over gRPC using the generated server stubs from the flintlock `api`
//# module.

// Serve listens on cfg.Listen and serves the MicroVM and MicroVMExec
// services (TD-020) with basic auth and TLS when configured (TD-024) until
// ctx is cancelled or the Host is closed. Ready is closed once the listener
// is bound. On the way out it closes the Host, which kills every exec
// process, then stops the server; it returns nil after a clean shutdown and
// the bind or TLS error otherwise. It can be called once.
func (h *Host) Serve(ctx context.Context) error {
	creds, err := h.serverCredentials()
	if err != nil {
		return err
	}
	lis, err := h.listen()
	if err != nil {
		return err
	}

	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.ChainUnaryInterceptor(h.unaryInterceptor),
		grpc.ChainStreamInterceptor(h.streamInterceptor),
		// The Runner keeps one connection per Host with keepalive on
		// (HO-002); permit its pings so that they are not answered with
		// GOAWAY too_many_pings.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             time.Second,
			PermitWithoutStream: true,
		}),
	)
	mvmv1.RegisterMicroVMServer(srv, &microVMService{h: h})
	execv1.RegisterMicroVMExecServer(srv, &execService{h: h})

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	select {
	case <-ctx.Done():
	case <-h.ctx.Done():
	case err := <-serveErr:
		_ = h.Close()
		return fmt.Errorf("fake host %s: serve: %w", h.cfg.Name, err)
	}

	// Kill the processes first so that in-flight ExecCommand handlers return
	// and GracefulStop has nothing to wait for.
	_ = h.Close()
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(gracefulStopTimeout):
		srv.Stop()
		<-stopped
	}
	<-serveErr
	h.mu.Lock()
	h.mu.Unlock()
	return nil
}

//= docs/requirements/10-test-doubles.md#fake-host
//# The fake Host SHALL enforce basic auth and TLS when configured so that
//# the Runner's authentication code is exercised.

// serverCredentials builds the transport credentials from cfg.TLS (TD-024):
// plaintext when unset, otherwise the server certificate, and client
// certificate verification against ClientCAFile when that is set too.
func (h *Host) serverCredentials() (credentials.TransportCredentials, error) {
	if h.cfg.TLS == nil {
		return insecure.NewCredentials(), nil
	}
	cert, err := tls.LoadX509KeyPair(h.cfg.TLS.CertFile, h.cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("fake host %s: loading server certificate: %w", h.cfg.Name, err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if h.cfg.TLS.ClientCAFile != "" {
		pem, err := os.ReadFile(h.cfg.TLS.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("fake host %s: reading client CA: %w", h.cfg.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("fake host %s: no certificates in client CA file %s", h.cfg.Name, h.cfg.TLS.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(tlsCfg), nil
}

// unaryInterceptor applies the Unresponsive fault and basic auth to unary
// RPCs, in that order: a Host that has stopped answering does not get as
// far as checking credentials.
func (h *Host) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// streamInterceptor is unaryInterceptor for streams.
func (h *Host) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := h.admit(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

// admit blocks while the Host is Unresponsive (TD-025) and then checks
// basic auth (TD-024). It returns status errors.
func (h *Host) admit(ctx context.Context) error {
	if err := h.awaitResponsive(ctx); err != nil {
		if errors.Is(err, errClosed) {
			return errClosedStatus()
		}
		return status.FromContextError(err).Err()
	}
	return h.authenticate(ctx)
}

// authenticate enforces basic auth when a token is configured (TD-024,
// HO-004). It accepts exactly what flintlockd's BasicAuthFunc accepts: an
// `authorization` header whose scheme is `basic` (case-insensitive) and
// whose credential is the base64 of the configured token.
func (h *Host) authenticate(ctx context.Context) error {
	if h.cfg.Token == "" {
		return nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "Request unauthenticated with basic")
	}
	scheme, credential, ok := strings.Cut(values[0], " ")
	if !ok {
		return status.Error(codes.Unauthenticated, "Bad authorization string")
	}
	if !strings.EqualFold(scheme, "basic") {
		return status.Error(codes.Unauthenticated, "Request unauthenticated with basic")
	}
	if credential != base64.StdEncoding.EncodeToString([]byte(h.cfg.Token)) {
		return status.Error(codes.Unauthenticated, "invalid auth token: failed basic authentication. Check the token supplied")
	}
	return nil
}

// microVMService serves microvm.services.api.v1alpha1.MicroVM (TD-020).
type microVMService struct {
	mvmv1.UnimplementedMicroVMServer
	h *Host
}

// CreateMicroVM implements MicroVMServer.
func (s *microVMService) CreateMicroVM(_ context.Context, req *mvmv1.CreateMicroVMRequest) (*mvmv1.CreateMicroVMResponse, error) {
	vm, err := s.h.createMicroVM(req.GetMicrovm())
	if err != nil {
		return nil, err
	}
	return &mvmv1.CreateMicroVMResponse{Microvm: vm}, nil
}

// DeleteMicroVM implements MicroVMServer.
func (s *microVMService) DeleteMicroVM(_ context.Context, req *mvmv1.DeleteMicroVMRequest) (*emptypb.Empty, error) {
	if err := s.h.deleteMicroVM(req.GetUid()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// GetMicroVM implements MicroVMServer.
func (s *microVMService) GetMicroVM(_ context.Context, req *mvmv1.GetMicroVMRequest) (*mvmv1.GetMicroVMResponse, error) {
	vm, err := s.h.getMicroVM(req.GetUid())
	if err != nil {
		return nil, err
	}
	return &mvmv1.GetMicroVMResponse{Microvm: vm}, nil
}

// ListMicroVMs implements MicroVMServer.
func (s *microVMService) ListMicroVMs(_ context.Context, req *mvmv1.ListMicroVMsRequest) (*mvmv1.ListMicroVMsResponse, error) {
	vms, err := s.h.listMicroVMs(req.GetNamespace(), req.Name)
	if err != nil {
		return nil, err
	}
	return &mvmv1.ListMicroVMsResponse{Microvm: vms}, nil
}

// ListMicroVMsStream implements MicroVMServer.
func (s *microVMService) ListMicroVMsStream(req *mvmv1.ListMicroVMsRequest, stream grpc.ServerStreamingServer[mvmv1.ListMessage]) error {
	vms, err := s.h.listMicroVMs(req.GetNamespace(), req.Name)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if err := stream.Send(&mvmv1.ListMessage{Microvm: vm}); err != nil {
			return err
		}
	}
	return nil
}

// ServerInfo implements MicroVMServer (TD-026).
func (s *microVMService) ServerInfo(context.Context, *emptypb.Empty) (*mvmv1.ServerInfoResponse, error) {
	return s.h.serverInfo()
}

// execService serves microvmexec.services.api.v1alpha1.MicroVMExec (TD-020).
type execService struct {
	execv1.UnimplementedMicroVMExecServer
	h *Host
}

// ExecCommand implements MicroVMExecServer.
func (s *execService) ExecCommand(stream grpc.BidiStreamingServer[execv1.ExecCommandRequest, execv1.ExecCommandResponse]) error {
	return s.h.execCommand(stream)
}

// statusToSentinel maps a status error from the store onto the flintlock
// sentinels for the in-process Client, so that its callers see exactly
// what they would see from the real client over gRPC.
func statusToSentinel(host string, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("fake host %s: %w", host, err)
	}
	var sentinel error
	switch st.Code() {
	case codes.NotFound:
		sentinel = flintlock.ErrNotFound
	case codes.Unimplemented:
		sentinel = flintlock.ErrUnimplemented
	case codes.Unauthenticated:
		sentinel = flintlock.ErrUnauthenticated
	case codes.Unavailable:
		sentinel = flintlock.ErrUnavailable
	case codes.DeadlineExceeded:
		sentinel = errors.Join(flintlock.ErrUnavailable, context.DeadlineExceeded)
	case codes.Canceled:
		sentinel = errors.Join(flintlock.ErrUnavailable, context.Canceled)
	default:
		return fmt.Errorf("fake host %s: %s: %w", host, st.Code(), err)
	}
	return fmt.Errorf("fake host %s: %s: %w", host, st.Message(), sentinel)
}

// Compile-time checks that the services implement the generated servers.
var (
	_ mvmv1.MicroVMServer      = (*microVMService)(nil)
	_ execv1.MicroVMExecServer = (*execService)(nil)
)
