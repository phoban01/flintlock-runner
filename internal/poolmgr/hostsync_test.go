package poolmgr_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// hostAdmin dials the fake Pool Manager's HostAdmin service with the
// package's own client.
func (p *poolManager) hostAdmin() poolmgr.HostAdminConn {
	p.t.Helper()
	c, err := poolmgr.DialHostAdmin(poolmgr.ClientConfig{
		Endpoint: p.addr,
		TLS:      config.ClientTLS{Insecure: true},
		Deadline: testTimeout,
	})
	if err != nil {
		p.t.Fatalf("DialHostAdmin: %v", err)
	}
	p.t.Cleanup(func() { _ = c.Close() })
	return c
}

// heldHosts lists what the Pool Manager holds, by name.
func heldHosts(t *testing.T, ctx context.Context, admin poolmgr.HostAdmin) map[string]poolmgr.Host {
	t.Helper()
	listed, err := admin.ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	out := map[string]poolmgr.Host{}
	for _, st := range listed {
		out[st.Host.Name] = st.Host
	}
	return out
}

// declarePool creates a Pool of size on hosts.
func declarePool(t *testing.T, ctx context.Context, c poolmgr.PoolAdmin, name string, size int, hosts ...string) {
	t.Helper()
	spec, err := poolmgr.NewSpecBuilder().Build(testProfile(name, size), poolmgr.SpecInput{RunnerName: testRunner, Namespace: testNamespace, Hosts: hosts})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreatePool(ctx, spec); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
}

var fleetTLS = poolmgr.HostTLS{CAFile: "/etc/flintlock-runner/pki/ca.pem", CertFile: "/etc/flintlock-runner/pki/poolmgr.pem", KeyFile: "/etc/flintlock-runner/pki/poolmgr.key"}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# When the Pool Manager daemon answers, the Fleet Controller SHALL
//# register with `AddHost` every Inventory Host that the Pool Manager does
//# not hold, with its `flintlockd` endpoint and TLS settings.

// TestSyncHostsRegistersMissingHosts starts from a Pool Manager that holds
// no Host, as battery v0.5.0 does on a new database, and checks that every
// wanted Host is added with its endpoint and TLS settings, that a Pool can
// then name them, and that a second sync changes nothing.
func TestSyncHostsRegistersMissingHosts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{EmptyHostRegistry: true}, "host-a", "host-b")
	admin, pools := pm.hostAdmin(), pm.client()
	want := []poolmgr.Host{
		{Name: "host-b", Address: "10.0.1.11:9090", TLS: fleetTLS},
		{Name: "host-a", Address: "10.0.1.10:9090", TLS: fleetTLS},
	}
	res, err := poolmgr.SyncHosts(ctx, admin, pools, want, poolmgr.HostSyncOptions{})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}
	if !slices.Equal(res.Added, []string{"host-a", "host-b"}) || len(res.Updated)+len(res.Removed)+len(res.Kept) != 0 {
		t.Fatalf("result = %+v, want host-a and host-b added and nothing else", res)
	}
	held := heldHosts(t, ctx, admin)
	for _, h := range want {
		if held[h.Name] != h {
			t.Errorf("the Pool Manager holds %+v, want %+v", held[h.Name], h)
		}
	}
	declarePool(t, ctx, pools, "small", 0, "host-a", "host-b")

	res, err = poolmgr.SyncHosts(ctx, admin, pools, want, poolmgr.HostSyncOptions{})
	if err != nil || len(res.Added)+len(res.Updated)+len(res.Removed)+len(res.Kept) != 0 {
		t.Fatalf("second sync = %+v, %v; want nothing to do", res, err)
	}
}

// hideListed is a HostAdmin whose ListHosts reports nothing, as a Pool
// Manager that gained the Hosts between the list and the add would look.
type hideListed struct{ poolmgr.HostAdmin }

func (hideListed) ListHosts(context.Context) ([]poolmgr.HostStatus, error) { return nil, nil }

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# If `AddHost` answers that the Pool Manager already holds a Host
//# of that name, then the Fleet Controller SHALL read the Host with `GetHost`
//# and bring it in step as FL-129 says, without failing the run.

