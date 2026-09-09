package fake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// execCase is one ExecCommand exchange and what it has to produce. The
// script may refer to $SANDBOX, which the test substitutes before sending.
type execCase struct {
	name  string
	start func(uid string) *execv1.ExecStart
	stdin []string
	// setup prepares the sandbox.
	setup func(t *testing.T, sandbox string)

	wantStdout func(sandbox string) string
	wantStderr string
	wantExit   int32
	// wantErrs are substrings expected in the error payloads, in order;
	// nil means no error payload.
	wantErrs []string
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL represent each MicroVM as a sandbox directory on the
//# local filesystem and SHALL run each `ExecCommand` as a local process
//# rooted in that directory, streaming standard input, standard output,
//# standard error and the exit code as `flintlockd` does.

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL honour the `cwd`, `env`, `timeout_seconds`,
//# `has_stdin` and `stdin_eof` fields of an exec request and SHALL accept
//# and ignore `user`.

// TestExecCommand runs the exec table over gRPC and over the in-memory
// stream: stdout, stderr, stdin and exit code framing (TD-021) and the
// cwd, env, has_stdin/stdin_eof, shell and user fields (TD-023).
func TestExecCommand(t *testing.T) {
	t.Parallel()
	uidStr := strconv.Itoa(os.Getuid())
	fixed := func(s string) func(string) string { return func(string) string { return s } }
	cases := []execCase{
		{
			name:       "stdout and exit code",
			start:      func(uid string) *execv1.ExecStart { return shell(uid, "printf hello; exit 3") },
			wantStdout: fixed("hello"),
			wantExit:   3,
		},
		{
			name:       "stderr",
			start:      func(uid string) *execv1.ExecStart { return shell(uid, "printf oops >&2") },
			wantStdout: fixed(""),
			wantStderr: "oops",
		},
		{
			name: "stdin chunks then stdin_eof",
			start: func(uid string) *execv1.ExecStart {
				return &execv1.ExecStart{Uid: uid, Cmd: "cat", HasStdin: true}
			},
			stdin:      []string{"line one\n", "line two\n"},
			wantStdout: fixed("line one\nline two\n"),
		},
		{
			name: "stdin_eof ends input for a reader",
			start: func(uid string) *execv1.ExecStart {
				return &execv1.ExecStart{Uid: uid, Cmd: "sh", Args: []string{"-c", "wc -c | tr -d ' '"}, HasStdin: true}
			},
			stdin:      []string{"12345"},
			wantStdout: fixed("5\n"),
		},
		{
			name:       "default cwd is the sandbox",
			start:      func(uid string) *execv1.ExecStart { return shell(uid, "pwd -P") },
			wantStdout: func(sandbox string) string { return realPath(sandbox) + "\n" },
		},
		{
			name: "relative cwd inside the sandbox",
			setup: func(t *testing.T, sandbox string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(sandbox, "sub"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			start: func(uid string) *execv1.ExecStart {
				s := shell(uid, "pwd -P")
				s.Cwd = "sub"
				return s
			},
			wantStdout: func(sandbox string) string { return realPath(filepath.Join(sandbox, "sub")) + "\n" },
		},
		{
			name: "absolute cwd is rooted at the sandbox",
			setup: func(t *testing.T, sandbox string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(sandbox, "builds", "proj"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			start: func(uid string) *execv1.ExecStart {
				s := shell(uid, "pwd -P")
				s.Cwd = "/builds/proj"
				return s
			},
			wantStdout: func(sandbox string) string { return realPath(filepath.Join(sandbox, "builds", "proj")) + "\n" },
		},
		{
			name: "cwd escaping the sandbox is refused",
			start: func(uid string) *execv1.ExecStart {
				s := shell(uid, "pwd")
				s.Cwd = "../../.."
				return s
			},
			wantStdout: fixed(""),
			wantErrs:   []string{"escapes the microvm sandbox"},
			wantExit:   exitStartError,
		},
		{
			name: "missing cwd fails to start",
			start: func(uid string) *execv1.ExecStart {
				s := shell(uid, "pwd")
				s.Cwd = "does-not-exist"
				return s
			},
			wantStdout: fixed(""),
			wantErrs:   []string{"does-not-exist"},
			wantExit:   exitStartError,
		},
		{
			name: "env is added and overrides",
			start: func(uid string) *execv1.ExecStart {
				s := shell(uid, `printf '%s|%s' "$FAKE_ONLY" "$HOME"`)
				s.Env = map[string]string{"FAKE_ONLY": "yes", "HOME": "/overridden"}
				return s
			},
			wantStdout: fixed("yes|/overridden"),
		},
		{
			name: "inherited env stays available",
			start: func(uid string) *execv1.ExecStart {
				return shell(uid, `test -n "$PATH" && printf ok`)
			},
			wantStdout: fixed("ok"),
		},
		{
			name: "shell mode joins cmd and args",
			start: func(uid string) *execv1.ExecStart {
				return &execv1.ExecStart{Uid: uid, Shell: true, Cmd: "printf", Args: []string{"%s-%s", "a", "b"}}
			},
			wantStdout: fixed("a-b"),
		},
		{
			name: "shell mode with only cmd",
			start: func(uid string) *execv1.ExecStart {
				return &execv1.ExecStart{Uid: uid, Shell: true, Cmd: "exit 42"}
			},
			wantStdout: fixed(""),
			wantExit:   42,
		},
		{
			name: "user is accepted and ignored",
			start: func(uid string) *execv1.ExecStart {
				s := shell(uid, "id -u")
				s.User = "nobody"
				return s
			},
			wantStdout: fixed(uidStr + "\n"),
		},
		{
			name: "command not found",
			start: func(uid string) *execv1.ExecStart {
				return &execv1.ExecStart{Uid: uid, Cmd: "definitely-not-a-command-fake-host"}
			},
			wantStdout: fixed(""),
			wantErrs:   []string{"definitely-not-a-command-fake-host"},
			wantExit:   exitNotFound,
		},
		{
			name: "output is streamed in order across sizes",
			start: func(uid string) *execv1.ExecStart {
				return shell(uid, "head -c 200000 /dev/zero | tr '\\0' x; printf end")
			},
			wantStdout: fixed(strings.Repeat("x", 200000) + "end"),
		},
		{
			name: "files in the sandbox are visible",
			setup: func(t *testing.T, sandbox string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(sandbox, "seed.txt"), []byte("seeded"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			start:      func(uid string) *execv1.ExecStart { return shell(uid, "cat seed.txt && touch made.txt") },
			wantStdout: fixed("seeded"),
		},
	}

	for transport, open := range execTransports {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			h := newTestHost(t, flintlock.FakeHostConfig{ExecEnabled: true})
			opener := open(t, h)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					vm := createVM(t, h, nil)
					uid := vm.GetSpec().GetUid()
					sandbox, _ := h.SandboxPath(uid)
					if tc.setup != nil {
						tc.setup(t, sandbox)
					}
					res := runExec(t, opener, tc.start(uid), tc.stdin...)
					if res.err != nil {
						t.Fatalf("stream error: %v", res.err)
					}
					if got, want := res.stdout.String(), tc.wantStdout(sandbox); got != want {
						t.Errorf("stdout = %q, want %q", truncate(got), truncate(want))
					}
					if got := res.stderr.String(); got != tc.wantStderr {
						t.Errorf("stderr = %q, want %q", got, tc.wantStderr)
					}
					if got := exitCode(t, res); got != tc.wantExit {
						t.Errorf("exit code = %d, want %d", got, tc.wantExit)
					}
					if len(res.errs) != len(tc.wantErrs) {
						t.Fatalf("error payloads = %q, want %d matching %q", res.errs, len(tc.wantErrs), tc.wantErrs)
					}
					for i, want := range tc.wantErrs {
						if !strings.Contains(res.errs[i], want) {
							t.Errorf("error payload %d = %q, want it to contain %q", i, res.errs[i], want)
						}
					}
					if tc.name == "files in the sandbox are visible" {
						if _, err := os.Stat(filepath.Join(sandbox, "made.txt")); err != nil {
							t.Errorf("file made by the command not in the sandbox: %v", err)
						}
					}
				})
			}
		})
	}
}

// TestExecRejectsBadStreams: the flintlockd rules for the first message and
// the MicroVM's state, over both transports.
func TestExecRejectsBadStreams(t *testing.T) {
	t.Parallel()
	for transport, open := range execTransports {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			clk := newFakeClock()
			h := newTestHost(t, flintlock.FakeHostConfig{BootDelay: time.Hour}, WithClock(clk))
			opener := open(t, h)
			pending := createVM(t, h, nil).GetSpec().GetUid()

			tests := []struct {
				name string
				send func(s flintlock.ExecStream)
				want codes.Code
				// wantIs is the sentinel the in-memory stream has to wrap.
				wantIs error
			}{
				{
					name: "stdin before start",
					send: func(s flintlock.ExecStream) {
						_ = s.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Stdin{Stdin: []byte("x")}})
					},
					want: codes.InvalidArgument,
				},
				{
					name: "start without uid",
					send: func(s flintlock.ExecStream) { sendStart(s, &execv1.ExecStart{Cmd: "true"}) },
					want: codes.InvalidArgument,
				},
				{
					name: "start without cmd or shell",
					send: func(s flintlock.ExecStream) { sendStart(s, &execv1.ExecStart{Uid: pending}) },
					want: codes.InvalidArgument,
				},
				{
					name:   "unknown uid",
					send:   func(s flintlock.ExecStream) { sendStart(s, shell("missing", "true")) },
					want:   codes.NotFound,
					wantIs: flintlock.ErrNotFound,
				},
				{
					name: "microvm still pending",
					send: func(s flintlock.ExecStream) { sendStart(s, shell(pending, "true")) },
					want: codes.FailedPrecondition,
				},
				{
					name: "client half-closes without a start",
					send: func(s flintlock.ExecStream) { _ = s.CloseSend() },
					want: codes.Unknown,
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					stream, err := opener(testCtx(t))
					if err != nil {
						t.Fatalf("open: %v", err)
					}
					tt.send(stream)
					res := &execResult{}
					drain(stream, res)
					if res.err == nil {
						t.Fatalf("stream ended cleanly, want an error; exit=%v", res.exit)
					}
					if transport == "grpc" {
						if status.Code(res.err) != tt.want {
							t.Errorf("code = %s (%v), want %s", status.Code(res.err), res.err, tt.want)
						}
					} else if tt.wantIs != nil && !errors.Is(res.err, tt.wantIs) {
						t.Errorf("error = %v, want %v", res.err, tt.wantIs)
					}
				})
			}
		})
	}
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL honour the `cwd`, `env`, `timeout_seconds`,
//# `has_stdin` and `stdin_eof` fields of an exec request and SHALL accept
//# and ignore `user`.

