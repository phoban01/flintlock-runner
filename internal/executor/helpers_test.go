package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/buildlogger"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	_ "gitlab.com/gitlab-org/gitlab-runner/shells" // the bash shell the Stage scripts are generated with

	"github.com/liquidmetal-dev/flintlock/api/types"
	"github.com/sirupsen/logrus"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// testProfile is the Profile the fake Scheduler resolves every Job to unless
// a test says otherwise. Its directories differ from the RunnerConfig's so
// that a script generated from the wrong one is caught.
func testProfile() *scheduler.Profile {
	return &scheduler.Profile{
		Profile: config.Profile{
			Name:         "builders",
			Shell:        "/usr/bin/bash",
			BuildsDir:    "/srv/builds",
			CacheDir:     "/srv/cache",
			HelperPath:   "/opt/helper/gitlab-runner-helper",
			User:         "ci",
			ReadyTimeout: 30 * time.Second,
		},
		PoolRef: poolmgr.PoolRef{Name: "builders", Namespace: "ns"},
	}
}

// fakeScheduler is the Executor's Scheduler with every answer settable and
// every call recorded.
type fakeScheduler struct {
	mu sync.Mutex

	refuse     error
	profile    *scheduler.Profile
	profileErr error
	allocErr   error
	// allocBlock makes Allocate wait for its context and return its error.
	allocBlock bool

	reservations []*scheduler.Reservation
	resReleased  []*scheduler.Reservation
	resolved     []scheduler.JobInfo
	allocCalls   int
	handles      []*fakeHandle
	released     []scheduler.Handle
	retained     []scheduler.Handle
	// allocStarted is signalled when Allocate is entered.
	allocStarted chan struct{}
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{profile: testProfile(), allocStarted: make(chan struct{}, 16)}
}

func (s *fakeScheduler) Reserve(context.Context) (*scheduler.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse != nil {
		return nil, s.refuse
	}
	r := &scheduler.Reservation{ID: uint64(len(s.reservations) + 1)}
	s.reservations = append(s.reservations, r)
	return r, nil
}

func (s *fakeScheduler) ReleaseReservation(r *scheduler.Reservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resReleased = append(s.resReleased, r)
}

func (s *fakeScheduler) ResolveProfile(job scheduler.JobInfo) (*scheduler.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, job)
	if s.profileErr != nil {
		return nil, s.profileErr
	}
	return s.profile, nil
}

func (s *fakeScheduler) Allocate(ctx context.Context, r *scheduler.Reservation, job scheduler.JobInfo, p *scheduler.Profile) (scheduler.Handle, error) {
	s.mu.Lock()
	s.allocCalls++
	block, allocErr := s.allocBlock, s.allocErr
	s.mu.Unlock()
	s.allocStarted <- struct{}{}
	if block {
		<-ctx.Done()
		return nil, &scheduler.AllocationError{Profile: p.Name, Pool: p.PoolRef, Err: context.Cause(ctx)}
	}
	if allocErr != nil {
		return nil, allocErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := &fakeHandle{
		done: make(chan struct{}),
		alloc: scheduler.Allocation{
			JobID:         job.ID,
			Profile:       p.Name,
			VMUID:         "vm-" + string(rune('a'+len(s.handles))),
			Placement:     scheduler.Placement{Host: "host-a", Source: scheduler.PlacementFromClaim},
			ReservationID: r.ID,
		},
	}
	s.handles = append(s.handles, h)
	return h, nil
}

func (s *fakeScheduler) Release(h scheduler.Handle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, h)
}

func (s *fakeScheduler) Retain(h scheduler.Handle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retained = append(s.retained, h)
}

func (s *fakeScheduler) counts() (allocs, released, retained int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allocCalls, len(s.released), len(s.retained)
}

// fakeHostClient is a HostClient that is never called: the fake transport
// stands in for everything that would reach the Host.
type fakeHostClient struct{ name string }

func (c fakeHostClient) Name() string { return c.name }
func (fakeHostClient) ServerInfo(context.Context) (*flintlock.HostInfo, error) {
	return nil, flintlock.ErrUnimplemented
}
func (fakeHostClient) GetMicroVM(context.Context, string) (*types.MicroVM, error) {
	return nil, flintlock.ErrNotFound
}
func (fakeHostClient) ListMicroVMs(context.Context, string) ([]*types.MicroVM, error) {
	return nil, nil
}
func (fakeHostClient) Exec(context.Context) (flintlock.ExecStream, error) {
	return nil, flintlock.ErrUnavailable
}
func (fakeHostClient) SSHProxy(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, flintlock.ErrUnavailable
}
func (fakeHostClient) Close() error { return nil }

// fakeRegistry hands out leases on fake Host clients and counts them.
type fakeRegistry struct {
	mu       sync.Mutex
	leased   []string
	returned int
	err      error
}

