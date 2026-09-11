package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// These tests stand a shell script in for the Runner binary, so that the
// harness's process handling is tested without the executor: a Runner that
// exits at once, one that ignores SIGTERM, and one that stops when asked.

// processGroupAlive reports whether any process in group pgid exists.
func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// fakeRunner writes an executable bash script that stands in for
// flintlock-runner and returns its path.
func fakeRunner(t *testing.T, body string) string {
	t.Helper()
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	path := filepath.Join(t.TempDir(), "flintlock-runner")
	script := "#!" + bash + "\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// shortShutdown makes the configured shutdown timeout one second.
func shortShutdown(c *config.Config) { c.GitLab.ShutdownTimeout = time.Second }

func TestARunnerThatExitsEarlyFailsWaitAndShutdown(t *testing.T) {
	bin := fakeRunner(t, `echo "runner: not implemented yet"; exit 3`)
	s := start(t, Options{Hosts: 1, RunnerBinary: bin})
	if err := s.StartRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, err := s.Enqueue(Job{Script: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = s.Wait(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "not implemented yet") {
		t.Errorf("Wait on a Runner that exited: %v, want its exit status and log tail", err)
	}
	if err := s.Shutdown(context.Background()); err == nil || !strings.Contains(err.Error(), "exited unexpectedly") {
		t.Errorf("Shutdown after the Runner exited on its own: %v, want it reported", err)
	}
}

func TestARunnerIgnoringSIGTERMIsKilled(t *testing.T) {
	old := runnerStopMargin
	runnerStopMargin = 500 * time.Millisecond
	t.Cleanup(func() { runnerStopMargin = old })

	// A child in the same group checks that the whole group goes.
	bin := fakeRunner(t, `trap '' TERM; sleep 300 & wait`)
	s := start(t, Options{Hosts: 1, RunnerBinary: bin, Configure: shortShutdown})
	if err := s.StartRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := s.runner.cmd.Process.Pid
	time.Sleep(200 * time.Millisecond) // let the script install its trap

	err := s.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "was killed") {
		t.Errorf("Shutdown of a Runner that ignores SIGTERM: %v, want it reported as killed", err)
	}
	if processGroupAlive(pid) {
		t.Errorf("process group %d survived Shutdown", pid)
	}
}

func TestARunnerThatStopsOnSIGTERMShutsDownClean(t *testing.T) {
	bin := fakeRunner(t, `trap 'echo "runner: stopping"; exit 0' TERM; echo "runner: started $*"; while true; do sleep 0.1; done`)
	s := start(t, Options{Hosts: 1, RunnerBinary: bin, Configure: shortShutdown})
	if err := s.StartRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := s.runner.cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(s.RunnerLogTail(5), "runner: started") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !s.RunnerRunning() {
		t.Fatal("the stand-in Runner is not running")
	}
	logTail := s.RunnerLogTail(5)
	if want := "--config " + s.ConfigPath + " run"; !strings.Contains(logTail, want) {
		t.Errorf("runner arguments: log %q, want %q", logTail, want)
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if processGroupAlive(pid) {
		t.Errorf("process group %d survived Shutdown", pid)
	}
}

func TestRunnerEnvDropsOverridesAndMovesHome(t *testing.T) {
	env := runnerEnv([]string{
		"PATH=/usr/bin", "HOME=/home/me",
		"FLINTLOCK_RUNNER_GITLAB_TOKEN=glrt-from-env", "FLINTLOCK_RUNNER_CONFIG=/etc/x.yaml",
	}, "/tmp/root/home")
	got := strings.Join(env, " ")
	if got != "PATH=/usr/bin HOME=/tmp/root/home" {
		t.Errorf("runnerEnv = %q, want PATH kept, the overrides dropped and HOME moved", got)
	}
}
