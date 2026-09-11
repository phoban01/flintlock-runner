package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/buildlogger"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

//= docs/requirements/02-executor.md#prepare
//= type=test
//# When `Prepare` is called, the Executor SHALL ask the Scheduler to
//# resolve a Profile for the Job.

func TestPrepareResolvesTheProfileForTheJob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	job := testJob()
	job.Image.Name = "$PROFILE_IMAGE"
	job.Variables = append(job.Variables, spec.Variable{Key: "PROFILE_IMAGE", Value: "builders", Public: true})
	e, _, err := f.prepareOnly(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	if len(f.sched.resolved) != 1 {
		t.Fatalf("ResolveProfile called %d times", len(f.sched.resolved))
	}
	if got := f.sched.resolved[0]; got.ID != job.ID || got.Image != "builders" {
		t.Errorf("ResolveProfile(%+v), want id %d and the expanded image", got, job.ID)
	}
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# If no Profile can be resolved for the Job, then the Executor
//# SHALL fail the Job with the failure reason `runner_unsupported` and a log
//# line naming the requested Job Image.

func TestPrepareWithoutAProfileFailsRunnerUnsupported(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// The Scheduler's error deliberately does not name the image, so the
	// log line has to.
	f.sched.profileErr = scheduler.ErrNoProfile
	job := testJob()
	job.Image.Name = "ruby:3.3"
	_, trace, err := f.prepareOnly(context.Background(), job)
	be := buildError(t, err)
	if be.FailureReason != RunnerUnsupported {
		t.Errorf("failure reason = %s, want runner_unsupported", be.FailureReason)
	}
	if !strings.Contains(trace.String(), `"ruby:3.3"`) {
		t.Errorf("job log does not name the image:\n%s", trace)
	}
	if allocs, _, _ := f.sched.counts(); allocs != 0 {
		t.Errorf("a microvm was allocated for a job with no profile")
	}
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# When a Profile has been resolved, the Executor SHALL ask the
//# Scheduler to allocate a MicroVM for the Job and the Profile, passing the
//# Job's context so that cancellation aborts the allocation.

// TestPrepareAllocationFollowsTheJobContext blocks the allocation until its
// context ends and cancels the Job: Prepare has to come back, which it can
// only do if the Job's context reached Allocate.
func TestPrepareAllocationFollowsTheJobContext(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.sched.allocBlock = true
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := f.prepareOnly(ctx, testJob())
		errc <- err
	}()
	<-f.sched.allocStarted
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Prepare succeeded after the job was cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the job did not abort the allocation")
	}
	if len(f.sched.resolved) != 1 {
		t.Errorf("allocation requested without resolving the profile")
	}
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# If allocation fails, then the Executor SHALL fail the Job with
//# the failure reason `runner_system_failure` and a log line stating the
//# allocation error.

func TestPrepareAllocationFailureIsASystemFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.sched.allocErr = &scheduler.AllocationError{Profile: "builders", Err: scheduler.ErrAllocationTimeout}
	_, trace, err := f.prepareOnly(context.Background(), testJob())
	be := buildError(t, err)
	if be.FailureReason != common.RunnerSystemFailure {
		t.Errorf("failure reason = %s, want runner_system_failure", be.FailureReason)
	}
	if !strings.Contains(trace.String(), scheduler.ErrAllocationTimeout.Error()) {
		t.Errorf("job log does not state the allocation error:\n%s", trace)
	}
}

// advanceUntil advances the fake clock by step whenever n timers are
// waiting, until done is closed.
func advanceUntil(t *testing.T, clk *clock.Fake, n int, step time.Duration, done <-chan struct{}) {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := clk.BlockUntil(ctx, n)
		cancel()
		select {
		case <-done:
			return
		default:
		}
		if err == nil {
			clk.Advance(step)
		}
	}
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# When a MicroVM has been allocated, the Executor SHALL wait until
//# the Guest Transport reports the guest ready or the Profile's ready timeout
//# elapses.

