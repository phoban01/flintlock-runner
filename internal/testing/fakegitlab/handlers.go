package fakegitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
)

// Header names used by the GitLab Runner API.
const (
	headerRunnerToken         = "RUNNER-TOKEN"
	headerJobToken            = "JOB-TOKEN"
	headerJobStatus           = "Job-Status"
	headerLastUpdate          = "X-GitLab-Last-Update"
	headerTraceUpdateInterval = "X-GitLab-Trace-Update-Interval"
	headerContentRange        = "Content-Range"
	headerRange               = "Range"

	contentTypeJSON = "application/json"

	// maxBodyBytes bounds every request body the fake reads into memory.
	maxBodyBytes = 256 << 20
	// multipartMemory is how much of an artifact upload multipart form is
	// kept in memory before spilling to disk.
	multipartMemory = 32 << 20
)

// runnerRequest is the body of a runner-scoped request: registration,
// verification and job request all carry the token and, apart from
// registration, the system identifier.
type runnerRequest struct {
	Token      string `json:"token"`
	SystemID   string `json:"system_id"`
	LastUpdate string `json:"last_update"`
}

// runnerResponse is the body of a successful registration or verification.
type runnerResponse struct {
	ID             int64     `json:"id"`
	Token          string    `json:"token"`
	TokenExpiresAt time.Time `json:"token_expires_at"`
}

// updateRequest is the body of PUT /api/v4/jobs/:id.
type updateRequest struct {
	Token         string `json:"token"`
	State         string `json:"state"`
	FailureReason string `json:"failure_reason"`
	ExitCode      int    `json:"exit_code"`
	Checksum      string `json:"checksum"`
	Output        struct {
		Checksum string `json:"checksum"`
		Bytesize int    `json:"bytesize"`
	} `json:"output"`
}

// apiError is a refusal with the status and JSON message GitLab would send,
// plus the Job-Status header when the job is known but not running.
type apiError struct {
	code      int
	message   string
	jobStatus string
}

func (e *apiError) write(w http.ResponseWriter) {
	if e.jobStatus != "" {
		w.Header().Set(headerJobStatus, e.jobStatus)
	}
	writeError(w, e.code, e.message)
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

// Handler returns the HTTP handler behind Start, for tests that prefer
// httptest or want to mount the fake elsewhere. Its routes are the subset
// of the GitLab Runner API the network client uses (TD-030).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v4/runners", s.handleRegister)
	mux.HandleFunc("POST /api/v4/runners/verify", s.handleVerify)
	mux.HandleFunc("POST /api/v4/jobs/request", s.handleRequestJob)
	mux.HandleFunc("PUT /api/v4/jobs/{id}", s.handleUpdateJob)
	mux.HandleFunc("PATCH /api/v4/jobs/{id}/trace", s.handlePatchTrace)
	mux.HandleFunc("POST /api/v4/jobs/{id}/artifacts", s.handleUploadArtifacts)
	mux.HandleFunc("GET /api/v4/jobs/{id}/artifacts", s.handleDownloadArtifacts)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "404 Not Found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		mux.ServeHTTP(w, r)
	})
}

// handleRegister is POST /api/v4/runners. With a runner authentication
// token GitLab registers a runner manager and returns the runner; the fake
// accepts its own runner token and reports its runner id.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req runnerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.authRunner(r, req, false); err != nil {
		err.write(w)
		return
	}
	writeJSON(w, http.StatusCreated, runnerResponse{ID: s.opts.RunnerID, Token: s.opts.RunnerToken})
}

// handleVerify is POST /api/v4/runners/verify.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req runnerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.authRunner(r, req, true); err != nil {
		err.write(w)
		return
	}
	writeJSON(w, http.StatusOK, runnerResponse{ID: s.opts.RunnerID, Token: s.opts.RunnerToken})
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The fake GitLab SHALL require the runner token and a system
//# identifier on runner-scoped requests and the Job token on job-scoped
//# requests, as the real API does.

// authRunner checks a runner-scoped request (TD-033): the runner token has
// to be present in the body or the RUNNER-TOKEN header and match, and when
// needSystemID is set the body has to carry a system_id. Registration is
// the one runner-scoped request without a system identifier.
func (s *Server) authRunner(r *http.Request, req runnerRequest, needSystemID bool) *apiError {
	token := req.Token
	if token == "" {
		token = r.Header.Get(headerRunnerToken)
	}
	if token == "" || token != s.opts.RunnerToken {
		return &apiError{code: http.StatusForbidden, message: "403 Forbidden"}
	}
	if needSystemID && req.SystemID == "" {
		return &apiError{code: http.StatusForbidden, message: "403 Forbidden - system_id is required"}
	}
	return nil
}

