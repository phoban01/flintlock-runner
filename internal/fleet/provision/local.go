package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// LocalRemote runs a Script on the machine the Fleet Controller runs on,
// for the control_node step when that machine is the Control Node. The
// script is written to an owner-only temporary file and run with bash;
// Stdin is fed to it, so secrets never appear on a command line (SE-015).
type LocalRemote struct {
	// Bash is the bash binary; empty means "bash" from PATH.
	Bash string
}

var _ fleet.Remote = LocalRemote{}

// Run runs s locally. Output is streamed to out prefixed with the instance
// id, as every Remote does (FL-014).
func (l LocalRemote) Run(ctx context.Context, inst fleet.Instance, s fleet.Script, out io.Writer) (*fleet.RunResult, error) {
	bash := l.Bash
	if bash == "" {
		bash = "bash"
	}
	f, err := os.CreateTemp("", "flintlock-runner-"+s.Name+"-*.sh")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(s.Content); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bash, f.Name())
	cmd.Stdin = s.Stdin
	if cmd.Stdin == nil {
		cmd.Stdin = strings.NewReader("")
	}
	var stdout, stderr bytes.Buffer
	pw := &prefixWriter{w: out, prefix: inst.ID + ": "}
	cmd.Stdout = io.MultiWriter(&stdout, pw)
	cmd.Stderr = io.MultiWriter(&stderr, pw)
	err = cmd.Run()
	pw.flush()
	res := &fleet.RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && ctx.Err() == nil:
		res.ExitCode = exit.ExitCode()
	default:
		return res, fmt.Errorf("run %s locally: %w", s.Name, err)
	}
	return res, nil
}

// prefixWriter prefixes every line written to it.
type prefixWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		if _, err := fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf[:i]); err != nil {
			return len(b), err
		}
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

func (p *prefixWriter) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) > 0 {
		_, _ = fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf)
		p.buf = nil
	}
}
