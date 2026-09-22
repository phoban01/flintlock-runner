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
func newGuestAccess(cfg *config.Config, client *poolClient, inv *inventoryView, log *slog.Logger) guestAccess {
	if client.kube == nil {
		return guestAccess{
			dialer:     flintlock.NewDialer(flintlock.WithCallDeadline(cfg.Scheduler.HostCallDeadline)),
			endpoints:  inventory.Endpoints(cfg.Inventory.Hosts),
			transports: transport.NewFactory(transport.WithLogger(log)),
			inventory:  inv,
		}
	}
	k := client.kube
	return guestAccess{
		dialer: noHostDialer{},
		transports: transport.NewFactory(transport.WithLogger(log),
			transport.WithKubeExec(k.config, k.namespace)),
		inventory: executor.NewVirtualNodeInventory(k.client.CoreV1().Nodes(), cfg.HostServices.HTTPCache.Upstreams, log),
		options:   []executor.Option{executor.WithGuestTransport(transport.KindKubeExec)},
	}
}

// noHostDialer is the Host dialer of a cluster fleet's Runner. The
// configuration lists no Host for it (the Inventory is refused with the
// Kubernetes pool backend), so it is never called; should anything try, it
// refuses rather than connect.
type noHostDialer struct{}

// Dial implements flintlock.Dialer.
func (noHostDialer) Dial(_ context.Context, ep flintlock.Endpoint) (flintlock.HostClient, error) {
	return nil, fmt.Errorf("host %s: the runner of a cluster fleet connects to no host", ep.Name)
}
