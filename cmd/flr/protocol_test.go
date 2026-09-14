package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/urfave/cli"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/network"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/config/runnercfg"
	"github.com/phoban01/flintlock-runner/internal/executor"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// stubScheduler satisfies executor.Scheduler for tests that only need a
// provider to exist.
type stubScheduler struct{}

func (stubScheduler) Reserve(context.Context) (*scheduler.Reservation, error) {
	return nil, &scheduler.Refusal{Reason: scheduler.RefusalNoFreeSlot}
}
func (stubScheduler) ReleaseReservation(*scheduler.Reservation) {}
func (stubScheduler) ResolveProfile(scheduler.JobInfo) (*scheduler.Profile, error) {
	return nil, scheduler.ErrNoProfile
}
func (stubScheduler) Allocate(context.Context, *scheduler.Reservation, scheduler.JobInfo, *scheduler.Profile) (scheduler.Handle, error) {
	return nil, errors.New("stub")
}
func (stubScheduler) Release(scheduler.Handle) {}
func (stubScheduler) Retain(scheduler.Handle)  {}

// stubProvider is the flintlock provider over stubs.
func stubProvider(t *testing.T) executor.Provider {
	t.Helper()
	reg, err := flintlock.NewRegistry(context.Background(), flintlockDialerStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := executor.NewProvider(executor.Deps{Scheduler: stubScheduler{}, Transports: transport.NewFactory(), Hosts: reg})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type flintlockDialerStub struct{}

func (flintlockDialerStub) Dial(context.Context, flintlock.Endpoint) (flintlock.HostClient, error) {
	return nil, errors.New("stub")
}

// runnerConfigFor translates the minimal configuration pointed at url.
func runnerConfigFor(t *testing.T, url string) *common.RunnerConfig {
	t.Helper()
	yaml := strings.Replace(minimalConfigFile, "url: https://gitlab.example.com", "url: "+url+"\n  allow_insecure: true", 1)
	cfg, err := config.Parse([]byte(yaml), t.TempDir(), config.WithEnv(func(string) (string, bool) { return "", false }))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := runnercfg.Build(cfg, "s_abcdefABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	return rc.Runners[0]
}

// runLoopClient builds the run loop the way `run` does, with the flintlock
// provider registered, and returns its network client.
func runLoopClient(t *testing.T) *network.GitLabClient {
	t.Helper()
	providers := map[string]common.ExecutorProvider{}
	executor.Register(providers, stubProvider(t))
	_, client := newRunLoopCommand(providers)
	gl, ok := client.(*network.GitLabClient)
	if !ok {
		t.Fatalf("the run loop's network client is %T, want *network.GitLabClient", client)
	}
	return gl
}

//= docs/requirements/01-gitlab-protocol.md#library-basis
//= type=test
//# The Runner SHALL perform every request to the GitLab API through
//# the `GitLabClient` type of the gitlab-runner `network` package.

//= docs/requirements/01-gitlab-protocol.md#library-basis
//= type=test
//# The Runner SHALL drive job acquisition and execution through the
//# gitlab-runner run loop (`commands.NewRunCommand`) with a provider registry
//# that contains the Executor, so that graceful shutdown, token rotation and
//# the session server come from the library unchanged.

// TestRunLoopUsesTheLibraryClientAndRegistry checks the run loop's client is
// the network package's GitLabClient and that it finds the flintlock
// executor in the registry the run loop was given: the info it would send
// carries the executor's own features, which only the registered provider
// sets.
func TestRunLoopUsesTheLibraryClientAndRegistry(t *testing.T) {
	t.Parallel()
	client := runLoopClient(t)
	req := client.PrepareJobRequest(*runnerConfigFor(t, "https://gitlab.example.com"), nil)
	if !req.Info.Features.Image {
		t.Error("the network client does not see the flintlock provider in the run loop's registry")
	}
}

//= docs/requirements/01-gitlab-protocol.md#advertised-capabilities
//= type=test
//# The Runner SHALL advertise the executor name `flintlock` in the
//# `info.executor` field of every job request.

// TestJobRequestInfoAdvertisesFlintlock builds the job request the run loop
// sends from the configuration `run` writes and checks its info: the
// executor name, the bash shell and the feature set of GL-022 and GL-023 as
// the three layers (network client, provider, shell) combine them.
func TestJobRequestInfoAdvertisesFlintlock(t *testing.T) {
	t.Parallel()
	client := runLoopClient(t)
	info := client.PrepareJobRequest(*runnerConfigFor(t, "https://gitlab.example.com"), nil).Info
	if info.Executor != "flintlock" {
		t.Errorf("info.executor = %q, want flintlock", info.Executor)
	}
	if info.Shell != "bash" {
		t.Errorf("info.shell = %q, want bash", info.Shell)
	}
	f := info.Features
	for name, on := range map[string]bool{
		"variables": f.Variables, "image": f.Image, "refspecs": f.Refspecs, "masking": f.Masking,
		"raw_variables": f.RawVariables, "artifacts": f.Artifacts, "artifacts_exclude": f.ArtifactsExclude,
		"upload_multiple_artifacts": f.UploadMultipleArtifacts, "upload_raw_artifacts": f.UploadRawArtifacts,
		"cache": f.Cache, "fallback_cache_keys": f.FallbackCacheKeys, "multi_build_steps": f.MultiBuildSteps,
		"return_exit_code": f.ReturnExitCode, "trace_reset": f.TraceReset, "trace_checksum": f.TraceChecksum,
		"trace_size": f.TraceSize, "cancelable": f.Cancelable, "cancel_gracefully": f.CancelGracefully,
	} {
		if !on {
			t.Errorf("job request does not advertise %s", name)
		}
	}
	for name, on := range map[string]bool{
		"services": f.Services, "session": f.Session, "terminal": f.Terminal, "proxy": f.Proxy, "shared": f.Shared,
	} {
		if on {
			t.Errorf("job request advertises %s", name)
		}
	}
}

// recorder is an httptest GitLab that records request bodies by path.
type recorder struct {
	mu     sync.Mutex
	bodies map[string][][]byte
	handle func(w http.ResponseWriter, r *http.Request, n int)
}

func newRecorder(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, n int)) (*recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{bodies: map[string][][]byte{}, handle: handle}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.bodies[r.URL.Path] = append(rec.bodies[r.URL.Path], body)
		n := len(rec.bodies[r.URL.Path])
		rec.mu.Unlock()
		rec.handle(w, r, n)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *recorder) get(path string) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodies[path]
}

//= docs/requirements/01-gitlab-protocol.md#authentication
//= type=test
//# When starting, the Runner SHALL call `POST /api/v4/runners/verify`
//# with the full `info` payload before it requests any Job.

// TestVerifyRunnerSendsTheFullInfo runs `run`'s verification against a
// recording GitLab: runners/verify is called and its info names the
// flintlock executor with its features.
func TestVerifyRunnerSendsTheFullInfo(t *testing.T) {
	t.Parallel()
	rec, srv := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":1,"token":"glrt-super-secret"}`))
	})
	client := runLoopClient(t)
	if err := verifyRunner(context.Background(), client, runnerConfigFor(t, srv.URL), "s_abcdefABCDEF", slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	bodies := rec.get("/api/v4/runners/verify")
	if len(bodies) != 1 {
		t.Fatalf("runners/verify called %d times", len(bodies))
	}
	var req common.VerifyRunnerRequest
	if err := json.Unmarshal(bodies[0], &req); err != nil {
		t.Fatal(err)
	}
	if req.Info.Executor != "flintlock" || req.Info.Shell != "bash" || !req.Info.Features.CancelGracefully || !req.Info.Features.Image {
		t.Errorf("verify info = %+v", req.Info)
	}
	if req.SystemID != "s_abcdefABCDEF" {
		t.Errorf("verify system_id = %q", req.SystemID)
	}
	if len(rec.get("/api/v4/runners")) != 0 {
		t.Error("the runner registration endpoint was called")
	}
}

