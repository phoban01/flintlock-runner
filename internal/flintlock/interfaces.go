package flintlock

import (
	"context"
	"errors"
	"io"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
)

// Sentinel errors returned by HostClient implementations. Implementations
// wrap them so that errors.Is works; callers never import gRPC codes.
var (
	// ErrNotFound is returned by GetMicroVM when the Host does not have the
	// MicroVM. It is the negative answer in the Placement fan-out (SC-031).
	ErrNotFound = errors.New("flintlock: microvm not found")
	// ErrUnimplemented is returned by ServerInfo on a Host that predates the
	// RPC (HO-013, SC-040, TD-026).
	ErrUnimplemented = errors.New("flintlock: rpc not implemented by host")
	// ErrUnavailable is returned when the Host cannot be reached or the
	// connection was lost (HO-002, EX-051).
	ErrUnavailable = errors.New("flintlock: host unavailable")
	// ErrUnauthenticated is returned when the Host rejects the basic auth
	// token (HO-004, TD-024).
	ErrUnauthenticated = errors.New("flintlock: unauthenticated")
	// ErrUnknownHost is returned by Registry.Get for a name that is not in
	// the Inventory (SC-034).
	ErrUnknownHost = errors.New("flintlock: host not in inventory")
)

// TLSOptions is the client TLS material for one Host (HO-005, HO-006). There
// is no option to skip verification (SE-022).
type TLSOptions struct {
	// CAFile verifies the server certificate. Empty uses the system roots.
	CAFile string
	// CertFile and KeyFile enable mutual TLS when both are set.
	CertFile string
	KeyFile  string
	// Insecure dials plaintext. It has to be explicit (SE-021).
	Insecure bool
}

// Endpoint is how to reach one Host. It is built from a config.HostEntry by
// the caller so that this package does not depend on the configuration
// schema and the fake Host can be dialled from a literal.
type Endpoint struct {
	// Name is the Host name the Pool Manager uses in flintlock_hosts
	// (HO-010, FL-052).
	Name string
	// Address is host:port of flintlockd.
	Address string
	// Token is the basic auth token sent as `Basic <base64>` (HO-004).
	Token string
	// TLS is the client TLS material.
	TLS TLSOptions
}

// GuestService reports whether an optional guest-agent service is enabled on
// a Host, from ServerInfo (HO-011, HO-012).
type GuestService struct {
	Enabled bool
	// Address is populated only when Enabled.
	Address string
}

// HostInfo is the Runner's view of a Host from ServerInfo (HO-011).
type HostInfo struct {
	// Name is the Host name.
	Name string
	// VersionKnown is false when ServerInfo is unimplemented (HO-013); the
	// remaining fields are then zero.
	VersionKnown bool
	Version      string
	BuildDate    string
	Commit       string
	Uptime       time.Duration
	// Exec and SSHProxy report the guest-agent services (HO-012, FL-070).
	Exec     GuestService
	SSHProxy GuestService
}

// ExecStream is one MicroVMExec.ExecCommand exchange (EX-043). It is the
// narrowest surface the exec Guest Transport needs: send ExecStart, stdin
// chunks and stdin_eof (EX-044); receive stdout, stderr, error and exit_code
// (EX-045). The generated grpc.BidiStreamingClient satisfies it (checked
// below at compile time), and a scripted in-memory fake can too, so the
// transport's framing is unit-tested with no gRPC.
type ExecStream interface {
	// Send writes one request message. After CloseSend it returns an error.
	Send(*execv1.ExecCommandRequest) error
	// Recv blocks for the next response. It returns io.EOF after the stream
	// ends normally and a non-EOF error when the stream is dropped before
	// the exit code (EX-023, TD-025).
	Recv() (*execv1.ExecCommandResponse, error)
	// CloseSend half-closes the client side.
	CloseSend() error
}

// The generated bidi client is an ExecStream.
var _ ExecStream = (grpc.BidiStreamingClient[execv1.ExecCommandRequest, execv1.ExecCommandResponse])(nil)

