package executor

import (
	"context"
	"errors"
	"testing"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/executors"

	"github.com/phoban01/flintlock-runner/internal/config/runnercfg"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
)

//= docs/requirements/02-executor.md#interface
//= type=test
//# The Executor SHALL implement the `ExecutorProvider` and
//# `Executor` interfaces of the gitlab-runner `common` package.

// TestProviderAndExecutorImplementTheLibraryInterfaces checks that the value
// NewProvider returns is accepted by gitlab-runner's own provider validation
// and creates a common.Executor.
func TestProviderAndExecutorImplementTheLibraryInterfaces(t *testing.T) {
	t.Parallel()
	p := newFixture(t).build()
	var ep common.ExecutorProvider = p
	if err := common.ValidateExecutorProvider(ep); err != nil {
		t.Fatalf("gitlab-runner rejects the provider: %v", err)
	}
	ex := ep.Create()
	if ex == nil {
		t.Fatal("Create returned nil")
	}
	if _, ok := ep.(common.ManagedExecutorProvider); !ok {
		t.Error("the provider is not a ManagedExecutorProvider")
	}
}

//= docs/requirements/02-executor.md#interface
//= type=test
//# The Executor SHALL be registered in the provider registry under
//# the executor name `flintlock`.

// TestProviderIsRegisteredAsFlintlock registers the provider the way `run`
// does and looks it up by the executor name the RunnerConfig carries.
func TestProviderIsRegisteredAsFlintlock(t *testing.T) {
	t.Parallel()
	p := newFixture(t).build()
	providers := map[string]common.ExecutorProvider{}
	Register(providers, p)
	reg := executors.NewProviderRegistry(providers)
	if got := reg.GetByName("flintlock"); got != p {
		t.Fatalf("registry[flintlock] = %v, want the flintlock provider", got)
	}
	if runnercfg.ExecutorName != Name {
		t.Errorf("runnercfg selects executor %q, the provider is registered as %q", runnercfg.ExecutorName, Name)
	}
}

//= docs/requirements/02-executor.md#interface
//= type=test
//# When the run loop calls `Acquire`, the Executor SHALL request a
//# Reservation from the Scheduler and return it as the executor data.

func TestAcquireReturnsTheReservation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := f.build()
	data, err := p.Acquire(runnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	d, ok := data.(*Data)
	if !ok {
		t.Fatalf("executor data is %T, want *Data", data)
	}
	if len(f.sched.reservations) != 1 || d.Reservation != f.sched.reservations[0] {
		t.Fatalf("executor data carries %v, scheduler granted %v", d.Reservation, f.sched.reservations)
	}
}

//= docs/requirements/02-executor.md#interface
//= type=test
//# If the Scheduler refuses a Reservation, then the Executor SHALL
//# return an error that the run loop treats as no free executor, so that no
//# Job is requested from GitLab.

//= docs/requirements/01-gitlab-protocol.md#job-acquisition
//= type=test
//# The Runner SHALL request a Job from GitLab only after the
//# Scheduler has granted a Reservation for it.

// TestAcquireRefusalIsNoFreeExecutor checks that a refusal comes back as the
// error type the run loop checks for before it would request a Job.
func TestAcquireRefusalIsNoFreeExecutor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.sched.refuse = &scheduler.Refusal{Reason: scheduler.RefusalNoWarmMicroVM}
	p := f.build()
	data, err := p.Acquire(runnerConfig())
	var nfe *common.NoFreeExecutorError
	if !errors.As(err, &nfe) {
		t.Fatalf("Acquire error = %v (%T), want *common.NoFreeExecutorError", err, err)
	}
	if data != nil {
		t.Errorf("executor data = %v on refusal", data)
	}
}

//= docs/requirements/02-executor.md#interface
//= type=test
//# When the run loop calls `Release`, the Executor SHALL release the
//# Reservation held in the executor data.

//= docs/requirements/01-gitlab-protocol.md#job-acquisition
//= type=test
//# When GitLab returns no job, the Runner SHALL release the
//# Reservation before the next request is made.

