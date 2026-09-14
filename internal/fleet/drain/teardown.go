package drain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// RenderInput.Options keys the teardown script reads (FL-082, FL-083). A key
// is present with the value "true" only when its flag was passed.
const (
	OptionPurge     = "purge"
	OptionTerminate = "terminate"
)

// StepPools and StepWait name the teardown steps that run before any Host
// script, in the failure summary.
const (
	StepPools fleet.Step = "delete_pools"
	StepWait  fleet.Step = "wait_microvms"
)

// TeardownConfig is what NewTeardown needs.
type TeardownConfig struct {
	// Pools is the Pool Manager's PoolAdmin. Required.
	Pools poolmgr.PoolAdmin
	// Profiles are the declared Profiles whose Pools are deleted.
	Profiles []config.Profile
	// Hosts dials Hosts to watch their MicroVMs go. Required.
	Hosts flintlock.Dialer
	// Scripts renders the teardown script and Remote runs it. Required.
	Scripts fleet.Scripts
	Remote  fleet.Remote
	// Render is the base input for the teardown script; Instance, Inventory
	// and Options are filled in per Host.
	Render fleet.RenderInput
	// EC2 terminates instances; only used with the terminate flag.
	EC2 fleet.EC2
	// InventoryPath is the Inventory file removed at the end.
	InventoryPath string
	// Timeout bounds the wait for the Pools' MicroVMs to be removed.
	Timeout time.Duration
	// PollInterval defaults to one second.
	PollInterval time.Duration
	// Out receives script output and progress; nil discards.
	Out io.Writer
}

// Teardown is the fleet.Teardown.
type Teardown struct {
	cfg TeardownConfig
}

var _ fleet.Teardown = (*Teardown)(nil)

