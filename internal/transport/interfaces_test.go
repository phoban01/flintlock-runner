package transport

import (
	"errors"
	"fmt"
	"testing"
)

func TestStreamFailureIsDistinguishable(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("exec: %w", ErrStreamFailed)
	if !errors.Is(err, ErrStreamFailed) {
		t.Error("wrapped ErrStreamFailed not matched")
	}
	if errors.Is(err, ErrNotReady) || errors.Is(ErrNotReady, ErrServiceDisabled) {
		t.Error("sentinels overlap")
	}
}
