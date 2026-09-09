package fakegitlab

// Handler-level tests: the paths that need control over time (long polling)
// or a deliberately malformed request are driven with plain HTTP against
// Handler, with a fake clock, so nothing here sleeps.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

const (
	testRunnerToken = "glrt-handler-token"
	testSystemID    = "r_handler"
)

// fakeClock hands out one controllable channel per After call and reports
// each call on afterCalled, so a test knows when a handler started waiting.
type fakeClock struct {
	afterCalled chan time.Duration
	fire        chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{afterCalled: make(chan time.Duration, 8), fire: make(chan time.Time)}
}

func (c *fakeClock) Now() time.Time { return time.Unix(0, 0) }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.afterCalled <- d
	return c.fire
}
func (c *fakeClock) NewTimer(time.Duration) clock.Timer { panic("not used") }

type response struct {
	code   int
	header http.Header
	body   []byte
}

func do(t *testing.T, url, method, path string, header http.Header, body []byte) response {
	t.Helper()
	req, err := http.NewRequest(method, url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{code: res.StatusCode, header: res.Header, body: out}
}

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newHandlerServer(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	opts.RunnerToken = testRunnerToken
	s := New(opts)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		s.Close()
		ts.Close()
	})
	return s, ts.URL
}

func requestJobBody(t *testing.T, lastUpdate string) []byte {
	t.Helper()
	return jsonBody(t, map[string]any{"token": testRunnerToken, "system_id": testSystemID, "last_update": lastUpdate})
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

func TestLongPollWakesOnEnqueue(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	s, url := newHandlerServer(t, Options{Clock: clk, LongPollTimeout: 42 * time.Second})

	// Without last_update the request is answered at once with the queue
	// version.
	first := do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, ""))
	if first.code != http.StatusNoContent || first.header.Get("X-GitLab-Last-Update") == "" {
		t.Fatalf("first request = %d %v, want 204 with X-GitLab-Last-Update", first.code, first.header)
	}
	version := first.header.Get("X-GitLab-Last-Update")

	// With the current version the request waits.
	done := make(chan response, 1)
	go func() { done <- do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, version)) }()
	select {
	case d := <-clk.afterCalled:
		if d != 42*time.Second {
			t.Errorf("long poll armed for %v, want 42s", d)
		}
	case r := <-done:
		t.Fatalf("request with the current version answered %d without waiting", r.code)
	}

	s.Enqueue(&spec.Job{ID: 5, Token: "glcbt-5"})
	r := <-done
	if r.code != http.StatusCreated || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("woken request = %d %s, want 201 application/json", r.code, r.header.Get("Content-Type"))
	}
	var job spec.Job
	if err := json.Unmarshal(r.body, &job); err != nil || job.ID != 5 {
		t.Fatalf("woken request body = %s (%v), want job 5", r.body, err)
	}
	// GitLab sends the queue version only when it has no Job to hand out,
	// so a request that produced a Job leaves the client's stored
	// last_update alone.
	if lu := r.header.Get("X-GitLab-Last-Update"); lu != "" {
		t.Errorf("job response carried X-GitLab-Last-Update %q, want none", lu)
	}

	// The version moved on, so the stale one is answered at once.
	stale := do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, version))
	if stale.code != http.StatusNoContent || stale.header.Get("X-GitLab-Last-Update") == version {
		t.Errorf("stale request = %d version %q, want 204 with a new version", stale.code, stale.header.Get("X-GitLab-Last-Update"))
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

func TestLongPollTimesOutAndClosesCleanly(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	s, url := newHandlerServer(t, Options{Clock: clk})
	version := do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, "")).header.Get("X-GitLab-Last-Update")

	done := make(chan response, 1)
	go func() { done <- do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, version)) }()
	<-clk.afterCalled
	clk.fire <- time.Time{}
	r := <-done
	if r.code != http.StatusNoContent || r.header.Get("X-GitLab-Last-Update") != version {
		t.Fatalf("timed out request = %d version %q, want 204 with version %q", r.code, r.header.Get("X-GitLab-Last-Update"), version)
	}

	// Close releases a waiting request rather than leaking its goroutine.
	go func() { done <- do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, version)) }()
	<-clk.afterCalled
	s.Close()
	if r := <-done; r.code != http.StatusNoContent {
		t.Errorf("request released by Close = %d, want 204", r.code)
	}
	s.Close() // idempotent
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake GitLab HTTP server that implements
//# runner verification, job request with long polling and the
//# `X-GitLab-Last-Update` header, job update, trace patching with
//# `Content-Range` and range-not-satisfiable handling, artifact upload and
//# dependency artifact download.

