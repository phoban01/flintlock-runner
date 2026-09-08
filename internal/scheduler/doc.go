// Package scheduler is the scheduling component inserted between the
// gitlab-runner run loop and the Executor (docs/requirements/03-scheduler.md).
// It decides whether the Runner may ask GitLab for a Job (Reservations),
// which Profile a Job needs, claims a warm MicroVM from that Profile's Pool
// (Allocations), resolves which Host runs it (Placements), keeps the Lease
// alive and releases it. It never creates, deletes or places a MicroVM.
//
// Every dependency is injected through Deps and every one is an interface
// with an in-memory implementation: the Pool Manager client and the PL policy
// units from internal/poolmgr, the Host Registry from internal/flintlock,
// time from internal/clock and metrics through a small Metrics sink. Logs go
// to a slog.Logger; the once-per-minute throttles of OB-003 and OB-004 run on
// the injected Clock, so tests assert them with a recording slog.Handler and
// a fake clock, with no sleeping and no Prometheus registry.
//
// The Scheduler interface is the union of five small interfaces. The
// Executor takes only the three it calls; the readiness endpoint takes Status;
// main takes Lifecycle. The package takes JobInfo rather than spec.Job so
// that neither it nor its tests import gitlab-runner.
package scheduler
