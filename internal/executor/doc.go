// Package executor is the flintlock executor
// (docs/requirements/02-executor.md): the implementation of gitlab-runner's
// common.ExecutorProvider and common.Executor that runs each Job inside its
// own MicroVM (EX-001), registered under the name "flintlock" (EX-002).
//
// The Executor is the one package that has to import gitlab-runner, so it is
// kept thin: it translates the run loop's calls into calls on three
// interfaces, Scheduler, transport.Factory and HostServiceEnvResolver, each
// of which has an in-memory fake. Executor unit tests therefore run with a
// fake Scheduler and a recording Transport and assert on the exact scripts,
// directories and environment sent to the guest.
//
// Aborts decided by the Scheduler (SC-043, SC-061, SC-062) reach the Build
// through Data, which implements common.WithContext: the run loop calls it
// after Prepare and before the first Stage, and the derived context is
// cancelled when the Allocation's Handle is done.
package executor
