package transport_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// newExecTransport builds the exec Guest Transport for a target.
func newExecTransport(t *testing.T, target transport.Target) transport.Transport {
	t.Helper()
	if target.Kind == "" {
		target.Kind = transport.KindExec
	}
	tr, err := transport.NewFactory().New(context.Background(), target)
	if err != nil {
		t.Fatalf("building the %s transport: %v", target.Kind, err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The Guest Transport SHALL provide an operation that runs a
//# command in a MicroVM with a working directory, environment, user, standard
//# input stream, standard output stream, standard error stream and returns the
//# command's exit status.

// TestRunCarriesEveryPartOfTheCommand runs a script in a MicroVM on a fake
// Host with a working directory, an environment and standard input, and
// checks that each one reached the guest: the script prints the directory it
// ran in, a variable from the environment and what it read on standard
// input, writes to both output streams and exits with a status of its own
// choosing. The guest user travels in the same message, which the fake Host
// accepts and ignores as flintlockd's guest agent documents, so it is
// checked on the message rather than in the output.
func TestRunCarriesEveryPartOfTheCommand(t *testing.T) {
	t.Parallel()
	host, client, uid := newFakeHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	sandbox, ok := host.SandboxPath(uid)
	if !ok {
		t.Fatal("the fake host has no sandbox for the microvm")
	}
	if err := os.MkdirAll(filepath.Join(sandbox, "builds", "project"), 0o755); err != nil {
		t.Fatalf("creating the builds directory: %v", err)
	}

	tr := newExecTransport(t, transport.Target{Host: client, VMUID: uid})
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	status, err := tr.Run(ctx, transport.Command{
		Path: "sh",
		Dir:  "/builds/project",
		Env:  map[string]string{"CI_JOB_STAGE": "build"},
		User: "root",
		// The script is the standard input: a shell reading its commands
		// from the stream is how every Stage is delivered (EX-020).
		Stdin:  strings.NewReader("pwd\necho $CI_JOB_STAGE\necho to-stderr >&2\nexit 7\n"),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != 7 {
		t.Errorf("Run returned exit status %d, want 7", status)
	}
	out := stdout.String()
	if !strings.Contains(out, "/builds/project") {
		t.Errorf("the command ran in the wrong directory; stdout was %q", out)
	}
	if !strings.Contains(out, "build") {
		t.Errorf("the environment did not reach the guest; stdout was %q", out)
	}
	if strings.Count(out, "\n") != 2 {
		t.Errorf("the script on standard input did not run to its end; stdout was %q", out)
	}
	if got := stderr.String(); !strings.Contains(got, "to-stderr") {
		t.Errorf("standard error was %q, want it to carry the command's error output", got)
	}

	t.Run("the guest user travels in the start message", func(t *testing.T) {
		stream := newScriptedStream(exitCode(0))
		stub := &stubHost{name: "h1", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
		scripted := newExecTransport(t, transport.Target{Host: stub, VMUID: "vm"})
		if _, err := scripted.Run(ctx, transport.Command{Path: "sh", User: "builder", Args: []string{"-c", "true"}}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		start := stream.requests()[0].GetStart()
		if start.GetUser() != "builder" {
			t.Errorf("the start message carried user %q, want builder", start.GetUser())
		}
		if start.GetCmd() != "sh" || len(start.GetArgs()) != 2 {
			t.Errorf("the start message carried cmd %q args %v, want the command and its arguments", start.GetCmd(), start.GetArgs())
		}
		if start.GetUid() != "vm" {
			t.Errorf("the start message named microvm %q, want vm", start.GetUid())
		}
	})
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The Guest Transport SHALL provide a readiness operation that
//# succeeds only when a trivial command can be executed in the guest.

// TestReadySucceedsOnlyWhenAGuestCanRunSomething asks a booted MicroVM and a
// MicroVM that is still booting. The booted one runs the trivial command and
// is ready; the one that is still PENDING cannot run anything, so readiness
// has to be refused rather than assumed from the Host being reachable.
func TestReadySucceedsOnlyWhenAGuestCanRunSomething(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("a booted guest", func(t *testing.T) {
		_, client, uid := newFakeHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
		tr := newExecTransport(t, transport.Target{Host: client, VMUID: uid})
		if err := tr.Ready(ctx); err != nil {
			t.Fatalf("Ready on a booted guest: %v", err)
		}
	})

	t.Run("a guest that is still booting", func(t *testing.T) {
		// The boot timer is armed on a clock that never advances, so the
		// MicroVM stays PENDING and nothing can run in it.
		_, client, uid := newFakeHost(t,
			flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true, BootDelay: time.Minute},
			fake.WithClock(stoppedClock{}),
		)
		tr := newExecTransport(t, transport.Target{Host: client, VMUID: uid})
		err := tr.Ready(ctx)
		if !errors.Is(err, transport.ErrNotReady) {
			t.Fatalf("Ready on a booting guest returned %v, want ErrNotReady", err)
		}
	})

	t.Run("a guest whose trivial command fails", func(t *testing.T) {
		stream := newScriptedStream(exitCode(1))
		stub := &stubHost{name: "h1", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
		tr := newExecTransport(t, transport.Target{Host: stub, VMUID: "vm"})
		if err := tr.Ready(ctx); !errors.Is(err, transport.ErrNotReady) {
			t.Fatalf("Ready returned %v when the trivial command exited non-zero, want ErrNotReady", err)
		}
	})
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The `exec` Guest Transport SHALL send an `ExecStart` message
//# with `has_stdin` set, followed by the Stage script as `stdin` chunks,
//# followed by `stdin_eof`.

// TestExecFramingIsStartStdinEof records the exact exchange the transport
// sends for a Stage: the start message first, with has_stdin set because
// there is a script to deliver, then the script itself as stdin chunks, then
// stdin_eof, which is what lets the shell in the guest see the end of its
// input while the stream stays open for output. A Stage with no standard
// input sends the start message alone with has_stdin clear.
func TestExecFramingIsStartStdinEof(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	script := strings.Repeat("echo hello\n", 4096)

	stream := newScriptedStream(exitCode(0))
	stub := &stubHost{name: "h1", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
	tr := newExecTransport(t, transport.Target{Host: stub, VMUID: "vm"})
	if _, err := tr.Run(ctx, transport.Command{Path: "bash", Stdin: strings.NewReader(script)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := stream.requests()
	if len(reqs) < 3 {
		t.Fatalf("the transport sent %d messages, want a start, at least one stdin chunk and stdin_eof", len(reqs))
	}
	start := reqs[0].GetStart()
	if start == nil {
		t.Fatalf("the first message was %T, want the start message", reqs[0].GetPayload())
	}
	if !start.GetHasStdin() {
		t.Error("the start message did not set has_stdin for a stage with a script")
	}
	var delivered []byte
	for i, req := range reqs[1 : len(reqs)-1] {
		chunk, ok := req.GetPayload().(*execv1.ExecCommandRequest_Stdin)
		if !ok {
			t.Fatalf("message %d was %T, want a stdin chunk", i+1, req.GetPayload())
		}
		delivered = append(delivered, chunk.Stdin...)
	}
	if string(delivered) != script {
		t.Errorf("the guest received %d bytes of script, want %d", len(delivered), len(script))
	}
	last := reqs[len(reqs)-1]
	if eof, ok := last.GetPayload().(*execv1.ExecCommandRequest_StdinEof); !ok || !eof.StdinEof {
		t.Errorf("the last message was %T, want stdin_eof", last.GetPayload())
	}

	t.Run("a stage with no standard input", func(t *testing.T) {
		stream := newScriptedStream(exitCode(0))
		stub := &stubHost{name: "h1", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
		tr := newExecTransport(t, transport.Target{Host: stub, VMUID: "vm"})
		if _, err := tr.Run(ctx, transport.Command{Path: "true"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		reqs := stream.requests()
		if len(reqs) != 1 {
			t.Fatalf("the transport sent %d messages, want the start message alone", len(reqs))
		}
		if reqs[0].GetStart().GetHasStdin() {
			t.Error("has_stdin was set for a command with no standard input")
		}
	})
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The `exec` Guest Transport SHALL set the `timeout_seconds` field
//# of `ExecStart` from the remaining time on the operation's context.

// TestTimeoutSecondsComesFromTheContext checks the field upstream now
// derives its own idle deadline from: an operation with a deadline sends
// what is left of it, rounded up to the whole seconds the field holds, and
// an operation without one sends zero, which is no guest-side timeout at
// all.
func TestTimeoutSecondsComesFromTheContext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		timeout time.Duration
		want    func(int32) bool
		wantStr string
	}{
		{
			name:    "a deadline of ninety seconds",
			timeout: 90 * time.Second,
			want:    func(secs int32) bool { return secs == 90 || secs == 89 },
			wantStr: "89 or 90",
		},
		{
			name:    "a deadline under a second",
			timeout: 500 * time.Millisecond,
			want:    func(secs int32) bool { return secs == 1 },
			wantStr: "1",
		},
		{
			name:    "no deadline at all",
			timeout: 0,
			want:    func(secs int32) bool { return secs == 0 },
			wantStr: "0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			stream := newScriptedStream(exitCode(0))
			stub := &stubHost{name: "h1", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
			tr := newExecTransport(t, transport.Target{Host: stub, VMUID: "vm"})
			if _, err := tr.Run(ctx, transport.Command{Path: "true"}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := stream.requests()[0].GetStart().GetTimeoutSeconds()
			if !tc.want(got) {
				t.Errorf("the start message carried timeout_seconds %d, want %s", got, tc.wantStr)
			}
		})
	}
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The `exec` Guest Transport SHALL treat an `error` payload in the
//# response stream as a transport failure and SHALL treat an `exit_code`
//# payload as the command's exit status.

// TestErrorPayloadFailsAndExitCodeIsTheStatus separates the two payloads
// that end an exchange. An exit_code is the command's own answer, zero or
// not, and the transport reports it with no error. An error payload is the
// Host saying the command never ran or was stopped from underneath it --
// upstream's exec session passing its control-channel idle deadline reads
// exactly like this -- so the status is unknown, the failure wraps
// ErrStreamFailed and its message names the Host and repeats what the Host
// said, whatever follows it on the stream.
func TestErrorPayloadFailsAndExitCodeIsTheStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("exit_code is the status", func(t *testing.T) {
		for _, code := range []int32{0, 1, 137} {
			stream := newScriptedStream(stdout("out"), stderr("err"), exitCode(code))
			stub := &stubHost{name: "h1", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
			tr := newExecTransport(t, transport.Target{Host: stub, VMUID: "vm"})
			var out, errOut bytes.Buffer
			status, err := tr.Run(ctx, transport.Command{Path: "sh", Stdout: &out, Stderr: &errOut})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if status != int(code) {
				t.Errorf("Run returned %d, want the exit_code payload %d", status, code)
			}
			if out.String() != "out" || errOut.String() != "err" {
				t.Errorf("Run wrote (%q, %q), want the output that came before the exit code", out.String(), errOut.String())
			}
		}
	})

	t.Run("an error payload is a transport failure", func(t *testing.T) {
		// The exit code after the error payload is the guest agent's own
		// framing; the error is still what the Stage's outcome is decided
		// on, because the code that follows it is not the command's.
		stream := newScriptedStream(stdout("partial"), errorPayload("exec session control channel idle deadline exceeded"), exitCode(0))
		stub := &stubHost{name: "h1", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
		tr := newExecTransport(t, transport.Target{Host: stub, VMUID: "vm"})
		status, err := tr.Run(ctx, transport.Command{Path: "sh"})
		if !errors.Is(err, transport.ErrStreamFailed) {
			t.Fatalf("Run returned (%d, %v), want a failure wrapping ErrStreamFailed", status, err)
		}
		if status == 0 {
			t.Errorf("Run returned exit status %d after an error payload; the status is not known", status)
		}
		for _, want := range []string{"h1", "vm", "idle deadline"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the failure %q does not mention %q", err, want)
			}
		}
	})

	t.Run("a stream that ends without either", func(t *testing.T) {
		host, client, uid := newFakeHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
		host.SetFaults(flintlock.HostFaults{DropExecBeforeExit: true})
		tr := newExecTransport(t, transport.Target{Host: client, VMUID: uid})
		status, err := tr.Run(ctx, transport.Command{Path: "echo", Args: []string{"hello"}})
		if !errors.Is(err, transport.ErrStreamFailed) {
			t.Fatalf("Run returned (%d, %v) when the host dropped the stream, want ErrStreamFailed", status, err)
		}
		if !strings.Contains(err.Error(), "h1") {
			t.Errorf("the failure %q does not name the host", err)
		}
	})
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# If the Host that runs the MicroVM becomes unreachable, then the
//# Guest Transport SHALL fail the in-flight operation within the configured
//# transport deadline.

// TestRunFailsWhenTheHostStopsAnswering starts a long command and then makes
// the Host stop answering, which is what a Host dying mid-Stage looks like:
// the connection stays up, the stream stalls and nothing more arrives. The
// operation has to end within the configured deadline with a failure that
// names the Host, and not hang until the Job's own timeout. The second half
// of the test is the case that makes this hard: a Stage that is merely
// quiet, on a Host that is answering, has to be left alone.
func TestRunFailsWhenTheHostStopsAnswering(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("a host that stops answering", func(t *testing.T) {
		host, client, uid := newFakeHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
		const deadline = 500 * time.Millisecond
		tr := newExecTransport(t, transport.Target{Host: client, VMUID: uid, Deadline: deadline})

		started := make(chan struct{})
		var once sync.Once
		out := writerFunc(func(p []byte) (int, error) {
			once.Do(func() { close(started) })
			return len(p), nil
		})

		type result struct {
			status int
			err    error
			took   time.Duration
		}
		results := make(chan result, 1)
		go func() {
			begin := time.Now()
			status, err := tr.Run(ctx, transport.Command{
				Path:   "sh",
				Args:   []string{"-c", "echo started; sleep 60"},
				Stdout: out,
			})
			results <- result{status: status, err: err, took: time.Since(begin)}
		}()

		select {
		case <-started:
		case <-time.After(testTimeout):
			t.Fatal("the command never produced output")
		}
		host.SetFaults(flintlock.HostFaults{Unresponsive: true})

		select {
		case r := <-results:
			if !errors.Is(r.err, transport.ErrStreamFailed) {
				t.Fatalf("Run returned (%d, %v), want a failure wrapping ErrStreamFailed", r.status, r.err)
			}
			if !strings.Contains(r.err.Error(), "h1") {
				t.Errorf("the failure %q does not name the host", r.err)
			}
			if r.took > 10*deadline {
				t.Errorf("Run took %s to give up on an unreachable host, want about %s", r.took, deadline)
			}
		case <-time.After(testTimeout):
			t.Fatal("Run never returned after the host stopped answering")
		}
	})

	t.Run("a host that stops answering while the script is being sent", func(t *testing.T) {
		// The Stage's script is larger than the stream's buffer and the
		// command never reads it, so the goroutine feeding standard input
		// is blocked in a Send when the Host stops answering. Run still has
		// to come back: an operation whose stdin cannot drain must not hold
		// the caller for ever.
		host, client, uid := newFakeHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
		const deadline = 500 * time.Millisecond
		tr := newExecTransport(t, transport.Target{Host: client, VMUID: uid, Deadline: deadline})

		script := strings.NewReader(strings.Repeat("# a line of a very long script\n", 400_000))
		done := make(chan error, 1)
		go func() {
			_, err := tr.Run(ctx, transport.Command{
				Path:  "sh",
				Args:  []string{"-c", "sleep 60"},
				Stdin: script,
			})
			done <- err
		}()
		time.Sleep(100 * time.Millisecond)
		host.SetFaults(flintlock.HostFaults{Unresponsive: true})

		select {
		case err := <-done:
			if !errors.Is(err, transport.ErrStreamFailed) {
				t.Fatalf("Run returned %v, want a failure wrapping ErrStreamFailed", err)
			}
		case <-time.After(testTimeout):
			t.Fatal("Run never returned while its standard input was stuck")
		}
	})

	t.Run("a quiet stage on a healthy host", func(t *testing.T) {
		_, client, uid := newFakeHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
		// The command says nothing for several deadlines, which is what a
		// compile or a quiet test suite does.
		tr := newExecTransport(t, transport.Target{Host: client, VMUID: uid, Deadline: 100 * time.Millisecond})
		var out bytes.Buffer
		status, err := tr.Run(ctx, transport.Command{
			Path:   "sh",
			Args:   []string{"-c", "sleep 1; echo done"},
			Stdout: &out,
		})
		if err != nil {
			t.Fatalf("Run killed a quiet stage on a healthy host: %v", err)
		}
		if status != 0 || !strings.Contains(out.String(), "done") {
			t.Errorf("Run returned (%d, %q), want (0, done)", status, out.String())
		}
	})
}

// TestRunTerminatesWhenTheContextIsCancelled checks that a cancelled
// operation comes back promptly rather than waiting for the command, which
// is what the Executor's graceful kill depends on.
func TestRunTerminatesWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()
	_, client, uid := newFakeHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	tr := newExecTransport(t, transport.Target{Host: client, VMUID: uid})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "sleep 60"}})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned no error for a cancelled operation")
		}
	case <-time.After(testTimeout):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

// Write implements io.Writer.
func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// stoppedClock is a Clock whose timers never fire, so that a fake Host's
// boot delay never elapses and its MicroVM stays PENDING.
type stoppedClock struct{}

// Now implements clock.Clock.
func (stoppedClock) Now() time.Time { return time.Unix(0, 0) }

// After implements clock.Clock.
func (stoppedClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

// NewTimer implements clock.Clock.
func (stoppedClock) NewTimer(time.Duration) clock.Timer { return stoppedTimer{} }

// stoppedTimer is a Timer that never fires.
type stoppedTimer struct{}

// C implements clock.Timer.
func (stoppedTimer) C() <-chan time.Time { return make(chan time.Time) }

// Stop implements clock.Timer.
func (stoppedTimer) Stop() bool { return true }

// Reset implements clock.Timer.
func (stoppedTimer) Reset(time.Duration) bool { return true }
