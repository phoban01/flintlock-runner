package agent_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/phoban01/flintlock-runner/internal/agent/agenttest"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL relay a request's streams to
//# `MicroVMExec.ExecCommand` on the local `flintlockd` and SHALL end every
//# response with an exit status frame, sent only after `flintlockd` has
//# reported the command's exit.

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL authenticate to the Exec
//# Agent with the Runner's ServiceAccount token and SHALL verify the agent's
//# serving certificate against the configured certificate authority.

// TestAgentExecRunsAStage is the exec transport's
// TestRunCarriesEveryPartOfTheCommand over agent-exec against the real Exec
// Agent: a Stage script far larger than one stdin chunk on standard input,
// run in a working directory with an environment, writes to both output
// streams and exits with a status of its own choosing, and each part
// arrives. The transport is the production one, holding the Runner's token
// and the agents' certificate authority and nothing else; Ready succeeds.
func TestAgentExecRunsAStage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, agenttest.HostOptions{})
	f.bind("job")
	time.Sleep(200 * time.Millisecond)
	if err := os.MkdirAll(filepath.Join(f.host.Sandbox(), "builds", "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := f.transportFor(f.runner.Token, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := tr.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	var script strings.Builder
	script.WriteString("pwd\necho $CI_JOB_STAGE\necho to-stderr >&2\n")
	for script.Len() < 256*1024 {
		script.WriteString(": padding the script past one stdin chunk\n")
	}
	script.WriteString("echo last-line\nexit 7\n")
	var stdout, stderr bytes.Buffer
	status, err := tr.Run(ctx, transport.Command{
		Path: "sh", Dir: "/builds/project", Env: map[string]string{"CI_JOB_STAGE": "build"},
		Stdin: strings.NewReader(script.String()), Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, f.host.Log())
	}
	if status != 7 {
		t.Errorf("exit status %d, want 7", status)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "/builds/project") || lines[1] != "build" || lines[2] != "last-line" {
		t.Errorf("stdout = %q, want the directory, the variable and the last line", stdout.String())
	}
	if !strings.Contains(stderr.String(), "to-stderr") {
		t.Errorf("stderr = %q", stderr.String())
	}

	for _, code := range []int{0, 1, 2, 42, 255} {
		status, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "exit " + strconv.Itoa(code)}})
		if err != nil || status != code {
			t.Errorf("exit %d: Run = (%d, %v)", code, status, err)
		}
	}

	// A certificate authority that did not issue the agent's certificate
	// fails the handshake: the transport never talks to an agent it cannot
	// verify, and nothing runs.
	certs, err := hostfakeCerts(t)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := transport.NewAgentHosts(transport.AgentExecConfig{CAFile: certs, TokenFile: tokenFile(t, f.runner.Token)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agents.Close() }()
	client, release, err := agents.Lease(f.host.Node, f.host.Address)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	untrusted, err := transport.NewFactory().New(ctx, transport.Target{Kind: transport.KindAgentExec, Host: client, VMUID: f.host.VMUID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Run(ctx, transport.Command{Path: "touch", Args: []string{"ran-untrusted"}}); !errors.Is(err, transport.ErrStreamFailed) {
		t.Errorf("Run against an agent the configured authority did not certify = %v, want a stream failure", err)
	}
	if f.ran("ran-untrusted") {
		t.Error("a command ran over a connection whose certificate was not verified")
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// TestAgentExecStreamsOutputAsItIsProduced checks EX-021 over agent-exec:
// the Stage prints a line and waits for the test to answer it, which the
// test does only once that line has reached its writer.
func TestAgentExecStreamsOutputAsItIsProduced(t *testing.T) {
	t.Parallel()
	f := newFixture(t, agenttest.HostOptions{})
	f.bind("job")
	time.Sleep(200 * time.Millisecond)
	tr := f.transportFor(f.runner.Token, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	answer := filepath.Join(f.host.Sandbox(), "answer")

	seen := make(chan struct{})
	var once sync.Once
	var out lockedBuffer
	stdout := writerFunc(func(b []byte) (int, error) {
		n, err := out.Write(b)
		if strings.Contains(out.String(), "first") {
			once.Do(func() { close(seen) })
		}
		return n, err
	})
	go func() {
		select {
		case <-seen:
			_ = os.WriteFile(answer, nil, 0o644)
		case <-ctx.Done():
		}
	}()
	script := "echo first\nwhile [ ! -e answer ]; do sleep 0.05; done\necho second\n"
	status, err := tr.Run(ctx, transport.Command{Path: "sh", Dir: "/", Stdin: strings.NewReader(script), Stdout: stdout})
	if err != nil || status != 0 || out.String() != "first\nsecond\n" {
		t.Errorf("Run = (%d, %v, %q), want (0, nil, first and second)", status, err, out.String())
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// TestAgentExecCancellationAndTimeout checks EX-024 and EX-046 over
// agent-exec: a Stage that would sleep a minute is stopped by its context
// being cancelled and by its deadline passing, Run returns promptly with a
// stream failure carrying the context's cause, and the process in the guest
// is gone, which is the agent cancelling its stream to flintlockd once the
// caller's ended.
func TestAgentExecCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t, agenttest.HostOptions{})
	f.bind("job")
	time.Sleep(200 * time.Millisecond)
	tr := f.transportFor(f.runner.Token, 10*time.Second)

	for _, how := range []string{"cancelled", "deadline"} {
		t.Run(how, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if how == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			started := make(chan struct{})
			var once sync.Once
			stdout := writerFunc(func(b []byte) (int, error) {
				if strings.Contains(string(b), "started") {
					once.Do(func() { close(started) })
				}
				return len(b), nil
			})
			pidFile := "pid-" + how
			result := make(chan error, 1)
			begun := time.Now()
			go func() {
				script := fmt.Sprintf("echo $$ > %s\necho started\nexec sleep 60\n", pidFile)
				_, err := tr.Run(ctx, transport.Command{Path: "sh", Dir: "/", Stdin: strings.NewReader(script), Stdout: stdout})
				result <- err
			}()
			select {
			case <-started:
			case <-time.After(testTimeout):
				t.Fatal("the stage never started")
			}
			if how == "cancelled" {
				cancel()
			}
			var err error
			select {
			case err = <-result:
			case <-time.After(testTimeout):
				t.Fatal("Run did not return after its context ended")
			}
			if elapsed := time.Since(begun); elapsed > 20*time.Second {
				t.Errorf("Run took %s", elapsed)
			}
			want := context.Canceled
			if how == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, transport.ErrStreamFailed) || !errors.Is(err, want) {
				t.Errorf("Run = %v, want a stream failure carrying %v", err, want)
			}
			data, err := os.ReadFile(filepath.Join(f.host.Sandbox(), pidFile))
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			deadline := time.Now().Add(testTimeout)
			for pid > 0 && syscall.Kill(pid, 0) == nil {
				if time.Now().After(deadline) {
					t.Fatalf("the stage's process %d is still running in the guest", pid)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL relay a request's streams to
//# `MicroVMExec.ExecCommand` on the local `flintlockd` and SHALL end every
//# response with an exit status frame, sent only after `flintlockd` has
//# reported the command's exit.

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL treat a response that
//# ends without an exit status frame as a stream failure, as EX-023 requires.

// TestACutResponseIsNeverASuccess is the first harness defect: a session
// cut short must never read as a finished command. Three ways of cutting
// one are tried while a Stage runs -- flintlockd dropping its stream before
// the exit code, the Exec Agent restarting, and the connection between the
// Runner and the agent being cut -- and each ends as a stream failure with
// no exit status, over the raw API as well as the transport. After the
// restart the agent serves again.
func TestACutResponseIsNeverASuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t, agenttest.HostOptions{})
	f.bind("job")
	time.Sleep(200 * time.Millisecond)

	t.Run("flintlockd drops the stream before the exit code", func(t *testing.T) {
		f.host.Fake.SetFaults(flintlock.HostFaults{DropExecBeforeExit: true})
		defer f.host.Fake.SetFaults(flintlock.HostFaults{})
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, gotExit, out, err := execRaw(ctx, f.rawExec(f.runner.Token), f.host.VMUID, "echo partial; exit 0")
		if err == nil || gotExit {
			t.Errorf("raw exec = (exit sent %v, %v), want an error status and no exit code", gotExit, err)
		}
		if statusCode(err) != codes.Unavailable {
			t.Errorf("status = %v, want unavailable", err)
		}
		t.Logf("output relayed before the drop: %q", out)
		status, err := f.transportFor(f.runner.Token, 0).Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "echo partial; exit 0"}})
		if !errors.Is(err, transport.ErrStreamFailed) || status != -1 {
			t.Errorf("Run = (%d, %v), want a stream failure", status, err)
		}
	})

	runLong := func(tr transport.Transport, marker string) (started <-chan struct{}, result <-chan error) {
		s := make(chan struct{})
		r := make(chan error, 1)
		var once sync.Once
		stdout := writerFunc(func(b []byte) (int, error) {
			if strings.Contains(string(b), "started") {
				once.Do(func() { close(s) })
			}
			return len(b), nil
		})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*testTimeout)
			defer cancel()
			status, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "echo started; sleep 20; touch " + marker}, Stdout: stdout})
			if err == nil {
				err = fmt.Errorf("the stage was reported finished with status %d", status)
			}
			r <- err
		}()
		return s, r
	}

	t.Run("the exec agent restarts", func(t *testing.T) {
		tr := f.transportFor(f.runner.Token, 0)
		started, result := runLong(tr, "finished-restart")
		<-started
		f.host.StopAgent()
		select {
		case err := <-result:
			if !errors.Is(err, transport.ErrStreamFailed) {
				t.Errorf("Run across an agent restart = %v, want a stream failure", err)
			}
		case <-time.After(testTimeout):
			t.Fatal("Run did not return when the agent stopped")
		}
		f.host.StartAgent()
		time.Sleep(200 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		var status int
		var err error
		agenttest.Eventually(t, "the restarted agent to serve", func() bool {
			status, err = tr.Run(ctx, transport.Command{Path: "true"})
			return err == nil
		})
		if status != 0 {
			t.Errorf("Run after the restart = (%d, %v)", status, err)
		}
	})

	t.Run("the connection to the agent is cut", func(t *testing.T) {
		proxy := newCutProxy(t, f.host.Address)
		agents, err := transport.NewAgentHosts(transport.AgentExecConfig{CAFile: env.Certs.CAFile, TokenFile: tokenFile(t, f.runner.Token)})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = agents.Close() }()
		client, release, err := agents.Lease(f.host.Node, proxy.addr())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		tr, err := transport.NewFactory().New(context.Background(), transport.Target{Kind: transport.KindAgentExec, Host: client, VMUID: f.host.VMUID})
		if err != nil {
			t.Fatal(err)
		}
		started, result := runLong(tr, "finished-cut")
		<-started
		proxy.cut()
		select {
		case err := <-result:
			if !errors.Is(err, transport.ErrStreamFailed) {
				t.Errorf("Run across a cut connection = %v, want a stream failure", err)
			}
		case <-time.After(testTimeout):
			t.Fatal("Run did not return when its connection was cut")
		}
	})
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# If `flintlockd` does not open the exec stream of a request
//# within the configured deadline, then the Exec Agent SHALL end the
//# response as a stream failure.

