package poolmgr

import (
	"context"
	"time"
)

// jobTimeoutKey is the context key of WithJobTimeout.
type jobTimeoutKey struct{}

// WithJobTimeout returns a context that carries the timeout of the Job a
// claim is for. Lease.ClaimVM has no parameter for it because battery has no
// use for one; the Kubernetes pool backend reads it to set the claimed pod's
// active deadline (KF-127). A backend that has no use for it ignores it.
func WithJobTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, jobTimeoutKey{}, timeout)
}

// JobTimeoutFrom returns the Job timeout a context carries, and whether it
// carries one that is positive.
func JobTimeoutFrom(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(jobTimeoutKey{}).(time.Duration)
	return d, ok && d > 0
}
