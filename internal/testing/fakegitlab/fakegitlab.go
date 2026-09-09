// Package fakegitlab is the fake GitLab HTTP server
// (docs/requirements/10-test-doubles.md#fake-gitlab): runner verification,
// job request with long polling, job update, trace patching, artifact upload
// and dependency download (TD-030), handing out spec.Job payloads from a
// queue and recording every state update and trace (TD-031).
//
// The fake speaks the subset of the GitLab Runner REST API that the
// gitlab-runner network client uses, with the same paths, methods, headers,
// status codes and JSON shapes, so that a Runner built from that client can
// be pointed at it unchanged. Its state is observable through Record and
// Records for assertions, and it is driven through Enqueue, Cancel,
// ForceCancel and SeedArtifact.
package fakegitlab

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

const (
	// DefaultLongPollTimeout bounds a job request whose last_update matches
	// the current queue version when the queue is empty.
	DefaultLongPollTimeout = 30 * time.Second
	// DefaultTraceUpdateInterval is what the fake advertises in
	// X-GitLab-Trace-Update-Interval unless Options say otherwise.
	DefaultTraceUpdateInterval = 3 * time.Second
	// DefaultRunnerID is the runner id reported by registration and
	// verification.
	DefaultRunnerID int64 = 1

	shutdownTimeout = 5 * time.Second
)

// Job statuses as GitLab reports them in the Job-Status header.
const (
	StatusRunning   = "running"
	StatusCanceling = "canceling"
	StatusCanceled  = "canceled"
	StatusSuccess   = "success"
	StatusFailed    = "failed"
)

// ErrUnknownJob is returned by Cancel and ForceCancel for a Job the fake has
// not handed out.
var ErrUnknownJob = errors.New("fakegitlab: unknown job")

// ErrJobFinished is returned by Cancel and ForceCancel for a Job whose final
// state has already been confirmed.
var ErrJobFinished = errors.New("fakegitlab: job already finished")

// Options configure a Server.
type Options struct {
	// RunnerToken is the token required on runner-scoped requests (TD-033).
	RunnerToken string
	// PendingFinalUpdates is how many times a final job update is answered
	// with accepted-but-pending before it is confirmed (TD-032).
	PendingFinalUpdates int
	// LongPollTimeout bounds how long a job request whose last_update equals
	// the current queue version waits for a Job before answering no content.
	// Zero means DefaultLongPollTimeout.
	LongPollTimeout time.Duration
	// TraceUpdateInterval is sent, in whole seconds, as
	// X-GitLab-Trace-Update-Interval on trace patch and job update responses.
	// Zero means DefaultTraceUpdateInterval; a value under one second is
	// sent as 0, which the network client ignores.
	TraceUpdateInterval time.Duration
	// RunnerID is reported by registration and verification. Zero means
	// DefaultRunnerID.
	RunnerID int64
	// Clock times the long poll. Nil means clock.Real.
	Clock clock.Clock
}

// Update is one job update request as received on PUT /api/v4/jobs/:id.
type Update struct {
	// State is the job state the Runner reported.
	State string
	// FailureReason accompanies a failed state.
	FailureReason string
	// ExitCode is the exit code the Runner reported, zero if none.
	ExitCode int
	// Checksum and Bytesize describe the Job log as the Runner sees it.
	Checksum string
	Bytesize int
	// Accepted is false when the update was answered accepted-but-pending.
	Accepted bool
}

// Upload is one artifact upload as received on POST /api/v4/jobs/:id/artifacts.
type Upload struct {
	// Name is the file name from the multipart form.
	Name string
	// Type, Format and ExpireIn are the query parameters of the upload.
	Type     string
	Format   string
	ExpireIn string
	// Content is the uploaded file.
	Content []byte
}

