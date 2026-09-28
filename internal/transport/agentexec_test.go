package transport_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"

	"github.com/phoban01/battery-operator/pkg/claimclient"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/testing/fakeexecagent"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL treat a response that
//# ends without an exit status frame as a stream failure, as EX-023 requires.

// TestAgentExecWithoutAnExitStatusIsAStreamFailure builds the agent-exec
// transport over a scripted exchange that ends cleanly, as a cut session
// through a relay would, after output and after nothing at all. Neither is
// a finished command: both are stream failures with no status, and the
// output that did arrive is still written. TestAgentExecCutResponses runs
// the same cuts through the Exec Agent's test double.
func TestAgentExecWithoutAnExitStatusIsAStreamFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	for name, stream := range map[string]*scriptedStream{
		"after output":   newScriptedStream(stdout("partial")),
		"with no output": newScriptedStream(),
	} {
		stub := &stubHost{name: "agent", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
		tr := newExecTransport(t, transport.Target{Kind: transport.KindAgentExec, Host: stub, VMUID: "vm"})
		var out bytes.Buffer
		status, err := tr.Run(ctx, transport.Command{Path: "sh", Stdout: &out})
		if !errors.Is(err, transport.ErrStreamFailed) || status != -1 {
			t.Errorf("%s: Run = (%d, %v), want a stream failure with no status", name, status, err)
		}
		if name == "after output" && out.String() != "partial" {
			t.Errorf("%s: output %q was not written", name, out.String())
		}
	}
}

// claimNamespace, claimPool and claimHolder are the namespace, Pool and
// Holder of the claims in these tests, and agentNode the Host's Node.
const (
	claimNamespace = "ci"
	claimPool      = "small"
	claimHolder    = "runner"
	agentNode      = "host-1"
)

// claimFixture is the claim design in miniature: a fake Host with the test
// double of battery-operator's Exec Agent in front of it, a World that
// plays the API server and battery, battery-operator's Client Library
// holding claims there, and the production agent-exec client pool over a
// dialler that finds each claim by its lease id, as the claim backend's
// does.
type claimFixture struct {
	t      *testing.T
	host   *fake.Host
	agent  *fakeexecagent.Agent
	world  *fakeexecagent.World
	claims *claimclient.Client
	agents *transport.AgentHosts

	mu   sync.Mutex
	held map[string]*claimclient.Claim
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	f := &claimFixture{t: t, held: map[string]*claimclient.Claim{}}
	f.host = fake.New(flintlock.FakeHostConfig{Name: agentNode, ExecEnabled: true, SandboxRoot: t.TempDir()})
	t.Cleanup(func() { _ = f.host.Close() })
	certs, err := fake.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.world, err = fakeexecagent.NewWorld(claimNamespace, claimPool)
	if err != nil {
		t.Fatal(err)
	}
	f.agent, err = fakeexecagent.Start(fakeexecagent.Config{
		Node: agentNode, CertFile: certs.ServerCertFile, KeyFile: certs.ServerKeyFile,
		Upstream: f.host.Client(), Authorizer: f.world,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.agent.Close() })
	f.claims = f.claimClient(certs.CAFile)
	f.agents, err = transport.NewAgentHosts(f.dial, transport.AgentExecConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.agents.Close() })
	return f
}

// claimClient is battery-operator's Client Library over the World, which
// verifies Exec Agents against the certificate authority in caFile.
func (f *claimFixture) claimClient(caFile string) *claimclient.Client {
	f.t.Helper()
	data, err := os.ReadFile(caFile)
	if err != nil {
		f.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		f.t.Fatalf("no certificates in %s", caFile)
	}
	cl, err := claimclient.New(claimclient.Config{Client: f.world.Kube, Namespace: claimNamespace, ServingCA: pool})
	if err != nil {
		f.t.Fatal(err)
	}
	return cl
}

// dial is the claim backend's side of agent-exec: it dials the Exec Agent
// of the claim it holds under the lease id, with that claim's Dial.
func (f *claimFixture) dial(ctx context.Context, leaseID string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	f.mu.Lock()
	claim := f.held[leaseID]
	f.mu.Unlock()
	if claim == nil {
		return nil, fmt.Errorf("no claim holds lease %s", leaseID)
	}
	return claim.Dial(ctx, opts...)
}

