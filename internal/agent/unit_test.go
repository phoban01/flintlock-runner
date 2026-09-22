package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL run a command in a MicroVM only for an
//# identity that created a `MicroVMClaim` which is `Bound`, has not expired,
//# and names that MicroVM's uid and this Host, and SHALL refuse every other
//# request.

// TestAuthorize checks every condition of KF-174 on its own, and that a
// claim which records nothing about a condition fails it.
func TestAuthorize(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	good := Claim{Namespace: "ns", Name: "c", Phase: ClaimBound, VMUID: "vm", HostNode: "host", ExpiresAt: now.Add(time.Minute), Creator: "runner"}
	if err := Authorize(good, "runner", "vm", "host", now); err != nil {
		t.Fatalf("a good claim: %v", err)
	}
	for name, mutate := range map[string]func(*Claim){
		"pending":            func(c *Claim) { c.Phase = ClaimPending },
		"expired phase":      func(c *Claim) { c.Phase = ClaimExpired },
		"released":           func(c *Claim) { c.Phase = ClaimReleased },
		"no phase":           func(c *Claim) { c.Phase = "" },
		"another microvm":    func(c *Claim) { c.VMUID = "other" },
		"no microvm":         func(c *Claim) { c.VMUID = "" },
		"another host":       func(c *Claim) { c.HostNode = "other" },
		"no host":            func(c *Claim) { c.HostNode = "" },
		"lease ran out":      func(c *Claim) { c.ExpiresAt = now },
		"no expiry recorded": func(c *Claim) { c.ExpiresAt = time.Time{} },
		"another creator":    func(c *Claim) { c.Creator = "stranger" },
		"no creator":         func(c *Claim) { c.Creator = "" },
	} {
		c := good
		mutate(&c)
		if err := Authorize(c, "runner", "vm", "host", now); !errors.Is(err, errNotAuthorized) {
			t.Errorf("%s: Authorize = %v, want a refusal", name, err)
		}
	}
}

// authenticated answers every TokenReview as the Runner's identity.
func authenticated() (*authenticationv1.TokenReview, error) {
	return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true, User: authenticationv1.UserInfo{Username: "runner"},
	}}, nil
}

// stubClaims is a ClaimLookup of fixed answers.
type stubClaims struct {
	claims []Claim
	err    error
}

func (s stubClaims) ClaimsForVM(context.Context, string) ([]Claim, error) { return s.claims, s.err }
func (s stubClaims) BoundOnHost(context.Context, string) ([]Claim, error) { return s.claims, s.err }

// unitAgent serves the exec API over TLS in front of a fake flintlockd
// served over gRPC, with a fake API server client whose TokenReviews are
// answered by review, and returns the client to reach it with, the fake
// Host and the uid of a CREATED MicroVM on it.
func unitAgent(t *testing.T, review func() (*authenticationv1.TokenReview, error), claims ClaimLookup) (execv1.MicroVMExecClient, string, *hostfake.Host) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := hostfake.New(flintlock.FakeHostConfig{Name: "h", SandboxRoot: t.TempDir(), ExecEnabled: true})
	served := make(chan error, 1)
	go func() { served <- host.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	<-host.Ready()
	vm, err := host.Client().CreateMicroVM(ctx, &types.MicroVMSpec{Id: "job", Namespace: "unit"})
	if err != nil {
		t.Fatal(err)
	}
	fl, err := DialFlintlockd(host.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.Close() })

	kube := kubefake.NewClientset()
	kube.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		r, err := review()
		return true, r, err
	})
	certs, err := hostfake.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := newServingCert(ServerTLS{CertFile: certs.ServerCertFile, KeyFile: certs.ServerKeyFile}, "127.0.0.1", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		hostNode: "host", fl: fl, claims: claims, clk: clock.Real{}, log: slog.New(slog.DiscardHandler),
		authn:       &authenticator{reviews: kube.AuthenticationV1().TokenReviews(), timeout: time.Second},
		openTimeout: time.Second, callTimeout: time.Second,
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cert.tlsConfig())),
		grpc.ChainUnaryInterceptor(s.unaryInterceptor), grpc.ChainStreamInterceptor(s.streamInterceptor))
	s.register(srv)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	creds, err := credentials.NewClientTLSFromFile(certs.CAFile, "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return execv1.NewMicroVMExecClient(conn), vm.GetSpec().GetUid(), host
}