// TestPrepareWaitsForTheGuest has the guest refuse two probes: Prepare keeps
// probing and only runs anything in the guest after the third says ready.
func TestPrepareWaitsForTheGuest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	f.clk = clk
	f.tr.notReady = 2
	done := make(chan struct{})
	var err error
	var trace *recordTrace
	var e *executor
	go func() {
		defer close(done)
		e, trace, err = f.prepareOnly(context.Background(), testJob())
	}()
	advanceUntil(t, clk, 2, readyPollInterval, done)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	if f.tr.readyCalls != 3 {
		t.Errorf("readiness probed %d times, want 3", f.tr.readyCalls)
	}
	if !strings.Contains(trace.String(), "Guest ready in 1s") {
		t.Errorf("job log does not report the time to readiness:\n%s", trace)
	}
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# If the guest is not ready when the ready timeout elapses, then
//# the Executor SHALL release the MicroVM to the Scheduler and fail the Job
//# with the failure reason `runner_system_failure`.

//= docs/requirements/02-executor.md#finish-and-cleanup
//= type=test
//# If `Prepare` fails after a MicroVM was allocated, then the
//# Executor SHALL release that MicroVM to the Scheduler before returning.

// TestPrepareReadyTimeoutReleasesTheMicroVM never lets the guest become
// ready and checks that, by the time Prepare returns and before any
// Cleanup, the MicroVM and the Host lease are handed back.
func TestPrepareReadyTimeoutReleasesTheMicroVM(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	f.clk = clk
	f.tr.notReady = -1
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, _, err = f.prepareOnly(context.Background(), testJob())
	}()
	advanceUntil(t, clk, 2, 10*time.Second, done)

	be := buildError(t, err)
	if be.FailureReason != common.RunnerSystemFailure {
		t.Errorf("failure reason = %s, want runner_system_failure", be.FailureReason)
	}
	if !errors.Is(err, transport.ErrNotReady) {
		t.Errorf("error %v does not carry the readiness failure", err)
	}
	if _, released, _ := f.sched.counts(); released != 1 {
		t.Errorf("microvm released %d times before Prepare returned, want 1", released)
	}
	if n := f.hosts.outstanding(); n != 0 {
		t.Errorf("%d host leases outstanding", n)
	}
	if len(f.tr.all()) != 0 {
		t.Errorf("commands ran in a guest that never became ready")
	}
}