// JobRecord is what the fake recorded about one Job (TD-031).
type JobRecord struct {
	ID int64
	// Token is the Job token that job-scoped requests have to present.
	Token string
	// SystemID is the system_id of the runner manager the Job was handed
	// to, empty for a seeded record.
	SystemID string
	// Status is the job status on the GitLab side: running, canceling,
	// canceled, success or failed.
	Status string
	// States are the job states received in order, last one final.
	States []string
	// Updates are the job update requests received in order.
	Updates []Update
	// FailureReason is the last failure reason received, if any.
	FailureReason string
	// ExitCode is the exit code reported on the final update.
	ExitCode int
	// Trace is the assembled Job log.
	Trace string
	// Artifacts maps uploaded artifact file names to their content.
	Artifacts map[string][]byte
	// Uploads are the artifact uploads received in order.
	Uploads []Upload
}

// jobState is the fake's mutable state for one Job, guarded by Server.mu.
type jobState struct {
	job          *spec.Job
	rec          JobRecord
	trace        []byte
	pendingLeft  int
	dependencies map[int64]bool
}

func (j *jobState) processingOnRunner() bool {
	return j.rec.Status == StatusRunning || j.rec.Status == StatusCanceling
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

// Server is the fake GitLab. New builds it, Start serves it on a loopback
// port and Close stops it; Handler exposes it for httptest. Its methods are
// safe for concurrent use.
type Server struct {
	opts Options

	mu           sync.Mutex
	queue        []*spec.Job
	jobs         map[int64]*jobState
	queueVersion uint64
	wake         chan struct{}
	nextID       int64
	requests     []string

	closed   chan struct{}
	listener net.Listener
	server   *http.Server
	serveErr chan error
}

// New builds a Server. Start serves it.
func New(opts Options) *Server {
	if opts.LongPollTimeout <= 0 {
		opts.LongPollTimeout = DefaultLongPollTimeout
	}
	if opts.TraceUpdateInterval <= 0 {
		opts.TraceUpdateInterval = DefaultTraceUpdateInterval
	}
	if opts.RunnerID == 0 {
		opts.RunnerID = DefaultRunnerID
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	return &Server{
		opts:         opts,
		jobs:         map[int64]*jobState{},
		queueVersion: 1,
		wake:         make(chan struct{}),
		nextID:       1,
		closed:       make(chan struct{}),
	}
}

// Start begins serving on a free loopback port.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return errors.New("fakegitlab: already started")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("fakegitlab: listen: %w", err)
	}
	s.listener = ln
	s.server = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.serveErr = make(chan error, 1)
	go func(srv *http.Server) {
		s.serveErr <- srv.Serve(ln)
	}(s.server)
	return nil
}

// Close stops the server, releases every long-polling request and waits for
// the serving goroutine. It is safe to call more than once and before Start.
func (s *Server) Close() {
	s.mu.Lock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	srv, serveErr := s.server, s.serveErr
	s.server, s.listener = nil, nil
	s.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
	<-serveErr
}

// URL is the base URL to give the Runner as the GitLab URL, empty before
// Start.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return "http://" + s.listener.Addr().String()
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The fake GitLab SHALL hand out Jobs from a queue of `spec.Job` payloads
//# supplied by the test and SHALL record every state update and the
//# assembled trace for each Job.

// Enqueue adds a Job to hand out on the next job request (TD-031). A zero
// ID gets the next free one and an empty Token gets a generated one, since
// the real API never hands out a Job without either. The Job is deep-copied
// through the wire form the fake hands out, so that later changes by the
// caller are not seen; a shallow copy would leave Steps, Variables,
// Dependencies, Services, Artifacts and Cache shared, and since the fake
// re-serialises at hand-out time a mutation made after Enqueue would be
// picked up at request time. Enqueue bumps the queue version and wakes every
// long-polling job request. It returns an error when the payload cannot be
// serialised.
func (s *Server) Enqueue(job *spec.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := copyJob(job)
	if err != nil {
		return fmt.Errorf("fakegitlab: enqueue job %d: %w", job.ID, err)
	}
	if j.ID == 0 {
		for s.jobs[s.nextID] != nil {
			s.nextID++
		}
		j.ID = s.nextID
	}
	if j.ID >= s.nextID {
		s.nextID = j.ID + 1
	}
	if j.Token == "" {
		j.Token = "glcbt-" + strconv.FormatInt(j.ID, 10)
	}
	s.queue = append(s.queue, j)
	s.bumpQueueLocked()
	return nil
}