// HostClient is the Runner's connection to one Host (HO-001). One value owns
// one long-lived gRPC connection with keepalive and reconnects with backoff
// (HO-002); every stream it opens shares that connection (EX-050).
//
// It has no CreateMicroVM and no DeleteMicroVM (HO-007). Implementations are
// safe for concurrent use.
type HostClient interface {
	// Name returns the Host name from the Endpoint.
	Name() string

	// ServerInfo calls MicroVM.ServerInfo (HO-011, SC-040). It returns
	// ErrUnimplemented on Hosts without the RPC (HO-013).
	ServerInfo(ctx context.Context) (*HostInfo, error)

	// GetMicroVM calls MicroVM.GetMicroVM by uid (SC-031). It returns
	// ErrNotFound when the Host does not have it.
	GetMicroVM(ctx context.Context, uid string) (*types.MicroVM, error)

	// ListMicroVMs calls MicroVM.ListMicroVMs. The Runner only calls it with
	// its own namespace and only as the health probe fallback (HO-008,
	// SC-040).
	ListMicroVMs(ctx context.Context, namespace string) ([]*types.MicroVM, error)

	// Exec opens a MicroVMExec.ExecCommand stream (EX-043). The stream lives
	// until ctx is cancelled, the exit code is received or the Host stops
	// answering, in which case it fails within the transport deadline
	// (EX-051).
	Exec(ctx context.Context) (ExecStream, error)

	// SSHProxy opens a MicroVMSSHProxy.SSHProxy stream to the guest's sshd
	// and returns it as a byte stream (EX-047). The uid message is sent
	// before it returns.
	SSHProxy(ctx context.Context, uid string) (io.ReadWriteCloser, error)

	// Close releases the connection. In-flight streams fail.
	Close() error
}

// HostAdminClient is the part of the flintlock API that only a pool manager
// uses: it creates and deletes MicroVMs. The Runner holds no value of this
// type (HO-007, PL-044); the fake Pool Manager does (TD-002).
type HostAdminClient interface {
	// CreateMicroVM calls MicroVM.CreateMicroVM and returns the MicroVM with
	// its assigned uid. The MicroVM is PENDING until the Host reports it
	// CREATED (TD-022) or FAILED (TD-025).
	CreateMicroVM(ctx context.Context, spec *types.MicroVMSpec) (*types.MicroVM, error)
	// DeleteMicroVM calls MicroVM.DeleteMicroVM. Deleting an unknown uid
	// returns ErrNotFound.
	DeleteMicroVM(ctx context.Context, uid string) error
}

// PoolHostClient is what a pool manager needs from a Host: everything the
// Runner needs plus create and delete. The fake Pool Manager takes this
// interface so it can be pointed at fake Hosts or at real flintlockd
// instances with the same code (TD-002).
type PoolHostClient interface {
	HostClient
	HostAdminClient
}

// Dialer opens Runner-side connections to Hosts. The gRPC implementation is
// the only one in production; tests inject a Dialer that returns in-memory
// HostClients so the Scheduler's health probing and Placement fan-out run
// without a network.
type Dialer interface {
	// Dial connects to a Host. It returns quickly; the connection is
	// established lazily and reconnects on its own (HO-002).
	Dial(ctx context.Context, ep Endpoint) (HostClient, error)
}

// AdminDialer opens pool-manager-side connections. Only the fake Pool Manager
// (TD-002, TD-009) and the Fleet Controller's verification use it.
type AdminDialer interface {
	DialAdmin(ctx context.Context, ep Endpoint) (PoolHostClient, error)
}

