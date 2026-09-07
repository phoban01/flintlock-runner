# Fleet {#fleet}

This document specifies the Fleet Controller: the `flintlock-runner fleet`
subcommands that turn a set of EC2 bare-metal instances into flintlock Hosts,
install the Pool Manager daemon, its host agents and the Host Services, and
produce the Inventory and Runner configuration. The intent is that an operator
launches metal instances with a known tag, runs one command from a Control
Node, and ends with a working Runner. Nothing in the flintlock ecosystem is
EC2-specific; the Fleet Controller supplies that layer.

## Discovery {#discovery}

- **FL-001** The Fleet Controller SHALL discover candidate instances with the
  EC2 `DescribeInstances` API filtered by the configured tag key and value and
  by the `running` instance state.
- **FL-002** Where explicit instance ids are configured, the Fleet Controller
  SHALL use them instead of tag discovery.
- **FL-003** If a discovered instance's type does not end in `.metal`, then
  the Fleet Controller SHALL exclude it and report it as unsupported because
  KVM is unavailable.
- **FL-004** The Fleet Controller SHALL determine each instance's
  architecture from the EC2 instance attributes and record it in the
  Inventory.
- **FL-005** The Fleet Controller SHALL use the instance's private IP address
  as the Host endpoint address unless an override is configured.
- **FL-006** The Fleet Controller SHALL obtain AWS credentials through the
  default credential chain of the AWS SDK.

## Remote execution {#remote-execution}

- **FL-010** The Fleet Controller SHALL execute commands on instances through
  AWS Systems Manager Run Command by default.
- **FL-011** Where SSH is configured, the Fleet Controller SHALL execute
  commands over SSH with the configured user and key instead of Systems
  Manager.
- **FL-012** The Fleet Controller SHALL provision instances in parallel with
  the configured parallelism limit.
- **FL-013** If provisioning fails on one instance, then the Fleet Controller
  SHALL continue provisioning the others and SHALL exit with a non-zero
  status that summarises every failure.
- **FL-014** The Fleet Controller SHALL stream each instance's command output
  to its log prefixed with the instance id.

## Host provisioning {#host-provisioning}

- **FL-020** The Fleet Controller SHALL provision each instance by running
  the flintlock host provisioner in unattended mode, installing containerd,
  Firecracker, Cloud Hypervisor and `flintlockd` at the versions pinned in the
  configuration.
- **FL-021** The Fleet Controller SHALL use the `flintlock-provision` binary
  when the pinned flintlock release ships it and SHALL fall back to the
  `provision.sh` script from the same release otherwise.
- **FL-022** The Fleet Controller SHALL create the containerd devicemapper
  thin pool on the block device named in the configuration.
- **FL-023** If the configured thin pool already exists on an instance, then
  the Fleet Controller SHALL NOT recreate it or wipe its device.
- **FL-024** The Fleet Controller SHALL configure `flintlockd` to listen on
  the instance's private address on the configured port, to require the
  configured basic auth token and to serve TLS with the configured
  certificates.
- **FL-025** The Fleet Controller SHALL enable the `flintlockd` exec API on
  every Host and SHALL enable the SSH proxy API where any Profile uses the
  proxied `ssh` Guest Transport.
- **FL-026** The Fleet Controller SHALL NOT start `flintlockd` in insecure
  mode unless the configuration explicitly requests it.
- **FL-027** The Fleet Controller SHALL select binaries and images matching
  the instance's architecture.
- **FL-028** The Fleet Controller SHALL pre-pull the kernel and root
  filesystem images of every Profile into the flintlock containerd namespace
  on each Host after provisioning.
- **FL-029** The Fleet Controller SHALL record the installed versions of
  flintlock, Firecracker, Cloud Hypervisor and containerd in the Inventory.
- **FL-030** When run against an instance that is already provisioned at the
  pinned versions, the Fleet Controller SHALL make no changes to that
  instance and report it as up to date.

## Host networking {#host-networking}

- **FL-040** The Fleet Controller SHALL create a Linux bridge on each Host
  with the configured private guest subnet and SHALL configure `flintlockd`
  to attach TAP interfaces to it.
- **FL-041** The Fleet Controller SHALL install and configure a DHCP and DNS
  service bound to the bridge so that guests obtain an address, gateway and
  resolver without static configuration.
- **FL-042** The Fleet Controller SHALL enable IPv4 forwarding persistently
  on each Host.
- **FL-043** The Fleet Controller SHALL configure source NAT from the guest
  subnet to the Host's primary interface so that guests have outbound
  connectivity.
