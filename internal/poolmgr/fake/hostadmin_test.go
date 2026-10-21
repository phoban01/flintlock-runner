package fake

import (
	"errors"
	"strings"
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// hostAdmin is the harness's in-process HostAdmin client.
func (h *harness) hostAdmin() poolmgr.HostAdminConn {
	h.t.Helper()
	c, err := h.pm.HostAdmin()
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	return c
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL serve `AddHost`, `UpdateHost`,
//# `RemoveHost`, `GetHost` and `ListHosts` of the `poolmgr.v1alpha1`
//# `HostAdmin` service, and SHALL answer `ALREADY_EXISTS` to `AddHost` for a
//# Host it holds and `NOT_FOUND` to the others for a Host it does not hold.

func TestHostAdminFollowsBatterysRules(t *testing.T) {
	t.Run("an empty registry", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{EmptyHostRegistry: true}, "host-a")
		admin := h.hostAdmin()
		if hosts, err := admin.ListHosts(h.ctx); err != nil || len(hosts) != 0 {
			t.Fatalf("ListHosts = %v, %v; want no Host", hosts, err)
		}
		for name, err := range map[string]error{
			"GetHost":    func() error { _, err := admin.GetHost(h.ctx, "host-a"); return err }(),
			"UpdateHost": func() error { _, err := admin.UpdateHost(h.ctx, poolmgr.Host{Name: "host-a"}); return err }(),
			"RemoveHost": admin.RemoveHost(h.ctx, "host-a"),
		} {
			if !errors.Is(err, poolmgr.ErrNotFound) {
				t.Errorf("%s of a Host the fake does not hold = %v, want NOT_FOUND", name, err)
			}
		}

		want := poolmgr.Host{Name: "host-a", Address: "10.0.0.1:9090", TLS: poolmgr.HostTLS{CAFile: "/ca.pem"}}
		if got, err := admin.AddHost(h.ctx, want); err != nil || *got != want {
			t.Fatalf("AddHost = %+v, %v; want %+v", got, err, want)
		}
		if _, err := admin.AddHost(h.ctx, want); !errors.Is(err, poolmgr.ErrAlreadyExists) {
			t.Fatalf("second AddHost = %v, want ALREADY_EXISTS", err)
		}
		st, err := admin.GetHost(h.ctx, "host-a")
		if err != nil || st.Host != want || st.VMCount != 0 {
			t.Fatalf("GetHost = %+v, %v; want %+v with no MicroVM", st, err, want)
		}
		want.Address, want.TLS = "10.0.0.2:9090", poolmgr.HostTLS{Insecure: true}
		if got, err := admin.UpdateHost(h.ctx, want); err != nil || *got != want {
			t.Fatalf("UpdateHost = %+v, %v; want %+v", got, err, want)
		}
		if hosts, err := admin.ListHosts(h.ctx); err != nil || len(hosts) != 1 || hosts[0].Host != want {
			t.Fatalf("ListHosts = %+v, %v; want just %+v", hosts, err, want)
		}
		if err := admin.RemoveHost(h.ctx, "host-a"); err != nil {
			t.Fatalf("RemoveHost: %v", err)
		}
		if _, err := admin.GetHost(h.ctx, "host-a"); !errors.Is(err, poolmgr.ErrNotFound) {
			t.Fatalf("GetHost after RemoveHost = %v, want NOT_FOUND", err)
		}
	})

	t.Run("a Host source's names are held until removed", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		admin := h.hostAdmin()
		hosts, err := admin.ListHosts(h.ctx)
		if err != nil || len(hosts) != 1 || hosts[0].Host != (poolmgr.Host{Name: "host-a", Address: "host-a:9090"}) {
			t.Fatalf("ListHosts = %+v, %v; want host-a at its source's address with no TLS settings", hosts, err)
		}
		if _, err := admin.AddHost(h.ctx, poolmgr.Host{Name: "host-a"}); !errors.Is(err, poolmgr.ErrAlreadyExists) {
			t.Fatalf("AddHost = %v, want ALREADY_EXISTS", err)
		}
		if err := admin.RemoveHost(h.ctx, "host-a"); err != nil {
			t.Fatalf("RemoveHost: %v", err)
		}
		if hosts, err := admin.ListHosts(h.ctx); err != nil || len(hosts) != 0 {
			t.Fatalf("ListHosts after RemoveHost = %+v, %v; want none", hosts, err)
		}
	})
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL refuse with `FAILED_PRECONDITION` to
//# remove a Host that a Pool names or that a MicroVM is on.

