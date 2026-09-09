package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// execStream is the server side of one ExecCommand exchange. The generated
// grpc bidi server stream satisfies it and so does the in-memory stream
// behind Client.Exec, which is how one handler serves both.
type execStream interface {
	Context() context.Context
	Send(*execv1.ExecCommandResponse) error
	Recv() (*execv1.ExecCommandRequest, error)
}

// Exit codes the fake reports when the process never ran. 127 is what a
// shell reports for a command it cannot find; 1 covers every other start
// failure such as a missing working directory.
const (
	exitNotFound   = 127
	exitStartError = 1
	// waitDelay bounds how long Wait waits for output pipes to drain after
	// the process group has been killed, so that a grandchild that inherited
	// stdout cannot hold a stream open forever.
	waitDelay = 5 * time.Second
)

//= docs/requirements/10-test-doubles.md#fake-host
//# The fake Host SHALL represent each MicroVM as a sandbox directory on the
//# local filesystem and SHALL run each `ExecCommand` as a local process
//# rooted in that directory, streaming standard input, standard output,
//# standard error and the exit code as `flintlockd` does.

// execCommand serves one ExecCommand exchange (TD-021): the first message
// has to be an ExecStart naming a CREATED MicroVM, the command runs as a
// local process in that MicroVM's sandbox, stdout and stderr chunks are
// streamed back as they are produced, stdin messages are piped in until
// stdin_eof or the client half-closes, and exit_code ends the stream. An
// agent-side failure (the command could not start, the timeout fired, the
// MicroVM was deleted) is sent as an error payload followed by exit_code,
// which is the guest-agent's framing.
//
// The framing rules and status codes are flintlockd's: InvalidArgument for
// a bad first message, NotFound for an unknown uid, FailedPrecondition for a
// MicroVM that is not CREATED. Unlike flintlockd the fake does not require
// allow_guest_agent on the spec.
func (h *Host) execCommand(stream execStream) error {
	first, err := stream.Recv()
	if err != nil {
		// io.EOF is never propagated: to a client it is the marker of a
		// clean end of stream, and a client that half-closed without ever
		// starting a command has to see a failure instead.
		if errors.Is(err, io.EOF) {
			return errors.New("exec stream closed before the start message")
		}
		return fmt.Errorf("receiving exec start message: %w", err)
	}
	start := first.GetStart()
	if start == nil || start.GetUid() == "" || (start.GetCmd() == "" && !start.GetShell()) {
		return status.Error(codes.InvalidArgument, "first message must be a start message with uid and cmd set")
	}

	// ctx ends when the client goes away, the Host closes, the timeout
	// fires or the MicroVM is deleted; any of those kills the process.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	defer context.AfterFunc(h.ctx, cancel)()

	sandbox, detach, err := h.attachExec(start.GetUid(), cancel)
	if err != nil {
		return err
	}
	defer detach()

	run := &execRun{h: h, ctx: ctx, stream: stream, start: start, sandbox: sandbox}
	return run.do()
}

// execRun is the state of one running command.
type execRun struct {
	h       *Host
	ctx     context.Context
	stream  execStream
	start   *execv1.ExecStart
	sandbox string

	// sendMu serialises Send, which the stream does not allow from more
	// than one goroutine; stdout and stderr are copied concurrently.
	sendMu  sync.Mutex
	sendErr error
}

