package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/buildlogger"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"gitlab.com/gitlab-org/gitlab-runner/executors"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// readyPollInterval is how long the Executor waits between readiness probes
// of a guest that is not ready yet (EX-014).
const readyPollInterval = 500 * time.Millisecond

// ErrPrepared is the cause when Prepare is called a second time on one
// executor: a MicroVM that has run a Job never runs another (EX-031).
var ErrPrepared = errors.New("executor: already prepared; a microvm runs one job")

// executor runs one Job in one MicroVM. The embedded AbstractExecutor holds
// the gitlab-runner half: the Build, its logger, the stage bookkeeping and
// the shell configuration the Stage scripts are generated from.
type executor struct {
	executors.AbstractExecutor

	p *provider

	// Set by Prepare.
	data      *Data
	profile   *scheduler.Profile
	handle    scheduler.Handle
	tr        transport.Transport
	hostDone  func()
	prepared  bool
	jobFailed bool

	// mu guards failedStages, which Run reads and writes; the run loop
	// calls Run from one goroutine at a time but the race detector cannot
	// know that.
	mu           sync.Mutex
	failedStages map[common.BuildStage]error
}

// buildErr is a BuildError with a reason and a formatted cause.
func buildErr(reason spec.JobFailureReason, format string, args ...any) error {
	return &common.BuildError{Inner: fmt.Errorf(format, args...), FailureReason: reason}
}

// Prepare implements common.Executor. It resolves the Job's Profile,
// allocates a MicroVM from its Pool, waits for the guest, creates the
// Profile's directories there, points the shell configuration at them and
// adds the Host Service environment, writing the flintlock_prepare section
// as it goes. Every error it returns is a *common.BuildError, because the
// run loop retries a Prepare that fails with any other error, and a retry
// would allocate a second MicroVM for the same Job.
func (e *executor) Prepare(options common.ExecutorPrepareOptions) (err error) {
	e.PrepareConfiguration(options)

	sec := newPrepareSection(&e.BuildLogger, e.p.clk)
	defer func() {
		if err != nil {
			sec.errorf("ERROR: %v", err)
		}
		sec.end()
	}()

	if e.prepared {
		return &common.BuildError{Inner: ErrPrepared, FailureReason: common.RunnerSystemFailure}
	}
	e.prepared = true

	data, ok := options.Build.ExecutorData.(*Data)
	if !ok || data == nil || data.Reservation == nil {
		return buildErr(common.RunnerSystemFailure, "flintlock: the run loop gave Prepare no reservation")
	}
	e.data = data

	if err := e.checkJob(); err != nil {
		return err
	}

	//= docs/requirements/02-executor.md#prepare
	//# The Executor SHALL complete `Prepare` within the configured
	//# prepare timeout or fail the Job.
	ctx := options.Context
	if d := e.p.deps.Timeouts.Prepare; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, d,
			fmt.Errorf("flintlock: prepare did not complete within %s", d))
		defer cancel()
	}
	// A second termination signal aborts a Job still being prepared too.
	ctx, abortPrepare := context.WithCancelCause(ctx)
	defer abortPrepare(nil)
	go func() {
		select {
		case <-e.p.abort:
			abortPrepare(ErrAborted)
		case <-ctx.Done():
		}
	}()

	if err := e.resolveProfile(sec); err != nil {
		return err
	}
	if err := e.configureShell(); err != nil {
		return err
	}
	if err := e.allocate(ctx, sec); err != nil {
		return prepareErr(ctx, err)
	}

	// From here on the Job holds a MicroVM, and a failure hands it back
	// before returning.
	if err := e.startGuest(ctx, sec); err != nil {
		e.releaseAfterFailedPrepare()
		return prepareErr(ctx, err)
	}
	e.addHostServiceEnv(sec)
	return nil
}

// prepareErr turns an error from a step of Prepare into the error Prepare
// returns: when the prepare timeout is what stopped the step, the timeout
// is the cause the Job log names.
func prepareErr(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
		var be *common.BuildError
		if errors.As(err, &be) {
			be.Inner = fmt.Errorf("%w (%w)", be.Inner, cause)
			return be
		}
	}
	return err
}

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//# If a Job declares services, then the Runner SHALL fail the Job
//# with the failure reason `runner_unsupported` and a log line stating that
//# services are not supported by this executor.

