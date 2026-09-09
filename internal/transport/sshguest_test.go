package transport_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The ssh Guest Transport is exercised against a guest of this package's
// own making: a real SSH server on the far end of the byte stream the Host's
// SSHProxy RPC hands out. The fake Host serves no MicroVMSSHProxy -- it
// advertises the service in ServerInfo and registers no server for it -- so
// the guest here stands in for the sshd inside a MicroVM, and the pipe
// between them stands in for the proxy. Nothing is listening on a port: the
// only way to reach this sshd is the stream, which is the point of EX-048.

// sshGuest is an SSH server that answers exec requests with a canned result
// and records the command lines it was asked to run.
type sshGuest struct {
	config *ssh.ServerConfig
	// hostKey is the guest's host key, for a Profile that pins one.
	hostKey ssh.Signer

	// stdout, stderr and status are what every exec request answers with.
	stdout string
	stderr string
	status uint32
	// silent makes the guest accept an exec request and then say nothing
	// more, which is what a Stage looks like from outside while it works.
	silent bool

	// done is closed when the test ends, releasing a silent session.
	done chan struct{}

	mu       sync.Mutex
	commands []string
	stdins   []string
	wg       sync.WaitGroup
}

// newSSHGuest builds a guest that accepts the given public key and answers
// exec requests with stdout, stderr and status.
func newSSHGuest(t *testing.T, authorized ssh.PublicKey, stdout, stderr string, status uint32) *sshGuest {
	t.Helper()
	hostKey := generateSigner(t)
	guest := &sshGuest{hostKey: hostKey, stdout: stdout, stderr: stderr, status: status}
	guest.config = &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized == nil {
				return nil, errUnauthorizedKey
			}
			if string(key.Marshal()) != string(authorized.Marshal()) {
				return nil, errUnauthorizedKey
			}
			return &ssh.Permissions{}, nil
		},
	}
	guest.config.AddHostKey(hostKey)
	guest.done = make(chan struct{})
	t.Cleanup(guest.wg.Wait)
	// Registered after the wait, so that it runs before it and releases a
	// silent session that is still holding its channel open.
	t.Cleanup(func() { close(guest.done) })
	return guest
}

// errUnauthorizedKey is what the guest tells a client presenting the wrong
// key.
var errUnauthorizedKey = errUnauthorized{}

// errUnauthorized is the guest's rejection of a key.
type errUnauthorized struct{}

// Error implements error.
func (errUnauthorized) Error() string { return "ssh: unauthorized key" }

// dialProxy returns the Runner-side end of a proxied session and starts the
// guest on the other end, which is what a Host's SSHProxy RPC does.
func (g *sshGuest) dialProxy() io.ReadWriteCloser {
	runner, guest := duplexPipe()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.serve(guest)
	}()
	return runner
}

// serve runs one SSH connection to completion.
func (g *sshGuest) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	serverConn, chans, reqs, err := ssh.NewServerConn(conn, g.config)
	if err != nil {
		return
	}
	defer func() { _ = serverConn.Close() }()
	go ssh.DiscardRequests(reqs)
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			g.session(channel, requests)
		}()
	}
}

// session answers the requests of one session channel.
func (g *sshGuest) session(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()
	for req := range requests {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)

		if g.silent {
			g.mu.Lock()
			g.commands = append(g.commands, payload.Command)
			g.mu.Unlock()
			// Say nothing at all until the test is over: no output, no exit
			// status, and the channel kept open, which is what a Stage that
			// is quietly working looks like from the Runner's side.
			_, _ = io.Copy(io.Discard, channel)
			<-g.done
			return
		}

		var stdin []byte
		if b, err := io.ReadAll(channel); err == nil {
			stdin = b
		}
		g.mu.Lock()
		g.commands = append(g.commands, payload.Command)
		g.stdins = append(g.stdins, string(stdin))
		g.mu.Unlock()

		if g.stdout != "" {
			_, _ = channel.Write([]byte(g.stdout))
		}
		if g.stderr != "" {
			_, _ = channel.Stderr().Write([]byte(g.stderr))
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{g.status}))
		return
	}
}

// ran returns the command lines the guest was asked to run.
func (g *sshGuest) ran() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.commands...)
}

// stdinSeen returns the standard input the guest received, per command.
func (g *sshGuest) stdinSeen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.stdins...)
}

// duplexPipe is a buffered, in-memory connection between two ends. It is
// not net.Pipe, which has no buffer at all and deadlocks on an SSH
// handshake, where both sides write their version string before either
// reads. Each end is a net.Conn so that the SSH server can take it.
func duplexPipe() (a, b net.Conn) {
	toB, toA := newPipeBuffer(), newPipeBuffer()
	return &pipeEnd{r: toA, w: toB}, &pipeEnd{r: toB, w: toA}
}

// pipeEnd is one end of a duplexPipe.
type pipeEnd struct {
	r *pipeBuffer
	w *pipeBuffer
}

// Read implements net.Conn.
func (p *pipeEnd) Read(b []byte) (int, error) { return p.r.Read(b) }

// Write implements net.Conn.
func (p *pipeEnd) Write(b []byte) (int, error) { return p.w.Write(b) }

// Close implements net.Conn: both directions end, so a peer blocked on a
// read sees the connection go away.
func (p *pipeEnd) Close() error {
	p.w.close()
	p.r.close()
	return nil
}

// LocalAddr implements net.Conn.
func (p *pipeEnd) LocalAddr() net.Addr { return pipeAddr{} }

// RemoteAddr implements net.Conn.
func (p *pipeEnd) RemoteAddr() net.Addr { return pipeAddr{} }

// SetDeadline implements net.Conn.
func (p *pipeEnd) SetDeadline(time.Time) error { return nil }

// SetReadDeadline implements net.Conn.
func (p *pipeEnd) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline implements net.Conn.
func (p *pipeEnd) SetWriteDeadline(time.Time) error { return nil }

// pipeAddr is the placeholder address of a pipe end.
type pipeAddr struct{}

// Network implements net.Addr.
func (pipeAddr) Network() string { return "pipe" }

// String implements net.Addr.
func (pipeAddr) String() string { return "pipe" }

// pipeBuffer is one direction of a duplexPipe: writes never block and reads
// wait for bytes or for the direction to be closed.
type pipeBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

// newPipeBuffer builds an open direction.
func newPipeBuffer() *pipeBuffer {
	p := &pipeBuffer{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Write implements io.Writer.
func (p *pipeBuffer) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := p.buf.Write(b)
	p.cond.Broadcast()
	return n, err
}

// Read implements io.Reader.
func (p *pipeBuffer) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buf.Len() == 0 {
		if p.closed {
			return 0, io.EOF
		}
		p.cond.Wait()
	}
	return p.buf.Read(b)
}

// close ends the direction, waking every reader.
func (p *pipeBuffer) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
}

// generateSigner makes a throwaway ed25519 key.
func generateSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("building a signer: %v", err)
	}
	return signer
}

// generateKeyPair makes a throwaway key and returns it as PEM together with
// its public half, the way a Profile carries a private key file and a Host
// carries an authorized key.
func generateKeyPair(t *testing.T) (privatePEM []byte, public ssh.PublicKey) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling the key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("building a signer: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), signer.PublicKey()
}