// do runs the command to completion and finishes the stream.
func (r *execRun) do() error {
	cmd, err := r.buildCommand()
	if err != nil {
		return r.finish(err.Error(), exitStartError)
	}

	var stdin io.WriteCloser
	if r.start.GetHasStdin() {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return r.finish(err.Error(), exitStartError)
		}
	}
	cmd.Stdout = &chunkWriter{run: r, kind: chunkStdout}
	cmd.Stderr = &chunkWriter{run: r, kind: chunkStderr}

	if err := cmd.Start(); err != nil {
		code := exitStartError
		if errors.Is(err, exec.ErrNotFound) {
			code = exitNotFound
		}
		return r.finish(err.Error(), code)
	}

	// Stdin is relayed from the client until stdin_eof or half-close; when
	// the command asked for no stdin the messages are read and dropped so
	// that a chatty client never blocks on flow control. The goroutine ends
	// when the stream does, which is when this handler returns.
	go pumpStdin(r.stream, stdin)

	//= docs/requirements/10-test-doubles.md#fake-host
	//# The fake Host SHALL honour the `cwd`, `env`, `timeout_seconds`,
	//# `has_stdin` and `stdin_eof` fields of an exec request and SHALL accept
	//# and ignore `user`.
	// timeout_seconds is a deadline on the command, not on the stream: when
	// it fires the process group is killed and the client is told why,
	// rather than the stream being dropped (TD-023).
	var timedOut atomic.Bool
	if secs := r.start.GetTimeoutSeconds(); secs > 0 {
		timer := r.h.clk.NewTimer(time.Duration(secs) * time.Second)
		defer timer.Stop()
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-timer.C():
				timedOut.Store(true)
				_ = cmd.Cancel()
			case <-done:
			}
		}()
	}

	waitErr := cmd.Wait()

	// The client is gone or the Host is shutting down: nobody is listening,
	// so end the stream with the cause rather than a synthetic exit code.
	if err := r.stream.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	if r.h.ctx.Err() != nil {
		return status.Error(codes.Unavailable, "fake host shutting down")
	}

	code := cmd.ProcessState.ExitCode()
	switch {
	case timedOut.Load():
		return r.finish(fmt.Sprintf("command timed out after %ds", r.start.GetTimeoutSeconds()), code)
	case r.ctx.Err() != nil:
		// Only DeleteMicroVM is left as a cause of cancellation.
		return r.finish("microvm deleted while command was running", code)
	case waitErr != nil && !isExitError(waitErr):
		return r.finish(waitErr.Error(), code)
	}
	return r.exit(code)
}

// buildCommand turns the ExecStart into an exec.Cmd rooted in the sandbox
// (TD-023): cwd is resolved inside the sandbox, env entries are added to and
// override the fake's own environment, shell mode runs the command line
// through sh -c, and user is accepted and ignored because the fake runs
// everything as the test user. The process gets its own process group so
// that cancellation kills whatever the command spawned.
func (r *execRun) buildCommand() (*exec.Cmd, error) {
	//= docs/requirements/10-test-doubles.md#fake-host
	//# The fake Host SHALL honour the `cwd`, `env`, `timeout_seconds`,
	//# `has_stdin` and `stdin_eof` fields of an exec request and SHALL accept
	//# and ignore `user`.
	dir, err := sandboxDir(r.sandbox, r.start.GetCwd())
	if err != nil {
		return nil, err
	}
	// os/exec reports a working directory that does not exist as a failure
	// of the binary it could not run ("fork/exec /bin/sh: no such file or
	// directory"), which names the wrong thing. Check it here so that the
	// error payload names the cwd the request asked for, as the guest agent's
	// chdir failure would.
	if info, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("cwd %q: %w", r.start.GetCwd(), err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("cwd %q is not a directory", r.start.GetCwd())
	}

	var cmd *exec.Cmd
	if r.start.GetShell() {
		line := strings.Join(append([]string{r.start.GetCmd()}, r.start.GetArgs()...), " ")
		cmd = exec.CommandContext(r.ctx, "sh", "-c", line)
	} else {
		cmd = exec.CommandContext(r.ctx, r.start.GetCmd(), r.start.GetArgs()...)
	}
	cmd.Dir = dir
	cmd.Env = mergeEnv(os.Environ(), r.start.GetEnv())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
	return cmd, nil
}

// sandboxDir resolves a guest working directory inside the sandbox. The
// sandbox stands in for the guest's root, so an absolute cwd is taken
// relative to it and a relative one relative to it as well.
//
// The two are clamped differently, which is how the guest's kernel would
// see them. An absolute cwd is a path from the guest's root and cannot name
// anything above it: "/.." is the root itself, exactly as it is inside a
// chroot, so it is cleaned as an absolute path before it is joined. A
// relative cwd is interpreted from wherever the caller thinks it is, and one
// that climbs above the sandbox is a request the fake cannot honour without
// letting a Job read the test machine, so it is refused.
func sandboxDir(sandbox, cwd string) (string, error) {
	if cwd == "" {
		return sandbox, nil
	}
	if filepath.IsAbs(cwd) {
		cwd = filepath.Clean(cwd)
	}
	dir := filepath.Join(sandbox, cwd)
	if dir != sandbox && !strings.HasPrefix(dir, sandbox+string(filepath.Separator)) {
		return "", fmt.Errorf("cwd %q escapes the microvm sandbox", cwd)
	}
	return dir, nil
}

