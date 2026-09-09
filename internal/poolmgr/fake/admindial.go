package fake

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	microvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// AdminDialer is the pool-manager-side flintlock.AdminDialer over gRPC: it
// connects the fake Pool Manager to real flintlockd instances so the
// standalone binary (TD-009) can run a fleet (TD-002). It speaks the
// flintlock MicroVM and MicroVMExec services with the generated clients,
// sends the basic auth token, and verifies TLS unless the Endpoint is
// explicitly insecure. The Runner side never uses it; its own Host client
// lives in package flintlock.
type AdminDialer struct{}

// DialAdmin implements flintlock.AdminDialer. The connection is established
// lazily, so an unreachable Host fails on first use rather than here.
func (AdminDialer) DialAdmin(_ context.Context, ep flintlock.Endpoint) (flintlock.PoolHostClient, error) {
	creds, err := transportCredentials(ep.TLS)
	if err != nil {
		return nil, fmt.Errorf("host %q: %w", ep.Name, err)
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	}
	if ep.Token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(basicToken{
			header: "Basic " + base64.StdEncoding.EncodeToString([]byte(ep.Token)),
			secure: !ep.TLS.Insecure,
		}))
	}
	conn, err := grpc.NewClient(ep.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("host %q: dial %s: %w", ep.Name, ep.Address, err)
	}
	return &grpcHost{name: ep.Name, conn: conn, vm: microvmv1.NewMicroVMClient(conn), exec: execv1.NewMicroVMExecClient(conn)}, nil
}

// transportCredentials builds the client TLS from a flintlock.TLSOptions.
func transportCredentials(o flintlock.TLSOptions) (credentials.TransportCredentials, error) {
	if o.Insecure {
		return insecure.NewCredentials(), nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca file %s: no certificates found", o.CAFile)
		}
		cfg.RootCAs = pool
	}
	if o.CertFile != "" || o.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(cfg), nil
}

// basicToken sends the flintlockd basic auth header on every call.
type basicToken struct {
	header string
	secure bool
}

func (b basicToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": b.header}, nil
}

func (b basicToken) RequireTransportSecurity() bool { return b.secure }

// grpcHost is a flintlock.PoolHostClient over one connection to flintlockd.
type grpcHost struct {
	name string
	conn *grpc.ClientConn
	vm   microvmv1.MicroVMClient
	exec execv1.MicroVMExecClient
}

// Name implements flintlock.HostClient.
func (h *grpcHost) Name() string { return h.name }

// ServerInfo implements flintlock.HostClient.
func (h *grpcHost) ServerInfo(ctx context.Context) (*flintlock.HostInfo, error) {
	resp, err := h.vm.ServerInfo(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, hostErr(err)
	}
	return &flintlock.HostInfo{
		Name:         h.name,
		VersionKnown: true,
		Version:      resp.GetVersion().GetVersion(),
		BuildDate:    resp.GetVersion().GetBuildDate(),
		Commit:       resp.GetVersion().GetCommitHash(),
		Uptime:       resp.GetUptime().AsDuration(),
		Exec:         flintlock.GuestService{Enabled: resp.GetExec().GetEnabled(), Address: resp.GetExec().GetAddress()},
		SSHProxy:     flintlock.GuestService{Enabled: resp.GetSshProxy().GetEnabled(), Address: resp.GetSshProxy().GetAddress()},
	}, nil
}

// GetMicroVM implements flintlock.HostClient.
func (h *grpcHost) GetMicroVM(ctx context.Context, uid string) (*types.MicroVM, error) {
	resp, err := h.vm.GetMicroVM(ctx, &microvmv1.GetMicroVMRequest{Uid: uid})
	if err != nil {
		return nil, hostErr(err)
	}
	return resp.GetMicrovm(), nil
}

// ListMicroVMs implements flintlock.HostClient.
func (h *grpcHost) ListMicroVMs(ctx context.Context, namespace string) ([]*types.MicroVM, error) {
	resp, err := h.vm.ListMicroVMs(ctx, &microvmv1.ListMicroVMsRequest{Namespace: namespace})
	if err != nil {
		return nil, hostErr(err)
	}
	return resp.GetMicrovm(), nil
}

// Exec implements flintlock.HostClient.
func (h *grpcHost) Exec(ctx context.Context) (flintlock.ExecStream, error) {
	stream, err := h.exec.ExecCommand(ctx)
	if err != nil {
		return nil, hostErr(err)
	}
	return stream, nil
}

// SSHProxy implements flintlock.HostClient. A pool manager never proxies
// SSH, so this dialer does not carry the sshproxy client.
func (h *grpcHost) SSHProxy(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("%w: ssh proxy is not offered by the pool manager's host dialer", errors.ErrUnsupported)
}

// Close implements flintlock.HostClient.
func (h *grpcHost) Close() error { return h.conn.Close() }

// CreateMicroVM implements flintlock.HostAdminClient.
func (h *grpcHost) CreateMicroVM(ctx context.Context, spec *types.MicroVMSpec) (*types.MicroVM, error) {
	resp, err := h.vm.CreateMicroVM(ctx, &microvmv1.CreateMicroVMRequest{Microvm: spec})
	if err != nil {
		return nil, hostErr(err)
	}
	return resp.GetMicrovm(), nil
}

// DeleteMicroVM implements flintlock.HostAdminClient.
func (h *grpcHost) DeleteMicroVM(ctx context.Context, uid string) error {
	_, err := h.vm.DeleteMicroVM(ctx, &microvmv1.DeleteMicroVMRequest{Uid: uid})
	return hostErr(err)
}

// hostErr maps flintlockd status codes onto the flintlock sentinel errors.
func hostErr(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("flintlock: %w", err)
	}
	var sentinel error
	switch st.Code() {
	case codes.NotFound:
		sentinel = flintlock.ErrNotFound
	case codes.Unimplemented:
		sentinel = flintlock.ErrUnimplemented
	case codes.Unavailable:
		sentinel = flintlock.ErrUnavailable
	case codes.Unauthenticated:
		sentinel = flintlock.ErrUnauthenticated
	default:
		return fmt.Errorf("flintlock: %s: %s", st.Code(), st.Message())
	}
	return fmt.Errorf("%w: %s", sentinel, st.Message())
}

// Compile-time interface checks.
var (
	_ flintlock.AdminDialer         = AdminDialer{}
	_ flintlock.PoolHostClient      = (*grpcHost)(nil)
	_ credentials.PerRPCCredentials = basicToken{}
)
