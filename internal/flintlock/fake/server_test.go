package fake

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The project SHALL provide a fake Host that serves the flintlock
//# `MicroVM` service, including `ServerInfo`, and the `MicroVMExec` service
//# over gRPC using the generated server stubs from the flintlock `api`
//# module.

// TestServeMicroVMService drives every MicroVM RPC and an ExecCommand
// through the generated gRPC clients against a served fake Host, then
// cancels Serve and checks it returns cleanly.
func TestServeMicroVMService(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{Name: "h1", Version: "v0.14.0", ExecEnabled: true})
	stop := serveHost(t, h)
	if h.Addr() == "" {
		t.Fatal("Addr empty while serving")
	}
	conn := dialHost(t, h, nil)
	vms := mvmv1.NewMicroVMClient(conn)
	ctx := testCtx(t)

	info, err := vms.ServerInfo(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("ServerInfo: %v", err)
	}
	if info.GetVersion().GetVersion() != "v0.14.0" || !info.GetExec().GetEnabled() || info.GetExec().GetAddress() != h.Addr() {
		t.Errorf("ServerInfo = %v, want version v0.14.0 and exec enabled at %s", info, h.Addr())
	}

	created, err := vms.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{
		Microvm: &types.MicroVMSpec{Id: "vm1", Namespace: "ns", AllowGuestAgent: true},
	})
	if err != nil {
		t.Fatalf("CreateMicroVM: %v", err)
	}
	uid := created.GetMicrovm().GetSpec().GetUid()
	if uid == "" {
		t.Fatal("CreateMicroVM returned no uid")
	}
	if got := created.GetMicrovm().GetStatus().GetState(); got != types.MicroVMStatus_CREATED {
		t.Errorf("state with zero BootDelay = %s, want CREATED", got)
	}
	if _, err := vms.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateMicroVM(no spec) code = %s, want InvalidArgument", status.Code(err))
	}

	got, err := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: uid})
	if err != nil {
		t.Fatalf("GetMicroVM: %v", err)
	}
	if got.GetMicrovm().GetSpec().GetId() != "vm1" || got.GetMicrovm().GetStatus().GetVsockPath() == "" {
		t.Errorf("GetMicroVM = %v, want id vm1 with vsock_path", got.GetMicrovm())
	}
	if _, err := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: "missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetMicroVM(missing) code = %s, want NotFound", status.Code(err))
	}

	list, err := vms.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Namespace: "ns"})
	if err != nil {
		t.Fatalf("ListMicroVMs: %v", err)
	}
	if len(list.GetMicrovm()) != 1 || list.GetMicrovm()[0].GetSpec().GetUid() != uid {
		t.Errorf("ListMicroVMs(ns) = %v, want the one MicroVM", list.GetMicrovm())
	}
	other, err := vms.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Namespace: "other"})
	if err != nil || len(other.GetMicrovm()) != 0 {
		t.Errorf("ListMicroVMs(other) = %v, %v; want empty", other.GetMicrovm(), err)
	}

	stream, err := vms.ListMicroVMsStream(ctx, &mvmv1.ListMicroVMsRequest{Namespace: "ns"})
	if err != nil {
		t.Fatalf("ListMicroVMsStream: %v", err)
	}
	var streamed int
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ListMicroVMsStream Recv: %v", err)
		}
		if msg.GetMicrovm().GetSpec().GetUid() != uid {
			t.Errorf("streamed uid %q, want %q", msg.GetMicrovm().GetSpec().GetUid(), uid)
		}
		streamed++
	}
	if streamed != 1 {
		t.Errorf("ListMicroVMsStream sent %d messages, want 1", streamed)
	}

	res := runExec(t, func(ctx context.Context) (flintlock.ExecStream, error) {
		return execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
	}, shell(uid, "printf hello; exit 7"))
	if res.err != nil || res.stdout.String() != "hello" || exitCode(t, res) != 7 {
		t.Errorf("ExecCommand over gRPC: stdout=%q exit=%v err=%v", res.stdout.String(), res.exit, res.err)
	}

	sandbox, ok := h.SandboxPath(uid)
	if !ok {
		t.Fatal("SandboxPath: unknown uid")
	}
	if _, err := vms.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid}); err != nil {
		t.Fatalf("DeleteMicroVM: %v", err)
	}
	if _, err := os.Stat(sandbox); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sandbox %s after delete: stat err = %v, want not exist", sandbox, err)
	}
	if _, err := vms.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid}); status.Code(err) != codes.NotFound {
		t.Errorf("second DeleteMicroVM code = %s, want NotFound", status.Code(err))
	}

	if err := stop(); err != nil {
		t.Errorf("Serve returned %v after cancel, want nil", err)
	}
	if _, err := vms.ServerInfo(ctx, &emptypb.Empty{}); err == nil {
		t.Error("ServerInfo succeeded after Serve stopped")
	}
}

