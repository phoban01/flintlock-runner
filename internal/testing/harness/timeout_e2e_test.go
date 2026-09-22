//go:build e2e

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// timeoutRuns is how many Jobs TestJobTimeoutIsReportedAsJobExecutionTimeout
// runs past their timeout. The race it guards against lost about one Job in
// seven, so twenty gives it every chance to come back.
const timeoutRuns = 20

//= docs/requirements/01-gitlab-protocol.md#job-execution
//= type=test
//# When a Job's context is cancelled because its timeout elapsed,
//# the Runner SHALL report the failure reason `job_execution_timeout`.

// TestJobTimeoutIsReportedAsJobExecutionTimeout runs Jobs past their
// timeout on the fake-Host stack, one after another on one Runner, and
// requires every one of them to be reported as job_execution_timeout.
//
// It is the harness's guard on a race that used to lose now and then. The
// Runner passes the Job's deadline to the Host twice, as timeout_seconds in
// ExecStart (EX-046) and as gRPC's own grpc-timeout on the stream, and the
// Host's gRPC server resets the stream (RST_STREAM) when the latter
// expires. That reset could reach the Runner before the Runner's own timer
// had cancelled the Job's context, and the Executor, seeing a stream
// failure under a context that did not yet say it was done, reported it as
// runner_system_failure; after_script was then attempted too and failed on
// the expired deadline. The Executor now treats a Stage that ends at or
// after its context's deadline as stopped by that deadline, so the reason
// is the timeout whichever side notices first, and after_script is skipped
// as gitlab-runner skips it for an expired Job. On a loaded machine the
// same Jobs can also run out of time while their MicroVM is still being
// allocated, which used to be reported as runner_system_failure too.
func TestJobTimeoutIsReportedAsJobExecutionTimeout(t *testing.T) {
	opts := FakeTier()
	opts.RunnerBinary = runnerBinary
	s := New(t, opts)
	if err := startRunner(s); err != nil {
		t.Fatal(err)
	}
	started := 0
	for i := range timeoutRuns {
		id, err := s.Enqueue(Job{
			Name:        fmt.Sprintf("slow-%d", i),
			Script:      []string{`echo "sleeping past the timeout"`, `sleep 120`, `echo "woke up"`},
			AfterScript: []string{`echo "after_script ran"`},
			Timeout:     5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
		rec, err := s.Wait(ctx, id)
		cancel()
		if err != nil {
			t.Fatalf("run %d: %v\nlast lines of the runner log:\n%s", i, err, s.RunnerLogTail(40))
		}
		if rec.Status != fakegitlab.StatusFailed || rec.FailureReason != "job_execution_timeout" {
			t.Fatalf("run %d: a job past its timeout ended %s with reason %q, want failed with job_execution_timeout\n%s",
				i, rec.Status, rec.FailureReason, rec.Trace)
		}
		// On a loaded machine a Job can use up its timeout before its script
		// starts; that is the timeout too, but it does not exercise the race.
		if strings.Contains(rec.Trace, "sleeping past the timeout") {
			started++
		} else {
			t.Logf("run %d: the job timed out before its script started", i)
		}
		for _, unwanted := range []string{"woke up", "after_script ran", "guest transport failed", "exec stream"} {
			if strings.Contains(rec.Trace, unwanted) {
				t.Errorf("run %d: the trace of a timed-out job contains %q:\n%s", i, unwanted, rec.Trace)
			}
		}
	}
	if started < timeoutRuns/2 {
		t.Errorf("only %d of %d jobs reached their script before the timeout, too few to exercise the race", started, timeoutRuns)
	}
}
