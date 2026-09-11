package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// sleepingJob is a Job whose script creates marker, which is how the test
// knows the script is running (the fake Host runs it as a local process),
// then sleeps and prints finished.
func sleepingJob(id int64, marker string, sleep int) *spec.Job {
	return &spec.Job{
		ID:         id,
		JobInfo:    spec.JobInfo{Name: "sleeper", Stage: "test", ProjectID: 7, ProjectName: "demo"},
		GitInfo:    spec.GitInfo{RepoURL: "https://gitlab.example.com/group/demo.git", Sha: "0123456789abcdef0123456789abcdef01234567", Ref: "main"},
		RunnerInfo: spec.RunnerInfo{Timeout: 600},
		Variables:  spec.Variables{{Key: "GIT_STRATEGY", Value: "none", Public: true}},
		Steps: spec.Steps{{
			Name:   spec.StepNameScript,
			Script: spec.StepScript{"touch '" + marker + "'", "sleep " + strconv.Itoa(sleep), "echo finished-sleeping"},
			When:   spec.StepWhenOnSuccess,
		}},
	}
}

// runUntilMidScript starts the stack and the runner with the given shutdown
// timeout, enqueues a sleeping Job and returns once its script is running.
func runUntilMidScript(t *testing.T, shutdownTimeout time.Duration, sleep int) (*stack, *runnerProc) {
	t.Helper()
	bin := buildBinary(t)
	s, path := startStack(t, shutdownTimeout)
	marker := filepath.Join(s.dir, "script-started")
	if err := s.gitlab.Enqueue(sleepingJob(7, marker, sleep)); err != nil {
		t.Fatal(err)
	}
	r := startRunner(t, bin, path)
	waitFor(t, 60*time.Second, "the job's script to start", func() bool {
		select {
		case err := <-r.exited:
			t.Fatalf("runner exited early: %v\n%s", err, r.out.String())
		default:
		}
		_, err := os.Stat(marker)
		return err == nil
	})
	return s, r
}

// finalRecord waits for the Job's final state.
func finalRecord(t *testing.T, s *stack, r *runnerProc, d time.Duration) *fakegitlab.JobRecord {
	t.Helper()
	waitFor(t, d, "the job to finish", func() bool {
		rec := s.gitlab.Record(7)
		return rec != nil && (rec.Status == fakegitlab.StatusSuccess || rec.Status == fakegitlab.StatusFailed)
	})
	return s.gitlab.Record(7)
}

//= docs/requirements/01-gitlab-protocol.md#shutdown
//= type=test
//# While a graceful shutdown is in progress, the Runner SHALL allow
//# running Jobs to finish for up to the configured shutdown timeout.

//= docs/requirements/01-gitlab-protocol.md#shutdown
//= type=test
//# When the Runner receives `SIGTERM` or `SIGQUIT`, the Runner SHALL
//# stop requesting Jobs and release every unconverted Reservation.

// TestSIGTERMLetsARunningJobFinish sends SIGTERM while a Job's script is
// sleeping. The Job has to run to the end and succeed, the Runner has to
// stop asking GitLab for Jobs and exit cleanly, and every Lease has to be
// handed back.
func TestSIGTERMLetsARunningJobFinish(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end")
	}
	s, r := runUntilMidScript(t, 60*time.Second, 4)
	r.signal(t, syscall.SIGTERM)
	signalledAt := len(s.gitlab.Requests())

	rec := finalRecord(t, s, r, 60*time.Second)
	if rec.Status != fakegitlab.StatusSuccess {
		t.Fatalf("job ended %s (%s) after SIGTERM; want it to finish\ntrace:\n%s\nrunner:\n%s",
			rec.Status, rec.FailureReason, rec.Trace, r.out.String())
	}
	if !strings.Contains(rec.Trace, "finished-sleeping") {
		t.Errorf("the script did not run to its end:\n%s", rec.Trace)
	}
	r.waitExit(t, 60*time.Second)
	requests := s.gitlab.Requests()
	for _, req := range requests[min(signalledAt+1, len(requests)):] {
		if req == "POST /api/v4/jobs/request" {
			t.Errorf("a job was requested after SIGTERM: %v", requests[signalledAt:])
			break
		}
	}
	waitFor(t, 10*time.Second, "every lease to be released", func() bool { return len(s.pm.Leases()) == 0 })
}

//= docs/requirements/01-gitlab-protocol.md#shutdown
//= type=test
//# If running Jobs have not finished when the shutdown timeout
//# elapses, then the Runner SHALL cancel them, report them as failed with the
//# reason `runner_system_failure` and release their MicroVMs.

// TestShutdownTimeoutFailsTheJobAsARunnerSystemFailure gives a Job longer to
// sleep than the shutdown timeout.
func TestShutdownTimeoutFailsTheJobAsARunnerSystemFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end")
	}
	s, r := runUntilMidScript(t, 2*time.Second, 120)
	start := time.Now()
	r.signal(t, syscall.SIGTERM)
	rec := finalRecord(t, s, r, 60*time.Second)
	if rec.Status != fakegitlab.StatusFailed || rec.FailureReason != "runner_system_failure" {
		t.Fatalf("job ended %s (%s); want failed with runner_system_failure\nrunner:\n%s", rec.Status, rec.FailureReason, r.out.String())
	}
	if d := time.Since(start); d > 45*time.Second {
		t.Errorf("the job was cancelled %s after SIGTERM with a 2s shutdown timeout", d)
	}
	r.waitExit(t, 60*time.Second)
	waitFor(t, 10*time.Second, "every lease to be released", func() bool { return len(s.pm.Leases()) == 0 })
}

//= docs/requirements/01-gitlab-protocol.md#shutdown
//= type=test
//# When the Runner receives a second termination signal during a
//# graceful shutdown, the Runner SHALL cancel all running Jobs immediately.

// TestSecondSignalCancelsRunningJobs sends SIGTERM twice with a long
// shutdown timeout: the second one ends the Job at once.
func TestSecondSignalCancelsRunningJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end")
	}
	s, r := runUntilMidScript(t, 5*time.Minute, 120)
	r.signal(t, syscall.SIGTERM)
	time.Sleep(time.Second)
	start := time.Now()
	r.signal(t, syscall.SIGTERM)
	rec := finalRecord(t, s, r, 45*time.Second)
	if rec.Status != fakegitlab.StatusFailed || rec.FailureReason != "runner_system_failure" {
		t.Fatalf("job ended %s (%s); want failed with runner_system_failure\nrunner:\n%s", rec.Status, rec.FailureReason, r.out.String())
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("the job ended %s after the second signal", d)
	}
	r.waitExit(t, 60*time.Second)
	waitFor(t, 10*time.Second, "every lease to be released", func() bool { return len(s.pm.Leases()) == 0 })
}
