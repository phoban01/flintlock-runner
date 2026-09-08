package flintlock

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelsAreDistinct(t *testing.T) {
	t.Parallel()
	all := []error{ErrNotFound, ErrUnimplemented, ErrUnavailable, ErrUnauthenticated, ErrUnknownHost}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v matches %v", a, b)
			}
		}
	}
	wrapped := fmt.Errorf("host h1: %w", ErrUnavailable)
	if !errors.Is(wrapped, ErrUnavailable) {
		t.Error("wrapped ErrUnavailable not matched by errors.Is")
	}
}
