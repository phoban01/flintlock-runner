package flintlock_test

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// These two tests are the only ones that put the Guest Transport on top of a
// real gRPC connection to a fake Host. They live here rather than with the
// transport's own tests because dialling a Host is this package's job; what
// they check is that the exec transport really speaks the MicroVMExec RPC
// and that a Job's Stages travel over one connection.

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The `exec` Guest Transport SHALL use the flintlock
//# `MicroVMExec.ExecCommand` streaming RPC of the Host that runs the MicroVM.

// TestExecTransportRunsOverTheExecCommandRPC runs a Stage in a MicroVM on a
// fake Host reached over gRPC. The only path from this process into that
// MicroVM is the MicroVMExec service the Host registered, so a Stage whose
// output comes back has necessarily travelled over ExecCommand.
func TestExecTransportRunsOverTheExecCommandRPC(t *testing.T) {
	t.Parallel()
	host := startHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	uid := createVM(t, host)
	client := dial(t, host)

	tr, err := transport.NewFactory().New(context.Background(), transport.Target{
		Kind:  transport.KindExec,
		Host:  client,
		VMUID: uid,
	})
	if err != nil {
		t.Fatalf("building the exec transport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var stdout bytes.Buffer
	status, err := tr.Run(ctx, transport.Command{
		Path:   "sh",
		Stdin:  strings.NewReader("echo over-the-rpc\n"),
		Stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != 0 || !strings.Contains(stdout.String(), "over-the-rpc") {
		t.Errorf("Run returned (%d, %q), want (0, over-the-rpc)", status, stdout.String())
	}
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The Guest Transport SHALL reuse the Host client's gRPC
//# connection rather than opening a new connection per Stage.

// TestStagesShareOneConnectionToTheHost counts the connections a Job's
// Stages cost, at the only place where the answer is not a matter of
// opinion: the TCP relay in front of the Host. Several Stages run through
// one transport built on one Host client, and exactly one connection is
// made, so the Stages travelled as streams on the connection the Host client
// already had.
func TestStagesShareOneConnectionToTheHost(t *testing.T) {
	t.Parallel()
	host := startHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	uid := createVM(t, host)
	relay := startCountingRelay(t, host.Addr())

	client := dialEndpoint(t, flintlock.Endpoint{
		Name:    "h1",
		Address: relay.addr,
		TLS:     flintlock.TLSOptions{Insecure: true},
	})
	tr, err := transport.NewFactory().New(context.Background(), transport.Target{
		Kind:  transport.KindExec,
		Host:  client,
		VMUID: uid,
	})
	if err != nil {
		t.Fatalf("building the exec transport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	for i := 0; i < 4; i++ {
		var stdout bytes.Buffer
		status, err := tr.Run(ctx, transport.Command{
			Path:   "echo",
			Args:   []string{"stage"},
			Stdout: &stdout,
		})
		if err != nil || status != 0 {
			t.Fatalf("stage %d returned (%d, %v)", i, status, err)
		}
	}
	if got := relay.connections(); got != 1 {
		t.Errorf("four stages cost %d connections to the host, want one", got)
	}
}

// countingRelay is a TCP relay that counts the connections made through it.
type countingRelay struct {
	addr string

	mu    sync.Mutex
	count int
}

// connections returns how many connections have been made.
func (r *countingRelay) connections() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// startCountingRelay listens on a loopback port and relays every connection
// to target until the test ends.
func startCountingRelay(t *testing.T, target string) *countingRelay {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	relay := &countingRelay{addr: lis.Addr().String()}
	var wg sync.WaitGroup
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			client, err := lis.Accept()
			if err != nil {
				return
			}
			relay.mu.Lock()
			relay.count++
			relay.mu.Unlock()
			upstream, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			// Either direction ending closes both connections, so that the
			// goroutine copying the other way is never left reading from a
			// peer that has nothing more to say.
			closeBoth := func() {
				_ = client.Close()
				_ = upstream.Close()
			}
			wg.Add(2)
			go func() {
				defer wg.Done()
				defer closeBoth()
				copyStream(client, upstream)
			}()
			go func() {
				defer wg.Done()
				defer closeBoth()
				copyStream(upstream, client)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		select {
		case <-accepting:
		case <-time.After(testTimeout):
			t.Error("the relay did not stop accepting")
		}
		wg.Wait()
	})
	return relay
}

// copyStream forwards bytes one way and stops on the first error.
func copyStream(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
