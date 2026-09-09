package fake

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/protobuf/proto"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// waitBooted waits for uid's boot to complete after the clock was advanced.
func waitBooted(t *testing.T, h *Host, uid string) {
	t.Helper()
	ch := h.bootDone(uid)
	if ch == nil {
		t.Fatalf("bootDone(%s): unknown uid", uid)
	}
	select {
	case <-ch:
	case <-time.After(testTimeout):
		t.Fatalf("microvm %s did not finish booting", uid)
	}
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL move a MicroVM from `PENDING` to `CREATED` after a
//# configurable boot delay and SHALL populate `vsock_path` in its status.

// TestBootDelay drives the boot with a fake clock: PENDING with no
// vsock_path until BootDelay has elapsed, then CREATED with vsock_path in
// the sandbox.
func TestBootDelay(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	h := newTestHost(t, flintlock.FakeHostConfig{BootDelay: 2 * time.Second}, WithClock(clk))
	c := h.Client()
	ctx := testCtx(t)

	vm := createVM(t, h, nil)
	uid := vm.GetSpec().GetUid()
	if vm.GetStatus().GetState() != types.MicroVMStatus_PENDING || vm.GetStatus().GetVsockPath() != "" {
		t.Fatalf("right after create: %v, want PENDING without vsock_path", vm.GetStatus())
	}
	sandbox, ok := h.SandboxPath(uid)
	if !ok {
		t.Fatal("no sandbox recorded")
	}
	if st, err := os.Stat(sandbox); err != nil || !st.IsDir() {
		t.Fatalf("sandbox %s: stat = %v, %v; want a directory", sandbox, st, err)
	}
	if filepath.Dir(sandbox) != h.SandboxRoot() || filepath.Base(sandbox) != uid {
		t.Errorf("sandbox %s, want %s", sandbox, filepath.Join(h.SandboxRoot(), uid))
	}

	clk.Advance(time.Second)
	got, err := c.GetMicroVM(ctx, uid)
	if err != nil {
		t.Fatalf("GetMicroVM: %v", err)
	}
	if got.GetStatus().GetState() != types.MicroVMStatus_PENDING {
		t.Errorf("after 1s of a 2s boot: state = %s, want PENDING", got.GetStatus().GetState())
	}

	clk.Advance(time.Second)
	waitBooted(t, h, uid)
	got, err = c.GetMicroVM(ctx, uid)
	if err != nil {
		t.Fatalf("GetMicroVM: %v", err)
	}
	if got.GetStatus().GetState() != types.MicroVMStatus_CREATED {
		t.Errorf("after the boot delay: state = %s, want CREATED", got.GetStatus().GetState())
	}
	if want := filepath.Join(sandbox, vsockSocketName); got.GetStatus().GetVsockPath() != want {
		t.Errorf("vsock_path = %q, want %q", got.GetStatus().GetVsockPath(), want)
	}
	if got.GetVersion() <= vm.GetVersion() {
		t.Errorf("version did not advance on boot: %d -> %d", vm.GetVersion(), got.GetVersion())
	}
	if !got.GetSpec().GetUpdatedAt().AsTime().After(vm.GetSpec().GetUpdatedAt().AsTime()) {
		t.Error("updated_at did not advance on boot")
	}

	// The returned MicroVM is a copy: mutating it does not touch the Host.
	got.Status.State = types.MicroVMStatus_FAILED
	again, _ := c.GetMicroVM(ctx, uid)
	if again.GetStatus().GetState() != types.MicroVMStatus_CREATED {
		t.Error("GetMicroVM returned the Host's own record rather than a copy")
	}
}

// TestCreateDefaultsAndDuplicates covers uid assignment, id and namespace
// defaults, honouring a supplied uid, and rejecting a duplicate.
func TestCreateDefaultsAndDuplicates(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	ctx := testCtx(t)
	c := h.Client()

	vm := createVM(t, h, &types.MicroVMSpec{})
	if vm.GetSpec().GetUid() == "" || vm.GetSpec().GetId() != vm.GetSpec().GetUid() || vm.GetSpec().GetNamespace() != "default" {
		t.Errorf("defaults: %v", vm.GetSpec())
	}
	if vm.GetSpec().GetCreatedAt() == nil {
		t.Error("created_at not set")
	}

	own := createVM(t, h, &types.MicroVMSpec{Uid: proto.String("my-uid"), Id: "x", Namespace: "ns"})
	if own.GetSpec().GetUid() != "my-uid" {
		t.Errorf("supplied uid not honoured: %q", own.GetSpec().GetUid())
	}
	if _, err := c.CreateMicroVM(ctx, &types.MicroVMSpec{Uid: proto.String("my-uid")}); err == nil {
		t.Error("duplicate uid accepted")
	}

	list, err := c.ListMicroVMs(ctx, "")
	if err != nil || len(list) != 2 {
		t.Errorf("ListMicroVMs(all) = %d, %v; want 2", len(list), err)
	}
	list, err = c.ListMicroVMs(ctx, "ns")
	if err != nil || len(list) != 1 || list[0].GetSpec().GetUid() != "my-uid" {
		t.Errorf("ListMicroVMs(ns) = %v, %v; want my-uid only", list, err)
	}
	name := "x"
	byName, err := h.listMicroVMs("", &name)
	if err != nil || len(byName) != 1 {
		t.Errorf("listMicroVMs(name=x) = %v, %v; want one", byName, err)
	}
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL support fault injection for a create that ends in
//# `FAILED`, an exec stream dropped before the exit code, and a Host that
//# stops answering.

// TestCreateFailsFault: with CreateFails set the boot ends in FAILED with no
// vsock_path and the MicroVM cannot run commands.
func TestCreateFailsFault(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	h := newTestHost(t, flintlock.FakeHostConfig{BootDelay: time.Second}, WithClock(clk))
	ctx := testCtx(t)
	c := h.Client()

	vm := createVM(t, h, nil)
	uid := vm.GetSpec().GetUid()
	h.SetFaults(flintlock.HostFaults{CreateFails: true})
	clk.Advance(time.Second)
	waitBooted(t, h, uid)

	got, err := c.GetMicroVM(ctx, uid)
	if err != nil {
		t.Fatalf("GetMicroVM: %v", err)
	}
	if got.GetStatus().GetState() != types.MicroVMStatus_FAILED || got.GetStatus().GetVsockPath() != "" {
		t.Errorf("status = %v, want FAILED without vsock_path", got.GetStatus())
	}
	res := runExec(t, c.Exec, shell(uid, "true"))
	if res.err == nil || res.exit != nil {
		t.Errorf("exec on a FAILED microvm: err=%v exit=%v; want an error", res.err, res.exit)
	}

	// Zero boot delay reads the fault at create time.
	h2 := newTestHost(t, flintlock.FakeHostConfig{})
	h2.SetFaults(flintlock.HostFaults{CreateFails: true})
	if vm := createVM(t, h2, nil); vm.GetStatus().GetState() != types.MicroVMStatus_FAILED {
		t.Errorf("immediate boot with CreateFails: state = %s", vm.GetStatus().GetState())
	}
}

// TestDeleteDuringBoot: deleting a PENDING MicroVM cancels its boot without
// a panic or a resurrected record.
func TestDeleteDuringBoot(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	h := newTestHost(t, flintlock.FakeHostConfig{BootDelay: time.Second}, WithClock(clk))
	ctx := testCtx(t)
	c := h.Client()

	vm := createVM(t, h, nil)
	uid := vm.GetSpec().GetUid()
	booted := h.bootDone(uid)
	if err := c.DeleteMicroVM(ctx, uid); err != nil {
		t.Fatalf("DeleteMicroVM: %v", err)
	}
	clk.Advance(time.Second)
	// Wait for the boot goroutine to observe the fired timer and give up:
	// Close waits for it, and the record must still be gone afterwards.
	_ = h.Close()
	select {
	case <-booted:
		t.Error("boot completed for a deleted microvm")
	default:
	}
	if _, err := c.GetMicroVM(ctx, uid); !errors.Is(err, flintlock.ErrNotFound) && !errors.Is(err, flintlock.ErrUnavailable) {
		t.Errorf("GetMicroVM after delete = %v", err)
	}
	if left, _ := h.Sandboxes(); len(left) != 0 {
		t.Errorf("sandboxes after delete = %v, want none", left)
	}
}

// TestSandboxes: the leak check lists exactly the sandboxes that exist,
// including after Close, and DeleteMicroVM is the only thing that removes
// one.
func TestSandboxes(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	ctx := testCtx(t)
	c := h.Client()

	if left, err := h.Sandboxes(); err != nil || left != nil {
		t.Errorf("Sandboxes before any create = %v, %v; want nil", left, err)
	}
	a := createVM(t, h, &types.MicroVMSpec{Uid: proto.String("b-second")})
	b := createVM(t, h, &types.MicroVMSpec{Uid: proto.String("a-first")})
	left, err := h.Sandboxes()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[0] != "a-first" || left[1] != "b-second" {
		t.Errorf("Sandboxes = %v, want [a-first b-second]", left)
	}
	if err := c.DeleteMicroVM(ctx, a.GetSpec().GetUid()); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteMicroVM(ctx, "never-existed"); !errors.Is(err, flintlock.ErrNotFound) {
		t.Errorf("DeleteMicroVM(unknown) = %v, want ErrNotFound", err)
	}
	_ = h.Close()
	left, err = h.Sandboxes()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != b.GetSpec().GetUid() {
		t.Errorf("Sandboxes after Close = %v, want [%s]", left, b.GetSpec().GetUid())
	}

	// A Host without a configured root creates a temporary one on first use.
	tmp := New(flintlock.FakeHostConfig{Name: "tmp"})
	t.Cleanup(func() {
		_ = tmp.Close()
		if root := tmp.SandboxRoot(); root != "" {
			_ = os.RemoveAll(root)
		}
	})
	if tmp.SandboxRoot() != "" {
		t.Error("SandboxRoot set before first create")
	}
	createVM(t, tmp, nil)
	if root := tmp.SandboxRoot(); root == "" || filepath.Base(root) == "" {
		t.Errorf("SandboxRoot after create = %q, want a temporary directory", root)
	}
	if left, _ := tmp.Sandboxes(); len(left) != 1 {
		t.Errorf("temporary root Sandboxes = %v, want one", left)
	}
}

// TestGetMicroVMErrors covers the sentinel mapping of the in-process
// client for the lookups the Placement fan-out relies on (SC-031).
func TestGetMicroVMErrors(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	ctx := testCtx(t)
	c := h.Client()
	if _, err := c.GetMicroVM(ctx, "missing"); !errors.Is(err, flintlock.ErrNotFound) {
		t.Errorf("GetMicroVM(missing) = %v, want ErrNotFound", err)
	}
	if _, err := c.GetMicroVM(ctx, ""); err == nil || errors.Is(err, flintlock.ErrNotFound) {
		t.Errorf("GetMicroVM(empty) = %v, want an invalid-argument error", err)
	}
	_ = h.Close()
	if _, err := c.GetMicroVM(ctx, "missing"); !errors.Is(err, flintlock.ErrUnavailable) {
		t.Errorf("GetMicroVM after Close = %v, want ErrUnavailable", err)
	}
	if _, err := c.CreateMicroVM(ctx, &types.MicroVMSpec{}); !errors.Is(err, flintlock.ErrUnavailable) {
		t.Errorf("CreateMicroVM after Close = %v, want ErrUnavailable", err)
	}
}
