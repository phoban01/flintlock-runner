package poolmgr_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// TestSlowHandshakeStillConnects is the regression test for the connection
// attempt deadline. With WithConnectParams grpc-go gives each attempt
// MinConnectTimeout or the current backoff, whichever is longer, and the
// client once set MinConnectTimeout to ReconnectMax. With the 50ms
// ReconnectMax the tests use, a handshake slower than 50ms, which a busy
// machine running the race detector produces now and then, was cut off
// ("error reading server preface: use of closed network connection") and
// retried with the same deadline, so the client could not connect at all.
// A proxy that holds every connection for longer than that stands in for
// the busy machine.
func TestSlowHandshakeStillConnects(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	slow := startSlowProxy(t, pm.addr, 300*time.Millisecond)

	c, err := poolmgr.NewClient(poolmgr.ClientConfig{
		Endpoint:      slow,
		TLS:           config.ClientTLS{Insecure: true},
		Deadline:      testTimeout,
		ReconnectBase: 10 * time.Millisecond,
		ReconnectMax:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()
	if _, err := c.ListPools(callCtx, testNamespace); err != nil {
		t.Fatalf("ListPools through a proxy that takes 300ms to connect: %v", err)
	}
}

// startSlowProxy forwards TCP connections to upstream, holding each one for
// delay before it dials upstream, so that the server's HTTP/2 preface
// reaches the client no sooner than delay after it connected.
func startSlowProxy(t *testing.T, upstream string, delay time.Duration) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("slow proxy: listen: %v", err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = lis.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			down, err := lis.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = down.Close() }()
				time.Sleep(delay)
				up, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, down); done <- struct{}{} }()
				go func() { _, _ = io.Copy(down, up); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return lis.Addr().String()
}
