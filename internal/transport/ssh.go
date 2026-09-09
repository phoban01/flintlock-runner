package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// sshProxyAddr is the address the SSH client is told it is talking to. The
// byte stream comes from the Host's SSHProxy RPC and has no address of its
// own, and the name only ever appears in SSH's own error messages, so it
// names the MicroVM rather than pretending to be a network endpoint.
const sshProxyAddr = "microvm"

// sshHandshakeGrace bounds how long Close waits for a handshake that is
// still running when the transport is closed.
const sshHandshakeGrace = 5 * time.Second

// sshTransport is the ssh Guest Transport. One value holds one SSH
// connection to one guest for the life of a Job: the connection is
// established on the first operation and reused by the ones that follow,
// and it travels over the Host client's gRPC connection (EX-050).
type sshTransport struct {
	target Target
	clk    clock.Clock
	log    *slog.Logger

	// connCtx is the life of the SSH connection, which outlives any single
	// operation; Close ends it.
	connCtx    context.Context
	connCancel context.CancelFunc

	mu     sync.Mutex
	conn   io.ReadWriteCloser
	client *ssh.Client
	closed bool
}

// newSSHTransport builds an ssh transport for a target. Nothing is
// contacted here; the first operation dials.
func newSSHTransport(target Target, clk clock.Clock, log *slog.Logger) (*sshTransport, error) {
	if len(target.SSH.PrivateKey) == 0 {
		return nil, fmt.Errorf("host %s: microvm %s: the ssh transport needs the profile's private key",
			target.Host.Name(), target.VMUID)
	}
	if _, err := ssh.ParsePrivateKey(target.SSH.PrivateKey); err != nil {
		return nil, fmt.Errorf("host %s: microvm %s: parsing the profile's ssh private key: %w",
			target.Host.Name(), target.VMUID, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &sshTransport{
		target:     target,
		clk:        clk,
		log:        log,
		connCtx:    ctx,
		connCancel: cancel,
	}, nil
}

//= docs/requirements/02-executor.md#guest-transport
//# The Guest Transport SHALL provide a readiness operation that
//# succeeds only when a trivial command can be executed in the guest.

// Ready implements Transport: the guest is ready when sshd accepts the
// Profile's key and runs a trivial command that exits zero.
func (t *sshTransport) Ready(ctx context.Context) error {
	status, err := t.Run(ctx, Command{Path: readyCommand})
	if err != nil {
		return fmt.Errorf("host %s: microvm %s is not ready: %w",
			t.target.Host.Name(), t.target.VMUID, errors.Join(ErrNotReady, err))
	}
	if status != 0 {
		return fmt.Errorf("host %s: microvm %s: readiness command exited %d: %w",
			t.target.Host.Name(), t.target.VMUID, status, ErrNotReady)
	}
	return nil
}

//= docs/requirements/02-executor.md#guest-transport
//# The Guest Transport SHALL provide an operation that runs a
//# command in a MicroVM with a working directory, environment, user, standard
//# input stream, standard output stream, standard error stream and returns the
//# command's exit status.

// Run implements Transport over one SSH session: the working directory,
// the environment and the program are sent as the session's command,
// standard input is streamed into it, standard output and standard error
// come back as they are produced, and the session's exit status is the
// command's. The guest user is the user the connection was made as, so a
// Command asking for a different one is refused rather than quietly run as
// somebody else.
func (t *sshTransport) Run(ctx context.Context, cmd Command) (int, error) {
	if cmd.Path == "" {
		return -1, fmt.Errorf("host %s: microvm %s: command has no path", t.target.Host.Name(), t.target.VMUID)
	}
	login := t.loginUser(cmd.User)
	if cmd.User != "" && cmd.User != login {
		return -1, fmt.Errorf("host %s: microvm %s: the ssh transport runs commands as %s and cannot run this one as %s",
			t.target.Host.Name(), t.target.VMUID, login, cmd.User)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watch := watchHost(ctx, cancel, t.target, t.clk)
	defer watch.stop()

	client, err := t.dial(ctx, login)
	if err != nil {
		if unreachable := watch.err(); unreachable != nil {
			return -1, t.streamFailure("host stopped answering", unreachable)
		}
		return -1, err
	}
	session, err := client.NewSession()
	if err != nil {
		return -1, t.streamFailure("opening ssh session", err)
	}
	defer func() { _ = session.Close() }()

	session.Stdin = cmd.Stdin
	session.Stdout = cmd.Stdout
	session.Stderr = cmd.Stderr

	// A cancelled operation, whether by the caller or by the liveness watch,
	// closes the session, which ends the command in the guest.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
		case <-done:
		}
	}()
	runErr := session.Run(commandLine(cmd))
	close(done)
	wg.Wait()

	var exitErr *ssh.ExitError
	switch {
	case runErr == nil:
		return 0, nil
	case errors.As(runErr, &exitErr):
		// The status is known, so this is the script's failure and not the
		// transport's: a non-zero status with a nil error.
		return exitErr.ExitStatus(), nil
	}
	if unreachable := watch.err(); unreachable != nil {
		return -1, t.streamFailure("host stopped answering", unreachable)
	}
	if err := ctx.Err(); err != nil {
		return -1, fmt.Errorf("host %s: microvm %s: ssh session cancelled: %w",
			t.target.Host.Name(), t.target.VMUID, errors.Join(err, ErrStreamFailed))
	}
	return -1, t.streamFailure("ssh session failed before the exit status", runErr)
}

// Close implements Transport: it closes the SSH connection and the proxy
// stream it runs over, and leaves the Host client's gRPC connection alone
// (EX-050).
func (t *sshTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	client, conn := t.client, t.conn
	t.client, t.conn = nil, nil
	t.mu.Unlock()

	var errs []error
	if client != nil {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			errs = append(errs, err)
		}
	}
	if conn != nil {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// Cancelling last means a handshake still in flight sees its stream go
	// away rather than being left to finish into a closed transport.
	t.connCancel()
	return errors.Join(errs...)
}

//= docs/requirements/02-executor.md#guest-transport
//# The `ssh` Guest Transport SHALL connect to the guest's SSH
//# service through the flintlock `MicroVMSSHProxy.SSHProxy` streaming RPC of
//# the Host that runs the MicroVM.

//= docs/requirements/02-executor.md#guest-transport
//# The `ssh` Guest Transport SHALL NOT offer a direct TCP mode,
//# because pool MicroVMs are created from one shared template and flintlock
//# reports no guest address the Runner could connect to.

//= docs/requirements/02-executor.md#guest-transport
//# The `ssh` Guest Transport SHALL authenticate with the private
//# key named by the Profile and SHALL verify the guest host key only where the
//# Profile provides a known host key.

// dial establishes the SSH connection on first use. The byte stream it
// speaks over comes from the Host's SSHProxy RPC and from nowhere else:
// there is no address to dial, because every MicroVM in a Pool is created
// from one template and flintlock reports no guest address, so this
// transport has no TCP mode to fall back to and a Host without the proxy
// service simply cannot run ssh Stages.
//
// Authentication is the Profile's private key. The guest's host key is
// verified when the Profile names one; when it does not there is nothing to
// verify it against, because a pooled MicroVM generates its host key on
// first boot, so the key is accepted. That is the requirement's own
// wording, and it is safe here for the reason the transport exists: the
// stream is not a network path a stranger could sit in, but a proxy inside
// the Host that already runs the MicroVM.
func (t *sshTransport) dial(ctx context.Context, login string) (*ssh.Client, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("host %s: microvm %s: ssh transport is closed", t.target.Host.Name(), t.target.VMUID)
	}
	if t.client != nil {
		client := t.client
		t.mu.Unlock()
		return client, nil
	}
	t.mu.Unlock()

	signer, err := ssh.ParsePrivateKey(t.target.SSH.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("host %s: microvm %s: parsing the profile's ssh private key: %w",
			t.target.Host.Name(), t.target.VMUID, err)
	}
	hostKey, err := t.hostKeyCallback()
	if err != nil {
		return nil, err
	}

	conn, err := t.target.Host.SSHProxy(t.connCtx, t.target.VMUID)
	if err != nil {
		return nil, t.streamFailure("opening the ssh proxy stream", err)
	}

	type handshake struct {
		client *ssh.Client
		err    error
	}
	result := make(chan handshake, 1)
	go func() {
		cfg := &ssh.ClientConfig{
			User:            login,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: hostKey,
		}
		c, chans, reqs, err := ssh.NewClientConn(&streamConn{stream: conn}, sshProxyAddr, cfg)
		if err != nil {
			result <- handshake{err: err}
			return
		}
		result <- handshake{client: ssh.NewClient(c, chans, reqs)}
	}()

	select {
	case r := <-result:
		if r.err != nil {
			_ = conn.Close()
			return nil, t.streamFailure("ssh handshake", r.err)
		}
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			_ = r.client.Close()
			_ = conn.Close()
			return nil, fmt.Errorf("host %s: microvm %s: ssh transport is closed", t.target.Host.Name(), t.target.VMUID)
		}
		t.conn, t.client = conn, r.client
		t.mu.Unlock()
		return r.client, nil
	case <-ctx.Done():
		// Closing the stream unblocks the handshake, which then ends with a
		// read error rather than being left running.
		_ = conn.Close()
		select {
		case r := <-result:
			if r.client != nil {
				_ = r.client.Close()
			}
		case <-time.After(sshHandshakeGrace):
		}
		return nil, fmt.Errorf("host %s: microvm %s: ssh handshake cancelled: %w",
			t.target.Host.Name(), t.target.VMUID, errors.Join(ctx.Err(), ErrStreamFailed))
	}
}

