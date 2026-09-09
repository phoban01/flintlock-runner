package transport_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

// testTimeout bounds every wait in these tests; hitting it means a hang.
const testTimeout = 30 * time.Second

// testNamespace is the namespace the fake Host's MicroVMs live in.
const testNamespace = "flintlock-runner-test"

// newFakeHost builds a fake Host, a Runner-side client for it and one
// CREATED MicroVM, and returns the client and the uid. The client comes from
// the fake's Dialer, so it has no admin methods, exactly as a dialled one
// does not (HO-007).
func newFakeHost(t *testing.T, cfg flintlock.FakeHostConfig, opts ...fake.Option) (*fake.Host, flintlock.HostClient, string) {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "h1"
	}
	if cfg.SandboxRoot == "" {
		cfg.SandboxRoot = t.TempDir()
	}
	host := fake.New(cfg, opts...)
	t.Cleanup(func() { _ = host.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	vm, err := host.Client().CreateMicroVM(ctx, &types.MicroVMSpec{Id: "vm", Namespace: testNamespace})
	if err != nil {
		t.Fatalf("CreateMicroVM: %v", err)
	}
	client, err := fake.NewDialer(host).Dial(ctx, flintlock.Endpoint{Name: cfg.Name, Token: cfg.Token})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return host, client, vm.GetSpec().GetUid()
}

// scriptedStream is an ExecStream that records what the transport sent and
// answers with a script. It exists so that the exec transport's framing can
// be asserted message by message without a Host.
//
// Recv holds its answers back until the client has half-closed, which is
// what makes the recorded exchange deterministic: the transport's stdin
// goroutine has finished by the time the first response arrives.
type scriptedStream struct {
	responses []*execv1.ExecCommandResponse
	// recvErr is returned once the responses run out; io.EOF by default,
	// which is a stream that ended without an exit code.
	recvErr error
	// sendErr, when set, fails the nth Send, counting from one.
	sendErr  error
	failSend int

	mu        sync.Mutex
	sent      []*execv1.ExecCommandRequest
	sends     int
	closed    bool
	closeSend chan struct{}
	once      sync.Once
}

// newScriptedStream builds a stream that answers with responses.
func newScriptedStream(responses ...*execv1.ExecCommandResponse) *scriptedStream {
	return &scriptedStream{responses: responses, closeSend: make(chan struct{})}
}

// Send implements flintlock.ExecStream.
func (s *scriptedStream) Send(req *execv1.ExecCommandRequest) error {
	s.mu.Lock()
	s.sends++
	n := s.sends
	if s.closed {
		s.mu.Unlock()
		return errors.New("send after CloseSend")
	}
	s.sent = append(s.sent, req)
	s.mu.Unlock()
	if s.sendErr != nil && n == s.failSend {
		return s.sendErr
	}
	return nil
}

// Recv implements flintlock.ExecStream.
func (s *scriptedStream) Recv() (*execv1.ExecCommandResponse, error) {
	<-s.closeSend
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.responses) == 0 {
		if s.recvErr != nil {
			return nil, s.recvErr
		}
		return nil, io.EOF
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

// CloseSend implements flintlock.ExecStream.
func (s *scriptedStream) CloseSend() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.once.Do(func() { close(s.closeSend) })
	return nil
}

// requests returns what the transport sent, in order.
func (s *scriptedStream) requests() []*execv1.ExecCommandRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*execv1.ExecCommandRequest(nil), s.sent...)
}

// stubHost is a HostClient built from functions, for the cases a fake Host
// cannot produce: a scripted exec stream, an ssh proxy stream, a Host that
// refuses one of them.
type stubHost struct {
	name     string
	info     *flintlock.HostInfo
	infoErr  error
	exec     func(ctx context.Context) (flintlock.ExecStream, error)
	sshProxy func(ctx context.Context, uid string) (io.ReadWriteCloser, error)
	getVM    func(ctx context.Context, uid string) (*types.MicroVM, error)

	mu       sync.Mutex
	execs    int
	proxies  int
	vmProbes int
}

// Name implements flintlock.HostClient.
func (h *stubHost) Name() string {
	if h.name == "" {
		return "stub"
	}
	return h.name
}

// ServerInfo implements flintlock.HostClient.
func (h *stubHost) ServerInfo(context.Context) (*flintlock.HostInfo, error) {
	if h.infoErr != nil {
		return nil, h.infoErr
	}
	if h.info != nil {
		return h.info, nil
	}
	return &flintlock.HostInfo{
		Name:         h.Name(),
		VersionKnown: true,
		Version:      "stub",
		Exec:         flintlock.GuestService{Enabled: true},
		SSHProxy:     flintlock.GuestService{Enabled: true},
	}, nil
}

// GetMicroVM implements flintlock.HostClient.
func (h *stubHost) GetMicroVM(ctx context.Context, uid string) (*types.MicroVM, error) {
	h.mu.Lock()
	h.vmProbes++
	h.mu.Unlock()
	if h.getVM != nil {
		return h.getVM(ctx, uid)
	}
	return &types.MicroVM{}, nil
}

// ListMicroVMs implements flintlock.HostClient.
func (h *stubHost) ListMicroVMs(context.Context, string) ([]*types.MicroVM, error) {
	return nil, errors.New("not used")
}

// Exec implements flintlock.HostClient.
func (h *stubHost) Exec(ctx context.Context) (flintlock.ExecStream, error) {
	h.mu.Lock()
	h.execs++
	h.mu.Unlock()
	if h.exec == nil {
		return nil, errors.New("no exec stream configured")
	}
	return h.exec(ctx)
}

// SSHProxy implements flintlock.HostClient.
func (h *stubHost) SSHProxy(ctx context.Context, uid string) (io.ReadWriteCloser, error) {
	h.mu.Lock()
	h.proxies++
	h.mu.Unlock()
	if h.sshProxy == nil {
		return nil, flintlock.ErrUnimplemented
	}
	return h.sshProxy(ctx, uid)
}

// Close implements flintlock.HostClient.
func (h *stubHost) Close() error { return nil }

// counts returns how many exec streams, ssh proxy streams and MicroVM
// probes the transport asked for.
func (h *stubHost) counts() (execs, proxies, probes int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.execs, h.proxies, h.vmProbes
}

// exitCode is an exit_code response.
func exitCode(code int32) *execv1.ExecCommandResponse {
	return &execv1.ExecCommandResponse{Payload: &execv1.ExecCommandResponse_ExitCode{ExitCode: code}}
}

// stdout is a stdout response.
func stdout(s string) *execv1.ExecCommandResponse {
	return &execv1.ExecCommandResponse{Payload: &execv1.ExecCommandResponse_Stdout{Stdout: []byte(s)}}
}

// stderr is a stderr response.
func stderr(s string) *execv1.ExecCommandResponse {
	return &execv1.ExecCommandResponse{Payload: &execv1.ExecCommandResponse_Stderr{Stderr: []byte(s)}}
}

// errorPayload is an error response, which is how a Host reports that a
// command never ran or was stopped from underneath it.
func errorPayload(msg string) *execv1.ExecCommandResponse {
	return &execv1.ExecCommandResponse{Payload: &execv1.ExecCommandResponse_Error{Error: msg}}
}