func TestPatchTraceRanges(t *testing.T) {
	t.Parallel()
	s, url := newHandlerServer(t, Options{TraceUpdateInterval: 5 * time.Second})
	s.Enqueue(&spec.Job{ID: 9, Token: "glcbt-9"})
	if r := do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, "")); r.code != http.StatusCreated {
		t.Fatalf("request job = %d, want 201", r.code)
	}
	auth := http.Header{"Job-Token": {"glcbt-9"}}
	patch := func(contentRange, body string) response {
		h := auth.Clone()
		if contentRange != "" {
			h.Set("Content-Range", contentRange)
		}
		return do(t, url, http.MethodPatch, "/api/v4/jobs/9/trace?debug_trace=false", h, []byte(body))
	}

	tests := []struct {
		name         string
		contentRange string
		body         string
		wantCode     int
		wantRange    string
		wantTrace    string
	}{
		{"missing content-range", "", "x", http.StatusBadRequest, "", ""},
		{"malformed content-range", "abc-def", "x", http.StatusBadRequest, "", ""},
		{"first chunk", "0-4", "hello", http.StatusAccepted, "0-5", "hello"},
		{"gap", "7-8", "!!", http.StatusRequestedRangeNotSatisfiable, "0-5", "hello"},
		{"replay of sent bytes", "0-4", "hello", http.StatusRequestedRangeNotSatisfiable, "0-5", "hello"},
		{"next chunk", "5-10", " world", http.StatusAccepted, "0-11", "hello world"},
		{"empty chunk at the end", "11-10", "", http.StatusAccepted, "0-11", "hello world"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := patch(tt.contentRange, tt.body)
			if r.code != tt.wantCode {
				t.Fatalf("code = %d (%s), want %d", r.code, r.body, tt.wantCode)
			}
			if got := r.header.Get("Range"); got != tt.wantRange {
				t.Errorf("Range = %q, want %q", got, tt.wantRange)
			}
			if r.code == http.StatusAccepted {
				if r.header.Get("Job-Status") != StatusRunning || r.header.Get("X-GitLab-Trace-Update-Interval") != "5" {
					t.Errorf("headers = %v, want Job-Status running and a 5s update interval", r.header)
				}
			}
			if got := s.Record(9).Trace; got != tt.wantTrace {
				t.Errorf("Trace = %q, want %q", got, tt.wantTrace)
			}
		})
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL be able to cancel a running Job through the
//# `Job-Status` header and to answer a final update with an
//# accepted-but-pending response a configurable number of times.

