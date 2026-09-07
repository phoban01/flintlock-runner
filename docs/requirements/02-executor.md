# Executor {#executor}

This document specifies the flintlock Executor: the gitlab-runner executor
implementation that runs each Job inside its own MicroVM, and the Guest
Transport it uses to run Stage scripts in the guest. Capacity decisions,
Profile resolution and MicroVM acquisition are delegated to the Scheduler
and are specified in `03-scheduler.md`; this document only states how the
Executor calls into it.

## Interface {#interface}

- **EX-001** The Executor SHALL implement the `ExecutorProvider` and
  `Executor` interfaces of the gitlab-runner `common` package.
- **EX-002** The Executor SHALL be registered in the provider registry under
  the executor name `flintlock`.
- **EX-003** When the run loop calls `Acquire`, the Executor SHALL request a
  Reservation from the Scheduler and return it as the executor data.
- **EX-004** If the Scheduler refuses a Reservation, then the Executor SHALL
  return an error that the run loop treats as no free executor, so that no
  Job is requested from GitLab.
- **EX-005** When the run loop calls `Release`, the Executor SHALL release the
  Reservation held in the executor data.
- **EX-006** The Executor SHALL report the shell name `bash` and the feature
  set listed in the GitLab protocol document from `GetFeatures` and
  `GetDefaultShell`.

## Prepare {#prepare}

- **EX-010** When `Prepare` is called, the Executor SHALL ask the Scheduler to
  resolve a Profile for the Job.
- **EX-011** If no Profile can be resolved for the Job, then the Executor
  SHALL fail the Job with the failure reason `runner_unsupported` and a log
  line naming the requested Job Image.
- **EX-012** When a Profile has been resolved, the Executor SHALL ask the
  Scheduler to allocate a MicroVM for the Job and the Profile, passing the
  Job's context so that cancellation aborts the allocation.
- **EX-013** If allocation fails, then the Executor SHALL fail the Job with
  the failure reason `runner_system_failure` and a log line stating the
  allocation error.
- **EX-014** When a MicroVM has been allocated, the Executor SHALL wait until
  the Guest Transport reports the guest ready or the Profile's ready timeout
  elapses.
- **EX-015** If the guest is not ready when the ready timeout elapses, then
  the Executor SHALL release the MicroVM to the Scheduler and fail the Job
  with the failure reason `runner_system_failure`.
- **EX-016** When the guest is ready, the Executor SHALL create the Profile's
  builds directory and cache directory in the guest if they do not exist.
- **EX-017** The Executor SHALL complete `Prepare` within the configured
  prepare timeout or fail the Job.
- **EX-018** The Executor SHALL set the shell script configuration so that
  the builds directory, cache directory and helper binary path used in
  generated scripts are those of the Profile.
- **EX-019** The Executor SHALL write a collapsible section to the Job log
  during `Prepare` that names the Profile, whether the MicroVM was warm or
  overflow, the Host name, the MicroVM uid and the time taken to become
  ready.

The helper binary path matters because the generated `upload_artifacts`,
`download_artifacts`, `restore_cache` and `archive_cache` scripts invoke
`gitlab-runner-helper`; root filesystem images used by a Profile carry that
binary at the configured path.

## Run {#run}

- **EX-020** When `Run` is called for a Stage, the Executor SHALL execute the
  Profile's shell inside the MicroVM through the Guest Transport with the
  Stage script supplied on standard input.
- **EX-021** The Executor SHALL stream the standard output and standard
  error of a Stage to the Job log as they are produced rather than after the
  Stage completes.
- **EX-022** When a Stage's command exits with a non-zero status, the
  Executor SHALL return a build error carrying that exit code so that the
  Runner reports it to GitLab.
- **EX-023** If the Guest Transport stream fails before the Stage's exit
  status is known, then the Executor SHALL return a system error and SHALL
  NOT re-run the Stage.