// TestPrepareTransportFailureReleasesTheMicroVM is EX-032 on another path:
// the transport cannot even be built.
func TestPrepareTransportFailureReleasesTheMicroVM(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.factory.err = transport.ErrServiceDisabled
	_, _, err := f.prepareOnly(context.Background(), testJob())
	if buildError(t, err).FailureReason != common.RunnerSystemFailure {
		t.Errorf("error = %v", err)
	}
	if _, released, _ := f.sched.counts(); released != 1 {
		t.Errorf("microvm released %d times, want 1", released)
	}
	if n := f.hosts.outstanding(); n != 0 {
		t.Errorf("%d host leases outstanding", n)
	}
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# When the guest is ready, the Executor SHALL create the Profile's
//# builds directory and cache directory in the guest if they do not exist.

func TestPrepareCreatesTheProfileDirectories(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	recs := f.tr.all()
	if len(recs) != 1 {
		t.Fatalf("%d commands in the guest during Prepare, want 1", len(recs))
	}
	got := recs[0]
	if got.Path != "/usr/bin/bash" || got.Dir != "/" || got.User != "ci" {
		t.Errorf("directory command = %s in %s as %s", got.Path, got.Dir, got.User)
	}
	if want := "mkdir -p -- './srv/builds' './srv/cache'"; !strings.Contains(string(got.Stdin), want) {
		t.Errorf("directory script = %q, want %q", got.Stdin, want)
	}
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# The Executor SHALL complete `Prepare` within the configured
//# prepare timeout or fail the Job.

// TestPrepareTimeout lets the allocation hang: Prepare still returns, as a
// failed Job, once the prepare timeout is up.
func TestPrepareTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.deps.Timeouts.Prepare = 50 * time.Millisecond
	f.sched.allocBlock = true
	errc := make(chan error, 1)
	go func() {
		_, _, err := f.prepareOnly(context.Background(), testJob())
		errc <- err
	}()
	select {
	case err := <-errc:
		if !strings.Contains(err.Error(), "prepare did not complete within 50ms") {
			t.Errorf("error = %v, want the prepare timeout", err)
		}
		buildError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Prepare did not return after the prepare timeout")
	}
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# The Executor SHALL set the shell script configuration so that
//# the builds directory, cache directory and helper binary path used in
//# generated scripts are those of the Profile.

// TestScriptsUseTheProfileDirectoriesAndHelper runs a Job with artifacts and
// a cache through the library's Build. The RunnerConfig names other
// directories, so a script generated from it rather than from the Profile
// shows up.
func TestScriptsUseTheProfileDirectoriesAndHelper(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.deps.CacheConfigured = true
	job := testJob()
	job.Artifacts = spec.Artifacts{{Paths: spec.ArtifactPaths{"out/"}, When: spec.ArtifactWhenOnSuccess}}
	job.Cache = spec.Caches{{Key: "deps", Paths: spec.ArtifactPaths{"vendor/"}, Policy: spec.CachePolicyPullPush, When: spec.CacheWhenOnSuccess}}
	_, b, err := f.runBuild(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if b.RootDir != "/srv/builds" || !strings.HasPrefix(b.CacheDir, "/srv/cache/") {
		t.Errorf("build dirs = %s, %s; want the profile's", b.RootDir, b.CacheDir)
	}
	var sawHelper, sawCache bool
	for _, r := range f.tr.stages() {
		s := string(r.Stdin)
		if strings.Contains(s, "/runner-default-builds") || strings.Contains(s, "/runner-default-cache") {
			t.Errorf("a stage script uses the RunnerConfig's directories:\n%s", s)
		}
		sawHelper = sawHelper || strings.Contains(s, "/opt/helper/gitlab-runner-helper")
		sawCache = sawCache || strings.Contains(s, "/srv/cache/")
	}
	if !stageUses(f.tr.stages(), "/srv/builds/") {
		t.Error("no stage script uses the profile's builds directory")
	}
	if !sawHelper {
		t.Error("no stage script invokes the profile's helper binary")
	}
	if !sawCache {
		t.Error("no stage script uses the profile's cache directory")
	}
}

func stageUses(recs []transport.Recorded, s string) bool {
	for _, r := range recs {
		if strings.Contains(string(r.Stdin), s) {
			return true
		}
	}
	return false
}

//= docs/requirements/02-executor.md#prepare
//= type=test
//# The Executor SHALL write a collapsible section to the Job log
//# during `Prepare` that names the Profile, the Pool, the Host name, the
//# MicroVM uid and the time taken to become ready.

func TestPrepareWritesTheCollapsibleSection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, trace, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	log := trace.String()
	start := strings.Index(log, "section_start:")
	end := strings.Index(log, "section_end:")
	if start < 0 || end < start {
		t.Fatalf("no section in the job log:\n%s", log)
	}
	if !strings.Contains(log[start:], PrepareSection+"[collapsed=true]") {
		t.Errorf("section is not the collapsed %s section:\n%s", PrepareSection, log)
	}
	body := log[start:end]
	for _, want := range []string{`"builders"`, "ns/builders", "host host-a", "vm-a", "Guest ready in"} {
		if !strings.Contains(body, want) {
			t.Errorf("section lacks %q:\n%s", want, body)
		}
	}
}

//= docs/requirements/02-executor.md#run
//= type=test
//# When `Run` is called for a Stage, the Executor SHALL execute the
//# Profile's shell inside the MicroVM through the Guest Transport with the
//# Stage script supplied on standard input.

//= docs/requirements/02-executor.md#run
//= type=test
//# The Executor SHALL run each Stage with the working directory set
//# to the Profile's builds directory.

//= docs/requirements/02-executor.md#run
//= type=test
//# The Executor SHALL run each Stage as the user named by the
//# Profile, defaulting to `root`.

//= docs/requirements/02-executor.md#run
//= type=test
//# The Executor SHALL deliver every Job-specific value to the guest
//# through the Stage scripts sent over the Guest Transport and SHALL NOT place
//# Job-specific values in MicroVM metadata.

// TestStagesRunTheProfileShellWithTheScriptOnStdin runs a whole Job and
// checks every Stage command: the Profile's shell, in the builds directory,
// as the Profile's user, with the script on stdin and nothing of the Job in
// the command's environment. The Job's variable reaches the guest inside
// the scripts and nowhere else; the Scheduler, which alone could put
// anything into a MicroVM, is told only the Job id and image.
func TestStagesRunTheProfileShellWithTheScriptOnStdin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ user, want string }{{"ci", "ci"}, {"", "root"}} {
		t.Run("user="+tc.want, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			p := testProfile()
			p.User = tc.user
			f.sched.profile = p
			_, _, err := f.runBuild(context.Background(), testJob())
			if err != nil {
				t.Fatal(err)
			}
			stages := f.tr.stages()
			if len(stages) < 3 {
				t.Fatalf("%d stages ran, want at least prepare, get_sources and step_script", len(stages))
			}
			for _, r := range stages {
				if r.Path != "/usr/bin/bash" || r.Dir != "/srv/builds" || r.User != tc.want {
					t.Errorf("stage ran %s in %s as %s", r.Path, r.Dir, r.User)
				}
				if len(r.Env) != 0 {
					t.Errorf("stage command carries environment %v", r.Env)
				}
				if len(r.Stdin) == 0 {
					t.Error("stage command has no script on stdin")
				}
			}
			step := stageScript(t, stages, `echo "$JOB_SECRET_VALUE"`)
			if !strings.Contains(string(step.Stdin), "only-in-the-script") {
				t.Error("the job variable is not in the stage script")
			}
			for _, j := range f.sched.resolved {
				if strings.Contains(fmt.Sprint(j), "only-in-the-script") {
					t.Error("a job value reached the scheduler")
				}
			}
		})
	}
}