// TestUpdateJobStatusHeader pins the headers of the job update endpoint
// across the life of a cancelled Job, since they are what the client reads
// to decide between carrying on, cancelling gracefully and aborting: every
// answered update carries the Job's status on the GitLab side, as the real
// endpoint sets Job-Status from the job once its update service has run,
// together with the trace update interval; a request for a Job that is no
// longer processing on a runner is refused with the status alone.
func TestUpdateJobStatusHeader(t *testing.T) {
	t.Parallel()
	s, url := newHandlerServer(t, Options{PendingFinalUpdates: 1, TraceUpdateInterval: 9 * time.Second})
	s.Enqueue(&spec.Job{ID: 21, Token: "glcbt-21"})
	if r := do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, "")); r.code != http.StatusCreated {
		t.Fatalf("request job = %d, want 201", r.code)
	}
	update := func(state string) response {
		body := jsonBody(t, map[string]any{"state": state, "failure_reason": "job_canceled"})
		return do(t, url, http.MethodPut, "/api/v4/jobs/21", http.Header{"Job-Token": {"glcbt-21"}}, body)
	}

	steps := []struct {
		name       string
		cancel     bool
		state      string
		wantCode   int
		wantStatus string
	}{
		{name: "heartbeat while running", state: "running", wantCode: http.StatusOK, wantStatus: StatusRunning},
		{name: "heartbeat after Cancel", cancel: true, state: "running", wantCode: http.StatusOK, wantStatus: StatusCanceling},
		{name: "pending final update stays cancelling", state: "failed", wantCode: http.StatusAccepted, wantStatus: StatusCanceling},
		{name: "confirmed final update cancels", state: "failed", wantCode: http.StatusOK, wantStatus: StatusCanceled},
		{name: "update after the job finished", state: "failed", wantCode: http.StatusForbidden, wantStatus: StatusCanceled},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			if st.cancel {
				if err := s.Cancel(21); err != nil {
					t.Fatalf("Cancel: %v", err)
				}
			}
			r := update(st.state)
			if r.code != st.wantCode {
				t.Fatalf("code = %d (%s), want %d", r.code, r.body, st.wantCode)
			}
			if got := r.header.Get(headerJobStatus); got != st.wantStatus {
				t.Errorf("Job-Status = %q, want %q", got, st.wantStatus)
			}
			// The interval is advertised on an answered update and left off
			// a refusal, as GitLab does.
			want := "9"
			if st.wantCode == http.StatusForbidden {
				want = ""
			}
			if got := r.header.Get(headerTraceUpdateInterval); got != want {
				t.Errorf("X-GitLab-Trace-Update-Interval = %q, want %q", got, want)
			}
		})
	}
	if rec := s.Record(21); rec.Status != StatusCanceled || rec.FailureReason != "job_canceled" {
		t.Errorf("record = status %q reason %q, want canceled/job_canceled", rec.Status, rec.FailureReason)
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL require the runner token and a system
//# identifier on runner-scoped requests and the Job token on job-scoped
//# requests, as the real API does.

func TestAuthentication(t *testing.T) {
	t.Parallel()
	s, url := newHandlerServer(t, Options{})
	s.Enqueue(&spec.Job{ID: 11, Token: "glcbt-11"})
	if r := do(t, url, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, "")); r.code != http.StatusCreated {
		t.Fatalf("request job = %d, want 201", r.code)
	}
	s.SeedArtifact(12, "glcbt-12", "artifacts.zip", []byte("zip"))

	runnerBody := func(token, systemID string) []byte {
		return jsonBody(t, map[string]any{"token": token, "system_id": systemID})
	}
	updateBody := func(token string) []byte {
		return jsonBody(t, map[string]any{"token": token, "state": "running"})
	}
	multipartBody := func(t *testing.T) ([]byte, string) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", "artifacts.zip")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte("zip"))
		_ = mw.Close()
		return buf.Bytes(), mw.FormDataContentType()
	}

	tests := []struct {
		name     string
		method   string
		path     string
		header   http.Header
		body     []byte
		wantCode int
		wantJob  string // expected Job-Status header, if any
	}{
		// Runner-scoped: token plus system_id, either token location.
		{"register with body token", http.MethodPost, "/api/v4/runners", nil, runnerBody(testRunnerToken, ""), http.StatusCreated, ""},
		{"register with header token", http.MethodPost, "/api/v4/runners", http.Header{"Runner-Token": {testRunnerToken}}, []byte(`{}`), http.StatusCreated, ""},
		{"register with wrong token", http.MethodPost, "/api/v4/runners", nil, runnerBody("glrt-wrong", ""), http.StatusForbidden, ""},
		{"verify", http.MethodPost, "/api/v4/runners/verify", nil, runnerBody(testRunnerToken, testSystemID), http.StatusOK, ""},
		{"verify without token", http.MethodPost, "/api/v4/runners/verify", nil, runnerBody("", testSystemID), http.StatusForbidden, ""},
		{"verify with wrong token", http.MethodPost, "/api/v4/runners/verify", nil, runnerBody("glrt-wrong", testSystemID), http.StatusForbidden, ""},
		{"verify without system id", http.MethodPost, "/api/v4/runners/verify", nil, runnerBody(testRunnerToken, ""), http.StatusForbidden, ""},
		{"verify with header token only", http.MethodPost, "/api/v4/runners/verify", http.Header{"Runner-Token": {testRunnerToken}}, runnerBody("", testSystemID), http.StatusOK, ""},
		{"verify with mismatched header token", http.MethodPost, "/api/v4/runners/verify", http.Header{"Runner-Token": {testRunnerToken}}, runnerBody("glrt-wrong", testSystemID), http.StatusForbidden, ""},
		{"request without system id", http.MethodPost, "/api/v4/jobs/request", nil, runnerBody(testRunnerToken, ""), http.StatusForbidden, ""},
		{"request with wrong token", http.MethodPost, "/api/v4/jobs/request", nil, runnerBody("glrt-wrong", testSystemID), http.StatusForbidden, ""},
		{"request with malformed body", http.MethodPost, "/api/v4/jobs/request", nil, []byte(`{`), http.StatusBadRequest, ""},
		// Job-scoped: the job's own token, in the header or the body.
		{"update with header token", http.MethodPut, "/api/v4/jobs/11", http.Header{"Job-Token": {"glcbt-11"}}, updateBody(""), http.StatusOK, StatusRunning},
		{"update with body token", http.MethodPut, "/api/v4/jobs/11", nil, updateBody("glcbt-11"), http.StatusOK, StatusRunning},
		{"update with runner token", http.MethodPut, "/api/v4/jobs/11", http.Header{"Job-Token": {testRunnerToken}}, updateBody(""), http.StatusForbidden, ""},
		{"update with another job's token", http.MethodPut, "/api/v4/jobs/11", http.Header{"Job-Token": {"glcbt-12"}}, updateBody(""), http.StatusForbidden, ""},
		{"update without token", http.MethodPut, "/api/v4/jobs/11", nil, updateBody(""), http.StatusForbidden, ""},
		{"update unknown job", http.MethodPut, "/api/v4/jobs/99", http.Header{"Job-Token": {"glcbt-11"}}, updateBody(""), http.StatusNotFound, ""},
		{"update finished job", http.MethodPut, "/api/v4/jobs/12", http.Header{"Job-Token": {"glcbt-12"}}, updateBody(""), http.StatusForbidden, StatusSuccess},
		{"update with bad state", http.MethodPut, "/api/v4/jobs/11", http.Header{"Job-Token": {"glcbt-11"}}, jsonBody(t, map[string]any{"state": "bogus"}), http.StatusBadRequest, ""},
		{"update with bad id", http.MethodPut, "/api/v4/jobs/abc", http.Header{"Job-Token": {"glcbt-11"}}, updateBody(""), http.StatusBadRequest, ""},
		{"trace without token", http.MethodPatch, "/api/v4/jobs/11/trace", http.Header{"Content-Range": {"0-0"}}, []byte("x"), http.StatusForbidden, ""},
		{"trace with query token", http.MethodPatch, "/api/v4/jobs/11/trace?token=glcbt-11", http.Header{"Content-Range": {"0-0"}}, []byte("x"), http.StatusAccepted, StatusRunning},
		{"upload without token", http.MethodPost, "/api/v4/jobs/11/artifacts", nil, nil, http.StatusForbidden, ""},
		{"upload unknown job", http.MethodPost, "/api/v4/jobs/99/artifacts", http.Header{"Job-Token": {"glcbt-11"}}, nil, http.StatusNotFound, ""},
		{"download without token", http.MethodGet, "/api/v4/jobs/12/artifacts", nil, nil, http.StatusUnauthorized, ""},
		{"download with unrelated token", http.MethodGet, "/api/v4/jobs/12/artifacts", http.Header{"Job-Token": {"glcbt-11"}}, nil, http.StatusForbidden, ""},
		{"download with own token", http.MethodGet, "/api/v4/jobs/12/artifacts", http.Header{"Job-Token": {"glcbt-12"}}, nil, http.StatusOK, ""},
		{"unknown route", http.MethodGet, "/api/v4/nope", nil, nil, http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := tt.header.Clone()
			body := tt.body
			if strings.HasSuffix(tt.path, "/artifacts") && tt.method == http.MethodPost {
				var contentType string
				body, contentType = multipartBody(t)
				if header == nil {
					header = http.Header{}
				}
				header.Set("Content-Type", contentType)
			}
			r := do(t, url, tt.method, tt.path, header, body)
			if r.code != tt.wantCode {
				t.Fatalf("code = %d (%s), want %d", r.code, r.body, tt.wantCode)
			}
			if got := r.header.Get("Job-Status"); got != tt.wantJob {
				t.Errorf("Job-Status = %q, want %q", got, tt.wantJob)
			}
			if r.code >= 400 && r.code != http.StatusRequestedRangeNotSatisfiable {
				var msg map[string]string
				if err := json.Unmarshal(r.body, &msg); err != nil || msg["message"] == "" {
					t.Errorf("error body = %s, want a JSON message envelope", r.body)
				}
			}
		})
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The fake GitLab SHALL hand out Jobs from a queue of `spec.Job` payloads
//# supplied by the test and SHALL record every state update and the
//# assembled trace for each Job.

