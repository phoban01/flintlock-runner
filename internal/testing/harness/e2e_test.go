//go:build e2e

package harness

import (
	"bytes"
	"context"
	"errors"
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
	// helperBinary is the gitlab-runner-helper stand-in the artifact
	// scenario puts at the Profile's helper path.
	helperBinary string
	// cluster is the API server test environment every Stack on the
	// Kubernetes backend shares, nil when there is none; clusterNote says
	// why.
	cluster     *Cluster
	clusterNote string
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
	// The cluster stack's scenarios need a real kube-apiserver and etcd.
	// Where their binaries are missing those scenarios skip, unless
	// FLINTLOCK_RUNNER_REQUIRE_ENVTEST is set, as it is in CI, and then the
	// run fails.
	c, err := StartCluster()
	switch {
	case errors.Is(err, ErrNoCluster):
		clusterNote = err.Error()
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		return 1
	default:
		cluster = c
		defer func() {
			if err := c.Stop(); err != nil {
				fmt.Fprintln(os.Stderr, "stopping the test api server:", err)
			}
		}()
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

// runScenarios runs every scenario on a fresh Stack built from opts, skipping
// those that do not apply to its backend with the reason. Each Stack is shut
// down, and checked for held Leases and leftover sandboxes (TD-054), by the
// cleanup New registers.
func runScenarios(t *testing.T, opts Options) {
	opts.RunnerBinary = runnerBinary
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			if reason := sc.notOn[opts.Backend]; reason != "" {
				t.Skipf("not on the %s stack: %s", opts.Backend, reason)
			}
			o := opts
			if sc.opts != nil {
				sc.opts(&o)
			}
			s := New(t, o)
			if err := startRunner(s); err != nil {
				logStack(t, s)
				t.Fatal(err)
			}
			defer func() {
				if t.Failed() {
					logStack(t, s)
				}
			}()
			sc.run(t, s)
		})
	}
}

// logStack logs the end of the Runner's log and, on the Kubernetes stack,
// each Pod Provider's.
func logStack(t *testing.T, s *Stack) {
	t.Helper()
	t.Logf("last lines of the runner log:\n%s", s.RunnerLogTail(60))
	if d := s.DescribeCluster(context.Background()); d != "" {
		t.Logf("cluster:\n%s", d)
	}
	for _, h := range s.KubeHosts() {
		t.Logf("pod provider log of %s (last lines):\n%s", h.Node, lastLines(h.Log(), 40))
	}
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The harness SHALL run every scenario of TD-051 over the
//# Kubernetes pool backend, the `kube-exec` Guest Transport, the Pod Provider
//# and the fake Host.

// TestKubernetesStack runs the same scenarios on the cluster stack: the
// Runner on the Kubernetes pool backend and kube-exec, against an API
// server with the Pod Provider in front of each fake Host.
func TestKubernetesStack(t *testing.T) {
	runScenarios(t, KubernetesTier(t, cluster, clusterNote))
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

// allocatedRE matches the line the Executor logs once it holds a MicroVM:
// the MicroVM, which is a pod's UID on the Kubernetes stack.
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