// runMarker execs `touch marker` as a caller with a token and returns the
// status the exchange ended with.
func runMarker(t *testing.T, client execv1.MicroVMExecClient, uid, marker string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer token")
	stream, err := client.ExecCommand(ctx)
	if err != nil {
		return err
	}
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: &execv1.ExecStart{Uid: uid, Cmd: "touch " + marker, Shell: true}}})
	_ = stream.CloseSend()
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, ok := resp.GetPayload().(*execv1.ExecCommandResponse_ExitCode); ok {
			return nil
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# If the TokenReview or the claim lookup of a request cannot be
//# completed, then the Exec Agent SHALL refuse the request.

// TestRefusedWhenTheReviewOrTheLookupCannotBeCompleted runs the agent with
// an API server that fails the TokenReview, and with one that authenticates
// the caller but whose claims cannot be looked up. Both requests are
// refused as unavailable and neither runs anything; with both answering
// and a good claim, the same request runs.
func TestRefusedWhenTheReviewOrTheLookupCannotBeCompleted(t *testing.T) {
	t.Parallel()
	good := func(uid string) Claim {
		return Claim{Phase: ClaimBound, VMUID: uid, HostNode: "host", ExpiresAt: time.Now().Add(time.Hour), Creator: "runner"}
	}

	t.Run("the TokenReview fails", func(t *testing.T) {
		t.Parallel()
		lookup := &stubClaims{}
		client, uid, host := unitAgent(t, func() (*authenticationv1.TokenReview, error) {
			return nil, errors.New("the API server is down")
		}, lookup)
		lookup.claims = []Claim{good(uid)}
		if err := runMarker(t, client, uid, "ran"); status.Code(err) != codes.Unavailable {
			t.Errorf("exec = %v, want unavailable", err)
		}
		if ran(t, host, uid, "ran") {
			t.Error("the command ran although the token could not be reviewed")
		}
	})
	t.Run("the claim lookup fails", func(t *testing.T) {
		t.Parallel()
		client, uid, host := unitAgent(t, authenticated, stubClaims{err: errors.New("the API server is down")})
		if err := runMarker(t, client, uid, "ran"); status.Code(err) != codes.Unavailable {
			t.Errorf("exec = %v, want unavailable", err)
		}
		if ran(t, host, uid, "ran") {
			t.Error("the command ran although the claims could not be looked up")
		}
	})
	t.Run("both answer", func(t *testing.T) {
		t.Parallel()
		lookup := &stubClaims{}
		client, uid, host := unitAgent(t, authenticated, lookup)
		lookup.claims = []Claim{good(uid)}
		if err := runMarker(t, client, uid, "ran"); err != nil {
			t.Errorf("exec = %v", err)
		}
		if !ran(t, host, uid, "ran") {
			t.Error("the command did not run")
		}
	})
}

// TestASecondExecStartEndsTheExchange authorizes an exchange for a MicroVM
// and then sends a second ExecStart on it. The claim was checked for the
// first message only, so the second must never reach flintlockd: the
// exchange ends as an invalid argument, and the command the second
// ExecStart named does not run.
func TestASecondExecStartEndsTheExchange(t *testing.T) {
	t.Parallel()
	lookup := &stubClaims{}
	client, uid, host := unitAgent(t, authenticated, lookup)
	lookup.claims = []Claim{{Phase: ClaimBound, VMUID: uid, HostNode: "host", ExpiresAt: time.Now().Add(time.Hour), Creator: "runner"}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer token")
	stream, err := client.ExecCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := func(cmd string) *execv1.ExecCommandRequest {
		return &execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: &execv1.ExecStart{Uid: uid, Cmd: cmd, Shell: true}}}
	}
	if err := stream.Send(start("touch first; sleep 5")); err != nil {
		t.Fatal(err)
	}
	// The first command is running once its marker is there, so the
	// exchange is past the claim check and relaying.
	deadline := time.Now().Add(5 * time.Second)
	for !ran(t, host, uid, "first") {
		if time.Now().After(deadline) {
			t.Fatal("the authorized command never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stream.Send(start("touch second")); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for {
		_, err := stream.Recv()
		if err == nil {
			continue
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("exchange ended with %v, want invalid argument", err)
		}
		break
	}
	if ran(t, host, uid, "second") {
		t.Error("the command of the second ExecStart ran")
	}
}

// ran reports whether a marker file is in a MicroVM's sandbox.
func ran(t *testing.T, host *hostfake.Host, uid, marker string) bool {
	t.Helper()
	dir, ok := host.SandboxPath(uid)
	if !ok {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, marker))
	return err == nil
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL serve its exec API over TLS on the Host's
//# internal address, with a serving certificate that names that address.

// TestServingCertificateNamesTheAddress checks that the agent refuses to
// serve with a certificate that does not name its address, accepts one
// that does, and serves only TLS.
func TestServingCertificateNamesTheAddress(t *testing.T) {
	t.Parallel()
	certs, err := hostfake.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := ServerTLS{CertFile: certs.ServerCertFile, KeyFile: certs.ServerKeyFile}
	log := slog.New(slog.DiscardHandler)
	if _, err := newServingCert(files, "127.0.0.1", log); err != nil {
		t.Errorf("a certificate for 127.0.0.1 on 127.0.0.1: %v", err)
	}
	if _, err := newServingCert(files, "10.0.0.7", log); err == nil || !strings.Contains(err.Error(), "does not name") {
		t.Errorf("a certificate for 127.0.0.1 on 10.0.0.7 = %v, want it refused", err)
	}
	cfg := &Config{HostNode: "h", Guard: Guard{Namespace: "ns"}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "tls.cert_file") {
		t.Errorf("a configuration without a serving certificate = %v, want it refused", err)
	}
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL reach `flintlockd` only through the local
//# endpoint of HI-042, and the Fleet Manifests SHALL run it as the user id
//# that HI-063 admits there and run no other container as that user id.

// TestFlintlockdOnlyLocal checks that the configuration and the dialler
// both refuse a flintlockd that is not on a local endpoint, and that the
// user id defaults to the one the Host Image admits.
func TestFlintlockdOnlyLocal(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"10.0.0.5:9090", "localhost:9090", "dns:///flintlockd:9090", "0.0.0.0:9090"} {
		if _, err := DialFlintlockd(endpoint); err == nil {
			t.Errorf("DialFlintlockd(%q) succeeded, want it refused", endpoint)
		}
		cfg := &Config{HostNode: "h", Flintlockd: endpoint, TLS: ServerTLS{CertFile: "c", KeyFile: "k"}, Guard: Guard{Namespace: "ns"}}
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "flintlockd:") {
			t.Errorf("a configuration with flintlockd at %q = %v, want it refused", endpoint, err)
		}
	}
	cfg := &Config{HostNode: "h", TLS: ServerTLS{CertFile: "c", KeyFile: "k"}, Guard: Guard{Namespace: "ns"}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Flintlockd != "127.0.0.1:9090" || cfg.FlintlockdUserID != 10250 || cfg.Port != 10270 {
		t.Errorf("defaults = %s, user %d, port %d", cfg.Flintlockd, cfg.FlintlockdUserID, cfg.Port)
	}
}
