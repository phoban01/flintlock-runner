package verify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/opstest"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

func newVerifier(t *testing.T, s *opstest.Stack, timeout time.Duration) *Verifier {
	t.Helper()
	return newVerifierWith(t, s, timeout, transport.NewFactory())
}

func newVerifierWith(t *testing.T, s *opstest.Stack, timeout time.Duration, f transport.Factory) *Verifier {
	t.Helper()
	v, err := New(Config{
		PoolManager:       s.Client,
		Hosts:             opstest.Dialer(),
		Transports:        f,
		Profiles:          s.Profiles,
		Timeout:           timeout,
		TransportDeadline: 5 * time.Second,
		PollInterval:      20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func failuresFor(r *fleet.VerifyReport, host string) []fleet.Failure {
	var out []fleet.Failure
	for _, f := range r.Failures {
		if f.Host == host {
			out = append(out, f)
		}
	}
	return out
}

func noLeasesLeft(t *testing.T, s *opstest.Stack) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.PoolManager.Leases()) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("verification left %d leases held: %+v", len(s.PoolManager.Leases()), s.PoolManager.Leases())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

//= docs/requirements/06-fleet.md#verification
//= type=test
//# The Fleet Controller SHALL provide a verification command that
//# checks every Host answers `ServerInfo` with the exec service enabled and
//# that the Pool Manager lists every declared Pool at its target size.

func TestVerifyChecksServerInfoExecAndPoolSize(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 2, ExecDisabled: map[string]bool{"host-2": true}})
	s.Declare(t)
	s.WaitAvailable(t, 2)

	inv := &fleet.Inventory{Hosts: append(s.Inventory, config.HostEntry{
		// A Host that is in the Inventory but answers nothing.
		Name: "host-gone", Endpoint: "127.0.0.1:1", Arch: s.Inventory[0].Arch, VCPU: 1, MemoryMB: 1,
		TLS: config.ClientTLS{Insecure: true},
	})}
	report, err := newVerifier(t, s, 2*time.Second).Verify(context.Background(), inv)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(report.Hosts) != 3 {
		t.Fatalf("hosts reported = %d, want every Inventory Host", len(report.Hosts))
	}
	h1 := report.Hosts[0]
	if h1.Info == nil || !h1.ExecEnabled || h1.Info.Version != "v0.0.0-fleet-test" {
		t.Errorf("host-1 = %+v, want ServerInfo answered with exec enabled", h1)
	}
	if f := failuresFor(report, "host-1"); len(f) != 0 {
		t.Errorf("host-1 failures = %v, want none", f)
	}
	if f := failuresFor(report, "host-2"); len(f) == 0 || f[0].Step != StepExecService {
		t.Errorf("host-2 failures = %v, want an %s failure for the disabled exec service", f, StepExecService)
	}
	if f := failuresFor(report, "host-gone"); len(f) == 0 || f[0].Step != StepServerInfo {
		t.Errorf("host-gone failures = %v, want a %s failure", f, StepServerInfo)
	}

	if len(report.Pools) != 1 || !report.Pools[0].AtTargetSize || report.Pools[0].TargetSize != 2 {
		t.Errorf("pools = %+v, want the one declared Pool at its target size 2", report.Pools)
	}
	if f := failuresFor(report, s.Ref().String()); len(f) != 0 {
		t.Errorf("pool failures = %v, want none", f)
	}
	noLeasesLeft(t, s)
}

func TestVerifyReportsUndeclaredPool(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 1})
	report, err := newVerifier(t, s, 300*time.Millisecond).Verify(context.Background(), &fleet.Inventory{Hosts: s.Inventory})
	if err != nil {
		t.Fatal(err)
	}
	f := failuresFor(report, s.Ref().String())
	if len(f) != 1 || f[0].Step != StepPool || !strings.Contains(f[0].Err.Error(), "does not list") {
		t.Fatalf("failures = %v, want the undeclared Pool named at step %s", report.Failures, StepPool)
	}
	if len(report.Pools) != 1 || report.Pools[0].AtTargetSize {
		t.Errorf("pools = %+v", report.Pools)
	}
}

func TestVerifyReportsPoolBelowTargetSize(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 1, PoolSize: 1})
	s.Declare(t)
	s.WaitAvailable(t, 1)
	// The Profile asks for more than the Pool Manager holds.
	s.Profiles[0].Pool.Size = 5
	v := newVerifier(t, s, 300*time.Millisecond)
	report, err := v.Verify(context.Background(), &fleet.Inventory{Hosts: s.Inventory})
	if err != nil {
		t.Fatal(err)
	}
	f := failuresFor(report, s.Ref().String())
	if len(f) != 1 || f[0].Step != StepPool || !strings.Contains(f[0].Err.Error(), "of 5") {
		t.Fatalf("failures = %v, want the Pool below its target size", report.Failures)
	}
	noLeasesLeft(t, s)
}

//= docs/requirements/06-fleet.md#verification
//= type=test
//# The verification command SHALL claim warm MicroVMs from every
//# Pool, run a trivial command in each through the Guest Transport and
//# release them, continuing until it has exercised every Host in the Pool's
//# host list or the verification timeout elapses, and SHALL report the time
//# from claim to readiness per Host.