// TestServeTwiceAndAfterClose: Serve is one-shot and refuses a closed Host.
func TestServeTwiceAndAfterClose(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	serveHost(t, h)
	if err := h.Serve(context.Background()); err == nil {
		t.Error("second Serve succeeded, want an error")
	}
	h2 := newTestHost(t, flintlock.FakeHostConfig{})
	_ = h2.Close()
	if err := h2.Serve(context.Background()); err == nil {
		t.Error("Serve after Close succeeded, want an error")
	}
}

// TestServeShutdownKillsProcesses: cancelling Serve while a command runs
// ends the stream, kills the process and leaves the sandbox behind for the
// leak check.
func TestServeShutdownKillsProcesses(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	vm := createVM(t, h, nil)
	uid := vm.GetSpec().GetUid()
	stop := serveHost(t, h)
	conn := dialHost(t, h, nil)

	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(testCtx(t))
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}
	sendStart(stream, shell(uid, "echo $$; sleep 60"))
	res := &execResult{}
	waitForStdout(t, stream, res, "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(res.stdout.String()))
	if err != nil {
		t.Fatalf("parsing pid from %q: %v", res.stdout.String(), err)
	}

	if err := stop(); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d still alive after shutdown (kill 0 = %v)", pid, err)
	}
	drain(stream, res)
	if res.err == nil || res.exit != nil {
		t.Errorf("stream after shutdown: err=%v exit=%v; want an error and no exit code", res.err, res.exit)
	}
	left, err := h.Sandboxes()
	if err != nil {
		t.Fatalf("Sandboxes: %v", err)
	}
	if len(left) != 1 || left[0] != uid {
		t.Errorf("Sandboxes after shutdown = %v, want [%s]: shutdown must not delete MicroVMs", left, uid)
	}
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL enforce basic auth and TLS when configured so that
//# the Runner's authentication code is exercised.

// TestBasicAuth: with a token configured, every RPC needs the flintlockd
// header, `authorization: Basic base64(token)`.
func TestBasicAuth(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{Token: "s3cret"})
	serveHost(t, h)
	conn := dialHost(t, h, nil)
	vms := mvmv1.NewMicroVMClient(conn)
	good := base64.StdEncoding.EncodeToString([]byte("s3cret"))

	tests := []struct {
		name   string
		header string
		want   codes.Code
	}{
		{name: "no header", header: "", want: codes.Unauthenticated},
		{name: "bearer scheme", header: "Bearer " + good, want: codes.Unauthenticated},
		{name: "no scheme", header: good, want: codes.Unauthenticated},
		{name: "wrong token", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("nope")), want: codes.Unauthenticated},
		{name: "raw token not base64", header: "Basic s3cret", want: codes.Unauthenticated},
		{name: "right token", header: "Basic " + good, want: codes.OK},
		{name: "lower-case scheme", header: "basic " + good, want: codes.OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := testCtx(t)
			if tt.header != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", tt.header)
			}
			_, err := vms.ServerInfo(ctx, &emptypb.Empty{})
			if status.Code(err) != tt.want {
				t.Errorf("ServerInfo code = %s (%v), want %s", status.Code(err), err, tt.want)
			}
			// Streams go through the same check.
			stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
			if err != nil {
				t.Fatalf("ExecCommand: %v", err)
			}
			sendStart(stream, &execv1.ExecStart{Uid: "missing", Cmd: "true"})
			res := &execResult{}
			drain(stream, res)
			wantStream := tt.want
			if wantStream == codes.OK {
				wantStream = codes.NotFound // authenticated, then the uid lookup fails
			}
			if status.Code(res.err) != wantStream {
				t.Errorf("ExecCommand code = %s (%v), want %s", status.Code(res.err), res.err, wantStream)
			}
		})
	}
}

