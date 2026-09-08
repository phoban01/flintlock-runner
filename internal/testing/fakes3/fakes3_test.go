package fakes3

import (
	"errors"
	"testing"
)

func TestSkeleton(t *testing.T) {
	t.Parallel()
	s := New()
	if len(s.Objects()) != 0 {
		t.Error("new store should be empty")
	}
	if err := s.Start(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Start error = %v, want ErrUnsupported", err)
	}
	if s.Endpoint() != "" {
		t.Error("endpoint before Start should be empty")
	}
}