- **FL-044** The Fleet Controller SHALL configure each Host to drop traffic
  from the guest subnet to the EC2 instance metadata service address.
- **FL-045** The Fleet Controller SHALL configure each Host to drop traffic
  from the guest subnet to the Host's own `flintlockd`, Pool Manager agent
  and metrics ports.
- **FL-046** The Fleet Controller SHALL configure each Host to drop traffic
  from the guest subnet to the private addresses of other Hosts in the
  Inventory.
- **FL-047** The Fleet Controller SHALL verify TCP reachability from the
  Control Node to each Host's `flintlockd` port after provisioning and SHALL
  report any Host that is unreachable together with the security group
  rule that would be needed.

Guest networking is deliberately host-local. EC2 does not deliver frames to
MAC addresses it has not assigned to an interface, so bridging guests
directly onto the VPC does not work without per-guest secondary addresses.
A NAT'd bridge gives every Job outbound access for cloning and package
downloads, while the Guest Transport reaches the guest over vsock through
the Host, so no inbound path to the guest is needed.

## Pool Manager {#pool-manager-install}

- **FL-050** The Fleet Controller SHALL install the pinned Pool Manager host
  agent on every Host as a systemd service.
- **FL-051** The Fleet Controller SHALL install the pinned Pool Manager daemon
  on the Control Node with a host list generated from the Inventory that
  names every Host with its `flintlockd` endpoint, token and TLS settings.
- **FL-052** The Fleet Controller SHALL name each Host identically in the
  Pool Manager's host list and in the Inventory, because the Runner joins
  the two by name when resolving Placement.
- **FL-053** The Fleet Controller SHALL verify that each installed service is
  active before reporting an instance as provisioned.
- **FL-054** The Fleet Controller SHALL verify after installation that the
  Pool Manager daemon answers `ListPools` and reports every Host in the
  Inventory as reachable.
- **FL-055** When run again after the Inventory changes, the Fleet Controller
  SHALL regenerate the Pool Manager's host list and reload the daemon without
  interrupting existing Leases.

The Pool Manager is the only component that places MicroVMs, so its host
list is the fleet's definition of "eligible host". The Inventory the Runner
reads is the same list with the additional detail the Runner needs to reach
each Host for the Guest Transport.

## Inventory and Runner configuration {#inventory-and-runner-configuration}

- **FL-060** When provisioning completes, the Fleet Controller SHALL write an
  Inventory file listing every Host with its name, `flintlockd` endpoint,
  architecture, vCPU and memory capacity, labels, Host Service addresses and
  installed versions.
- **FL-061** The Fleet Controller SHALL compute each Host's capacity as the
  instance's vCPU and memory minus the configured Host reserve.
- **FL-062** The Fleet Controller SHALL generate a Runner configuration file
  that references the Inventory, names the Pool Manager endpoint and carries
  the Profiles from its input.
- **FL-063** When run again after new instances match the discovery filter,
  the Fleet Controller SHALL provision only the new instances and SHALL merge
  them into the existing Inventory.
- **FL-064** When run again after an instance in the Inventory no longer
  exists, the Fleet Controller SHALL remove it from the Inventory and report
  the removal.

## Verification {#verification}

- **FL-070** The Fleet Controller SHALL provide a verification command that
  checks every Host answers `ServerInfo` with the exec service enabled and
  that the Pool Manager lists every declared Pool at its target size.
- **FL-071** If verification fails on any Host or service, then the Fleet
  Controller SHALL exit with a non-zero status naming each failure and the
  step that failed.
- **FL-072** The verification command SHALL claim warm MicroVMs from every
  Pool, run a trivial command in each through the Guest Transport and
  release them, continuing until it has exercised every Host in the Pool's
  host list or the verification timeout elapses, and SHALL report the time
  from claim to readiness per Host.
- **FL-073** The verification command SHALL report any Host in a Pool's host
  list on which no MicroVM could be exercised within the verification
  timeout.

## Drain and teardown {#drain-and-teardown}

- **FL-080** The Fleet Controller SHALL provide a drain command that removes
  a Host from every Pool's `flintlock_hosts` so that the Pool Manager places
  nothing new there, and waits until the Pool Manager reports no leased
  MicroVM on that Host or the configured drain timeout elapses.
- **FL-084** The drain command SHALL NOT stop `flintlockd` on the drained
  Host while any leased MicroVM remains on it.
- **FL-081** The Fleet Controller SHALL provide a teardown command that
  deletes the declared Pools through the Pool Manager, waits for their
  MicroVMs to be removed, stops and disables the installed services on every
  Host and removes the Inventory.
