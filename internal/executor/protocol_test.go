package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/transport"
)

//= docs/requirements/01-gitlab-protocol.md#job-execution
//= type=test
//# The Runner SHALL generate each Stage script with the `bash`
//# shell implementation of the gitlab-runner `shells` package.

// TestStageScriptsAreBash runs a Job under a RunnerConfig that asks for the
// sh shell and checks every Stage script the guest received came from the
// bash shell implementation all the same.
func TestStageScriptsAreBash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.runnerShell = "sh"
	if _, _, err := f.runBuild(context.Background(), testJob()); err != nil {
		t.Fatal(err)
	}
	stages := f.tr.stages()
	if len(stages) == 0 {
		t.Fatal("no stage ran")
	}
	for _, r := range stages {
		if !strings.HasPrefix(string(r.Stdin), "#!/usr/bin/env bash\n") {
			t.Errorf("stage script is not the bash shell's:\n%.200s", r.Stdin)
		}
	}
}

//= docs/requirements/01-gitlab-protocol.md#shutdown
//= type=test
//# When the Runner receives a second termination signal during a
//# graceful shutdown, the Runner SHALL cancel all running Jobs immediately.

// TestAbortAllCancelsRunningAndPreparingJobs aborts while one Job's Stage
// runs and another Job waits for its MicroVM: both end at once as
// runner_system_failure.
func TestAbortAllCancelsRunningAndPreparingJobs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	f.tr.hook = func(ctx context.Context, _ transport.Command, _ []byte) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	running := make(chan error, 1)
	go func() {
		running <- e.Run(common.ExecutorCommand{Script: "sleep 600\n", Stage: "step_script", Context: context.Background()})
	}()

	f.sched.mu.Lock()
	f.sched.allocBlock = true
	f.sched.mu.Unlock()
	preparing := make(chan error, 1)
	go func() {
		_, _, err := f.prepareOnly(context.Background(), testJob())
		preparing <- err
	}()
	<-f.sched.allocStarted // the first job's allocation
	<-f.sched.allocStarted // the second's, now blocked

	f.provider.AbortAll()
	for name, ch := range map[string]chan error{"running": running, "preparing": preparing} {
		select {
		case err := <-ch:
			be := buildError(t, err)
			if be.FailureReason != common.RunnerSystemFailure || !errors.Is(err, ErrAborted) {
				t.Errorf("%s job: %v (%s), want runner_system_failure from the abort", name, err, be.FailureReason)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s job was not cancelled", name)
		}
	}
}

//= docs/requirements/01-gitlab-protocol.md#job-log
//= type=test
//# The Runner SHALL mask the values of Job variables flagged as
//# masked before they are written to the Job log.

// TestMaskedOutputIsMaskedInTheJobLog has the guest print a masked
// variable's value: the Job log shows it masked, because the executor
// writes Stage output only through the Build's logger.
func TestMaskedOutputIsMaskedInTheJobLog(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tr.runs = []transport.Scripted{{}, {}, {Stdout: []byte("token is s3cr3t-value-123\n")}}
	job := testJob()
	job.Variables = append(job.Variables, spec.Variable{Key: "API_TOKEN", Value: "s3cr3t-value-123", Masked: true})
	trace, _, err := f.runBuild(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(trace.String(), "s3cr3t-value-123") {
		t.Errorf("masked value in the job log:\n%s", trace)
	}
	if !strings.Contains(trace.String(), "token is [MASKED]") {
		t.Errorf("the stage output did not reach the job log masked:\n%s", trace)
	}
}

//= docs/requirements/01-gitlab-protocol.md#job-execution
//= type=test
//# When a Job's context is cancelled because its timeout elapsed,
//# the Runner SHALL report the failure reason `job_execution_timeout`.

//= docs/requirements/01-gitlab-protocol.md#job-execution
//= type=test
//# The Runner SHALL enforce the Job timeout carried in the Job's
//# `runner_info.timeout` field by cancelling the Job's context when it elapses.

// TestJobTimeoutEndsTheStage gives a Job one second and a script that never
// finishes: the Stage's context is cancelled and the Job fails with
// job_execution_timeout.
func TestJobTimeoutEndsTheStage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tr.hook = func(ctx context.Context, _ transport.Command, stdin []byte) (int, error) {
		if !strings.Contains(string(stdin), "sleep-forever") {
			return 0, nil
		}
		<-ctx.Done()
		return -1, ctx.Err()
	}
	job := testJob()
	job.RunnerInfo.Timeout = 1
	job.Steps[0].Script = spec.StepScript{"sleep-forever"}
	start := time.Now()
	_, _, err := f.runBuild(context.Background(), job)
	if be := buildError(t, err); be.FailureReason != common.JobExecutionTimeout {
		t.Errorf("failure reason = %s, want job_execution_timeout", be.FailureReason)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("the job took %s to time out", d)
	}
}
