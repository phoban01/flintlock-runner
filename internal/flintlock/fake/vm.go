package fake

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// vsockSocketName is the file name of the guest-agent vsock socket the fake
// reports in vsock_path. flintlockd puts it in the MicroVM's state
// directory; the sandbox is the fake's state directory.
const vsockSocketName = "vsock.sock"

// microVM is one MicroVM record. Its fields are guarded by Host.mu.
type microVM struct {
	vm      *types.MicroVM
	sandbox string
	// booted is closed when the boot timer has fired and the state moved
	// off PENDING, for tests that wait on it.
	booted chan struct{}
	// execs are the cancel functions of the exec processes running in this
	// MicroVM, so that DeleteMicroVM can kill them.
	execs  map[uint64]context.CancelFunc
	nextID uint64
}

// The store's errors are gRPC status errors so that the gRPC services return
// them as they are and the in-process Client maps them onto the sentinels.
// The codes are the ones a caller of flintlockd has to handle.

func errNotFound(uid string) error {
	return status.Errorf(codes.NotFound, "microvm spec %s not found", uid)
}

func errClosedStatus() error {
	return status.Error(codes.Unavailable, errClosed.Error())
}

//= docs/requirements/10-test-doubles.md#fake-host
//# The fake Host SHALL represent each MicroVM as a sandbox directory on the
//# local filesystem and SHALL run each `ExecCommand` as a local process
//# rooted in that directory, streaming standard input, standard output,
//# standard error and the exit code as `flintlockd` does.

// createMicroVM records a new MicroVM in state PENDING, creates its sandbox
// directory under SandboxRoot and starts its boot timer (TD-021, TD-022).
// A spec without a uid gets one; a spec whose uid is already known is
// rejected with ALREADY_EXISTS. An empty id becomes the uid and an empty
// namespace becomes "default", which is more forgiving than flintlockd's
// validation and is all a template from the Pool Manager needs.
func (h *Host) createMicroVM(spec *types.MicroVMSpec) (*types.MicroVM, error) {
	if spec == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid create microvm request: MicroVMSpec required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errClosedStatus()
	}

	spec = cloneProto(spec)
	uid := spec.GetUid()
	if uid == "" {
		var err error
		if uid, err = newUID(); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		spec.Uid = proto.String(uid)
	}
	if _, exists := h.vms[uid]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "microvm %s already exists", uid)
	}
	if spec.GetId() == "" {
		spec.Id = uid
	}
	if spec.GetNamespace() == "" {
		spec.Namespace = "default"
	}
	now := timestamppb.New(h.clk.Now())
	spec.CreatedAt = now
	spec.UpdatedAt = now

	root, err := h.ensureSandboxRootLocked()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	sandbox := filepath.Join(root, uid)
	if err := os.Mkdir(sandbox, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "creating sandbox %s: %v", sandbox, err)
	}

	rec := &microVM{
		vm: &types.MicroVM{
			Version: 1,
			Spec:    spec,
			Status:  &types.MicroVMStatus{State: types.MicroVMStatus_PENDING},
		},
		sandbox: sandbox,
		booted:  make(chan struct{}),
		execs:   make(map[uint64]context.CancelFunc),
	}
	h.vms[uid] = rec

	if h.cfg.BootDelay <= 0 {
		h.finishBootLocked(rec)
	} else {
		timer := h.clk.NewTimer(h.cfg.BootDelay)
		h.wg.Add(1)
		go h.bootMicroVM(rec, timer)
	}
	return cloneMicroVM(rec.vm), nil
}

//= docs/requirements/10-test-doubles.md#fake-host
//# The fake Host SHALL move a MicroVM from `PENDING` to `CREATED` after a
//# configurable boot delay and SHALL populate `vsock_path` in its status.

// bootMicroVM waits for the boot timer and completes the boot (TD-022). It
// gives up quietly when the Host is closed first, leaving the MicroVM
// PENDING, or when the MicroVM was deleted while booting.
func (h *Host) bootMicroVM(rec *microVM, timer clock.Timer) {
	defer h.wg.Done()
	select {
	case <-timer.C():
	case <-h.ctx.Done():
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	uid := rec.vm.GetSpec().GetUid()
	if h.vms[uid] != rec || rec.vm.GetStatus().GetState() != types.MicroVMStatus_PENDING {
		return
	}
	h.finishBootLocked(rec)
}

// finishBootLocked moves a PENDING MicroVM to CREATED with vsock_path set,
// or to FAILED when the CreateFails fault is on (TD-022, TD-025). Callers
// hold h.mu.
func (h *Host) finishBootLocked(rec *microVM) {
	//= docs/requirements/10-test-doubles.md#fake-host
	//# The fake Host SHALL support fault injection for a create that ends in
	//# `FAILED`, an exec stream dropped before the exit code, and a Host that
	//# stops answering.
	if h.Faults().CreateFails {
		rec.vm.Status.State = types.MicroVMStatus_FAILED
	} else {
		rec.vm.Status.State = types.MicroVMStatus_CREATED
		rec.vm.Status.VsockPath = filepath.Join(rec.sandbox, vsockSocketName)
	}
	rec.vm.Spec.UpdatedAt = timestamppb.New(h.clk.Now())
	rec.vm.Version++
	close(rec.booted)
}

// bootDone returns the channel closed when uid's boot has completed, or nil
// when uid is unknown. Tests wait on it after advancing the clock.
func (h *Host) bootDone(uid string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.vms[uid]
	if !ok {
		return nil
	}
	return rec.booted
}

// getMicroVM returns a copy of the named MicroVM or NOT_FOUND.
func (h *Host) getMicroVM(uid string) (*types.MicroVM, error) {
	if uid == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errClosedStatus()
	}
	rec, ok := h.vms[uid]
	if !ok {
		return nil, errNotFound(uid)
	}
	return cloneMicroVM(rec.vm), nil
}

