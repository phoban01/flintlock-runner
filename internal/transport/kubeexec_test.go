package transport_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"k8s.io/client-go/rest"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/transport"
	"github.com/phoban01/flintlock-runner/internal/transport/kubeexectest"
)

// The kube-exec tests run the transport against a real kube-apiserver and
// the real Pod Provider in front of the fake Host: the transport is given
// the Runner's client configuration and nothing else, so every session goes
// from it to the API server, which checks the Runner's shipped RBAC, then
// from the API server over its kubelet client certificate to the provider's
// TLS kubelet endpoint, and from there to MicroVMExec on the fake Host.
// kubeexectest says how the API server is set up to make that last hop.
// Without the envtest binaries the tests skip, or fail where
// FLINTLOCK_RUNNER_REQUIRE_ENVTEST is set, as it is in CI.

// kubeEnv is the shared API server, nil when there is none.
var kubeEnv *kubeexectest.Env

func TestMain(m *testing.M) {
	env, err := kubeexectest.Start()
	switch {
	case errors.Is(err, kubeexectest.ErrNoAssets):
		// The kube-exec tests skip; the rest need no API server.
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	default:
		kubeEnv = env
	}
	code := m.Run()
	if kubeEnv != nil {
		_ = kubeEnv.Stop()
	}
	os.Exit(code)
}

// kubeFixture is one Host with its Pod Provider, a namespace standing for
// the Runner's and the Runner's client configuration for it.
type kubeFixture struct {
	t         *testing.T
	host      *kubeexectest.Host
	namespace string
	runner    *rest.Config
}

func newKubeFixture(t *testing.T, opts kubeexectest.HostOptions) *kubeFixture {
	t.Helper()
	if kubeEnv == nil {
		t.Skip(kubeexectest.ErrNoAssets)
	}
	ns := kubeEnv.Namespace(t)
	return &kubeFixture{
		t:         t,
		host:      kubeEnv.NewHost(t, opts),
		namespace: ns,
		runner:    kubeEnv.RunnerConfig(t, ns),
	}
}

// protocols are the two ways a session is opened: WebSocket, which is tried
// first, and SPDY, which is what the fallback takes.
var protocols = []struct {
	name string
	opts []transport.FactoryOption
}{
	{name: "websocket"},
	{name: "spdy", opts: []transport.FactoryOption{transport.WithSPDYOnly()}},
}