// TestExecTimeout: timeout_seconds is enforced on the Host's clock; the
// process group is killed and the client gets an error payload followed by
// the exit code.
func TestExecTimeout(t *testing.T) {
	t.Parallel()
	for transport, open := range execTransports {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			clk := newFakeClock()
			h := newTestHost(t, flintlock.FakeHostConfig{}, WithClock(clk))
			opener := open(t, h)
			uid := createVM(t, h, nil).GetSpec().GetUid()

			stream, err := opener(testCtx(t))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			start := shell(uid, "echo $$; sleep 300")
			start.TimeoutSeconds = 7
			sendStart(stream, start)
			res := &execResult{}
			waitForStdout(t, stream, res, "\n")

			// The timer is armed before the process starts and the first
			// stdout chunk can only come from a started process, so by now
			// the clock holds the timer and one advance past the deadline
			// fires it. Anything less relies on an ordering that the copier
			// goroutine carrying stdout does not provide.
			clk.Advance(7 * time.Second)
			drain(stream, res)
			if res.err != nil {
				t.Fatalf("stream error: %v", res.err)
			}
			if len(res.errs) != 1 || !strings.Contains(res.errs[0], "timed out after 7s") {
				t.Errorf("error payloads = %q, want one timeout message", res.errs)
			}
			if got := exitCode(t, res); got != -1 {
				t.Errorf("exit code = %d, want -1 for a killed process", got)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(res.stdout.String()))
			if err != nil {
				t.Fatalf("pid from %q: %v", res.stdout.String(), err)
			}
			if err := processGone(pid); err != nil {
				t.Errorf("shell %d after timeout: %v", pid, err)
			}
		})
	}
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL support fault injection for a create that ends in
//# `FAILED`, an exec stream dropped before the exit code, and a Host that
//# stops answering.