//= docs/requirements/02-executor.md#run
//= type=test
//# The Executor SHALL stream the standard output and standard
//# error of a Stage to the Job log as they are produced rather than after the
//# Stage completes.

// TestStageOutputIsStreamed has the guest write a line and then wait until
// that line is in the Job log before it exits. An executor that forwarded
// output only after the Stage completed would never let it exit.
func TestStageOutputIsStreamed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, trace, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	f.tr.hook = func(ctx context.Context, cmd transport.Command, stdin []byte) (int, error) {
		_, _ = cmd.Stdout.Write([]byte("first line out\n"))
		_, _ = cmd.Stderr.Write([]byte("first line err\n"))
		deadline := time.After(5 * time.Second)
		for !strings.Contains(trace.String(), "first line out") || !strings.Contains(trace.String(), "first line err") {
			select {
			case <-deadline:
				return -1, errors.New("output not in the job log while the stage runs")
			case <-time.After(5 * time.Millisecond):
			}
		}
		return 0, nil
	}
	if err := e.Run(common.ExecutorCommand{Script: "true\n", Stage: common.BuildStagePrepare, Context: context.Background()}); err != nil {
		t.Fatal(err)
	}
}

//= docs/requirements/02-executor.md#run
//= type=test
//# When a Stage's command exits with a non-zero status, the
//# Executor SHALL return a build error carrying that exit code so that the
//# Runner reports it to GitLab.

func TestNonZeroExitIsAScriptFailureWithTheCode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	f.tr.runs = []transport.Scripted{{ExitStatus: 3}}
	be := buildError(t, e.Run(common.ExecutorCommand{Script: "exit 3\n", Stage: "step_script", Context: context.Background()}))
	if be.FailureReason != common.ScriptFailure || be.ExitCode != 3 {
		t.Errorf("error = %+v, want script_failure with exit code 3", be)
	}
}

