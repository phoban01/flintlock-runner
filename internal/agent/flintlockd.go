package agent

import (
	"fmt"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/phoban01/flintlock-runner/internal/hostcheck"
)

// Keepalive of the connection to flintlockd. It is a local connection, so
// the pings are cheap, and they are what notices a flintlockd that died
// with streams open.
const (
	flintlockdKeepaliveTime    = 30 * time.Second
	flintlockdKeepaliveTimeout = 10 * time.Second
)

// Flintlockd is the Exec Agent's connection to the flintlockd on its own
// Host. The agent relays requests to it message for message, so it holds
// the generated clients rather than the Runner's Host client, which maps
// every status onto the Runner's own errors.
type Flintlockd struct {
	conn *grpc.ClientConn
	vms  mvmv1.MicroVMClient
	exec execv1.MicroVMExecClient
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# The Exec Agent SHALL reach `flintlockd` only through the local
//# endpoint of HI-042, and the Fleet Manifests SHALL run it as the user id
//# that HI-063 admits there and run no other container as that user id.

// DialFlintlockd connects to flintlockd at a local endpoint and refuses any
// other. The connection is plaintext and carries no token: the endpoint is
// a unix socket or a loopback address that nothing outside the Host can
// reach (HI-042, HI-044) and that the Host Image admits only the Exec
// Agent's user id to (HI-063), which is the only reason that is acceptable,
// and why the check is made here as well as in the configuration.
func DialFlintlockd(endpoint string) (*Flintlockd, error) {
	if err := hostcheck.ValidateLocalEndpoint(endpoint); err != nil {
		return nil, fmt.Errorf("agent: flintlockd endpoint %w", err)
	}
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                flintlockdKeepaliveTime,
			Timeout:             flintlockdKeepaliveTimeout,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("agent: dialling flintlockd at %s: %w", endpoint, err)
	}
	return &Flintlockd{conn: conn, vms: mvmv1.NewMicroVMClient(conn), exec: execv1.NewMicroVMExecClient(conn)}, nil
}

// Close closes the connection; streams still open on it fail.
func (f *Flintlockd) Close() error { return f.conn.Close() }
