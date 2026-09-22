package kubelet

import (
	"context"
	"errors"
	"fmt"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// localFlintlockd is the Pod Provider's client for the flintlockd on its own
// Host. Reading and exec go through the project's Host client, so that
// keepalive, reconnection and error mapping are the ones every other
// component has; creating and deleting, which that client deliberately
// cannot do (HO-007), go through a MicroVM client of the provider's own on a
// second connection to the same local endpoint.
type localFlintlockd struct {
	flintlock.HostClient
	conn *grpc.ClientConn
	vms  mvmv1.MicroVMClient
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//# The Pod Provider SHALL reach `flintlockd` only through the
//# local endpoint of HI-042.

// DialLocal connects to flintlockd at a local endpoint and refuses any other
// (KF-018). The connection is plaintext and carries no token: the endpoint
// is a unix socket or a loopback address that nothing outside the Host can
// reach (HI-042, HI-044), which is the only reason that is acceptable, and
// why the check is made here as well as in the configuration.
func DialLocal(ctx context.Context, hostName, endpoint string) (flintlock.PoolHostClient, error) {
	if err := ValidateLocalEndpoint(endpoint); err != nil {
		return nil, fmt.Errorf("kubelet: flintlockd endpoint %w", err)
	}
	ep := flintlock.Endpoint{
		Name:    hostName,
		Address: endpoint,
		TLS:     flintlock.TLSOptions{Insecure: true},
	}
	reader, err := flintlock.NewDialer().Dial(ctx, ep)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("kubelet: dialling flintlockd at %s: %w", endpoint, err)
	}
	return &localFlintlockd{HostClient: reader, conn: conn, vms: mvmv1.NewMicroVMClient(conn)}, nil
}

// CreateMicroVM implements flintlock.HostAdminClient.
func (l *localFlintlockd) CreateMicroVM(ctx context.Context, spec *types.MicroVMSpec) (*types.MicroVM, error) {
	resp, err := l.vms.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{Microvm: spec})
	if err != nil {
		return nil, mapStatus(err)
	}
	return resp.GetMicrovm(), nil
}

// DeleteMicroVM implements flintlock.HostAdminClient.
func (l *localFlintlockd) DeleteMicroVM(ctx context.Context, uid string) error {
	_, err := l.vms.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid})
	return mapStatus(err)
}

// Close implements flintlock.HostClient for both connections.
func (l *localFlintlockd) Close() error {
	return errors.Join(l.HostClient.Close(), l.conn.Close())
}

// mapStatus maps the status codes a caller has to tell apart onto the
// flintlock sentinels, as the Host client does for its own calls.
func mapStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("flintlock: %w", err)
	}
	switch st.Code() {
	case codes.NotFound:
		return fmt.Errorf("%w: %s", flintlock.ErrNotFound, st.Message())
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %s", flintlock.ErrUnavailable, st.Message())
	default:
		return fmt.Errorf("flintlock: %s: %s", st.Code(), st.Message())
	}
}

var _ flintlock.PoolHostClient = (*localFlintlockd)(nil)
