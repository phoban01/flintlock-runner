package fake

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// testTimeout bounds every wait in these tests; a hit means a hang, not a
// slow machine.
const testTimeout = 30 * time.Second

// newTestHost builds a Host with a name and a private sandbox root and
// closes it when the test ends.
func newTestHost(t *testing.T, cfg flintlock.FakeHostConfig, opts ...Option) *Host {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "h1"
	}
	if cfg.SandboxRoot == "" {
		cfg.SandboxRoot = t.TempDir()
	}
	h := New(cfg, opts...)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// serveHost runs Serve in the background until the test ends or the
// returned stop function is called, which returns Serve's result.
func serveHost(t *testing.T, h *Host) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- h.Serve(ctx) }()
	select {
	case <-h.Ready():
	case err := <-errc:
		cancel()
		t.Fatalf("Serve exited before listening: %v", err)
	case <-time.After(testTimeout):
		cancel()
		t.Fatal("Serve did not start listening")
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-errc:
			case <-time.After(testTimeout):
				result = errors.New("Serve did not return after cancel")
			}
		})
		return result
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("Serve returned %v", err)
		}
	})
	return stop
}

// dialHost opens a gRPC connection to a serving Host.
func dialHost(t *testing.T, h *Host, creds credentials.TransportCredentials, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, opts...)
	conn, err := grpc.NewClient(h.Addr(), opts...)
	if err != nil {
		t.Fatalf("NewClient(%s): %v", h.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// testCtx is a context bounded by testTimeout and the test's lifetime.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// createVM creates a MicroVM through the in-process client and fails the
// test on error. With BootDelay zero it is CREATED on return.
func createVM(t *testing.T, h *Host, spec *types.MicroVMSpec) *types.MicroVM {
	t.Helper()
	if spec == nil {
		spec = &types.MicroVMSpec{Id: "vm", Namespace: "ns", AllowGuestAgent: true}
	}
	vm, err := h.Client().CreateMicroVM(testCtx(t), spec)
	if err != nil {
		t.Fatalf("CreateMicroVM: %v", err)
	}
	return vm
}

// execOpener opens an exec stream against a Host over some transport.
type execOpener func(ctx context.Context) (flintlock.ExecStream, error)

// execTransports are the two ways to reach the exec handler. Every exec
// test runs against both so that the in-memory stream cannot drift from
// the gRPC one.
var execTransports = map[string]func(t *testing.T, h *Host) execOpener{
	"grpc": func(t *testing.T, h *Host) execOpener {
		t.Helper()
		serveHost(t, h)
		conn := dialHost(t, h, nil)
		return func(ctx context.Context) (flintlock.ExecStream, error) {
			return execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
		}
	},
	"memory": func(t *testing.T, h *Host) execOpener {
		t.Helper()
		c := h.Client()
		t.Cleanup(func() { _ = c.Close() })
		return c.Exec
	},
}

// execResult is everything one exec exchange produced, in order.
type execResult struct {
	stdout, stderr strings.Builder
	errs           []string
	exit           *int32
	// err is the non-EOF error Recv ended with, nil after a clean EOF.
	err error
}

// sendStart sends the ExecStart, the stdin chunks and stdin_eof (when
// has_stdin) and half-closes, the way the exec Guest Transport does
// (EX-044). Send errors are ignored: the stream's status arrives on Recv.
func sendStart(stream flintlock.ExecStream, start *execv1.ExecStart, stdin ...string) {
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: start}})
	for _, chunk := range stdin {
		_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Stdin{Stdin: []byte(chunk)}})
	}
	if start.GetHasStdin() {
		_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_StdinEof{StdinEof: true}})
	}
	_ = stream.CloseSend()
}

// recvOne receives the next response into res and reports whether the
// stream is still open.
func recvOne(stream flintlock.ExecStream, res *execResult) bool {
	resp, err := stream.Recv()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			res.err = err
		}
		return false
	}
	switch p := resp.GetPayload().(type) {
	case *execv1.ExecCommandResponse_Stdout:
		res.stdout.Write(p.Stdout)
	case *execv1.ExecCommandResponse_Stderr:
		res.stderr.Write(p.Stderr)
	case *execv1.ExecCommandResponse_Error:
		res.errs = append(res.errs, p.Error)
	case *execv1.ExecCommandResponse_ExitCode:
		code := p.ExitCode
		res.exit = &code
	}
	return true
}

// drain receives until the stream ends.
func drain(stream flintlock.ExecStream, res *execResult) {
	for recvOne(stream, res) {
	}
}

// runExec performs one whole exchange.
func runExec(t *testing.T, open execOpener, start *execv1.ExecStart, stdin ...string) *execResult {
	t.Helper()
	stream, err := open(testCtx(t))
	if err != nil {
		t.Fatalf("open exec stream: %v", err)
	}
	sendStart(stream, start, stdin...)
	res := &execResult{}
	drain(stream, res)
	return res
}

// waitForStdout receives until stdout contains want, failing the test if
// the stream ends first.
func waitForStdout(t *testing.T, stream flintlock.ExecStream, res *execResult, want string) {
	t.Helper()
	for !strings.Contains(res.stdout.String(), want) {
		if !recvOne(stream, res) {
			t.Fatalf("stream ended before stdout contained %q: stdout=%q errs=%v err=%v", want, res.stdout.String(), res.errs, res.err)
		}
	}
}

// shell builds an ExecStart that runs script through sh -c.
func shell(uid, script string) *execv1.ExecStart {
	return &execv1.ExecStart{Uid: uid, Cmd: "sh", Args: []string{"-c", script}}
}

// exitCode returns the exit code or fails.
func exitCode(t *testing.T, res *execResult) int32 {
	t.Helper()
	if res.exit == nil {
		t.Fatalf("no exit code received: stdout=%q stderr=%q errs=%v err=%v", res.stdout.String(), res.stderr.String(), res.errs, res.err)
	}
	return *res.exit
}