// bumpQueueLocked advances the queue version and wakes long pollers. The
// caller holds s.mu.
func (s *Server) bumpQueueLocked() {
	s.queueVersion++
	close(s.wake)
	s.wake = make(chan struct{})
}

// Pending is the number of Jobs not yet handed out.
func (s *Server) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The fake GitLab SHALL be able to cancel a running Job through the
//# `Job-Status` header and to answer a final update with an
//# accepted-but-pending response a configurable number of times.

// Cancel marks a running Job as being cancelled, the way GitLab does for a
// runner that advertises graceful cancellation: the next trace patch or
// update is still accepted but returns Job-Status: canceling, and the
// Runner is expected to run after_script and report a final state (TD-032).
func (s *Server) Cancel(jobID int64) error {
	return s.setStatus(jobID, StatusCanceling)
}

// ForceCancel marks a running Job cancelled outright, the way GitLab does
// when a Job is cancelled without graceful support: every further
// job-scoped request is refused with 403 and Job-Status: canceled, which
// makes the Runner abort without after_script.
func (s *Server) ForceCancel(jobID int64) error {
	return s.setStatus(jobID, StatusCanceled)
}

func (s *Server) setStatus(jobID int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil {
		return fmt.Errorf("%w: %d", ErrUnknownJob, jobID)
	}
	if !j.processingOnRunner() {
		return fmt.Errorf("%w: %d is %s", ErrJobFinished, jobID, j.rec.Status)
	}
	j.rec.Status = status
	return nil
}

// SeedArtifact makes an archive artifact downloadable for a Job the fake
// did not run, so that dependency download can be tested without running
// the producing Job first. The record it creates is finished with status
// success and is authenticated with token.
func (s *Server) SeedArtifact(jobID int64, token, name string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil {
		j = &jobState{
			job: &spec.Job{ID: jobID, Token: token},
			rec: JobRecord{ID: jobID, Token: token, Status: StatusSuccess, Artifacts: map[string][]byte{}},
		}
		s.jobs[jobID] = j
		if jobID >= s.nextID {
			s.nextID = jobID + 1
		}
	}
	j.addUpload(Upload{Name: name, Type: "archive", Format: "zip", Content: content})
}

func (j *jobState) addUpload(u Upload) {
	u.Content = append([]byte(nil), u.Content...)
	j.rec.Uploads = append(j.rec.Uploads, u)
	j.rec.Artifacts[u.Name] = u.Content
}

// Record returns a snapshot of what was recorded for a Job, nil if unknown.
func (s *Server) Record(jobID int64) *JobRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil {
		return nil
	}
	return j.snapshot()
}

// Records returns a snapshot of every Job handed out or seeded, by ID.
func (s *Server) Records() []*JobRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*JobRecord, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j.snapshot())
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// Requests returns every request received so far as "METHOD path", in
// order, so that tests can assert on the sequence of API calls.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (j *jobState) snapshot() *JobRecord {
	rec := j.rec
	rec.Trace = string(j.trace)
	rec.States = append([]string(nil), j.rec.States...)
	rec.Updates = append([]Update(nil), j.rec.Updates...)
	rec.Artifacts = make(map[string][]byte, len(j.rec.Artifacts))
	for k, v := range j.rec.Artifacts {
		rec.Artifacts[k] = append([]byte(nil), v...)
	}
	rec.Uploads = make([]Upload, len(j.rec.Uploads))
	for i, u := range j.rec.Uploads {
		u.Content = append([]byte(nil), u.Content...)
		rec.Uploads[i] = u
	}
	return &rec
}
