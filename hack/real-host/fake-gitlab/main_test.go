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

// TestOtherJobFiles checks that the Job files of the services and fault
// steps decode as Jobs for the arm64 Profile, with the commands run.sh
// looks for in their traces.
func TestOtherJobFiles(t *testing.T) {
	for file, wants := range map[string][]string{
		"../job-host-services.json": {"$GOPROXY/github.com/google/uuid/@v/v1.6.0.info", "registry mirror: %{http_code}", "$CI_REGISTRY_MIRROR/v2/"},
		"../job-long.json":          {`echo "tick $i"`},
	} {
		t.Run(file, func(t *testing.T) {
			f, err := os.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			job, err := decodeJob(f)
			if err != nil {
				t.Fatal(err)
			}
			if job.Image.Name != "flr-arm64" || job.Variables.Get("GIT_STRATEGY") != "none" || len(job.Steps) != 1 {
				t.Fatalf("image %q, GIT_STRATEGY %q, %d steps; want flr-arm64, none, 1",
					job.Image.Name, job.Variables.Get("GIT_STRATEGY"), len(job.Steps))
			}
			script := strings.Join(job.Steps[0].Script, "\n")
			for _, want := range wants {
				if !strings.Contains(script, want) {
					t.Errorf("script lacks %q:\n%s", want, script)
				}
			}
		})
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
	var handed struct{ Token string }
	err = json.NewDecoder(resp.Body).Decode(&handed)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("job request = %d (%v), want 201", resp.StatusCode, err)
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

	checkPlain(t, srv.URL, map[string]string{
		"/fake/jobs/1/status":         "running\n",
		"/fake/jobs/1/failure_reason": "",
		"/fake/jobs/2/status":         "pending\n",
		"/fake/jobs/2/failure_reason": "",
		"/fake/jobs/3/status":         "404 page not found\n",
	})

	// The Runner fails job 1, as it does when the Job's Host goes.
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v4/jobs/1", strings.NewReader(
		`{"token":"`+handed.Token+`","state":"failed","failure_reason":"runner_system_failure"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("job update = %d, want 200", resp.StatusCode)
	}
	checkPlain(t, srv.URL, map[string]string{
		"/fake/jobs/1/status":         "failed\n",
		"/fake/jobs/1/failure_reason": "runner_system_failure\n",
	})
}

// checkPlain checks the plain text answer of each GET path.
func checkPlain(t *testing.T, url string, want map[string]string) {
	t.Helper()
	for path, w := range want {
		resp, err := http.Get(url + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(b) != w {
			t.Errorf("GET %s = %q, want %q", path, b, w)
		}
	}
}
