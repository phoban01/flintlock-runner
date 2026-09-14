package remote

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// InstanceError is one instance's failure in Fan.
type InstanceError struct {
	Instance fleet.Instance
	Err      error
}

func (e InstanceError) Error() string { return e.Instance.ID + ": " + e.Err.Error() }

func (e InstanceError) Unwrap() error { return e.Err }

//= docs/requirements/06-fleet.md#remote-execution
//# If provisioning fails on one instance, then the Fleet Controller
//# SHALL continue provisioning the others and SHALL exit with a non-zero
//# status that summarises every failure.

// Failures is every failed instance of a Fan, in instance order. Its message
// is the summary the Fleet Controller prints before exiting non-zero;
// errors.Is and errors.As see each instance's error.
type Failures []InstanceError

func (f Failures) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d instance(s) failed:", len(f))
	for _, e := range f {
		b.WriteString("\n  ")
		b.WriteString(e.Error())
	}
	return b.String()
}

// Unwrap returns every instance's error.
func (f Failures) Unwrap() []error {
	out := make([]error, len(f))
	for i, e := range f {
		out[i] = e
	}
	return out
}

//= docs/requirements/06-fleet.md#remote-execution
//# The Fleet Controller SHALL provision instances in parallel with
//# the configured parallelism limit.

// Fan runs fn on every instance with at most parallelism running at once
// (FL-012); below one means one. A failure on one instance does not stop the
// others (FL-013). Results are in instance order, with fn's result kept even
// when it failed. The error is nil when every fn succeeded and Failures
// otherwise; an instance not started because ctx ended fails with ctx's
// error.
func Fan[T any](ctx context.Context, insts []fleet.Instance, parallelism int, fn func(context.Context, fleet.Instance) (T, error)) ([]T, error) {
	if parallelism < 1 {
		parallelism = 1
	}
	results := make([]T, len(insts))
	errs := make([]error, len(insts))
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, inst := range insts {
		if err := ctx.Err(); err != nil {
			errs[i] = err
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			errs[i] = ctx.Err()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i], errs[i] = fn(ctx, inst)
		}()
	}
	wg.Wait()
	var failed Failures
	for i, err := range errs {
		if err != nil {
			failed = append(failed, InstanceError{Instance: insts[i], Err: err})
		}
	}
	if len(failed) > 0 {
		return results, failed
	}
	return results, nil
}
