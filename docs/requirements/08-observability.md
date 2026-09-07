# Observability {#observability}

This document specifies logging, metrics, health endpoints and the
annotations the Runner writes into Job logs. The aim is that an operator
can tell from metrics alone whether Jobs are waiting on GitLab, on warm
capacity, on MicroVM creation or on Host health.

## Logging {#logging}

- **OB-001** The Runner SHALL write structured logs in JSON or text format as
  configured, using the standard library `slog` package.
- **OB-002** The Runner SHALL attach the Job id, Profile name, MicroVM uid
  and Host name to every log record emitted while handling a Job.
- **OB-003** The Runner SHALL log at `info` level each Reservation refusal
  reason at most once per minute per reason to avoid flooding when capacity
  is exhausted.
- **OB-004** The Runner SHALL log at `warn` level every overflow creation
  with the name of the exhausted Pool and the cause.

## Metrics {#metrics}

- **OB-010** The Runner SHALL expose Prometheus metrics on the configured
  listen address at the `/metrics` path.
- **OB-011** The Runner SHALL expose the standard metrics of the
  gitlab-runner packages alongside its own.
- **OB-012** The Runner SHALL expose a counter of Jobs by final state and by
  MicroVM source.
- **OB-013** The Runner SHALL expose a histogram of allocation duration
  labelled by Profile and source.
- **OB-014** The Runner SHALL expose a histogram of the time from allocation
  to guest readiness labelled by Profile and source.
- **OB-015** The Runner SHALL expose a counter of Reservation refusals
  labelled by reason.
- **OB-016** The Runner SHALL expose a gauge of available warm MicroVMs per
  Pool.
- **OB-017** The Runner SHALL expose gauges of active Overflow MicroVMs and
  active Leases.
- **OB-018** The Runner SHALL expose a gauge of Host health per Host and a
  gauge of Runner-owned MicroVMs per Host.
- **OB-019** The Runner SHALL expose a histogram of `CreateMicroVM` to
  `CREATED` latency labelled by Host.
- **OB-020** The Runner SHALL expose counters of garbage-collected MicroVMs,
  failed release operations, Guest Transport stream failures and Lease
  heartbeat failures.

## Health {#health}

- **OB-030** The Runner SHALL serve a liveness endpoint at `/healthz` that
  returns success while the process is running.
- **OB-031** The Runner SHALL serve a readiness endpoint at `/readyz` that
  returns success only after the runner token has been verified, the
  Orchestrator and the Pool Manager have each answered at least once and at
  least one Host is healthy.
- **OB-032** The readiness endpoint SHALL report, in its response body, the
  number of healthy Hosts, whether the Orchestrator is reachable and whether
  the Pool Manager is reachable.

## Job log annotations {#job-log-annotations}

- **OB-040** The Runner SHALL write a collapsible section named
  `flintlock_prepare` to the Job log covering Profile resolution, MicroVM
  allocation and guest readiness.
- **OB-041** The `flintlock_prepare` section SHALL state the Profile name,
  the MicroVM source, the Host name, the MicroVM uid and the time to
  readiness.
- **OB-042** When an allocation fails, the Runner SHALL write the reason to
  the Job log in plain language before the Job is failed.
- **OB-043** The Runner SHALL NOT write Host endpoints, tokens or internal
  addresses to the Job log.
