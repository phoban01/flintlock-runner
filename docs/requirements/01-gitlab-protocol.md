# GitLab protocol {#gitlab-protocol}

This document specifies how the Runner communicates with GitLab. The Runner
is built from the gitlab-runner Go packages, so most of the wire protocol
(long polling, trace patching, artifact upload, cancellation) is provided by
the `network` and `common` packages. The requirements here pin down which of
that behaviour the Runner relies on and which choices the Runner makes
itself, so that a reader of the code can see where each obligation is met.
Requirements that are satisfied entirely by the imported packages are cited
in code with duvet `implication` annotations that name the package.

## Library basis {#library-basis}

- **GL-001** The Runner SHALL be a single Go program that imports the
  gitlab-runner `common`, `network`, `shells` and `executors` packages and
  SHALL NOT execute a `gitlab-runner` binary.
- **GL-002** The Runner SHALL perform every request to the GitLab API through
  the `GitLabClient` type of the gitlab-runner `network` package.
- **GL-003** The Runner SHALL pin the gitlab-runner module to an explicit
  commit and record that commit in `go.mod`.
- **GL-004** The Runner SHALL drive job acquisition and execution through the
  gitlab-runner run loop (`commands.NewRunCommand`) with a provider registry
  that contains the Executor, so that graceful shutdown, token rotation and
  the session server come from the library unchanged.

The reason for GL-004 is that the run loop already calls
`ExecutorProvider.Acquire` before it asks GitLab for a job, which is
precisely the insertion point the Scheduler needs. Reimplementing the loop
would duplicate roughly a thousand lines of shutdown, retry and health logic
for no gain.

## Authentication {#authentication}

- **GL-010** The Runner SHALL authenticate to GitLab with a runner
  authentication token (a token beginning with `glrt-`) supplied by
  configuration.
- **GL-011** The Runner SHALL NOT call the runner registration endpoint
  `POST /api/v4/runners`.
- **GL-012** When starting, the Runner SHALL load a persistent system
  identifier from its state directory or generate and store one if none
  exists.
- **GL-013** The Runner SHALL send its system identifier as `system_id` on
  every runner-scoped request to GitLab.
- **GL-014** When starting, the Runner SHALL call `POST /api/v4/runners/verify`
  with the full `info` payload before it requests any Job.
- **GL-015** If token verification returns a forbidden response, then the
  Runner SHALL log the failure and exit with a non-zero status.
- **GL-016** Where GitLab reports a token expiry time, the Runner SHALL rotate
  the token through `POST /api/v4/runners/reset_authentication_token` before
  that time is reached.

Verification is done eagerly, and with the `info` payload, because GitLab
records the runner manager's features at that moment; a runner that only
sends features on the job request is treated as not supporting graceful
cancellation for up to an hour.

## Advertised capabilities {#advertised-capabilities}

- **GL-020** The Runner SHALL advertise the executor name `flintlock` in the
  `info.executor` field of every job request.
- **GL-021** The Runner SHALL advertise `bash` as its default shell.
- **GL-022** The Runner SHALL advertise the features `variables`, `image`,
  `refspecs`, `masking`, `raw_variables`, `artifacts`, `artifacts_exclude`,
  `upload_multiple_artifacts`, `upload_raw_artifacts`, `cache`,
  `fallback_cache_keys`, `multi_build_steps`, `return_exit_code`,
  `trace_reset`, `trace_checksum`, `trace_size`, `cancelable` and
  `cancel_gracefully`.
- **GL-023** The Runner SHALL NOT advertise the `services`, `session`,
  `terminal`, `proxy` or `shared` features.
- **GL-024** If a Job declares services, then the Runner SHALL fail the Job
  with the failure reason `runner_unsupported` and a log line stating that
  services are not supported by this executor.

The `image` feature is advertised even though no container image is pulled:
the Runner reads the Job Image only to select a Profile. Services are not
supported because there is no container runtime in the guest by default;
Jobs that need auxiliary services run them from their own script.

## Job acquisition {#job-acquisition}

- **GL-030** The Runner SHALL request a Job from GitLab only after the
  Scheduler has granted a Reservation for it.
