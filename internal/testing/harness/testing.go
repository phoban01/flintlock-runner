package harness

import (
	"context"
	"testing"
)

// New starts a Stack for a test and registers its Shutdown as a cleanup
// that fails the test on any leak (TD-054). The Runner is not started;
// call StartRunner. Harness steps are logged through t.Logf unless opts
// sets Logf.
func New(t testing.TB, opts Options) *Stack {
	t.Helper()
	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	s, err := Start(context.Background(), opts)
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