// claim creates a MicroVM on the Host and claims it with cl as name, and
// returns the MicroVM's uid. The claim is released when the test ends.
func (f *claimFixture) claim(cl *claimclient.Client, name string) string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	vm, err := f.host.Client().CreateMicroVM(ctx, &types.MicroVMSpec{Id: name, Namespace: testNamespace})
	if err != nil {
		f.t.Fatalf("CreateMicroVM: %v", err)
	}
	uid := vm.GetSpec().GetUid()
	claim, err := f.world.Claim(ctx, cl, claimclient.Request{Pool: claimPool, ServiceAccountName: claimHolder, Name: name}, uid, f.agent)
	if err != nil {
		f.t.Fatalf("claiming %s: %v", name, err)
	}
	f.t.Cleanup(func() { _ = claim.Release(context.Background()) })
	f.mu.Lock()
	f.held[name] = claim
	f.mu.Unlock()
	return uid
}

// transportFor is the agent-exec transport of the claim leaseID for the
// MicroVM vmUID, as the Executor builds it for a Job.
func (f *claimFixture) transportFor(leaseID, vmUID string) transport.Transport {
	f.t.Helper()
	client, release, err := f.agents.Lease(context.Background(), transport.AgentClaim{
		LeaseID: leaseID, VMUID: vmUID, Host: agentNode, Address: f.agent.Addr(),
	})
	if err != nil {
		f.t.Fatalf("Lease: %v", err)
	}
	f.t.Cleanup(release)
	return newExecTransport(f.t, transport.Target{Kind: transport.KindAgentExec, Host: client, VMUID: vmUID})
}

// sandbox is where the MicroVM uid keeps its files.
func (f *claimFixture) sandbox(uid string) string {
	f.t.Helper()
	dir, ok := f.host.SandboxPath(uid)
	if !ok {
		f.t.Fatalf("the fake host has no sandbox for %s", uid)
	}
	return dir
}

// ran reports whether a Stage made the file name in the MicroVM uid.
func (f *claimFixture) ran(uid, name string) bool {
	_, err := os.Stat(filepath.Join(f.sandbox(uid), name))
	return err == nil
}

