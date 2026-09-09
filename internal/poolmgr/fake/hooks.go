package fake

import (
	"context"
	"errors"
	"fmt"
	"io"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// runHooks runs a Pool's create or pre-lease commands in vm over the Host's
// MicroVMExec service, the way battery does, and fails on the first command
// that errors or exits non-zero. An injected hook failure (TD-010) fires at
// this point whether or not the Pool declares any command, so tests can
// exercise VM_HOOK_FAILED and the failure policy without a guest agent.
// Before create hooks it waits for the guest agent to answer, because a
// MicroVM that has just reached CREATED has usually not started it yet.
func (p *PoolManager) runHooks(ctx context.Context, vm *vmState, kind poolmgr.HookKind, cmds []string) error {
	if p.consumeHookFailure(vm.pool, kind) {
		return fmt.Errorf("fault injection: %s hook failed", kind)
	}
	if len(cmds) == 0 {
		return nil
	}
	hc, err := p.hostClient(vm.host)
	if err != nil {
		return err
	}
	if kind == poolmgr.HookCreate {
		if err := p.waitGuestReady(ctx, hc, vm.uid); err != nil {
			return err
		}
	}
	for _, cmd := range cmds {
		code, err := execHook(ctx, hc, vm.uid, cmd)
		if err != nil {
			return fmt.Errorf("%s hook %q: %w", kind, cmd, err)
		}
		if code != 0 {
			return fmt.Errorf("%s hook %q: exit code %d", kind, cmd, code)
		}
	}
	return nil
}

// consumeHookFailure takes one pending HookFailure for the Pool and hook
// kind, if any. A HookFailure with a zero Pool matches every Pool.
func (p *PoolManager) consumeHookFailure(key poolKey, kind poolmgr.HookKind) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.faults.HookFailures {
		hf := &p.faults.HookFailures[i]
		if hf.Hook != kind || hf.Remaining <= 0 {
			continue
		}
		if hf.Pool != (poolmgr.PoolRef{}) && keyOf(hf.Pool) != key {
			continue
		}
		hf.Remaining--
		if hf.Remaining == 0 {
			p.faults.HookFailures = append(p.faults.HookFailures[:i], p.faults.HookFailures[i+1:]...)
		}
		return true
	}
	return false
}

// waitGuestReady retries a no-op command until the guest agent answers or
// cfg.ReadyTimeout passes on the fake's clock. Like battery's WaitReady it
// treats any exec failure as "not yet".
func (p *PoolManager) waitGuestReady(ctx context.Context, hc flintlock.HostClient, uid string) error {
	deadline := p.cfg.Clock.Now().Add(p.cfg.ReadyTimeout)
	var last error
	for {
		if _, err := execHook(ctx, hc, uid, "true"); err == nil {
			return nil
		} else if isCtxErr(err) {
			return err
		} else {
			last = err
		}
		if !p.cfg.Clock.Now().Before(deadline) {
			return fmt.Errorf("guest agent of %s not ready within %s: %w", uid, p.cfg.ReadyTimeout, last)
		}
		timer := p.cfg.Clock.NewTimer(createPollInterval)
		select {
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// execHook runs one shell command in a MicroVM through ExecCommand and
// returns its exit code. It sends ExecStart with shell set and no stdin,
// half-closes, and reads until exit_code; an error message from the server
// or a stream that ends first is an error.
func execHook(ctx context.Context, hc flintlock.HostClient, uid, cmd string) (int32, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := hc.Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("open exec stream: %w", err)
	}
	start := &execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: &execv1.ExecStart{
		Uid:   uid,
		Cmd:   cmd,
		Shell: true,
	}}}
	if err := stream.Send(start); err != nil {
		return 0, fmt.Errorf("send exec start: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return 0, fmt.Errorf("close send: %w", err)
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return 0, errors.New("exec stream closed before exit code")
		}
		if err != nil {
			return 0, fmt.Errorf("exec stream: %w", err)
		}
		switch payload := resp.GetPayload().(type) {
		case *execv1.ExecCommandResponse_Error:
			return 0, fmt.Errorf("exec error: %s", payload.Error)
		case *execv1.ExecCommandResponse_ExitCode:
			return payload.ExitCode, nil
		case *execv1.ExecCommandResponse_Stdout, *execv1.ExecCommandResponse_Stderr:
		}
	}
}
