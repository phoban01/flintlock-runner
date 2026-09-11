package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	pmfake "github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
)

// runnerStopMargin is added to the configured shutdown timeout before the
// harness gives up on SIGTERM and kills the Runner. It is a variable so
// that the tests of that path do not wait ten seconds.
var runnerStopMargin = 10 * time.Second

// Bounds on the other steps of Shutdown.
const (
	// poolManagerStopTimeout covers the fake Pool Manager deleting every
	// MicroVM it created, which it bounds at 30s itself.
	poolManagerStopTimeout = 45 * time.Second
	// hostStopTimeout covers a fake Host killing its processes and draining
	// its streams, which it bounds at 5s itself.
	hostStopTimeout = 15 * time.Second
	// checkTimeout bounds each call the leak checks make.
	checkTimeout = 10 * time.Second
)

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//# The harness SHALL fail if any scenario leaves a Lease held or
//# a sandbox directory behind after the Runner has shut down.

// Shutdown stops the Stack in the order that makes its leak checks mean
// something (TD-054): the Runner first, with SIGTERM; then the check that
// the Pool Manager holds no Lease, made before the fake Pool Manager stops
// because stopping it drops every Lease; then the Pool Manager, which
// deletes the MicroVMs it created; then the Hosts; then the check that no
// sandbox is left on any fake Host. It returns every failure joined,
// including a Runner that crashed, had to be killed, or exited non-zero.
// The fake GitLab is closed last and the root removed unless KeepRoot is
// set. Shutdown is idempotent; only the first call does anything.
func (s *Stack) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.down {
		s.mu.Unlock()
		return nil
	}
	s.down = true
	s.mu.Unlock()

	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	grace := runnerStopMargin
	if s.Config != nil {
		grace += s.Config.GitLab.ShutdownTimeout
	}
	add(s.stopRunner(grace))
	add(s.checkLeases(ctx))
	add(s.stopPoolManager())
	add(s.stopHosts())
	add(s.checkSandboxes(ctx))
	s.GitLab.Close()
	if s.opts.KeepRoot {
		s.logf("kept %s", s.Root)
	} else if err := os.RemoveAll(s.Root); err != nil {
		add(fmt.Errorf("harness: removing %s: %w", s.Root, err))
	}
	if len(errs) == 0 {
		s.logf("shut down cleanly: no Lease held, no sandbox left")
	}
	return errors.Join(errs...)
}

// checkLeases fails when the Pool Manager still holds a Lease after the
// Runner has gone (TD-054). The fake is asked directly through its
// Inspector; a real Pool Manager is asked for the leased count of every
// Pool in the Runner namespace, which is all its API exposes.
func (s *Stack) checkLeases(ctx context.Context) error {
	if s.PoolManager != nil {
		leases := s.PoolManager.Leases()
		if len(leases) == 0 {
			return nil
		}
		held := make([]string, 0, len(leases))
		for _, l := range leases {
			held = append(held, fmt.Sprintf("%s (pool %s, microvm %s)", l.LeaseID, l.Pool, l.VMUID))
		}
		return fmt.Errorf("harness: %d Lease(s) still held after the Runner shut down: %s", len(held), strings.Join(held, ", "))
	}
	if s.pmAddr == "" {
		return nil
	}
	client, err := pmfake.Dial(s.pmAddr)
	if err != nil {
		return fmt.Errorf("harness: checking leases at %s: %w", s.pmAddr, err)
	}
	defer func() { _ = client.Close() }()
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkTimeout)
	defer cancel()
	pools, err := client.ListPools(callCtx, Namespace)
	if err != nil {
		return fmt.Errorf("harness: checking leases at %s: %w", s.pmAddr, err)
	}
	var held []string
	for _, p := range pools {
		if p.Status.Leased > 0 {
			held = append(held, fmt.Sprintf("pool %s has %d leased", p.Spec.Ref, p.Status.Leased))
		}
	}
	if len(held) > 0 {
		return fmt.Errorf("harness: Leases still held after the Runner shut down: %s", strings.Join(held, ", "))
	}
	return nil
}