// NewTeardown checks cfg and returns a Teardown.
func NewTeardown(cfg TeardownConfig) (*Teardown, error) {
	switch {
	case cfg.Pools == nil:
		return nil, errors.New("teardown: a Pool Manager client is required")
	case cfg.Hosts == nil:
		return nil, errors.New("teardown: a Host dialer is required")
	case cfg.Scripts == nil || cfg.Remote == nil:
		return nil, errors.New("teardown: scripts and remote execution are required")
	case cfg.Timeout <= 0:
		return nil, errors.New("teardown: the timeout has to be positive")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	return &Teardown{cfg: cfg}, nil
}

// Error is a teardown that failed on one or more Hosts or steps.
type Error struct {
	Failures []fleet.Failure
}

func (e *Error) Error() string {
	lines := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		lines = append(lines, fmt.Sprintf("  %s: step %s: %v", f.Host, f.Step, f.Err))
	}
	return fmt.Sprintf("teardown failed with %d failure(s):\n%s", len(lines), strings.Join(lines, "\n"))
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//# The Fleet Controller SHALL provide a teardown command that
//# deletes the declared Pools through the Pool Manager, waits for their
//# MicroVMs to be removed, stops and disables the installed services on every
//# Host and removes the Inventory.

// Teardown deletes every declared Pool, waits until no Host has a MicroVM
// in the Pools' namespaces, runs the teardown script on every Host, then,
// only with the terminate flag, terminates the instances, and finally
// removes the Inventory file. A Pool that cannot be deleted, or MicroVMs
// that outlive the timeout, stop it before any service is touched; a Host
// whose script fails is reported and the others continue, and the Inventory
// is kept so that a re-run knows the Hosts.
func (t *Teardown) Teardown(ctx context.Context, inv *fleet.Inventory, opts fleet.TeardownOptions) error {
	if inv == nil {
		inv = &fleet.Inventory{}
	}
	if err := t.deletePools(ctx); err != nil {
		return err
	}
	if err := t.waitMicroVMs(ctx, inv); err != nil {
		return err
	}

	var failures []fleet.Failure
	for i := range inv.Hosts {
		h := &inv.Hosts[i]
		if err := t.runScript(ctx, inv, h, opts); err != nil {
			failures = append(failures, fleet.Failure{Host: h.Name, Step: fleet.StepTeardown, Err: err})
		}
	}
	if len(failures) > 0 {
		return &Error{Failures: failures}
	}

	if err := t.terminate(ctx, inv, opts); err != nil {
		return err
	}
	if t.cfg.InventoryPath != "" {
		if err := inventory.Remove(t.cfg.InventoryPath); err != nil {
			return err
		}
		fmt.Fprintf(t.cfg.Out, "removed the Inventory %s\n", t.cfg.InventoryPath)
	}
	return nil
}

// deletePools deletes each declared Pool; one the Pool Manager does not
// know is already gone.
func (t *Teardown) deletePools(ctx context.Context) error {
	var failures []fleet.Failure
	for i := range t.cfg.Profiles {
		p := &t.cfg.Profiles[i]
		ref := poolmgr.PoolRef{Name: p.Pool.Name, Namespace: p.Pool.Namespace}
		err := t.cfg.Pools.DeletePool(ctx, ref)
		switch {
		case err == nil:
			fmt.Fprintf(t.cfg.Out, "deleted pool %s\n", ref)
		case errors.Is(err, poolmgr.ErrNotFound):
			fmt.Fprintf(t.cfg.Out, "pool %s already gone\n", ref)
		default:
			failures = append(failures, fleet.Failure{Host: ref.String(), Step: StepPools, Err: err})
		}
	}
	if len(failures) > 0 {
		return &Error{Failures: failures}
	}
	return nil
}

// namespaces are the Runner namespaces of the declared Pools.
func (t *Teardown) namespaces() []string {
	var out []string
	for i := range t.cfg.Profiles {
		if ns := t.cfg.Profiles[i].Pool.Namespace; !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// waitMicroVMs polls every Host until none has a MicroVM in the Pools'
// namespaces, or the timeout elapses.
func (t *Teardown) waitMicroVMs(ctx context.Context, inv *fleet.Inventory) error {
	nss := t.namespaces()
	if len(nss) == 0 || len(inv.Hosts) == 0 {
		return nil
	}
	clients := map[string]flintlock.HostClient{}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for i := range inv.Hosts {
		h := &inv.Hosts[i]
		c, err := t.cfg.Hosts.Dial(ctx, inventory.Endpoint(h))
		if err != nil {
			return &Error{Failures: []fleet.Failure{{Host: h.Name, Step: StepWait, Err: err}}}
		}
		clients[h.Name] = c
	}
	wctx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()
	for {
		remaining := map[string]int{}
		var lastErr error
		for name, c := range clients {
			for _, ns := range nss {
				vms, err := c.ListMicroVMs(wctx, ns)
				if err != nil {
					lastErr = fmt.Errorf("%s: ListMicroVMs: %w", name, err)
					remaining[name]++
					continue
				}
				remaining[name] += len(vms)
			}
		}
		total := 0
		for _, n := range remaining {
			total += n
		}
		if total == 0 {
			fmt.Fprintln(t.cfg.Out, "every MicroVM of the declared pools is removed")
			return nil
		}
		if !sleep(wctx, t.cfg.PollInterval) {
			var failures []fleet.Failure
			for i := range inv.Hosts {
				name := inv.Hosts[i].Name
				if n := remaining[name]; n > 0 {
					err := fmt.Errorf("%d MicroVM(s) still present after %s; services left running", n, t.cfg.Timeout)
					if lastErr != nil {
						err = fmt.Errorf("%w (last error: %v)", err, lastErr)
					}
					failures = append(failures, fleet.Failure{Host: name, Step: StepWait, Err: err})
				}
			}
			return &Error{Failures: failures}
		}
	}
}

// runScript renders and runs the teardown script on one Host. The purge
// and terminate options are set only when their flags were passed.
func (t *Teardown) runScript(ctx context.Context, inv *fleet.Inventory, h *config.HostEntry, opts fleet.TeardownOptions) error {
	in := t.cfg.Render
	in.Instance = inventory.InstanceOf(h)
	in.Inventory = *inv
	in.Options = map[string]string{}
	//= docs/requirements/06-fleet.md#drain-and-teardown
	//# The teardown command SHALL NOT remove the thin pool or its
	//# backing device unless the purge flag is passed explicitly.
	if opts.Purge {
		in.Options[OptionPurge] = "true"
	}
	if opts.Terminate {
		in.Options[OptionTerminate] = "true"
	}
	script, err := t.cfg.Scripts.Render(fleet.StepTeardown, in)
	if err != nil {
		return fmt.Errorf("rendering the teardown script: %w", err)
	}
	res, err := t.cfg.Remote.Run(ctx, in.Instance, script, t.cfg.Out)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("teardown script exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	fmt.Fprintf(t.cfg.Out, "stopped and disabled the services on %s\n", h.Name)
	return nil
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//# The teardown command SHALL NOT terminate EC2 instances unless
//# the terminate flag is passed explicitly.

// terminate terminates the Inventory's instances, and only with the
// terminate flag.
func (t *Teardown) terminate(ctx context.Context, inv *fleet.Inventory, opts fleet.TeardownOptions) error {
	if !opts.Terminate {
		return nil
	}
	if t.cfg.EC2 == nil {
		return errors.New("teardown: --terminate needs EC2 access; the instances were left running")
	}
	ids := make([]string, 0, len(inv.Hosts))
	for i := range inv.Hosts {
		ids = append(ids, inventory.InstanceID(&inv.Hosts[i]))
	}
	if len(ids) == 0 {
		return nil
	}
	if err := t.cfg.EC2.TerminateInstances(ctx, ids); err != nil {
		return fmt.Errorf("teardown: terminating %v: %w", ids, err)
	}
	fmt.Fprintf(t.cfg.Out, "terminated %s\n", strings.Join(ids, " "))
	return nil
}