// TestAHungFlintlockdFailsWithinTheDeadline is the second harness defect:
// a flintlockd that accepts connections and answers nothing must not hold a
// Job until its timeout. The fake flintlockd stops answering; an exec
// through the agent, from a transport with no liveness watch of its own and
// a context of a minute, ends as a stream failure within the agent's open
// deadline of a second, and nothing runs.
func TestAHungFlintlockdFailsWithinTheDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, agenttest.HostOptions{ExecOpenTimeout: time.Second})
	f.bind("job")
	time.Sleep(200 * time.Millisecond)
	tr := f.transportFor(f.runner.Token, 0)
	f.host.Fake.SetFaults(flintlock.HostFaults{Unresponsive: true})
	defer f.host.Fake.SetFaults(flintlock.HostFaults{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	begun := time.Now()
	_, gotExit, _, err := execRaw(ctx, f.rawExec(f.runner.Token), f.host.VMUID, "touch ran-hung-raw")
	if statusCode(err) != codes.Unavailable || gotExit {
		t.Errorf("raw exec to a hung flintlockd = (exit sent %v, %v), want unavailable", gotExit, err)
	}
	status, err := tr.Run(ctx, transport.Command{Path: "touch", Args: []string{"ran-hung"}})
	elapsed := time.Since(begun)
	if !errors.Is(err, transport.ErrStreamFailed) || status != -1 {
		t.Errorf("Run = (%d, %v), want a stream failure", status, err)
	}
	if elapsed > 6*time.Second {
		t.Errorf("two exchanges took %s to fail, want each within the one-second open deadline", elapsed)
	}
	f.host.Fake.SetFaults(flintlock.HostFaults{})
	if f.ran("ran-hung") || f.ran("ran-hung-raw") {
		t.Error("a command ran although flintlockd never opened its stream")
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// TestAgentExecFailsWhenTheHostStopsAnswering checks EX-051 over
// agent-exec: a Stage is running when flintlockd stops answering, and the
// transport's liveness probe, a GetMicroVM the agent authorizes by the
// claim and relays, fails, so Run ends within its deadline.
func TestAgentExecFailsWhenTheHostStopsAnswering(t *testing.T) {
	t.Parallel()
	f := newFixture(t, agenttest.HostOptions{})
	f.bind("job")
	time.Sleep(200 * time.Millisecond)
	tr := f.transportFor(f.runner.Token, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	started := make(chan struct{})
	var once sync.Once
	stdout := writerFunc(func(b []byte) (int, error) {
		once.Do(func() { close(started) })
		return len(b), nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "echo started; sleep 50"}, Stdout: stdout})
		result <- err
	}()
	<-started
	f.host.Fake.SetFaults(flintlock.HostFaults{Unresponsive: true})
	defer f.host.Fake.SetFaults(flintlock.HostFaults{})
	begun := time.Now()
	select {
	case err := <-result:
		if !errors.Is(err, transport.ErrStreamFailed) {
			t.Errorf("Run = %v, want a stream failure", err)
		}
		if elapsed := time.Since(begun); elapsed > 15*time.Second {
			t.Errorf("Run took %s to fail, want within the transport deadline", elapsed)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run did not fail when the host stopped answering")
	}
}

// hostfakeCerts writes a certificate authority that issued nothing the
// agent serves, and returns its file.
func hostfakeCerts(t *testing.T) (string, error) {
	t.Helper()
	certs, err := writeTestCerts(t.TempDir())
	if err != nil {
		return "", err
	}
	return certs, nil
}
