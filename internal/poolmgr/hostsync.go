package poolmgr

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Defaults for the HostSyncOptions a caller leaves zero.
const (
	// DefaultRemoveWait is how long SyncHosts retries a RemoveHost that the
	// Pool Manager refuses (FL-131).
	DefaultRemoveWait = time.Minute
	// DefaultRemoveRetryInterval paces those retries.
	DefaultRemoveRetryInterval = 2 * time.Second
)

// HostSyncOptions tune SyncHosts.
type HostSyncOptions struct {
	// RemoveWait bounds the retries of a refused RemoveHost; zero means
	// DefaultRemoveWait.
	RemoveWait time.Duration
	// RetryInterval paces them; zero means DefaultRemoveRetryInterval.
	RetryInterval time.Duration
}

// KeptHost is a Host SyncHosts meant to remove and the Pool Manager still
// refused to remove when the wait ended (FL-132).
type KeptHost struct {
	Name string
	// Reason is the Pool Manager's refusal.
	Reason error
}

// HostSyncResult is what SyncHosts changed, each list sorted by name.
type HostSyncResult struct {
	Added   []string
	Updated []string
	Removed []string
	// Kept are the Hosts gone from the Inventory that the Pool Manager still
	// holds because it refused to remove them.
	Kept []KeptHost
}

// SyncHosts brings the Hosts the Pool Manager holds in step with want: it
// adds the missing ones, updates the ones whose endpoint or TLS settings
// differ, and takes the others out of every Pool and removes them. Every
// Host is attempted whatever happens to the others. The error joins the
// failures to add, update or remove a Host or to change a Pool; a removal
// the Pool Manager still refuses when the wait ends is in Kept and is not
// an error. A failure to list the Hosts stops the sync and is returned
// alone.
func SyncHosts(ctx context.Context, hosts HostAdmin, pools PoolAdmin, want []Host, o HostSyncOptions) (*HostSyncResult, error) {
	if o.RemoveWait <= 0 {
		o.RemoveWait = DefaultRemoveWait
	}
	if o.RetryInterval <= 0 {
		o.RetryInterval = DefaultRemoveRetryInterval
	}
	listed, err := hosts.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the Pool Manager's Hosts: %w", err)
	}
	held := make(map[string]Host, len(listed))
	for _, st := range listed {
		held[st.Host.Name] = st.Host
	}
	wanted := make(map[string]Host, len(want))
	for _, h := range want {
		wanted[h.Name] = h
	}

	res := &HostSyncResult{}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(wanted)) {
		if err := syncHost(ctx, hosts, wanted[name], held, res); err != nil {
			errs = append(errs, err)
		}
	}

	var gone []string
	for name := range held {
		if _, ok := wanted[name]; !ok {
			gone = append(gone, name)
		}
	}
	slices.Sort(gone)
	if len(gone) > 0 {
		if err := dropFromPools(ctx, pools, gone); err != nil {
			errs = append(errs, err)
		}
		errs = append(errs, removeHosts(ctx, hosts, gone, o, res)...)
	}
	return res, errors.Join(errs...)
}

// syncHost adds or updates one wanted Host.
func syncHost(ctx context.Context, hosts HostAdmin, h Host, held map[string]Host, res *HostSyncResult) error {
	cur, ok := held[h.Name]
	if !ok {
		//= docs/requirements/06-fleet.md#pool-manager-install
		//# When the Pool Manager daemon answers, the Fleet Controller SHALL
		//# register with `AddHost` every Inventory Host that the Pool Manager does
		//# not hold, with its `flintlockd` endpoint and TLS settings.
		_, err := hosts.AddHost(ctx, h)
		switch {
		case err == nil:
			res.Added = append(res.Added, h.Name)
			return nil
		case errors.Is(err, ErrAlreadyExists):
			//= docs/requirements/06-fleet.md#pool-manager-install
			//# If `AddHost` answers that the Pool Manager already holds a Host
			//# of that name, then the Fleet Controller SHALL read the Host with `GetHost`
			//# and bring it in step as FL-129 says, without failing the run.
			st, err := hosts.GetHost(ctx, h.Name)
			if err != nil {
				return fmt.Errorf("reading Host %s, which the Pool Manager already holds: %w", h.Name, err)
			}
			cur = st.Host
		default:
			return fmt.Errorf("adding Host %s to the Pool Manager: %w", h.Name, err)
		}
	}
	//= docs/requirements/06-fleet.md#pool-manager-install
	//# When the Pool Manager holds an Inventory Host with an endpoint
	//# or TLS settings other than the Inventory's, the Fleet Controller SHALL
	//# update the Host with `UpdateHost`.
	if cur.Address == h.Address && cur.TLS == h.TLS {
		return nil
	}
	if _, err := hosts.UpdateHost(ctx, h); err != nil {
		return fmt.Errorf("updating Host %s in the Pool Manager: %w", h.Name, err)
	}
	res.Updated = append(res.Updated, h.Name)
	return nil
}