// transport builds the kube-exec transport for a pod, with the Runner's
// client configuration and extra options after WithKubeExec.
func (f *kubeFixture) transport(pod string, extra ...transport.FactoryOption) transport.Transport {
	f.t.Helper()
	opts := append([]transport.FactoryOption{transport.WithKubeExec(f.runner, f.namespace)}, extra...)
	tr, err := transport.NewFactory(opts...).New(context.Background(), transport.Target{Kind: transport.KindKubeExec, VMUID: pod})
	if err != nil {
		f.t.Fatalf("building the kube-exec transport: %v", err)
	}
	f.t.Cleanup(func() { _ = tr.Close() })
	return tr
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# Where the `kube-exec` Guest Transport is configured, the
//# Executor SHALL run each Stage by opening the exec subresource of the
//# claimed pod through the Kubernetes API, with the Stage script on standard
//# input.

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# The `kube-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// TestKubeExecRunsAStageThroughThePod is the exec transport's
// TestRunCarriesEveryPartOfTheCommand over kube-exec: a Stage script on
// standard input, run in a working directory with an environment and a
// user, writes to both output streams and exits with a status of its own
// choosing, and each part arrives. A script far larger than one frame of
// either protocol runs to its last line, so standard input is delivered
// whole and then closed. The transport is given the API server's address
// and nothing else, so the session went through it and the Pod Provider.
func TestKubeExecRunsAStageThroughThePod(t *testing.T) {
	t.Parallel()
	f := newKubeFixture(t, kubeexectest.HostOptions{})
	pod := f.host.Pod(f.namespace, "job")
	sandbox := f.host.Sandbox(pod)
	if err := os.MkdirAll(filepath.Join(sandbox, "builds", "project"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			tr := f.transport(pod.Name, p.opts...)
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()

			var script strings.Builder
			script.WriteString("pwd\necho $CI_JOB_STAGE\necho to-stderr >&2\n")
			for script.Len() < 256*1024 {
				script.WriteString(": padding the script past a frame of either protocol\n")
			}
			script.WriteString("echo last-line\nexit 7\n")

			var stdout, stderr bytes.Buffer
			status, err := tr.Run(ctx, transport.Command{
				Path:   "sh",
				Dir:    "/builds/project",
				Env:    map[string]string{"CI_JOB_STAGE": "build"},
				User:   "root",
				Stdin:  strings.NewReader(script.String()),
				Stdout: &stdout,
				Stderr: &stderr,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if status != 7 {
				t.Errorf("Run returned exit status %d, want 7", status)
			}
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) != 3 {
				t.Fatalf("stdout was %q, want the directory, the variable and the last line", stdout.String())
			}
			if want := filepath.Join(sandbox, "builds", "project"); !sameDir(lines[0], want) {
				t.Errorf("the stage ran in %q, want %q, the builds directory of the guest", lines[0], want)
			}
			if lines[1] != "build" {
				t.Errorf("the environment did not reach the guest: %q", lines[1])
			}
			if lines[2] != "last-line" {
				t.Errorf("the script on standard input did not run to its end: %q", lines[2])
			}
			if got := stderr.String(); !strings.Contains(got, "to-stderr") {
				t.Errorf("standard error was %q, want the command's error output", got)
			}
		})
	}
}

// sameDir compares two directories after resolving symbolic links, since
// the sandbox may sit under one.
func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// TestKubeExecRunsAsTheStageUser checks that a Stage user other than root is
// taken on in the guest with runuser. The fake Host runs everything as the
// test's own user, so a runuser of the test's making on the fake Host's
// PATH stands in for the guest's, and says whom it was asked to run as.
// It changes the process environment and so does not run in parallel.
func TestKubeExecRunsAsTheStageUser(t *testing.T) {
	f := newKubeFixture(t, kubeexectest.HostOptions{})
	bin := t.TempDir()
	runuser := "#!/bin/sh\n[ \"$1\" = -u ] || exit 90\necho \"runuser as $2\"\nshift 3\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "runuser"), []byte(runuser), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	pod := f.host.Pod(f.namespace, "job")

	tr := f.transport(pod.Name)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	for user, want := range map[string]string{"builder": "runuser as builder\nran\n", "root": "ran\n", "": "ran\n"} {
		var stdout bytes.Buffer
		status, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "echo ran"}, User: user, Stdout: &stdout})
		if err != nil || status != 0 {
			t.Fatalf("user %q: Run = (%d, %v)", user, status, err)
		}
		if stdout.String() != want {
			t.Errorf("user %q: stdout = %q, want %q", user, stdout.String(), want)
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# The `kube-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// TestKubeExecStreamsOutputAsItIsProduced checks EX-021 over kube-exec: the
// Stage prints a line and then waits for the test to answer it, which the
// test does only once that line has reached its writer. Output that
// arrived only when the Stage ended would leave both waiting until the
// context ran out.
func TestKubeExecStreamsOutputAsItIsProduced(t *testing.T) {
	t.Parallel()
	f := newKubeFixture(t, kubeexectest.HostOptions{})
	pod := f.host.Pod(f.namespace, "job")
	sandbox := f.host.Sandbox(pod)

	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			tr := f.transport(pod.Name, p.opts...)
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			answer := filepath.Join(sandbox, "answer-"+p.name)

			seen := make(chan struct{})
			var once sync.Once
			var out lockedBuffer
			stdout := writerFunc(func(b []byte) (int, error) {
				n, err := out.Write(b)
				if strings.Contains(out.String(), "first") {
					once.Do(func() { close(seen) })
				}
				return n, err
			})
			go func() {
				select {
				case <-seen:
					_ = os.WriteFile(answer, nil, 0o644)
				case <-ctx.Done():
				}
			}()

			script := fmt.Sprintf("echo first\nwhile [ ! -e %s ]; do sleep 0.05; done\necho second\n", filepath.Base(answer))
			status, err := tr.Run(ctx, transport.Command{Path: "sh", Dir: "/", Stdin: strings.NewReader(script), Stdout: stdout})
			if err != nil {
				t.Fatalf("Run: %v (stdout so far %q)", err, out.String())
			}
			if status != 0 || out.String() != "first\nsecond\n" {
				t.Errorf("Run = (%d, %q), want (0, first and second)", status, out.String())
			}
		})
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# The `kube-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// TestKubeExecExitStatus is the exec transport's
// TestExitCodeIsTheStatusAndAnErrorWithoutOneFails over kube-exec. A command
// that ran reports its exit status, zero or not, with no error (EX-022,
// EX-045). A session that ends without one leaves the status unknown and
// is a stream failure the Executor never re-runs (EX-023): a MicroVMExec
// stream the Host drops before the exit code, after output has already
// been relayed, and an exec in a pod that is not there.
func TestKubeExecExitStatus(t *testing.T) {
	t.Parallel()
	f := newKubeFixture(t, kubeexectest.HostOptions{})
	pod := f.host.Pod(f.namespace, "job")

	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			tr := f.transport(pod.Name, p.opts...)
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			for _, code := range []int{0, 1, 2, 42, 255} {
				status, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "exit " + strconv.Itoa(code)}})
				if err != nil {
					t.Fatalf("exit %d: Run returned error %v, want the status", code, err)
				}
				if status != code {
					t.Errorf("exit %d: Run returned status %d", code, status)
				}
			}

			missing := f.transport("no-such-pod", p.opts...)
			if status, err := missing.Run(ctx, transport.Command{Path: "true"}); !errors.Is(err, transport.ErrStreamFailed) || status != -1 {
				t.Errorf("exec in a missing pod = (%d, %v), want a stream failure", status, err)
			}
		})
	}

	t.Run("a stream dropped before the exit code", func(t *testing.T) {
		f.host.Fake.SetFaults(flintlock.HostFaults{DropExecBeforeExit: true})
		defer f.host.Fake.SetFaults(flintlock.HostFaults{})
		tr := f.transport(pod.Name)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		status, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "echo partial; exit 0"}})
		if !errors.Is(err, transport.ErrStreamFailed) {
			t.Fatalf("Run = (%d, %v), want a stream failure", status, err)
		}
		if status != -1 {
			t.Errorf("status = %d with the exit code never sent, want -1", status)
		}
	})
}

