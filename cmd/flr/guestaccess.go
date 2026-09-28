package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/executor"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/inventory"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// guestAccess is how the Runner reaches the guests of its Jobs and the Host
// Services they use: the Host dialer and the endpoints of the Host Registry,
// the Guest Transport factory, the Executor's view of a Placement's Host
// Services, and the Executor options that go with them.
type guestAccess struct {
	dialer     flintlock.Dialer
	endpoints  []flintlock.Endpoint
	transports transport.Factory
	inventory  executor.InventoryLookup
	options    []executor.Option
	// agents are the Exec Agent clients of the claim design, nil for the
	// other backends. The Runner closes them on the way out.
	agents *transport.AgentHosts
}

// close closes what the guest access holds of its own: the Exec Agent
// clients of the claim design.
func (g guestAccess) close() {
	if g.agents != nil {
		_ = g.agents.Close()
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-allocation
//# Where the Kubernetes pool backend is configured, the Executor
//# SHALL use the `kube-exec` Guest Transport for every Profile

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//# Where the `kube-exec` Guest Transport is configured, the Runner
//# SHALL NOT open any connection to a Host.

// newGuestAccess chooses the guest access of the configured Pool backend.
// Battery's Runner dials every Inventory Host and runs each Profile over its
// own transport, finding Host Services in the Inventory. A cluster fleet's
// Runner reaches everything through the API server, with the Kubernetes
// pool backend's own client configuration: every Profile runs over
// kube-exec, Host Services come from the Placement's Virtual Node, and the
// Host Registry is empty and has a dialer that refuses, so that nothing
// the Runner does can open a connection to a Host.
//
// The claim design's Runner runs every Profile over agent-exec, through
// the Exec Agent of the Host each Job's claim names, on the connection
// that claim dials; it reads Host Services from that Host's Node, and its
// Host Registry is empty with a dialer that refuses, as a cluster fleet's.
func newGuestAccess(cfg *config.Config, client *poolClient, inv *inventoryView, log *slog.Logger) (guestAccess, error) {
	if client.claim != nil {
		return newClaimGuestAccess(cfg, client.claim, log)
	}
	if client.kube == nil {
		return guestAccess{
			dialer:     flintlock.NewDialer(flintlock.WithCallDeadline(cfg.Scheduler.HostCallDeadline)),
			endpoints:  inventory.Endpoints(cfg.Inventory.Hosts),
			transports: transport.NewFactory(transport.WithLogger(log)),
			inventory:  inv,
		}, nil
	}
	k := client.kube
	return guestAccess{
		dialer: noHostDialer{},
		transports: transport.NewFactory(transport.WithLogger(log),
			transport.WithKubeExec(k.config, k.namespace)),
		inventory: executor.NewVirtualNodeInventory(k.client.CoreV1().Nodes(), cfg.HostServices.HTTPCache.Upstreams, log),
		options:   []executor.Option{executor.WithGuestTransport(transport.KindKubeExec)},
	}, nil
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//# Where the claim backend is configured, the Executor SHALL run
//# each Stage through the Exec Agent of the Host named in the Job's claim,
//# with the Stage script on standard input.

// newClaimGuestAccess is the guest access of the claim pool backend. The
// agent-exec clients dial each claim's Exec Agent through the claim the
// backend holds for the Job (KF-186), and the Executor runs every Profile
// over them. The Host Services of a Job are read from the Node of its
// claim's Host (KF-189).
func newClaimGuestAccess(cfg *config.Config, c *claimAccess, log *slog.Logger) (guestAccess, error) {
	agents, err := transport.NewAgentHosts(c.dialAgent, transport.AgentExecConfig{CallDeadline: cfg.Scheduler.HostCallDeadline})
	if err != nil {
		return guestAccess{}, err
	}
	return guestAccess{
		dialer:     noHostDialer{},
		transports: transport.NewFactory(transport.WithLogger(log)),
		inventory:  executor.NewHostNodeInventory(c.nodes, cfg.HostServices.HTTPCache.Upstreams, log),
		options:    []executor.Option{executor.WithAgentExec(agents)},
		agents:     agents,
	}, nil
}

// noHostDialer is the Host dialer of a cluster fleet's Runner. The
// configuration lists no Host for it (the Inventory is refused with the
// Kubernetes and the claim pool backends), so it is never called; should
// anything try, it refuses rather than connect.
type noHostDialer struct{}

// Dial implements flintlock.Dialer.
func (noHostDialer) Dial(_ context.Context, ep flintlock.Endpoint) (flintlock.HostClient, error) {
	return nil, fmt.Errorf("host %s: the runner of a cluster fleet connects to no host", ep.Name)
}