// checkJob refuses a Job that asks for something no MicroVM provides, before
// any MicroVM is claimed for it: services (GL-024), and `cache:` when the
// Distributed cache is not configured (CF-083).
func (e *executor) checkJob() error {
	if len(e.Build.Services) > 0 {
		return buildErr(RunnerUnsupported,
			"services are not supported by the flintlock executor; start auxiliary services from the job script")
	}
	if !e.p.deps.CacheConfigured && usesCache(e.Build) {
		return buildErr(RunnerUnsupported,
			"the job uses cache: but this runner has no distributed_cache configured")
	}
	return nil
}

// usesCache reports whether the Job declares a cache with anything in it.
func usesCache(b *common.Build) bool {
	for _, c := range b.Cache {
		if c.Key != "" || len(c.Paths) > 0 || c.Untracked {
			return true
		}
	}
	return false
}

//= docs/requirements/02-executor.md#prepare
//# When `Prepare` is called, the Executor SHALL ask the Scheduler to
//# resolve a Profile for the Job.

//= docs/requirements/02-executor.md#prepare
//# If no Profile can be resolved for the Job, then the Executor
//# SHALL fail the Job with the failure reason `runner_unsupported` and a log
//# line naming the requested Job Image.

// resolveProfile asks the Scheduler which Profile the Job runs in.
func (e *executor) resolveProfile(sec *prepareSection) error {
	image := e.ExpandValue(e.Build.Image.Name)
	p, err := e.p.deps.Scheduler.ResolveProfile(e.jobInfo())
	if err != nil {
		if image == "" {
			return buildErr(RunnerUnsupported, "no profile for a job without an image: %v", err)
		}
		return buildErr(RunnerUnsupported, "no profile for job image %q: %v", image, err)
	}
	e.profile = p
	sec.report.Profile = p.Name
	sec.report.Pool = p.PoolRef.String()
	sec.printf("Using flintlock profile %q (pool %s)", p.Name, p.PoolRef)
	return nil
}

// jobInfo is the Scheduler's view of the Job.
func (e *executor) jobInfo() scheduler.JobInfo {
	return scheduler.JobInfo{ID: e.Build.ID, Image: e.ExpandValue(e.Build.Image.Name)}
}

//= docs/requirements/02-executor.md#prepare
//# The Executor SHALL set the shell script configuration so that
//# the builds directory, cache directory and helper binary path used in
//# generated scripts are those of the Profile.

// configureShell points the Build and the shell configuration at the
// Profile: the builds and cache directories every generated script uses, and
// the gitlab-runner-helper path the artifact and cache scripts invoke. The
// RunnerConfig carries the Default Profile's directories (runnercfg.Build);
// the per-Job copy in the AbstractExecutor is overwritten with the resolved
// Profile's before the Build computes its paths from it.
func (e *executor) configureShell() error {
	e.Config.BuildsDir = e.profile.BuildsDir
	e.Config.CacheDir = e.profile.CacheDir
	// The RunnerConfig's shell would override the executor's; the guest
	// runs the Profile's bash, so the scripts are always bash (GL-041).
	e.Config.Shell = DefaultShell
	e.Shell().RunnerCommand = e.profile.HelperPath
	if err := e.PrepareBuildAndShell(); err != nil {
		var be *common.BuildError
		if errors.As(err, &be) {
			return err
		}
		return &common.BuildError{Inner: err, FailureReason: common.ConfigurationError}
	}
	return nil
}

//= docs/requirements/02-executor.md#prepare
//# When a Profile has been resolved, the Executor SHALL ask the
//# Scheduler to allocate a MicroVM for the Job and the Profile, passing the
//# Job's context so that cancellation aborts the allocation.

//= docs/requirements/02-executor.md#prepare
//# If allocation fails, then the Executor SHALL fail the Job with
//# the failure reason `runner_system_failure` and a log line stating the
//# allocation error.

