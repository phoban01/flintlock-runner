package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

//= docs/requirements/03-scheduler.md#lease-keep-alive
//= type=test
//# If a heartbeat reports that the Lease no longer exists, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and drop the Allocation without a release call.

// TestStageKilledWithItsMicroVMIsARunnerSystemFailure runs a whole Job
// whose Stage ends with the exit status -1 while the Handle is still live,
// which is what the guest agent reports when battery deletes the MicroVM of
// an expired Lease before the next heartbeat. The Executor asks the
// Scheduler at once, finds the Lease lost, and the Job ends as
// runner_system_failure, not as a script_failure with the status -1.
func TestStageKilledWithItsMicroVMIsARunnerSystemFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.sched.leaseLost = fmt.Errorf("%w: claim is Expired", scheduler.ErrLeaseLost)
	f.tr.runs = []transport.Scripted{{ExitStatus: -1}}
	job := testJob()
	job.Steps[0].Script = spec.StepScript{"sleep 600"}

	_, _, err := f.runBuild(context.Background(), job)
	be := buildError(t, err)
	if be.FailureReason != common.RunnerSystemFailure {
		t.Errorf("failure reason = %s, want runner_system_failure", be.FailureReason)
	}
	if !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Errorf("error %v does not carry ErrLeaseLost", err)
	}
	if got := f.sched.checks(); got != 1 {
		t.Errorf("lease checks = %d, want 1", got)
	}
	if _, released, _ := f.sched.counts(); released != 1 {
		t.Errorf("microvm handed back %d times, want 1", released)
	}
}

// TestLeaseCheckOnlyForAStageWithoutAnExitStatus checks each Stage outcome
// against a Lease that is lost and one that is held. Only a Stage without
// an exit status of its own asks the Scheduler, and only a lost Lease
// changes how it is reported. A real exit status is a script_failure even
// when the Lease is lost, and a status of -1 on a held Lease stays what
// the transport said.
func TestLeaseCheckOnlyForAStageWithoutAnExitStatus(t *testing.T) {
	t.Parallel()
	streamErr := fmt.Errorf("host-a: %w", transport.ErrStreamFailed)
	tests := []struct {
		name       string
		run        transport.Scripted
		lost       bool
		wantReason spec.JobFailureReason
		wantCode   int
		wantChecks int
		wantLost   bool
	}{
		{name: "exit -1, lease lost", run: transport.Scripted{ExitStatus: -1}, lost: true,
			wantReason: common.RunnerSystemFailure, wantChecks: 1, wantLost: true},
		{name: "exit -1, lease held", run: transport.Scripted{ExitStatus: -1},
			wantReason: common.ScriptFailure, wantCode: common.NormalizeExitCode(-1), wantChecks: 1},
		{name: "stream failure, lease lost", run: transport.Scripted{Err: streamErr}, lost: true,
			wantReason: common.RunnerSystemFailure, wantChecks: 1, wantLost: true},
		{name: "stream failure, lease held", run: transport.Scripted{Err: streamErr},
			wantReason: common.RunnerSystemFailure, wantChecks: 1},
		{name: "exit 3, lease lost", run: transport.Scripted{ExitStatus: 3}, lost: true,
			wantReason: common.ScriptFailure, wantCode: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if tt.lost {
				f.sched.leaseLost = scheduler.ErrLeaseLost
			}
			e, _, err := f.prepareOnly(context.Background(), testJob())
			if err != nil {
				t.Fatal(err)
			}
			defer e.Cleanup()
			f.tr.runs = []transport.Scripted{tt.run}
			cmd := common.ExecutorCommand{Script: "make\n", Stage: "step_script", Context: context.Background()}

			be := buildError(t, e.Run(cmd))
			if be.FailureReason != tt.wantReason || be.ExitCode != tt.wantCode {
				t.Errorf("error = %v (%s, exit code %d), want %s with exit code %d",
					be, be.FailureReason, be.ExitCode, tt.wantReason, tt.wantCode)
			}
			if got := errors.Is(be, scheduler.ErrLeaseLost); got != tt.wantLost {
				t.Errorf("error %v carries ErrLeaseLost: %t, want %t", be, got, tt.wantLost)
			}
			if tt.run.Err != nil && !errors.Is(be, transport.ErrStreamFailed) {
				t.Errorf("error %v does not carry the stream failure", be)
			}
			if got := f.sched.checks(); got != tt.wantChecks {
				t.Errorf("lease checks = %d, want %d", got, tt.wantChecks)
			}
			if !tt.wantLost {
				return
			}
			// A Stage that ended with its Lease lost is not run again.
			if again := e.Run(cmd); again == nil {
				t.Fatal("the stage ran again and succeeded")
			}
			if n := len(f.tr.stages()); n != 1 {
				t.Errorf("the stage reached the guest %d times, want 1", n)
			}
		})
	}
}