// TestKubeExecReady is the exec transport's
// TestReadySucceedsOnlyWhenAGuestCanRunSomething over kube-exec: a running
// pod is ready, and a pod whose MicroVM is still booting, which the Pod
// Provider refuses exec into, or a pod that does not exist, is not.
func TestKubeExecReady(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	f := newKubeFixture(t, kubeexectest.HostOptions{})
	pod := f.host.Pod(f.namespace, "job")
	if err := f.transport(pod.Name).Ready(ctx); err != nil {
		t.Errorf("Ready on a running pod: %v", err)
	}
	if err := f.transport("no-such-pod").Ready(ctx); !errors.Is(err, transport.ErrNotReady) {
		t.Errorf("Ready on a missing pod = %v, want ErrNotReady", err)
	}

	booting := newKubeFixture(t, kubeexectest.HostOptions{BootDelay: time.Hour})
	slow := booting.host.CreatePod(booting.namespace, "booting")
	if err := booting.transport(slow.Name).Ready(ctx); !errors.Is(err, transport.ErrNotReady) {
		t.Errorf("Ready on a pod whose microvm is booting = %v, want ErrNotReady", err)
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# The `kube-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// TestKubeExecCancellationAndTimeout checks EX-024 and EX-046 over kube-exec.
// A Stage that would sleep for a minute is stopped by its context being
// cancelled, and by its deadline passing: Run returns promptly with a
// stream failure naming the context's cause, and the process in the guest
// is gone, which is the Pod Provider cancelling its MicroVMExec exchange
// once its side of the session closed.
func TestKubeExecCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	f := newKubeFixture(t, kubeexectest.HostOptions{})
	pod := f.host.Pod(f.namespace, "job")
	sandbox := f.host.Sandbox(pod)

	for _, p := range protocols {
		for _, how := range []string{"cancelled", "deadline"} {
			t.Run(p.name+" "+how, func(t *testing.T) {
				tr := f.transport(pod.Name, p.opts...)
				pidFile := "pid-" + p.name + "-" + how

				var ctx context.Context
				var cancel context.CancelFunc
				if how == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				defer cancel()

				started := make(chan struct{})
				var once sync.Once
				stdout := writerFunc(func(b []byte) (int, error) {
					if strings.Contains(string(b), "started") {
						once.Do(func() { close(started) })
					}
					return len(b), nil
				})
				result := make(chan error, 1)
				begun := time.Now()
				go func() {
					script := fmt.Sprintf("echo $$ > %s\necho started\nexec sleep 60\n", pidFile)
					_, err := tr.Run(ctx, transport.Command{Path: "sh", Dir: "/", Stdin: strings.NewReader(script), Stdout: stdout})
					result <- err
				}()
				select {
				case <-started:
				case <-time.After(testTimeout):
					t.Fatal("the stage never started")
				}
				if how == "cancelled" {
					cancel()
				}

				var err error
				select {
				case err = <-result:
				case <-time.After(testTimeout):
					t.Fatal("Run did not return after its context ended")
				}
				if elapsed := time.Since(begun); elapsed > 20*time.Second {
					t.Errorf("Run took %s to return, want it soon after the context ended", elapsed)
				}
				if !errors.Is(err, transport.ErrStreamFailed) {
					t.Errorf("Run returned %v, want a stream failure", err)
				}
				want := context.Canceled
				if how == "deadline" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) {
					t.Errorf("Run returned %v, want it to carry %v", err, want)
				}

				pid := readPid(t, filepath.Join(sandbox, pidFile))
				deadline := time.Now().Add(testTimeout)
				for syscall.Kill(pid, 0) == nil {
					if time.Now().After(deadline) {
						t.Fatalf("the stage's process %d is still running in the guest", pid)
					}
					time.Sleep(50 * time.Millisecond)
				}
			})
		}
	}
}

