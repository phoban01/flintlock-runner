// Command fake-gitlab serves the fake GitLab of internal/testing/fakegitlab
// on a TCP address, for a Runner that runs outside the test process: the run
// of a Job on a real Host (hack/real-host/README.md).
//
// It starts with no Job. POST /fake/jobs queues one, a spec.Job in GitLab's
// JSON wire form, and answers with the id it gives the Job. GET /fake/jobs
// returns the fake's record of every Job handed out: its status, states and
// trace; GET /fake/jobs/{id}/status and /fake/jobs/{id}/trace return one
// Job's status and trace as plain text. It logs each Job's record once the
// Job ends. The runner token comes from FAKE_GITLAB_RUNNER_TOKEN.
//
// It is a test tool. It keeps its state in memory, serves plain HTTP and
// checks nothing but the runner and Job tokens.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

const (
	// tokenEnv names the environment variable that holds the runner token.
	tokenEnv = "FAKE_GITLAB_RUNNER_TOKEN"
	// maxJobBytes bounds the body of POST /fake/jobs.
	maxJobBytes = 1 << 20
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fake-gitlab:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("fake-gitlab", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "the address to serve on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return fmt.Errorf("%s is empty", tokenEnv)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	gl := fakegitlab.New(fakegitlab.Options{RunnerToken: token})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler(gl, log), ReadHeaderTimeout: 10 * time.Second}
	log.Info("Serving the fake GitLab", "address", ln.Addr().String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go logFinished(ctx, log, os.Stdout, gl)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// decodeJob reads one Job from its JSON wire form.
func decodeJob(r io.Reader) (*spec.Job, error) {
	var job spec.Job
	if err := json.NewDecoder(io.LimitReader(r, maxJobBytes)).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

// handler serves GitLab's Runner API from the fake; POST /fake/jobs, which
// queues a Job; and GET /fake/jobs, the fake's records.
func handler(gl *fakegitlab.Server, log *slog.Logger) http.Handler {
	var (
		mu     sync.Mutex
		nextID int64
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fake/jobs", func(w http.ResponseWriter, r *http.Request) {
		job, err := decodeJob(r.Body)
		if err != nil {
			http.Error(w, "decode job: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Each Job gets a new id and token, so the same file queues a new
		// Job every time.
		mu.Lock()
		nextID++
		job.ID, job.Token = nextID, ""
		mu.Unlock()
		if err := gl.Enqueue(job); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		log.Info("Queued a Job", "job", job.ID, "image", job.Image.Name)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]int64{"id": job.ID})
	})
	mux.HandleFunc("GET /fake/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(summaries(gl.Records()))
	})
	// The status and the trace of one Job as plain text, for a script
	// without a JSON parser. A Job still in the queue is "pending".
	mux.HandleFunc("GET /fake/jobs/{id}/{field}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad job id", http.StatusBadRequest)
			return
		}
		mu.Lock()
		queued := id >= 1 && id <= nextID
		mu.Unlock()
		rec := gl.Record(id)
		if rec == nil && !queued {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch r.PathValue("field") {
		case "status":
			status := "pending"
			if rec != nil {
				status = rec.Status
			}
			_, _ = fmt.Fprintln(w, status)
		case "trace":
			if rec != nil {
				_, _ = io.WriteString(w, rec.Trace)
			}
		default:
			http.NotFound(w, r)
		}
	})
	mux.Handle("/", gl.Handler())
	return mux
}

// summary is what GET /fake/jobs says about one Job.
type summary struct {
	ID            int64    `json:"id"`
	Status        string   `json:"status"`
	States        []string `json:"states"`
	FailureReason string   `json:"failure_reason,omitempty"`
	ExitCode      int      `json:"exit_code"`
	Trace         string   `json:"trace"`
}

func summaries(recs []*fakegitlab.JobRecord) []summary {
	out := make([]summary, 0, len(recs))
	for _, r := range recs {
		out = append(out, summary{
			ID: r.ID, Status: r.Status, States: r.States,
			FailureReason: r.FailureReason, ExitCode: r.ExitCode, Trace: r.Trace,
		})
	}
	return out
}

// finished reports whether GitLab has the Job's final state.
func finished(status string) bool {
	switch status {
	case fakegitlab.StatusSuccess, fakegitlab.StatusFailed, fakegitlab.StatusCanceled:
		return true
	}
	return false
}

// logFinished logs each Job's status, and writes its trace to out, once,
// when the Job ends.
func logFinished(ctx context.Context, log *slog.Logger, out io.Writer, gl *fakegitlab.Server) {
	logged := map[int64]bool{}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, r := range gl.Records() {
			if logged[r.ID] || !finished(r.Status) {
				continue
			}
			logged[r.ID] = true
			log.Info("Job finished", "job", r.ID, "status", r.Status, "failureReason", r.FailureReason, "exitCode", r.ExitCode)
			_, _ = fmt.Fprintf(out, "--- trace of job %d ---\n%s\n--- end of trace of job %d ---\n", r.ID, r.Trace, r.ID)
		}
	}
}
