package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/transport"
)

// lateCtx is a Job context whose deadline timer fires late: it reports its
// deadline at once, as a context from context.WithTimeout does, but it is
// done, with context.DeadlineExceeded, only when expire is called. That is
// the window in which the Host's gRPC server, enforcing the same deadline
// from the stream's grpc-timeout, can reset the exec stream before the
// Runner's own timer has cancelled the Job (GL-043).
type lateCtx struct {
	context.Context
	deadline time.Time
	done     chan struct{}
	once     sync.Once
}

func newLateCtx(deadline time.Time) *lateCtx {
	return &lateCtx{Context: context.Background(), deadline: deadline, done: make(chan struct{})}
}

func (c *lateCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *lateCtx) Done() <-chan struct{}       { return c.done }

func (c *lateCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// expire is the late timer firing.
func (c *lateCtx) expire() { c.once.Do(func() { close(c.done) }) }

// hostReset is the error the exec transport returns when the Host's gRPC
// server resets the stream at the deadline, worded as the harness recorded
// it.
func hostReset() error {
	return fmt.Errorf("host host-a: microvm vm-a: exec stream failed: %w", errors.Join(
		errors.New("stream terminated by RST_STREAM with error code: CANCEL"),
		context.DeadlineExceeded,
		transport.ErrStreamFailed))
}

//= docs/requirements/01-gitlab-protocol.md#job-execution
//= type=test
//# When a Job's context is cancelled because its timeout elapsed,
//# the Runner SHALL report the failure reason `job_execution_timeout`.

// TestStreamResetAtTheDeadlineIsTheTimeout is the Executor's half of the
// GL-043 race the harness found: the Host ends the Stage's stream at the
// Job's deadline and the failure reaches Run before the Job's context says
// it is done. Run must not return while the context is still live, and
// once it is done has to report the deadline, not a runner_system_failure.
func TestStreamResetAtTheDeadlineIsTheTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()

	ctx := newLateCtx(time.Now().Add(-time.Millisecond))
	defer ctx.expire()
	reset := make(chan struct{})
	f.tr.hook = func(context.Context, transport.Command, []byte) (int, error) {
		close(reset)
		return -1, hostReset()
	}
	errc := make(chan error, 1)
	go func() {
		errc <- e.Run(common.ExecutorCommand{Script: "sleep 120\n", Stage: "step_script", Context: ctx})
	}()
	<-reset
	select {
	case err := <-errc:
		t.Fatalf("Run returned %v while the job's context was still live, so the build would not see the timeout", err)
	case <-time.After(200 * time.Millisecond):
	}
	ctx.expire()
	select {
	case err := <-errc:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run error = %v, want the job's deadline", err)
		}
		var be *common.BuildError
		if errors.As(err, &be) {
			t.Errorf("Run returned a BuildError with reason %s, want the bare deadline the Build maps to job_execution_timeout", be.FailureReason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return once the job's context was done")
	}
}

//= docs/requirements/01-gitlab-protocol.md#job-execution
//= type=test
//# When a Job's context is cancelled because its timeout elapsed,
//# the Runner SHALL report the failure reason `job_execution_timeout`.

// TestJobTimeoutRacingAStreamResetIsReportedAsTheTimeout runs a whole Job
// through the library's Build in the race the harness found: at the Job's
// deadline the Host resets step_script's stream, and the Job's context is
// done a little later. The Job has to fail with job_execution_timeout, and
// after_script must not be attempted, because gitlab-runner does not run
// after_script for a Job whose own timeout expired.
func TestJobTimeoutRacingAStreamResetIsReportedAsTheTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	deadline := time.Now().Add(2 * time.Second)
	ctx := newLateCtx(deadline)
	defer ctx.expire()
	f.tr.hook = func(_ context.Context, _ transport.Command, stdin []byte) (int, error) {
		if !strings.Contains(string(stdin), "sleep-past-the-deadline") {
			return 0, nil
		}
		time.Sleep(time.Until(deadline))
		// The late timer fires after the reset has reached the Runner.
		time.AfterFunc(200*time.Millisecond, ctx.expire)
		return -1, hostReset()
	}
	job := testJob()
	job.Steps = spec.Steps{
		{Name: spec.StepNameScript, Script: spec.StepScript{"sleep-past-the-deadline"}, When: spec.StepWhenOnSuccess},
		{Name: spec.StepNameAfterScript, Script: spec.StepScript{"after-script-marker"}, When: spec.StepWhenAlways},
	}
	trace, _, err := f.runBuild(ctx, job)
	if be := buildError(t, err); be.FailureReason != common.JobExecutionTimeout {
		t.Errorf("failure reason = %s, want job_execution_timeout\n%s", be.FailureReason, trace)
	}
	for _, r := range f.tr.stages() {
		if strings.Contains(string(r.Stdin), "after-script-marker") {
			t.Errorf("after_script was attempted after the job's timeout expired\n%s", trace)
		}
	}
}

//= docs/requirements/01-gitlab-protocol.md#job-execution
//= type=test
//# When a Job's context is cancelled because its timeout elapsed,
//# the Runner SHALL report the failure reason `job_execution_timeout`.

// TestJobTimeoutDuringPrepareIsTheTimeout lets the allocation wait out the
// Job's own timeout, as it does when a loaded Pool Manager is slow: the Job
// has to fail with job_execution_timeout, not with the allocation's
// runner_system_failure. The harness hit this under load, reported as
// "Job failed (system failure): allocating a microvm: ... context deadline
// exceeded". The Executor's own prepare timeout is a different matter and
// stays a system failure.
func TestJobTimeoutDuringPrepareIsTheTimeout(t *testing.T) {
	t.Parallel()
	t.Run("job timeout", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.sched.allocBlock = true
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, _, err := f.prepareOnly(ctx, testJob())
		if be := buildError(t, err); be.FailureReason != common.JobExecutionTimeout {
			t.Errorf("failure reason = %s, want job_execution_timeout (%v)", be.FailureReason, err)
		}
	})
	t.Run("executor prepare timeout", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.deps.Timeouts.Prepare = 50 * time.Millisecond
		f.sched.allocBlock = true
		_, _, err := f.prepareOnly(context.Background(), testJob())
		if be := buildError(t, err); be.FailureReason != common.RunnerSystemFailure {
			t.Errorf("failure reason = %s, want runner_system_failure (%v)", be.FailureReason, err)
		}
	})
}

//= docs/requirements/02-executor.md#run
//= type=test
//# If the Guest Transport stream fails before the Stage's exit
//# status is known, then the Executor SHALL return a system error and SHALL
//# NOT re-run the Stage.

// TestStreamFailureBeforeTheDeadlineIsASystemFailure is the other side of
// the same line: a stream that fails while the Job still has time left is a
// stream failure, not a timeout, although the Job has a deadline and the
// failure looks like the one a reset at the deadline produces.
func TestStreamFailureBeforeTheDeadlineIsASystemFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	f.tr.runs = []transport.Scripted{{Err: hostReset()}}
	be := buildError(t, e.Run(common.ExecutorCommand{Script: "make\n", Stage: "step_script", Context: ctx}))
	if be.FailureReason != common.RunnerSystemFailure || !errors.Is(be, transport.ErrStreamFailed) {
		t.Errorf("error = %v (%s), want a runner_system_failure wrapping the stream failure", be, be.FailureReason)
	}
}