// allocate converts the Reservation into an Allocation. The context is the
// Job's, bounded by the prepare timeout, so a cancelled Job stops waiting
// for a warm MicroVM.
func (e *executor) allocate(ctx context.Context, sec *prepareSection) error {
	start := e.p.clk.Now()
	h, err := e.p.deps.Scheduler.Allocate(ctx, e.data.Reservation, e.jobInfo(), e.profile)
	if err != nil {
		return buildErr(common.RunnerSystemFailure, "allocating a microvm: %w", err)
	}
	e.handle = h
	e.data.Handle = h
	a := h.Allocation()
	sec.report.HostName = a.Placement.Host
	sec.report.MicroVMUID = a.VMUID
	sec.report.Source = a.Placement.Source
	sec.report.AllocatedIn = e.p.clk.Now().Sub(start)
	sec.printf("Allocated microvm %s on host %s in %s", a.VMUID, a.Placement.Host, roundDuration(sec.report.AllocatedIn))
	return nil
}

// startGuest connects to the MicroVM, waits for it and creates the
// Profile's directories in it.
func (e *executor) startGuest(ctx context.Context, sec *prepareSection) error {
	a := e.handle.Allocation()
	client, done, err := e.p.deps.Hosts.Lease(a.Placement.Host)
	if err != nil {
		return buildErr(common.RunnerSystemFailure, "host %s: %w", a.Placement.Host, err)
	}
	e.hostDone = done

	target := transport.Target{
		Kind:     transport.Kind(e.profile.Transport.Kind),
		Host:     client,
		VMUID:    a.VMUID,
		Deadline: e.p.deps.Timeouts.Transport,
	}
	if target.Kind == transport.KindSSH {
		ssh, err := sshOptions(e.profile)
		if err != nil {
			return buildErr(common.ConfigurationError, "profile %s: %w", e.profile.Name, err)
		}
		target.SSH = ssh
	}
	tr, err := e.p.deps.Transports.New(ctx, target)
	if err != nil {
		return buildErr(common.RunnerSystemFailure, "guest transport to microvm %s on host %s: %w", a.VMUID, a.Placement.Host, err)
	}
	e.tr = tr

	start := e.p.clk.Now()
	if err := e.waitReady(ctx); err != nil {
		return err
	}
	sec.report.ReadyIn = e.p.clk.Now().Sub(start)
	sec.printf("Guest ready in %s", roundDuration(sec.report.ReadyIn))

	return e.makeDirs(ctx)
}

// sshOptions loads the ssh transport's settings from the Profile (EX-049).
// The key is read here so that the transport never touches the filesystem.
func sshOptions(p *scheduler.Profile) (transport.SSHOptions, error) {
	cfg := p.Transport.SSH
	key, err := os.ReadFile(cfg.PrivateKeyFile)
	if err != nil {
		return transport.SSHOptions{}, fmt.Errorf("reading ssh private key: %w", err)
	}
	user := cfg.User
	if user == "" {
		user = p.User
	}
	return transport.SSHOptions{User: user, PrivateKey: key, KnownHostKey: cfg.KnownHostKey}, nil
}

//= docs/requirements/02-executor.md#prepare
//# When a MicroVM has been allocated, the Executor SHALL wait until
//# the Guest Transport reports the guest ready or the Profile's ready timeout
//# elapses.

//= docs/requirements/02-executor.md#prepare
//# If the guest is not ready when the ready timeout elapses, then
//# the Executor SHALL release the MicroVM to the Scheduler and fail the Job
//# with the failure reason `runner_system_failure`.

// waitReady probes the guest until the transport says it is ready, the
// Profile's ready timeout elapses or ctx ends. The release on failure is
// Prepare's (releaseAfterFailedPrepare), which every error path here
// reaches.
func (e *executor) waitReady(ctx context.Context) error {
	timeout := e.profile.ReadyTimeout
	if timeout <= 0 {
		timeout = config.DefaultReadyTimeout
	}
	deadline := e.p.clk.NewTimer(timeout)
	defer deadline.Stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-deadline.C():
			cancel()
		case <-ctx.Done():
		}
	}()

	var last error
	for {
		last = e.tr.Ready(ctx)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		wait := e.p.clk.NewTimer(readyPollInterval)
		select {
		case <-wait.C():
		case <-ctx.Done():
		}
		wait.Stop()
		if ctx.Err() != nil {
			break
		}
	}
	return buildErr(common.RunnerSystemFailure,
		"microvm %s was not ready within the ready timeout of %s: %w", e.handle.Allocation().VMUID, timeout, last)
}

