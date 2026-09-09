package fake

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
)

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL represent each MicroVM as a sandbox directory on the
//# local filesystem and SHALL run each `ExecCommand` as a local process
//# rooted in that directory, streaming standard input, standard output,
//# standard error and the exit code as `flintlockd` does.

// TestRecvPrefersTheHandlerResultOverTheCancellationThatFollowsIt pins the
// ranking in Recv against the scheduler.
//
// finish closes done and then cancels the stream context, so at the end of
// every healthy exchange both become ready a moment apart. Recv checks done
// without blocking first, but finish can land in the window between that
// check and the select below it, leaving both cases ready; Go then polls at
// random and half the time picks the context. The caller would see a
// transport failure in place of the exit code the handler had already
// produced, which is the one thing a fake Host must never invent.
//
// The window is small, so this drives it many times rather than once. Before
// the re-check in Recv's context branch it failed roughly one run in sixty
// of the package's own suite, and at iteration 6883 here.
func TestRecvPrefersTheHandlerResultOverTheCancellationThatFollowsIt(t *testing.T) {
	t.Parallel()

	const iterations = 50000
	for i := range iterations {
		s := newMemExecStream(context.Background(), context.Background(), context.Background(), "h1")

		// What the handler produced before it finished: one response, then
		// a normal end of stream.
		s.resps <- &execv1.ExecCommandResponse{}

		ready := make(chan struct{})
		go func() {
			close(ready)
			s.finish(nil)
		}()
		<-ready

		// The response is buffered, so the first Recv returns it whatever
		// the scheduler does. The second is the one that races.
		if _, err := s.Recv(); err != nil {
			t.Fatalf("iteration %d: first Recv returned %v, want the buffered response", i, err)
		}
		switch _, err := s.Recv(); {
		case errors.Is(err, io.EOF):
			// The handler ended normally, which is what finish(nil) means.
		case err == nil:
			t.Fatalf("iteration %d: second Recv returned a response, want io.EOF", i)
		case strings.Contains(err.Error(), "context canceled"):
			t.Fatalf("iteration %d: Recv reported %v; the handler had already"+
				" finished, so its result must win over the cancellation that"+
				" accompanies it", i, err)
		default:
			t.Fatalf("iteration %d: Recv returned %v, want io.EOF", i, err)
		}
	}
}
