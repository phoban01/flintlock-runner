package fake

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The project SHALL provide a fake Pool Manager that serves the
//# `poolmgr.v1alpha1` `PoolAdmin`, `Lease` and `Events` services over gRPC
//# using the generated server stubs from the battery module.

// TestServesTheThreeServicesOverGRPC drives all three services through the
// generated clients over a real TCP gRPC connection to a served fake, so
// that the test fails if any service stops being registered or a handler
// stops answering. It is the wire-level counterpart of the loopback client
// the rest of the tests use.
func TestServesTheThreeServicesOverGRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	host := newStubHost("host-a")
	hosts := NewHosts()
	hosts.Add(host.Name(), host, "host-a:9090")
	pm := New(poolmgr.FakeConfig{
		Listen:            "127.0.0.1:0",
		Hosts:             hosts,
		Clock:             clock.NewFake(testEpoch),
		ReconcileInterval: testInterval,
	})

	served := make(chan error, 1)
	go func() { served <- pm.Serve(ctx) }()
	select {
	case <-pm.Ready():
	case err := <-served:
		t.Fatalf("Serve returned before listening: %v", err)
	}

	conn, err := grpc.NewClient(pm.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", pm.Addr(), err)
	}
	defer func() { _ = conn.Close() }()
	admin := poolmgrv1.NewPoolAdminClient(conn)
	lease := poolmgrv1.NewLeaseClient(conn)
	events := poolmgrv1.NewEventsClient(conn)

	// Events first, so that the whole life of the Pool is on the stream.
	stream, err := events.Subscribe(ctx, &poolmgrv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Events.Subscribe: %v", err)
	}

	spec := &poolmgrv1.PoolSpec{
		Name:                     "wire",
		Namespace:                testNamespace,
		Size:                     1,
		FlintlockHosts:           []string{host.Name()},
		MicrovmTemplate:          &types.MicroVMSpec{Namespace: testNamespace, Vcpu: 1, MemoryInMb: 256},
		ReplenishmentStrategy:    &poolmgrv1.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatExpiryThreshold: durationpb.New(30 * time.Second),
	}
	ref := &poolmgrv1.PoolRef{Name: spec.GetName(), Namespace: spec.GetNamespace()}

	// PoolAdmin.
	if _, err := admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: spec}); err != nil {
		t.Fatalf("PoolAdmin.CreatePool: %v", err)
	}
	if _, err := admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: spec}); statusCode(t, err) != codes.AlreadyExists {
		t.Fatalf("PoolAdmin.CreatePool twice: code %v, want ALREADY_EXISTS", statusCode(t, err))
	}
	// Events: the Pool fills without the clock moving, because a create
	// kicks the control loop.
	waitProtoEvent(t, stream, poolmgrv1.EventType_VM_AVAILABLE)

	got, err := admin.GetPool(ctx, &poolmgrv1.GetPoolRequest{Ref: ref})
	if err != nil {
		t.Fatalf("PoolAdmin.GetPool: %v", err)
	}
	if n := got.GetStatus().GetAvailableCount(); n != 1 {
		t.Fatalf("GetPool available_count = %d, want 1", n)
	}
	list, err := admin.ListPools(ctx, &poolmgrv1.ListPoolsRequest{})
	if err != nil {
		t.Fatalf("PoolAdmin.ListPools: %v", err)
	}
	if len(list.GetPools()) != 1 || list.GetPools()[0].GetSpec().GetName() != "wire" {
		t.Fatalf("ListPools = %v, want the one wire pool", list.GetPools())
	}
	spec.Size = 2
	if _, err := admin.UpdatePool(ctx, &poolmgrv1.UpdatePoolRequest{Spec: spec}); err != nil {
		t.Fatalf("PoolAdmin.UpdatePool: %v", err)
	}
	waitProtoEvent(t, stream, poolmgrv1.EventType_VM_AVAILABLE)

	// Lease.
	claim, err := lease.ClaimVM(ctx, &poolmgrv1.ClaimVMRequest{Pool: ref})
	if err != nil {
		t.Fatalf("Lease.ClaimVM: %v", err)
	}
	if claim.GetLeaseId() == "" || claim.GetVmUid() == "" {
		t.Fatalf("ClaimVMResponse = %v, want a lease id and a vm uid", claim)
	}
	hb, err := lease.Heartbeat(ctx, &poolmgrv1.HeartbeatRequest{LeaseId: claim.GetLeaseId()})
	if err != nil {
		t.Fatalf("Lease.Heartbeat: %v", err)
	}
	if want := testEpoch.Add(30 * time.Second); !hb.GetExpiresAt().AsTime().Equal(want) {
		t.Fatalf("Heartbeat expires_at = %s, want %s", hb.GetExpiresAt().AsTime(), want)
	}
	if _, err := lease.ReleaseVM(ctx, &poolmgrv1.ReleaseVMRequest{LeaseId: claim.GetLeaseId()}); err != nil {
		t.Fatalf("Lease.ReleaseVM: %v", err)
	}
	waitProtoEvent(t, stream, poolmgrv1.EventType_VM_DELETED_ON_RELEASE)

	if _, err := admin.DeletePool(ctx, &poolmgrv1.DeletePoolRequest{Ref: ref}); err != nil {
		t.Fatalf("PoolAdmin.DeletePool: %v", err)
	}
	if _, err := admin.GetPool(ctx, &poolmgrv1.GetPoolRequest{Ref: ref}); statusCode(t, err) != codes.NotFound {
		t.Fatalf("GetPool after DeletePool: code %v, want NOT_FOUND", statusCode(t, err))
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if n := host.live(); n != 0 {
		t.Fatalf("%d microvms left on the host after shutdown, want 0", n)
	}
}