// TestTLS: the Host serves TLS from cfg.TLS, verifiable against the test
// CA, and requires a client certificate when ClientCAFile is set.
func TestTLS(t *testing.T) {
	t.Parallel()
	certs, err := WriteTestCerts(t.TempDir(), "h1")
	if err != nil {
		t.Fatalf("WriteTestCerts: %v", err)
	}
	caPEM, err := os.ReadFile(certs.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA file has no certificate")
	}
	clientCert, err := tls.LoadX509KeyPair(certs.ClientCertFile, certs.ClientKeyFile)
	if err != nil {
		t.Fatalf("loading client cert: %v", err)
	}
	for _, name := range []string{certs.CAFile, certs.ServerCertFile, certs.ServerKeyFile, certs.ClientCertFile, certs.ClientKeyFile} {
		if filepath.Dir(name) != certs.Dir {
			t.Errorf("%s not under %s", name, certs.Dir)
		}
	}

	tests := []struct {
		name   string
		mutual bool
		creds  credentials.TransportCredentials
		wantOK bool
	}{
		{name: "server tls, verifying client", creds: credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}), wantOK: true},
		{name: "server tls, plaintext client", creds: nil, wantOK: false},
		{name: "server tls, wrong roots", creds: credentials.NewTLS(&tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}), wantOK: false},
		{name: "mutual tls, client with cert", mutual: true, creds: credentials.NewTLS(&tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS12}), wantOK: true},
		{name: "mutual tls, client without cert", mutual: true, creds: credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHost(t, flintlock.FakeHostConfig{Name: "h1", TLS: certs.ServerTLS(tt.mutual)})
			serveHost(t, h)
			conn := dialHost(t, h, tt.creds)
			ctx, cancel := context.WithTimeout(testCtx(t), 5*time.Second)
			defer cancel()
			_, err := mvmv1.NewMicroVMClient(conn).ServerInfo(ctx, &emptypb.Empty{})
			if (err == nil) != tt.wantOK {
				t.Errorf("ServerInfo err = %v, want ok=%v", err, tt.wantOK)
			}
		})
	}

	t.Run("bad certificate files", func(t *testing.T) {
		t.Parallel()
		h := newTestHost(t, flintlock.FakeHostConfig{TLS: &flintlock.ServerTLS{CertFile: "/nonexistent", KeyFile: "/nonexistent"}})
		if err := h.Serve(context.Background()); err == nil {
			t.Error("Serve with missing certificate files succeeded")
		}
	})
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL report a configurable flintlock version and service
//# flags from `ServerInfo` and SHALL provide a mode in which `ServerInfo`
//# returns `UNIMPLEMENTED`.

