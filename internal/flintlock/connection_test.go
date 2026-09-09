package flintlock_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# The Runner SHALL maintain one long-lived gRPC connection per
//# Host, with keepalive enabled, and SHALL reconnect with exponential backoff
//# when a connection is lost.

// TestConnectionOutlivesTheHostAndReconnects checks the long-lived half of
// the requirement: one Client is dialled once, the Host it is talking to
// goes away and comes back, and the same Client starts working again
// without anybody dialling a second connection. The Host that comes back
// reports a different version, so the answer proves the call reached the
// new process rather than a cached one.
func TestConnectionOutlivesTheHostAndReconnects(t *testing.T) {
	t.Parallel()
	addr := freeAddr(t)

	host := fake.New(flintlock.FakeHostConfig{Name: "h1", Listen: addr, Version: "v1", SandboxRoot: t.TempDir()})
	t.Cleanup(func() { _ = host.Close() })
	stopFirst := serve(t, host)
	client := dial(t, host, flintlock.WithCallDeadline(time.Second),
		flintlock.WithReconnectBackoff(50*time.Millisecond, 200*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	info, err := client.ServerInfo(ctx)
	if err != nil {
		t.Fatalf("ServerInfo against the first host: %v", err)
	}
	if info.Version != "v1" {
		t.Fatalf("ServerInfo reported version %q, want v1", info.Version)
	}

	// The Host goes away. Its listener is closed, so calls fail.
	stopFirst()
	if _, err := client.ServerInfo(ctx); err == nil {
		t.Fatal("ServerInfo succeeded against a host that has stopped")
	}

	// A new process comes up at the same address.
	second := flintlock.FakeHostConfig{Name: "h1", Listen: addr, Version: "v2", SandboxRoot: t.TempDir()}
	replacement := startHost(t, second)
	_ = replacement

	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := client.ServerInfo(ctx)
		if err == nil {
			if info.Version != "v2" {
				t.Fatalf("after reconnecting, ServerInfo reported version %q, want v2", info.Version)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the client never reconnected to the restarted host: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# The Runner SHALL maintain one long-lived gRPC connection per
//# Host, with keepalive enabled, and SHALL reconnect with exponential backoff
//# when a connection is lost.

// TestReconnectionBackoffIsExponential watches a Host that refuses every
// connection and times the attempts the client makes. Each gap has to be
// longer than the one before it, which is what exponential backoff means
// and what keeps a fleet of unreachable Hosts from being hammered.
func TestReconnectionBackoffIsExponential(t *testing.T) {
	t.Parallel()
	attempts := make(chan time.Time, 16)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			select {
			case attempts <- time.Now():
			default:
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		<-done
	})

	client := dialEndpoint(t, flintlock.Endpoint{
		Name:    "h1",
		Address: lis.Addr().String(),
		TLS:     flintlock.TLSOptions{Insecure: true},
	}, flintlock.WithCallDeadline(100*time.Millisecond),
		flintlock.WithReconnectBackoff(100*time.Millisecond, 5*time.Second))

	// The first call starts the connection; it and everything after it fails.
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			_, _ = client.ServerInfo(ctx)
		}
	}()

	const want = 4
	seen := make([]time.Time, 0, want)
	for len(seen) < want {
		select {
		case at := <-attempts:
			seen = append(seen, at)
		case <-time.After(testTimeout):
			t.Fatalf("saw only %d connection attempts, want %d", len(seen), want)
		}
	}
	var previous time.Duration
	for i := 1; i < len(seen); i++ {
		gap := seen[i].Sub(seen[i-1])
		if i > 1 && gap <= previous {
			t.Errorf("connection attempt %d came %s after the one before, which is not longer than the previous gap of %s",
				i, gap, previous)
		}
		previous = gap
	}
}

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# The Runner SHALL maintain one long-lived gRPC connection per
//# Host, with keepalive enabled, and SHALL reconnect with exponential backoff
//# when a connection is lost.

// TestConnectionSendsKeepalivePings checks the keepalive half by counting
// the HTTP/2 PING frames the client sends through a relay in front of a
// Host it is not otherwise talking to. A connection without keepalive sends
// none, and this one has no stream open, so the pings also prove that the
// Runner pings without a stream, which is what detects a Host that has gone
// away while no Job is running on it.
//
// gRPC clamps a client's ping interval to ten seconds, so this test is
// necessarily slow; there is no faster way to see a ping on the wire.
func TestConnectionSendsKeepalivePings(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("keepalive pings are clamped to a ten second interval by gRPC")
	}
	host := startHost(t, flintlock.FakeHostConfig{Name: "h1", Version: "v1"})
	relay := startPingRelay(t, host.Addr())

	client := dialEndpoint(t, flintlock.Endpoint{
		Name:    "h1",
		Address: relay.addr,
		TLS:     flintlock.TLSOptions{Insecure: true},
	}, flintlock.WithKeepalive(time.Second, 5*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if _, err := client.ServerInfo(ctx); err != nil {
		t.Fatalf("ServerInfo through the relay: %v", err)
	}

	// gRPC also pings to size its flow-control window while data is
	// travelling, so the pings that count are the ones sent after the
	// connection has gone quiet.
	relay.reset()
	deadline := time.Now().Add(25 * time.Second)
	for {
		if relay.pings() > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle connection sent no keepalive ping")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// isKeepalivePing reports whether a PING frame's payload is the eight zero
// bytes gRPC sends for keepalive.
func isKeepalivePing(payload []byte) bool {
	if len(payload) != 8 {
		return false
	}
	for _, b := range payload {
		if b != 0 {
			return false
		}
	}
	return true
}

// freeAddr returns a loopback address nothing is listening on, for a Host
// that has to be restarted at the same address.
func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("closing the probe listener: %v", err)
	}
	return addr
}

// pingRelay is a TCP relay that forwards a connection to a Host and counts
// the HTTP/2 PING frames travelling towards it.
type pingRelay struct {
	addr string

	mu    sync.Mutex
	count int
}

// pings returns how many PING frames the client has sent.
func (r *pingRelay) pings() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// reset forgets the pings seen so far.
func (r *pingRelay) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count = 0
}

// countPing records one PING frame.
func (r *pingRelay) countPing() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
}

// startPingRelay listens on a loopback port and relays every connection to
// target until the test ends.
func startPingRelay(t *testing.T, target string) *pingRelay {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	relay := &pingRelay{addr: lis.Addr().String()}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		for {
			client, err := lis.Accept()
			if err != nil {
				return
			}
			upstream, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			wg.Add(2)
			go func() {
				defer wg.Done()
				defer func() { _ = upstream.Close() }()
				relay.copyCountingPings(client, upstream)
			}()
			go func() {
				defer wg.Done()
				defer func() { _ = client.Close() }()
				_, _ = io.Copy(client, upstream)
			}()
		}
	}()
	go func() {
		<-done
		_ = lis.Close()
	}()
	t.Cleanup(func() {
		close(done)
		wg.Wait()
	})
	return relay
}

