package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/discovery"
)

// handshakeTimeout bounds the SSH handshake when the context has no
// earlier deadline.
const handshakeTimeout = 30 * time.Second

// uploadCommand stores the script it reads on standard input in a new
// private file and prints the file's path.
const uploadCommand = `umask 077 && f=$(mktemp) && cat >"$f" && printf '%s' "$f"`

// safePath is what a path printed by mktemp looks like; anything else is
// refused rather than quoted into a command.
var safePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// SSH runs Scripts over SSH with the configured user and key, verifying
// Host keys against a known_hosts file (FL-011).
type SSH struct {
	user      string
	port      int
	signer    ssh.Signer
	hostKeys  ssh.HostKeyCallback
	overrides map[string]string
	dialer    net.Dialer
}

var _ fleet.Remote = (*SSH)(nil)

// NewSSH returns an SSH Remote for cfg. The key file is read now; the host
// key callback is cfg.KnownHostsFile, or ~/.ssh/known_hosts, unless
// WithHostKeyCallback is given. Only WithHostKeyCallback applies.
func NewSSH(cfg config.SSHRemote, overrides map[string]string, opts ...Option) (*SSH, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return newSSH(cfg, overrides, o)
}

func newSSH(cfg config.SSHRemote, overrides map[string]string, o options) (*SSH, error) {
	if cfg.User == "" {
		return nil, errors.New("remote: SSH needs fleet.remote.ssh.user")
	}
	if cfg.KeyFile == "" {
		return nil, errors.New("remote: SSH needs fleet.remote.ssh.key_file")
	}
	pem, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("remote: read SSH key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("remote: parse SSH key %s: %w", cfg.KeyFile, err)
	}
	hostKeys := o.hostKeys
	if hostKeys == nil {
		path := cfg.KnownHostsFile
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("remote: locate known_hosts: %w", err)
			}
			path = filepath.Join(home, ".ssh", "known_hosts")
		}
		if hostKeys, err = knownhosts.New(path); err != nil {
			return nil, fmt.Errorf("remote: load known hosts %s: %w", path, err)
		}
	}
	port := cfg.Port
	if port == 0 {
		port = config.DefaultSSHPort
	}
	return &SSH{user: cfg.User, port: port, signer: signer, hostKeys: hostKeys, overrides: overrides}, nil
}

// Addr is where the SSH Remote connects for inst.
func (r *SSH) Addr(inst fleet.Instance) string {
	return net.JoinHostPort(discovery.EndpointAddress(inst, r.overrides), strconv.Itoa(r.port))
}

// Run implements fleet.Remote. The script content is copied to a private
// temporary file on the instance and run there with bash, with s.Stdin as
// its standard input, so neither the content nor the secrets on stdin appear
// on a command line (SE-015). Output is written to out prefixed with the
// instance id as it arrives (FL-014). A non-zero exit returns the result and
// an *ExitError; when ctx ends the connection is closed, which ends the
// remote processes.
func (r *SSH) Run(ctx context.Context, inst fleet.Instance, s fleet.Script, out io.Writer) (*fleet.RunResult, error) {
	ctx, cancel := withScriptTimeout(ctx, s)
	defer cancel()
	client, err := r.dial(ctx, inst)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()

	wrap := func(err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%s on %s: %w", s.Name, inst.ID, ctxErr)
		}
		return fmt.Errorf("%s on %s: %w", s.Name, inst.ID, err)
	}

	path, err := upload(client, s.Content)
	if err != nil {
		return nil, wrap(err)
	}

	sess, err := client.NewSession()
	if err != nil {
		return nil, wrap(fmt.Errorf("open session: %w", err))
	}
	defer sess.Close()
	var mu sync.Mutex
	pOut, pErr := newLinePrefixer(&mu, out, inst.ID), newLinePrefixer(&mu, out, inst.ID)
	var stdout, stderr bytes.Buffer
	sess.Stdout = io.MultiWriter(&stdout, pOut)
	sess.Stderr = io.MultiWriter(&stderr, pErr)
	if s.Stdin != nil {
		sess.Stdin = s.Stdin
	}
	q := "'" + path + "'"
	runErr := sess.Run("bash " + q + "; rc=$?; rm -f " + q + "; exit $rc")
	_ = pOut.Flush()
	_ = pErr.Flush()

	res := &fleet.RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *ssh.ExitError
	switch {
	case runErr == nil:
		return res, nil
	case ctx.Err() != nil:
		return nil, wrap(runErr)
	case errors.As(runErr, &exit):
		res.ExitCode = exit.ExitStatus()
		return res, exitError(inst, s, res)
	default:
		return nil, wrap(runErr)
	}
}

// dial connects and authenticates to inst, verifying its host key.
func (r *SSH) dial(ctx context.Context, inst fleet.Instance) (*ssh.Client, error) {
	addr := r.Addr(inst)
	conn, err := r.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh %s at %s: %w", inst.ID, addr, err)
	}
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            r.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(r.signer)},
		HostKeyCallback: r.hostKeys,
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh %s at %s: %w", inst.ID, addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, chans, reqs), nil
}

// upload copies content to a new private file on the remote and returns its
// path.
func upload(client *ssh.Client, content string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("open upload session: %w", err)
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if err := sess.Run(uploadCommand); err != nil {
		return "", fmt.Errorf("upload script: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	path := stdout.String()
	if !safePath.MatchString(path) {
		return "", fmt.Errorf("upload script: unexpected temporary path %q", path)
	}
	return path, nil
}
