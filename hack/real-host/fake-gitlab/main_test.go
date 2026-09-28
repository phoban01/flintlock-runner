package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// TestJobFileIsTheRealHostJob checks that hack/real-host/job.json decodes
// as a Job for the arm64 Profile, with no clone of its own project, and
// with the script that README.md says the run proves.
func TestJobFileIsTheRealHostJob(t *testing.T) {
	f, err := os.Open("../job.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	job, err := decodeJob(f)
	if err != nil {
		t.Fatal(err)
	}
	if job.Image.Name != "flr-arm64" {
		t.Errorf("image = %q, want flr-arm64", job.Image.Name)
	}
	if v := job.Variables.Get("GIT_STRATEGY"); v != "none" {
		t.Errorf("GIT_STRATEGY = %q, want none", v)
	}
	if len(job.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(job.Steps))
	}
	script := strings.Join(job.Steps[0].Script, "\n")
	for _, want := range []string{"uname -a", "cat /etc/os-release", "git clone --depth 1 https://github.com/"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
}

// TestHandlerQueuesAndRecordsJobs checks that POST /fake/jobs queues the
// same file as a new Job each time, that the Runner API hands it out, and
// that GET /fake/jobs then reports it.
func TestHandlerQueuesAndRecordsJobs(t *testing.T) {
	gl := fakegitlab.New(fakegitlab.Options{RunnerToken: "glrt-test", LongPollTimeout: fakegitlab.NoLongPoll})
	srv := httptest.NewServer(handler(gl, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	body, err := os.ReadFile("../job.json")
	if err != nil {
		t.Fatal(err)
	}

	for want := int64(1); want <= 2; want++ {
		resp, err := http.Post(srv.URL+"/fake/jobs", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ ID int64 }
		err = json.NewDecoder(resp.Body).Decode(&got)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusCreated || got.ID != want {
			t.Fatalf("queue = %d, id %d (%v); want 201, id %d", resp.StatusCode, got.ID, err, want)
		}
	}

	resp, err := http.Post(srv.URL+"/api/v4/jobs/request", "application/json",
		strings.NewReader(`{"token":"glrt-test","system_id":"s_test"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("job request = %d, want 201", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/fake/jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got []summary
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 || got[0].Status != fakegitlab.StatusRunning {
		t.Errorf("records = %+v, want job 1 running", got)
	}

	for path, want := range map[string]string{
		"/fake/jobs/1/status": "running\n",
		"/fake/jobs/2/status": "pending\n",
		"/fake/jobs/3/status": "404 page not found\n",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(b) != want {
			t.Errorf("GET %s = %q, want %q", path, b, want)
		}
	}
}
