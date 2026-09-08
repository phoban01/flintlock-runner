package transport

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// Sentinel errors. Implementations wrap them so errors.Is works.
var (
	// ErrStreamFailed is returned by Run when the stream fails before the
	// exit status is known: an error payload (EX-045), a dropped stream or a
	// Host that stopped answering (EX-023, EX-051). The Executor turns it
	// into a system error and never re-runs the Stage.
	ErrStreamFailed = errors.New("transport: stream failed before exit status")
	// ErrNotReady is returned by Ready while the guest cannot yet execute a
	// trivial command (EX-041). The Executor keeps polling until the ready
	// timeout (EX-014).
	ErrNotReady = errors.New("transport: guest not ready")
	// ErrServiceDisabled is returned by Factory.New when the Host reports
	// the required guest-agent service disabled (HO-012).
	ErrServiceDisabled = errors.New("transport: guest-agent service disabled on host")
)

// Kind names a Guest Transport implementation (EX-042).
type Kind string

// The Guest Transport implementations. Exec is the default.
const (
	KindExec Kind = "exec"
	KindSSH  Kind = "ssh"
)

// Command is one operation of the Guest Transport (EX-040). Every field the
// requirement lists is explicit here rather than derived, so that a fake can
// record exactly what the Executor asked for.
type Command struct {
	// Path is the program to run; for a Stage it is the Profile's shell
	// (EX-020).
	Path string
	// Args are the program arguments.
	Args []string
	// Dir is the working directory; for a Stage the Profile's builds
	// directory (EX-025).
	Dir string
	// Env is added to the guest's environment. Job-specific values are only
	// ever delivered inside Stdin, never here (EX-027, SE-011).
	Env map[string]string
	// User is the guest user to run as (EX-026).
	User string
	// Stdin is streamed to the process; for a Stage it holds the script
	// (EX-020, EX-044). Nil means no stdin.
	Stdin io.Reader
	// Stdout and Stderr receive output as it is produced (EX-021). Nil
	// discards.
	Stdout io.Writer
	Stderr io.Writer
}

// Transport runs commands in one MicroVM. A value is bound to one MicroVM on
// one Host for the life of one Job. Implementations are safe for sequential
// use; the Executor never runs two Stages at once.
type Transport interface {
	// Ready succeeds only when a trivial command can be executed in the
	// guest (EX-041); until then it returns ErrNotReady or the underlying
	// failure.
	Ready(ctx context.Context) error

	// Run executes cmd and returns its exit status (EX-040). A non-zero
	// status with a nil error is a script failure (EX-022). A non-nil
	// error means the status is unknown; it wraps ErrStreamFailed for
	// stream failures (EX-023). When ctx is cancelled the process is
	// terminated and Run returns within the graceful kill timeout (EX-024);
	// the remaining time on ctx also bounds the guest-side timeout
	// (EX-046).
	Run(ctx context.Context, cmd Command) (exitStatus int, err error)

	// Close releases any per-Job state. It never closes the Host's gRPC
	// connection, which the HostClient owns (EX-050).
	Close() error
}

// SSHMode selects how the ssh transport reaches the guest.
type SSHMode string

// SSH modes: the flintlock proxy RPC (EX-047) or direct TCP (EX-048).
const (
	SSHModeProxy  SSHMode = "proxy"
	SSHModeDirect SSHMode = "direct"
)

// SSHOptions configure the ssh transport (EX-047 to EX-049).
type SSHOptions struct {
	Mode SSHMode
	// User is the SSH login user.
	User string
	// PrivateKey is the PEM private key (EX-049). It is loaded from the
	// Profile's file by the caller so that the transport never touches the
	// filesystem.
	PrivateKey []byte
	// KnownHostKey is the guest's host public key in authorized_keys form;
	// empty disables host key verification (EX-049).
	KnownHostKey string
	// Address is host:port for SSHModeDirect (EX-048). The caller resolves
	// it from the MicroVM's first network interface; the transport does not
	// know how to map a MAC to an address.
	Address string
}

// Target is everything needed to build a Transport for one Job's MicroVM.
type Target struct {
	// Kind selects the implementation (EX-042).
	Kind Kind
	// Host is the client of the Host that runs the MicroVM (EX-043, EX-047,
	// SE-053). Its connection is reused (EX-050).
	Host flintlock.HostClient
	// VMUID is the flintlock uid sent in ExecStart or the SSHProxy uid
	// message.
	VMUID string
	// Deadline bounds an in-flight operation once the Host stops answering
	// (EX-051).
	Deadline time.Duration
	// SSH is read only for KindSSH.
	SSH SSHOptions
}

// Factory builds Transports. The production Factory switches on Target.Kind
// and consults the Host's ServerInfo for HO-012; tests inject one that
// returns a fake.
type Factory interface {
	// New builds a Transport for the target. It does not contact the guest;
	// Ready does that. It returns ErrServiceDisabled when the Host has the
	// required guest-agent service disabled (HO-012).
	New(ctx context.Context, target Target) (Transport, error)
}

// Test-double shapes shared by every package that fakes a Transport.

// Recorded is one Command as seen by a recording fake Transport, with the
// stdin fully read, so that Executor tests assert on the exact script,
// directory, user and environment of every Stage (EX-020, EX-025 to EX-027,
// EX-060 to EX-066).
type Recorded struct {
	Command
	Stdin []byte
}

// Scripted is one response a scripted fake Transport gives to Run, in order.
// A non-nil Err is returned as is; otherwise Stdout and Stderr are written to
// the command's writers and ExitStatus returned. Executor tests use it to
// produce script failures (EX-022) and stream failures (EX-023) without a
// Host.
type Scripted struct {
	Stdout     []byte
	Stderr     []byte
	ExitStatus int
	Err        error
	// Delay, when set, makes Run block that long or until ctx is cancelled,
	// for cancellation tests (EX-024).
	Delay time.Duration
}
