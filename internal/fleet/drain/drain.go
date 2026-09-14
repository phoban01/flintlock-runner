package drain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

const defaultPollInterval = time.Second

// LeaseReporter reports how many leased MicroVMs a Host carries, as the Pool
// Manager sees them. pools are the Pools that could have placed on the Host;
// a reporter that cannot tell Hosts apart counts every lease in them.
type LeaseReporter interface {
	LeasedOn(ctx context.Context, host string, pools []poolmgr.PoolRef) (int, error)
}

// PoolLeases is the LeaseReporter over the PoolAdmin API. The battery API
// reports leased MicroVMs per Pool, not per Host, so it counts every lease
// in the Pools the Host was in: zero leases there is proof that none is on
// the Host, and a lease elsewhere in those Pools makes drain wait longer
// than it has to, never shorter.
type PoolLeases struct {
	Pools poolmgr.PoolAdmin
}

// LeasedOn implements LeaseReporter.
func (p PoolLeases) LeasedOn(ctx context.Context, _ string, pools []poolmgr.PoolRef) (int, error) {
	n := 0
	for _, ref := range pools {
		pool, err := p.Pools.GetPool(ctx, ref)
		if errors.Is(err, poolmgr.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		n += int(pool.Status.Leased)
	}
	return n, nil
}

// InspectorLeases is the exact LeaseReporter for a Pool Manager that exposes
// its MicroVM records, as the fake does (poolmgr.Inspector): it counts the
// LEASED MicroVMs on the Host.
type InspectorLeases struct {
	Inspector poolmgr.Inspector
}

// LeasedOn implements LeaseReporter.
func (i InspectorLeases) LeasedOn(_ context.Context, host string, pools []poolmgr.PoolRef) (int, error) {
	n := 0
	for _, vm := range i.Inspector.VMs() {
		if vm.Host == host && vm.LeaseID != "" && slices.Contains(pools, vm.Pool) {
			n++
		}
	}
	return n, nil
}

// Config is what NewDrainer needs.
type Config struct {
	// Pools is the Pool Manager's PoolAdmin. Required.
	Pools poolmgr.PoolAdmin
	// Leases reports leased MicroVMs on the drained Host. Nil means
	// PoolLeases over Pools.
	Leases LeaseReporter
	// Namespace restricts drain to the Pools of one namespace; empty drains
	// the Host out of every Pool the Pool Manager lists.
	Namespace string
	// Stop, when set, is run once no leased MicroVM remains on the Host, to
	// stop its services (the drain script). It is never run while a Lease
	// remains (FL-084).
	Stop func(ctx context.Context, host string) error
	// PollInterval is how often leases are counted; default one second.
	PollInterval time.Duration
	// Out receives progress lines; nil discards.
	Out io.Writer
}

// Drainer is the fleet.Drainer.
type Drainer struct {
	cfg Config
}

var _ fleet.Drainer = (*Drainer)(nil)

// NewDrainer checks cfg and returns a Drainer.
func NewDrainer(cfg Config) (*Drainer, error) {
	if cfg.Pools == nil {
		return nil, errors.New("drain: a Pool Manager client is required")
	}
	if cfg.Leases == nil {
		cfg.Leases = PoolLeases{Pools: cfg.Pools}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	return &Drainer{cfg: cfg}, nil
}

// ErrLeasesRemain is returned by Drain when the drain timeout elapses with a
// leased MicroVM still on the Host.
var ErrLeasesRemain = errors.New("drain: leased MicroVMs remain on the host")

//= docs/requirements/06-fleet.md#drain-and-teardown
//# The Fleet Controller SHALL provide a drain command that removes
//# a Host from every Pool's `flintlock_hosts` so that the Pool Manager places
//# nothing new there, and waits until the Pool Manager reports no leased
//# MicroVM on that Host or the configured drain timeout elapses.

//= docs/requirements/06-fleet.md#drain-and-teardown
//# The drain command SHALL NOT stop `flintlockd` on the drained
//# Host while any leased MicroVM remains on it.

// Drain removes host from the flintlock_hosts of every Pool that lists it,
// then counts the leased MicroVMs on it until there are none or timeout
// elapses. Only when none remains does it run Stop; when the timeout elapses
// first it returns an error wrapping ErrLeasesRemain and leaves the Host's
// services running.
func (d *Drainer) Drain(ctx context.Context, host string, timeout time.Duration) error {
	if timeout <= 0 {
		return errors.New("drain: the drain timeout has to be positive")
	}
	pools, err := d.cfg.Pools.ListPools(ctx, d.cfg.Namespace)
	if err != nil {
		return fmt.Errorf("drain: ListPools: %w", err)
	}
	var affected []poolmgr.PoolRef
	for _, pool := range pools {
		if !slices.Contains(pool.Spec.FlintlockHosts, host) {
			continue
		}
		spec := pool.Spec
		spec.FlintlockHosts = slices.DeleteFunc(slices.Clone(spec.FlintlockHosts), func(h string) bool { return h == host })
		if _, err := d.cfg.Pools.UpdatePool(ctx, spec); err != nil {
			return fmt.Errorf("drain: removing %s from pool %s: %w", host, spec.Ref, err)
		}
		affected = append(affected, spec.Ref)
		fmt.Fprintf(d.cfg.Out, "removed %s from pool %s\n", host, spec.Ref)
	}

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		n, err := d.cfg.Leases.LeasedOn(wctx, host, affected)
		if err == nil && n == 0 {
			break
		}
		if err != nil {
			fmt.Fprintf(d.cfg.Out, "counting leases on %s: %v\n", host, err)
		} else {
			fmt.Fprintf(d.cfg.Out, "waiting for %d leased MicroVM(s) on %s\n", n, host)
		}
		if !sleep(wctx, d.cfg.PollInterval) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %s still has leased MicroVMs after %s; flintlockd left running", ErrLeasesRemain, host, timeout)
		}
	}
	fmt.Fprintf(d.cfg.Out, "no leased MicroVM remains on %s\n", host)
	if d.cfg.Stop == nil {
		return nil
	}
	if err := d.cfg.Stop(ctx, host); err != nil {
		return fmt.Errorf("drain: stopping services on %s: %w", host, err)
	}
	return nil
}

// sleep waits d or until ctx ends, reporting whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
