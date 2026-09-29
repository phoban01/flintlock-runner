package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Gate is a condition a Job's script can wait on and the test opens. The
// fake Hosts run a Job's commands on this machine, so a file under the
// Stack's root is visible to both.
type Gate struct {
	path string
}

// NewGate makes a closed gate.
func (s *Stack) NewGate(name string) *Gate {
	return &Gate{path: filepath.Join(s.Root, "gate-"+name)}
}

// Wait is a shell line that blocks until the gate is open, for up to two
// minutes, after which it fails the script.
func (g *Gate) Wait() string {
	return fmt.Sprintf(`for i in $(seq 1 1200); do [ -e %q ] && break; sleep 0.1; done; [ -e %q ]`, g.path, g.path)
}

// Open opens the gate.
func (g *Gate) Open() error {
	return os.WriteFile(g.path, nil, 0o600)
}

// Trace is what the fake GitLab has recorded of Job id's log so far.
func (s *Stack) Trace(id int64) string {
	if rec := s.GitLab.Record(id); rec != nil {
		return rec.Trace
	}
	return ""
}

// WaitTrace blocks until Job id's log contains want, and fails if the Job
// ends or the Runner exits first.
func (s *Stack) WaitTrace(ctx context.Context, id int64, want string) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	exited := s.runnerExited()
	for {
		rec := s.GitLab.Record(id)
		if rec != nil && strings.Contains(rec.Trace, want) {
			return nil
		}
		if rec != nil && Final(rec.Status) {
			return fmt.Errorf("harness: job %d ended %s without logging %q:\n%s", id, rec.Status, want, rec.Trace)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("harness: waiting for job %d to log %q: %w; it logged so far:\n%s", id, want, context.Cause(ctx), s.Trace(id))
		case <-exited:
			return errors.Join(fmt.Errorf("harness: job %d did not log %q", id, want), s.runnerExitError())
		case <-ticker.C:
		}
	}
}