- **EX-024** When the Job's context is cancelled while a Stage is running,
  the Executor SHALL terminate the Stage's process in the guest and return
  within the configured graceful kill timeout.
- **EX-025** The Executor SHALL run each Stage with the working directory set
  to the Profile's builds directory.
- **EX-026** The Executor SHALL run each Stage as the user named by the
  Profile, defaulting to `root`.
- **EX-027** The Executor SHALL deliver every Job-specific value to the guest
  through the Stage scripts sent over the Guest Transport and SHALL NOT place
  Job-specific values in MicroVM metadata.

EX-027 keeps warm and Overflow MicroVMs identical from the guest's point of
view: a warm MicroVM was booted before the Job existed, so nothing about the
Job can be in its cloud-init data, and Overflow MicroVMs follow the same
rule so that both paths are exercised by the same tests.

## Finish and cleanup {#finish-and-cleanup}

- **EX-030** When `Cleanup` is called, the Executor SHALL release the Job's
  MicroVM to the Scheduler regardless of whether the Job succeeded, failed or
  was cancelled.
- **EX-031** The Executor SHALL NOT run a second Job in a MicroVM that has
  run a Job.
- **EX-032** If `Prepare` fails after a MicroVM was allocated, then the
  Executor SHALL release that MicroVM to the Scheduler before returning.
- **EX-033** Where the keep-on-failure debug option is enabled, the Executor
  SHALL ask the Scheduler to retain the MicroVM of a failed Job instead of
  releasing it and SHALL write the MicroVM uid and Host name to the Job log.

## Guest Transport {#guest-transport}

- **EX-040** The Guest Transport SHALL provide an operation that runs a
  command in a MicroVM with a working directory, environment, user, standard
  input stream, standard output stream, standard error stream and returns the
  command's exit status.
- **EX-041** The Guest Transport SHALL provide a readiness operation that
  succeeds only when a trivial command can be executed in the guest.
- **EX-042** The Guest Transport SHALL be selectable per Profile from the
  implementations `exec` and `ssh`, with `exec` as the default.
- **EX-043** The `exec` Guest Transport SHALL use the flintlock
  `MicroVMExec.ExecCommand` streaming RPC of the Host that runs the MicroVM.
- **EX-044** The `exec` Guest Transport SHALL send an `ExecStart` message
  with `has_stdin` set, followed by the Stage script as `stdin` chunks,
  followed by `stdin_eof`.
- **EX-045** The `exec` Guest Transport SHALL treat an `error` payload in the
  response stream as a transport failure and SHALL treat an `exit_code`
  payload as the command's exit status.
- **EX-046** The `exec` Guest Transport SHALL set the `timeout_seconds` field
  of `ExecStart` from the remaining time on the operation's context.
- **EX-047** The `ssh` Guest Transport SHALL connect to the guest's SSH
  service through the flintlock `MicroVMSSHProxy.SSHProxy` streaming RPC of
  the Host that runs the MicroVM.
- **EX-048** Where a Profile sets a direct SSH address mode, the `ssh` Guest
  Transport SHALL connect over TCP to the address reported for the MicroVM's
  first network interface instead of using the proxy RPC.
- **EX-049** The `ssh` Guest Transport SHALL authenticate with the private
  key named by the Profile and SHALL verify the guest host key only where the
  Profile provides a known host key.
- **EX-050** The Guest Transport SHALL reuse the Host client's gRPC
  connection rather than opening a new connection per Stage.
- **EX-051** If the Host that runs the MicroVM becomes unreachable, then the
  Guest Transport SHALL fail the in-flight operation within the configured
  transport deadline.

The `exec` transport is the default because the exec service is served by
`flintlockd` over the MicroVM's vsock and needs no guest networking that is
reachable from the Control Node. On EC2, guest addresses live behind a
host-local bridge and are not routable in the VPC, so a TCP transport would
require extra plumbing on every Host.