func (r *fakeRegistry) Get(name string) (flintlock.HostClient, error) {
	return fakeHostClient{name: name}, nil
}

func (r *fakeRegistry) Lease(name string) (flintlock.HostClient, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, nil, r.err
	}
	r.leased = append(r.leased, name)
	var once sync.Once
	return fakeHostClient{name: name}, func() {
		once.Do(func() {
			r.mu.Lock()
			r.returned++
			r.mu.Unlock()
		})
	}, nil
}

func (r *fakeRegistry) Endpoint(string) (flintlock.Endpoint, bool) {
	return flintlock.Endpoint{}, false
}
func (r *fakeRegistry) Names() []string                                   { return nil }
func (r *fakeRegistry) Apply(context.Context, []flintlock.Endpoint) error { return nil }
func (r *fakeRegistry) Close() error                                      { return nil }

func (r *fakeRegistry) outstanding() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.leased) - r.returned
}

// fakeTransport records every Command and answers from a script.
type fakeTransport struct {
	mu sync.Mutex
	// notReady is how many Ready calls fail before one succeeds; a negative
	// value never succeeds.
	notReady   int
	readyCalls int
	// runs answers Run calls in order after the directory creation; once it
	// is exhausted Run exits 0.
	runs     []transport.Scripted
	hook     func(ctx context.Context, cmd transport.Command, stdin []byte) (int, error)
	recorded []transport.Recorded
	closed   bool
}

func (t *fakeTransport) Ready(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.readyCalls++
	if t.notReady < 0 || t.readyCalls <= t.notReady {
		return transport.ErrNotReady
	}
	return nil
}

func (t *fakeTransport) Run(ctx context.Context, cmd transport.Command) (int, error) {
	var stdin []byte
	if cmd.Stdin != nil {
		stdin, _ = io.ReadAll(cmd.Stdin)
	}
	t.mu.Lock()
	t.recorded = append(t.recorded, transport.Recorded{Command: cmd, Stdin: stdin})
	isDirs := cmd.Dir == "/"
	hook := t.hook
	var sc transport.Scripted
	if !isDirs && len(t.runs) > 0 {
		sc, t.runs = t.runs[0], t.runs[1:]
	}
	t.mu.Unlock()

	if hook != nil && !isDirs {
		return hook(ctx, cmd, stdin)
	}
	if sc.Delay > 0 {
		select {
		case <-time.After(sc.Delay):
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
	if sc.Err != nil {
		return -1, sc.Err
	}
	if cmd.Stdout != nil && len(sc.Stdout) > 0 {
		_, _ = cmd.Stdout.Write(sc.Stdout)
	}
	if cmd.Stderr != nil && len(sc.Stderr) > 0 {
		_, _ = cmd.Stderr.Write(sc.Stderr)
	}
	return sc.ExitStatus, nil
}

func (t *fakeTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// stages returns the recorded Stage commands, without the directory
// creation.
func (t *fakeTransport) stages() []transport.Recorded {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []transport.Recorded
	for _, r := range t.recorded {
		if r.Dir != "/" {
			out = append(out, r)
		}
	}
	return out
}

func (t *fakeTransport) all() []transport.Recorded {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]transport.Recorded(nil), t.recorded...)
}

// fakeFactory returns one fakeTransport and records the Targets.
type fakeFactory struct {
	mu      sync.Mutex
	tr      *fakeTransport
	err     error
	targets []transport.Target
}

func (f *fakeFactory) New(_ context.Context, target transport.Target) (transport.Transport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, target)
	if f.err != nil {
		return nil, f.err
	}
	return f.tr, nil
}

// fixture is a Provider over the fakes.
type fixture struct {
	t        *testing.T
	sched    *fakeScheduler
	hosts    *fakeRegistry
	tr       *fakeTransport
	factory  *fakeFactory
	clk      clock.Clock
	deps     Deps
	opts     []Option
	provider *provider
	// currentBuild is the Build runBuild is running.
	currentBuild *common.Build
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tr := &fakeTransport{}
	f := &fixture{
		t:       t,
		sched:   newFakeScheduler(),
		hosts:   &fakeRegistry{},
		tr:      tr,
		factory: &fakeFactory{tr: tr},
		clk:     clock.Real{},
	}
	f.deps = Deps{
		Scheduler:  f.sched,
		Transports: f.factory,
		Hosts:      f.hosts,
		Timeouts:   Timeouts{Prepare: time.Minute, GracefulKill: 5 * time.Second, Transport: 10 * time.Second},
	}
	return f
}

// build makes the Provider from the fixture's current Deps and options.
func (f *fixture) build() *provider {
	f.t.Helper()
	p, err := NewProvider(f.deps, append([]Option{WithClock(f.clk)}, f.opts...)...)
	if err != nil {
		f.t.Fatal(err)
	}
	f.provider = p.(*provider)
	return f.provider
}

