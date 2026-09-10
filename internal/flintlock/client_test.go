package flintlock_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# The Runner SHALL communicate with Hosts through the flintlock
//# `microvm.services.api.v1alpha1`, `microvmexec.services.api.v1alpha1` and
//# `microvmsshproxy.services.api.v1alpha1` gRPC APIs using the generated
//# clients from the flintlock `api` module.

// TestClientSpeaksTheThreeFlintlockAPIs drives one dialled Client over each
// of the three services in turn against real gRPC servers built from the
// generated stubs: MicroVM for ServerInfo and GetMicroVM, MicroVMExec for a
// command in the guest, and MicroVMSSHProxy for a byte stream to it. A
// client that spoke anything but the generated clients of the flintlock api
// module could not be understood by any of them.
func TestClientSpeaksTheThreeFlintlockAPIs(t *testing.T) {
	t.Parallel()
	host := startHost(t, flintlock.FakeHostConfig{
		Name:        "h1",
		Version:     "v0.7.0",
		ExecEnabled: true,
	})
	uid := createVM(t, host)
	client := dial(t, host)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("microvm", func(t *testing.T) {
		info, err := client.ServerInfo(ctx)
		if err != nil {
			t.Fatalf("ServerInfo: %v", err)
		}
		if info.Version != "v0.7.0" || !info.VersionKnown {
			t.Errorf("ServerInfo reported %+v, want version v0.7.0", info)
		}
		vm, err := client.GetMicroVM(ctx, uid)
		if err != nil {
			t.Fatalf("GetMicroVM: %v", err)
		}
		if vm.GetSpec().GetUid() != uid {
			t.Errorf("GetMicroVM returned uid %q, want %q", vm.GetSpec().GetUid(), uid)
		}
	})

	t.Run("microvmexec", func(t *testing.T) {
		stdout, status, err := execCommand(ctx, client, uid, "echo", []string{"api"}, nil)
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if status != 0 || stdout != "api\n" {
			t.Errorf("exec returned (%q, %d), want (%q, 0)", stdout, status, "api\n")
		}
	})

	t.Run("microvmsshproxy", func(t *testing.T) {
		proxy, addr := startSSHProxy(t, func(_ string, rw io.ReadWriteCloser) {
			_, _ = rw.Write([]byte("ok"))
		})
		proxyClient := dialEndpoint(t, flintlock.Endpoint{
			Name:    "h1",
			Address: addr,
			TLS:     flintlock.TLSOptions{Insecure: true},
		})
		conn, err := proxyClient.SSHProxy(ctx, uid)
		if err != nil {
			t.Fatalf("SSHProxy: %v", err)
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 2)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("reading from the proxy: %v", err)
		}
		if uids := proxy.seenUIDs(); len(uids) != 1 || uids[0] != uid {
			t.Errorf("the proxy saw %v, want [%s]", uids, uid)
		}
	})
}

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# The Runner SHALL apply the configured deadline to every unary
//# flintlock call.

// TestUnaryCallsCarryTheConfiguredDeadline points the client at a Host that
// has stopped answering and calls every unary method with a context that has
// no deadline of its own. Each one has to come back at the configured
// deadline rather than block until the test times out, and a caller that
// brings a shorter deadline has to keep it.
func TestUnaryCallsCarryTheConfiguredDeadline(t *testing.T) {
	t.Parallel()
	host := startHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	uid := createVM(t, host)
	const deadline = 300 * time.Millisecond
	client := dial(t, host, flintlock.WithCallDeadline(deadline))
	host.SetFaults(flintlock.HostFaults{Unresponsive: true})

	calls := map[string]func(context.Context) error{
		"ServerInfo": func(ctx context.Context) error {
			_, err := client.ServerInfo(ctx)
			return err
		},
		"GetMicroVM": func(ctx context.Context) error {
			_, err := client.GetMicroVM(ctx, uid)
			return err
		},
		"ListMicroVMs": func(ctx context.Context) error {
			_, err := client.ListMicroVMs(ctx, testNamespace)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := call(context.Background())
			elapsed := time.Since(start)
			if !errors.Is(err, flintlock.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s on an unresponsive host returned %v, want the deadline to expire", name, err)
			}
			if elapsed > 5*deadline {
				t.Errorf("%s took %s to give up, want about %s", name, elapsed, deadline)
			}
		})
	}

	t.Run("caller deadline wins when shorter", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := client.ServerInfo(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ServerInfo returned %v, want the caller's deadline", err)
		}
		if elapsed := time.Since(start); elapsed >= deadline {
			t.Errorf("the caller's 20ms deadline was not applied: the call took %s", elapsed)
		}
	})
}

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# Where a Host is configured with a basic auth token, the Runner
//# SHALL send an `authorization` header with the value `Basic` followed by the
//# base64 encoding of the token on every call to that Host.