// TestReleaseReleasesTheReservation is the run loop's no-job path: Acquire,
// then Release straight away.
func TestReleaseReleasesTheReservation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := f.build()
	data, err := p.Acquire(runnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	p.Release(runnerConfig(), data)
	if len(f.sched.resReleased) != 1 || f.sched.resReleased[0] != data.(*Data).Reservation {
		t.Fatalf("released %v, want the acquired reservation", f.sched.resReleased)
	}
	p.Release(runnerConfig(), nil) // a nil data is ignored
}

//= docs/requirements/02-executor.md#interface
//= type=test
//# The Executor SHALL report the shell name `bash` and the feature
//# set listed in the GitLab protocol document from `GetFeatures` and
//# `GetDefaultShell`.

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//= type=test
//# The Runner SHALL advertise the features `variables`, `image`,
//# `refspecs`, `masking`, `raw_variables`, `artifacts`, `artifacts_exclude`,
//# `upload_multiple_artifacts`, `upload_raw_artifacts`, `cache`,
//# `fallback_cache_keys`, `multi_build_steps`, `return_exit_code`,
//# `trace_reset`, `trace_checksum`, `trace_size`, `cancelable` and
//# `cancel_gracefully`.

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//= type=test
//# The Runner SHALL NOT advertise the `services`, `session`,
//# `terminal`, `proxy` or `shared` features.

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//= type=test
//# The Runner SHALL advertise `bash` as its default shell.

// TestFeaturesAndDefaultShell starts from a FeaturesInfo with every flag
// set, as if another layer had set them, and checks the provider leaves
// exactly the GL-022 set on and the GL-023 set off.
func TestFeaturesAndDefaultShell(t *testing.T) {
	t.Parallel()
	p := newFixture(t).build()
	f := common.FeaturesInfo{Services: true, Session: true, Terminal: true, Proxy: true, Shared: true}
	if err := p.GetFeatures(&f); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"variables": f.Variables, "image": f.Image, "refspecs": f.Refspecs, "masking": f.Masking,
		"raw_variables": f.RawVariables, "artifacts": f.Artifacts, "artifacts_exclude": f.ArtifactsExclude,
		"upload_multiple_artifacts": f.UploadMultipleArtifacts, "upload_raw_artifacts": f.UploadRawArtifacts,
		"cache": f.Cache, "fallback_cache_keys": f.FallbackCacheKeys, "multi_build_steps": f.MultiBuildSteps,
		"return_exit_code": f.ReturnExitCode, "trace_reset": f.TraceReset, "trace_checksum": f.TraceChecksum,
		"trace_size": f.TraceSize, "cancelable": f.Cancelable, "cancel_gracefully": f.CancelGracefully,
	}
	for name, on := range want {
		if !on {
			t.Errorf("feature %s not advertised", name)
		}
	}
	forbidden := map[string]bool{
		"services": f.Services, "session": f.Session, "terminal": f.Terminal, "proxy": f.Proxy, "shared": f.Shared,
	}
	for name, on := range forbidden {
		if on {
			t.Errorf("feature %s advertised", name)
		}
	}
	if got := p.GetDefaultShell(); got != "bash" {
		t.Errorf("GetDefaultShell = %q, want bash", got)
	}
}

// fakeLifecycle is a Scheduler Lifecycle whose Run blocks until cancelled.
type fakeLifecycle struct {
	scheduler.Lifecycle
	started chan struct{}
	stopped chan struct{}
}

func (l *fakeLifecycle) Run(ctx context.Context) error {
	close(l.started)
	<-ctx.Done()
	close(l.stopped)
	return nil
}

// TestInitStartsAndShutdownStopsTheScheduler checks the provider owns the
// Scheduler's Run through the run loop's hooks.
func TestInitStartsAndShutdownStopsTheScheduler(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	lc := &fakeLifecycle{started: make(chan struct{}), stopped: make(chan struct{})}
	f.opts = append(f.opts, WithLifecycle(lc))
	p := f.build()
	p.Init()
	<-lc.started
	p.Shutdown(context.Background(), nil)
	select {
	case <-lc.stopped:
	default:
		t.Fatal("Shutdown returned before the Scheduler's Run did")
	}
}
