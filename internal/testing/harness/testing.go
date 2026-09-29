package harness

import (
	"context"
	"errors"
	"testing"
)

// New starts a Stack for a test and registers its Shutdown as a cleanup
// that fails the test on any leak (TD-054). The Runner is not started;
// call StartRunner. Harness steps are logged through t.Logf unless opts
// sets Logf. A claim stack without the envtest binaries skips the test;
// with FLINTLOCK_RUNNER_REQUIRE_ENVTEST set to 1 it fails instead.
func New(t testing.TB, opts Options) *Stack {
	t.Helper()
	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	s, err := Start(context.Background(), opts)
	if errors.Is(err, ErrNoEnvtest) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatalf("starting the harness: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Errorf("harness shutdown: %v", err)
		}
	})
	return s
}