// jobToken is the Job token of a job-scoped request: the JOB-TOKEN header,
// else the token field of the body (bodyToken), else the token query
// parameter.
func jobToken(r *http.Request, bodyToken string) string {
	if t := r.Header.Get(headerJobToken); t != "" {
		return t
	}
	if bodyToken != "" {
		return bodyToken
	}
	return r.URL.Query().Get("token")
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The fake GitLab SHALL require the runner token and a system
//# identifier on runner-scoped requests and the Job token on job-scoped
//# requests, as the real API does.

// authJobLocked resolves a job-scoped request the way authenticate_job!
// does. The convention is that every refusal to serve a job-scoped request
// is 403, never 404: an unknown Job, a token belonging to another Job and a
// Job that is no longer processing on a runner are answered alike, so that
// Job ids cannot be enumerated by their status codes, which is why the real
// API refuses an unresolvable Job with 403 as well. The refusal for a
// finished Job carries its status in Job-Status so that the Runner aborts.
// handleDownloadArtifacts follows the same convention for the Job whose
// artifacts are asked for; 404 there means the Job exists but has no
// archive. The caller holds s.mu.
func (s *Server) authJobLocked(id int64, token string) (*jobState, *apiError) {
	j := s.jobs[id]
	if j == nil {
		return nil, &apiError{code: http.StatusForbidden, message: "403 Forbidden - Job not found"}
	}
	if token == "" || token != j.rec.Token {
		return nil, &apiError{code: http.StatusForbidden, message: "403 Forbidden - Job token invalid"}
	}
	if !j.processingOnRunner() {
		return nil, &apiError{
			code:      http.StatusForbidden,
			message:   "403 Forbidden - Job is not processing on runner",
			jobStatus: j.rec.Status,
		}
	}
	return j, nil
}

// handleRequestJob is POST /api/v4/jobs/request. A request whose
// last_update equals the current queue version long-polls until a Job is
// enqueued, the long poll timeout elapses, the client goes away or the
// server closes; any other request is answered at once. No content carries
// the queue version in X-GitLab-Last-Update so that the next request polls.
func (s *Server) handleRequestJob(w http.ResponseWriter, r *http.Request) {
	var req runnerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.authRunner(r, req, true); err != nil {
		err.write(w)
		return
	}

	var (
		timeout  <-chan time.Time
		timedOut bool
	)
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			job := s.popLocked(req.SystemID)
			s.mu.Unlock()
			body, err := encodeJob(job)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "500 encode job: "+err.Error())
				return
			}
			w.Header().Set("Content-Type", contentTypeJSON)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(body)
			return
		}
		version := strconv.FormatUint(s.queueVersion, 10)
		wake := s.wake
		s.mu.Unlock()

		if timedOut || req.LastUpdate != version {
			w.Header().Set(headerLastUpdate, version)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if timeout == nil {
			timeout = s.opts.Clock.After(s.opts.LongPollTimeout)
		}
		select {
		case <-wake:
		case <-timeout:
			timedOut = true
		case <-s.closed:
			timedOut = true
		case <-r.Context().Done():
			return
		}
	}
}

// popLocked hands out the head of the queue to the runner manager
// systemID and opens its record with status running. The caller holds s.mu.
func (s *Server) popLocked(systemID string) *spec.Job {
	job := s.queue[0]
	s.queue[0] = nil
	s.queue = s.queue[1:]
	js := &jobState{
		job: job,
		rec: JobRecord{
			ID:        job.ID,
			Token:     job.Token,
			SystemID:  systemID,
			Status:    StatusRunning,
			Artifacts: map[string][]byte{},
		},
		pendingLeft:  s.opts.PendingFinalUpdates,
		dependencies: map[int64]bool{},
	}
	for _, d := range job.Dependencies {
		js.dependencies[d.ID] = true
	}
	s.jobs[job.ID] = js
	return job
}

// encodeJob serialises a Job the way GitLab does and the network client
// expects. spec.Job has two fields whose wire form differs from their Go
// form: inputs is a list on the wire but an opaque struct in Go, and run is
// a JSON string holding a JSON document. The fake cannot recover declared
// inputs from the struct, so it sends an empty list; run is re-encoded.
func encodeJob(job *spec.Job) ([]byte, error) {
	raw, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if in, ok := fields["inputs"]; !ok || len(in) == 0 || in[0] != '[' {
		fields["inputs"] = json.RawMessage("[]")
	}
	if run, ok := fields["run"]; ok && len(run) > 0 && run[0] != '"' {
		quoted, err := json.Marshal(string(run))
		if err != nil {
			return nil, err
		}
		fields["run"] = quoted
	}
	return json.Marshal(fields)
}