func TestVerifyExercisesEveryHostOfThePool(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 3})
	s.Declare(t)
	s.WaitAvailable(t, 3)

	rec := newRecordingFactory()
	report, err := newVerifierWith(t, s, 20*time.Second, rec).Verify(context.Background(), &fleet.Inventory{Hosts: s.Inventory})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %v, want none", report.Failures)
	}
	for _, h := range report.Hosts {
		if !h.Exercised {
			t.Errorf("%s was not exercised; verification has to continue until every Host in the host list is", h.Host)
		}
		if h.ClaimToReady <= 0 {
			t.Errorf("%s claim to ready = %v, want the measured time", h.Host, h.ClaimToReady)
		}
	}
	// Every MicroVM ran the trivial command through the Guest Transport:
	// the trivial command ran to exit status zero on each Host.
	for _, h := range s.Inventory {
		if got := rec.commands(h.Name); len(got) == 0 || !strings.HasSuffix(got[0], " -c true") {
			t.Errorf("%s ran %v, want the trivial command", h.Name, got)
		}
	}
	noLeasesLeft(t, s)
}

func TestVerifyFindsPlacementWithoutHostOnClaim(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 2, OmitHostOnClaim: true})
	s.Declare(t)
	s.WaitAvailable(t, 2)
	report, err := newVerifier(t, s, 20*time.Second).Verify(context.Background(), &fleet.Inventory{Hosts: s.Inventory})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %v, want none: the Host is found by GetMicroVM when the claim omits it", report.Failures)
	}
	noLeasesLeft(t, s)
}

//= docs/requirements/06-fleet.md#verification
//= type=test
//# The verification command SHALL report any Host in a Pool's host
//# list on which no MicroVM could be exercised within the verification
//# timeout.

func TestVerifyReportsHostThatCannotBeExercised(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 2})
	s.Declare(t)
	s.WaitAvailable(t, 2)
	// host-2's guests never finish a command.
	s.Hosts[1].SetFaults(flintlock.HostFaults{DropExecBeforeExit: true})
	s.Profiles[0].ReadyTimeout = 200 * time.Millisecond

	start := time.Now()
	report, err := newVerifier(t, s, 2*time.Second).Verify(context.Background(), &fleet.Inventory{Hosts: s.Inventory})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("verification took %v; it has to stop at the verification timeout", elapsed)
	}
	f := failuresFor(report, "host-2")
	if len(f) != 1 || f[0].Step != StepExercise || !strings.Contains(f[0].Err.Error(), "no MicroVM") {
		t.Fatalf("host-2 failures = %v, want one %s failure", report.Failures, StepExercise)
	}
	if got := failuresFor(report, "host-1"); len(got) != 0 {
		t.Errorf("host-1 failures = %v, want none", got)
	}
	if !report.Hosts[0].Exercised || report.Hosts[1].Exercised {
		t.Errorf("exercised = %v %v, want host-1 only", report.Hosts[0].Exercised, report.Hosts[1].Exercised)
	}
	noLeasesLeft(t, s)
}

//= docs/requirements/06-fleet.md#verification
//= type=test
//# If verification fails on any Host or service, then the Fleet
//# Controller SHALL exit with a non-zero status naming each failure and the
//# step that failed.

func TestSummaryNamesEveryFailureAndStep(t *testing.T) {
	t.Parallel()
	if err := Summary(&fleet.VerifyReport{}); err != nil {
		t.Errorf("Summary of a clean report = %v, want nil", err)
	}
	err := Summary(&fleet.VerifyReport{Failures: []fleet.Failure{
		{Host: "host-2", Step: StepExecService, Err: errors.New("exec disabled")},
		{Host: "runner-ns/small", Step: StepPool, Err: errors.New("1 of 2")},
	}})
	if err == nil {
		t.Fatal("Summary of a failing report = nil")
	}
	for _, want := range []string{"host-2: step exec_service: exec disabled", "runner-ns/small: step pool: 1 of 2", "2 failure(s)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("summary %q does not contain %q", err, want)
		}
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Error("New accepted an empty Config")
	}
}

// recordingFactory wraps the real Guest Transport factory and records, per
// Host, every command that ran to exit status zero.
type recordingFactory struct {
	inner transport.Factory
	mu    sync.Mutex
	ran   map[string][]string
}

func newRecordingFactory() *recordingFactory {
	return &recordingFactory{inner: transport.NewFactory(), ran: map[string][]string{}}
}

func (f *recordingFactory) New(ctx context.Context, target transport.Target) (transport.Transport, error) {
	tr, err := f.inner.New(ctx, target)
	if err != nil {
		return nil, err
	}
	return &recordingTransport{Transport: tr, f: f, host: target.Host.Name()}, nil
}

func (f *recordingFactory) commands(host string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran[host]...)
}

type recordingTransport struct {
	transport.Transport
	f    *recordingFactory
	host string
}

func (r *recordingTransport) Run(ctx context.Context, cmd transport.Command) (int, error) {
	status, err := r.Transport.Run(ctx, cmd)
	if err == nil && status == 0 {
		r.f.mu.Lock()
		r.f.ran[r.host] = append(r.f.ran[r.host], strings.Join(append([]string{cmd.Path}, cmd.Args...), " "))
		r.f.mu.Unlock()
	}
	return status, err
}
