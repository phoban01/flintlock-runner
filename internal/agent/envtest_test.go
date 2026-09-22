package agent_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/phoban01/flintlock-runner/internal/agent"
	"github.com/phoban01/flintlock-runner/internal/agent/agenttest"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// The tests of this file run the Exec Agent against a real kube-apiserver
// serving the provisional claim resource, with the agent's shipped RBAC and
// admission policy, and a fake flintlockd served over gRPC (agenttest).
// Callers are real ServiceAccounts with real tokens, which the agent
// reviews with real TokenReviews. Without the envtest binaries they skip,
// or fail where FLINTLOCK_RUNNER_REQUIRE_ENVTEST is set, as it is in CI.

// env is the shared API server, nil when there is none.
var env *agenttest.Env

func TestMain(m *testing.M) {
	e, err := agenttest.Start()
	switch {
	case errors.Is(err, agenttest.ErrNoAssets):
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	default:
		env = e
	}
	code := m.Run()
	if env != nil {
		_ = env.Stop()
	}
	os.Exit(code)
}

// testTimeout bounds every wait in these tests; hitting it means a hang.
const testTimeout = 30 * time.Second

// needEnv skips a test without an API server.
func needEnv(t *testing.T) {
	t.Helper()
	if env == nil {
		t.Skip(agenttest.ErrNoAssets)
	}
}

// fixture is one Host with its Exec Agent, a namespace standing for a
// Runner's, and the Runner's identity in it.
type fixture struct {
	t      *testing.T
	host   *agenttest.Host
	ns     string
	runner agenttest.Identity
}

func newFixture(t *testing.T, opts agenttest.HostOptions) *fixture {
	t.Helper()
	needEnv(t)
	ns := env.Namespace(t)
	return &fixture{t: t, host: env.NewHost(t, opts), ns: ns, runner: env.ServiceAccountToken(t, ns, "runner")}
}

// bind writes a Bound claim of the Runner on the Host's MicroVM, as battery
// does when it grants one, expiring in an hour.
func (f *fixture) bind(name string) {
	f.t.Helper()
	env.PutClaim(f.t, f.ns, name, f.runner.User, agenttest.ClaimStatus{
		Phase: agent.ClaimBound, VMUID: f.host.VMUID, HostNode: f.host.Node, ExpiresAt: time.Now().Add(time.Hour),
	})
}

// tokenFile writes a token where the agent-exec transport reads it.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// transportFor is the agent-exec transport of the Runner to the Host's
// MicroVM, built by the production factory from the production client
// pool, with the given liveness deadline.
func (f *fixture) transportFor(token string, deadline time.Duration) transport.Transport {
	f.t.Helper()
	agents, err := transport.NewAgentHosts(transport.AgentExecConfig{CAFile: env.Certs.CAFile, TokenFile: tokenFile(f.t, token)})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = agents.Close() })
	client, release, err := agents.Lease(f.host.Node, f.host.Address)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(release)
	tr, err := transport.NewFactory().New(context.Background(), transport.Target{
		Kind: transport.KindAgentExec, Host: client, VMUID: f.host.VMUID, Deadline: deadline,
	})
	if err != nil {
		f.t.Fatalf("building the agent-exec transport: %v", err)
	}
	f.t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// bearer is a static bearer token, for callers the tests shape by hand.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return true }

// rawExec opens a MicroVMExec client on the agent directly, verifying its
// certificate, with the given token or none, so that a test sees the exact
// status the agent answers with.
func (f *fixture) rawExec(token string) execv1.MicroVMExecClient {
	f.t.Helper()
	pem, err := os.ReadFile(env.Certs.CAFile)
	if err != nil {
		f.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer(token)))
	}
	conn, err := grpc.NewClient(f.host.Address, opts...)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	return execv1.NewMicroVMExecClient(conn)
}

// execRaw runs cmd through a raw exchange and returns the status the
// exchange ended with, whether an exit code arrived, and the output.
func execRaw(ctx context.Context, client execv1.MicroVMExecClient, uid, cmd string) (exitCode int32, gotExit bool, out string, err error) {
	stream, err := client.ExecCommand(ctx)
	if err != nil {
		return 0, false, "", err
	}
	if err := stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: &execv1.ExecStart{
		Uid: uid, Cmd: cmd, Shell: true,
	}}}); err != nil && !errors.Is(err, io.EOF) {
		return 0, false, "", err
	}
	_ = stream.CloseSend()
	var b strings.Builder
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return exitCode, gotExit, b.String(), nil
		}
		if err != nil {
			return exitCode, gotExit, b.String(), err
		}
		switch p := resp.GetPayload().(type) {
		case *execv1.ExecCommandResponse_Stdout:
			b.Write(p.Stdout)
		case *execv1.ExecCommandResponse_ExitCode:
			exitCode, gotExit = p.ExitCode, true
		}
	}
}

// ran reports whether a marker file exists in the MicroVM's sandbox, which
// is how a test tells that a command ran on the fake Host at all.
func (f *fixture) ran(marker string) bool {
	_, err := os.Stat(filepath.Join(f.host.Sandbox(), marker))
	return err == nil
}

// lockedBuffer is a writer safe for concurrent use.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// statusCode is the gRPC code of err.
func statusCode(err error) codes.Code { return status.Code(err) }

// cutProxy forwards TCP connections to an address until cut is called,
// which closes every connection it carries at once, as a network fault or
// a node losing its route would.
type cutProxy struct {
	l     net.Listener
	to    string
	mu    sync.Mutex
	conns []net.Conn
}

func newCutProxy(t *testing.T, to string) *cutProxy {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(agenttest.HostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{l: l, to: to}
	t.Cleanup(func() { _ = l.Close(); p.cut() })
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", to)
			if err != nil {
				_ = in.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, in, out)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
			go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
		}
	}()
	return p
}

func (p *cutProxy) addr() string { return p.l.Addr().String() }

func (p *cutProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// writeTestCerts writes a certificate authority of its own under dir and
// returns its file.
func writeTestCerts(dir string) (string, error) {
	certs, err := hostfake.WriteTestCerts(dir)
	if err != nil {
		return "", err
	}
	return certs.CAFile, nil
}