// recordTrace is a JobTrace that keeps the log.
type recordTrace struct {
	common.Trace
	mu  sync.Mutex
	buf bytes.Buffer
}

func newRecordTrace() *recordTrace {
	t := &recordTrace{}
	t.Trace.Writer = writerFunc(func(p []byte) (int, error) {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.buf.Write(p)
	})
	return t
}

func (t *recordTrace) IsStdout() bool { return false }

func (t *recordTrace) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// testJob is a Job that needs no repository: the script echoes a variable.
func testJob() spec.Job {
	return spec.Job{
		ID:         101,
		Token:      "glcbt-job-token",
		JobInfo:    spec.JobInfo{Name: "unit", Stage: "test", ProjectID: 3, ProjectName: "demo"},
		GitInfo:    spec.GitInfo{RepoURL: "https://gitlab.example.com/group/demo.git", Sha: "0123456789abcdef0123456789abcdef01234567", Ref: "main"},
		RunnerInfo: spec.RunnerInfo{Timeout: 3600},
		Image:      spec.Image{Name: "builders"},
		Variables: spec.Variables{
			{Key: "GIT_STRATEGY", Value: "none", Public: true},
			{Key: "JOB_SECRET_VALUE", Value: "only-in-the-script", Public: true},
		},
		Steps: spec.Steps{{
			Name:   spec.StepNameScript,
			Script: spec.StepScript{`echo "$JOB_SECRET_VALUE"`},
			When:   spec.StepWhenOnSuccess,
		}},
	}
}

// runnerConfig is the RunnerConfig runnercfg.Build would produce, with the
// Default Profile's directories deliberately different from testProfile.
func runnerConfig() *common.RunnerConfig {
	return &common.RunnerConfig{
		Name: "unit-runner",
		RunnerCredentials: common.RunnerCredentials{
			URL:   "https://gitlab.example.com",
			Token: "glrt-unit",
		},
		RunnerSettings: common.RunnerSettings{
			Executor:  Name,
			Shell:     DefaultShell,
			BuildsDir: "/runner-default-builds",
			CacheDir:  "/runner-default-cache",
		},
	}
}

// runBuild runs job through the library's Build with the fixture's
// provider, the way the run loop does after Acquire.
func (f *fixture) runBuild(ctx context.Context, job spec.Job) (*recordTrace, *common.Build, error) {
	f.t.Helper()
	if f.provider == nil {
		f.build()
	}
	data, err := f.provider.Acquire(runnerConfig())
	if err != nil {
		f.t.Fatalf("Acquire: %v", err)
	}
	defer f.provider.Release(runnerConfig(), data)
	b, err := common.NewBuild(job, runnerConfig(), nil, data, f.provider)
	if err != nil {
		f.t.Fatal(err)
	}
	trace := newRecordTrace()
	f.currentBuild = b
	err = b.Run(ctx, &common.Config{}, trace)
	return trace, b, err
}

// prepareOnly calls Prepare on a fresh executor directly.
func (f *fixture) prepareOnly(ctx context.Context, job spec.Job) (*executor, *recordTrace, error) {
	f.t.Helper()
	if f.provider == nil {
		f.build()
	}
	data, err := f.provider.Acquire(runnerConfig())
	if err != nil {
		f.t.Fatalf("Acquire: %v", err)
	}
	b, err := common.NewBuild(job, runnerConfig(), nil, data, f.provider)
	if err != nil {
		f.t.Fatal(err)
	}
	trace := newRecordTrace()
	e := f.provider.Create().(*executor)
	err = e.Prepare(common.ExecutorPrepareOptions{
		Config:      b.Runner,
		Build:       b,
		BuildLogger: buildlogger.New(trace, discardEntry(), buildlogger.Options{}),
		Context:     ctx,
	})
	return e, trace, err
}

// discardEntry is a logrus entry that writes nowhere; the build logger
// writes to the Job log only when it has one.
func discardEntry() *logrus.Entry {
	l := logrus.New()
	l.Out = io.Discard
	return logrus.NewEntry(l)
}

// buildError asserts err is a BuildError and returns it.
func buildError(t *testing.T, err error) *common.BuildError {
	t.Helper()
	var be *common.BuildError
	if !errors.As(err, &be) {
		t.Fatalf("error %v (%T) is not a BuildError", err, err)
	}
	return be
}

// stageScript returns the recorded stdin of the first Stage whose script
// contains marker.
func stageScript(t *testing.T, recs []transport.Recorded, marker string) transport.Recorded {
	t.Helper()
	for _, r := range recs {
		if strings.Contains(string(r.Stdin), marker) {
			return r
		}
	}
	t.Fatalf("no stage script contains %q", marker)
	return transport.Recorded{}
}
