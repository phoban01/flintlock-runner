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
	// Fleet pins the Pool Manager version and carries the flintlockd token.
	Fleet config.Fleet
	// Options are passed to the script: scripts.OptionPoolManagerListen,
	// OptionPoolManagerCert, OptionPoolManagerKey and OptionHostCAFile.
	Options map[string]string
	// PoolManager is dialled at the daemon's endpoint to check it answers
	// (FL-054).
	PoolManager poolmgr.PoolAdmin
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
	if o.Scripts == nil || o.Remote == nil || o.PoolManager == nil {
		return nil, errors.New("provision: Scripts, Remote and PoolManager are required")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = DefaultPoolManagerReadyTimeout
	}
	return &ControlNode{o: o}, nil
}

//= docs/requirements/06-fleet.md#pool-manager-install
//# The Fleet Controller SHALL install the pinned Pool Manager daemon
//# on the Control Node with a host list generated from the Inventory that
//# names every Host with its `flintlockd` endpoint, token and TLS settings.

// InstallPoolManager installs the pinned daemon with a host list generated
// from inv and waits until it answers.
func (c *ControlNode) InstallPoolManager(ctx context.Context, inv *fleet.Inventory) error {
	return c.apply(ctx, inv)
}

//= docs/requirements/06-fleet.md#pool-manager-install
//# When run again after the Inventory changes, the Fleet Controller
//# SHALL regenerate the Pool Manager's host list and reload the daemon without
//# interrupting existing Leases.

// ReloadPoolManager regenerates the host list and applies it. The script
// restarts the daemon only when the host list changed and never touches
// its Lease database, so Leases survive the restart.
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
	return c.waitReady(ctx)
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
