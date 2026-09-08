package fakegitlab

import (
	"errors"
	"testing"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
)

func TestSkeleton(t *testing.T) {
	t.Parallel()
	s := New(Options{RunnerToken: "glrt-test"})
	s.Enqueue(&spec.Job{ID: 7})
	if s.Pending() != 1 {
		t.Errorf("Pending = %d, want 1", s.Pending())
	}
	if err := s.Start(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Start error = %v, want ErrUnsupported", err)
	}
	if s.Record(7) != nil {
		t.Error("skeleton should record nothing")
	}
}
