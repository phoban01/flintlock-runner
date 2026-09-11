// The end-to-end test runs gitlab-runner's run loop in process, and that
// loop has a data race of its own at the pinned commit: RunCommand.runWait
// writes stopSignal while the workers read it in processRunners without
// synchronisation (commands/multi.go). The race detector would fail the
// test for code that is not ours, so under -race the test is not built;
// `go test ./cmd/flintlock-runner/` without -race runs it, and the
// harness (TD-050) runs the binary out of process.

//go:build !race

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	pmfake "github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// stack is the fake GitLab, the fake Pool Manager and one fake Host, all in
// process, with the real `run` subcommand pointed at them.
type stack struct {
	gitlab   *fakegitlab.Server
	pm       *pmfake.PoolManager
	host     *hostfake.Host
	dir      string
	buildDir string
}

const testRunnerToken = "glrt-e2e-token"

// startStack starts the fakes and returns them with a configuration file
// for the Runner.
func startStack(t *testing.T) (*stack, string) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	host := hostfake.New(flintlock.FakeHostConfig{
		Name:        "host-a",
		Listen:      "127.0.0.1:0",
		SandboxRoot: filepath.Join(dir, "sandboxes"),
		Version:     "v0.9.0",
		ExecEnabled: true,
	})
	hostErr := make(chan error, 1)
	go func() { hostErr <- host.Serve(ctx) }()
	select {
	case <-host.Ready():
	case err := <-hostErr:
		t.Fatalf("fake host: %v", err)
	}

	hosts := pmfake.NewHosts()
	hosts.Add("host-a", host.Client(), host.Addr())
	pm := pmfake.New(poolmgr.FakeConfig{
		Listen:            "127.0.0.1:0",
		Hosts:             hosts,
		ReconcileInterval: 50 * time.Millisecond,
		ReadyTimeout:      10 * time.Second,
	})
	pmErr := make(chan error, 1)
	go func() { pmErr <- pm.Serve(ctx) }()
	select {
	case <-pm.Ready():
	case err := <-pmErr:
		t.Fatalf("fake pool manager: %v", err)
	}

	gl := fakegitlab.New(fakegitlab.Options{RunnerToken: testRunnerToken, LongPollTimeout: 500 * time.Millisecond})
	if err := gl.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		gl.Close()
		cancel()
		<-pmErr
		<-hostErr
	})

	s := &stack{gitlab: gl, pm: pm, host: host, dir: dir, buildDir: filepath.Join(dir, "guest", "builds")}
	cfg := fmt.Sprintf(`
gitlab:
  url: %s
  token: %s
  name: e2e-runner
  allow_insecure: true
  check_interval: 1s
  shutdown_timeout: 10s
pool_manager:
  endpoint: %s
  tls:
    insecure: true
inventory:
  hosts:
    - name: host-a
      endpoint: %s
      arch: arm64
      vcpu: 8
      memory_mb: 16384
      tls:
        insecure: true
profiles:
  - name: default
    arch: arm64
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    builds_dir: %s
    cache_dir: %s
    ready_timeout: 20s
    shell: %s
    pool:
      size: 1
scheduler:
  host_health_interval: 1s
observability:
  log_format: text
  listen_address: 127.0.0.1:0
state_dir: %s
`, gl.URL(), testRunnerToken, pm.Addr(), host.Addr(), s.buildDir, filepath.Join(dir, "guest", "cache"), bashPath(t), filepath.Join(dir, "state"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, path
}

// syncBuffer is a goroutine-safe writer for the Runner's output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunRunsAJobEndToEnd starts the real `run` subcommand against the fake
// GitLab, the fake Pool Manager and a fake Host, hands it one Job and checks
// that the Job's script ran in the MicroVM, that its output reached GitLab
// and that the Job was reported successful; then it stops the Runner with
// SIGTERM and checks that every Lease was handed back.
func TestRunRunsAJobEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end")
	}
	s, path := startStack(t)

	job := &spec.Job{
		ID: 42,
		JobInfo: spec.JobInfo{
			Name: "hello", Stage: "test", ProjectID: 7, ProjectName: "demo",
		},
		GitInfo:    spec.GitInfo{RepoURL: "https://gitlab.example.com/group/demo.git", Sha: "0123456789abcdef0123456789abcdef01234567", Ref: "main"},
		RunnerInfo: spec.RunnerInfo{Timeout: 120},
		Variables: spec.Variables{
			{Key: "GIT_STRATEGY", Value: "none", Public: true},
			{Key: "GREETING", Value: "hello-from-the-microvm", Public: true},
		},
		Steps: spec.Steps{{
			Name:   spec.StepNameScript,
			Script: spec.StepScript{`echo "$GREETING"`, `pwd`},
			When:   spec.StepWhenOnSuccess,
		}},
	}
	if err := s.gitlab.Enqueue(job); err != nil {
		t.Fatal(err)
	}

	out := &syncBuffer{}
	app := newApp()
	app.Writer, app.ErrWriter = out, out
	done := make(chan error, 1)
	go func() { done <- app.Run([]string{"flintlock-runner", "--config", path, "run"}) }()

	waitFor(t, 90*time.Second, "the job to finish", func() bool {
		select {
		case err := <-done:
			t.Fatalf("runner exited early: %v\n%s", err, out.String())
		default:
		}
		r := s.gitlab.Record(42)
		return r != nil && (r.Status == fakegitlab.StatusSuccess || r.Status == fakegitlab.StatusFailed)
	})
	rec := s.gitlab.Record(42)
	if rec.Status != fakegitlab.StatusSuccess {
		t.Fatalf("job status = %s (%s)\ntrace:\n%s\nrunner:\n%s", rec.Status, rec.FailureReason, rec.Trace, out.String())
	}
	t.Logf("trace:\n%s", rec.Trace)
	for _, want := range []string{"hello-from-the-microvm", "section_start:", PrepareSectionName} {
		if !strings.Contains(rec.Trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, rec.Trace)
		}
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("runner did not stop after SIGTERM\n%s", out.String())
	}
	waitFor(t, 10*time.Second, "every lease to be released", func() bool {
		return len(s.pm.Leases()) == 0
	})
}

// PrepareSectionName is the section the Executor writes during Prepare.
const PrepareSectionName = "flintlock_prepare"

// bashPath is the absolute path of bash on this machine. The fake Host runs
// the Profile's shell as a local process, and not every machine has
// /bin/bash.
func bashPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