func TestEncodeJobMatchesTheWireFormat(t *testing.T) {
	t.Parallel()
	// A payload as GitLab sends it: inputs is a list, run is a JSON string.
	wire := `{"id":3,"token":"t","inputs":[],"run":"[{\"name\":\"s\",\"script\":\"echo\"}]"}`
	var job spec.Job
	if err := json.Unmarshal([]byte(wire), &job); err != nil {
		t.Fatalf("unmarshal wire payload: %v", err)
	}
	out, err := encodeJob(&job)
	if err != nil {
		t.Fatalf("encodeJob: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("encodeJob output is not an object: %v", err)
	}
	if string(fields["inputs"]) != "[]" {
		t.Errorf("inputs = %s, want []", fields["inputs"])
	}
	if run := fields["run"]; len(run) == 0 || run[0] != '"' {
		t.Errorf("run = %s, want a JSON string", run)
	}
	var back spec.Job
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("the client cannot decode encodeJob output: %v", err)
	}
	if back.ID != 3 || len(back.Run) != 1 || back.Run[0].Name == nil || *back.Run[0].Name != "s" {
		t.Errorf("decoded job = %+v, want id 3 with one run step", back)
	}
}

func TestEnqueueAssignsIDsAndTokens(t *testing.T) {
	t.Parallel()
	s := New(Options{RunnerToken: testRunnerToken})
	s.Enqueue(&spec.Job{ID: 10})
	s.Enqueue(&spec.Job{})
	s.Enqueue(&spec.Job{Token: "custom"})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	var got []struct {
		ID    int64
		Token string
	}
	for range 3 {
		r := do(t, ts.URL, http.MethodPost, "/api/v4/jobs/request", nil, requestJobBody(t, ""))
		var j spec.Job
		if err := json.Unmarshal(r.body, &j); err != nil {
			t.Fatalf("decode job: %v (%d %s)", err, r.code, r.body)
		}
		got = append(got, struct {
			ID    int64
			Token string
		}{j.ID, j.Token})
	}
	want := []struct {
		ID    int64
		Token string
	}{{10, "glcbt-10"}, {11, "glcbt-11"}, {12, "custom"}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("job %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if s.Record(99) != nil {
		t.Error("Record of an unknown job is not nil")
	}
	if s.URL() != "" {
		t.Error("URL before Start is not empty")
	}
}