// Registry is the live set of Hosts keyed by Inventory name (HO-010). It is
// what the Scheduler and the Executor look a Placement up in.
type Registry interface {
	// Get returns the client for a named Host or ErrUnknownHost (SC-034).
	// It is a borrow for immediate use: a reload that removes the Host may
	// release the connection under a caller that is still holding one.
	// Anything that holds a client across an operation takes a Lease.
	Get(name string) (HostClient, error)
	// Lease returns the client for a named Host together with a function
	// that releases it, or ErrUnknownHost (SC-034). While a lease is
	// outstanding the connection stays open even if a reload removes the
	// Host, which is how a Job already running there finishes on it
	// (HO-014); the Registry releases the connection when the last lease on
	// a removed Host is released. The release function is safe to call more
	// than once and from any goroutine.
	Lease(name string) (client HostClient, release func(), err error)
	// Endpoint returns the Endpoint a named Host was dialled with, so that
	// the Scheduler can compare a claim's host.address with the Inventory
	// (SC-030) without a second lookup structure.
	Endpoint(name string) (Endpoint, bool)
	// Names lists the Hosts currently in the Registry, sorted.
	Names() []string
	// Apply reconciles the Registry with a new Inventory on reload (HO-014):
	// new Hosts are dialled, removed Hosts are dropped from Names so probing
	// stops, but a removed Host's client stays usable by Jobs that already
	// hold a Lease on it until they finish, and its connection is released
	// when the last of those leases is. It is a no-op for unchanged entries.
	Apply(ctx context.Context, endpoints []Endpoint) error
	// Close closes every client.
	Close() error
}

// Test-double shapes. These are the configuration and control surface of
// the fake Host in package fake; they live here so that the harness and the
// fake share one definition without the harness importing the fake's
// internals.

// ServerTLS is the TLS material a fake Host serves with (TD-024).
type ServerTLS struct {
	CertFile string
	KeyFile  string
	// ClientCAFile, when set, requires client certificates.
	ClientCAFile string
}

// FakeHostConfig is the static configuration of one fake Host (TD-020 to
// TD-026). It is a value type so that the harness and unit tests build it
// from a literal; the fake's constructor lives in package fake.
type FakeHostConfig struct {
	// Name is the Host name reported by the fake and used in flintlock_hosts.
	Name string
	// Listen is the gRPC listen address; ":0" picks a free port.
	Listen string
	// SandboxRoot is the directory under which each MicroVM gets a sandbox
	// directory; ExecCommand runs local processes rooted there (TD-021).
	// The harness checks it is empty after shutdown (TD-054).
	SandboxRoot string
	// BootDelay is how long a MicroVM stays PENDING before CREATED (TD-022).
	BootDelay time.Duration
	// Version, ExecEnabled and SSHProxyEnabled shape ServerInfo (TD-026).
	Version         string
	ExecEnabled     bool
	SSHProxyEnabled bool
	// ServerInfoUnimplemented makes ServerInfo return UNIMPLEMENTED (TD-026)
	// to exercise HO-013 and the ListMicroVMs fallback of SC-040.
	ServerInfoUnimplemented bool
	// Token, when set, is required as basic auth on every call (TD-024).
	Token string
	// TLS, when set, serves TLS (TD-024).
	TLS *ServerTLS
}

// HostFaults are the runtime fault injection switches of a fake Host
// (TD-025). All zero means healthy. They are read on each request so a test
// can flip them mid-Job, which is how "a Host becoming unhealthy during a
// Job" (TD-051, SC-043) is produced.
type HostFaults struct {
	// CreateFails makes every CreateMicroVM end in state FAILED after
	// BootDelay instead of CREATED.
	CreateFails bool
	// DropExecBeforeExit closes every ExecCommand stream after forwarding
	// output but before sending exit_code (EX-023, EX-045).
	DropExecBeforeExit bool
	// Unresponsive makes the Host stop answering: new RPCs block until the
	// caller's deadline and in-flight streams stall (HO-002, EX-051,
	// SC-041). The listener stays open so that the failure is a timeout,
	// not a refused connection.
	Unresponsive bool
}

// HostFaultInjector is implemented by the fake Host so that tests and the
// harness set HostFaults at runtime.
type HostFaultInjector interface {
	SetFaults(HostFaults)
	Faults() HostFaults
}