// hostKeyCallback verifies the guest host key against the Profile's known
// host key when it has one (EX-049).
func (t *sshTransport) hostKeyCallback() (ssh.HostKeyCallback, error) {
	known := strings.TrimSpace(t.target.SSH.KnownHostKey)
	if known == "" {
		// EX-049: a pooled MicroVM generates its host key on first boot, so
		// a Profile that names none has nothing to verify against; the
		// stream is a proxy inside the Host, not a network path.
		return ssh.InsecureIgnoreHostKey(), nil
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(known))
	if err != nil {
		return nil, fmt.Errorf("host %s: microvm %s: parsing the profile's known host key: %w",
			t.target.Host.Name(), t.target.VMUID, err)
	}
	return ssh.FixedHostKey(key), nil
}

// loginUser is the guest user the session runs as: the Profile's SSH user,
// or the Command's user when the Profile names none, or root.
func (t *sshTransport) loginUser(commandUser string) string {
	switch {
	case t.target.SSH.User != "":
		return t.target.SSH.User
	case commandUser != "":
		return commandUser
	default:
		return "root"
	}
}

// streamFailure wraps a failure so that it names the Host and the MicroVM
// and satisfies errors.Is(err, ErrStreamFailed). The cause is joined rather
// than flattened, so that a Host which refuses the proxy stream is still
// reported as having refused it.
func (t *sshTransport) streamFailure(what string, cause error) error {
	if cause == nil {
		return fmt.Errorf("host %s: microvm %s: %s: %w", t.target.Host.Name(), t.target.VMUID, what, ErrStreamFailed)
	}
	return fmt.Errorf("host %s: microvm %s: %s: %w", t.target.Host.Name(), t.target.VMUID, what, errors.Join(cause, ErrStreamFailed))
}