// execTokens are the tokens the Exec Agent admitted exec requests on the
// MicroVM uid with.
func (f *claimFixture) execTokens(uid string) []string {
	var out []string
	for _, c := range f.agent.Calls() {
		if c.Method == "ExecCommand" && c.VMUID == uid && c.Admitted {
			out = append(out, c.Token)
		}
	}
	return out
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL authenticate to the Exec
//# Agent with a claim token of the Job's claim and SHALL verify the agent's
//# serving certificate against the configured certificate authority.

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//= type=test
//# The `agent-exec` Guest Transport SHALL be tested with
//# battery-operator's Client Library against a test double of
//# battery-operator's Exec Agent in front of the fake Host, with no KVM and
//# no battery.

// TestAgentExecUsesTheClaimsToken holds two claims on one Host through
// battery-operator's Client Library and runs a Stage in each claim's
// MicroVM over agent-exec. Each Stage reaches the Exec Agent with a claim
// token of its own claim, on a connection of its own. A client leased for
// one claim cannot run anything in the other claim's MicroVM, nor in its
// own once the claim is released. And a Client Library that verifies
// against a certificate authority that did not certify the agent reaches
// nothing: nothing runs.
func TestAgentExecUsesTheClaimsToken(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	vmA := f.claim(f.claims, "job-a")
	vmB := f.claim(f.claims, "job-b")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	trA := f.transportFor("job-a", vmA)
	trB := f.transportFor("job-b", vmB)
	for name, tr := range map[string]transport.Transport{"job-a": trA, "job-b": trB} {
		if status, err := tr.Run(ctx, transport.Command{Path: "touch", Args: []string{"ran-" + name}}); err != nil || status != 0 {
			t.Fatalf("%s: Run = (%d, %v)", name, status, err)
		}
	}
	if !f.ran(vmA, "ran-job-a") || !f.ran(vmB, "ran-job-b") {
		t.Fatal("a Stage did not run in its claim's MicroVM")
	}
	for name, uid := range map[string]string{"job-a": vmA, "job-b": vmB} {
		tokens := f.execTokens(uid)
		if len(tokens) == 0 {
			t.Fatalf("%s: the agent admitted no exec request", name)
		}
		for _, token := range tokens {
			if !slices.Contains(f.world.Tokens(name), token) {
				t.Errorf("%s: the Stage carried %q, which is no claim token of %s (%v)", name, token, name, f.world.Tokens(name))
			}
		}
	}

	// A claim's connection opens no other claim's MicroVM.
	cross := f.transportFor("job-a", vmB)
	if _, err := cross.Run(ctx, transport.Command{Path: "touch", Args: []string{"ran-across"}}); !errors.Is(err, transport.ErrStreamFailed) {
		t.Errorf("Run in job-b's MicroVM with job-a's client = %v, want a stream failure", err)
	}
	if f.ran(vmB, "ran-across") {
		t.Error("job-a's client ran a command in job-b's MicroVM")
	}

	// A released claim's token opens nothing, not even its own MicroVM.
	f.mu.Lock()
	claimA := f.held["job-a"]
	f.mu.Unlock()
	if err := claimA.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := trA.Run(ctx, transport.Command{Path: "touch", Args: []string{"ran-after-release"}}); !errors.Is(err, transport.ErrStreamFailed) {
		t.Errorf("Run after the claim's release = %v, want a stream failure", err)
	}
	if f.ran(vmA, "ran-after-release") {
		t.Error("a released claim's client ran a command")
	}

	// An authority that did not issue the agent's certificate: the
	// handshake fails, and the token is never sent to an agent that could
	// not be verified.
	other, err := fake.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vmC := f.claim(f.claimClient(other.CAFile), "job-c")
	before := len(f.agent.Calls())
	trC := f.transportFor("job-c", vmC)
	if _, err := trC.Run(ctx, transport.Command{Path: "touch", Args: []string{"ran-untrusted"}}); !errors.Is(err, transport.ErrStreamFailed) {
		t.Errorf("Run against an agent the configured authority did not certify = %v, want a stream failure", err)
	}
	if f.ran(vmC, "ran-untrusted") {
		t.Error("a command ran over a connection whose certificate was not verified")
	}
	if calls := f.agent.Calls()[before:]; len(calls) != 0 {
		t.Errorf("the agent got %d calls over a connection that should have failed its handshake", len(calls))
	}
}

// TestAgentHostsRefusals checks that the client pool is built only with a
// dialler, refuses a claim without a lease id, reports a dialler's
// failure, and gives out nothing once it is closed.
func TestAgentHostsRefusals(t *testing.T) {
	t.Parallel()
	if _, err := transport.NewAgentHosts(nil, transport.AgentExecConfig{}); err == nil {
		t.Error("the pool was built without a dialler")
	}
	failing := func(context.Context, string, ...grpc.DialOption) (*grpc.ClientConn, error) {
		return nil, errors.New("no such claim")
	}
	agents, err := transport.NewAgentHosts(failing, transport.AgentExecConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.Lease(context.Background(), transport.AgentClaim{Host: "host-1", Address: "127.0.0.1:10270"}); err == nil {
		t.Error("a claim with no lease id gave a client")
	}
	if _, _, err := agents.Lease(context.Background(), transport.AgentClaim{LeaseID: "job", Host: "host-1"}); err == nil ||
		!strings.Contains(err.Error(), "no such claim") {
		t.Errorf("Lease = %v, want the dialler's failure", err)
	}
	if err := agents.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.Lease(context.Background(), transport.AgentClaim{LeaseID: "job", Host: "host-1"}); err == nil {
		t.Error("a closed pool gave a client")
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//= type=test
//# The `agent-exec` Guest Transport SHALL be tested with
//# battery-operator's Client Library against a test double of
//# battery-operator's Exec Agent in front of the fake Host, with no KVM and
//# no battery.

// TestAgentExecRunsAStage is the exec transport's
// TestRunCarriesEveryPartOfTheCommand over agent-exec: a Stage script far
// larger than one stdin chunk on standard input, run in a working
// directory with an environment, writes to both output streams and exits
// with a status of its own choosing, and each part arrives. Ready succeeds,
// and every exit status comes back as it was.
func TestAgentExecRunsAStage(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	uid := f.claim(f.claims, "job")
	if err := os.MkdirAll(filepath.Join(f.sandbox(uid), "builds", "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := f.transportFor("job", uid)
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
	var out, errOut bytes.Buffer
	status, err := tr.Run(ctx, transport.Command{
		Path: "sh", Dir: "/builds/project", Env: map[string]string{"CI_JOB_STAGE": "build"},
		Stdin: strings.NewReader(script.String()), Stdout: &out, Stderr: &errOut,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != 7 {
		t.Errorf("exit status %d, want 7", status)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "/builds/project") || lines[1] != "build" || lines[2] != "last-line" {
		t.Errorf("stdout = %q, want the directory, the variable and the last line", out.String())
	}
	if !strings.Contains(errOut.String(), "to-stderr") {
		t.Errorf("stderr = %q", errOut.String())
	}
	for _, code := range []int{0, 1, 2, 42, 255} {
		status, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "exit " + strconv.Itoa(code)}})
		if err != nil || status != code {
			t.Errorf("exit %d: Run = (%d, %v)", code, status, err)
		}
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
	f := newClaimFixture(t)
	uid := f.claim(f.claims, "job")
	tr := f.transportFor("job", uid)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	answer := filepath.Join(f.sandbox(uid), "answer")

	seen := make(chan struct{})
	var once sync.Once
	var out lockedBuffer
	w := writerFunc(func(b []byte) (int, error) {
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
	status, err := tr.Run(ctx, transport.Command{Path: "sh", Dir: "/", Stdin: strings.NewReader(script), Stdout: w})
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
// stream failure carrying the context's cause, and the process in the
// guest is gone, which is the agent ending its stream to flintlockd once
// the caller's ended.
func TestAgentExecCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	uid := f.claim(f.claims, "job")
	tr := f.transportFor("job", uid)

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
			w := writerFunc(func(b []byte) (int, error) {
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
				_, err := tr.Run(ctx, transport.Command{Path: "sh", Dir: "/", Stdin: strings.NewReader(script), Stdout: w})
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
			data, err := os.ReadFile(filepath.Join(f.sandbox(uid), pidFile))
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

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL treat a response that
//# ends without an exit status frame as a stream failure, as EX-023 requires.

// TestAgentExecCutResponses cuts a Stage's response short in the two ways
// the Exec Agent can: flintlockd drops its stream before the exit code,
// and the Exec Agent stops while the Stage runs. Each ends as a stream
// failure with no exit status, never as a success.
func TestAgentExecCutResponses(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	uid := f.claim(f.claims, "job")
	tr := f.transportFor("job", uid)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	f.host.SetFaults(flintlock.HostFaults{DropExecBeforeExit: true})
	status, err := tr.Run(ctx, transport.Command{Path: "sh", Args: []string{"-c", "echo partial; exit 0"}})
	if !errors.Is(err, transport.ErrStreamFailed) || status != -1 {
		t.Errorf("flintlockd dropped the stream: Run = (%d, %v), want a stream failure", status, err)
	}
	f.host.SetFaults(flintlock.HostFaults{})

	type result struct {
		status int
		err    error
	}
	started := make(chan struct{})
	var once sync.Once
	w := writerFunc(func(b []byte) (int, error) {
		if strings.Contains(string(b), "started") {
			once.Do(func() { close(started) })
		}
		return len(b), nil
	})
	done := make(chan result, 1)
	go func() {
		status, err := tr.Run(ctx, transport.Command{Path: "sh", Dir: "/", Stdin: strings.NewReader("echo started\nsleep 30\nexit 0\n"), Stdout: w})
		done <- result{status, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the stage never started")
	}
	if err := f.agent.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if !errors.Is(r.err, transport.ErrStreamFailed) || r.status != -1 {
			t.Errorf("the agent stopped: Run = (%d, %v), want a stream failure", r.status, r.err)
		}
	case <-ctx.Done():
		t.Fatal("Run did not return after the agent stopped")
	}
}