//= docs/requirements/06-fleet.md#pool-manager-install
//# When the Pool Manager holds a Host that is not in the
//# Inventory, the Fleet Controller SHALL remove the Host from the
//# `flintlock_hosts` of every Pool that names it.

// dropFromPools takes the gone Hosts out of every Pool's flintlock_hosts,
// in every namespace, as drain does for one Host (FL-080). The Pool Manager
// refuses to remove a Host while a Pool names it.
func dropFromPools(ctx context.Context, pools PoolAdmin, gone []string) error {
	all, err := pools.ListPools(ctx, "")
	if err != nil {
		return fmt.Errorf("listing the Pool Manager's Pools: %w", err)
	}
	var errs []error
	for _, pool := range all {
		spec := pool.Spec
		kept := slices.DeleteFunc(slices.Clone(spec.FlintlockHosts), func(h string) bool { return slices.Contains(gone, h) })
		if len(kept) == len(spec.FlintlockHosts) {
			continue
		}
		spec.FlintlockHosts = kept
		if _, err := pools.UpdatePool(ctx, spec); err != nil {
			errs = append(errs, fmt.Errorf("taking Hosts gone from the Inventory out of Pool %s: %w", spec.Ref, err))
		}
	}
	return errors.Join(errs...)
}

// removeHosts removes the gone Hosts, retrying the ones the Pool Manager
// refuses until o.RemoveWait elapses, and records the result in res. It
// returns the failures other than a refusal.
func removeHosts(ctx context.Context, hosts HostAdmin, gone []string, o HostSyncOptions, res *HostSyncResult) []error {
	var errs []error
	deadline := time.Now().Add(o.RemoveWait)
	refused := map[string]error{}
retry:
	for {
		var again []string
		for _, name := range gone {
			//= docs/requirements/06-fleet.md#pool-manager-install
			//# When the Pool Manager holds a Host that is not in the
			//# Inventory, the Fleet Controller SHALL remove the Host with `RemoveHost`,
			//# and SHALL try again for up to one minute while the Pool Manager refuses
			//# because a Pool names the Host or a MicroVM is on it.
			err := hosts.RemoveHost(ctx, name)
			switch {
			case err == nil, errors.Is(err, ErrNotFound):
				res.Removed = append(res.Removed, name)
				delete(refused, name)
			case errors.Is(err, ErrFailedPrecondition):
				refused[name] = err
				again = append(again, name)
			default:
				errs = append(errs, fmt.Errorf("removing Host %s from the Pool Manager: %w", name, err))
				delete(refused, name)
			}
		}
		gone = again
		if len(gone) == 0 || !time.Now().Before(deadline) {
			break
		}
		t := time.NewTimer(min(o.RetryInterval, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			errs = append(errs, ctx.Err())
			break retry
		case <-t.C:
		}
	}
	slices.Sort(res.Removed)

	//= docs/requirements/06-fleet.md#pool-manager-install
	//# If the Pool Manager still refuses to remove a Host when that
	//# minute ends, then the Fleet Controller SHALL report the Host and the
	//# reason, and SHALL NOT count the refusal as a failure of the run.
	for _, name := range slices.Sorted(maps.Keys(refused)) {
		res.Kept = append(res.Kept, KeptHost{Name: name, Reason: refused[name]})
	}
	return errs
}