// copyJob deep-copies a Job by round-tripping it through the wire form
// encodeJob produces, which is the form the fake hands out anyway, so that
// the queue shares no reference field with the caller.
func copyJob(job *spec.Job) (*spec.Job, error) {
	raw, err := encodeJob(job)
	if err != nil {
		return nil, err
	}
	var out spec.Job
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// handleUpdateJob is PUT /api/v4/jobs/:id. A running state is a heartbeat
// answered 200. A final state is answered 202 accepted-but-pending as many
// times as Options.PendingFinalUpdates says and then 200, after which the
// Job is finished and further job-scoped requests are refused. Every
// response carries Job-Status and X-GitLab-Trace-Update-Interval.
func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathJobID(w, r)
	if !ok {
		return
	}
	var req updateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token := jobToken(r, req.Token)

	s.mu.Lock()
	j, apiErr := s.authJobLocked(id, token)
	if apiErr != nil {
		s.mu.Unlock()
		apiErr.write(w)
		return
	}
	code, apiErr := j.applyUpdate(req)
	status := j.rec.Status
	s.mu.Unlock()
	if apiErr != nil {
		apiErr.write(w)
		return
	}
	s.setJobHeaders(w, status)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, strconv.Itoa(code))
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The fake GitLab SHALL hand out Jobs from a queue of `spec.Job` payloads
//# supplied by the test and SHALL record every state update and the
//# assembled trace for each Job.

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The fake GitLab SHALL be able to cancel a running Job through the
//# `Job-Status` header and to answer a final update with an
//# accepted-but-pending response a configurable number of times.

// applyUpdate records an update request (TD-031) and decides its status
// code: a running state is a heartbeat, and a final state is answered
// accepted-but-pending while the configured count lasts (TD-032) and then
// confirmed, which moves the Job to its terminal status - canceled when
// GitLab was cancelling it, otherwise the state the Runner reported.
func (j *jobState) applyUpdate(req updateRequest) (int, *apiError) {
	u := Update{
		State:         req.State,
		FailureReason: req.FailureReason,
		ExitCode:      req.ExitCode,
		Checksum:      req.Output.Checksum,
		Bytesize:      req.Output.Bytesize,
		Accepted:      true,
	}
	if u.Checksum == "" {
		u.Checksum = req.Checksum
	}
	code := http.StatusOK
	switch req.State {
	case StatusRunning:
	case StatusSuccess, StatusFailed:
		j.rec.ExitCode = req.ExitCode
		switch {
		case j.pendingLeft > 0:
			j.pendingLeft--
			u.Accepted = false
			code = http.StatusAccepted
		case j.rec.Status == StatusCanceling:
			// A Job GitLab was cancelling ends as canceled whatever final
			// state the Runner reports, as it does on the GitLab side.
			j.rec.Status = StatusCanceled
		default:
			j.rec.Status = req.State
		}
	default:
		return 0, &apiError{code: http.StatusBadRequest, message: "400 Bad Request - state does not have a valid value"}
	}
	if req.FailureReason != "" {
		j.rec.FailureReason = req.FailureReason
	}
	j.rec.States = append(j.rec.States, req.State)
	j.rec.Updates = append(j.rec.Updates, u)
	return code, nil
}

// handlePatchTrace is PATCH /api/v4/jobs/:id/trace. The body is appended at
// the offset named by Content-Range when that offset is the current end of
// the log, answered 202 with Range 0-<size>; any other offset is 416 with
// Range 0-<size> so that the Runner resends from there.
func (s *Server) handlePatchTrace(w http.ResponseWriter, r *http.Request) {
	id, ok := pathJobID(w, r)
	if !ok {
		return
	}
	contentRange := r.Header.Get(headerContentRange)
	if contentRange == "" {
		writeError(w, http.StatusBadRequest, "400 Missing header Content-Range")
		return
	}
	startText, _, _ := strings.Cut(contentRange, "-")
	start, err := strconv.Atoi(strings.TrimSpace(startText))
	if err != nil || start < 0 {
		writeError(w, http.StatusBadRequest, "400 Invalid Content-Range")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "400 Bad Request - "+err.Error())
		return
	}
	token := jobToken(r, "")

	s.mu.Lock()
	j, apiErr := s.authJobLocked(id, token)
	if apiErr != nil {
		s.mu.Unlock()
		apiErr.write(w)
		return
	}
	size := len(j.trace)
	matched := start == size
	if matched {
		j.trace = append(j.trace, body...)
		size = len(j.trace)
	}
	status := j.rec.Status
	s.mu.Unlock()

	w.Header().Set(headerRange, "0-"+strconv.Itoa(size))
	if !matched {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	s.setJobHeaders(w, status)
	w.WriteHeader(http.StatusAccepted)
}