func TestRemoveHostRefusesAHostInUse(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	admin := h.hostAdmin()
	h.createPool(h.spec("pool", 1, "host-a"))

	err := admin.RemoveHost(h.ctx, "host-a")
	if !errors.Is(err, poolmgr.ErrFailedPrecondition) || !strings.Contains(err.Error(), "pools pool") || !strings.Contains(err.Error(), "vm_count is 1") {
		t.Fatalf("RemoveHost while a Pool names it = %v, want FAILED_PRECONDITION naming the pool and the MicroVM", err)
	}
	// Out of the Pool, the MicroVM on it still holds it.
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 1)); err != nil {
		t.Fatal(err)
	}
	err = admin.RemoveHost(h.ctx, "host-a")
	if !errors.Is(err, poolmgr.ErrFailedPrecondition) || strings.Contains(err.Error(), "pools") || !strings.Contains(err.Error(), "vm_count is 1") {
		t.Fatalf("RemoveHost with a MicroVM on it = %v, want FAILED_PRECONDITION naming the MicroVM alone", err)
	}
	if st, err := admin.GetHost(h.ctx, "host-a"); err != nil || st.VMCount != 1 {
		t.Fatalf("GetHost = %+v, %v; want one MicroVM counted", st, err)
	}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL refuse with `INVALID_ARGUMENT` a
//# Pool whose `flintlock_hosts` names a Host it does not hold, and SHALL
//# place MicroVMs only on Hosts it holds.

func TestPoolsNameOnlyHeldHosts(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{EmptyHostRegistry: true}, "host-a", "host-b")
	admin := h.hostAdmin()
	if _, err := h.client.CreatePool(h.ctx, h.spec("pool", 2, "host-a")); !errors.Is(err, poolmgr.ErrInvalid) || !strings.Contains(err.Error(), "unknown hosts host-a") {
		t.Fatalf("CreatePool on a Host the fake does not hold = %v, want INVALID_ARGUMENT", err)
	}
	for _, name := range []string{"host-a", "host-b"} {
		if _, err := admin.AddHost(h.ctx, poolmgr.Host{Name: name, Address: name + ":9090"}); err != nil {
			t.Fatal(err)
		}
	}
	h.createPool(h.spec("pool", 0, "host-a", "host-b"))
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 0, "host-a", "host-c")); !errors.Is(err, poolmgr.ErrInvalid) {
		t.Fatalf("UpdatePool onto a Host the fake does not hold = %v, want INVALID_ARGUMENT", err)
	}

	// A Host taken out of every Pool and removed gets no more MicroVMs, even
	// from a Pool spec that a reconcile already holds.
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 0, "host-a")); err != nil {
		t.Fatal(err)
	}
	if err := admin.RemoveHost(h.ctx, "host-b"); err != nil {
		t.Fatal(err)
	}
	h.pm.mu.Lock()
	ps := h.pm.pools[keyOf(h.ref("pool"))]
	ps.spec.FlintlockHosts = []string{"host-b", "host-a"}
	ps.spec.Size = 2
	h.pm.mu.Unlock()
	h.tick()
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	if got := h.vmHosts(); got["host-b"] != 0 || got["host-a"] != 2 {
		t.Fatalf("placement = %v, want every MicroVM on host-a, which the fake holds", got)
	}
}
