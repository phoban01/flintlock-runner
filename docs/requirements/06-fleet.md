# Fleet {#fleet}

This document specifies the Fleet Controller: the `flintlock-runner fleet`
subcommands that turn a set of EC2 bare-metal instances into flintlock Hosts,
install the Orchestrator on every Host and the Pool Manager daemon and host
agents, and produce the Inventory and Runner configuration. The intent is that an operator
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
  from the guest subnet to the Host's own `flintlockd`, Orchestrator, Pool
  Manager and metrics ports.
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

## Orchestrator and Pool Manager agents {#orchestrator-and-pool-manager-agents}

- **FL-050** The Fleet Controller SHALL install the pinned Orchestrator
  release on every Host as a systemd service configured with the Host's
  `flintlockd` endpoint and token, the north-edge token from the
  configuration, and cluster gossip peers set to the other Hosts in the
  Inventory.
- **FL-051** The Fleet Controller SHALL set each Orchestrator node's declared
  capacity and labels from the Host's Inventory entry, including the Host
  architecture, because the Runner and the Pool Manager select Hosts through
  the Orchestrator by those labels.
- **FL-052** The Fleet Controller SHALL set the Orchestrator's minimum
  cluster size to the configured value or, by default, to a majority of the
  Hosts.
- **FL-053** The Fleet Controller SHALL install the pinned Pool Manager host
  agent on every Host as a systemd service.
- **FL-054** The Fleet Controller SHALL install the pinned Pool Manager daemon
  on the Control Node with a host list generated from the Inventory that
  names every physical Host with its TLS settings.
- **FL-055** The Fleet Controller SHALL verify that each installed service is
  active before reporting an instance as provisioned.
- **FL-056** The Fleet Controller SHALL verify after installation that every
  Orchestrator node reports the full cluster membership and that the Pool
  Manager daemon answers `ListPools`.

The Pool Manager's host list names physical Hosts rather than the
Orchestrator because it reaches its per-Host agent through each MicroVM's
vsock path and has to know which Host that path lives on. Placement of warm
MicroVMs is therefore decided by the Pool Manager across the Hosts eligible
for the Pool, and placement of Overflow MicroVMs by the Orchestrator; the
Fleet Controller gives both the same view of the Hosts.

## Inventory and Runner configuration {#inventory-and-runner-configuration}

- **FL-060** When provisioning completes, the Fleet Controller SHALL write an
  Inventory file listing every Host with its name, `flintlockd` endpoint,
  Orchestrator endpoint, architecture, vCPU and memory capacity, labels and
  installed versions.
- **FL-061** The Fleet Controller SHALL compute each Host's capacity as the
  instance's vCPU and memory minus the configured Host reserve.
- **FL-062** The Fleet Controller SHALL generate a Runner configuration file
  that references the Inventory, lists every Host's Orchestrator endpoint,
  names the Pool Manager endpoint and carries the Profiles from its input.
- **FL-063** When run again after new instances match the discovery filter,
  the Fleet Controller SHALL provision only the new instances and SHALL merge
  them into the existing Inventory.
- **FL-064** When run again after an instance in the Inventory no longer
  exists, the Fleet Controller SHALL remove it from the Inventory and report
  the removal.

## Verification {#verification}

- **FL-070** The Fleet Controller SHALL provide a verification command that
  checks every Host answers `ServerInfo` with the exec service enabled, that
  every Orchestrator node reports the full cluster and that the Pool Manager
  lists every declared Pool.
- **FL-071** If verification fails on any Host or service, then the Fleet
  Controller SHALL exit with a non-zero status naming each failure and the
  step that failed.
- **FL-072** The verification command SHALL create one MicroVM per Profile
  through the Orchestrator, resolve and report which Host it landed on, run a
  trivial command in it through the Guest Transport and delete it.
- **FL-073** The verification command SHALL claim, exercise and release one
  warm MicroVM from every Pool and report the time from claim to readiness.

## Drain and teardown {#drain-and-teardown}

- **FL-080** The Fleet Controller SHALL provide a drain command that removes
  a Host from every Pool's `flintlock_hosts`, stops the Orchestrator node on
  that Host so that it receives no new placements, and waits until no Job
  MicroVM remains on it or the configured drain timeout elapses.
- **FL-084** The drain command SHALL NOT stop `flintlockd` on the drained
  Host while any Job MicroVM remains on it.
- **FL-081** The Fleet Controller SHALL provide a teardown command that
  deletes every Runner-owned MicroVM, stops and disables the installed
  services on every Host and removes the Inventory.
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