//= docs/requirements/02-executor.md#prepare
//# When the guest is ready, the Executor SHALL create the Profile's
//# builds directory and cache directory in the guest if they do not exist.

// makeDirs creates the builds and cache directories in the guest with the
// Profile's shell. The command runs from the guest's root and names the
// directories relative to it, which on a MicroVM is the same absolute path,
// and on the fake Host, whose root is a sandbox directory, is the sandbox's
// copy that every Stage's working directory (EX-025) resolves to.
func (e *executor) makeDirs(ctx context.Context) error {
	script := fmt.Sprintf("mkdir -p -- %s %s\n", relToRoot(e.profile.BuildsDir), relToRoot(e.profile.CacheDir))
	var stderr strings.Builder
	status, err := e.tr.Run(ctx, transport.Command{
		Path:   e.shell(),
		Dir:    "/",
		User:   e.user(),
		Stdin:  strings.NewReader(script),
		Stderr: &stderr,
	})
	if err != nil {
		return buildErr(common.RunnerSystemFailure, "creating the builds and cache directories: %w", err)
	}
	if status != 0 {
		return buildErr(common.RunnerSystemFailure, "creating the builds and cache directories exited %d: %s",
			status, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// relToRoot quotes dir for a shell as a path relative to "/".
func relToRoot(dir string) string {
	rel := "." + "/" + strings.TrimLeft(dir, "/")
	return "'" + strings.ReplaceAll(rel, "'", `'\''`) + "'"
}

//= docs/requirements/02-executor.md#finish-and-cleanup
//# If `Prepare` fails after a MicroVM was allocated, then the
//# Executor SHALL release that MicroVM to the Scheduler before returning.

// releaseAfterFailedPrepare hands the MicroVM back when Prepare fails after
// the allocation, so the Job does not keep it until Cleanup.
func (e *executor) releaseAfterFailedPrepare() {
	e.closeGuest()
	if e.handle != nil {
		e.p.deps.Scheduler.Release(e.handle)
		e.handle = nil
	}
}

// closeGuest closes the transport and returns the Host lease.
func (e *executor) closeGuest() {
	if e.tr != nil {
		_ = e.tr.Close()
		e.tr = nil
	}
	if e.hostDone != nil {
		e.hostDone()
		e.hostDone = nil
	}
}

//= docs/requirements/02-executor.md#run
//# When `Run` is called for a Stage, the Executor SHALL execute the
//# Profile's shell inside the MicroVM through the Guest Transport with the
//# Stage script supplied on standard input.

//= docs/requirements/02-executor.md#run
//# The Executor SHALL run each Stage with the working directory set
//# to the Profile's builds directory.

//= docs/requirements/02-executor.md#run
//# The Executor SHALL run each Stage as the user named by the
//# Profile, defaulting to `root`.

//= docs/requirements/02-executor.md#run
//# The Executor SHALL deliver every Job-specific value to the guest
//# through the Stage scripts sent over the Guest Transport and SHALL NOT place
//# Job-specific values in MicroVM metadata.

// Run implements common.Executor: one Stage, one transport operation. The
// command is the Profile's shell with the generated script on standard
// input, run from the Profile's builds directory as the Profile's user.
// The Command carries no environment: every Job value, variables included,
// is inside the script, and nothing of the Job reaches the MicroVM any other
// way (the MicroVM was booted from its Pool's template before the Job
// existed).
func (e *executor) Run(cmd common.ExecutorCommand) error {
	if e.tr == nil || e.handle == nil {
		return buildErr(common.RunnerSystemFailure, "flintlock: Run called before a successful Prepare")
	}
	if err := e.stageFailed(cmd.Stage); err != nil {
		return err
	}

	stdout := e.BuildLogger.Stream(buildlogger.StreamWorkLevel, buildlogger.Stdout)
	defer stdout.Close()
	stderr := e.BuildLogger.Stream(buildlogger.StreamWorkLevel, buildlogger.Stderr)
	defer stderr.Close()

	command := transport.Command{
		Path:   e.shell(),
		Args:   e.BuildShell.Arguments,
		Dir:    e.profile.BuildsDir,
		User:   e.user(),
		Stdin:  strings.NewReader(cmd.Script),
		Stdout: stdout,
		Stderr: stderr,
	}
	return e.runStage(cmd, command)
}

// stageResult is what the transport returned for one Stage.
type stageResult struct {
	status int
	err    error
}

//= docs/requirements/02-executor.md#run
//# The Executor SHALL stream the standard output and standard
//# error of a Stage to the Job log as they are produced rather than after the
//# Stage completes.

//= docs/requirements/02-executor.md#run
//# When the Job's context is cancelled while a Stage is running,
//# the Executor SHALL terminate the Stage's process in the guest and return
//# within the configured graceful kill timeout.

//= docs/requirements/03-scheduler.md#host-health
//# When a Host becomes unhealthy, the Scheduler SHALL abort every
//# Job whose MicroVM is placed on that Host with the failure reason
//# `runner_system_failure` and release their Leases.

//= docs/requirements/03-scheduler.md#lease-keep-alive
//# If a heartbeat reports that the Lease no longer exists, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and drop the Allocation without a release call.

// runStage runs the command and waits for it, the Job's context or the
// Allocation's Handle, whichever ends first. The transport writes output
// to the Job log's streams as it arrives. Cancelling the Job cancels the
// operation's context, which makes the transport terminate the process in
// the guest; Run then waits at most the graceful kill timeout for the
// transport to report back. A Handle that is done, because the Scheduler
// lost the Lease or the Host went unhealthy, aborts the Stage the same way
// but with a runner_system_failure BuildError, so that GitLab is told it
// was the Runner, not the Job, that failed (Data.WithContext does the same
// for the Build's context once Prepare has returned; the prepare_script
// Stage runs before that, so the Handle is watched here too).
func (e *executor) runStage(cmd common.ExecutorCommand, command transport.Command) error {
	ctx, cancel := context.WithCancelCause(cmd.Context)
	defer cancel(nil)

	h := e.handle
	go func() {
		select {
		case <-h.Done():
			cancel(&common.BuildError{Inner: h.Err(), FailureReason: common.RunnerSystemFailure})
		case <-e.p.abort:
			cancel(&common.BuildError{Inner: ErrAborted, FailureReason: common.RunnerSystemFailure})
		case <-ctx.Done():
		}
	}()

	result := make(chan stageResult, 1)
	go func() {
		status, err := e.tr.Run(ctx, command)
		result <- stageResult{status: status, err: err}
	}()

	select {
	case r := <-result:
		if ctx.Err() != nil {
			return stageCancelled(ctx)
		}
		return e.stageError(cmd.Stage, r)
	case <-ctx.Done():
	}

	grace := e.p.deps.Timeouts.GracefulKill
	if grace <= 0 {
		grace = config.DefaultGracefulKillTimeout
	}
	timer := e.p.clk.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-result:
	case <-timer.C():
		e.BuildLogger.Warningln(fmt.Sprintf("The %s stage did not stop within the graceful kill timeout of %s", cmd.Stage, grace))
	}
	return stageCancelled(ctx)
}

// stageCancelled is the error of a Stage stopped by its context: the
// BuildError cause when the Handle stopped it, the context's own cause
// otherwise, which the Build maps to job_canceled or job_execution_timeout.
func stageCancelled(ctx context.Context) error {
	cause := context.Cause(ctx)
	var be *common.BuildError
	if errors.As(cause, &be) {
		return be
	}
	return cause
}

//= docs/requirements/02-executor.md#run
//# When a Stage's command exits with a non-zero status, the
//# Executor SHALL return a build error carrying that exit code so that the
//# Runner reports it to GitLab.

//= docs/requirements/02-executor.md#run
//# If the Guest Transport stream fails before the Stage's exit
//# status is known, then the Executor SHALL return a system error and SHALL
//# NOT re-run the Stage.

// stageError maps the transport's answer onto the Build's error model: a
// non-zero status is a script failure carrying the status, an error is a
// system failure. The transport is called once per Run and never again for
// the same Stage after it failed that way: a Stage that dies with its exit
// status unknown may have had side effects, so running it again is not
// safe, and the Build's own stage retries (GET_SOURCES_ATTEMPTS and the
// like) are refused here.
func (e *executor) stageError(stage common.BuildStage, r stageResult) error {
	if r.err != nil {
		err := &common.BuildError{
			Inner:         fmt.Errorf("stage %s: guest transport failed before the exit status was known: %w", stage, r.err),
			FailureReason: common.RunnerSystemFailure,
		}
		e.mu.Lock()
		if e.failedStages == nil {
			e.failedStages = make(map[common.BuildStage]error)
		}
		e.failedStages[stage] = err
		e.mu.Unlock()
		return err
	}
	if r.status != 0 {
		return &common.BuildError{
			Inner:         fmt.Errorf("exit status %d", r.status),
			ExitCode:      common.NormalizeExitCode(r.status),
			FailureReason: common.ScriptFailure,
		}
	}
	return nil
}

// stageFailed returns the error of an earlier transport failure of stage.
func (e *executor) stageFailed(stage common.BuildStage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failedStages[stage]
}

//= docs/requirements/02-executor.md#finish-and-cleanup
//# Where the keep-on-failure debug option is enabled, the Executor
//# SHALL ask the Scheduler to retain the MicroVM of a failed Job instead of
//# releasing it and SHALL write the MicroVM uid and Host name to the Job log.

// Finish implements common.Executor. It records whether the Job failed, for
// Cleanup's keep-on-failure decision, and while the Job log is still open
// writes where a retained MicroVM can be found.
func (e *executor) Finish(err error) {
	e.jobFailed = err != nil
	if e.jobFailed && e.p.deps.KeepOnFailure && e.handle != nil {
		a := e.handle.Allocation()
		e.BuildLogger.Warningln(fmt.Sprintf(
			"keep_on_failure: keeping microvm %s on host %s for debugging", a.VMUID, a.Placement.Host))
	}
	e.AbstractExecutor.Finish(err)
}

//= docs/requirements/02-executor.md#finish-and-cleanup
//# When `Cleanup` is called, the Executor SHALL release the Job's
//# MicroVM to the Scheduler regardless of whether the Job succeeded, failed or
//# was cancelled.

//= docs/requirements/02-executor.md#finish-and-cleanup
//# The Executor SHALL NOT run a second Job in a MicroVM that has
//# run a Job.

// Cleanup implements common.Executor. The Build defers it, so it runs on
// every path out of a Build that got past Prepare: success, failure,
// cancellation, timeout. It hands the MicroVM back to the Scheduler, which
// releases the Lease and the Pool Manager deletes the MicroVM, so no MicroVM
// outlives its one Job; with keep-on-failure a failed Job's MicroVM is
// retained for the keep duration instead. The executor forgets the Handle,
// and Prepare refuses a second call, so the MicroVM cannot be reached
// through this executor again.
func (e *executor) Cleanup() {
	e.closeGuest()
	if h := e.handle; h != nil {
		e.handle = nil
		if e.jobFailed && e.p.deps.KeepOnFailure {
			e.p.deps.Scheduler.Retain(h)
		} else {
			e.p.deps.Scheduler.Release(h)
		}
	}
	e.AbstractExecutor.Cleanup()
}

// shell is the Profile's shell path, defaulting as the configuration does.
func (e *executor) shell() string {
	if e.profile.Shell != "" {
		return e.profile.Shell
	}
	return config.DefaultShell
}

// user is the Profile's guest user, defaulting to root (EX-026).
func (e *executor) user() string {
	if e.profile.User != "" {
		return e.profile.User
	}
	return config.DefaultUser
}

// roundDuration rounds d for the Job log.
func roundDuration(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(100 * time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(time.Millisecond)
	default:
		return d
	}
}