// waitProtoEvent reads the generated Event stream until an event of type typ
// arrives.
func waitProtoEvent(t *testing.T, stream grpc.ServerStreamingClient[poolmgrv1.Event], typ poolmgrv1.EventType) *poolmgrv1.Event {
	t.Helper()
	for {
		e, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for %s on the Events stream: %v", typ, err)
		}
		if e.GetType() == typ {
			return e
		}
	}
}

// countingHosts is a poolmgr.HostSource that records every name the fake
// resolves, so that a test can prove the fake reached its Hosts only through
// the HostSource.
type countingHosts struct {
	inner *Hosts

	mu      sync.Mutex
	lookups map[string]int
}

func newCountingHosts(inner *Hosts) *countingHosts {
	return &countingHosts{inner: inner, lookups: make(map[string]int)}
}

func (c *countingHosts) Host(name string) (flintlock.PoolHostClient, error) {
	c.mu.Lock()
	c.lookups[name]++
	c.mu.Unlock()
	return c.inner.Host(name)
}

func (c *countingHosts) Names() []string { return c.inner.Names() }

func (c *countingHosts) counts() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.lookups))
	for k, v := range c.lookups {
		out[k] = v
	}
	return out
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL create, place and delete MicroVMs through the
//# same Host client interface the Runner uses, so that it can be pointed at
//# fake Hosts or at real `flintlockd` instances.