//= docs/requirements/01-gitlab-protocol.md#authentication
//= type=test
//# If token verification returns a forbidden response, then the
//# Runner SHALL log the failure and exit with a non-zero status.

func TestVerifyRunnerForbiddenExitsNonZero(t *testing.T) {
	t.Parallel()
	_, srv := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusForbidden)
	})
	var logs bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil))
	err := verifyRunner(context.Background(), runLoopClient(t), runnerConfigFor(t, srv.URL), "s_abcdefABCDEF", log)
	var exit cli.ExitCoder
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("verifyRunner = %v, want a non-zero exit", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "rejected the runner token") {
		t.Errorf("the failure was not logged: %s", logs.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

//= docs/requirements/01-gitlab-protocol.md#authentication
//= type=test
//# When starting, the Runner SHALL load a persistent system
//# identifier from its state directory or generate and store one if none
//# exists.

func TestSystemIDIsGeneratedOnceAndReloaded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id, err := loadSystemID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^s_[0-9a-zA-Z]{12}$`).MatchString(id) {
		t.Errorf("system id %q is not in gitlab-runner's format", id)
	}
	info, err := os.Stat(filepath.Join(dir, systemIDFile))
	if err != nil {
		t.Fatalf("system id not stored: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("system id file mode = %v, want 0600", info.Mode().Perm())
	}
	again, err := loadSystemID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Errorf("second start got %q, first %q", again, id)
	}
	if err := os.WriteFile(filepath.Join(dir, systemIDFile), []byte("r_0123456789ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := loadSystemID(dir); got != "r_0123456789ab" {
		t.Errorf("an existing identifier was not loaded: %q", got)
	}
}

//= docs/requirements/01-gitlab-protocol.md#job-acquisition
//= type=test
//# When GitLab returns no job, the Runner SHALL send the
//# `X-GitLab-Last-Update` value it received as `last_update` in the next
//# request so that GitLab long-polls instead of returning immediately.

// TestNoJobSendsLastUpdateNext asks the run loop's client for a Job twice
// against a GitLab that has none.
func TestNoJobSendsLastUpdateNext(t *testing.T) {
	t.Parallel()
	rec, srv := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("X-GitLab-Last-Update", "queue-version-7")
		w.WriteHeader(http.StatusNoContent)
	})
	client := runLoopClient(t)
	rc := runnerConfigFor(t, srv.URL)
	for range 2 {
		if job, ok := client.RequestJob(context.Background(), *rc, nil); job != nil || !ok {
			t.Fatalf("RequestJob = %v, %v", job, ok)
		}
	}
	bodies := rec.get("/api/v4/jobs/request")
	if len(bodies) != 2 {
		t.Fatalf("%d job requests", len(bodies))
	}
	var second common.JobRequest
	if err := json.Unmarshal(bodies[1], &second); err != nil {
		t.Fatal(err)
	}
	if second.LastUpdate != "queue-version-7" {
		t.Errorf("second request last_update = %q", second.LastUpdate)
	}
}

//= docs/requirements/01-gitlab-protocol.md#library-basis
//= type=test
//# The Runner SHALL pin the gitlab-runner module to an explicit
//# commit and record that commit in `go.mod`.

func TestGoModPinsGitLabRunnerToACommit(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*gitlab\.com/gitlab-org/gitlab-runner v\S+[.-]\d{14}-([0-9a-f]{12})\s*$`)
	if !re.Match(data) {
		t.Error("go.mod does not pin gitlab.com/gitlab-org/gitlab-runner to a commit pseudo-version")
	}
}
