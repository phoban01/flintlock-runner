# Test doubles {#test-doubles}

This document specifies the fakes the project builds so that the whole
Runner can be exercised end to end without EC2, without KVM and without a
running battery. During the current phase none of those is available to
development, so the fakes are not a convenience; they are the primary test
environment, and they are held to requirements like any other component.
Each fake is built against the same generated protos and API types as the
real client it stands in for, so an upstream bump breaks the fake and the
client together rather than letting them drift apart.

## Fake Pool Manager {#fake-pool-manager}

- **TD-001** The project SHALL provide a fake Pool Manager that serves the
  `poolmgr.v1alpha1` `PoolAdmin`, `Lease` and `Events` services over gRPC
  using the generated server stubs from the battery module.
- **TD-002** The fake Pool Manager SHALL create, place and delete MicroVMs
  through the same Host client interface the Runner uses, so that it can be
  pointed at fake Hosts or at real `flintlockd` instances.
- **TD-003** The fake Pool Manager SHALL implement the immediate-on-lease,
  minimum-size-threshold and replace-on-delete replenishment strategies as
  declared in the `PoolSpec`.
- **TD-004** The fake Pool Manager SHALL expire a Lease whose last heartbeat
  is older than the Pool's heartbeat expiry threshold and SHALL delete the
  expired Lease's MicroVM.
- **TD-005** The fake Pool Manager SHALL return `RESOURCE_EXHAUSTED` from
  `ClaimVM` when the Pool has no MicroVM in the `AVAILABLE` phase.
- **TD-006** The fake Pool Manager SHALL emit every `Event` type defined in
  the proto at the corresponding phase transition.
- **TD-007** The fake Pool Manager SHALL place MicroVMs across a Pool's
  `flintlock_hosts` by least MicroVM count, matching battery's design.
- **TD-008** The fake Pool Manager SHALL populate the Host name on
  `ClaimVMResponse` when the proto carries a field for it and SHALL provide
  a switch that omits it, so that both Placement paths of the Scheduler are
  tested.
- **TD-009** The fake Pool Manager SHALL be runnable as a standalone binary
  as well as in process, so that a fleet without battery can run on it.
- **TD-010** The fake Pool Manager SHALL support fault injection for claim
  latency, a period of `UNAVAILABLE`, hook failure and a dropped `Events`
  stream.

## Fake Host {#fake-host}

- **TD-020** The project SHALL provide a fake Host that serves the flintlock
  `MicroVM` service, including `ServerInfo`, and the `MicroVMExec` service
  over gRPC using the generated server stubs from the flintlock `api`
  module.
- **TD-021** The fake Host SHALL represent each MicroVM as a sandbox
  directory on the local filesystem and SHALL run each `ExecCommand` as a
  local process rooted in that directory, streaming standard input,
  standard output, standard error and the exit code as `flintlockd` does.
- **TD-022** The fake Host SHALL move a MicroVM from `PENDING` to `CREATED`
  after a configurable boot delay and SHALL populate `vsock_path` in its
  status.
- **TD-023** The fake Host SHALL honour the `cwd`, `env`, `timeout_seconds`,
  `has_stdin` and `stdin_eof` fields of an exec request and SHALL accept
  and ignore `user`.
- **TD-024** The fake Host SHALL enforce basic auth and TLS when configured
  so that the Runner's authentication code is exercised.
- **TD-025** The fake Host SHALL support fault injection for a create that
  ends in `FAILED`, an exec stream dropped before the exit code, and a Host
  that stops answering.
- **TD-026** The fake Host SHALL report a configurable flintlock version and
  service flags from `ServerInfo` and SHALL provide a mode in which
  `ServerInfo` returns `UNIMPLEMENTED`.

## Fake GitLab {#fake-gitlab}

- **TD-030** The project SHALL provide a fake GitLab HTTP server that
  implements runner verification, job request with long polling and the
  `X-GitLab-Last-Update` header, job update, trace patching with
  `Content-Range` and range-not-satisfiable handling, artifact upload and
  dependency artifact download.
- **TD-031** The fake GitLab SHALL hand out Jobs from a queue of `spec.Job`
  payloads supplied by the test and SHALL record every state update and the
  assembled trace for each Job.
- **TD-032** The fake GitLab SHALL be able to cancel a running Job through
  the `Job-Status` header and to answer a final update with an
  accepted-but-pending response a configurable number of times.
- **TD-033** The fake GitLab SHALL require the runner token and a system
  identifier on runner-scoped requests and the Job token on job-scoped
  requests, as the real API does.
- **TD-034** The project SHALL provide a fake object store that accepts the
  pre-signed style requests the gitlab-runner cache client issues, so that
  the `cache:` keyword works end to end against a local endpoint.

## Fake AWS {#fake-aws}

- **TD-040** The Fleet Controller SHALL access EC2 and Systems Manager
  through narrow interfaces of its own so that fakes can stand in for the
  AWS SDK.
- **TD-041** The project SHALL provide fakes for `DescribeInstances`,
  `SendCommand`, `GetCommandInvocation`, `ListCommandInvocations` and
  `GetParameter` that record every call and return results the test
  configures.
- **TD-042** The fake Systems Manager SHALL capture the script content of
  every `SendCommand` so that tests can assert on what would have run on a
  Host.
- **TD-043** Every provisioning script the Fleet Controller sends to a Host
  SHALL pass `bash -n` and `shellcheck` in continuous integration.
- **TD-044** The Fleet Controller SHALL provide a static discovery provider
  that reads Hosts from a list in the configuration, so that provisioning
  over SSH can be exercised against any Linux machine without an AWS
  account.

## End-to-end harness {#end-to-end-harness}

- **TD-050** The project SHALL provide an end-to-end harness that starts the
  fake GitLab, the fake Pool Manager, a configurable number of fake Hosts
  and the real `flintlock-runner` binary, submits Jobs and asserts on the
  recorded trace and final state, and SHALL run in continuous integration
  on a machine without KVM.
- **TD-051** The harness SHALL cover a successful Job, a script failure with
  its exit code, cancellation with `after_script`, a Job timeout, a wait on
  an exhausted Pool, a Host becoming unhealthy during a Job, a Lease
  expiring during a Job, an unknown Job Image, artifact upload and
  dependency download, cache restore and save, and Host Service environment
  injection.
- **TD-052** Where the environment variable naming a hardware Inventory is
  set, the harness SHALL run the same scenarios against the real Hosts it
  lists instead of fake Hosts, and SHALL be skipped otherwise.
- **TD-053** Where the environment variable naming a real Pool Manager
  endpoint is set, the harness SHALL use it instead of the fake Pool
  Manager.
- **TD-054** The harness SHALL fail if any scenario leaves a Lease held or
  a sandbox directory behind after the Runner has shut down.

The hardware tier in TD-052 and TD-053 is how the project moves from fakes
to metal without rewriting tests: the same scenarios, pointed at a static
Inventory of real `flintlockd` hosts, and later at a real battery. Until
someone has such hosts, every test in the project runs on a laptop.