// TestSyncHostsTakesAlreadyExistsAsAHostToUpdate checks that an AddHost the
// Pool Manager answers with ALREADY_EXISTS is no failure: the Host is read
// and updated to the wanted settings.
func TestSyncHostsTakesAlreadyExistsAsAHostToUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	// The fake holds host-a with no TLS settings, as battery v0.5.0 holds a
	// Host it took over from a v0.4 database.
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	admin, pools := pm.hostAdmin(), pm.client()
	want := poolmgr.Host{Name: "host-a", Address: "host-a:9090", TLS: fleetTLS}
	res, err := poolmgr.SyncHosts(ctx, hideListed{admin}, pools, []poolmgr.Host{want}, poolmgr.HostSyncOptions{})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}
	if len(res.Added) != 0 || !slices.Equal(res.Updated, []string{"host-a"}) {
		t.Fatalf("result = %+v, want host-a updated, not added", res)
	}
	if got := heldHosts(t, ctx, admin)["host-a"]; got != want {
		t.Fatalf("the Pool Manager holds %+v, want %+v", got, want)
	}
}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# When the Pool Manager holds an Inventory Host with an endpoint
//# or TLS settings other than the Inventory's, the Fleet Controller SHALL
//# update the Host with `UpdateHost`.

// TestSyncHostsUpdatesEndpointAndTLS checks the upgrade from battery v0.4,
// whose Hosts battery v0.5.0 keeps without TLS settings, and a later change
// of endpoint: each is one UpdateHost, and a Host already in step is left
// alone.
func TestSyncHostsUpdatesEndpointAndTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a", "host-b")
	admin, pools := pm.hostAdmin(), pm.client()
	want := []poolmgr.Host{
		{Name: "host-a", Address: "host-a:9090", TLS: fleetTLS},
		{Name: "host-b", Address: "host-b:9090", TLS: fleetTLS},
	}
	res, err := poolmgr.SyncHosts(ctx, admin, pools, want, poolmgr.HostSyncOptions{})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}
	if len(res.Added) != 0 || !slices.Equal(res.Updated, []string{"host-a", "host-b"}) {
		t.Fatalf("upgrade = %+v, want both Hosts updated with their TLS settings", res)
	}
	want[1].Address = "10.0.9.9:9090"
	if res, err = poolmgr.SyncHosts(ctx, admin, pools, want, poolmgr.HostSyncOptions{}); err != nil || !slices.Equal(res.Updated, []string{"host-b"}) {
		t.Fatalf("endpoint change = %+v, %v; want host-b updated alone", res, err)
	}
	held := heldHosts(t, ctx, admin)
	for _, h := range want {
		if held[h.Name] != h {
			t.Errorf("the Pool Manager holds %+v, want %+v", held[h.Name], h)
		}
	}
}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# When the Pool Manager holds a Host that is not in the
//# Inventory, the Fleet Controller SHALL remove the Host from the
//# `flintlock_hosts` of every Pool that names it.

// TestSyncHostsTakesGoneHostsOutOfPools checks that a Host gone from the
// Inventory leaves every Pool that names it, whatever its namespace, so that
// the Pool Manager lets it go.
func TestSyncHostsTakesGoneHostsOutOfPools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a", "host-b")
	admin, pools := pm.hostAdmin(), pm.client()
	declarePool(t, ctx, pools, "one", 0, "host-a", "host-b")
	declarePool(t, ctx, pools, "two", 0, "host-b")
	res, err := poolmgr.SyncHosts(ctx, admin, pools, []poolmgr.Host{{Name: "host-a", Address: "host-a:9090"}}, poolmgr.HostSyncOptions{RemoveWait: time.Second, RetryInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}
	if !slices.Equal(res.Removed, []string{"host-b"}) || len(res.Kept) != 0 {
		t.Fatalf("result = %+v, want host-b removed", res)
	}
	for name, hosts := range map[string][]string{"one": {"host-a"}, "two": {}} {
		pool, err := pools.GetPool(ctx, ref(name))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(pool.Spec.FlintlockHosts, hosts) {
			t.Errorf("pool %s names %v, want %v", name, pool.Spec.FlintlockHosts, hosts)
		}
	}
	if _, held := heldHosts(t, ctx, admin)["host-b"]; held {
		t.Error("the Pool Manager still holds host-b")
	}
}

// refusing is a HostAdmin that refuses RemoveHost a number of times, as
// battery does while a MicroVM it is deleting is still on the Host.
type refusing struct {
	poolmgr.HostAdmin
	mu      sync.Mutex
	refuse  int
	removes int
}

func (r *refusing) ListHosts(context.Context) ([]poolmgr.HostStatus, error) {
	return []poolmgr.HostStatus{{Host: poolmgr.Host{Name: "gone"}}}, nil
}

