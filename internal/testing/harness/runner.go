package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runnerPackage is the import path of the Runner's main package.
const runnerPackage = "github.com/phoban01/flintlock-runner/cmd/flr"

// moduleGoMod is the first line of this module's go.mod, which is how
// moduleRoot recognises it.
const moduleGoMod = "module github.com/phoban01/flintlock-runner"

// killGrace is how long the Runner gets after SIGKILL before the harness
// gives up waiting for it.
const killGrace = 10 * time.Second

// BuildRunner builds the flr binary into dir and returns its
// path. It runs `go build` from this module's root, found by walking up
// from the working directory, so it works from a test in any package of
// the module and from a command started inside the checkout.
func BuildRunner(ctx context.Context, dir string) (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "flr")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, runnerPackage)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("harness: go build %s: %w\n%s", runnerPackage, err, stderr.String())
	}
	return out, nil
}

// helperPackage is the import path of the gitlab-runner-helper stand-in.
const helperPackage = "github.com/phoban01/flintlock-runner/internal/testing/harness/helperstub"

// BuildHelper builds the gitlab-runner-helper stand-in (helperstub) into dir
// and returns its path, for Options.HelperBinary.
func BuildHelper(ctx context.Context, dir string) (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "gitlab-runner-helper")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, helperPackage)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("harness: go build %s: %w\n%s", helperPackage, err, stderr.String())
	}
	return out, nil
}

// moduleRoot walks up from the working directory to this module's go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("harness: %w", err)
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(data), moduleGoMod+"\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("harness: not inside the flintlock-runner module; pass a prebuilt runner binary instead")
		}
		dir = parent
	}
}

// runnerBinary resolves the binary to run: Options.RunnerBinary, or a fresh
// build into the root.
func (s *Stack) runnerBinary(ctx context.Context) (string, error) {
	if s.opts.RunnerBinary != "" {
		path, err := filepath.Abs(s.opts.RunnerBinary)
		if err != nil {
			return "", fmt.Errorf("harness: runner binary: %w", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("harness: runner binary: %w", err)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("harness: runner binary %s is not an executable file", path)
		}
		return path, nil
	}
	s.logf("building flr (go build %s)", runnerPackage)
	dir := filepath.Join(s.Root, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("harness: %w", err)
	}
	return BuildRunner(ctx, dir)
}

// runnerProc is the Runner subprocess.
type runnerProc struct {
	cmd     *exec.Cmd
	log     *os.File
	logPath string
	done    chan struct{}
	// waitErr is cmd.Wait's result, readable once done is closed.
	waitErr error
}

func (r *runnerProc) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// StartRunner spawns `flr --config <ConfigPath> run` in its own
// process group, with its output in RunnerLog. The process is killed by
// Shutdown, and on Linux also when the process that started it dies, so no
// exit path of a test leaves it running.
func (s *Stack) StartRunner(ctx context.Context) error {
	s.mu.Lock()
	if s.runner != nil {
		s.mu.Unlock()
		return errors.New("harness: runner already started")
	}
	if s.down {
		s.mu.Unlock()
		return errors.New("harness: stack is shut down")
	}
	s.mu.Unlock()
	return s.spawnRunner(ctx, s.ConfigPath, s.RunnerLog, func(r *runnerProc) { s.runner = r })
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//# The harness SHALL include a scenario in which two Runners claim
//# from a Pool of one, and SHALL assert that exactly one obtains the pod.

// StartAnotherRunner starts one more Runner beside the one StartRunner
// started, as a second replica of the Runner's Deployment is (KF-123): the
// same runner name, token and Pools, with a state directory, a metrics
// address, a configuration file and a log of its own. It returns the path of
// its log. Shutdown stops it with the first.
func (s *Stack) StartAnotherRunner(ctx context.Context) (string, error) {
	s.mu.Lock()
	n := len(s.extra) + 2
	s.mu.Unlock()
	cfg := *s.Config
	cfg.StateDir = filepath.Join(s.Root, fmt.Sprintf("state-%d", n))
	metrics, err := freeLoopbackAddr()
	if err != nil {
		return "", err
	}
	cfg.Observability.ListenAddress = metrics
	path := filepath.Join(s.Root, fmt.Sprintf("config-%d.yaml", n))
	if err := writeConfigFile(path, &cfg); err != nil {
		return "", err
	}
	logPath := filepath.Join(s.Root, fmt.Sprintf("runner-%d.log", n))
	if err := s.spawnRunner(ctx, path, logPath, func(r *runnerProc) { s.extra = append(s.extra, r) }); err != nil {
		return "", err
	}
	return logPath, nil
}

// spawnRunner starts `flr --config <config> run` with its output in
// logPath, unless the Stack is shut down, and hands the process to record
// under the Stack's lock, so that Shutdown sees every Runner it has to
// stop.
func (s *Stack) spawnRunner(ctx context.Context, configPath, logPath string, record func(*runnerProc)) error {
	bin, err := s.runnerBinary(ctx)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("harness: runner log: %w", err)
	}
	cmd := exec.Command(bin, "--config", configPath, "run")
	cmd.Dir = s.Root
	cmd.Env = runnerEnv(os.Environ(), s.homeDir())
	var out io.Writer = logFile
	if s.opts.RunnerOutput != nil {
		out = io.MultiWriter(logFile, s.opts.RunnerOutput)
	}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = runnerSysProcAttr()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		_ = logFile.Close()
		return errors.New("harness: stack is shut down")
	}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("harness: starting %s: %w", bin, err)
	}
	r := &runnerProc{cmd: cmd, log: logFile, logPath: logPath, done: make(chan struct{})}
	go func() {
		r.waitErr = cmd.Wait()
		close(r.done)
	}()
	record(r)
	s.logf("flr started (pid %d, log %s)", cmd.Process.Pid, logPath)
	return nil
}