// TestMicroVMsGoThroughTheHostClient proves that every MicroVM the fake owns
// came from a flintlock.PoolHostClient the HostSource handed out and went
// back to it on deletion, that a Host the HostSource does not know is never
// created on, and that a HostSource built by dialling endpoints, which is
// how the standalone binary reaches real flintlockd, behaves identically.
func TestMicroVMsGoThroughTheHostClient(t *testing.T) {
	t.Run("created and deleted through the host client", func(t *testing.T) {
		var counting *countingHosts
		h := newHarnessWith(t, poolmgr.FakeConfig{}, func(inner *Hosts) poolmgr.HostSource {
			counting = newCountingHosts(inner)
			return counting
		}, "host-a", "host-b")
		h.createPool(h.spec("pool", 2, "host-a", "host-b"))

		// Every MicroVM the fake reports is one a stub Host minted; the fake
		// has no way to make one up.
		minted := map[string]bool{}
		for _, s := range h.stubs {
			s.mu.Lock()
			for _, uid := range s.created {
				minted[uid] = true
			}
			s.mu.Unlock()
		}
		uids := h.uids()
		if len(uids) != 2 {
			t.Fatalf("VMs() = %v, want two microvms", uids)
		}
		for _, uid := range uids {
			if !minted[uid] {
				t.Fatalf("microvm %q is not one the stub Hosts created (%v)", uid, minted)
			}
		}
		if lookups := counting.counts(); lookups["host-a"] == 0 || lookups["host-b"] == 0 {
			t.Fatalf("HostSource lookups = %v, want both Hosts resolved through it", lookups)
		}

		// The deletion path goes through the same client.
		claim := h.claim("pool")
		h.release(claim.LeaseID)
		var deleted []string
		for _, s := range h.stubs {
			s.mu.Lock()
			deleted = append(deleted, s.deleted...)
			s.mu.Unlock()
		}
		if len(deleted) != 1 || deleted[0] != claim.VMUID {
			t.Fatalf("deleted on the Hosts = %v, want [%s]", deleted, claim.VMUID)
		}
	})

	t.Run("pool hooks run in the microvm through the host client", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		spec := h.spec("pool", 1, "host-a")
		spec.CreateCommands = []string{"/opt/setup.sh"}
		spec.PreLeaseCommands = []string{"/opt/pre-lease.sh"}
		// The claim must not start a replacement, whose own create hooks
		// would run while the assertion below reads the Host's log.
		spec.Replenishment = poolmgr.ReplenishmentStrategy{
			Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			MinSize: minSize(1),
		}
		h.createPool(spec)
		claim := h.claim("pool")

		// The guest-agent readiness probe comes first, then the create
		// commands, then the pre-lease commands of the claim.
		want := []string{"true", "/opt/setup.sh", "/opt/pre-lease.sh"}
		got := h.stubs["host-a"].commands()
		if len(got) != len(want) {
			t.Fatalf("commands run on the host = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("commands run on the host = %v, want %v", got, want)
			}
		}
		h.release(claim.LeaseID)
	})

	t.Run("a deletion the host refuses is retried", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		host := h.stubs["host-a"]
		spec := h.spec("pool", 1, "host-a")
		spec.Replenishment = poolmgr.ReplenishmentStrategy{
			Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			MinSize: minSize(1),
		}
		h.createPool(spec)
		claim := h.claim("pool")

		host.set(func(s *stubHost) { s.deleteErr = errors.New("host busy") })
		err := h.client.ReleaseVM(h.ctx, claim.LeaseID)
		if !errors.Is(err, poolmgr.ErrUnavailable) {
			t.Fatalf("ReleaseVM while the host refuses = %v, want ErrUnavailable", err)
		}
		if _, deleted := host.counts(); deleted != 0 {
			t.Fatalf("%d microvms deleted, want none: the host refused", deleted)
		}
		if live := host.live(); live != 1 {
			t.Fatalf("%d microvms on the host, want the one the host would not delete", live)
		}

		// The control loop retries until the Host takes it.
		host.set(func(s *stubHost) { s.deleteErr = nil })
		h.advance(testInterval)
		deleted := h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
		if deleted.VMUID != claim.VMUID {
			t.Fatalf("VM_DELETED_ON_RELEASE for %q, want the released microvm %q", deleted.VMUID, claim.VMUID)
		}
		assertDeleted(t, host, claim.VMUID)
		if leases := h.pm.Leases(); len(leases) != 0 {
			t.Fatalf("leases after the retried deletion = %+v, want none", leases)
		}
	})

	t.Run("a host the source does not know is never created on", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		if _, err := h.client.CreatePool(h.ctx, h.spec("pool", 2, "host-a", "gone")); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if got := h.vmHosts(); got["gone"] != 0 || got["host-a"] != 2 {
			t.Fatalf("placement = %v, want both microvms on host-a and none on the unknown host", got)
		}
	})

	t.Run("hosts dialled as endpoints behave the same", func(t *testing.T) {
		hosts, err := HostsFromDialer(context.Background(), &stubDialer{}, []flintlock.Endpoint{
			{Name: "host-a", Address: "10.0.0.1:9090"},
			{Name: "host-b", Address: "10.0.0.2:9090", TLS: flintlock.TLSOptions{Insecure: true}},
		})
		if err != nil {
			t.Fatalf("HostsFromDialer: %v", err)
		}
		t.Cleanup(func() { _ = hosts.Close() })
		if got := hosts.Names(); len(got) != 2 || got[0] != "host-a" || got[1] != "host-b" {
			t.Fatalf("Names() = %v, want [host-a host-b]", got)
		}
		if addr, ok := hosts.Address("host-b"); !ok || addr != "10.0.0.2:9090" {
			t.Fatalf("Address(host-b) = %q, %v; want the endpoint address", addr, ok)
		}
		h := newHarness(t, poolmgr.FakeConfig{Hosts: hosts})
		h.createPool(h.spec("pool", 2, "host-a", "host-b"))
		if got := h.vmHosts(); got["host-a"] != 1 || got["host-b"] != 1 {
			t.Fatalf("placement over dialled hosts = %v, want one each", got)
		}
		for _, name := range []string{"host-a", "host-b"} {
			c, err := hosts.Host(name)
			if err != nil {
				t.Fatalf("Host(%s): %v", name, err)
			}
			if created, _ := c.(*stubHost).counts(); created != 1 {
				t.Fatalf("%s created %d microvms, want 1", name, created)
			}
		}
	})
}

// stubDialer is a flintlock.AdminDialer that hands out stub Hosts, standing
// in for AdminDialer's connections to real flintlockd instances.
type stubDialer struct{}

func (*stubDialer) DialAdmin(_ context.Context, ep flintlock.Endpoint) (flintlock.PoolHostClient, error) {
	return newStubHost(ep.Name), nil
}
