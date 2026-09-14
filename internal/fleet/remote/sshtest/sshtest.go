// Package sshtest is an in-process SSH server on localhost for testing the
// Fleet Controller's SSH Remote without a real machine. It accepts one
// authorized public key, runs each exec request with the local bash, and
// records every command it ran. It is test support; nothing in the Runner or
// the Fleet Controller imports it.
package sshtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Exec is one command the server ran.
type Exec struct {
	User    string
	Command string
}

// Server is a running SSH server. Close it when done; New registers Close
// with the test's cleanup.
type Server struct {
	// Addr is the listening address, 127.0.0.1:port.
	Addr string
	// HostKey is the server's public host key.
	HostKey ssh.PublicKey
	// ClientKeyFile is a private key file the server accepts, and
	// KnownHostsFile a known_hosts file naming the server's host key.
	ClientKeyFile  string
	KnownHostsFile string

	ln     net.Listener
	config *ssh.ServerConfig
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	execs []Exec
	conns map[net.Conn]struct{}
}

// New starts a server for tb that accepts the key in ClientKeyFile for any
// user.
func New(tb testing.TB) *Server {
	tb.Helper()
	dir := tb.TempDir()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		tb.Fatal(err)
	}
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		tb.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		tb.Fatal(err)
	}
	keyFile := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		tb.Fatal(err)
	}
	authorized := clientSigner.PublicKey().Marshal()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	s := &Server{
		Addr:          ln.Addr().String(),
		HostKey:       hostSigner.PublicKey(),
		ClientKeyFile: keyFile,
		ln:            ln,
		conns:         map[net.Conn]struct{}{},
	}
	s.KnownHostsFile = filepath.Join(dir, "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, s.HostKey) + "\n"
	if err := os.WriteFile(s.KnownHostsFile, []byte(line), 0o600); err != nil {
		tb.Fatal(err)
	}
	s.config = &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized) {
				return nil, nil
			}
			return nil, errors.New("sshtest: key not authorized")
		},
	}
	s.config.AddHostKey(hostSigner)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Add(1)
	go s.serve()
	tb.Cleanup(s.Close)
	return s
}

// Port is the listening port.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Execs returns every command run so far, in order.
func (s *Server) Execs() []Exec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Exec(nil), s.execs...)
}

// Close stops the server, ends every connection and its processes, and
// waits for them.
func (s *Server) Close() {
	s.cancel()
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handleConn(c)
	}
}

func (s *Server) handleConn(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	sc, chans, reqs, err := ssh.NewServerConn(c, s.config)
	if err != nil {
		return
	}
	// The processes of a connection end with it, as sshd's do.
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ssh.DiscardRequests(reqs)
	}()
	var chWG sync.WaitGroup
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		chWG.Add(1)
		go func() {
			defer chWG.Done()
			s.handleSession(ctx, sc.User(), ch, creqs)
		}()
	}
	cancel()
	chWG.Wait()
}

func (s *Server) handleSession(ctx context.Context, user string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		s.mu.Lock()
		s.execs = append(s.execs, Exec{User: user, Command: payload.Command})
		s.mu.Unlock()
		var stdinDone sync.WaitGroup
		code := s.run(ctx, payload.Command, ch, &stdinDone)
		var status [4]byte
		binary.BigEndian.PutUint32(status[:], uint32(code))
		_, _ = ch.SendRequest("exit-status", false, status[:])
		// Closing the channel ends the standard input copy.
		_ = ch.Close()
		stdinDone.Wait()
		return
	}
}

// run executes command with bash, wired to ch, and returns its exit status.
// The copy of the client's standard input is counted in stdinDone.
func (s *Server) run(ctx context.Context, command string, ch ssh.Channel, stdinDone *sync.WaitGroup) int {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	// Kill the whole process group when the connection ends, as sshd's
	// session teardown does, so a script's children do not outlive it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 255
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(ch.Stderr(), "sshtest: %v\n", err)
		return 255
	}
	stdinDone.Add(1)
	go func() {
		defer stdinDone.Done()
		// Ends when the client closes its side or the channel is closed.
		_, _ = io.Copy(stdin, ch)
		_ = stdin.Close()
	}()
	err = cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return exitErr.ExitCode()
	default:
		return 255
	}
}

