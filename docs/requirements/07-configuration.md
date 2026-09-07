# Configuration {#configuration}

This document specifies the Runner's configuration surface. One YAML file
describes GitLab access, the Inventory, the Pool Manager, the Profiles, the Scheduler limits and the Fleet Controller inputs.
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
  runner token, the Pool Manager endpoint, one Host and one Profile with a
  Pool size.
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
- **CF-012** The Runner SHALL default the concurrency limit to twice the sum
  of the declared Pool sizes, because immediate-on-lease replenishment keeps
  Jobs flowing beyond the idle Pool size.

## Profiles section {#profiles-section}

- **CF-020** A Profile SHALL have a unique name, an architecture, a vCPU
  count, a memory size in megabytes, a kernel image, a root filesystem image,
  a shell path, a builds directory, a cache directory, a helper binary path
  and pool settings.
- **CF-021** A Profile MAY declare image names and image glob patterns that
  map Job Images onto it, a kernel command line, an initrd image, additional
  volumes, a hypervisor provider, cloud-init user-data, a Guest Transport, a
  user, a ready timeout, a Host selector and a maximum concurrency.
- **CF-022** Exactly one Profile MAY be marked as the Default Profile.
- **CF-023** If two Profiles declare the same image name, then the Runner
  SHALL reject the configuration.
- **CF-024** A Profile's Host selector SHALL be expressed as label
  requirements matched against Inventory labels and SHALL determine the
  Pool's host list.
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
  MAY contain labels, a basic auth token, TLS settings, Host Service
  addresses and installed version information.
- **CF-032** The Host name in an Inventory entry SHALL be the name the Pool
  Manager uses for that Host in `flintlock_hosts`.
- **CF-031** The Runner SHALL reject an Inventory with duplicate Host names or
  duplicate endpoints.

## Pool Manager section {#pool-manager-section}

- **CF-040** The Pool Manager section SHALL contain an endpoint, TLS
  settings, a request deadline, a health backoff period, a health probe
  interval, an events poll interval, a pool declaration retry interval and a
  release retry limit.
- **CF-041** If the Pool Manager section is absent or has no endpoint, then
  the Runner SHALL reject the configuration.

## Scheduler section {#scheduler-section}

- **CF-050** The Scheduler section SHALL contain the Runner namespace, the
  allocation timeout, the Host health probe interval, the unhealthy probe
  threshold, the keep-on-failure flag and the keep duration.
- **CF-051** The Runner SHALL default the Runner namespace to the runner
  name so that two Runners sharing one Pool Manager declare their Pools in
  separate namespaces.

## Fleet section {#fleet-section}

- **CF-060** The Fleet section SHALL contain the AWS region, the discovery
  tag key and value or explicit instance ids, the remote execution mode and
  its SSH settings, the provisioning parallelism, the pinned versions of
  flintlock, Firecracker, Cloud Hypervisor, containerd and the Pool Manager,
  the thin pool device, the guest subnet, the `flintlockd`
  port and auth settings, the Host reserve, and the paths for the generated
  Inventory and Runner configuration.
- **CF-061** The Fleet section MAY contain launch template settings naming
  the Systems Manager parameters that hold secrets.

## Host services section {#host-services-section}

- **CF-070** The Host services section SHALL contain, for each of
  `buildkit`, `go_proxy`, `registry_mirror` and `http_cache`, an enabled flag
  that defaults to true and a port.
- **CF-071** The `buildkit` entry MAY contain a storage limit and a garbage
  collection policy.
- **CF-072** The `go_proxy` entry MAY contain the upstream proxy, a list of
  private module patterns, the version control host those patterns resolve
  to, the Systems Manager parameter holding the read-only credential for
  them, a private module revalidation interval, a storage limit and a list of
  modules to pre-warm.
- **CF-076** If the `go_proxy` entry lists private module patterns without a
  credential parameter, then the Runner SHALL reject the configuration.
- **CF-073** The `registry_mirror` entry MAY contain a list of upstream
  registries with optional credential parameter names, a storage limit and a
  list of images to pre-warm.
- **CF-074** The `http_cache` entry MAY contain a list of upstreams, each
  with a name, an upstream URL, a size limit, a time-to-live and the
  environment variable name the Executor injects for it.
- **CF-075** The Host services section SHALL contain the cache volume device
  or directory and its total size cap.

## Distributed cache section {#distributed-cache-section}

- **CF-080** The Distributed cache section SHALL contain an S3 bucket name,
  region and optional prefix, and MAY contain an endpoint for S3-compatible
  stores.
- **CF-081** The Runner SHALL map the Distributed cache section onto the
  gitlab-runner cache configuration so that the `cache:` keyword in a
  pipeline stores and restores through that bucket.
- **CF-082** The Runner SHALL configure the distributed cache so that
  pre-signed URLs are generated on the Control Node and no AWS credentials
  are passed into the guest.
- **CF-083** If the Distributed cache section is absent, then the Runner
  SHALL log at startup that the `cache:` keyword is unavailable and SHALL
  fail Jobs that use it with the failure reason `runner_unsupported`.
