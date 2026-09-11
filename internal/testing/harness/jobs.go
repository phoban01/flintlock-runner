package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// Job is a CI Job as a scenario describes it. BuildJob turns it into the
// spec.Job payload the fake GitLab hands out.
type Job struct {
	// Name is the Job name; empty means "harness".
	Name string
	// Script is the `script:` section, one shell line per entry.
	Script []string
	// AfterScript is the `after_script:` section.
	AfterScript []string
	// Timeout is the Job timeout (GL-042); zero means ten minutes.
	Timeout time.Duration
	// Image is the Job Image; empty selects the Default Profile (SC-010).
	Image string
	// Variables are added to the Job's variables, after the ones the
	// harness sets, so they can override them.
	Variables map[string]string
}

// defaultJobTimeout is the Job timeout when Job.Timeout is zero.
const defaultJobTimeout = 10 * time.Minute

// BuildJob returns the spec.Job payload for j with the given id: the
// script and after_script Steps, the Job timeout, a GitLab-like set of
// predefined variables and GIT_STRATEGY=none, so that get_sources makes the
// project directory without cloning a repository that does not exist. The
// Job carries no artifacts and no cache, which need gitlab-runner-helper.
func BuildJob(id int64, j Job) *spec.Job {
	name := j.Name
	if name == "" {
		name = "harness"
	}
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	idStr := strconv.FormatInt(id, 10)
	vars := map[string]string{
		"CI":                 "true",
		"GITLAB_CI":          "true",
		"CI_JOB_ID":          idStr,
		"CI_JOB_NAME":        name,
		"CI_JOB_STAGE":       "test",
		"CI_PIPELINE_ID":     idStr,
		"CI_PROJECT_ID":      "1",
		"CI_PROJECT_NAME":    "demo",
		"CI_PROJECT_PATH":    "harness/demo",
		"CI_COMMIT_SHA":      "0123456789abcdef0123456789abcdef01234567",
		"CI_COMMIT_REF_NAME": "main",
		"GIT_STRATEGY":       "none",
	}
	for k, v := range j.Variables {
		vars[k] = v
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	variables := make(spec.Variables, 0, len(keys))
	for _, k := range keys {
		variables = append(variables, spec.Variable{Key: k, Value: vars[k], Public: true})
	}

	steps := spec.Steps{{
		Name:    spec.StepNameScript,
		Script:  spec.StepScript(append([]string(nil), j.Script...)),
		Timeout: secs,
		When:    spec.StepWhenOnSuccess,
	}}
	if len(j.AfterScript) > 0 {
		steps = append(steps, spec.Step{
			Name:    spec.StepNameAfterScript,
			Script:  spec.StepScript(append([]string(nil), j.AfterScript...)),
			Timeout: secs,
			When:    spec.StepWhenAlways,
		})
	}
	return &spec.Job{
		ID:            id,
		Token:         "glcbt-harness-" + idStr,
		AllowGitFetch: true,
		JobInfo: spec.JobInfo{
			Name:            name,
			Stage:           "test",
			PipelineID:      id,
			ProjectID:       1,
			ProjectName:     "demo",
			ProjectFullPath: "harness/demo",
		},
		GitInfo: spec.GitInfo{
			RepoURL: "http://gitlab.invalid/harness/demo.git",
			Ref:     "main",
			Sha:     vars["CI_COMMIT_SHA"],
			RefType: spec.RefTypeBranch,
		},
		RunnerInfo: spec.RunnerInfo{Timeout: secs},
		Steps:      steps,
		Image:      spec.Image{Name: j.Image},
		Variables:  variables,
	}
}

// Enqueue queues j on the fake GitLab under the next Job id and returns
// the id.
func (s *Stack) Enqueue(j Job) (int64, error) {
	s.mu.Lock()
	id := s.nextJobID
	s.nextJobID++
	s.mu.Unlock()
	if err := s.GitLab.Enqueue(BuildJob(id, j)); err != nil {
		return 0, err
	}
	s.logf("job %d (%s) queued", id, j.Name)
	return id, nil
}

// Final reports whether a Job status is terminal on the GitLab side.
func Final(status string) bool {
	switch status {
	case fakegitlab.StatusSuccess, fakegitlab.StatusFailed, fakegitlab.StatusCanceled:
		return true
	}
	return false
}

// pollInterval paces Wait and Follow.
const pollInterval = 100 * time.Millisecond

// Wait blocks until Job id reaches a final state and returns what the fake
// GitLab recorded for it. It fails early, with the end of the Runner's log,
// if the Runner exits meanwhile.
func (s *Stack) Wait(ctx context.Context, id int64) (*fakegitlab.JobRecord, error) {
	return s.Follow(ctx, id, nil)
}

// Follow is Wait that also hands each new piece of the Job's trace to
// onTrace as the Runner patches it in (GL-050), which is how a caller
// streams a Job log while the Job runs. onTrace may be nil.
func (s *Stack) Follow(ctx context.Context, id int64, onTrace func(chunk string)) (*fakegitlab.JobRecord, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	exited := s.runnerExited()
	sent := 0
	emit := func(rec *fakegitlab.JobRecord) {
		if onTrace == nil || rec == nil || len(rec.Trace) <= sent {
			return
		}
		onTrace(rec.Trace[sent:])
		sent = len(rec.Trace)
	}
	for {
		rec := s.GitLab.Record(id)
		emit(rec)
		if rec != nil && Final(rec.Status) {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return rec, fmt.Errorf("harness: waiting for job %d: %w", id, context.Cause(ctx))
		case <-exited:
			// One last look: the Runner may have finished the Job and
			// then exited.
			rec = s.GitLab.Record(id)
			emit(rec)
			if rec != nil && Final(rec.Status) {
				return rec, nil
			}
			return rec, errors.Join(fmt.Errorf("harness: job %d did not finish", id), s.runnerExitError())
		case <-ticker.C:
		}
	}
}
