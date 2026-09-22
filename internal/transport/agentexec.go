package transport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//# The `agent-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// KindAgentExec is the Guest Transport of the claim design of a cluster
// fleet: the exec transport pointed at the Exec Agent of the Host named in
// the Job's claim, which authorizes the request against the claim and
// relays it to MicroVMExec on its local flintlockd (KF-185). Its framing,
// its exit status, its cancellation and its liveness watch are the exec
// transport's, because the Exec Agent serves flintlock's own exec service
// and relays it message for message (KF-188).
const KindAgentExec Kind = "agent-exec"

// AgentExecConfig is what the Runner reaches Exec Agents with.
type AgentExecConfig struct {
	// CAFile is the certificate authority an Exec Agent's serving
	// certificate is verified against (KF-186). It is required: an Exec
	// Agent's certificate is issued for the fleet, not by a public
	// authority.
	CAFile string
	// TokenFile is the Runner's ServiceAccount token, the file the kubelet
	// projects into the Runner's pod. It is read again for every call, so
	// that a rotated token is used as soon as the kubelet writes it.
	TokenFile string
	// CallDeadline bounds every unary call to an Exec Agent (HO-003); zero
	// keeps the Host client's default.
	CallDeadline time.Duration
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//# The `agent-exec` Guest Transport SHALL authenticate to the Exec
//# Agent with the Runner's ServiceAccount token and SHALL verify the agent's
//# serving certificate against the configured certificate authority.

// tokenFileCredentials send the token in a file as a bearer token on every
// call. The file is read each time rather than once, because the kubelet
// rotates a projected ServiceAccount token in place, and a token read at
// start would stop working within the hour. They require transport
// security, so gRPC never sends them over a plaintext connection.
type tokenFileCredentials struct{ path string }

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (c tokenFileCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("reading the service account token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, fmt.Errorf("the service account token file %s is empty", c.path)
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials.
func (tokenFileCredentials) RequireTransportSecurity() bool { return true }

// AgentHosts hands out Host clients of Exec Agents by the Host and address
// a claim names. A client is dialled on first use and shared by every Job
// on that Host, so that each Stage is a stream on one connection (EX-050);
// it is closed once the last Job holding it releases it.
type AgentHosts struct {
	dialer flintlock.Dialer
	caFile string

	mu      sync.Mutex
	clients map[agentKey]*agentClient
	closed  bool
}

type agentKey struct{ host, address string }

type agentClient struct {
	client flintlock.HostClient
	users  int
}

// NewAgentHosts checks the configuration and returns the client pool.
// Nothing is dialled until a Job asks for a Host.
func NewAgentHosts(cfg AgentExecConfig) (*AgentHosts, error) {
	switch {
	case cfg.CAFile == "":
		return nil, errors.New("transport: agent-exec needs the certificate authority of the Exec Agents' serving certificates")
	case cfg.TokenFile == "":
		return nil, errors.New("transport: agent-exec needs the Runner's service account token file")
	}
	if _, err := os.Stat(cfg.CAFile); err != nil {
		return nil, fmt.Errorf("transport: agent-exec certificate authority: %w", err)
	}
	return &AgentHosts{
		dialer: flintlock.NewDialer(
			flintlock.WithCallDeadline(cfg.CallDeadline),
			flintlock.WithPerRPCCredentials(tokenFileCredentials{path: cfg.TokenFile}),
		),
		caFile:  cfg.CAFile,
		clients: map[agentKey]*agentClient{},
	}, nil
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//# The `agent-exec` Guest Transport SHALL authenticate to the Exec
//# Agent with the Runner's ServiceAccount token and SHALL verify the agent's
//# serving certificate against the configured certificate authority.

// Lease returns the client of the Exec Agent at address on the named Host,
// with a function that releases it. The client speaks TLS, verified
// against the configured certificate authority and never skipped, and
// sends the Runner's token on every call.
func (a *AgentHosts) Lease(host, address string) (flintlock.HostClient, func(), error) {
	if address == "" {
		return nil, nil, fmt.Errorf("transport: host %s: the claim names no exec agent address", host)
	}
	key := agentKey{host: host, address: address}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, nil, errors.New("transport: the exec agent clients are closed")
	}
	c, ok := a.clients[key]
	if !ok {
		client, err := a.dialer.Dial(context.Background(), flintlock.Endpoint{
			Name:    host,
			Address: address,
			TLS:     flintlock.TLSOptions{CAFile: a.caFile},
		})
		if err != nil {
			return nil, nil, err
		}
		c = &agentClient{client: client}
		a.clients[key] = c
	}
	c.users++
	var once sync.Once
	return c.client, func() { once.Do(func() { a.release(key) }) }, nil
}

// release drops one user of a client and closes it after the last.
func (a *AgentHosts) release(key agentKey) {
	a.mu.Lock()
	c, ok := a.clients[key]
	if !ok {
		a.mu.Unlock()
		return
	}
	c.users--
	if c.users > 0 {
		a.mu.Unlock()
		return
	}
	delete(a.clients, key)
	a.mu.Unlock()
	_ = c.client.Close()
}

// Close closes every client; streams still open on them fail.
func (a *AgentHosts) Close() error {
	a.mu.Lock()
	clients := a.clients
	a.clients, a.closed = map[agentKey]*agentClient{}, true
	a.mu.Unlock()
	var errs []error
	for _, c := range clients {
		errs = append(errs, c.client.Close())
	}
	return errors.Join(errs...)
}