// TestBasicAuthTokenIsSentOnEveryCall runs against a fake Host that enforces
// flintlockd's own basic auth rules: it accepts only an `authorization`
// header whose scheme is basic and whose credential is the base64 of the
// configured token. Unary calls and streams both have to carry it, and a
// client without the token, or with the wrong one, is rejected.
func TestBasicAuthTokenIsSentOnEveryCall(t *testing.T) {
	t.Parallel()
	const token = "s3cret-host-token"
	host := startHost(t, flintlock.FakeHostConfig{Name: "h1", Token: token, ExecEnabled: true})
	uid := createVM(t, host)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	authorised := dial(t, host)
	if _, err := authorised.ServerInfo(ctx); err != nil {
		t.Fatalf("ServerInfo with the configured token: %v", err)
	}
	stdout, status, err := execCommand(ctx, authorised, uid, "echo", []string{"authorised"}, nil)
	if err != nil || status != 0 || stdout != "authorised\n" {
		t.Fatalf("exec with the configured token returned (%q, %d, %v)", stdout, status, err)
	}

	for name, tok := range map[string]string{"no token": "", "wrong token": "not-the-token"} {
		t.Run(name, func(t *testing.T) {
			client := dialEndpoint(t, flintlock.Endpoint{
				Name:    "h1",
				Address: host.Addr(),
				Token:   tok,
				TLS:     flintlock.TLSOptions{Insecure: true},
			})
			if _, err := client.ServerInfo(ctx); !errors.Is(err, flintlock.ErrUnauthenticated) {
				t.Errorf("ServerInfo with %s returned %v, want ErrUnauthenticated", name, err)
			}
			if _, _, err := execCommand(ctx, client, uid, "echo", []string{"x"}, nil); !errors.Is(err, flintlock.ErrUnauthenticated) {
				t.Errorf("exec with %s returned %v, want ErrUnauthenticated", name, err)
			}
		})
	}
}

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# The Runner SHALL connect to Hosts with TLS, verifying the server
//# certificate against the configured certificate authority, unless the Host
//# is explicitly marked insecure.

// TestTLSVerifiesTheServerCertificate dials a fake Host serving TLS. The
// connection succeeds with the certificate authority that signed the Host's
// certificate, and fails without it, which is what verification means; a
// Host is reached in plaintext only when its entry says so.
func TestTLSVerifiesTheServerCertificate(t *testing.T) {
	t.Parallel()
	certs, err := fake.WriteTestCerts(filepath.Join(t.TempDir(), "certs"))
	if err != nil {
		t.Fatalf("WriteTestCerts: %v", err)
	}
	host := startHost(t, flintlock.FakeHostConfig{Name: "h1", TLS: certs.ServerTLS(false)})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("with the configured authority", func(t *testing.T) {
		client := dialEndpoint(t, flintlock.Endpoint{
			Name:    "h1",
			Address: host.Addr(),
			TLS:     flintlock.TLSOptions{CAFile: certs.CAFile},
		})
		if _, err := client.ServerInfo(ctx); err != nil {
			t.Fatalf("ServerInfo over verified TLS: %v", err)
		}
	})

	t.Run("without it", func(t *testing.T) {
		// No CA file: the throwaway authority is not in the system roots, so
		// the certificate cannot be verified and the call has to fail.
		client := dialEndpoint(t, flintlock.Endpoint{Name: "h1", Address: host.Addr()})
		if _, err := client.ServerInfo(ctx); err == nil {
			t.Fatal("ServerInfo succeeded against a certificate signed by an unknown authority")
		}
	})

	t.Run("plaintext against a TLS host", func(t *testing.T) {
		client := dialEndpoint(t, flintlock.Endpoint{
			Name:    "h1",
			Address: host.Addr(),
			TLS:     flintlock.TLSOptions{Insecure: true},
		})
		if _, err := client.ServerInfo(ctx); err == nil {
			t.Fatal("a plaintext client reached a TLS host")
		}
	})
}

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# Where client certificate and key files are configured for a
//# Host, the Runner SHALL present them for mutual TLS.