- **GL-031** When GitLab returns no job, the Runner SHALL send the
  `X-GitLab-Last-Update` value it received as `last_update` in the next
  request so that GitLab long-polls instead of returning immediately.
- **GL-032** When GitLab returns no job, the Runner SHALL release the
  Reservation before the next request is made.
- **GL-033** The Runner SHALL run no more Jobs concurrently than the
  configured concurrency limit.
- **GL-034** When a Job is received, the Runner SHALL report the Job as
  `running` to GitLab before any Stage executes.
- **GL-035** If the transition to `running` is rejected by GitLab, then the
  Runner SHALL abandon the Job without executing any Stage and SHALL release
  its Reservation.

## Job execution {#job-execution}

- **GL-040** The Runner SHALL execute the Stages of a Job in the order
  defined by the gitlab-runner `common` package.
- **GL-041** The Runner SHALL generate each Stage script with the `bash`
  shell implementation of the gitlab-runner `shells` package.
- **GL-042** The Runner SHALL enforce the Job timeout carried in the Job's
  `runner_info.timeout` field by cancelling the Job's context when it elapses.
- **GL-043** When a Job's context is cancelled because its timeout elapsed,
  the Runner SHALL report the failure reason `job_execution_timeout`.
- **GL-044** The Runner SHALL clone the repository inside the MicroVM using
  the Job token embedded by the generated `get_sources` script.
- **GL-045** The Runner SHALL upload Job artifacts to GitLab with the Job
  token from within the MicroVM using the helper invocation generated by the
  shell.
- **GL-046** The Runner SHALL download dependency artifacts with each
  dependency's own token as provided in the Job payload.

Artifacts and caches are handled by the `gitlab-runner-helper` binary that
the generated scripts invoke; the Profile's root filesystem image therefore
has to contain a helper binary at the path the shell expects, which the
Executor document specifies.

## Job log {#job-log}

- **GL-050** While a Job executes, the Runner SHALL stream the Job log to
  GitLab with `PATCH /api/v4/jobs/:id/trace` requests carrying a
  `Content-Range` header.
- **GL-051** The Runner SHALL honour the `X-GitLab-Trace-Update-Interval`
  header when choosing the interval between trace patches.
- **GL-052** If a trace patch returns a range-not-satisfiable response, then
  the Runner SHALL resend the log from the offset given in the response's
  `Range` header.
- **GL-053** The Runner SHALL mask the values of Job variables flagged as
  masked before they are written to the Job log.
- **GL-054** The Runner SHALL truncate the Job log at the configured output
  limit and record the truncation in the log.

## Cancellation and completion {#cancellation-and-completion}

- **GL-060** When GitLab returns a `Job-Status` header of `canceled` on a
  trace patch or job update, the Runner SHALL stop the current Stage, run the
  `after_script` Stage and report the Job as failed with the reason
  `job_canceled`.
- **GL-061** When GitLab returns a `Job-Status` header of `failed`, the Runner
  SHALL abort the Job immediately without running `after_script`.
- **GL-062** When a Job finishes, the Runner SHALL report the final state,
  the failure reason where applicable, the exit code of the failing Stage and
  the checksum and size of the Job log to GitLab.
- **GL-063** If the final job update returns an accepted-but-pending
  response, then the Runner SHALL retry the update with backoff until GitLab
  confirms it or the configured retry limit is reached.
- **GL-064** If a failure reason is not in the list GitLab advertised as
  supported, then the Runner SHALL report `runner_system_failure` in its
  place.

## Shutdown {#shutdown}

- **GL-070** When the Runner receives `SIGTERM` or `SIGQUIT`, the Runner SHALL
  stop requesting Jobs and release every unconverted Reservation.
- **GL-071** While a graceful shutdown is in progress, the Runner SHALL allow
  running Jobs to finish for up to the configured shutdown timeout.
- **GL-072** If running Jobs have not finished when the shutdown timeout
  elapses, then the Runner SHALL cancel them, report them as failed with the
  reason `runner_system_failure` and release their MicroVMs.
- **GL-073** When the Runner receives a second termination signal during a
  graceful shutdown, the Runner SHALL cancel all running Jobs immediately.