//= docs/requirements/02-executor.md#run
//= type=test
//# If the Guest Transport stream fails before the Stage's exit
//# status is known, then the Executor SHALL return a system error and SHALL
//# NOT re-run the Stage.

// TestStreamFailureIsASystemErrorAndIsNotRerun fails the stream once and
// asks for the same Stage again, as the Build's stage retries would: the
// second attempt must not reach the guest.
func TestStreamFailureIsASystemErrorAndIsNotRerun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	f.tr.runs = []transport.Scripted{{Err: fmt.Errorf("host-a: %w", transport.ErrStreamFailed)}}
	cmd := common.ExecutorCommand{Script: "make\n", Stage: common.BuildStageGetSources, Context: context.Background()}
	be := buildError(t, e.Run(cmd))
	if be.FailureReason != common.RunnerSystemFailure || !errors.Is(be, transport.ErrStreamFailed) {
		t.Errorf("error = %v (%s), want a runner_system_failure wrapping the stream failure", be, be.FailureReason)
	}
	again := e.Run(cmd)
	if again == nil {
		t.Fatal("the failed stage ran again and succeeded")
	}
	if n := len(f.tr.stages()); n != 1 {
		t.Errorf("the stage reached the guest %d times, want 1", n)
	}
}

//= docs/requirements/02-executor.md#run
//= type=test
//# When the Job's context is cancelled while a Stage is running,
//# the Executor SHALL terminate the Stage's process in the guest and return
//# within the configured graceful kill timeout.

// TestCancelTerminatesTheStageWithinTheGracefulKillTimeout runs a Stage
// whose guest process ignores the first termination request. Cancelling the
// Job has to cancel the transport operation, which is what terminates the
// process in the guest, and Run has to return once the graceful kill
// timeout has passed on the clock even though the transport has not.
func TestCancelTerminatesTheStageWithinTheGracefulKillTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	f.clk = clk
	f.deps.Timeouts.GracefulKill = 7 * time.Second
	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()

	terminated := make(chan struct{})
	release := make(chan struct{})
	f.tr.hook = func(ctx context.Context, _ transport.Command, _ []byte) (int, error) {
		<-ctx.Done()
		close(terminated)
		<-release // the guest process is slow to die
		return -1, ctx.Err()
	}
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- e.Run(common.ExecutorCommand{Script: "sleep 600\n", Stage: "step_script", Context: ctx})
	}()
	cancel()
	select {
	case <-terminated:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the job did not cancel the guest operation")
	}
	if err := clk.BlockUntil(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(7*time.Second - time.Millisecond)
	select {
	case err := <-errc:
		t.Fatalf("Run returned %v before the graceful kill timeout", err)
	case <-time.After(200 * time.Millisecond):
	}
	clk.Advance(time.Millisecond)
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run error = %v, want the job's cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the graceful kill timeout")
	}
}

//= docs/requirements/03-scheduler.md#host-health
//= type=test
//# When a Host becomes unhealthy, the Scheduler SHALL abort every
//# Job whose MicroVM is placed on that Host with the failure reason
//# `runner_system_failure` and release their Leases.

//= docs/requirements/03-scheduler.md#lease-keep-alive
//= type=test
//# If a heartbeat reports that the Lease no longer exists, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and drop the Allocation without a release call.