// copyCountingPings forwards the client's bytes to the Host, parsing the
// HTTP/2 framing on the way past so that PING frames can be counted. The
// connection preface is 24 bytes and every frame after it starts with a
// nine byte header: three bytes of length, one of type, one of flags and
// four of stream identifier.
func (r *pingRelay) copyCountingPings(client, upstream net.Conn) {
	const (
		prefaceLen  = 24
		headerLen   = 9
		pingType    = 0x06
		ackFlag     = 0x01
		maxFrameLen = 1 << 20
	)
	preface := make([]byte, prefaceLen)
	if _, err := io.ReadFull(client, preface); err != nil {
		return
	}
	if _, err := upstream.Write(preface); err != nil {
		return
	}
	header := make([]byte, headerLen)
	for {
		if _, err := io.ReadFull(client, header); err != nil {
			return
		}
		length := int(binary.BigEndian.Uint32(append([]byte{0}, header[0:3]...)))
		if length > maxFrameLen {
			return
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(client, payload); err != nil {
			return
		}
		// gRPC sends two kinds of ping: a keepalive ping, whose payload is
		// eight zero bytes, and a flow-control probe carrying a fixed
		// pattern while data is in flight. Only the first kind is what
		// keepalive means.
		if header[3] == pingType && header[4]&ackFlag == 0 && isKeepalivePing(payload) {
			r.countPing()
		}
		if _, err := upstream.Write(header); err != nil {
			return
		}
		if _, err := upstream.Write(payload); err != nil {
			return
		}
	}
}