- **FL-082** The teardown command SHALL NOT terminate EC2 instances unless
  the terminate flag is passed explicitly.
- **FL-083** The teardown command SHALL NOT remove the thin pool or its
  backing device unless the purge flag is passed explicitly.

## Launch template mode {#launch-template-mode}

- **FL-090** Where launch template mode is selected, the Fleet Controller
  SHALL emit a cloud-init user-data script that performs the Host
  provisioning steps unattended at first boot so that instances launched by
  an auto scaling group self-provision.
- **FL-091** Where launch template mode is selected, the Fleet Controller
  SHALL read secrets for the emitted script from the configured Systems
  Manager parameters rather than embedding them in the user-data.
- **FL-092** Where launch template mode is selected, the Runner SHALL refresh
  its Inventory from tag discovery at the configured interval so that
  self-provisioned Hosts join without a restart.

## Host services {#host-services}

- **FL-100** The Fleet Controller SHALL install on every Host a `buildkitd`
  service, a Go module proxy service, a pull-through container registry
  mirror service and an HTTP cache service for the configured upstreams,
  collectively the Host Services.
- **FL-101** The Fleet Controller SHALL bind every Host Service only to the
  guest bridge gateway address so that it is reachable from guests on that
  Host and from nothing else.
- **FL-102** The Fleet Controller SHALL run `buildkitd` in rootless mode
  with the overlayfs snapshotter and SHALL configure its garbage collection
  with the storage limit from the configuration.
- **FL-103** The Fleet Controller SHALL configure `buildkitd` to resolve
  images through the Host's registry mirror before reaching upstream
  registries.
- **FL-104** The Fleet Controller SHALL configure the registry mirror as a
  pull-through cache for each upstream registry named in the configuration,
  with credentials for upstreams that need them supplied from Systems
  Manager parameters.
- **FL-105** The Fleet Controller SHALL configure the Go module proxy to
  store modules on the Host's cache volume and to fetch modules it does not
  have from the public proxy for public paths and from the configured
  version control host for the configured private module patterns.
- **FL-113** The Fleet Controller SHALL supply the Go module proxy with a
  read-only credential for the private module patterns, read from the
  configured Systems Manager parameter and held on the Host side only.
- **FL-114** The Fleet Controller SHALL configure the Go module proxy to
  skip checksum database verification for the private module patterns and
  to verify every other module against the public checksum database.
- **FL-115** The Fleet Controller SHALL configure the Go module proxy to
  serve private modules from its cache without contacting the version
  control host again until the configured private module revalidation
  interval elapses.
- **FL-106** The Fleet Controller SHALL configure the HTTP cache with one
  cache path per configured upstream, each with its own size limit and
  time-to-live.
- **FL-107** The Fleet Controller SHALL place all Host Service storage on a
  dedicated cache volume or directory whose total size is capped by the
  configuration.
- **FL-108** The Fleet Controller SHALL add guest firewall rules that allow
  traffic from the guest subnet to the bridge gateway address only on the
  Host Service ports and SHALL keep every other port on the gateway closed
  to guests.
- **FL-109** The verification command SHALL, from inside a verification
  MicroVM on each Host, build a trivial image with `buildctl`, fetch a
  module through the Go module proxy, pull an image through the registry
  mirror and fetch one object through the HTTP cache, and SHALL report the
  outcome per service per Host.
- **FL-110** The Fleet Controller SHALL record the address and port of each
  Host Service in the Host's Inventory entry.
- **FL-111** Where a Host Service is disabled in the configuration, the Fleet
  Controller SHALL NOT install it and SHALL NOT open its port.
- **FL-112** The Fleet Controller SHALL pre-warm the Go module proxy and the
  registry mirror on each Host with the modules and images listed in the
  configuration after provisioning.

Host Services exist so that a job in a fresh microVM does not start cold:
base image layers, Go modules and package indexes are served from the
host's disk after the first fetch on that host. They sit on the guest
bridge because every guest on a host is a job of this runner, and traffic to
the gateway never leaves the host, which is why the firewall is the boundary
rather than per-job credentials. The per-host cache is deliberately not
shared across hosts; cross-host reuse of build outputs is the job of the
GitLab distributed cache in `07-configuration.md`.

The Go module proxy manages private modules as well as public ones, using
one read-only credential per fleet rather than each job's token. A job
therefore never needs `GOPRIVATE` or a `.netrc`, and private modules are
cached on the host like any other. The consequence, accepted in the design,
is that a private module fetched by one job is readable by any later job on
that host; the credential is read-only and scoped to the groups named in the
private patterns so the exposure is bounded to source the fleet's jobs can
already reach.