// TestServerInfo checks the configured version, flags and uptime through
// both the gRPC service and the in-process client, and the UNIMPLEMENTED
// mode through both.
func TestServerInfo(t *testing.T) {
	t.Parallel()

	t.Run("configured", func(t *testing.T) {
		t.Parallel()
		clk := newFakeClock()
		h := newTestHost(t, flintlock.FakeHostConfig{
			Name: "h1", Version: "v0.99.0", ExecEnabled: true, SSHProxyEnabled: false,
		}, WithClock(clk))
		serveHost(t, h)
		clk.Advance(90 * time.Second)

		resp, err := mvmv1.NewMicroVMClient(dialHost(t, h, nil)).ServerInfo(testCtx(t), &emptypb.Empty{})
		if err != nil {
			t.Fatalf("ServerInfo over gRPC: %v", err)
		}
		if resp.GetVersion().GetVersion() != "v0.99.0" {
			t.Errorf("version = %q, want v0.99.0", resp.GetVersion().GetVersion())
		}
		if !resp.GetExec().GetEnabled() || resp.GetExec().GetAddress() != h.Addr() {
			t.Errorf("exec = %v, want enabled at %s", resp.GetExec(), h.Addr())
		}
		if resp.GetSshProxy().GetEnabled() || resp.GetSshProxy().GetAddress() != "" {
			t.Errorf("ssh_proxy = %v, want disabled with no address", resp.GetSshProxy())
		}
		if got := resp.GetUptime().AsDuration(); got != 90*time.Second {
			t.Errorf("uptime = %v, want 90s", got)
		}

		info, err := h.Client().ServerInfo(testCtx(t))
		if err != nil {
			t.Fatalf("ServerInfo in process: %v", err)
		}
		want := &flintlock.HostInfo{
			Name: "h1", VersionKnown: true, Version: "v0.99.0", BuildDate: "fake", Commit: "fake",
			Uptime: 90 * time.Second,
			Exec:   flintlock.GuestService{Enabled: true, Address: h.Addr()},
		}
		if *info != *want {
			t.Errorf("HostInfo = %+v, want %+v", info, want)
		}
	})

	t.Run("unimplemented", func(t *testing.T) {
		t.Parallel()
		h := newTestHost(t, flintlock.FakeHostConfig{Version: "v0.1.0", ServerInfoUnimplemented: true})
		serveHost(t, h)
		_, err := mvmv1.NewMicroVMClient(dialHost(t, h, nil)).ServerInfo(testCtx(t), &emptypb.Empty{})
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("gRPC ServerInfo code = %s, want Unimplemented", status.Code(err))
		}
		if _, err := h.Client().ServerInfo(testCtx(t)); !errors.Is(err, flintlock.ErrUnimplemented) {
			t.Errorf("in-process ServerInfo = %v, want ErrUnimplemented", err)
		}
		// The rest of the Host still works, as on an old flintlockd.
		if _, err := h.Client().ListMicroVMs(testCtx(t), "ns"); err != nil {
			t.Errorf("ListMicroVMs on an unimplemented-ServerInfo host: %v", err)
		}
	})
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL support fault injection for a create that ends in
//# `FAILED`, an exec stream dropped before the exit code, and a Host that
//# stops answering.

