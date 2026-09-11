//go:build e2e

package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// The scenarios in this file run the real flintlock-runner binary. They are
// behind the e2e build tag so that plain `go test ./...` stays fast; `make
// e2e` runs them. The binary is built once per test run unless
// FLINTLOCK_RUNNER_E2E_BINARY names one.

// runnerBinary is the binary every scenario runs.
var runnerBinary string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	runnerBinary = os.Getenv(EnvRunnerBinary)
	if runnerBinary == "" {
		dir, err := os.MkdirTemp("", "flintlock-harness-bin-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		bin, err := BuildRunner(ctx, dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		runnerBinary = bin
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
		t.Fatalf("flintlock-runner config show: %v\nstderr:\n%s", err, stderr.String())
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

// scenario is one TD-051 case: a Job, and what the fake GitLab has to have
// recorded once it is final.
type scenario struct {
	name  string
	job   Job
	check func(t *testing.T, s *Stack, rec *fakegitlab.JobRecord)
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=todo
//= tracking-issue=9
//# The harness SHALL cover a successful Job, a script failure with
//# its exit code, cancellation with `after_script`, a Job timeout, a wait on
//# an exhausted Pool, a Host becoming unhealthy during a Job, a Lease
//# expiring during a Job, an unknown Job Image, artifact upload and
//# dependency download, cache restore and save, and Host Service environment
//# injection.

// scenarios are the TD-051 cases covered so far: a successful Job and a
// script failure with its exit code. The rest are tracked in issue #9.
var scenarios = []scenario{
	{
		name: "successful job",
		job: Job{
			Name: "hello",
			Script: []string{
				`echo "hello from job $CI_JOB_ID ($CI_JOB_NAME)"`,
				`echo "stage cwd: $(pwd)"`,
				`for i in 1 2 3; do echo "step $i"; done`,
			},
		},
		check: func(t *testing.T, s *Stack, rec *fakegitlab.JobRecord) {
			if rec.Status != fakegitlab.StatusSuccess {
				t.Errorf("status %s (reason %q), want success", rec.Status, rec.FailureReason)
			}
			if last := rec.States[len(rec.States)-1]; last != fakegitlab.StatusSuccess {
				t.Errorf("final state reported %q, want success", last)
			}
			for _, want := range []string{"hello from job", "step 3", "Job succeeded"} {
				if !strings.Contains(rec.Trace, want) {
					t.Errorf("trace is missing %q:\n%s", want, rec.Trace)
				}
			}
			// The Stage ran in the Profile's builds directory under the
			// root, never in /builds.
			if !strings.Contains(rec.Trace, "stage cwd: "+filepath.Join(s.Root, "builds")) {
				t.Errorf("the stage did not run under %s:\n%s", filepath.Join(s.Root, "builds"), rec.Trace)
			}
		},
	},
	{
		name: "script failure with its exit code",
		job: Job{
			Name:   "fails",
			Script: []string{`echo "about to fail"`, `exit 3`, `echo "not reached"`},
		},
		check: func(t *testing.T, _ *Stack, rec *fakegitlab.JobRecord) {
			if rec.Status != fakegitlab.StatusFailed {
				t.Errorf("status %s, want failed", rec.Status)
			}
			if rec.FailureReason != "script_failure" {
				t.Errorf("failure reason %q, want script_failure", rec.FailureReason)
			}
			if rec.ExitCode != 3 {
				t.Errorf("exit code %d, want 3", rec.ExitCode)
			}
			if !strings.Contains(rec.Trace, "about to fail") || strings.Contains(rec.Trace, "not reached") {
				t.Errorf("trace does not stop at the failing line:\n%s", rec.Trace)
			}
			if !strings.Contains(rec.Trace, "exit status 3") {
				t.Errorf("trace does not report exit status 3:\n%s", rec.Trace)
			}
		},
	},
}

// runScenarios runs every scenario on a fresh Stack built from opts. Each
// Stack is shut down, and checked for held Leases and leftover sandboxes
// (TD-054), by the cleanup New registers.
func runScenarios(t *testing.T, opts Options) {
	opts.RunnerBinary = runnerBinary
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			s := New(t, opts)
			if err := s.StartRunner(context.Background()); err != nil {
				t.Fatal(err)
			}
			id, err := s.Enqueue(sc.job)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
			defer cancel()
			rec, err := s.Wait(ctx, id)
			if err != nil {
				t.Fatalf("%v\nlast lines of the runner log:\n%s", err, s.RunnerLogTail(40))
			}
			sc.check(t, s, rec)
			if t.Failed() {
				t.Logf("last lines of the runner log:\n%s", s.RunnerLogTail(40))
			}
		})
	}
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# The project SHALL provide an end-to-end harness that starts the
//# fake GitLab, the fake Pool Manager, a configurable number of fake Hosts
//# and the real `flintlock-runner` binary, submits Jobs and asserts on the
//# recorded trace and final state, and SHALL run in continuous integration
//# on a machine without KVM.

// TestFakeTier runs the scenarios against fake Hosts on this machine.
func TestFakeTier(t *testing.T) {
	runScenarios(t, FakeTier())
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

// TestRunnerIsGoneAfterShutdown checks that the Runner's process group is
// gone once Shutdown returns.
func TestRunnerIsGoneAfterShutdown(t *testing.T) {
	opts := FakeTier()
	opts.RunnerBinary = runnerBinary
	s := New(t, opts)
	if err := s.StartRunner(context.Background()); err != nil {
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