// commandLine renders a Command as the single command string an SSH session
// takes. The working directory and the environment are part of it because
// sshd accepts neither over the protocol unless it has been configured to,
// and a Job's Stage cannot depend on how the image's sshd was configured.
// Environment keys are sorted so that the line is the same every time.
func commandLine(cmd Command) string {
	var b strings.Builder
	if cmd.Dir != "" {
		b.WriteString("cd ")
		b.WriteString(shellQuote(cmd.Dir))
		b.WriteString(" && ")
	}
	if len(cmd.Env) > 0 {
		keys := make([]string, 0, len(cmd.Env))
		for k := range cmd.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("export ")
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(shellQuote(cmd.Env[k]))
			b.WriteString(" && ")
		}
	}
	b.WriteString("exec ")
	b.WriteString(shellQuote(cmd.Path))
	for _, arg := range cmd.Args {
		b.WriteString(" ")
		b.WriteString(shellQuote(arg))
	}
	return b.String()
}

// shellQuote renders s as a single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// streamConn presents the SSHProxy byte stream as the net.Conn that the SSH
// client needs. The addresses are placeholders: the stream is a tunnel
// inside a gRPC connection and has no addresses of its own, and SSH uses
// them only in messages. The deadline methods are accepted and ignored;
// nothing in the client sets them, and an operation's own context is what
// bounds it.
type streamConn struct {
	stream io.ReadWriteCloser
}

// Read implements net.Conn.
func (c *streamConn) Read(p []byte) (int, error) { return c.stream.Read(p) }

// Write implements net.Conn.
func (c *streamConn) Write(p []byte) (int, error) { return c.stream.Write(p) }

// Close implements net.Conn.
func (c *streamConn) Close() error { return c.stream.Close() }

// LocalAddr implements net.Conn.
func (c *streamConn) LocalAddr() net.Addr { return streamAddr("runner") }

// RemoteAddr implements net.Conn.
func (c *streamConn) RemoteAddr() net.Addr { return streamAddr(sshProxyAddr) }

// SetDeadline implements net.Conn.
func (c *streamConn) SetDeadline(time.Time) error { return nil }

// SetReadDeadline implements net.Conn.
func (c *streamConn) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline implements net.Conn.
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

// streamAddr is the placeholder address of a proxied stream.
type streamAddr string

// Network implements net.Addr.
func (streamAddr) Network() string { return "flintlock-sshproxy" }

// String implements net.Addr.
func (a streamAddr) String() string { return string(a) }

// Compile-time checks.
var (
	_ Transport = (*sshTransport)(nil)
	_ net.Conn  = (*streamConn)(nil)
)
