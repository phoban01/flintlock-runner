package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//# The `agent-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// KindAgentExec is the Guest Transport of the claim design of a cluster
// fleet: the exec transport pointed at battery-operator's Exec Agent on the
// Host named in the Job's claim, which authorizes the request against the
// claim and relays it to MicroVMExec on the Host's flintlockd (KF-185). Its
// framing, its exit status, its cancellation and its liveness watch are the
// exec transport's, because the Exec Agent serves flintlock's own exec
// service and relays it message for message (KF-188).
const KindAgentExec Kind = "agent-exec"

// AgentExecConfig is what the Runner reaches Exec Agents with, beyond what
// each claim gives.
type AgentExecConfig struct {
	// CallDeadline bounds every unary call to an Exec Agent (HO-003); zero
	// keeps the Host client's default.
	CallDeadline time.Duration
}

// DialClaimAgent dials the Exec Agent of the claim whose lease id is
// leaseID, and returns the connection. The claim backend provides it: the
// connection is the one battery-operator's Client Library dials for that
// claim with Claim.Dial. It speaks TLS, verified against the Operator's
// serving CA, and carries the claim's own token on every call, renewed as
// the Client Library renews it. opts are the Runner's connection options
// (flintlock.ConnOptions); the dialler applies them before its own
// credentials, which take precedence.
type DialClaimAgent func(ctx context.Context, leaseID string, opts ...grpc.DialOption) (*grpc.ClientConn, error)

// AgentClaim is the claim that a client of an Exec Agent is asked for: what
// the Job's claim says about its MicroVM and where it runs (KF-151).
type AgentClaim struct {
	// LeaseID is the claim's lease id. With the claim backend it is the
	// name of the MicroVMClaim, so an implementation can find the claim's
	// own connection by it.
	LeaseID string
	// VMUID is the uid of the claimed MicroVM.
	VMUID string
	// Host is the name of the Host the MicroVM runs on, and Address the
	// address of that Host's Exec Agent, as the claim gives them.
	Host    string
	Address string
}

// AgentHosts hands out Host clients of Exec Agents, one for each claim.
// battery-operator's Exec Agent admits a request only with a claim token of
// the claim whose MicroVM it names (its EA-010 to EA-013), so a connection
// that carries one claim's token opens no other claim's MicroVM. Each Lease
// therefore dials a connection of its own, through the claim backend, and
// the Job's Stages are streams on it (EX-050); releasing it closes it.
type AgentHosts struct {
	dial DialClaimAgent
	opts []grpc.DialOption

	mu      sync.Mutex
	clients map[*agentClient]struct{}
	closed  bool
}

type agentClient struct {
	client flintlock.HostClient
}

// NewAgentHosts returns the clients of the Exec Agents of the claims that
// dial reaches. Nothing is dialled until a Job asks for its claim.
func NewAgentHosts(dial DialClaimAgent, cfg AgentExecConfig) (*AgentHosts, error) {
	if dial == nil {
		return nil, errors.New("transport: agent-exec needs the claim backend's dialler of Exec Agents")
	}
	return &AgentHosts{
		dial:    dial,
		opts:    flintlock.ConnOptions(flintlock.WithCallDeadline(cfg.CallDeadline)),
		clients: map[*agentClient]struct{}{},
	}, nil
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//# The `agent-exec` Guest Transport SHALL authenticate to the Exec
//# Agent with a claim token of the Job's claim and SHALL verify the agent's
//# serving certificate against the configured certificate authority.

// Lease returns a client of the Exec Agent of claim, on a connection of its
// own that the claim backend dials for the claim's lease id, and a function
// that releases it. The connection carries the claim's token and verifies
// the agent against the Operator's serving CA, both from the Client
// Library. The transport adds no credential of its own and never replaces
// the connection's: the Runner's own identity opens no MicroVM. ctx bounds
// the dial; the connection outlives the call.
func (a *AgentHosts) Lease(ctx context.Context, claim AgentClaim) (flintlock.HostClient, func(), error) {
	if claim.LeaseID == "" {
		return nil, nil, fmt.Errorf("transport: host %s: the claim has no lease id", claim.Host)
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return nil, nil, errors.New("transport: the exec agent clients are closed")
	}
	conn, err := a.dial(ctx, claim.LeaseID, a.opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: dialling the exec agent of claim %s on host %s: %w", claim.LeaseID, claim.Host, err)
	}
	address := claim.Address
	if address == "" {
		address = conn.Target()
	}
	c := &agentClient{client: flintlock.NewConnClient(claim.Host, address, conn)}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = c.client.Close()
		return nil, nil, errors.New("transport: the exec agent clients are closed")
	}
	a.clients[c] = struct{}{}
	a.mu.Unlock()

	var once sync.Once
	return c.client, func() { once.Do(func() { a.release(c) }) }, nil
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//# by calling `GetMicroVM` for the claim's MicroVM
//# with a claim token of that claim

// Probe asks the Exec Agent of claim about the claim's MicroVM, and returns
// nil only when it answers for it (KF-200). battery-operator's Exec Agent
// relays GetMicroVM to its Host's flintlockd once the claim authorizes it,
// so an answer proves the agent, the Host's flintlockd and the MicroVM at
// once. Each probe dials a connection of its own, with the claim's token
// as every Lease does, and closes it: a Host that stops answering is not
// hidden behind a connection the Job's Stages still hold open. ctx bounds
// the probe.
func (a *AgentHosts) Probe(ctx context.Context, claim AgentClaim) error {
	client, done, err := a.Lease(ctx, claim)
	if err != nil {
		return err
	}
	defer done()
	if _, err := client.GetMicroVM(ctx, claim.VMUID); err != nil {
		return fmt.Errorf("transport: the exec agent of claim %s on host %s did not answer for microvm %s: %w",
			claim.LeaseID, claim.Host, claim.VMUID, err)
	}
	return nil
}

// release closes one claim's client.
func (a *AgentHosts) release(c *agentClient) {
	a.mu.Lock()
	delete(a.clients, c)
	a.mu.Unlock()
	_ = c.client.Close()
}

// Close closes every client; streams still open on them fail.
func (a *AgentHosts) Close() error {
	a.mu.Lock()
	clients := a.clients
	a.clients, a.closed = map[*agentClient]struct{}{}, true
	a.mu.Unlock()
	var errs []error
	for c := range clients {
		errs = append(errs, c.client.Close())
	}
	return errors.Join(errs...)
}