// stopPoolManager stops the fake Pool Manager, which deletes every MicroVM
// it created, and closes its Host connections.
func (s *Stack) stopPoolManager() error {
	if s.pmCancel == nil {
		return nil
	}
	s.pmCancel()
	var err error
	select {
	case serveErr := <-s.pmDone:
		if serveErr != nil {
			err = fmt.Errorf("harness: fake pool manager: %w", serveErr)
		}
	case <-time.After(poolManagerStopTimeout):
		err = fmt.Errorf("harness: fake pool manager did not stop within %s", poolManagerStopTimeout)
	}
	if s.pmHosts != nil {
		if closeErr := s.pmHosts.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("harness: closing pool manager host connections: %w", closeErr))
		}
	}
	return err
}

// stopHosts stops every fake Host, which kills whatever it is still
// running.
func (s *Stack) stopHosts() error {
	if s.hostCancel == nil {
		return nil
	}
	s.hostCancel()
	var errs []error
	for i, done := range s.hostDone {
		select {
		case err := <-done:
			if err != nil {
				errs = append(errs, fmt.Errorf("harness: fake host %s: %w", s.Hosts[i].Config().Name, err))
			}
		case <-time.After(hostStopTimeout):
			errs = append(errs, fmt.Errorf("harness: fake host %s did not stop within %s", s.Hosts[i].Config().Name, hostStopTimeout))
		}
	}
	return errors.Join(errs...)
}

// checkSandboxes fails when a fake Host still has a sandbox directory,
// that is, a MicroVM nobody deleted (TD-054). The fake Host reads its
// sandbox root from disk, so the answer holds after it has stopped.
//
// On the hardware tier with the fake Pool Manager, the equivalent is a
// MicroVM still listed in the Runner namespace on a real Host after the
// fake has deleted what it created. With a real Pool Manager there is no
// equivalent: it keeps its warm Pool after the Runner goes, by design.
func (s *Stack) checkSandboxes(ctx context.Context) error {
	var left []string
	for _, h := range s.Hosts {
		uids, err := h.Sandboxes()
		if err != nil {
			return fmt.Errorf("harness: %w", err)
		}
		for _, uid := range uids {
			left = append(left, fmt.Sprintf("%s:%s", h.Config().Name, uid))
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("harness: %d sandbox director(ies) left after the Runner shut down: %s", len(left), strings.Join(left, ", "))
	}
	if !s.opts.Hardware() || s.PoolManager == nil {
		return nil
	}
	return s.checkHardwareMicroVMs(ctx)
}

// checkHardwareMicroVMs lists the Runner namespace on every hardware Host.
func (s *Stack) checkHardwareMicroVMs(ctx context.Context) error {
	var left []string
	for _, ep := range s.endpoints() {
		c, err := pmfake.AdminDialer{}.DialAdmin(ctx, ep)
		if err != nil {
			return fmt.Errorf("harness: dialing %s to check for leftover microvms: %w", ep.Name, err)
		}
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkTimeout)
		vms, err := c.ListMicroVMs(callCtx, Namespace)
		cancel()
		_ = c.Close()
		if err != nil {
			return fmt.Errorf("harness: listing microvms on %s: %w", ep.Name, err)
		}
		for _, vm := range vms {
			left = append(left, fmt.Sprintf("%s:%s", ep.Name, vm.GetSpec().GetUid()))
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("harness: %d microvm(s) left in namespace %s after shutdown: %s", len(left), Namespace, strings.Join(left, ", "))
	}
	return nil
}

// teardown stops whatever Start got as far as starting, without checks,
// when Start fails.
func (s *Stack) teardown() {
	s.mu.Lock()
	s.down = true
	s.mu.Unlock()
	_ = s.stopRunner(0)
	_ = s.stopPoolManager()
	_ = s.stopHosts()
	if s.GitLab != nil {
		s.GitLab.Close()
	}
	if s.Root != "" && (s.ownRoot || !s.opts.KeepRoot) {
		_ = os.RemoveAll(s.Root)
	}
}