// runnerEnv is the environment of the Runner: this process's, without any
// FLINTLOCK_RUNNER_ variable, since those override secrets in the file
// (CF-002) and name its path, and with HOME inside the root so that nothing
// the run loop writes lands in the real home directory.
func runnerEnv(base []string, home string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "FLINTLOCK_RUNNER_") || k == "HOME" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+home)
}

// runnerExited returns a channel closed when the Runner exits, or nil when
// it was never started.
func (s *Stack) runnerExited() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner == nil {
		return nil
	}
	return s.runner.done
}

// runnerExitError describes an exit of the Runner that the harness did not
// ask for, with the end of its log.
func (s *Stack) runnerExitError() error {
	s.mu.Lock()
	r := s.runner
	s.mu.Unlock()
	return r.exitError()
}

// exitError describes an exit of r that the harness did not ask for, with
// the end of its log; nil while r runs.
func (r *runnerProc) exitError() error {
	if r == nil || !r.exited() {
		return nil
	}
	return fmt.Errorf("harness: flr exited unexpectedly (%v); last lines of %s:\n%s",
		exitDescription(r.waitErr), r.logPath, logTail(r.logPath, 20))
}

// stopRunner stops every Runner of the Stack at once, each as stop does,
// and returns every failure joined.
func (s *Stack) stopRunner(grace time.Duration) error {
	s.mu.Lock()
	procs := append([]*runnerProc(nil), s.extra...)
	if s.runner != nil {
		procs = append([]*runnerProc{s.runner}, procs...)
	}
	s.mu.Unlock()
	errs := make([]error, len(procs))
	var wg sync.WaitGroup
	for i, r := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.stop(r, grace)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stop sends SIGTERM to a Runner's process group and waits up to grace for
// it to exit (GL-070, GL-071), then kills the group. It returns an error
// when the Runner had exited before it was asked to, had to be killed, or
// exited with a non-zero status. The group is killed on every path, so a
// process the Runner left behind dies with it.
func (s *Stack) stop(r *runnerProc, grace time.Duration) error {
	defer func() { _ = r.log.Close() }()

	crashed := r.exited()
	var errs []error
	if crashed {
		errs = append(errs, r.exitError())
	} else {
		s.logf("stopping flr pid %d (SIGTERM, up to %s)", r.cmd.Process.Pid, grace)
		_ = signalGroup(r.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-r.done:
		case <-time.After(grace):
			errs = append(errs, fmt.Errorf("harness: flr did not exit within %s of SIGTERM and was killed", grace))
			_ = signalGroup(r.cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-r.done:
			case <-time.After(killGrace):
				errs = append(errs, fmt.Errorf("harness: flr (pid %d) survived SIGKILL", r.cmd.Process.Pid))
			}
		}
	}
	// Whatever the Runner started and left in its group goes too. A killed
	// child lingers until its new parent reaps it, so wait for the group
	// to empty rather than report a process that is already dead.
	_ = signalGroup(r.cmd.Process.Pid, syscall.SIGKILL)
	if err := waitGroupGone(r.cmd.Process.Pid, killGrace); err != nil {
		errs = append(errs, err)
	}
	if !crashed && r.exited() && r.waitErr != nil && len(errs) == 0 {
		errs = append(errs, fmt.Errorf("harness: flr exited on SIGTERM with %s; last lines of %s:\n%s",
			exitDescription(r.waitErr), r.logPath, logTail(r.logPath, 20)))
	}
	return errors.Join(errs...)
}

// signalGroup signals the process group led by pid. A group that no longer
// exists is not an error.
func signalGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// waitGroupGone polls until no process of group pgid is left, or fails
// after timeout.
func waitGroupGone(pgid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("harness: processes of the runner's group %d still exist %s after SIGKILL", pgid, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// exitDescription renders cmd.Wait's result for a message.
func exitDescription(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// RunnerLogTail returns the last n lines of the Runner's log.
func (s *Stack) RunnerLogTail(n int) string { return logTail(s.RunnerLog, n) }

// logTail returns the last n lines of the file at path.
func logTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(no runner log: %v)", err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return strings.Join(lines, "\n")
}