// TestHandleDoneAbortsTheJobAsARunnerSystemFailure runs a whole Job whose
// script hangs and closes the Allocation's Handle mid-Stage, as the
// Scheduler does when the Host goes unhealthy or the Lease is lost. The Job
// must end as runner_system_failure, not job_canceled, and the MicroVM must
// still be handed back to the Scheduler, which is what releases the Lease
// (or, for a lost Lease, drops it without a call).
func TestHandleDoneAbortsTheJobAsARunnerSystemFailure(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{scheduler.ErrHostUnhealthy, scheduler.ErrLeaseLost} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.tr.hook = func(ctx context.Context, cmd transport.Command, stdin []byte) (int, error) {
				if !strings.Contains(string(stdin), "hang-here") {
					return 0, nil
				}
				f.sched.mu.Lock()
				h := f.sched.handles[0]
				f.sched.mu.Unlock()
				h.err = cause
				close(h.done)
				<-ctx.Done()
				return -1, ctx.Err()
			}
			job := testJob()
			job.Steps[0].Script = spec.StepScript{"echo hang-here"}
			_, _, err := f.runBuild(context.Background(), job)
			be := buildError(t, err)
			if be.FailureReason != common.RunnerSystemFailure {
				t.Errorf("failure reason = %s, want runner_system_failure", be.FailureReason)
			}
			if !errors.Is(err, cause) {
				t.Errorf("error %v does not carry %v", err, cause)
			}
			if _, released, _ := f.sched.counts(); released != 1 {
				t.Errorf("microvm handed back %d times, want 1", released)
			}
		})
	}
}

//= docs/requirements/03-scheduler.md#host-health
//= type=test
//# When a Host becomes unhealthy, the Scheduler SHALL abort every
//# Job whose MicroVM is placed on that Host with the failure reason
//# `runner_system_failure` and release their Leases.

// TestHandleDoneAbortsAStageBeforeTheBuildContextExists closes the Handle
// while a Stage runs under a context that knows nothing of it, which is the
// prepare_script Stage's position: the Build derives its context from the
// Handle only for the Stages after it. Run itself has to stop the Stage
// with a runner_system_failure.
func TestHandleDoneAbortsAStageBeforeTheBuildContextExists(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	f.tr.hook = func(ctx context.Context, _ transport.Command, _ []byte) (int, error) {
		h := f.sched.handles[0]
		h.err = scheduler.ErrHostUnhealthy
		close(h.done)
		<-ctx.Done()
		return -1, ctx.Err()
	}
	errc := make(chan error, 1)
	go func() {
		errc <- e.Run(common.ExecutorCommand{Script: "true\n", Stage: common.BuildStagePrepare, Context: context.Background()})
	}()
	select {
	case err := <-errc:
		be := buildError(t, err)
		if be.FailureReason != common.RunnerSystemFailure || !errors.Is(err, scheduler.ErrHostUnhealthy) {
			t.Errorf("error = %v (%s), want runner_system_failure from the unhealthy host", err, be.FailureReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stage kept running after the handle was done")
	}
}

//= docs/requirements/02-executor.md#finish-and-cleanup
//= type=test
//# When `Cleanup` is called, the Executor SHALL release the Job's
//# MicroVM to the Scheduler regardless of whether the Job succeeded, failed or
//# was cancelled.

func TestCleanupReleasesTheMicroVMWhateverTheOutcome(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		runs   []transport.Scripted
		cancel bool
	}{
		"succeeded": {},
		"failed":    {runs: []transport.Scripted{{}, {}, {ExitStatus: 1}}},
		"cancelled": {runs: []transport.Scripted{{}, {}, {Delay: time.Hour}}, cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.tr.runs = tc.runs
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() {
					for len(f.tr.stages()) < 3 {
						time.Sleep(time.Millisecond)
					}
					cancel()
				}()
			}
			_, _, _ = f.runBuild(ctx, testJob())
			if _, released, retained := f.sched.counts(); released != 1 || retained != 0 {
				t.Errorf("released %d, retained %d; want the microvm released once", released, retained)
			}
			if n := f.hosts.outstanding(); n != 0 {
				t.Errorf("%d host leases outstanding after cleanup", n)
			}
		})
	}
}

//= docs/requirements/02-executor.md#finish-and-cleanup
//= type=test
//# The Executor SHALL NOT run a second Job in a MicroVM that has
//# run a Job.

