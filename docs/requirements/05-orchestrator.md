# Hosts and Orchestrator {#hosts-and-orchestrator}

This document specifies how the Runner talks to flintlock. All MicroVM
control operations the Runner issues (`CreateMicroVM`, `DeleteMicroVM`,
`ListMicroVMs`) go to the Orchestrator,
[brigade](https://github.com/liquidmetal-dev/brigade), which exposes
flintlock's `MicroVM` gRPC service unchanged and forwards each create to a
Host it selects. The Runner talks to individual Hosts only for the two
things the Orchestrator does not provide: running commands in a guest
(`MicroVMExec`, `MicroVMSSHProxy`) and finding out which Host runs a given
MicroVM (`GetMicroVM`).

## Client {#flintlock-client}

- **BR-001** The Runner SHALL communicate with Hosts and with the Orchestrator
  through the flintlock `microvm.services.api.v1alpha1` gRPC API using the
  generated client from the flintlock `api` module.
- **BR-002** The Runner SHALL treat the Orchestrator endpoint as a `MicroVM`
  service endpoint and SHALL NOT depend on any Orchestrator-specific RPC.
- **BR-003** The Runner SHALL maintain one long-lived gRPC connection per
  Host and per Orchestrator endpoint, with keepalive enabled, and SHALL
  reconnect with exponential backoff when a connection is lost.
- **BR-004** The Runner SHALL apply the configured deadline to every unary
  flintlock call.
- **BR-005** Where a Host is configured with a basic auth token, the Runner
  SHALL send an `authorization` header with the value `Basic` followed by the
  base64 encoding of the token on every call to that Host.
- **BR-006** Where the Orchestrator is configured with a token, the Runner
  SHALL send an `authorization` header on every call to it using the scheme
  named in the configuration, either `Basic` or `Bearer`.
- **BR-007** The Runner SHALL connect to Hosts and to the Orchestrator with
  TLS, verifying the server certificate against the configured certificate
  authority, unless the endpoint is explicitly marked insecure.
- **BR-008** Where client certificate and key files are configured for an
  endpoint, the Runner SHALL present them for mutual TLS.
- **BR-009** The Runner SHALL send `MicroVMExec.ExecCommand` and
  `MicroVMSSHProxy.SSHProxy` requests only to Host endpoints and never to the
  Orchestrator.
- **BR-010** When several Orchestrator endpoints are configured, the Runner
  SHALL use the first healthy endpoint in configuration order and SHALL move
  to the next when a call fails with `UNAVAILABLE`.

Every Orchestrator node accepts the full `MicroVM` service and forwards to
the cluster, so listing more than one endpoint only removes a single point
of failure on the Runner's side; it does not change where MicroVMs land.

## Host inventory {#host-inventory}

- **BR-011** The Runner SHALL load the set of Hosts from the Inventory
  section of its configuration.
- **BR-012** When starting, the Runner SHALL call `ServerInfo` on every Host
  and SHALL log the Host's flintlock version and whether the exec and SSH
  proxy services are enabled.
- **BR-013** If a Host reports that the exec service is disabled, then the
  Runner SHALL log a warning naming the Host, because Jobs placed there by
  the Orchestrator cannot be run over the `exec` Guest Transport.
- **BR-014** If `ServerInfo` is not implemented by a Host, then the Runner
  SHALL treat the Host's version as unknown and continue.
- **BR-015** When the configuration is reloaded, the Runner SHALL add new
  Hosts, stop probing removed Hosts and keep serving Jobs on removed Hosts
  until they finish.
- **BR-016** The Runner SHALL NOT call `CreateMicroVM`, `DeleteMicroVM` or
  `ListMicroVMs` on a Host endpoint directly.

## MicroVM specification {#microvm-specification}

- **BR-020** When creating an Overflow MicroVM, the Runner SHALL build the
  `MicroVMSpec` from the Profile: `vcpu`, `memory_in_mb`, `kernel`,
  `initrd` where set, `root_volume`, `additional_volumes`, `interfaces`,
  `metadata` and `provider` where set.
- **BR-021** The Runner SHALL set the MicroVM `id` to `job-` followed by the
  Job id and the MicroVM `namespace` to the configured Runner namespace.
- **BR-022** The Runner SHALL set `allow_guest_agent` to true on every
  Overflow MicroVM.
- **BR-023** The Runner SHALL label every Overflow MicroVM with the labels
  `gitlab-runner.flintlock.dev/runner-id`, `gitlab-runner.flintlock.dev/job-id`,
  `gitlab-runner.flintlock.dev/project-id`,
  `gitlab-runner.flintlock.dev/profile` and
  `gitlab-runner.flintlock.dev/created-at`.
- **BR-024** The Runner SHALL copy the Profile's architecture, Host selector
  and any declared Orchestrator scheduling constraints onto the MicroVM
  labels with the `brigade.scheduling/` prefix so that the Orchestrator
  places the MicroVM on a suitable Host.
- **BR-025** The Runner SHALL set `add_network_config` on the kernel spec so
  that flintlock generates the guest network configuration from the
  interfaces.
- **BR-026** The Runner SHALL NOT use `eth0` as a network interface
  `device_id`.
- **BR-027** The Runner SHALL base64-encode every value placed in the
  MicroVM `metadata` map and SHALL only use the keys `meta-data`, `user-data`,
  `vendor-data` and `network-config`.
- **BR-028** The Runner SHALL set the `meta-data` entry so that the guest's
  `instance-id` is the MicroVM id and its `local-hostname` is the MicroVM id.
- **BR-029** Where a Profile provides user-data, the Runner SHALL render it as
  a template with the Profile name and MicroVM id available and place the
  result in the `user-data` entry.
- **BR-030** The Runner SHALL build the Pool Manager's MicroVM template for a
  Profile from the same fields and labels as an Overflow MicroVM, omitting
  only the Job-specific `id` and labels.

The same specification builder serves both paths so that a warm MicroVM and
an Overflow MicroVM of the same Profile are indistinguishable to the Job.
The Orchestrator currently pins the flintlock v0.11.0 protos, which is why
`cpu_config` is not part of the Profile; it can be added once the
Orchestrator is rebuilt against a newer API. The `created-at` label exists
because older Hosts may not populate `created_at` on the spec and garbage
collection relies on it.

## Creation and deletion {#creation-and-deletion}

- **BR-040** When creating an Overflow MicroVM, the Runner SHALL call
  `CreateMicroVM` on the Orchestrator once and SHALL then poll `GetMicroVM`
  with the returned uid on the Orchestrator at the configured interval until
  the state is `CREATED` or `FAILED` or the create timeout elapses.
- **BR-041** If a MicroVM reaches the `FAILED` state, then the Runner SHALL
  delete it through the Orchestrator and report a creation error that
  includes the flintlock status message.
- **BR-042** If the create timeout elapses before the MicroVM is `CREATED`,
  then the Runner SHALL delete it through the Orchestrator and report a
  creation error.
- **BR-043** If `CreateMicroVM` returns `ALREADY_EXISTS`, then the Runner
  SHALL delete the existing MicroVM with that id through the Orchestrator and
  retry the create once.
- **BR-044** If `CreateMicroVM` returns `FAILED_PRECONDITION` or
  `RESOURCE_EXHAUSTED` because the Orchestrator has no eligible Host, then
  the Runner SHALL report a creation error naming the scheduling labels it
  sent.
- **BR-045** When deleting an Overflow MicroVM, the Runner SHALL call
  `DeleteMicroVM` with the uid on the Orchestrator.
- **BR-046** If `DeleteMicroVM` returns `NOT_FOUND`, then the Runner SHALL
  treat the deletion as complete.
- **BR-047** The Runner SHALL only call `ListMicroVMs` with its own namespace.
