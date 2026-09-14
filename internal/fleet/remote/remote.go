package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// ExitError is returned with the RunResult when a script ran to completion
// with a non-zero exit status. Every other error means the script's outcome
// is unknown (not delivered, timed out, cancelled or disconnected) and comes
// with a nil RunResult.
type ExitError struct {
	Instance string
	Script   string
	Code     int
	// Stderr is the tail of the script's standard error.
	Stderr string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("script %s on %s exited with status %d", e.Script, e.Instance, e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

// maxStderrTail bounds ExitError.Stderr.
const maxStderrTail = 512

func exitError(inst fleet.Instance, s fleet.Script, res *fleet.RunResult) error {
	if res.ExitCode == 0 {
		return nil
	}
	tail := res.Stderr
	if len(tail) > maxStderrTail {
		tail = "..." + tail[len(tail)-maxStderrTail:]
	}
	return &ExitError{Instance: inst.ID, Script: s.Name, Code: res.ExitCode, Stderr: tail}
}

// withScriptTimeout bounds ctx by s.Timeout when it is set.
func withScriptTimeout(ctx context.Context, s fleet.Script) (context.Context, context.CancelFunc) {
	if s.Timeout > 0 {
		return context.WithTimeout(ctx, s.Timeout)
	}
	return context.WithCancel(ctx)
}

// Option configures New.
type Option func(*options)

type options struct {
	clock        clock.Clock
	pollInterval time.Duration
	hostKeys     ssh.HostKeyCallback
}

// WithClock sets the clock the Systems Manager Remote polls on.
func WithClock(c clock.Clock) Option { return func(o *options) { o.clock = c } }

// WithPollInterval sets how often the Systems Manager Remote polls an
// invocation. The default is DefaultPollInterval.
func WithPollInterval(d time.Duration) Option { return func(o *options) { o.pollInterval = d } }

// WithHostKeyCallback replaces the known_hosts file as the SSH Remote's
// host key verification. It cannot be nil; verification is never skipped.
func WithHostKeyCallback(cb ssh.HostKeyCallback) Option { return func(o *options) { o.hostKeys = cb } }

//= docs/requirements/06-fleet.md#remote-execution
//# The Fleet Controller SHALL execute commands on instances through
//# AWS Systems Manager Run Command by default.

//= docs/requirements/06-fleet.md#remote-execution
//# Where SSH is configured, the Fleet Controller SHALL execute
//# commands over SSH with the configured user and key instead of Systems
//# Manager.

// New returns the Remote for f's remote execution mode: Systems Manager Run
// Command through ssmClient when the mode is ssm or unset, and SSH with the
// configured user, key and known hosts when it is ssh, in which case
// ssmClient is not used and may be nil. The SSH Remote reaches an instance
// at its endpoint override when one is configured, else its private IP.
func New(f *config.Fleet, ssmClient fleet.SSM, opts ...Option) (fleet.Remote, error) {
	if f == nil {
		return nil, errors.New("remote: no fleet section")
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	switch f.Remote.Mode {
	case config.RemoteSSM, "":
		if ssmClient == nil {
			return nil, errors.New("remote: Systems Manager mode needs an SSM client")
		}
		return newSSM(ssmClient, o), nil
	case config.RemoteSSH:
		return newSSH(f.Remote.SSH, f.EndpointOverrides, o)
	default:
		return nil, fmt.Errorf("remote: unknown remote mode %q", f.Remote.Mode)
	}
}
