package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// DefaultPollInterval is how often the Systems Manager Remote polls.
const DefaultPollInterval = 2 * time.Second

// ErrStdinUnsupported is returned when a Script with standard input is run
// over Systems Manager, which has no standard input. Secrets for that path
// come from Parameters on the Host side (SE-015, FL-091); they are never
// folded into the script content.
var ErrStdinUnsupported = errors.New("remote: Systems Manager Run Command has no standard input; read secrets from Systems Manager parameters on the Host")

// Systems Manager invocation statuses.
const (
	ssmSuccess = "Success"
	ssmFailed  = "Failed"
)

// ssmPending lists the statuses that mean the command has not finished.
var ssmPending = map[string]bool{"": true, "Pending": true, "InProgress": true, "Delayed": true}

// SSM runs Scripts through Systems Manager Run Command (FL-010).
type SSM struct {
	client fleet.SSM
	clock  clock.Clock
	poll   time.Duration
}

var _ fleet.Remote = (*SSM)(nil)

// NewSSM returns a Systems Manager Remote over client. Only WithClock and
// WithPollInterval apply.
func NewSSM(client fleet.SSM, opts ...Option) *SSM {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return newSSM(client, o)
}

func newSSM(client fleet.SSM, o options) *SSM {
	r := &SSM{client: client, clock: o.clock, poll: o.pollInterval}
	if r.clock == nil {
		r.clock = clock.Real{}
	}
	if r.poll <= 0 {
		r.poll = DefaultPollInterval
	}
	return r
}

// Run implements fleet.Remote. It sends s to inst, then polls the invocation,
// writing new output to out prefixed with the instance id at every poll
// (FL-014). Systems Manager keeps at most the first 24,000 characters of
// each stream. A non-zero exit returns the result and an *ExitError.
func (r *SSM) Run(ctx context.Context, inst fleet.Instance, s fleet.Script, out io.Writer) (*fleet.RunResult, error) {
	if err := requireNoStdin(s.Stdin); err != nil {
		return nil, err
	}
	ctx, cancel := withScriptTimeout(ctx, s)
	defer cancel()
	id, err := r.client.SendCommand(ctx, fleet.SendCommandInput{
		InstanceIDs: []string{inst.ID},
		Script:      s.Content,
		Comment:     s.Name,
		Timeout:     s.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("send %s to %s: %w", s.Name, inst.ID, err)
	}
	var mu sync.Mutex
	stdout, stderr := newLinePrefixer(&mu, out, inst.ID), newLinePrefixer(&mu, out, inst.ID)
	var seenOut, seenErr int
	for {
		inv, err := r.client.GetCommandInvocation(ctx, id, inst.ID)
		if err != nil {
			return nil, fmt.Errorf("poll %s on %s (command %s): %w", s.Name, inst.ID, id, err)
		}
		seenOut = writeNew(stdout, inv.Stdout, seenOut)
		seenErr = writeNew(stderr, inv.Stderr, seenErr)
		if !ssmPending[inv.Status] {
			_ = stdout.Flush()
			_ = stderr.Flush()
			res := &fleet.RunResult{ExitCode: inv.ResponseCode, Stdout: inv.Stdout, Stderr: inv.Stderr}
			switch {
			case inv.Status == ssmSuccess:
				return res, nil
			case inv.Status == ssmFailed && inv.ResponseCode > 0:
				// -1 is Systems Manager's code for a script that never ran.
				return res, exitError(inst, s, res)
			default:
				return nil, fmt.Errorf("%s on %s (command %s) ended %s", s.Name, inst.ID, id, inv.Status)
			}
		}
		t := r.clock.NewTimer(r.poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%s on %s (command %s): %w", s.Name, inst.ID, id, ctx.Err())
		case <-t.C():
		}
	}
}

// writeNew writes the part of full after the first seen bytes and returns
// the new count.
func writeNew(w io.Writer, full string, seen int) int {
	if len(full) <= seen {
		return seen
	}
	_, _ = io.WriteString(w, full[seen:])
	return len(full)
}

// requireNoStdin fails when r has any data.
func requireNoStdin(r io.Reader) error {
	if r == nil {
		return nil
	}
	var b [1]byte
	n, err := io.ReadFull(r, b[:])
	if n > 0 {
		return ErrStdinUnsupported
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("remote: read script stdin: %w", err)
	}
	return nil
}
