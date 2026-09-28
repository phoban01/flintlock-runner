package fakeexecagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

// admitAll is an Authorizer that lets every call through.
type admitAll struct{}

func (admitAll) Authorize(context.Context, string, string, string) error { return nil }

// startExec opens an exchange on conn that runs script in the MicroVM uid.
func startExec(ctx context.Context, t *testing.T, conn *grpc.ClientConn, uid, script string) execv1.MicroVMExec_ExecCommandClient {
	t.Helper()
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := &execv1.ExecStart{Uid: uid, Cmd: "/bin/sh", Args: []string{"-c", script}}
	if err := stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: start}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	return stream
}

// exitCode reads an exchange to its end: the exit status, or the error the
// exchange ended with instead.
func exitCode(stream execv1.MicroVMExec_ExecCommandClient, onOutput func()) (int32, error) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return 0, err
		}
		if code, ok := resp.GetPayload().(*execv1.ExecCommandResponse_ExitCode); ok {
			return code.ExitCode, nil
		}
		if onOutput != nil {
			onOutput()
			onOutput = nil
		}
	}
}

// TestRestartEndsOpenExchangesAndServesAgain checks the fault the harness
// injects for KF-193: Restart ends a running exchange without its exit
// status, and the Agent then serves again on the same address, so that a
// new exchange runs to its exit status.
func TestRestartEndsOpenExchangesAndServesAgain(t *testing.T) {
	host := fake.New(flintlock.FakeHostConfig{Name: "host-1", ExecEnabled: true, SandboxRoot: t.TempDir()})
	t.Cleanup(func() { _ = host.Close() })
	certs, err := fake.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, err := Start(Config{
		Node: "host-1", CertFile: certs.ServerCertFile, KeyFile: certs.ServerKeyFile,
		Upstream: host.Client(), Authorizer: admitAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	addr := agent.Addr()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	uid := "microvm-1"
	if _, err := host.Client().CreateMicroVM(ctx, &types.MicroVMSpec{Id: "vm", Namespace: "ns", Uid: &uid}); err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(certs.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// A Stage that runs until the restart: it has printed once when the
	// agent restarts.
	running := startExec(ctx, t, conn, uid, "echo started; sleep 30")
	printed := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		_, err := exitCode(running, func() { close(printed) })
		ended <- err
	}()
	select {
	case <-printed:
	case err := <-ended:
		t.Fatalf("the exchange ended before the restart: %v", err)
	case <-ctx.Done():
		t.Fatal("the stage printed nothing")
	}
	if err := agent.Restart(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the exchange ended with its exit status across a restart")
		}
	case <-ctx.Done():
		t.Fatal("the exchange outlived the restart")
	}

	if agent.Addr() != addr {
		t.Errorf("the agent serves on %s after the restart, want %s", agent.Addr(), addr)
	}
	code, err := exitCode(startExec(ctx, t, conn, uid, "exit 7"), nil)
	if err != nil {
		t.Fatalf("an exchange after the restart: %v", err)
	}
	if code != 7 {
		t.Errorf("exit status %d after the restart, want 7", code)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("timed out")
	}
}