// listMicroVMs returns copies of the MicroVMs in namespace (all namespaces
// when empty), further narrowed to one name when name is set, sorted by
// uid so that the order is stable.
func (h *Host) listMicroVMs(namespace string, name *string) ([]*types.MicroVM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errClosedStatus()
	}
	out := make([]*types.MicroVM, 0, len(h.vms))
	for _, rec := range h.vms {
		spec := rec.vm.GetSpec()
		if namespace != "" && spec.GetNamespace() != namespace {
			continue
		}
		if name != nil && spec.GetId() != *name {
			continue
		}
		out = append(out, cloneMicroVM(rec.vm))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetSpec().GetUid() < out[j].GetSpec().GetUid() })
	return out, nil
}

// deleteMicroVM kills the MicroVM's exec processes, removes its sandbox and
// forgets it. Deleting an unknown uid is NOT_FOUND. flintlockd moves the
// MicroVM through DELETING asynchronously; the fake deletes synchronously,
// so a GetMicroVM right after returns NOT_FOUND.
func (h *Host) deleteMicroVM(uid string) error {
	if uid == "" {
		return status.Error(codes.InvalidArgument, "invalid request")
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errClosedStatus()
	}
	rec, ok := h.vms[uid]
	if !ok {
		h.mu.Unlock()
		return errNotFound(uid)
	}
	rec.vm.Status.State = types.MicroVMStatus_DELETING
	delete(h.vms, uid)
	cancels := make([]context.CancelFunc, 0, len(rec.execs))
	for _, c := range rec.execs {
		cancels = append(cancels, c)
	}
	h.mu.Unlock()

	for _, c := range cancels {
		c()
	}
	if err := os.RemoveAll(rec.sandbox); err != nil {
		return status.Errorf(codes.Internal, "removing sandbox %s: %v", rec.sandbox, err)
	}
	return nil
}

// attachExec validates that uid can run a command, as flintlockd does before
// dialling the guest agent, and registers cancel so that DeleteMicroVM can
// kill the process. It returns the sandbox directory and a function that
// unregisters the exec.
func (h *Host) attachExec(uid string, cancel context.CancelFunc) (sandbox string, detach func(), err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", nil, errClosedStatus()
	}
	rec, ok := h.vms[uid]
	if !ok {
		return "", nil, errNotFound(uid)
	}
	if state := rec.vm.GetStatus().GetState(); state != types.MicroVMStatus_CREATED {
		return "", nil, status.Errorf(codes.FailedPrecondition, "microvm is not ready (state: %s)", strings.ToLower(state.String()))
	}
	id := rec.nextID
	rec.nextID++
	rec.execs[id] = cancel
	detach = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(rec.execs, id)
	}
	return rec.sandbox, detach, nil
}

//= docs/requirements/10-test-doubles.md#fake-host
//# The fake Host SHALL report a configurable flintlock version and service
//# flags from `ServerInfo` and SHALL provide a mode in which `ServerInfo`
//# returns `UNIMPLEMENTED`.

// serverInfo builds the ServerInfo response from the configuration
// (TD-026): the configured version, the uptime since New on the Host's
// clock, and the exec and SSH proxy flags with the listen address as their
// address when enabled, as flintlockd reports them. In the
// ServerInfoUnimplemented mode it returns UNIMPLEMENTED, which is what a
// flintlockd that predates the RPC answers (HO-013).
func (h *Host) serverInfo() (*mvmv1.ServerInfoResponse, error) {
	if h.cfg.ServerInfoUnimplemented {
		return nil, status.Error(codes.Unimplemented, "method ServerInfo not implemented")
	}
	h.mu.Lock()
	closed := h.closed
	addr := h.addr
	h.mu.Unlock()
	if closed {
		return nil, errClosedStatus()
	}
	if addr == "" {
		addr = h.cfg.Listen
	}
	uptime := h.clk.Now().Sub(h.started)
	if uptime < 0 {
		uptime = 0
	}
	resp := &mvmv1.ServerInfoResponse{
		Version: &mvmv1.VersionInfo{
			Version:    h.cfg.Version,
			BuildDate:  "fake",
			CommitHash: "fake",
		},
		Uptime:   durationpb.New(uptime),
		Exec:     &mvmv1.GuestAgentServiceInfo{Enabled: h.cfg.ExecEnabled},
		SshProxy: &mvmv1.GuestAgentServiceInfo{Enabled: h.cfg.SSHProxyEnabled},
	}
	if h.cfg.ExecEnabled {
		resp.Exec.Address = addr
	}
	if h.cfg.SSHProxyEnabled {
		resp.SshProxy.Address = addr
	}
	return resp, nil
}

// cloneProto deep-copies a message, keeping its concrete type.
func cloneProto[M proto.Message](m M) M {
	return proto.Clone(m).(M)
}