// TestAMicroVMRunsOneJob runs two Jobs back to back: each gets its own
// allocation and its MicroVM is released after it, and an executor that has
// prepared once refuses to prepare again.
func TestAMicroVMRunsOneJob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for range 2 {
		if _, _, err := f.runBuild(context.Background(), testJob()); err != nil {
			t.Fatal(err)
		}
	}
	allocs, released, _ := f.sched.counts()
	if allocs != 2 || released != 2 {
		t.Fatalf("allocations %d, releases %d; want one of each per job", allocs, released)
	}
	if f.sched.handles[0] == f.sched.handles[1] {
		t.Error("two jobs got the same microvm")
	}

	e, _, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	again := e.Prepare(common.ExecutorPrepareOptions{
		Config: runnerConfig(), Build: e.Build, Context: context.Background(),
		BuildLogger: buildlogger.New(newRecordTrace(), discardEntry(), buildlogger.Options{}),
	})
	if !errors.Is(again, ErrPrepared) {
		t.Errorf("second Prepare = %v, want ErrPrepared", again)
	}
	if allocs, _, _ := f.sched.counts(); allocs != 3 {
		t.Errorf("a second Prepare allocated again (%d allocations)", allocs)
	}
}

//= docs/requirements/02-executor.md#finish-and-cleanup
//= type=test
//# Where the keep-on-failure debug option is enabled, the Executor
//# SHALL ask the Scheduler to retain the MicroVM of a failed Job instead of
//# releasing it and SHALL write the MicroVM uid and Host name to the Job log.

func TestKeepOnFailureRetainsAFailedJobsMicroVM(t *testing.T) {
	t.Parallel()
	for name, failing := range map[string]bool{"failed": true, "succeeded": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.deps.KeepOnFailure = true
			if failing {
				f.tr.runs = []transport.Scripted{{}, {}, {ExitStatus: 2}}
			}
			trace, _, _ := f.runBuild(context.Background(), testJob())
			_, released, retained := f.sched.counts()
			if failing {
				if retained != 1 || released != 0 {
					t.Errorf("retained %d, released %d; want the failed job's microvm retained", retained, released)
				}
				found := false
				for _, line := range strings.Split(trace.String(), "\n") {
					if strings.Contains(line, "keep_on_failure") && strings.Contains(line, "vm-a") && strings.Contains(line, "host-a") {
						found = true
					}
				}
				if !found {
					t.Errorf("job log has no keep-on-failure line naming the microvm and host:\n%s", trace)
				}
				return
			}
			if retained != 0 || released != 1 {
				t.Errorf("retained %d, released %d; want a successful job's microvm released", retained, released)
			}
		})
	}
}

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//= type=test
//# If a Job declares services, then the Runner SHALL fail the Job
//# with the failure reason `runner_unsupported` and a log line stating that
//# services are not supported by this executor.

func TestServicesAreRunnerUnsupported(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	job := testJob()
	job.Services = spec.Services{{Name: "postgres:16"}}
	trace, _, err := f.runBuild(context.Background(), job)
	if be := buildError(t, err); be.FailureReason != RunnerUnsupported {
		t.Errorf("failure reason = %s, want runner_unsupported", be.FailureReason)
	}
	if !strings.Contains(trace.String(), "services are not supported by the flintlock executor") {
		t.Errorf("job log does not say services are unsupported:\n%s", trace)
	}
	if allocs, _, _ := f.sched.counts(); allocs != 0 {
		t.Error("a microvm was allocated for a job with services")
	}
}

// TestCacheWithoutDistributedCacheIsRunnerUnsupported is the failing half of
// CF-083.
func TestCacheWithoutDistributedCacheIsRunnerUnsupported(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	job := testJob()
	job.Cache = spec.Caches{{Key: "deps", Paths: spec.ArtifactPaths{"vendor/"}}}
	_, _, err := f.runBuild(context.Background(), job)
	if be := buildError(t, err); be.FailureReason != RunnerUnsupported {
		t.Errorf("failure reason = %s, want runner_unsupported", be.FailureReason)
	}
}
