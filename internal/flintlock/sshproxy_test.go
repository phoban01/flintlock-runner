package flintlock_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	sshv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmsshproxy/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// The fake Host serves no MicroVMSSHProxy: it advertises the service in
// ServerInfo but registers no server for it, and its in-memory client
// answers ErrUnimplemented (see fake.Client.SSHProxy). These tests therefore
// bring their own MicroVMSSHProxy server, which is a test double of this
// package rather than an extension of the fake Host: it is the smallest
// server that lets the Runner-side client be driven over a real gRPC
// stream, and it upholds flintlockd's two rules -- the first message has to
// be the uid, and everything after it is opaque bytes in both directions.

// sshProxyServer is a MicroVMSSHProxy server that hands each accepted
// session to a handler as a byte stream.
type sshProxyServer struct {
	sshv1.UnimplementedMicroVMSSHProxyServer

	// handle is called with the session's byte stream once the uid message
	// has arrived. It returns when the session is over.
	handle func(uid string, rw io.ReadWriteCloser)

	mu   sync.Mutex
	uids []string
}

// SSHProxy implements the generated server: it requires the uid first, as
// flintlockd does, then relays the session to the handler.
func (s *sshProxyServer) SSHProxy(stream grpc.BidiStreamingServer[sshv1.SSHProxyRequest, sshv1.SSHProxyResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	uid := first.GetUid()
	if uid == "" {
		return status.Error(codes.InvalidArgument, "first message must carry the microvm uid")
	}
	s.mu.Lock()
	s.uids = append(s.uids, uid)
	s.mu.Unlock()
	if s.handle == nil {
		return nil
	}
	rw := &serverSession{stream: stream}
	s.handle(uid, rw)
	return nil
}

// seenUIDs returns the uids the server was asked for.
func (s *sshProxyServer) seenUIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.uids...)
}

// serverSession is the server side of a proxied session as a byte stream.
type serverSession struct {
	stream  grpc.BidiStreamingServer[sshv1.SSHProxyRequest, sshv1.SSHProxyResponse]
	pending []byte
}

// Read implements io.Reader.
func (s *serverSession) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		req, err := s.stream.Recv()
		if err != nil {
			return 0, err
		}
		s.pending = req.GetData()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// Write implements io.Writer.
func (s *serverSession) Write(p []byte) (int, error) {
	if err := s.stream.Send(&sshv1.SSHProxyResponse{Data: append([]byte(nil), p...)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close implements io.Closer; the stream ends when the handler returns.
func (s *serverSession) Close() error { return nil }

// startSSHProxy serves a MicroVMSSHProxy on a loopback port until the test
// ends and returns the server and its address.
func startSSHProxy(t *testing.T, handle func(uid string, rw io.ReadWriteCloser)) (*sshProxyServer, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	proxy := &sshProxyServer{handle: handle}
	sshv1.RegisterMicroVMSSHProxyServer(srv, proxy)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Error("ssh proxy server did not stop")
		}
	})
	return proxy, lis.Addr().String()
}

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The `ssh` Guest Transport SHALL connect to the guest's SSH
//# service through the flintlock `MicroVMSSHProxy.SSHProxy` streaming RPC of
//# the Host that runs the MicroVM.

// TestSSHProxyStreamsBytesToTheGuest drives the Host client's half of the
// SSHProxy RPC: the uid the Runner asked for reaches the Host as the first
// message of the exchange, and everything after it is a byte stream in both
// directions, which is what an SSH client needs of it.
func TestSSHProxyStreamsBytesToTheGuest(t *testing.T) {
	t.Parallel()
	proxy, addr := startSSHProxy(t, func(_ string, rw io.ReadWriteCloser) {
		// Echo in upper case so that the test can tell the directions apart.
		buf := make([]byte, 64)
		for {
			n, err := rw.Read(buf)
			if n > 0 {
				if _, werr := rw.Write([]byte(strings.ToUpper(string(buf[:n])))); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})

	client := dialEndpoint(t, flintlock.Endpoint{
		Name:    "h1",
		Address: addr,
		TLS:     flintlock.TLSOptions{Insecure: true},
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := client.SSHProxy(ctx, "vm-uid")
	if err != nil {
		t.Fatalf("SSHProxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("writing to the proxy: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading from the proxy: %v", err)
	}
	if got := string(buf); got != "HELLO" {
		t.Errorf("read %q through the proxy, want %q", got, "HELLO")
	}
	if uids := proxy.seenUIDs(); len(uids) != 1 || uids[0] != "vm-uid" {
		t.Errorf("the proxy saw uids %v, want exactly [vm-uid] as the first message", uids)
	}
}

// TestSSHProxyReportsTheHostsRefusal checks that a Host which refuses the
// session, as one started without the ssh proxy service does, is reported
// as ErrUnimplemented. The refusal may arrive while the stream is being
// opened or on the first read from it, because gRPC accepts the uid message
// into its send buffer before the server has had a say; either way the
// caller sees the Host's reason and not a stream that simply stops.
func TestSSHProxyReportsTheHostsRefusal(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// A gRPC server with no MicroVMSSHProxy registered is a flintlockd
	// started without ssh proxying.
	srv := grpc.NewServer()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		<-done
	})

	client := dialEndpoint(t, flintlock.Endpoint{
		Name:    "h1",
		Address: lis.Addr().String(),
		TLS:     flintlock.TLSOptions{Insecure: true},
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := client.SSHProxy(ctx, "vm-uid")
	if err == nil {
		defer func() { _ = conn.Close() }()
		_, err = conn.Read(make([]byte, 1))
	}
	if !errors.Is(err, flintlock.ErrUnimplemented) {
		t.Fatalf("SSHProxy on a host without the service returned %v, want ErrUnimplemented", err)
	}
}
