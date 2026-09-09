package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// stdinChunk is how many bytes of the Stage script travel in one stdin
// message. It is well under gRPC's default 4 MiB message limit and large
// enough that a script is one or two messages.
const stdinChunk = 32 * 1024

// readyCommand is the trivial command the readiness probe runs in the guest
// (EX-041). It is run through the guest's shell so that it does not depend
// on where the image puts the binary.
const readyCommand = "true"

// execTransport is the exec Guest Transport: every operation is one
// MicroVMExec.ExecCommand exchange on the Host client's connection.
type execTransport struct {
	target Target
	clk    clock.Clock
	log    *slog.Logger
}

// hostName is the Host every error of this transport names.
func (t *execTransport) hostName() string { return t.target.Host.Name() }

// vmUID is the MicroVM this transport is bound to.
func (t *execTransport) vmUID() string { return t.target.VMUID }

// watchHost starts the liveness watch for one operation (EX-051).
func (t *execTransport) watchHost(ctx context.Context, cancel context.CancelFunc) *hostWatch {
	return watchHost(ctx, cancel, t.target, t.clk)
}

//= docs/requirements/02-executor.md#guest-transport
//# The Guest Transport SHALL provide a readiness operation that
//# succeeds only when a trivial command can be executed in the guest.

// Ready implements Transport: it runs a trivial command in the guest and
// succeeds only when that command ran and exited zero. Anything else -- the
// MicroVM not being CREATED yet, the guest agent not answering, the Host
// reporting an error payload -- is reported as not ready, wrapping the
// cause so that a caller which cares can tell a booting guest from a broken
// Host.
func (t *execTransport) Ready(ctx context.Context) error {
	start := &execv1.ExecStart{
		Uid:   t.vmUID(),
		Cmd:   readyCommand,
		Shell: true,
	}
	status, err := t.exchange(ctx, start, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("host %s: microvm %s is not ready: %w", t.hostName(), t.vmUID(), errors.Join(ErrNotReady, err))
	}
	if status != 0 {
		return fmt.Errorf("host %s: microvm %s: readiness command exited %d: %w", t.hostName(), t.vmUID(), status, ErrNotReady)
	}
	return nil
}

//= docs/requirements/02-executor.md#guest-transport
//# The Guest Transport SHALL provide an operation that runs a
//# command in a MicroVM with a working directory, environment, user, standard
//# input stream, standard output stream, standard error stream and returns the
//# command's exit status.

// Run implements Transport. Every field of the Command is carried in the
// ExecStart message: the program and its arguments, the working directory
// as cwd, the environment as env, the guest user as user, and whether there
// is standard input as has_stdin; standard output and standard error are
// written as their chunks arrive (EX-021) and the exit status is the
// exit_code payload (EX-045).
func (t *execTransport) Run(ctx context.Context, cmd Command) (int, error) {
	if cmd.Path == "" {
		return -1, fmt.Errorf("host %s: microvm %s: command has no path", t.hostName(), t.vmUID())
	}
	start := &execv1.ExecStart{
		Uid:      t.vmUID(),
		Cmd:      cmd.Path,
		Args:     cmd.Args,
		Cwd:      cmd.Dir,
		Env:      cmd.Env,
		User:     cmd.User,
		HasStdin: cmd.Stdin != nil,
	}
	return t.exchange(ctx, start, cmd.Stdin, cmd.Stdout, cmd.Stderr)
}

// Close implements Transport. The exec transport keeps no per-Job state:
// each operation is its own stream and the connection belongs to the Host
// client (EX-050), so there is nothing to release.
func (t *execTransport) Close() error { return nil }

//= docs/requirements/02-executor.md#guest-transport
//# The `exec` Guest Transport SHALL send an `ExecStart` message
//# with `has_stdin` set, followed by the Stage script as `stdin` chunks,
//# followed by `stdin_eof`.

//= docs/requirements/02-executor.md#guest-transport
//# The `exec` Guest Transport SHALL set the `timeout_seconds` field
//# of `ExecStart` from the remaining time on the operation's context.

