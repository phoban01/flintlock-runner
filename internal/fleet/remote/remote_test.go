package remote

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsfake"
	"github.com/phoban01/flintlock-runner/internal/fleet/discovery"
	"github.com/phoban01/flintlock-runner/internal/fleet/remote/sshtest"
)

// lockedBuffer is a bytes.Buffer safe to read while a Run writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

var ec2Inst = fleet.Instance{ID: "i-0abc", Type: "m7g.metal", Arch: config.ArchARM64, PrivateIP: "10.0.0.7"}

//= docs/requirements/06-fleet.md#remote-execution
//= type=test
//# The Fleet Controller SHALL execute commands on instances through
//# AWS Systems Manager Run Command by default.

func TestNewRunsThroughSystemsManagerByDefault(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.RemoteMode{"", config.RemoteSSM} {
		ssm := awsfake.NewSSM()
		ssm.SetReply(ec2Inst.ID, awsfake.Reply{Stdout: "containerd 2.1.4\n"})
		// An SSH section is present but not selected; it must be ignored.
		f := &config.Fleet{Remote: config.Remote{Mode: mode, SSH: config.SSHRemote{User: "ec2-user", KeyFile: "/nonexistent"}}}
		r, err := New(f, ssm)
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		script := fleet.Script{Name: "detect", Content: "#!/bin/bash\ncontainerd --version\n", Timeout: time.Minute}
		res, err := r.Run(context.Background(), ec2Inst, script, nil)
		if err != nil {
			t.Fatalf("mode %q: Run: %v", mode, err)
		}
		if res.ExitCode != 0 || res.Stdout != "containerd 2.1.4\n" {
			t.Errorf("mode %q: result %+v", mode, res)
		}
		sent := ssm.SentCommands()
		if len(sent) != 1 {
			t.Fatalf("mode %q: %d commands sent, want 1", mode, len(sent))
		}
		want := fleet.SendCommandInput{InstanceIDs: []string{ec2Inst.ID}, Script: script.Content, Comment: "detect", Timeout: time.Minute}
		if got := sent[0]; got.Script != want.Script || got.Comment != want.Comment || got.Timeout != want.Timeout || len(got.InstanceIDs) != 1 || got.InstanceIDs[0] != ec2Inst.ID {
			t.Errorf("mode %q: sent %+v, want %+v", mode, got, want)
		}
	}
}

// sshFleet is a fleet section selecting SSH to srv.
func sshFleet(srv *sshtest.Server, user string) *config.Fleet {
	return &config.Fleet{Remote: config.Remote{Mode: config.RemoteSSH, SSH: config.SSHRemote{
		User: user, KeyFile: srv.ClientKeyFile, Port: srv.Port(), KnownHostsFile: srv.KnownHostsFile,
	}}}
}

