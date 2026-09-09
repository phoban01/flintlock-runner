package flintlock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
)

//= docs/requirements/05-hosts.md#inventory
//# When starting, the Runner SHALL call `ServerInfo` on every Host
//# and SHALL log the Host's flintlock version and whether the exec and SSH
//# proxy services are enabled.

// ProbeAll calls Probe on every Host the Registry currently holds, in name
// order, and returns what each answered. A Host that does not answer is
// left out of the map and its error is joined into the returned error; the
// remaining Hosts are still probed, because one unreachable Host is not a
// reason to refuse to start.
func ProbeAll(ctx context.Context, reg Registry, namespace string, log *slog.Logger) (map[string]*HostInfo, error) {
	infos := make(map[string]*HostInfo)
	var errs []error
	for _, name := range reg.Names() {
		client, release, err := reg.Lease(name)
		if err != nil {
			// The Host left the Inventory between Names and Lease.
			continue
		}
		// The lease is what keeps the connection open for the length of the
		// probe when a reload removes the Host underneath it (HO-014).
		info, err := Probe(ctx, client, namespace, log)
		release()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		infos[name] = info
	}
	return infos, errors.Join(errs...)
}

//= docs/requirements/05-hosts.md#inventory
//# If a Host reports that the exec service is disabled, then the
//# Runner SHALL log a warning naming the Host, because Jobs the Pool Manager
//# places there cannot be run over the `exec` Guest Transport.

//= docs/requirements/05-hosts.md#inventory
//# If `ServerInfo` is not implemented by a Host, then the Runner
//# SHALL treat the Host's version as unknown and continue.

// Probe asks one Host what it is: its flintlock version and whether the
// exec and SSH proxy guest-agent services are enabled, logged at info, with
// a warning naming the Host when the exec service is off because the
// default Guest Transport cannot run a Job there.
//
// A Host that predates the RPC answers UNIMPLEMENTED. That is not a
// failure: the version is reported unknown and the probe continues, using
// ListMicroVMs to establish that the Host is answering at all. Any other
// error is the Host's, and is returned.
func Probe(ctx context.Context, c HostClient, namespace string, log *slog.Logger) (*HostInfo, error) {
	if log == nil {
		log = discardLogger()
	}
	info, err := c.ServerInfo(ctx)
	switch {
	case err == nil:
		log.Info("flintlock host",
			"host", c.Name(),
			"version", info.Version,
			"commit", info.Commit,
			"build_date", info.BuildDate,
			"uptime", info.Uptime,
			"exec_enabled", info.Exec.Enabled,
			"ssh_proxy_enabled", info.SSHProxy.Enabled,
		)
		if !info.Exec.Enabled {
			log.Warn("flintlock host has the exec service disabled; jobs placed there cannot run over the exec guest transport",
				"host", c.Name())
		}
		return info, nil
	case errors.Is(err, ErrUnimplemented):
		if err := probeFallback(ctx, c, namespace); err != nil {
			return nil, err
		}
		log.Info("flintlock host", "host", c.Name(), "version", "unknown",
			"reason", "ServerInfo is not implemented by this host")
		return &HostInfo{Name: c.Name()}, nil
	default:
		return nil, err
	}
}

//= docs/requirements/05-hosts.md#flintlock-client
//# The Runner SHALL only call `ListMicroVMs` with its own namespace
//# and only as a health probe fallback.

// probeFallback is the one place in the Runner that calls ListMicroVMs, and
// it is reached only from the ServerInfo-unimplemented branch of Probe: a
// Host without ServerInfo still has to be shown to be answering, and
// listing the Runner's own namespace is the cheapest call that does it. The
// result is discarded; only the error matters. The namespace is the
// Runner's own, never a wildcard, so the probe never looks at MicroVMs that
// are not the Runner's.
func probeFallback(ctx context.Context, c HostClient, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("flintlock: host %s: probing needs the Runner's namespace", c.Name())
	}
	if _, err := c.ListMicroVMs(ctx, namespace); err != nil {
		return err
	}
	return nil
}

//= docs/requirements/05-hosts.md#inventory
//# When starting, the Runner SHALL compare the Inventory with the
//# Hosts the Pool Manager reports for each Pool and SHALL log a warning for
//# any Pool Host missing from the Inventory, because a MicroVM placed there
//# could not be reached.

// CheckPoolHosts compares the Hosts the Pool Manager reports for each Pool,
// keyed by Pool name, with the Inventory in reg, and returns the Pool Hosts
// that the Inventory does not have, sorted, by Pool. Each one is logged as
// a warning naming the Pool and the Host: the Pool Manager may place a
// MicroVM there and the Runner would then hold a Lease it cannot reach.
func CheckPoolHosts(reg Registry, pools map[string][]string, log *slog.Logger) map[string][]string {
	if log == nil {
		log = discardLogger()
	}
	known := make(map[string]struct{})
	for _, name := range reg.Names() {
		known[name] = struct{}{}
	}

	poolNames := make([]string, 0, len(pools))
	for pool := range pools {
		poolNames = append(poolNames, pool)
	}
	sort.Strings(poolNames)

	var missing map[string][]string
	for _, pool := range poolNames {
		var absent []string
		for _, host := range pools[pool] {
			if _, ok := known[host]; !ok {
				absent = append(absent, host)
			}
		}
		if len(absent) == 0 {
			continue
		}
		sort.Strings(absent)
		absent = dedupe(absent)
		for _, host := range absent {
			log.Warn("pool host is missing from the inventory; a microvm placed there could not be reached",
				"pool", pool, "host", host)
		}
		if missing == nil {
			missing = make(map[string][]string)
		}
		missing[pool] = absent
	}
	return missing
}

// dedupe removes adjacent duplicates from a sorted slice in place.
func dedupe(sorted []string) []string {
	out := sorted[:0]
	var last string
	for i, s := range sorted {
		if i > 0 && s == last {
			continue
		}
		out = append(out, s)
		last = s
	}
	return out
}