// exchange runs one ExecCommand exchange to its end and returns the exit
// status. The framing is flintlockd's: ExecStart first, carrying
// timeout_seconds derived from what is left of ctx so that the guest agent
// gives up when the caller would; then, when the command has standard
// input, the input as stdin chunks and a stdin_eof message that lets a
// reader in the guest see the end of its input while the stream stays open
// for output.
//
// The stream is cancelled on the way out whatever happens, which is what
// terminates the process in the guest when ctx is cancelled (EX-024), and
// the goroutine that feeds standard input is waited for, so no operation
// leaves anything running behind it.
func (t *execTransport) exchange(ctx context.Context, start *execv1.ExecStart, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	start.TimeoutSeconds = remainingSeconds(ctx)

	stream, err := t.target.Host.Exec(ctx)
	if err != nil {
		return -1, t.streamFailure("opening exec stream", err)
	}
	if err := stream.Send(&execv1.ExecCommandRequest{
		Payload: &execv1.ExecCommandRequest_Start{Start: start},
	}); err != nil {
		return -1, t.streamFailure("sending exec start", firstError(err, stream))
	}

	var (
		wg      sync.WaitGroup
		stdinCh = make(chan error, 1)
	)
	if start.GetHasStdin() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stdinCh <- sendStdin(stream, stdin)
		}()
	} else {
		// Nothing to send: half-close so that a guest agent waiting for the
		// client never holds the exchange open.
		_ = stream.CloseSend()
		stdinCh <- nil
	}
	// Cancel before waiting, and in that order: a stdin goroutine blocked on
	// a Send that a stalled Host is not reading would otherwise keep this
	// call here for ever, because the deferred cancel that would release it
	// runs only after the wait returns.
	defer func() {
		cancel()
		wg.Wait()
	}()

	status, err := t.receive(ctx, cancel, stream, stdout, stderr)
	if err != nil {
		// A stdin failure is the better explanation when both happened: the
		// stream fell over because the input could not be sent.
		select {
		case stdinErr := <-stdinCh:
			if stdinErr != nil {
				return -1, t.streamFailure("sending stdin", stdinErr)
			}
		default:
		}
		return -1, err
	}
	return status, nil
}

// receive reads the response stream to its end, writing output as it
// arrives and watching the Host for liveness while it does.
func (t *execTransport) receive(ctx context.Context, cancel context.CancelFunc, stream flintlock.ExecStream, stdout, stderr io.Writer) (int, error) {
	watch := t.watchHost(ctx, cancel)
	defer watch.stop()

	for {
		resp, err := stream.Recv()
		if err != nil {
			if unreachable := watch.err(); unreachable != nil {
				return -1, t.streamFailure("host stopped answering", unreachable)
			}
			if errors.Is(err, io.EOF) {
				// The stream ended without an exit_code payload, so the
				// command's status is unknown and the Stage must not be run
				// again (EX-023).
				return -1, t.streamFailure("exec stream ended before the exit code", errors.New("no exit_code payload"))
			}
			return -1, t.streamFailure("exec stream failed", err)
		}
		watch.sawResponse()

		//= docs/requirements/02-executor.md#guest-transport
		//# The `exec` Guest Transport SHALL treat an `error` payload in the
		//# response stream as a transport failure and SHALL treat an
		//# `exit_code` payload as the command's exit status.

		// Output is written on its way past; the other two payloads end the
		// exchange, one as a failure whose status is unknown and one as the
		// command's own answer.
		switch payload := resp.GetPayload().(type) {
		case *execv1.ExecCommandResponse_Stdout:
			if err := write(stdout, payload.Stdout); err != nil {
				return -1, t.streamFailure("writing stdout", err)
			}
		case *execv1.ExecCommandResponse_Stderr:
			if err := write(stderr, payload.Stderr); err != nil {
				return -1, t.streamFailure("writing stderr", err)
			}
		case *execv1.ExecCommandResponse_Error:
			return -1, t.execError(payload.Error)
		case *execv1.ExecCommandResponse_ExitCode:
			return int(payload.ExitCode), nil
		}
	}
}

