package transport_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"golang.org/x/crypto/ssh"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// sshTarget wires a guest to a Target: the Host hands out the proxied
// stream, as flintlockd's MicroVMSSHProxy does, and the Profile's key and
// known host key come from the caller.
func sshTarget(guest *sshGuest, key []byte, knownHostKey string) (transport.Target, *stubHost) {
	host := &stubHost{
		name: "h1",
		sshProxy: func(_ context.Context, _ string) (io.ReadWriteCloser, error) {
			return guest.dialProxy(), nil
		},
	}
	return transport.Target{
		Kind:  transport.KindSSH,
		Host:  host,
		VMUID: "vm-uid",
		SSH: transport.SSHOptions{
			User:         "runner",
			PrivateKey:   key,
			KnownHostKey: knownHostKey,
		},
	}, host
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The `ssh` Guest Transport SHALL connect to the guest's SSH
//# service through the flintlock `MicroVMSSHProxy.SSHProxy` streaming RPC of
//# the Host that runs the MicroVM.

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The `ssh` Guest Transport SHALL NOT offer a direct TCP mode,
//# because pool MicroVMs are created from one shared template and flintlock
//# reports no guest address the Runner could connect to.

// TestSSHReachesTheGuestOnlyThroughTheProxy runs a Stage over ssh against a
// guest that is not listening on any address: the only way to it is the
// stream the Host's SSHProxy RPC returns, and the Target the transport is
// built from carries no address it could dial instead. The command, its
// working directory, its environment and its standard input all arrive, the
// output comes back and the exit status is the guest's.
//
// The second half is the case that would expose a TCP mode if one existed:
// a Host that refuses the proxy stream. The operation has to fail with the
// Host's reason and the transport must not have looked for the guest
// anywhere else.
func TestSSHReachesTheGuestOnlyThroughTheProxy(t *testing.T) {
	t.Parallel()
	key, public := generateKeyPair(t)
	guest := newSSHGuest(t, public, "hello from the guest\n", "and its stderr\n", 3)
	target, host := sshTarget(guest, key, "")

	tr, err := transport.NewFactory().New(context.Background(), target)
	if err != nil {
		t.Fatalf("building the ssh transport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	status, err := tr.Run(ctx, transport.Command{
		Path:   "/bin/bash",
		Args:   []string{"--login"},
		Dir:    "/builds/project",
		Env:    map[string]string{"CI_JOB_STAGE": "build"},
		Stdin:  strings.NewReader("echo hi\n"),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != 3 {
		t.Errorf("Run returned exit status %d, want the guest's 3", status)
	}
	if stdout.String() != "hello from the guest\n" || stderr.String() != "and its stderr\n" {
		t.Errorf("Run wrote (%q, %q), want the guest's output on each stream", stdout.String(), stderr.String())
	}
	ran := guest.ran()
	if len(ran) != 1 {
		t.Fatalf("the guest ran %d commands, want one", len(ran))
	}
	for _, want := range []string{"cd '/builds/project'", "export CI_JOB_STAGE='build'", "exec '/bin/bash' '--login'"} {
		if !strings.Contains(ran[0], want) {
			t.Errorf("the guest was asked to run %q, which does not contain %q", ran[0], want)
		}
	}
	if got := guest.stdinSeen(); len(got) != 1 || got[0] != "echo hi\n" {
		t.Errorf("the guest received standard input %q, want the script", got)
	}
	if _, proxies, _ := host.counts(); proxies != 1 {
		t.Errorf("the transport opened %d proxy streams, want one", proxies)
	}

	t.Run("a host that refuses the proxy", func(t *testing.T) {
		refusing := &stubHost{
			name: "h1",
			sshProxy: func(context.Context, string) (io.ReadWriteCloser, error) {
				return nil, flintlock.ErrUnimplemented
			},
		}
		target := target
		target.Host = refusing
		tr, err := transport.NewFactory().New(context.Background(), target)
		if err != nil {
			t.Fatalf("building the ssh transport: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })
		if _, err := tr.Run(ctx, transport.Command{Path: "true"}); !errors.Is(err, flintlock.ErrUnimplemented) {
			t.Fatalf("Run against a host without the proxy returned %v, want the host's refusal", err)
		}
		if _, proxies, _ := refusing.counts(); proxies != 1 {
			t.Errorf("the transport made %d proxy attempts, want exactly one and no fallback", proxies)
		}
	})
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The `ssh` Guest Transport SHALL authenticate with the private
//# key named by the Profile and SHALL verify the guest host key only where the
//# Profile provides a known host key.

// TestSSHAuthenticationAndHostKeyVerification covers both halves of the
// requirement. A guest that does not know the Profile's key turns the
// connection away, so the key really is what authenticates. A Profile that
// names the guest's host key connects; one that names a different key
// refuses to; and one that names none connects anyway, because a pooled
// MicroVM generates its host key on first boot and there is nothing to
// verify it against.
func TestSSHAuthenticationAndHostKeyVerification(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	key, public := generateKeyPair(t)
	otherKey, _ := generateKeyPair(t)
	_, strangerPublic := generateKeyPair(t)

	run := func(t *testing.T, guest *sshGuest, key []byte, knownHostKey string) error {
		t.Helper()
		target, _ := sshTarget(guest, key, knownHostKey)
		tr, err := transport.NewFactory().New(context.Background(), target)
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = tr.Close() })
		_, err = tr.Run(ctx, transport.Command{Path: "true"})
		return err
	}

	t.Run("the profile's key authenticates", func(t *testing.T) {
		guest := newSSHGuest(t, public, "", "", 0)
		if err := run(t, guest, key, ""); err != nil {
			t.Fatalf("Run with the profile's key: %v", err)
		}
	})

	t.Run("another key is refused by the guest", func(t *testing.T) {
		guest := newSSHGuest(t, public, "", "", 0)
		err := run(t, guest, otherKey, "")
		if err == nil {
			t.Fatal("Run succeeded with a key the guest does not accept")
		}
		if !errors.Is(err, transport.ErrStreamFailed) {
			t.Errorf("Run returned %v, want a failure wrapping ErrStreamFailed", err)
		}
	})

	t.Run("a matching known host key", func(t *testing.T) {
		guest := newSSHGuest(t, public, "", "", 0)
		known := string(ssh.MarshalAuthorizedKey(guest.hostKey.PublicKey()))
		if err := run(t, guest, key, known); err != nil {
			t.Fatalf("Run with the guest's host key pinned: %v", err)
		}
	})

	t.Run("a host key that does not match", func(t *testing.T) {
		guest := newSSHGuest(t, public, "", "", 0)
		known := string(ssh.MarshalAuthorizedKey(strangerPublic))
		err := run(t, guest, key, known)
		if err == nil {
			t.Fatal("Run succeeded against a guest whose host key is not the pinned one")
		}
		if !strings.Contains(err.Error(), "host key") && !strings.Contains(err.Error(), "ssh:") {
			t.Errorf("Run failed with %v, want the host key mismatch", err)
		}
	})

	t.Run("no known host key at all", func(t *testing.T) {
		guest := newSSHGuest(t, public, "", "", 0)
		if err := run(t, guest, key, ""); err != nil {
			t.Fatalf("Run against a profile that pins no host key: %v", err)
		}
	})

	t.Run("a profile with no key at all is refused", func(t *testing.T) {
		guest := newSSHGuest(t, public, "", "", 0)
		target, _ := sshTarget(guest, nil, "")
		if _, err := transport.NewFactory().New(context.Background(), target); err == nil {
			t.Fatal("the ssh transport was built without the profile's private key")
		}
	})
}

// TestSSHReadyRunsATrivialCommand checks the readiness operation over ssh:
// it succeeds when the guest runs the command and refuses when the guest
// answers with a failure.
func TestSSHReadyRunsATrivialCommand(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	key, public := generateKeyPair(t)

	t.Run("a guest that answers", func(t *testing.T) {
		guest := newSSHGuest(t, public, "", "", 0)
		target, _ := sshTarget(guest, key, "")
		tr, err := transport.NewFactory().New(context.Background(), target)
		if err != nil {
			t.Fatalf("building the ssh transport: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })
		if err := tr.Ready(ctx); err != nil {
			t.Fatalf("Ready: %v", err)
		}
	})

	t.Run("a guest whose command fails", func(t *testing.T) {
		guest := newSSHGuest(t, public, "", "", 1)
		target, _ := sshTarget(guest, key, "")
		tr, err := transport.NewFactory().New(context.Background(), target)
		if err != nil {
			t.Fatalf("building the ssh transport: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })
		if err := tr.Ready(ctx); !errors.Is(err, transport.ErrNotReady) {
			t.Fatalf("Ready returned %v, want ErrNotReady", err)
		}
	})
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# If the Host that runs the MicroVM becomes unreachable, then the
//# Guest Transport SHALL fail the in-flight operation within the configured
//# transport deadline.

// TestSSHRunFailsWhenTheHostStopsAnswering runs a Stage against a guest that
// accepts the command and then says nothing, on a Host that has stopped
// answering. The ssh session itself cannot tell the two apart -- a working
// Stage is silent too -- so the transport asks the Host about the MicroVM,
// and when the Host does not answer that either the operation ends within
// the configured deadline instead of waiting on a guest that will never
// reply.
func TestSSHRunFailsWhenTheHostStopsAnswering(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	key, public := generateKeyPair(t)
	guest := newSSHGuest(t, public, "", "", 0)
	guest.silent = true

	target, host := sshTarget(guest, key, "")
	const deadline = 400 * time.Millisecond
	target.Deadline = deadline
	host.getVM = func(ctx context.Context, _ string) (*types.MicroVM, error) {
		// The Host has stopped answering: the call runs out of time.
		<-ctx.Done()
		return nil, errors.Join(flintlock.ErrUnavailable, ctx.Err())
	}

	tr, err := transport.NewFactory().New(context.Background(), target)
	if err != nil {
		t.Fatalf("building the ssh transport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	begin := time.Now()
	status, err := tr.Run(ctx, transport.Command{Path: "true"})
	took := time.Since(begin)
	if !errors.Is(err, transport.ErrStreamFailed) {
		t.Fatalf("Run returned (%d, %v), want a failure wrapping ErrStreamFailed", status, err)
	}
	if !strings.Contains(err.Error(), "h1") {
		t.Errorf("the failure %q does not name the host", err)
	}
	if took > 10*deadline {
		t.Errorf("Run took %s to give up on an unreachable host, want about %s", took, deadline)
	}
	if _, _, probes := host.counts(); probes == 0 {
		t.Error("the transport never asked the host about the microvm; it cannot know a silent stage from a dead host")
	}
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The Guest Transport SHALL reuse the Host client's gRPC
//# connection rather than opening a new connection per Stage.

// TestSSHReusesOneProxyStreamForEveryStage runs several Stages through one
// ssh transport. The SSH connection, and with it the proxy stream on the
// Host client's connection, is established once and reused, so a Job's
// Stages cost sessions rather than connections.
func TestSSHReusesOneProxyStreamForEveryStage(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	key, public := generateKeyPair(t)
	guest := newSSHGuest(t, public, "out\n", "", 0)
	target, host := sshTarget(guest, key, "")

	tr, err := transport.NewFactory().New(context.Background(), target)
	if err != nil {
		t.Fatalf("building the ssh transport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	for i := 0; i < 3; i++ {
		if _, err := tr.Run(ctx, transport.Command{Path: "true"}); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
	}
	if _, proxies, _ := host.counts(); proxies != 1 {
		t.Errorf("three stages opened %d proxy streams, want one", proxies)
	}
	if got := len(guest.ran()); got != 3 {
		t.Errorf("the guest ran %d commands, want 3", got)
	}
}