func (r *refusing) RemoveHost(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removes++
	if r.removes <= r.refuse {
		return errors.Join(poolmgr.ErrFailedPrecondition, errors.New(`host "gone" is still in use (vm_count is 1)`))
	}
	return nil
}

// noPools is a PoolAdmin with no Pool.
type noPools struct{ poolmgr.PoolAdmin }

func (noPools) ListPools(context.Context, string) ([]*poolmgr.Pool, error) { return nil, nil }

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# When the Pool Manager holds a Host that is not in the
//# Inventory, the Fleet Controller SHALL remove the Host with `RemoveHost`,
//# and SHALL try again for up to one minute while the Pool Manager refuses
//# because a Pool names the Host or a MicroVM is on it.

// TestSyncHostsRetriesARefusedRemoval checks that a removal the Pool Manager
// refuses is tried again until it succeeds, and that the wait defaults to
// one minute.
func TestSyncHostsRetriesARefusedRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	r := &refusing{refuse: 3}
	res, err := poolmgr.SyncHosts(ctx, r, noPools{}, nil, poolmgr.HostSyncOptions{RemoveWait: 5 * time.Second, RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}
	if !slices.Equal(res.Removed, []string{"gone"}) || len(res.Kept) != 0 || r.removes != 4 {
		t.Fatalf("result = %+v after %d RemoveHost calls, want gone removed on the fourth", res, r.removes)
	}
	if poolmgr.DefaultRemoveWait != time.Minute {
		t.Errorf("DefaultRemoveWait = %s, want one minute", poolmgr.DefaultRemoveWait)
	}
}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# If the Pool Manager still refuses to remove a Host when that
//# minute ends, then the Fleet Controller SHALL report the Host and the
//# reason, and SHALL NOT count the refusal as a failure of the run.

// TestSyncHostsKeepsAHostStillInUse runs against the fake Pool Manager with
// a MicroVM on the gone Host, which the fake keeps after the Host leaves
// its Pool: the removal is refused to the end of the wait, and the Host is
// reported with battery's reason and no error.
func TestSyncHostsKeepsAHostStillInUse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a", "host-b")
	admin, pools := pm.hostAdmin(), pm.client()
	declarePool(t, ctx, pools, "busy", 1, "host-b")
	for !slices.ContainsFunc(pm.pm.VMs(), func(vm poolmgr.VMRecord) bool { return vm.Host == "host-b" && vm.UID != "" }) {
		select {
		case <-ctx.Done():
			t.Fatal("no MicroVM was created on host-b")
		case <-time.After(10 * time.Millisecond):
		}
	}
	res, err := poolmgr.SyncHosts(ctx, admin, pools, []poolmgr.Host{{Name: "host-a", Address: "host-a:9090"}}, poolmgr.HostSyncOptions{RemoveWait: 100 * time.Millisecond, RetryInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("SyncHosts = %v, want no error for a refused removal", err)
	}
	if len(res.Removed) != 0 || len(res.Kept) != 1 || res.Kept[0].Name != "host-b" ||
		!errors.Is(res.Kept[0].Reason, poolmgr.ErrFailedPrecondition) || !strings.Contains(res.Kept[0].Reason.Error(), "vm_count is 1") {
		t.Fatalf("result = %+v, want host-b kept with battery's reason", res)
	}
	if _, held := heldHosts(t, ctx, admin)["host-b"]; !held {
		t.Error("the Pool Manager no longer holds host-b")
	}
}

// TestSyncHostsCarriesOnPastAFailure checks that one Host the Pool Manager
// will not take does not stop the others, and that its failure is returned.
func TestSyncHostsCarriesOnPastAFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{EmptyHostRegistry: true}, "host-a")
	admin, pools := pm.hostAdmin(), pm.client()
	res, err := poolmgr.SyncHosts(ctx, admin, pools, []poolmgr.Host{{Name: "", Address: "10.0.0.1:9090"}, {Name: "host-a", Address: "host-a:9090"}}, poolmgr.HostSyncOptions{})
	if err == nil || !errors.Is(err, poolmgr.ErrInvalid) {
		t.Fatalf("SyncHosts = %v, want the nameless Host's INVALID_ARGUMENT", err)
	}
	if !slices.Equal(res.Added, []string{"host-a"}) {
		t.Fatalf("result = %+v, want host-a added all the same", res)
	}
}