// TestMutualTLSPresentsTheClientCertificate dials a fake Host that requires
// and verifies client certificates. The connection succeeds only when the
// Endpoint names both the client certificate and its key, and a client that
// presents none is turned away by the Host.
func TestMutualTLSPresentsTheClientCertificate(t *testing.T) {
	t.Parallel()
	certs, err := fake.WriteTestCerts(filepath.Join(t.TempDir(), "certs"))
	if err != nil {
		t.Fatalf("WriteTestCerts: %v", err)
	}
	host := startHost(t, flintlock.FakeHostConfig{Name: "h1", TLS: certs.ServerTLS(true)})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("with the client certificate", func(t *testing.T) {
		client := dialEndpoint(t, flintlock.Endpoint{
			Name:    "h1",
			Address: host.Addr(),
			TLS: flintlock.TLSOptions{
				CAFile:   certs.CAFile,
				CertFile: certs.ClientCertFile,
				KeyFile:  certs.ClientKeyFile,
			},
		})
		if _, err := client.ServerInfo(ctx); err != nil {
			t.Fatalf("ServerInfo over mutual TLS: %v", err)
		}
	})

	t.Run("without it", func(t *testing.T) {
		client := dialEndpoint(t, flintlock.Endpoint{
			Name:    "h1",
			Address: host.Addr(),
			TLS:     flintlock.TLSOptions{CAFile: certs.CAFile},
		})
		if _, err := client.ServerInfo(ctx); err == nil {
			t.Fatal("ServerInfo succeeded without presenting a client certificate")
		}
	})

	t.Run("a key without a certificate is refused", func(t *testing.T) {
		_, err := flintlock.NewDialer().Dial(context.Background(), flintlock.Endpoint{
			Name:    "h1",
			Address: host.Addr(),
			TLS:     flintlock.TLSOptions{CAFile: certs.CAFile, KeyFile: certs.ClientKeyFile},
		})
		if err == nil {
			t.Fatal("Dial accepted a key with no certificate")
		}
	})
}

// execCommand runs one command in a MicroVM over an ExecCommand stream and
// returns its standard output and exit status. It is the smallest client of
// the stream the tests need; the exec Guest Transport is the real one.
func execCommand(ctx context.Context, client flintlock.HostClient, uid, cmd string, args []string, stdin []byte) (string, int, error) {
	stream, err := client.Exec(ctx)
	if err != nil {
		return "", -1, err
	}
	start := &execv1.ExecStart{Uid: uid, Cmd: cmd, Args: args, HasStdin: stdin != nil}
	if err := stream.Send(&execv1.ExecCommandRequest{
		Payload: &execv1.ExecCommandRequest_Start{Start: start},
	}); err != nil {
		if _, recvErr := stream.Recv(); recvErr != nil && !errors.Is(recvErr, io.EOF) {
			return "", -1, recvErr
		}
		return "", -1, err
	}
	if stdin != nil {
		if err := stream.Send(&execv1.ExecCommandRequest{
			Payload: &execv1.ExecCommandRequest_Stdin{Stdin: stdin},
		}); err != nil {
			return "", -1, err
		}
		if err := stream.Send(&execv1.ExecCommandRequest{
			Payload: &execv1.ExecCommandRequest_StdinEof{StdinEof: true},
		}); err != nil {
			return "", -1, err
		}
	}
	_ = stream.CloseSend()

	var out string
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, -1, errors.New("stream ended before the exit code")
			}
			return out, -1, err
		}
		switch payload := resp.GetPayload().(type) {
		case *execv1.ExecCommandResponse_Stdout:
			out += string(payload.Stdout)
		case *execv1.ExecCommandResponse_Error:
			return out, -1, errors.New(payload.Error)
		case *execv1.ExecCommandResponse_ExitCode:
			return out, int(payload.ExitCode), nil
		}
	}
}
