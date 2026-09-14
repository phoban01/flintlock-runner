package drain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/fleet/opstest"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// claimOn claims MicroVMs from the Stack's Pool until one is on host and
// returns its lease; the other claims are released.
func claimOn(t *testing.T, s *opstest.Stack, host string) string {
	t.Helper()
	ctx := context.Background()
	var others []string
	defer func() {
		for _, id := range others {
			_ = s.Client.ReleaseVM(ctx, id)
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := s.Client.ClaimVM(ctx, s.Ref())
		if errors.Is(err, poolmgr.ErrExhausted) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("ClaimVM: %v", err)
		}
		if c.Host.Name == host {
			return c.LeaseID
		}
		others = append(others, c.LeaseID)
	}
	t.Fatalf("no MicroVM was placed on %s", host)
	return ""
}

func hostsOf(t *testing.T, s *opstest.Stack) []string {
	t.Helper()
	pool, err := s.Client.GetPool(context.Background(), s.Ref())
	if err != nil {
		t.Fatal(err)
	}
	return pool.Spec.FlintlockHosts
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//= type=test
//# The Fleet Controller SHALL provide a drain command that removes
//# a Host from every Pool's `flintlock_hosts` so that the Pool Manager places
//# nothing new there, and waits until the Pool Manager reports no leased
//# MicroVM on that Host or the configured drain timeout elapses.

func TestDrainRemovesHostFromPoolsAndWaitsForLeases(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 2})
	s.Declare(t)
	s.WaitAvailable(t, 2)
	lease := claimOn(t, s, "host-1")

	var stopped atomic.Int32
	d, err := NewDrainer(Config{
		Pools:        s.Client,
		Leases:       InspectorLeases{Inspector: s.PoolManager},
		PollInterval: 10 * time.Millisecond,
		Stop:         func(context.Context, string) error { stopped.Add(1); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Drain(context.Background(), "host-1", 30*time.Second) }()

	// The Pool no longer lists the Host...
	deadline := time.Now().Add(10 * time.Second)
	for slices.Contains(hostsOf(t, s), "host-1") {
		if time.Now().After(deadline) {
			t.Fatal("drain did not remove host-1 from the Pool's flintlock_hosts")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := hostsOf(t, s); !slices.Equal(got, []string{"host-2"}) {
		t.Errorf("flintlock_hosts = %v, want [host-2]", got)
	}
	// ...and drain waits while the lease is held.
	select {
	case err := <-done:
		t.Fatalf("drain returned %v while a leased MicroVM remained on host-1", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := s.Client.ReleaseVM(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("drain did not return once the lease was released")
	}
	if stopped.Load() != 1 {
		t.Errorf("Stop ran %d times, want once after the last lease", stopped.Load())
	}
	// Nothing new is placed on the drained Host.
	time.Sleep(300 * time.Millisecond)
	for _, vm := range s.PoolManager.VMs() {
		if vm.Host == "host-1" && vm.CreatedAt.After(time.Now().Add(-250*time.Millisecond)) {
			t.Errorf("MicroVM %s was placed on the drained host after drain", vm.UID)
		}
	}
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//= type=test
//# The drain command SHALL NOT stop `flintlockd` on the drained
//# Host while any leased MicroVM remains on it.

func TestDrainTimeoutLeavesServicesRunning(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 2})
	s.Declare(t)
	s.WaitAvailable(t, 2)
	lease := claimOn(t, s, "host-2")
	t.Cleanup(func() { _ = s.Client.ReleaseVM(context.Background(), lease) })

	var stopped atomic.Int32
	d, err := NewDrainer(Config{
		Pools:        s.Client,
		Leases:       InspectorLeases{Inspector: s.PoolManager},
		PollInterval: 10 * time.Millisecond,
		Stop:         func(context.Context, string) error { stopped.Add(1); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Drain(context.Background(), "host-2", 300*time.Millisecond)
	if !errors.Is(err, ErrLeasesRemain) {
		t.Fatalf("Drain = %v, want ErrLeasesRemain", err)
	}
	if stopped.Load() != 0 {
		t.Error("the Host's services were stopped while a leased MicroVM remained on it")
	}
}

func TestPoolLeasesCountsLeasesInThePools(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 1, PoolSize: 1})
	s.Declare(t)
	s.WaitAvailable(t, 1)
	r := PoolLeases{Pools: s.Client}
	n, err := r.LeasedOn(context.Background(), "host-1", []poolmgr.PoolRef{s.Ref(), {Name: "gone", Namespace: opstest.Namespace}})
	if err != nil || n != 0 {
		t.Fatalf("LeasedOn = %d, %v; want 0", n, err)
	}
	lease := claimOn(t, s, "host-1")
	n, err = r.LeasedOn(context.Background(), "host-1", []poolmgr.PoolRef{s.Ref()})
	if err != nil || n != 1 {
		t.Fatalf("LeasedOn = %d, %v; want 1", n, err)
	}
	_ = s.Client.ReleaseVM(context.Background(), lease)
}

func teardownFor(t *testing.T, s *opstest.Stack, invPath string, remote *opstest.Remote, scripts *opstest.Scripts, ec2 fleet.EC2) *Teardown {
	t.Helper()
	td, err := NewTeardown(TeardownConfig{
		Pools:         s.Client,
		Profiles:      s.Profiles,
		Hosts:         opstest.Dialer(),
		Scripts:       scripts,
		Remote:        remote,
		EC2:           ec2,
		InventoryPath: invPath,
		Timeout:       10 * time.Second,
		PollInterval:  20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return td
}

func savedInventory(t *testing.T, s *opstest.Stack) (*fleet.Inventory, string) {
	t.Helper()
	inv := &fleet.Inventory{Hosts: s.Inventory}
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := (inventory.Store{}).Save(context.Background(), path, inv); err != nil {
		t.Fatal(err)
	}
	return inv, path
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//= type=test
//# The Fleet Controller SHALL provide a teardown command that
//# deletes the declared Pools through the Pool Manager, waits for their
//# MicroVMs to be removed, stops and disables the installed services on every
//# Host and removes the Inventory.

func TestTeardownDeletesPoolsWaitsStopsServicesAndRemovesInventory(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 2})
	s.Declare(t)
	s.WaitAvailable(t, 2)
	inv, path := savedInventory(t, s)
	remote, scripts := &opstest.Remote{}, &opstest.Scripts{}

	if err := teardownFor(t, s, path, remote, scripts, nil).Teardown(context.Background(), inv, fleet.TeardownOptions{}); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	pools, err := s.Client.ListPools(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 0 {
		t.Errorf("pools after teardown = %d, want none", len(pools))
	}
	for _, h := range s.Hosts {
		vms, err := h.Client().ListMicroVMs(context.Background(), opstest.Namespace)
		if err != nil {
			t.Fatal(err)
		}
		if len(vms) != 0 {
			t.Errorf("%s still has %d MicroVMs", h.Config().Name, len(vms))
		}
	}
	var ran []string
	for _, c := range remote.Calls() {
		if c.Script == string(fleet.StepTeardown) {
			ran = append(ran, c.Instance.ID)
		}
	}
	if !slices.Equal(ran, []string{"host-1", "host-2"}) {
		t.Errorf("teardown script ran on %v, want every Host", ran)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the Inventory is still there: %v", err)
	}
}

func TestTeardownStopsBeforeServicesWhenAPoolCannotBeDeleted(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 1, PoolSize: 1})
	s.Declare(t)
	s.WaitAvailable(t, 1)
	lease := claimOn(t, s, "host-1")
	t.Cleanup(func() { _ = s.Client.ReleaseVM(context.Background(), lease) })
	inv, path := savedInventory(t, s)
	remote := &opstest.Remote{}

	err := teardownFor(t, s, path, remote, &opstest.Scripts{}, nil).Teardown(context.Background(), inv, fleet.TeardownOptions{})
	var terr *Error
	if !errors.As(err, &terr) || terr.Failures[0].Step != StepPools {
		t.Fatalf("Teardown = %v, want a %s failure while a lease is outstanding", err, StepPools)
	}
	if len(remote.Calls()) != 0 {
		t.Error("services were stopped although the Pool could not be deleted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the Inventory was removed although teardown failed")
	}
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//= type=test
//# The teardown command SHALL NOT terminate EC2 instances unless
//# the terminate flag is passed explicitly.

func TestTeardownTerminatesOnlyWithTheFlag(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 1, PoolSize: 1})
	s.Declare(t)
	inv, path := savedInventory(t, s)
	ec2 := &opstest.EC2{}
	if err := teardownFor(t, s, path, &opstest.Remote{}, &opstest.Scripts{}, ec2).Teardown(context.Background(), inv, fleet.TeardownOptions{}); err != nil {
		t.Fatal(err)
	}
	if calls := ec2.TerminateCalls(); len(calls) != 0 {
		t.Fatalf("TerminateInstances called %v without the terminate flag", calls)
	}

	s.Declare(t)
	if err := teardownFor(t, s, path, &opstest.Remote{}, &opstest.Scripts{}, ec2).Teardown(context.Background(), inv, fleet.TeardownOptions{Terminate: true}); err != nil {
		t.Fatal(err)
	}
	if calls := ec2.TerminateCalls(); len(calls) != 1 || !slices.Equal(calls[0], []string{"host-1"}) {
		t.Errorf("TerminateInstances calls = %v, want one for [host-1]", calls)
	}
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//= type=test
//# The teardown command SHALL NOT remove the thin pool or its
//# backing device unless the purge flag is passed explicitly.

func TestTeardownPurgesOnlyWithTheFlag(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 1, PoolSize: 1})
	_, path := savedInventory(t, s)
	inv := &fleet.Inventory{Hosts: s.Inventory}

	for _, purge := range []bool{false, true} {
		scripts := &opstest.Scripts{}
		if err := teardownFor(t, s, path, &opstest.Remote{}, scripts, nil).Teardown(context.Background(), inv, fleet.TeardownOptions{Purge: purge}); err != nil {
			t.Fatal(err)
		}
		calls := scripts.Calls()
		if len(calls) != 1 || calls[0].Step != fleet.StepTeardown {
			t.Fatalf("render calls = %+v, want one teardown script", calls)
		}
		got, set := calls[0].Input.Options[OptionPurge]
		if purge && got != "true" {
			t.Errorf("with the purge flag, options = %v, want purge=true", calls[0].Input.Options)
		}
		if !purge && set {
			t.Errorf("without the purge flag, options = %v; the teardown script must not be told to purge", calls[0].Input.Options)
		}
	}
}

func TestTeardownReportsHostScriptFailures(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 2})
	inv, path := savedInventory(t, s)
	remote := &opstest.Remote{Fail: map[string]int{"host-2": 3}}
	err := teardownFor(t, s, path, remote, &opstest.Scripts{}, nil).Teardown(context.Background(), inv, fleet.TeardownOptions{})
	var terr *Error
	if !errors.As(err, &terr) || len(terr.Failures) != 1 || terr.Failures[0].Host != "host-2" {
		t.Fatalf("Teardown = %v, want one failure naming host-2", err)
	}
	if len(remote.Calls()) != 2 {
		t.Error("teardown stopped at the failing Host instead of continuing with the others")
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the Inventory was removed although a Host failed")
	}
}