// TestUnresponsive: with the fault on, the listener accepts and RPCs hang
// until the caller's deadline, in-flight exec streams stall, and clearing
// the fault lets everything resume.
func TestUnresponsive(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	vm := createVM(t, h, nil)
	uid := vm.GetSpec().GetUid()
	serveHost(t, h)
	conn := dialHost(t, h, nil)
	vms := mvmv1.NewMicroVMClient(conn)

	// An exec stream that is mid-flight when the Host stops answering.
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(testCtx(t))
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{
		Start: &execv1.ExecStart{Uid: uid, Cmd: "sh", Args: []string{"-c", "echo first; cat"}, HasStdin: true},
	}})
	res := &execResult{}
	waitForStdout(t, stream, res, "first")

	h.SetFaults(flintlock.HostFaults{Unresponsive: true})

	// New unary RPCs time out at the caller's deadline rather than fail.
	ctx, cancel := context.WithTimeout(testCtx(t), 100*time.Millisecond)
	_, err = vms.ServerInfo(ctx, &emptypb.Empty{})
	cancel()
	if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("ServerInfo while unresponsive: code = %s (%v), want DeadlineExceeded", status.Code(err), err)
	}

	// In-process calls behave the same and wrap both sentinel and cause.
	ctx, cancel = context.WithTimeout(testCtx(t), 100*time.Millisecond)
	_, err = h.Client().ServerInfo(ctx)
	cancel()
	if !errors.Is(err, flintlock.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("in-process ServerInfo while unresponsive = %v, want ErrUnavailable and DeadlineExceeded", err)
	}

	// The in-flight stream produces output but the Host does not forward it.
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Stdin{Stdin: []byte("second\n")}})
	got := make(chan bool, 1)
	go func() { got <- recvOne(stream, res) }()
	select {
	case <-got:
		t.Fatalf("stream delivered %q while the Host was unresponsive", res.stdout.String())
	case <-time.After(100 * time.Millisecond):
	}

	// Clearing the fault wakes the blocked send and later RPCs succeed.
	h.SetFaults(flintlock.HostFaults{})
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("stream ended after recovery: err=%v", res.err)
		}
	case <-time.After(testTimeout):
		t.Fatal("stream did not resume after the fault was cleared")
	}
	if res.stdout.String() != "first\nsecond\n" {
		t.Errorf("stdout after recovery = %q, want %q", res.stdout.String(), "first\nsecond\n")
	}
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_StdinEof{StdinEof: true}})
	_ = stream.CloseSend()
	drain(stream, res)
	if res.err != nil || exitCode(t, res) != 0 {
		t.Errorf("stream end after recovery: err=%v exit=%v", res.err, res.exit)
	}
	if _, err := vms.ServerInfo(testCtx(t), &emptypb.Empty{}); err != nil {
		t.Errorf("ServerInfo after recovery: %v", err)
	}

	// Closing the Host while a call is blocked on the fault fails it.
	h.SetFaults(flintlock.HostFaults{Unresponsive: true})
	blocked := make(chan error, 1)
	go func() {
		_, err := h.Client().GetMicroVM(testCtx(t), uid)
		blocked <- err
	}()
	_ = h.Close()
	select {
	case err := <-blocked:
		if !errors.Is(err, flintlock.ErrUnavailable) {
			t.Errorf("blocked call after Close = %v, want ErrUnavailable", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("blocked call did not return after Close")
	}
}

// TestStatusToSentinel maps the store's codes onto the flintlock sentinels.
func TestStatusToSentinel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code codes.Code
		want []error
	}{
		{codes.NotFound, []error{flintlock.ErrNotFound}},
		{codes.Unimplemented, []error{flintlock.ErrUnimplemented}},
		{codes.Unauthenticated, []error{flintlock.ErrUnauthenticated}},
		{codes.Unavailable, []error{flintlock.ErrUnavailable}},
		{codes.DeadlineExceeded, []error{flintlock.ErrUnavailable, context.DeadlineExceeded}},
		{codes.Canceled, []error{flintlock.ErrUnavailable, context.Canceled}},
	}
	for _, tt := range tests {
		err := statusToSentinel("h", status.Error(tt.code, "x"))
		for _, want := range tt.want {
			if !errors.Is(err, want) {
				t.Errorf("%s: %v does not match %v", tt.code, err, want)
			}
		}
		if !strings.Contains(err.Error(), "fake host h") {
			t.Errorf("%s: %v does not name the host", tt.code, err)
		}
	}
	if statusToSentinel("h", nil) != nil {
		t.Error("nil error mapped to non-nil")
	}
	plain := errors.New("plain")
	if err := statusToSentinel("h", plain); !errors.Is(err, plain) {
		t.Errorf("plain error not wrapped: %v", err)
	}
	other := statusToSentinel("h", status.Error(codes.FailedPrecondition, "not ready"))
	if status.Code(other) != codes.FailedPrecondition {
		t.Errorf("unmapped code lost: %v", other)
	}
}
