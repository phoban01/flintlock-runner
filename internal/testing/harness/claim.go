package harness

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/testing/claimstack"
)

// Backend is the stack a Stack runs the Runner over.
type Backend string

const (
	// BackendBattery is the fake Pool Manager, which speaks battery's gRPC
	// API, with the exec Guest Transport straight to the fake Hosts. It is
	// the zero value's meaning.
	BackendBattery Backend = ""
	// BackendClaim is the claim stack (KF-192): the claim backend on an API
	// server with battery-operator's CRDs, the fake battery binding claims,
	// and the agent-exec Guest Transport through the test double of
	// battery-operator's Exec Agent in front of each fake Host.
	BackendClaim Backend = "claim"
)

// String names the backend in test output.
func (b Backend) String() string {
	if b == BackendBattery {
		return "battery"
	}
	return string(b)
}

// ErrNoEnvtest is Start's error when the claim stack is asked for and the
// envtest binaries it needs are missing. New skips the test on it, unless
// FLINTLOCK_RUNNER_REQUIRE_ENVTEST is 1, as it is in CI.
var ErrNoEnvtest = errors.New("harness: the claim stack needs the envtest binaries")

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//# The harness SHALL run every scenario of TD-051 over the claim
//# backend, the `agent-exec` Guest Transport, the Exec Agent and the fake
//# Host.

// startClaim starts the claim stack (claimstack.Stack) with opts.Hosts fake
// Hosts, each behind its own Exec Agent, and deploy/runner's identities and
// permissions applied unchanged. Each Host's Node carries the addresses of
// the Host Service stand-ins, as the Host Agent publishes them (KF-194).
// The Runner reaches the API server with a token of its own
// ServiceAccount, and the Exec Agents with the claim tokens of its claims.
func (s *Stack) startClaim(ctx context.Context) error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	hosts := make([]claimstack.Host, 0, s.opts.Hosts)
	for i := 1; i <= s.opts.Hosts; i++ {
		annotations := map[string]string{}
		for name := range s.serviceBackends {
			annotations[kubelabels.HostServiceAnnotation(name)] = s.ServiceAddr(name)
		}
		hosts = append(hosts, claimstack.Host{
			Name: fmt.Sprintf("host-%d", i), Annotations: annotations, BootDelay: s.opts.BootDelay,
		})
	}
	stack, note, err := claimstack.Start(ctx, claimstack.Options{
		Dir:       filepath.Join(s.Root, "claim"),
		DeployDir: filepath.Join(root, "deploy", "runner"),
		Hosts:     hosts,
	})
	if err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	if stack == nil {
		return fmt.Errorf("%w: %s", ErrNoEnvtest, note)
	}
	s.Claim = stack
	s.Hosts = stack.Hosts
	for _, a := range stack.Agents {
		s.logf("fake Host %s behind its exec agent on %s", a.Node(), a.Addr())
	}
	s.logf("claim stack: api server at %s, runner namespace %s", stack.Env.Config.Host, claimstack.RunnerNamespace)
	return nil
}

// Host is the fake Host of that name, nil if there is none.
func (s *Stack) Host(name string) *hostfake.Host {
	for _, h := range s.Hosts {
		if h.Config().Name == name {
			return h
		}
	}
	return nil
}

// LeaseHosts names the Host of every Lease held, one entry per Lease,
// sorted: from the fake Pool Manager's records on the battery stack, and
// from the Bound claims on the claim stack. It is how a scenario finds the
// Host its Job runs on, to fault it.
func (s *Stack) LeaseHosts(ctx context.Context) ([]string, error) {
	var out []string
	switch {
	case s.Claim != nil:
		claims, err := s.Claim.Claims(ctx)
		if err != nil {
			return nil, err
		}
		for _, cl := range claims {
			if cl.Status.Phase == batteryv1alpha1.MicroVMClaimBound && cl.Status.Host != nil {
				out = append(out, cl.Status.Host.NodeName)
			}
		}
	case s.PoolManager != nil:
		vms := map[string]string{}
		for _, vm := range s.PoolManager.VMs() {
			vms[vm.UID] = vm.Host
		}
		for _, l := range s.PoolManager.Leases() {
			out = append(out, vms[l.VMUID])
		}
	default:
		return nil, errors.New("harness: no fake pool manager or claim stack to ask for leases")
	}
	sort.Strings(out)
	return out, nil
}

// BoundClaims names the Bound claims on the claim stack, sorted.
func (s *Stack) BoundClaims(ctx context.Context) ([]string, error) {
	if s.Claim == nil {
		return nil, errors.New("harness: not a claim stack")
	}
	claims, err := s.Claim.Claims(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, cl := range claims {
		if cl.Status.Phase == batteryv1alpha1.MicroVMClaimBound {
			out = append(out, cl.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// claimsGoneTimeout bounds the wait for the Runner's last claims to go,
// and for the fake battery to delete their MicroVMs, after the Runner has
// shut down.
const claimsGoneTimeout = 10 * time.Second

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//# The harness SHALL fail if any scenario leaves a Lease held or
//# a sandbox directory behind after the Runner has shut down.

// checkClaims fails when a MicroVMClaim is left in the Runner's namespace
// once the Runner has shut down: on the claim stack a claim is a Lease
// (KF-150), and the Runner deletes each when its Job ends (KF-153). It
// then waits for the fake battery to delete the MicroVMs of the claims
// that went, so that a sandbox still there afterwards is one nobody
// deleted. The Pools stay: the Runner leaves them behind by design.
func (s *Stack) checkClaims(ctx context.Context) error {
	if s.Claim == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimsGoneTimeout)
	defer cancel()
	var left []string
	for {
		claims, err := s.Claim.Claims(ctx)
		if err != nil {
			return fmt.Errorf("harness: checking claims: %w", err)
		}
		left = left[:0]
		for _, cl := range claims {
			left = append(left, fmt.Sprintf("%s (%s)", cl.Name, cl.Status.Phase))
		}
		if len(left) == 0 && s.sandboxCount() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			if len(left) > 0 {
				return fmt.Errorf("harness: %d claim(s) left after the Runner shut down: %s", len(left), strings.Join(left, ", "))
			}
			// The sandboxes are reported by checkSandboxes.
			return nil
		case <-time.After(pollInterval):
		}
	}
}

// sandboxCount is the number of sandbox directories on the fake Hosts,
// counting one that cannot be read.
func (s *Stack) sandboxCount() int {
	n := 0
	for _, h := range s.Hosts {
		uids, err := h.Sandboxes()
		if err != nil {
			n++
		}
		n += len(uids)
	}
	return n
}

// stopClaim stops the claim stack: the fake battery, the Exec Agents, the
// fake Hosts and the API server.
func (s *Stack) stopClaim() error {
	if s.Claim == nil {
		return nil
	}
	if err := s.Claim.Stop(); err != nil {
		return fmt.Errorf("harness: stopping the claim stack: %w", err)
	}
	return nil
}

// claimMinMemoryMB is the least memory, in MiB, that battery-operator's
// Pool resource accepts for a MicroVM.
const claimMinMemoryMB = 1024
