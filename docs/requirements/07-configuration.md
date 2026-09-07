# Configuration {#configuration}

This document specifies the Runner's configuration surface. One YAML file
describes GitLab access, the Inventory, the Orchestrator, the Pool Manager,
the Profiles, the Scheduler limits and the Fleet Controller inputs.
The Runner translates that file into the `RunnerConfig` structure the
gitlab-runner packages expect, so operators never write a `config.toml`.

## File and precedence {#file-and-precedence}

- **CF-001** The Runner SHALL read its configuration from a single YAML file
  whose path is given by the `--config` flag or the `FLINTLOCK_RUNNER_CONFIG`
  environment variable.
- **CF-002** The Runner SHALL allow every secret value in the configuration
  to be supplied by an environment variable named in the field's
  documentation, with the environment variable taking precedence over the
  file.
- **CF-003** The Runner SHALL allow an Inventory to be supplied inline in the
  configuration file or by reference to a separate Inventory file.
- **CF-004** When starting, the Runner SHALL validate the configuration and,
  if it is invalid, SHALL exit with a non-zero status and a message naming
  the first invalid field and the reason.
- **CF-005** The Runner SHALL apply documented defaults to every optional
  field so that a minimal configuration contains only the GitLab URL, the
  runner token, the Orchestrator endpoint, the Pool Manager endpoint, one
  Host and one Profile with a Pool size.
- **CF-006** The Runner SHALL provide a `config show` command that prints the
  effective configuration with every secret value redacted.
- **CF-007** When the Runner receives `SIGHUP`, the Runner SHALL reload the
  Profiles and the Inventory from disk without interrupting running Jobs.
- **CF-008** If a reload produces an invalid configuration, then the Runner
  SHALL keep the previous configuration and log the validation error.

## GitLab section {#gitlab-section}

- **CF-010** The GitLab section SHALL contain the GitLab URL, the runner
  authentication token, the runner name, the concurrency limit, the check
  interval, the output limit, the shutdown timeout and optional TLS
  certificate authority, client certificate and client key paths.
- **CF-011** The Runner SHALL map the GitLab section onto the `RunnerConfig`
  and `Config` structures of the gitlab-runner `common` package.
- **CF-012** The Runner SHALL default the concurrency limit to the sum of the
  declared Pool sizes plus the maximum number of Overflow MicroVMs.

## Profiles section {#profiles-section}

- **CF-020** A Profile SHALL have a unique name, an architecture, a vCPU
  count, a memory size in megabytes, a kernel image, a root filesystem image,
  a shell path, a builds directory, a cache directory, a helper binary path
  and pool settings.
- **CF-021** A Profile MAY declare image names and image glob patterns that
  map Job Images onto it, a kernel command line, an initrd image, additional
  volumes, a hypervisor provider, cloud-init user-data, a Guest Transport, a
  user, a ready timeout, a Host selector, Orchestrator scheduling
  constraints, an exhaustion policy, a maximum number of Overflow MicroVMs
  and a maximum concurrency.
- **CF-022** Exactly one Profile MAY be marked as the Default Profile.
- **CF-023** If two Profiles declare the same image name, then the Runner
  SHALL reject the configuration.
- **CF-024** A Profile's exhaustion policy SHALL be either `overflow` or
  `wait` and SHALL default to `overflow`.
- **CF-025** A Profile's pool settings SHALL contain the Pool size and MAY
  contain the Pool name and namespace, replenishment strategy, minimum size,
  create hooks, pre-lease hooks, hook failure policy, heartbeat interval and
  heartbeat expiry threshold.
- **CF-028** The Runner SHALL default a Pool's name to the Profile name and
  its namespace to the Runner namespace.
- **CF-029** If two Profiles resolve to the same Pool name and namespace,
  then the Runner SHALL reject the configuration.
- **CF-026** The Runner SHALL reject a Profile whose images are specified by
  neither a digest nor a tag.
- **CF-027** Where a Profile image is specified by tag rather than by digest,
  the Runner SHALL log a warning at startup naming the Profile and the image.

## Inventory section {#inventory-section}

- **CF-030** An Inventory entry SHALL contain a Host name, a `flintlockd`
  gRPC endpoint, an architecture, a vCPU capacity and a memory capacity, and
  MAY contain an Orchestrator endpoint, labels, a basic auth token, TLS
  settings and installed version information.
- **CF-031** The Runner SHALL reject an Inventory with duplicate Host names or
  duplicate endpoints.

## Orchestrator and Pool Manager sections {#orchestrator-and-pool-manager-sections}

- **CF-040** The Orchestrator section SHALL contain one or more endpoints, an
  optional token with its authorization scheme, TLS settings and a request
  deadline.
- **CF-041** If the Orchestrator section is absent or has no endpoint, then
  the Runner SHALL reject the configuration.
- **CF-042** The Pool Manager section SHALL contain an endpoint, TLS
  settings, a request deadline, a health backoff period, an events poll
  interval, a pool declaration retry interval and a release retry limit.
- **CF-043** If the Pool Manager section is absent or has no endpoint, then
  the Runner SHALL reject the configuration.

## Scheduler section {#scheduler-section}

- **CF-050** The Scheduler section SHALL contain the Runner namespace, the
  Runner identity used in labels, the maximum number of Overflow MicroVMs,
  the allocation timeout, the create timeout, the create poll interval, the
  health probe interval, the unhealthy probe threshold, the garbage
  collection interval, the orphan grace period, the maximum MicroVM lifetime,
  the release retry limit and the keep-on-failure flag.
- **CF-051** The Runner SHALL default the Runner identity to the runner name
  combined with the system identifier so that two Runners with the same name
  on different Control Nodes do not collect each other's MicroVMs.

## Fleet section {#fleet-section}

- **CF-060** The Fleet section SHALL contain the AWS region, the discovery
  tag key and value or explicit instance ids, the remote execution mode and
  its SSH settings, the provisioning parallelism, the pinned versions of
  flintlock, Firecracker, Cloud Hypervisor, containerd, the Orchestrator and
  the Pool Manager, the thin pool device, the guest subnet, the `flintlockd`
  port and auth settings, the Host reserve, and the paths for the generated
  Inventory and Runner configuration.
- **CF-061** The Fleet section MAY contain launch template settings naming
  the Systems Manager parameters that hold secrets.
