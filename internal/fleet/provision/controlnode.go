package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// DefaultPoolManagerReadyTimeout bounds the wait for the Pool Manager daemon
// to answer after an install or reload (FL-054).
const DefaultPoolManagerReadyTimeout = 2 * time.Minute

// ControlNodeOptions configure a ControlNode.
type ControlNodeOptions struct {
	Scripts fleet.Scripts
	// Remote runs the control_node script on the Control Node itself: a
	// LocalRemote when the Fleet Controller runs there, or an SSH Remote.
	Remote fleet.Remote
	// Self is the Control Node as Remote addresses it.
	Self fleet.Instance
	// Fleet pins the Pool Manager version and says how the daemon reaches
	// flintlockd.
	Fleet config.Fleet
	// Options are passed to the script: scripts.OptionPoolManagerListen,
	// OptionPoolManagerCert, OptionPoolManagerKey and OptionHostCAFile. The
	// last three are also the Hosts' TLS settings in battery (FL-127).
	Options map[string]string
	// PoolManager is dialled at the daemon's endpoint to check it answers
	// (FL-054), and to take Hosts gone from the Inventory out of its Pools
	// (FL-130).
	PoolManager poolmgr.PoolAdmin
	// Hosts is the daemon's HostAdmin service, through which the
	// Inventory's Hosts are registered (FL-127 to FL-132).
	Hosts poolmgr.HostAdmin
	// HostSync tunes the retries of a refused Host removal; zero values
	// take poolmgr's defaults.
	HostSync poolmgr.HostSyncOptions
	// ReadyTimeout bounds that check; zero means
	// DefaultPoolManagerReadyTimeout.
	ReadyTimeout time.Duration
	Out          io.Writer
}

// ControlNode implements fleet.ControlNode.
type ControlNode struct {
	o ControlNodeOptions
}

var _ fleet.ControlNode = (*ControlNode)(nil)

// NewControlNode returns a ControlNode.
func NewControlNode(o ControlNodeOptions) (*ControlNode, error) {
	if o.Scripts == nil || o.Remote == nil || o.PoolManager == nil || o.Hosts == nil {
		return nil, errors.New("provision: Scripts, Remote, PoolManager and Hosts are required")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = DefaultPoolManagerReadyTimeout
	}
	return &ControlNode{o: o}, nil
}

// InstallPoolManager installs the pinned daemon, waits until it answers and
// registers inv's Hosts with it.
func (c *ControlNode) InstallPoolManager(ctx context.Context, inv *fleet.Inventory) error {
	return c.apply(ctx, inv)
}

// ReloadPoolManager does what InstallPoolManager does. The script changes
// nothing the Inventory decides, so it restarts the daemon only for a new
// version, configuration or unit (FL-133); the Hosts are brought in step
// over HostAdmin while the daemon runs.
func (c *ControlNode) ReloadPoolManager(ctx context.Context, inv *fleet.Inventory) error {
	return c.apply(ctx, inv)
}

func (c *ControlNode) apply(ctx context.Context, inv *fleet.Inventory) error {
	if inv == nil {
		inv = &fleet.Inventory{}
	}
	sc, err := c.o.Scripts.Render(fleet.StepControlNode, fleet.RenderInput{
		Fleet:     c.o.Fleet,
		Instance:  c.o.Self,
		Inventory: *inv,
		Options:   c.o.Options,
	})
	if err != nil {
		return err
	}
	if tok := c.o.Fleet.Flintlockd.Token; tok != "" {
		sc.Stdin = scripts.EncodeSecrets(map[string][]byte{scripts.SecretFlintlockdToken: []byte(tok)})
	}
	rr, err := c.o.Remote.Run(ctx, c.o.Self, sc, c.o.Out)
	if err != nil {
		return fmt.Errorf("pool manager install: %w", err)
	}
	if rr.ExitCode != 0 {
		return fmt.Errorf("pool manager install: exit status %d: %s", rr.ExitCode, lastLine(rr.Stderr))
	}
	if err := c.waitReady(ctx); err != nil {
		return err
	}
	return c.syncHosts(ctx, inv)
}

//= docs/requirements/06-fleet.md#pool-manager-install
//# The Fleet Controller SHALL verify after installation that the
//# Pool Manager daemon answers `ListPools`

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=todo
//= tracking-issue=8
//# and reports every Host in the Inventory as reachable.

// waitReady polls ListPools until the daemon answers or the ready timeout
// elapses. battery's API has no per-host reachability report, so the
// second half of FL-054 waits on upstream.
func (c *ControlNode) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.o.ReadyTimeout)
	defer cancel()
	for {
		_, err := c.o.PoolManager.ListPools(ctx, "")
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pool manager did not answer ListPools within %s: %w", c.o.ReadyTimeout, err)
		case <-time.After(time.Second):
		}
	}
}

// syncHosts registers inv's Hosts with the daemon, updates the ones it
// holds differently and removes the ones gone from inv (FL-127 to FL-132),
// and reports what it did. A Host the daemon still refuses to remove is
// reported and is not an error.
func (c *ControlNode) syncHosts(ctx context.Context, inv *fleet.Inventory) error {
	want := make([]poolmgr.Host, 0, len(inv.Hosts))
	tls := c.hostTLS()
	for _, e := range inv.Hosts {
		//= docs/requirements/06-fleet.md#pool-manager-install
		//# The Fleet Controller SHALL name each Host identically in the
		//# Pool Manager's host list and in the Inventory, because the Runner joins
		//# the two by name when resolving Placement.
		want = append(want, poolmgr.Host{Name: e.Name, Address: e.Endpoint, TLS: tls})
	}
	res, err := poolmgr.SyncHosts(ctx, c.o.Hosts, c.o.PoolManager, want, c.o.HostSync)
	if res != nil {
		for _, name := range res.Added {
			fmt.Fprintf(c.o.Out, "registered Host %s with the Pool Manager\n", name)
		}
		for _, name := range res.Updated {
			fmt.Fprintf(c.o.Out, "updated Host %s in the Pool Manager\n", name)
		}
		for _, name := range res.Removed {
			fmt.Fprintf(c.o.Out, "removed Host %s, which left the Inventory, from the Pool Manager\n", name)
		}
		for _, k := range res.Kept {
			fmt.Fprintf(c.o.Out, "the Pool Manager still holds Host %s, which left the Inventory: %v; the next run tries again\n", k.Name, k.Reason)
		}
	}
	if err != nil {
		return fmt.Errorf("pool manager hosts: %w", err)
	}
	return nil
}

// hostTLS is how the daemon reaches every Host's flintlockd, the same for
// all of them: no TLS for an insecure fleet; otherwise the fleet's CA, and,
// where flintlockd authenticates its clients by certificate (FL-024), the
// daemon's own certificate and key, which come from the same authority.
// The files are on the Control Node, where the daemon runs.
func (c *ControlNode) hostTLS() poolmgr.HostTLS {
	f := c.o.Fleet.Flintlockd
	if f.Insecure {
		return poolmgr.HostTLS{Insecure: true}
	}
	t := poolmgr.HostTLS{CAFile: c.o.Options[scripts.OptionHostCAFile]}
	mutual := f.TLS.CAFile != "" && f.TLS.CertFile != "" && f.TLS.KeyFile != ""
	if mutual && t.CAFile != "" {
		t.CertFile = c.o.Options[scripts.OptionPoolManagerCert]
		t.KeyFile = c.o.Options[scripts.OptionPoolManagerKey]
	}
	return t
}
