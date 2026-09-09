package fakegitlab_test

// These tests drive the fake with the real gitlab-runner network client,
// which is the client the Runner uses (GL-002). If the fake diverges from
// the GitLab API in a path, method, header, status code or JSON shape the
// client cares about, the client notices here before the Runner does.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"gitlab.com/gitlab-org/gitlab-runner/network"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

const (
	runnerToken = "glrt-fake-runner-token"
	systemID    = "r_fake_system_id"
)

func startServer(t *testing.T, opts fakegitlab.Options) *fakegitlab.Server {
	t.Helper()
	if opts.RunnerToken == "" {
		opts.RunnerToken = runnerToken
	}
	s := fakegitlab.New(opts)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func runnerConfig(url string) common.RunnerConfig {
	return common.RunnerConfig{
		SystemID: systemID,
		RunnerCredentials: common.RunnerCredentials{
			URL:   url,
			Token: runnerToken,
		},
	}
}

func jobCredentials(url string, job *spec.Job) *common.JobCredentials {
	return &common.JobCredentials{ID: job.ID, Token: job.Token, URL: url}
}

func testJob(id int64) *spec.Job {
	return &spec.Job{
		ID:            id,
		Token:         fmt.Sprintf("glcbt-%d", id),
		AllowGitFetch: true,
		JobInfo:       spec.JobInfo{Name: "build", Stage: "test", ProjectID: 42},
		GitInfo: spec.GitInfo{
			RepoURL: "http://gitlab.example/group/project.git",
			Ref:     "main",
			Sha:     "0123456789abcdef",
		},
		RunnerInfo: spec.RunnerInfo{Timeout: 3600},
		Steps: spec.Steps{{
			Name:    spec.StepNameScript,
			Script:  spec.StepScript{"echo hello"},
			Timeout: 3600,
			When:    spec.StepWhenOnSuccess,
		}},
		Image:     spec.Image{Name: "default"},
		Variables: spec.Variables{{Key: "FOO", Value: "bar", Public: true}},
	}
}

// requestJob enqueues job and fetches it through the real client, checking
// the payload survived the round trip.
func requestJob(t *testing.T, s *fakegitlab.Server, client *network.GitLabClient, job *spec.Job) *spec.Job {
	t.Helper()
	s.Enqueue(job)
	got, healthy := client.RequestJob(context.Background(), runnerConfig(s.URL()), nil)
	if !healthy || got == nil {
		t.Fatalf("RequestJob = (%v, %v), want a job", got, healthy)
	}
	if got.ID != job.ID || got.Token != job.Token {
		t.Fatalf("RequestJob returned job %d/%s, want %d/%s", got.ID, got.Token, job.ID, job.Token)
	}
	return got
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

func TestRealClientRunnerVerification(t *testing.T) {
	t.Parallel()
	s := startServer(t, fakegitlab.Options{RunnerID: 17})
	client := network.NewGitLabClient()
	cfg := runnerConfig(s.URL())

	reg := client.RegisterRunner(cfg, common.RegisterRunnerParameters{Description: "fake"})
	if reg == nil || reg.ID != 17 || reg.Token != runnerToken {
		t.Fatalf("RegisterRunner = %+v, want id 17 and the runner token", reg)
	}

	ver, err := client.VerifyRunner(context.Background(), cfg, systemID)
	if err != nil || ver == nil || ver.ID != 17 {
		t.Fatalf("VerifyRunner = (%+v, %v), want id 17", ver, err)
	}

	bad := cfg
	bad.Token = "glrt-wrong"
	ver, err = client.VerifyRunner(context.Background(), bad, systemID)
	if err != nil || ver != nil {
		t.Fatalf("VerifyRunner with a wrong token = (%+v, %v), want (nil, nil) for forbidden", ver, err)
	}

	want := []string{
		"POST /api/v4/runners",
		"POST /api/v4/runners/verify",
		"POST /api/v4/runners/verify",
	}
	if got := s.Requests(); !reflect.DeepEqual(got, want) {
		t.Errorf("Requests = %v, want %v", got, want)
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL hand out Jobs from a queue of `spec.Job` payloads
//# supplied by the test and SHALL record every state update and the
//# assembled trace for each Job.

func TestRealClientJobLifecycle(t *testing.T) {
	t.Parallel()
	s := startServer(t, fakegitlab.Options{TraceUpdateInterval: 7 * time.Second})
	client := network.NewGitLabClient()
	cfg := runnerConfig(s.URL())
	ctx := context.Background()

	// An empty queue is no content; the client keeps the queue version from
	// X-GitLab-Last-Update and sends it as last_update from then on.
	if job, healthy := client.RequestJob(ctx, cfg, nil); job != nil || !healthy {
		t.Fatalf("RequestJob on an empty queue = (%v, %v), want (nil, true)", job, healthy)
	}
	if lu := client.PrepareJobRequest(cfg, nil).LastUpdate; lu == "" {
		t.Fatal("client did not pick up X-GitLab-Last-Update from the no-content response")
	}

	want := testJob(101)
	job := requestJob(t, s, client, want)
	if job.JobInfo.Name != "build" || job.GitInfo.RepoURL != want.GitInfo.RepoURL ||
		len(job.Steps) != 1 || job.Steps[0].Script[0] != "echo hello" ||
		job.Image.Name != "default" || len(job.Variables) != 1 || job.Variables[0].Value != "bar" ||
		job.RunnerInfo.Timeout != 3600 {
		t.Fatalf("job payload did not survive the round trip: %+v", job)
	}
	if s.Pending() != 0 {
		t.Errorf("Pending = %d after handing out the only job", s.Pending())
	}
	rec := s.Record(job.ID)
	if rec == nil || rec.Status != fakegitlab.StatusRunning || rec.SystemID != systemID || rec.Token != job.Token {
		t.Fatalf("Record after handout = %+v, want running, system id %q", rec, systemID)
	}

	creds := jobCredentials(s.URL(), job)
	res := client.UpdateJob(cfg, creds, common.UpdateJobInfo{ID: job.ID, State: common.Running})
	if res.State != common.UpdateSucceeded || res.CancelRequested || res.NewUpdateInterval != 7*time.Second {
		t.Fatalf("UpdateJob(running) = %+v, want succeeded with a 7s interval", res)
	}

	patch := client.PatchTrace(cfg, creds, []byte("hello "), 0, false)
	if patch.State != common.PatchSucceeded || patch.SentOffset != 6 || patch.NewUpdateInterval != 7*time.Second {
		t.Fatalf("PatchTrace(0) = %+v, want succeeded up to offset 6", patch)
	}
	patch = client.PatchTrace(cfg, creds, []byte("world"), 6, false)
	if patch.State != common.PatchSucceeded || patch.SentOffset != 11 {
		t.Fatalf("PatchTrace(6) = %+v, want succeeded up to offset 11", patch)
	}
	// A patch at the wrong offset is range-not-satisfiable and the Range
	// header tells the client where the log actually ends.
	patch = client.PatchTrace(cfg, creds, []byte("lost"), 20, false)
	if patch.State != common.PatchRangeMismatch || patch.SentOffset != 11 {
		t.Fatalf("PatchTrace(20) = %+v, want range mismatch with offset 11", patch)
	}
	patch = client.PatchTrace(cfg, creds, []byte("!"), 11, false)
	if patch.State != common.PatchSucceeded || patch.SentOffset != 12 {
		t.Fatalf("PatchTrace(11) = %+v, want succeeded up to offset 12", patch)
	}

	// The confirmed final update carries the state the Job now has on the
	// GitLab side, exactly as the real API answers with the job's status
	// once the update service has run. For a Job that ends failed that
	// header is "failed", which network.RemoteJobStateResponse.IsFailed
	// reports as a job that is no longer the Runner's to work on, so the
	// client returns UpdateAbort even though the request succeeded. The
	// client treats that as a terminal outcome for a final update
	// (clientJobTrace.finalUpdate returns nil for both UpdateSucceeded and
	// UpdateAbort), which TestRealClientProcessJobTrace exercises.
	res = client.UpdateJob(cfg, creds, common.UpdateJobInfo{
		ID:            job.ID,
		State:         common.Failed,
		FailureReason: common.ScriptFailure,
		ExitCode:      3,
		Output:        common.JobTraceOutput{Checksum: "crc32:deadbeef", Bytesize: 12},
	})
	if res.State != common.UpdateAbort || res.CancelRequested {
		t.Fatalf("final UpdateJob = %+v, want abort from Job-Status: failed", res)
	}

	rec = s.Record(job.ID)
	if rec.Trace != "hello world!" {
		t.Errorf("Trace = %q, want %q", rec.Trace, "hello world!")
	}
	if wantStates := []string{"running", "failed"}; !reflect.DeepEqual(rec.States, wantStates) {
		t.Errorf("States = %v, want %v", rec.States, wantStates)
	}
	if rec.Status != fakegitlab.StatusFailed || rec.FailureReason != "script_failure" || rec.ExitCode != 3 {
		t.Errorf("record = status %q reason %q exit %d, want failed/script_failure/3", rec.Status, rec.FailureReason, rec.ExitCode)
	}
	if len(rec.Updates) != 2 || rec.Updates[1].Checksum != "crc32:deadbeef" || rec.Updates[1].Bytesize != 12 || !rec.Updates[1].Accepted {
		t.Errorf("Updates = %+v, want two accepted updates with the checksum on the final one", rec.Updates)
	}

	// A finished job refuses further job-scoped requests with its status,
	// which the client turns into an abort.
	if patch := client.PatchTrace(cfg, creds, []byte("late"), 12, false); patch.State != common.PatchAbort {
		t.Errorf("PatchTrace after the final update = %+v, want abort", patch)
	}
	if res := client.UpdateJob(cfg, creds, common.UpdateJobInfo{ID: job.ID, State: common.Running}); res.State != common.UpdateAbort {
		t.Errorf("UpdateJob after the final update = %+v, want abort", res)
	}

	if recs := s.Records(); len(recs) != 1 || recs[0].ID != job.ID {
		t.Errorf("Records = %+v, want the one job", recs)
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL hand out Jobs from a queue of `spec.Job` payloads
//# supplied by the test and SHALL record every state update and the
//# assembled trace for each Job.

func TestRealClientHandsOutJobsInOrder(t *testing.T) {
	t.Parallel()
	s := startServer(t, fakegitlab.Options{})
	client := network.NewGitLabClient()
	cfg := runnerConfig(s.URL())

	s.Enqueue(testJob(1))
	s.Enqueue(testJob(2))
	s.Enqueue(&spec.Job{}) // gets an id and a token
	if s.Pending() != 3 {
		t.Fatalf("Pending = %d, want 3", s.Pending())
	}
	var ids []int64
	for range 3 {
		job, healthy := client.RequestJob(context.Background(), cfg, nil)
		if !healthy || job == nil {
			t.Fatalf("RequestJob = (%v, %v), want a job", job, healthy)
		}
		if job.Token == "" {
			t.Errorf("job %d handed out without a token", job.ID)
		}
		ids = append(ids, job.ID)
	}
	if want := []int64{1, 2, 3}; !reflect.DeepEqual(ids, want) {
		t.Errorf("handed out %v, want %v", ids, want)
	}
	if job, healthy := client.RequestJob(context.Background(), cfg, nil); job != nil || !healthy {
		t.Errorf("RequestJob on the drained queue = (%v, %v), want (nil, true)", job, healthy)
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL be able to cancel a running Job through the
//# `Job-Status` header and to answer a final update with an
//# accepted-but-pending response a configurable number of times.

func TestRealClientCancellation(t *testing.T) {
	t.Parallel()
	s := startServer(t, fakegitlab.Options{})
	client := network.NewGitLabClient()
	cfg := runnerConfig(s.URL())

	if err := s.Cancel(404); err == nil {
		t.Error("Cancel of an unknown job succeeded")
	}

	// Graceful cancellation: requests are still accepted and carry
	// Job-Status: canceling, which the client reports as CancelRequested.
	job := requestJob(t, s, client, testJob(201))
	creds := jobCredentials(s.URL(), job)
	if err := s.Cancel(job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	patch := client.PatchTrace(cfg, creds, []byte("running after_script\n"), 0, false)
	if patch.State != common.PatchSucceeded || !patch.CancelRequested {
		t.Fatalf("PatchTrace after Cancel = %+v, want succeeded with CancelRequested", patch)
	}
	res := client.UpdateJob(cfg, creds, common.UpdateJobInfo{ID: job.ID, State: common.Running})
	if res.State != common.UpdateSucceeded || !res.CancelRequested {
		t.Fatalf("UpdateJob after Cancel = %+v, want succeeded with CancelRequested", res)
	}
	// The final update moves the Job from canceling to canceled, and the
	// response carries that status, as GitLab's own update endpoint answers
	// with the job's status after the transition. Job-Status: canceled is
	// what IsFailed reports, so the client returns UpdateAbort; for a final
	// update that is terminal and not an error.
	res = client.UpdateJob(cfg, creds, common.UpdateJobInfo{ID: job.ID, State: common.Failed, FailureReason: common.JobCanceled})
	if res.State != common.UpdateAbort || res.CancelRequested {
		t.Fatalf("final UpdateJob after Cancel = %+v, want abort from Job-Status: canceled", res)
	}
	if rec := s.Record(job.ID); rec.Status != fakegitlab.StatusCanceled || rec.FailureReason != "job_canceled" {
		t.Errorf("record after graceful cancel = status %q reason %q, want canceled/job_canceled", rec.Status, rec.FailureReason)
	}
	if err := s.Cancel(job.ID); err == nil {
		t.Error("Cancel of a finished job succeeded")
	}

	// Forced cancellation: the job is canceled outright and every further
	// request is refused with Job-Status: canceled, which the client turns
	// into an abort.
	job = requestJob(t, s, client, testJob(202))
	creds = jobCredentials(s.URL(), job)
	if err := s.ForceCancel(job.ID); err != nil {
		t.Fatalf("ForceCancel: %v", err)
	}
	if patch := client.PatchTrace(cfg, creds, []byte("x"), 0, false); patch.State != common.PatchAbort {
		t.Errorf("PatchTrace after ForceCancel = %+v, want abort", patch)
	}
	if res := client.UpdateJob(cfg, creds, common.UpdateJobInfo{ID: job.ID, State: common.Running}); res.State != common.UpdateAbort {
		t.Errorf("UpdateJob after ForceCancel = %+v, want abort", res)
	}
	if rec := s.Record(job.ID); rec.Status != fakegitlab.StatusCanceled {
		t.Errorf("Status after ForceCancel = %q, want canceled", rec.Status)
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL be able to cancel a running Job through the
//# `Job-Status` header and to answer a final update with an
//# accepted-but-pending response a configurable number of times.

func TestRealClientPendingFinalUpdates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pending int
	}{
		{"confirmed at once", 0},
		{"pending once", 1},
		{"pending three times", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := startServer(t, fakegitlab.Options{PendingFinalUpdates: tt.pending})
			client := network.NewGitLabClient()
			cfg := runnerConfig(s.URL())
			job := requestJob(t, s, client, testJob(301))
			creds := jobCredentials(s.URL(), job)
			final := common.UpdateJobInfo{ID: job.ID, State: common.Success, ExitCode: 0}

			for i := range tt.pending {
				res := client.UpdateJob(cfg, creds, final)
				if res.State != common.UpdateAcceptedButNotCompleted {
					t.Fatalf("final update %d = %+v, want accepted-but-pending", i+1, res)
				}
				if rec := s.Record(job.ID); rec.Status != fakegitlab.StatusRunning {
					t.Fatalf("Status while pending = %q, want running", rec.Status)
				}
			}
			if res := client.UpdateJob(cfg, creds, final); res.State != common.UpdateSucceeded {
				t.Fatalf("confirming update = %+v, want succeeded", res)
			}
			rec := s.Record(job.ID)
			if rec.Status != fakegitlab.StatusSuccess {
				t.Errorf("Status = %q, want success", rec.Status)
			}
			if len(rec.Updates) != tt.pending+1 {
				t.Fatalf("recorded %d updates, want %d", len(rec.Updates), tt.pending+1)
			}
			for i, u := range rec.Updates {
				if want := i == tt.pending; u.Accepted != want || u.State != "success" {
					t.Errorf("update %d = %+v, want state success accepted=%v", i, u, want)
				}
			}
		})
	}
}

type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

func TestRealClientArtifacts(t *testing.T) {
	t.Parallel()
	s := startServer(t, fakegitlab.Options{})
	client := network.NewGitLabClient()
	archive := []byte("PK\x03\x04 pretend this is a zip")

	// The producing job uploads its archive and a report.
	producer := requestJob(t, s, client, testJob(401))
	producerCreds := jobCredentials(s.URL(), producer)
	state, _, err := client.UploadRawArtifacts(*producerCreds, common.BytesProvider{Data: archive}, common.ArtifactsOptions{
		BaseName: "artifacts.zip",
		Type:     "archive",
		Format:   spec.ArtifactFormatZip,
		ExpireIn: "1 day",
	})
	if err != nil || state != common.UploadSucceeded {
		t.Fatalf("UploadRawArtifacts(archive) = (%v, %v), want succeeded", state, err)
	}
	state, _, err = client.UploadRawArtifacts(*producerCreds, common.BytesProvider{Data: []byte("<testsuite/>")}, common.ArtifactsOptions{
		BaseName: "junit.xml.gz",
		Type:     "junit",
		Format:   spec.ArtifactFormatGzip,
	})
	if err != nil || state != common.UploadSucceeded {
		t.Fatalf("UploadRawArtifacts(junit) = (%v, %v), want succeeded", state, err)
	}
	rec := s.Record(producer.ID)
	if !bytes.Equal(rec.Artifacts["artifacts.zip"], archive) || !bytes.Equal(rec.Artifacts["junit.xml.gz"], []byte("<testsuite/>")) {
		t.Fatalf("Artifacts = %v, want both uploads by file name", rec.Artifacts)
	}
	if len(rec.Uploads) != 2 || rec.Uploads[0].Type != "archive" || rec.Uploads[0].Format != "zip" || rec.Uploads[0].ExpireIn != "1 day" || rec.Uploads[1].Type != "junit" {
		t.Fatalf("Uploads = %+v, want archive/zip/1 day then junit", rec.Uploads)
	}

	// A dependent job downloads the archive with the token from its
	// dependencies list, both with and without direct download.
	depJob := testJob(402)
	depJob.Dependencies = spec.Dependencies{{ID: producer.ID, Token: producer.Token, Name: "build"}}
	consumer := requestJob(t, s, client, depJob)
	for _, direct := range []*bool{nil, new(bool), func() *bool { b := true; return &b }()} {
		for _, token := range []string{consumer.Token, producer.Token} {
			out := nopWriteCloser{&bytes.Buffer{}}
			state := client.DownloadArtifacts(common.JobCredentials{ID: producer.ID, Token: token, URL: s.URL()}, out, direct)
			if state != common.DownloadSucceeded || !bytes.Equal(out.Bytes(), archive) {
				t.Errorf("DownloadArtifacts(direct=%v, token=%s) = %v with %q, want the archive", direct, token, state, out.String())
			}
		}
	}

	// A seeded archive stands in for a job the fake never ran.
	s.SeedArtifact(77, "glcbt-77", "artifacts.zip", []byte("seeded"))
	seededDep := testJob(403)
	seededDep.Dependencies = spec.Dependencies{{ID: 77, Token: "glcbt-77"}}
	seededConsumer := requestJob(t, s, client, seededDep)
	out := nopWriteCloser{&bytes.Buffer{}}
	if state := client.DownloadArtifacts(common.JobCredentials{ID: 77, Token: seededConsumer.Token, URL: s.URL()}, out, nil); state != common.DownloadSucceeded || out.String() != "seeded" {
		t.Errorf("DownloadArtifacts(seeded) = %v with %q, want the seeded archive", state, out.String())
	}

	tests := []struct {
		name  string
		jobID int64
		token string
		want  common.DownloadState
	}{
		{"unknown token", producer.ID, "glcbt-nope", common.DownloadUnauthorized},
		{"unrelated job token", producer.ID, seededConsumer.Token, common.DownloadForbidden},
		{"unknown job", 999, consumer.Token, common.DownloadForbidden},
		{"job without an archive", consumer.ID, consumer.Token, common.DownloadNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := nopWriteCloser{&bytes.Buffer{}}
			if got := client.DownloadArtifacts(common.JobCredentials{ID: tt.jobID, Token: tt.token, URL: s.URL()}, out, nil); got != tt.want {
				t.Errorf("DownloadArtifacts = %v, want %v", got, tt.want)
			}
		})
	}

	// Upload is job-scoped: the wrong token is forbidden.
	wrong := *producerCreds
	wrong.Token = "glcbt-nope"
	if state, _, err := client.UploadRawArtifacts(wrong, common.BytesProvider{Data: archive}, common.ArtifactsOptions{BaseName: "x.zip"}); err != nil || state != common.UploadForbidden {
		t.Errorf("UploadRawArtifacts with a wrong token = (%v, %v), want forbidden", state, err)
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL require the runner token and a system
//# identifier on runner-scoped requests and the Job token on job-scoped
//# requests, as the real API does.

func TestRealClientRunnerScopedAuth(t *testing.T) {
	t.Parallel()
	s := startServer(t, fakegitlab.Options{})
	client := network.NewGitLabClient()
	s.Enqueue(testJob(501))

	tests := []struct {
		name     string
		token    string
		systemID string
		wantJob  bool
		healthy  bool
	}{
		{"wrong token", "glrt-wrong", systemID, false, false},
		{"empty token", "", systemID, false, false},
		{"missing system id", runnerToken, "", false, false},
		{"valid", runnerToken, systemID, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := runnerConfig(s.URL())
			cfg.Token = tt.token
			cfg.SystemID = tt.systemID
			job, healthy := client.RequestJob(context.Background(), cfg, nil)
			if (job != nil) != tt.wantJob || healthy != tt.healthy {
				t.Errorf("RequestJob = (%v, %v), want job=%v healthy=%v", job != nil, healthy, tt.wantJob, tt.healthy)
			}
		})
	}
	if s.Pending() != 0 {
		t.Errorf("Pending = %d, want 0 after the valid request", s.Pending())
	}
}

// discardTrace drains a JobTrace so ProcessJob can be exercised; the real
// Runner streams the log through it.
func discardTrace(t *testing.T, trace common.JobTrace) {
	t.Helper()
	if _, err := io.WriteString(trace, "line\n"); err != nil {
		t.Fatalf("write trace: %v", err)
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL hand out Jobs from a queue of `spec.Job` payloads
//# supplied by the test and SHALL record every state update and the
//# assembled trace for each Job.

func TestRealClientProcessJobTrace(t *testing.T) {
	t.Parallel()
	// The client backs off for the advertised interval between a pending
	// answer and its retry, so the fake advertises the smallest one.
	s := startServer(t, fakegitlab.Options{PendingFinalUpdates: 1, TraceUpdateInterval: time.Second})
	client := network.NewGitLabClient()
	cfg := runnerConfig(s.URL())
	job := requestJob(t, s, client, testJob(601))

	// ProcessJob is the client's own trace streamer: it patches the log,
	// sends the final update with checksum and size and retries while the
	// fake answers accepted-but-pending.
	trace, err := client.ProcessJob(cfg, jobCredentials(s.URL(), job))
	if err != nil {
		t.Fatalf("ProcessJob: %v", err)
	}
	discardTrace(t, trace)
	if err := trace.Success(); err != nil {
		t.Fatalf("trace.Success: %v", err)
	}

	rec := s.Record(job.ID)
	if rec.Trace != "line\n" {
		t.Errorf("Trace = %q, want %q", rec.Trace, "line\n")
	}
	if rec.Status != fakegitlab.StatusSuccess {
		t.Errorf("Status = %q, want success", rec.Status)
	}
	if n := len(rec.Updates); n != 2 || rec.Updates[0].Accepted || !rec.Updates[1].Accepted {
		t.Errorf("Updates = %+v, want one pending then one accepted final update", rec.Updates)
	}
	if last := rec.Updates[len(rec.Updates)-1]; last.Checksum == "" || last.Bytesize != len("line\n") {
		t.Errorf("final update = %+v, want a checksum and bytesize %d", last, len("line\n"))
	}
}
