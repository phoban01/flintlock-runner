// Package fakegitlab is the fake GitLab HTTP server
// (docs/requirements/10-test-doubles.md#fake-gitlab): runner verification,
// job request with long polling, job update, trace patching, artifact upload
// and dependency download (TD-030), handing out spec.Job payloads from a
// queue and recording every state update and trace (TD-031).
//
// Phase 0 provides the compiling skeleton. The `fakes` work package of
// docs/PLAN.md fills it in.
package fakegitlab

import (
	"errors"
	"sync"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
)

// Options configure a Server.
type Options struct {
	// RunnerToken is the token required on runner-scoped requests (TD-033).
	RunnerToken string
	// PendingFinalUpdates is how many times a final job update is answered
	// with accepted-but-pending before it is confirmed (TD-032).
	PendingFinalUpdates int
}

// JobRecord is what the fake recorded about one Job (TD-031).
type JobRecord struct {
	ID int64
	// States are the job states received in order, last one final.
	States []string
	// FailureReason is the last failure reason received, if any.
	FailureReason string
	// ExitCode is the exit code reported on the final update.
	ExitCode int
	// Trace is the assembled Job log.
	Trace string
	// Artifacts maps uploaded artifact file names to their content.
	Artifacts map[string][]byte
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=todo
//= tracking-issue=TBD
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

// Server is the fake GitLab.
type Server struct {
	opts Options

	mu    sync.Mutex
	queue []*spec.Job
}

// New builds a Server. Start serves it.
func New(opts Options) *Server { return &Server{opts: opts} }

// Start begins serving on a free loopback port.
func (s *Server) Start() error { return errors.ErrUnsupported }

// Close stops the server.
func (s *Server) Close() {}

// URL is the base URL to give the Runner as the GitLab URL, empty before
// Start.
func (s *Server) URL() string { return "" }

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=todo
//= tracking-issue=TBD
//# The fake GitLab SHALL hand out Jobs from a queue of `spec.Job` payloads
//# supplied by the test and SHALL record every state update and the
//# assembled trace for each Job.

// Enqueue adds a Job to hand out on the next job request (TD-031).
func (s *Server) Enqueue(job *spec.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, job)
}

// Pending is the number of Jobs not yet handed out.
func (s *Server) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=todo
//= tracking-issue=TBD
//# The fake GitLab SHALL be able to cancel a running Job through the
//# `Job-Status` header and to answer a final update with an
//# accepted-but-pending response a configurable number of times.

// Cancel marks a running Job cancelled so that the next trace patch or
// update returns Job-Status: canceled (TD-032).
func (s *Server) Cancel(jobID int64) error {
	_ = jobID
	return errors.ErrUnsupported
}

// Record returns what was recorded for a Job, nil if unknown.
func (s *Server) Record(jobID int64) *JobRecord {
	_ = jobID
	return nil
}