//= docs/requirements/02-executor.md#guest-transport
//# The `exec` Guest Transport SHALL treat an `error` payload in the
//# response stream as a transport failure and SHALL treat an `exit_code`
//# payload as the command's exit status.

// execError turns an error payload into a transport failure. The payload is
// how the Host reports that the command never ran or was stopped from
// underneath it -- a command it could not start, a MicroVM deleted
// mid-Stage, an exec session whose control channel passed its idle deadline
// -- and none of those leave the command's exit status known, so the
// Executor has to treat it as a system error and must not re-run the Stage
// (EX-023). The Host and the payload's own words are both in the message,
// so the Job log says which Host gave up and why rather than reporting a
// bare stream failure.
func (t *execTransport) execError(payload string) error {
	if payload == "" {
		payload = "no reason given"
	}
	return fmt.Errorf("host %s: microvm %s: exec reported an error before the exit status: %s: %w",
		t.hostName(), t.vmUID(), payload, ErrStreamFailed)
}

// streamFailure wraps a stream failure so that it names the Host and the
// MicroVM and satisfies errors.Is(err, ErrStreamFailed). The cause is joined
// rather than flattened, so that a caller can still tell an unauthenticated
// Host from an unreachable one.
func (t *execTransport) streamFailure(what string, cause error) error {
	if cause == nil {
		return fmt.Errorf("host %s: microvm %s: %s: %w", t.hostName(), t.vmUID(), what, ErrStreamFailed)
	}
	return fmt.Errorf("host %s: microvm %s: %s: %w", t.hostName(), t.vmUID(), what, errors.Join(cause, ErrStreamFailed))
}

//= docs/requirements/02-executor.md#guest-transport
//# The `exec` Guest Transport SHALL send an `ExecStart` message
//# with `has_stdin` set, followed by the Stage script as `stdin` chunks,
//# followed by `stdin_eof`.

// sendStdin streams r as stdin messages and ends with stdin_eof, then
// half-closes. A stream that has already failed reports io.EOF from Send,
// which is not a stdin failure: the reason is on the response stream and
// the receiving side reports it.
func sendStdin(stream flintlock.ExecStream, r io.Reader) error {
	defer func() { _ = stream.CloseSend() }()
	if r == nil {
		return nil
	}
	buf := make([]byte, stdinChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if sendErr := stream.Send(&execv1.ExecCommandRequest{
				Payload: &execv1.ExecCommandRequest_Stdin{Stdin: chunk},
			}); sendErr != nil {
				if errors.Is(sendErr, io.EOF) {
					return nil
				}
				return sendErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("reading stdin: %w", err)
		}
	}
	if err := stream.Send(&execv1.ExecCommandRequest{
		Payload: &execv1.ExecCommandRequest_StdinEof{StdinEof: true},
	}); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// firstError prefers the status the Host put on the response stream over
// the io.EOF gRPC reports from Send when a stream has already failed.
func firstError(err error, stream flintlock.ExecStream) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	if _, recvErr := stream.Recv(); recvErr != nil && !errors.Is(recvErr, io.EOF) {
		return recvErr
	}
	return err
}

// write sends a chunk to a writer, tolerating the nil writer that means the
// caller wants the output discarded.
func write(w io.Writer, chunk []byte) error {
	if w == nil || len(chunk) == 0 {
		return nil
	}
	if _, err := w.Write(chunk); err != nil {
		return err
	}
	return nil
}

//= docs/requirements/02-executor.md#guest-transport
//# The `exec` Guest Transport SHALL set the `timeout_seconds` field
//# of `ExecStart` from the remaining time on the operation's context.

// remainingSeconds is what is left of ctx, in whole seconds rounded up,
// which is the unit ExecStart has. A context without a deadline yields
// zero, meaning no guest-side timeout; a context that is already past its
// deadline yields one, the smallest timeout that can be expressed, because
// sending zero would remove the bound instead of tightening it.
func remainingSeconds(ctx context.Context) int32 {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 1
	}
	secs := math.Ceil(remaining.Seconds())
	if secs > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(secs)
}

// Compile-time check.
var _ Transport = (*execTransport)(nil)