// staticLocalhost discovers the in-process server's machine through the
// static provider, the path TD-044 exists for.
func staticLocalhost(t *testing.T) fleet.Instance {
	t.Helper()
	d, err := discovery.New(&config.Fleet{Discovery: config.Discovery{Static: []config.StaticHost{
		{Name: "lab-1", Address: "127.0.0.1", Arch: config.ArchAMD64, VCPU: 4, MemoryMB: 8192},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	insts, err := d.Discover(context.Background())
	if err != nil || len(insts) != 1 {
		t.Fatalf("static discovery = %v, %v", insts, err)
	}
	return insts[0]
}

//= docs/requirements/06-fleet.md#remote-execution
//= type=test
//# Where SSH is configured, the Fleet Controller SHALL execute
//# commands over SSH with the configured user and key instead of Systems
//# Manager.

//= docs/requirements/10-test-doubles.md#fake-aws
//= type=test
//# The Fleet Controller SHALL provide a static discovery provider
//# that reads Hosts from a list in the configuration, so that provisioning
//# over SSH can be exercised against any Linux machine without an AWS
//# account.

// TestSSHModeRunsOverSSHWithConfiguredUserAndKey runs a script with secret
// standard input on a statically listed machine over SSH, and checks the
// configured user and key were used, Systems Manager was not, and neither
// the script nor the secret was on a command line.
func TestSSHModeRunsOverSSHWithConfiguredUserAndKey(t *testing.T) {
	t.Parallel()
	srv := sshtest.New(t)
	ssm := awsfake.NewSSM()
	r, err := New(sshFleet(srv, "fleet-admin"), ssm)
	if err != nil {
		t.Fatal(err)
	}
	inst := staticLocalhost(t)
	script := fleet.Script{
		Name:    "flintlockd",
		Content: "#!/bin/bash\nset -euo pipefail\nread -r token\necho \"token has ${#token} bytes\"\n",
		Stdin:   strings.NewReader("s3cret-token\n"),
		Timeout: 30 * time.Second,
	}
	res, err := r.Run(context.Background(), inst, script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "token has 12 bytes\n" {
		t.Errorf("result %+v", res)
	}
	if n := len(ssm.SentCommands()); n != 0 {
		t.Errorf("%d commands went through Systems Manager in SSH mode", n)
	}
	execs := srv.Execs()
	if len(execs) == 0 {
		t.Fatal("nothing ran over SSH")
	}
	for _, e := range execs {
		if e.User != "fleet-admin" {
			t.Errorf("ran as %q, want the configured user", e.User)
		}
		if strings.Contains(e.Command, "s3cret") || strings.Contains(e.Command, "token has") {
			t.Errorf("command line carries the script or its secret: %q", e.Command)
		}
	}

	// Another key is refused: the configured key is the one that got in.
	other := sshtest.New(t)
	f := sshFleet(srv, "fleet-admin")
	f.Remote.SSH.KeyFile = other.ClientKeyFile
	r2, err := New(f, ssm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Run(context.Background(), inst, fleet.Script{Name: "true", Content: "true\n"}, nil); err == nil {
		t.Error("a key the server does not accept got in")
	}
}

// TestSSHVerifiesHostKey checks that a server whose key is not the one in
// known_hosts is refused before anything runs.
func TestSSHVerifiesHostKey(t *testing.T) {
	t.Parallel()
	srv := sshtest.New(t)
	impostor := sshtest.New(t)
	known := filepath.Join(t.TempDir(), "known_hosts")
	// known_hosts names srv's address with the impostor's key.
	if err := os.WriteFile(known, []byte(knownhosts.Line([]string{knownhosts.Normalize(srv.Addr)}, impostor.HostKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := sshFleet(srv, "fleet-admin")
	f.Remote.SSH.KnownHostsFile = known
	r, err := New(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), staticLocalhost(t), fleet.Script{Name: "true", Content: "true\n"}, nil)
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		t.Errorf("Run = %v, want a host key mismatch", err)
	}
	if n := len(srv.Execs()); n != 0 {
		t.Errorf("%d commands ran on an unverified host", n)
	}
}

//= docs/requirements/06-fleet.md#remote-execution
//= type=test
//# The Fleet Controller SHALL stream each instance's command output
//# to its log prefixed with the instance id.

// TestSSMStreamsOutputPrefixedAsItArrives polls a command on a fake clock
// and checks each poll's new output reaches out, prefixed, before the
// command finishes.
func TestSSMStreamsOutputPrefixedAsItArrives(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := clock.NewFake(time.Unix(0, 0))
	ssm := awsfake.NewSSM()
	ssm.SetReply(ec2Inst.ID, awsfake.Reply{
		Progress: []string{"pulling kernel\npart", "pulling kernel\npartial line\n"},
		Stdout:   "pulling kernel\npartial line\ndone\n",
		Stderr:   "warning: slow mirror\n",
	})
	r := NewSSM(ssm, WithClock(clk), WithPollInterval(time.Second))
	out := &lockedBuffer{}
	type result struct {
		res *fleet.RunResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := r.Run(ctx, ec2Inst, fleet.Script{Name: "prepull", Content: "true\n"}, out)
		done <- result{res, err}
	}()

	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "[i-0abc] pulling kernel\n"; got != want {
		t.Errorf("after first poll out = %q, want %q (partial line held)", got, want)
	}
	clk.Advance(time.Second)
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "[i-0abc] pulling kernel\n[i-0abc] partial line\n"; got != want {
		t.Errorf("after second poll out = %q, want %q", got, want)
	}
	clk.Advance(time.Second)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	want := "[i-0abc] pulling kernel\n[i-0abc] partial line\n[i-0abc] done\n[i-0abc] warning: slow mirror\n"
	if out.String() != want {
		t.Errorf("out = %q, want %q", out.String(), want)
	}
	if got.res.Stdout != "pulling kernel\npartial line\ndone\n" {
		t.Errorf("result stdout = %q", got.res.Stdout)
	}
}

func TestSSHStreamsOutputPrefixed(t *testing.T) {
	t.Parallel()
	srv := sshtest.New(t)
	r, err := New(sshFleet(srv, "fleet-admin"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	_, err = r.Run(context.Background(), staticLocalhost(t), fleet.Script{Name: "s", Content: "echo out-1\necho err-1 >&2\nprintf 'no newline'\n"}, out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[lab-1] out-1\n", "[lab-1] err-1\n", "[lab-1] no newline\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("out = %q, lacks %q", out.String(), want)
		}
	}
}

func TestNonZeroExitReturnsResultAndExitError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ssm := awsfake.NewSSM()
	ssm.SetReply(ec2Inst.ID, awsfake.Reply{ExitCode: 3, Stderr: "thin pool device missing\n"})
	res, err := NewSSM(ssm).Run(ctx, ec2Inst, fleet.Script{Name: "thin_pool", Content: "exit 3\n"}, nil)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 || res == nil || res.ExitCode != 3 || !strings.Contains(err.Error(), "thin pool device missing") {
		t.Errorf("SSM: res %+v err %v", res, err)
	}
	ssm.SetReply(ec2Inst.ID, awsfake.Reply{Status: awsfake.StatusTimedOut, ExitCode: -1})
	if res, err := NewSSM(ssm).Run(ctx, ec2Inst, fleet.Script{Name: "slow", Content: "sleep 1\n"}, nil); err == nil || res != nil || errors.As(err, &exit) {
		t.Errorf("SSM timed out: res %+v err %v, want an error without a result", res, err)
	}

	srv := sshtest.New(t)
	r, err := New(sshFleet(srv, "fleet-admin"), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = r.Run(ctx, staticLocalhost(t), fleet.Script{Name: "fail", Content: "echo nope >&2\nexit 4\n"}, nil)
	if !errors.As(err, &exit) || exit.Code != 4 || res == nil || res.ExitCode != 4 || res.Stderr != "nope\n" {
		t.Errorf("SSH: res %+v err %v", res, err)
	}
}

func TestSSMRefusesStandardInput(t *testing.T) {
	t.Parallel()
	ssm := awsfake.NewSSM()
	_, err := NewSSM(ssm).Run(context.Background(), ec2Inst, fleet.Script{Name: "s", Content: "true\n", Stdin: strings.NewReader("token")}, nil)
	if !errors.Is(err, ErrStdinUnsupported) {
		t.Errorf("err = %v, want ErrStdinUnsupported", err)
	}
	if n := len(ssm.SentCommands()); n != 0 {
		t.Errorf("%d commands sent", n)
	}
	// An empty reader is no input.
	if _, err := NewSSM(ssm).Run(context.Background(), ec2Inst, fleet.Script{Name: "s", Content: "true\n", Stdin: strings.NewReader("")}, nil); err != nil {
		t.Errorf("empty stdin: %v", err)
	}
}

func TestSSHScriptTimeoutEndsTheRun(t *testing.T) {
	t.Parallel()
	srv := sshtest.New(t)
	r, err := New(sshFleet(srv, "fleet-admin"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), staticLocalhost(t), fleet.Script{Name: "hang", Content: "sleep 30\n", Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the script timeout", err)
	}
}

func TestLinePrefixer(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var mu sync.Mutex
	p := newLinePrefixer(&mu, &out, "i-1")
	for _, chunk := range []string{"a\nb", "c\n\nd"} {
		if _, err := p.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := "[i-1] a\n[i-1] bc\n[i-1] \n[i-1] d\n"; out.String() != want {
		t.Errorf("out = %q, want %q", out.String(), want)
	}
}