// mergeEnv overlays extra onto base, replacing entries with the same key.
// Keys are applied in sorted order so that the result is deterministic.
func mergeEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, override := extra[k]; !override {
			out = append(out, kv)
		}
	}
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}

//= docs/requirements/10-test-doubles.md#fake-host
//# The fake Host SHALL honour the `cwd`, `env`, `timeout_seconds`,
//# `has_stdin` and `stdin_eof` fields of an exec request and SHALL accept
//# and ignore `user`.

// pumpStdin relays stdin and stdin_eof messages into w until the client
// half-closes or the stream ends (TD-023): stdin_eof closes the pipe, which
// is what lets a reader such as cat see end of input while the stream stays
// open for its output. A nil w means the request did not set has_stdin;
// messages are then drained and dropped.
func pumpStdin(stream execStream, w io.WriteCloser) {
	defer func() {
		if w != nil {
			_ = w.Close()
		}
	}()
	for {
		msg, err := stream.Recv()
		if err != nil {
			return
		}
		switch p := msg.GetPayload().(type) {
		case *execv1.ExecCommandRequest_Stdin:
			if w == nil {
				continue
			}
			if _, err := w.Write(p.Stdin); err != nil {
				return
			}
		case *execv1.ExecCommandRequest_StdinEof:
			if p.StdinEof && w != nil {
				return
			}
		}
	}
}

// chunkKind selects the stdout or stderr payload.
type chunkKind int

const (
	chunkStdout chunkKind = iota
	chunkStderr
)

// chunkWriter sends each write as one output chunk, copying the bytes
// because os/exec reuses its buffer and the stream may retain the message.
type chunkWriter struct {
	run  *execRun
	kind chunkKind
}

// Write implements io.Writer.
func (w *chunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	data := append([]byte(nil), p...)
	resp := &execv1.ExecCommandResponse{}
	if w.kind == chunkStdout {
		resp.Payload = &execv1.ExecCommandResponse_Stdout{Stdout: data}
	} else {
		resp.Payload = &execv1.ExecCommandResponse_Stderr{Stderr: data}
	}
	if err := w.run.send(resp); err != nil {
		return 0, err
	}
	return len(p), nil
}

// send writes one response, stalling first while the Host is Unresponsive
// (TD-025). After the first failure every send returns the same error.
func (r *execRun) send(resp *execv1.ExecCommandResponse) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if r.sendErr != nil {
		return r.sendErr
	}
	if err := r.h.awaitResponsive(r.ctx); err != nil {
		r.sendErr = err
		return err
	}
	if err := r.stream.Send(resp); err != nil {
		r.sendErr = err
		return err
	}
	return nil
}

// finish sends an error payload followed by the exit code, the guest
// agent's way of reporting a failure of its own.
func (r *execRun) finish(msg string, code int) error {
	if err := r.send(&execv1.ExecCommandResponse{
		Payload: &execv1.ExecCommandResponse_Error{Error: msg},
	}); err != nil {
		return fmt.Errorf("sending exec error: %w", err)
	}
	return r.exit(code)
}

//= docs/requirements/10-test-doubles.md#fake-host
//# The fake Host SHALL support fault injection for a create that ends in
//# `FAILED`, an exec stream dropped before the exit code, and a Host that
//# stops answering.

// exit ends the stream with exit_code, or drops it first when the
// DropExecBeforeExit fault is on (TD-025): the client then sees a non-EOF
// error from Recv after all the output and no exit code, which is what a
// Host dying mid-Stage looks like (EX-023).
func (r *execRun) exit(code int) error {
	if r.h.Faults().DropExecBeforeExit {
		return status.Error(codes.Unavailable, "fake host: exec stream dropped before exit code")
	}
	if err := r.send(&execv1.ExecCommandResponse{
		Payload: &execv1.ExecCommandResponse_ExitCode{ExitCode: int32(code)},
	}); err != nil {
		return fmt.Errorf("sending exit code: %w", err)
	}
	return nil
}

// isExitError reports whether err is the process exiting non-zero, which is
// an exit code rather than a failure of the fake.
func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}
