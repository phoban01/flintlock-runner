//go:build e2e

package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// The scenarios in this file run the real flr binary. They are
// behind the e2e build tag so that plain `go test ./...` stays fast; `make
// e2e` runs them. The binary is built once per test run unless
// FLINTLOCK_RUNNER_E2E_BINARY names one.

var (
	// runnerBinary is the binary every scenario runs.
	runnerBinary string
	// helperBinary is the gitlab-runner-helper stand-in that the artifact
	// and cache scenarios put at the Profile's helper path.
	helperBinary string
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "flintlock-harness-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	runnerBinary = os.Getenv(EnvRunnerBinary)
	if runnerBinary == "" {
		bin, err := BuildRunner(ctx, dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		runnerBinary = bin
	}
	if helperBinary, err = BuildHelper(ctx, dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// jobTimeout bounds how long a scenario waits for its Job.
const jobTimeout = 2 * time.Minute

func TestConfigShowAcceptsTheGeneratedConfiguration(t *testing.T) {
	s := New(t, FakeTier())
	cmd := exec.Command(runnerBinary, "--config", s.ConfigPath, "config", "show")
	cmd.Env = runnerEnv(os.Environ(), t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("flr config show: %v\nstderr:\n%s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{s.GitLab.URL(), s.PoolManagerAddr(), s.Inventory[0].Endpoint, s.Config.Profiles[0].BuildsDir} {
		if !strings.Contains(out, want) {
			t.Errorf("config show does not print %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{RunnerToken, HostToken} {
		if strings.Contains(out, secret) {
			t.Errorf("config show prints the secret %q", secret)
		}
	}
}

// runScenarios runs every scenario on a fresh Stack built from opts,
// skipping those that do not apply to it with the reason. Each Stack is
// shut down, and checked for held Leases and leftover sandboxes (TD-054),
// by the cleanup New registers.
func runScenarios(t *testing.T, opts Options) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			if sc.fakeHostsOnly && opts.Hardware() {
				t.Skip("injects a fault into a fake Host, and the hardware tier has none")
			}
			runOn(t, opts, sc.opts, sc.run)
		})
	}
}

// runOn starts a Stack from opts, adjusted by adjust when it is set, and
// its Runner, and runs run on it.
func runOn(t *testing.T, opts Options, adjust func(*Options), run func(*testing.T, *Stack)) {
	t.Helper()
	opts.RunnerBinary = runnerBinary
	if adjust != nil {
		adjust(&opts)
	}
	s := New(t, opts)
	if err := startRunner(s); err != nil {
		t.Logf("last lines of the runner log:\n%s", s.RunnerLogTail(60))
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			t.Logf("last lines of the runner log:\n%s", s.RunnerLogTail(60))
		}
	}()
	run(t, s)
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# The project SHALL provide an end-to-end harness that starts the
//# fake GitLab, the fake Pool Manager, a configurable number of fake Hosts
//# and the real `flr` binary, submits Jobs and asserts on the
//# recorded trace and final state, and SHALL run in continuous integration
//# on a machine without KVM.

// TestFakeTier runs the scenarios against fake Hosts on this machine, over
// the fake Pool Manager.
func TestFakeTier(t *testing.T) {
	runScenarios(t, FakeTier())
}

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//= type=test
//# The harness SHALL run every scenario of TD-051 over the claim
//# backend, the `agent-exec` Guest Transport, the Exec Agent and the fake
//# Host.

// TestClaimStack runs the same scenarios on the claim stack: the Runner on
// the claim backend and agent-exec, against an API server with
// battery-operator's CRDs, the fake battery, and the test double of
// battery-operator's Exec Agent in front of each fake Host.
func TestClaimStack(t *testing.T) {
	runScenarios(t, ClaimTier())
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# Where the environment variable naming a hardware Inventory is
//# set, the harness SHALL run the same scenarios against the real Hosts it
//# lists instead of fake Hosts, and SHALL be skipped otherwise.

// TestHardwareTier runs the same scenarios against the Hosts in
// FLINTLOCK_RUNNER_E2E_INVENTORY, and is skipped without it.
func TestHardwareTier(t *testing.T) {
	runScenarios(t, HardwareTier(t))
}

// startRunner starts the Runner and waits until it polls for Jobs.
func startRunner(s *Stack) error {
	if err := s.StartRunner(context.Background()); err != nil {
		return err
	}
	return s.WaitReady(context.Background())
}

// TestRunnerIsGoneAfterShutdown checks that the Runner's process group is
// gone once Shutdown returns.
func TestRunnerIsGoneAfterShutdown(t *testing.T) {
	opts := FakeTier()
	opts.RunnerBinary = runnerBinary
	s := New(t, opts)
	if err := startRunner(s); err != nil {
		t.Fatal(err)
	}
	pid := s.runner.cmd.Process.Pid
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if processGroupAlive(pid) {
		t.Errorf("the runner's process group %d is still alive after Shutdown", pid)
	}
}

// runJob queues j and waits for it to finish.
func runJob(t *testing.T, s *Stack, j Job) *fakegitlab.JobRecord {
	t.Helper()
	id, err := s.Enqueue(j)
	if err != nil {
		t.Fatal(err)
	}
	return wait(t, s, id)
}

// wait waits for Job id to finish.
func wait(t *testing.T, s *Stack, id int64) *fakegitlab.JobRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	rec, err := s.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// waitTrace waits until Job id has logged want.
func waitTrace(t *testing.T, s *Stack, id int64, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	if err := s.WaitTrace(ctx, id, want); err != nil {
		t.Fatal(err)
	}
}

// eventually polls cond until it holds, and fails the test after a minute.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(pollInterval)
	}
}

// allocatedRE matches the line the Executor logs once it holds a MicroVM.
var allocatedRE = regexp.MustCompile(`Allocated microvm (\S+) on host \S+`)

// allocation reads the MicroVM a Job ran on from its log.
func allocation(t *testing.T, rec *fakegitlab.JobRecord) string {
	t.Helper()
	m := allocatedRE.FindStringSubmatch(rec.Trace)
	if m == nil {
		t.Fatalf("job %d did not log its allocation:\n%s", rec.ID, rec.Trace)
	}
	return m[1]
}

// wantStatus checks a Job's final status and failure reason.
func wantStatus(t *testing.T, rec *fakegitlab.JobRecord, status, reason string) {
	t.Helper()
	if rec.Status != status || rec.FailureReason != reason {
		t.Errorf("job %d ended %s (reason %q), want %s (reason %q)\n%s", rec.ID, rec.Status, rec.FailureReason, status, reason, rec.Trace)
	}
}

// wantTrace checks that a Job's log contains every one of want.
func wantTrace(t *testing.T, rec *fakegitlab.JobRecord, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(rec.Trace, w) {
			t.Errorf("job %d's log is missing %q:\n%s", rec.ID, w, rec.Trace)
		}
	}
}

// wantNotInTrace checks that a Job's log contains none of unwanted.
func wantNotInTrace(t *testing.T, rec *fakegitlab.JobRecord, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(rec.Trace, u) {
			t.Errorf("job %d's log contains %q:\n%s", rec.ID, u, rec.Trace)
		}
	}
}