// readPid reads the process id a Stage wrote.
func readPid(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the stage's pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("the stage wrote pid %q", data)
	}
	return pid
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# Where the `kube-exec` Guest Transport is configured, the Runner
//# SHALL NOT open any connection to a Host.

// TestKubeExecReachesNoHost builds and uses a kube-exec transport whose
// Target also carries a Host client and a transport deadline, the two
// things that make the exec transport talk to a Host: the Host fails the
// test on any call, and so does a Host dialer handed to nothing. The API
// server here is an address nothing listens on, so the operations fail,
// but they fail without a word to the Host. It needs no API server.
func TestKubeExecReachesNoHost(t *testing.T) {
	t.Parallel()
	host := &tripwireHost{t: t}
	f := transport.NewFactory(transport.WithKubeExec(&rest.Config{Host: "https://127.0.0.1:1"}, "runners"))
	tr, err := f.New(context.Background(), transport.Target{
		Kind: transport.KindKubeExec, VMUID: "pool-default-abcde", Host: host, Deadline: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := tr.Ready(ctx); !errors.Is(err, transport.ErrNotReady) {
		t.Errorf("Ready with no API server = %v, want ErrNotReady", err)
	}
	if _, err := tr.Run(ctx, transport.Command{Path: "true"}); !errors.Is(err, transport.ErrStreamFailed) {
		t.Errorf("Run with no API server = %v, want a stream failure", err)
	}
	_ = tr.Close()
}

// TestKubeExecNeedsItsConfiguration checks that kube-exec is refused where
// the Runner was not given a Kubernetes client for it, and for a Target
// that names no pod.
func TestKubeExecNeedsItsConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := transport.NewFactory().New(ctx, transport.Target{Kind: transport.KindKubeExec, VMUID: "pod"}); err == nil {
		t.Error("kube-exec was built with no Kubernetes client configuration")
	}
	f := transport.NewFactory(transport.WithKubeExec(&rest.Config{Host: "https://127.0.0.1:1"}, "runners"))
	if _, err := f.New(ctx, transport.Target{Kind: transport.KindKubeExec}); err == nil {
		t.Error("kube-exec was built for a target that names no pod")
	}
	if _, err := transport.NewFactory(transport.WithKubeExec(&rest.Config{Host: "https://127.0.0.1:1"}, "")).
		New(ctx, transport.Target{Kind: transport.KindKubeExec, VMUID: "pod"}); err == nil {
		t.Error("kube-exec was built with no namespace")
	}
}

// tripwireHost is a Host client that fails the test on every call.
type tripwireHost struct{ t *testing.T }

func (h *tripwireHost) called(what string) {
	h.t.Helper()
	h.t.Errorf("the kube-exec transport called %s on a Host", what)
}

// Name implements flintlock.HostClient.
func (h *tripwireHost) Name() string { h.called("Name"); return "tripwire" }

// ServerInfo implements flintlock.HostClient.
func (h *tripwireHost) ServerInfo(context.Context) (*flintlock.HostInfo, error) {
	h.called("ServerInfo")
	return nil, errors.New("tripwire")
}

// GetMicroVM implements flintlock.HostClient.
func (h *tripwireHost) GetMicroVM(context.Context, string) (*types.MicroVM, error) {
	h.called("GetMicroVM")
	return nil, errors.New("tripwire")
}

// ListMicroVMs implements flintlock.HostClient.
func (h *tripwireHost) ListMicroVMs(context.Context, string) ([]*types.MicroVM, error) {
	h.called("ListMicroVMs")
	return nil, errors.New("tripwire")
}

// Exec implements flintlock.HostClient.
func (h *tripwireHost) Exec(context.Context) (flintlock.ExecStream, error) {
	h.called("Exec")
	return nil, errors.New("tripwire")
}

// SSHProxy implements flintlock.HostClient.
func (h *tripwireHost) SSHProxy(context.Context, string) (io.ReadWriteCloser, error) {
	h.called("SSHProxy")
	return nil, errors.New("tripwire")
}

// Close implements flintlock.HostClient.
func (h *tripwireHost) Close() error { h.called("Close"); return nil }

// lockedBuffer is a buffer safe to read while the transport writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