// TestExecDroppedBeforeExit: the DropExecBeforeExit fault forwards all the
// output and then ends the stream with an error instead of an exit code.
func TestExecDroppedBeforeExit(t *testing.T) {
	t.Parallel()
	for transport, open := range execTransports {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			h := newTestHost(t, flintlock.FakeHostConfig{})
			opener := open(t, h)
			uid := createVM(t, h, nil).GetSpec().GetUid()
			h.SetFaults(flintlock.HostFaults{DropExecBeforeExit: true})

			res := runExec(t, opener, shell(uid, "printf out; printf err >&2; exit 5"))
			if res.stdout.String() != "out" || res.stderr.String() != "err" {
				t.Errorf("output before the drop: stdout=%q stderr=%q", res.stdout.String(), res.stderr.String())
			}
			if res.exit != nil {
				t.Errorf("exit code %d received, want the stream dropped first", *res.exit)
			}
			if res.err == nil {
				t.Fatal("stream ended with EOF, want a non-EOF error")
			}
			if transport == "grpc" {
				if status.Code(res.err) != codes.Unavailable {
					t.Errorf("code = %s, want Unavailable", status.Code(res.err))
				}
			} else if !errors.Is(res.err, flintlock.ErrUnavailable) {
				t.Errorf("error = %v, want ErrUnavailable", res.err)
			}

			h.SetFaults(flintlock.HostFaults{})
			res = runExec(t, opener, shell(uid, "exit 5"))
			if res.err != nil || exitCode(t, res) != 5 {
				t.Errorf("after clearing the fault: err=%v exit=%v", res.err, res.exit)
			}
		})
	}
}