// handleUploadArtifacts is POST /api/v4/jobs/:id/artifacts: a multipart
// form whose file part is the artifact, with artifact_type, artifact_format
// and expire_in as query parameters, answered 201.
//
// The token is checked before the form is parsed whenever it is in the
// header or the query, which is where the real client puts it, so that an
// unauthenticated upload is refused rather than buffered and spilled to
// disk first. Only the body-form fallback has to wait for the parse.
func (s *Server) handleUploadArtifacts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathJobID(w, r)
	if !ok {
		return
	}
	if token := jobToken(r, ""); token != "" {
		s.mu.Lock()
		_, apiErr := s.authJobLocked(id, token)
		s.mu.Unlock()
		if apiErr != nil {
			apiErr.write(w)
			return
		}
	}
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		writeError(w, http.StatusBadRequest, "400 Bad Request - "+err.Error())
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "400 Bad Request - file is missing")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "400 Bad Request - "+err.Error())
		return
	}
	token := jobToken(r, r.FormValue("token"))
	q := r.URL.Query()
	upload := Upload{
		Name:     header.Filename,
		Type:     q.Get("artifact_type"),
		Format:   q.Get("artifact_format"),
		ExpireIn: q.Get("expire_in"),
		Content:  content,
	}

	s.mu.Lock()
	j, apiErr := s.authJobLocked(id, token)
	if apiErr != nil {
		s.mu.Unlock()
		apiErr.write(w)
		return
	}
	j.addUpload(upload)
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]string{"message": "201 Created"})
}

// handleDownloadArtifacts is GET /api/v4/jobs/:id/artifacts. The token is a
// Job token: that of the Job whose archive is requested, or that of a Job
// which lists it as a dependency. An unknown token is 401, a token without
// access or an unknown Job is 403 and a Job without an archive is 404.
func (s *Server) handleDownloadArtifacts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathJobID(w, r)
	if !ok {
		return
	}
	token := jobToken(r, "")

	s.mu.Lock()
	caller := s.jobByTokenLocked(token)
	if caller == nil {
		s.mu.Unlock()
		writeError(w, http.StatusUnauthorized, "401 Unauthorized")
		return
	}
	target := s.jobs[id]
	if target == nil || (caller != target && !caller.dependencies[id]) {
		s.mu.Unlock()
		writeError(w, http.StatusForbidden, "403 Forbidden")
		return
	}
	archive, found := target.archiveLocked()
	s.mu.Unlock()

	if !found {
		writeError(w, http.StatusNotFound, "404 Not Found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive)
}

// jobByTokenLocked finds the Job a Job token belongs to. The caller holds
// s.mu.
func (s *Server) jobByTokenLocked(token string) *jobState {
	if token == "" {
		return nil
	}
	for _, j := range s.jobs {
		if j.rec.Token == token {
			return j
		}
	}
	return nil
}

// archiveLocked is the content of the Job's archive artifact: the last
// upload typed archive, else the last untyped upload. The caller holds s.mu.
func (j *jobState) archiveLocked() ([]byte, bool) {
	var untyped []byte
	var haveUntyped bool
	for i := len(j.rec.Uploads) - 1; i >= 0; i-- {
		u := j.rec.Uploads[i]
		switch u.Type {
		case "archive":
			return append([]byte(nil), u.Content...), true
		case "":
			if !haveUntyped {
				untyped, haveUntyped = append([]byte(nil), u.Content...), true
			}
		}
	}
	return untyped, haveUntyped
}

// setJobHeaders adds the Job-Status and X-GitLab-Trace-Update-Interval
// headers GitLab puts on accepted trace patches and job updates.
func (s *Server) setJobHeaders(w http.ResponseWriter, status string) {
	w.Header().Set(headerJobStatus, status)
	w.Header().Set(headerTraceUpdateInterval, strconv.Itoa(int(s.opts.TraceUpdateInterval/time.Second)))
}

func pathJobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "400 Bad Request - id is invalid")
		return 0, false
	}
	return id, true
}

// decodeJSON reads a JSON body into v, answering 400 and returning false
// when it cannot. An empty body decodes to the zero value, as with Grape.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	writeError(w, http.StatusBadRequest, "400 Bad Request - "+err.Error())
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("500 encode response: %v", err))
		return
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// writeError answers with the JSON message envelope GitLab uses for errors.
func writeError(w http.ResponseWriter, code int, message string) {
	body, _ := json.Marshal(map[string]string{"message": message})
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(code)
	_, _ = w.Write(body)
}