// TestDeleteKillsRunningExec: DeleteMicroVM kills the MicroVM's processes
// and the stream reports why.
func TestDeleteKillsRunningExec(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	c := h.Client()
	uid := createVM(t, h, nil).GetSpec().GetUid()

	stream, err := c.Exec(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	sendStart(stream, shell(uid, "echo $$; sleep 300"))
	res := &execResult{}
	waitForStdout(t, stream, res, "\n")
	pid, _ := strconv.Atoi(strings.TrimSpace(res.stdout.String()))

	if err := c.DeleteMicroVM(testCtx(t), uid); err != nil {
		t.Fatalf("DeleteMicroVM: %v", err)
	}
	drain(stream, res)
	if res.err != nil {
		t.Fatalf("stream error: %v", res.err)
	}
	if len(res.errs) != 1 || !strings.Contains(res.errs[0], "deleted") {
		t.Errorf("error payloads = %q, want one about deletion", res.errs)
	}
	if exitCode(t, res) != -1 {
		t.Errorf("exit code = %d, want -1", *res.exit)
	}
	if err := processGone(pid); err != nil {
		t.Error(err)
	}
	if left, _ := h.Sandboxes(); len(left) != 0 {
		t.Errorf("sandboxes after delete = %v", left)
	}
}

// TestExecCancelledByClient: cancelling the client's context kills the
// process; the in-memory stream reports the cancellation.
func TestExecCancelledByClient(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	uid := createVM(t, h, nil).GetSpec().GetUid()
	ctx, cancel := context.WithCancel(testCtx(t))
	stream, err := h.Client().Exec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sendStart(stream, shell(uid, "echo $$; sleep 300"))
	res := &execResult{}
	waitForStdout(t, stream, res, "\n")
	pid, _ := strconv.Atoi(strings.TrimSpace(res.stdout.String()))
	cancel()
	drain(stream, res)
	if res.err == nil || res.exit != nil {
		t.Errorf("after cancel: err=%v exit=%v; want an error and no exit code", res.err, res.exit)
	}
	// Close waits for the handler, which waits for the process.
	_ = h.Close()
	if err := processGone(pid); err != nil {
		t.Error(err)
	}
}

// TestSandboxDir pins the cwd resolution rules.
func TestSandboxDir(t *testing.T) {
	t.Parallel()
	const sb = "/root/sb"
	tests := []struct {
		cwd     string
		want    string
		wantErr bool
	}{
		{cwd: "", want: sb},
		{cwd: ".", want: sb},
		{cwd: "/", want: sb},
		{cwd: "sub", want: sb + "/sub"},
		{cwd: "/sub/deeper", want: sb + "/sub/deeper"},
		{cwd: "sub/../other", want: sb + "/other"},
		{cwd: "..", wantErr: true},
		{cwd: "/../etc", want: sb + "/etc"},
		{cwd: "sub/../../x", wantErr: true},
	}
	for _, tt := range tests {
		got, err := sandboxDir(sb, tt.cwd)
		if (err != nil) != tt.wantErr {
			t.Errorf("sandboxDir(%q) err = %v, wantErr %v", tt.cwd, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("sandboxDir(%q) = %q, want %q", tt.cwd, got, tt.want)
		}
	}
}

// TestMergeEnv pins the environment overlay.
func TestMergeEnv(t *testing.T) {
	t.Parallel()
	got := mergeEnv([]string{"A=1", "B=2", "C=3"}, map[string]string{"B": "x", "D": "4"})
	want := []string{"A=1", "C=3", "B=x", "D=4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("mergeEnv = %v, want %v", got, want)
	}
	base := []string{"A=1"}
	if got := mergeEnv(base, nil); &got[0] != &base[0] {
		t.Error("mergeEnv with no extras should return base unchanged")
	}
}

// realPath resolves symlinks so that pwd -P output compares equal.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL represent each MicroVM as a sandbox directory on the
//# local filesystem and SHALL run each `ExecCommand` as a local process
//# rooted in that directory, streaming standard input, standard output,
//# standard error and the exit code as `flintlockd` does.

// TestShutdownWithIdleExecStream: a stream that is opened and never spoken
// on does not hold the Host open (TD-021). In memory the handler behind it
// is counted by Close's WaitGroup, so a receive the Host cannot interrupt
// hangs Close, and with it Serve; over gRPC the same handler costs
// GracefulStop its whole timeout. Both have to end promptly.
func TestShutdownWithIdleExecStream(t *testing.T) {
	t.Parallel()
	// Under gracefulStopTimeout, so that a Serve which waits the graceful
	// stop out fails here rather than passing slowly.
	const promptly = gracefulStopTimeout / 2

	t.Run("memory", func(t *testing.T) {
		t.Parallel()
		// Built without newTestHost's cleanup, because a Close that hangs
		// has to fail this test rather than the package's teardown.
		h := New(flintlock.FakeHostConfig{Name: "h1", SandboxRoot: t.TempDir()})
		c := h.Client()
		t.Cleanup(func() { _ = c.Close() })
		if _, err := c.Exec(testCtx(t)); err != nil {
			t.Fatalf("Exec: %v", err)
		}

		closed := make(chan error, 1)
		go func() { closed <- h.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(promptly):
			t.Fatalf("Close did not return within %s with an idle exec stream open", promptly)
		}
	})

	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		h := newTestHost(t, flintlock.FakeHostConfig{})
		stop := serveHost(t, h)
		client := execv1.NewMicroVMExecClient(dialHost(t, h, nil))

		if _, err := client.ExecCommand(testCtx(t)); err != nil {
			t.Fatalf("opening the idle stream: %v", err)
		}
		// A second stream that does speak. Its headers were written after
		// the idle stream's and the connection delivers frames in order, so
		// its first output proves the idle handler is already parked in its
		// first receive.
		busy, err := client.ExecCommand(testCtx(t))
		if err != nil {
			t.Fatalf("opening the busy stream: %v", err)
		}
		uid := createVM(t, h, nil).GetSpec().GetUid()
		sendStart(busy, shell(uid, "echo ready; sleep 300"))
		waitForStdout(t, busy, &execResult{}, "ready")

		started := time.Now()
		if err := stop(); err != nil {
			t.Fatalf("Serve: %v", err)
		}
		if elapsed := time.Since(started); elapsed > promptly {
			t.Errorf("Serve took %s to stop with an idle exec stream open, want under %s", elapsed, promptly)
		}
	})
}

// truncate shortens long strings in failure messages.
func truncate(s string) string {
	if len(s) > 80 {
		return s[:40] + "..." + s[len(s)-40:]
	}
	return s
}

// processGone reports an error while pid still exists.
func processGone(pid int) error {
	if pid <= 0 {
		return errors.New("no pid captured")
	}
	// Signal 0 checks for the process without touching it; ESRCH means the
	// fake Host reaped it.
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return errors.New("process " + strconv.Itoa(pid) + " still exists")
}
