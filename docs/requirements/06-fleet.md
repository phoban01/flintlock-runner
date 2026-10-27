# Fleet {#fleet}

**The fleet stack is removed.** This document specified the Fleet
Controller: the `flr fleet` subcommands that turned a set of EC2 instances
with KVM into flintlock Hosts, installed the Pool Manager daemon and the
Host Services, and produced the Inventory and Runner configuration. #148
removed the `flr fleet` subcommands, `internal/fleet`, the `fleet`
configuration section and `docs/fleet/`.

The Hosts come from [battery-operator](https://github.com/phoban01/battery-operator)
now. Its Host Image boots a Host with `flintlockd`, containerd and the
guest network (its `11-host-image.md`, HI). Its Host Pool Templates make
the Host pools on EC2 with Cluster API (its `12-host-pool.md`, HP, and
`config/capi/` there). Its Inventory Controller registers every Host with
battery through battery's HostAdmin API (its `04-inventory.md`, IN, and
its ADR 0008), and battery runs as its sidecar. The Runner runs on the
claim backend in that cluster, as `12-cluster-fleet.md` specifies. The
direct battery backend and the fake Pool Manager stay as the test
substrate.

FL-001 to FL-093 and FL-116 to FL-124 are withdrawn. Each keeps its
number and names its replacement, as #103 did. FL-100 to FL-115 keep their
text and stay in force. The Host services section stays because the Host
Agent of `12-cluster-fleet.md` implements it: KF-070 and KF-074 name
FL-100 and FL-102 to FL-115, and `deploy/host-agent`, `deploy/tests` and
`internal/hostservices` implement them for the Host Agent.

## Discovery {#discovery}

- **FL-001** (withdrawn) Replaced by battery-operator's Inventory
  Controller (its IN-001 and IN-017): a Host is a Node of the cluster that
  carries the Host label and whose Exec Agent reports it ready, not an EC2
  instance found by tag.
- **FL-002** (withdrawn) Replaced by battery-operator IN-001; there is no
  list of instance ids.
- **FL-003** (withdrawn) Nothing replaces it: battery-operator HP-001 names
  the instance type of each Host pool, so no instance is excluded by type.
- **FL-004** (withdrawn) Replaced by battery-operator HI-002 and HP-001:
  the Host Image is built for both architectures, and a Host pool names one
  instance type.
- **FL-005** (withdrawn) Replaced by battery-operator IN-003: battery
  reaches a Host at the `flintlockd` address the Host's Node report gives.
- **FL-006** (withdrawn) Replaced by battery-operator's Host Pool
  Templates: Cluster API holds the AWS credentials, and nothing of the
  cluster fleet holds any (KF-112).
- **FL-116** (withdrawn) Nothing replaces it: battery-operator HP-001 names
  the instance type of each Host pool, so no instance is excluded by type.
- **FL-117** (withdrawn) Replaced by battery-operator HP-001: a Host pool
  names a single instance type, one with KVM (its `config/capi`), so no
  instance without KVM joins the Inventory.

KVM is available on bare-metal instance types and, where nested
virtualization is enabled, on some virtualized types too, so the name of an
instance type does not say whether an instance can run MicroVMs. The check is
therefore made on the host itself, before anything else is installed there.
On battery-operator's Host Image the check is its HI-011: a Host without
usable KVM does not start `flintlockd` and reports itself not ready, and the
Inventory Controller gives it to no Pool (its IN-001).

## Remote execution {#remote-execution}

- **FL-010** (withdrawn) Nothing is pushed to a Host. A Host boots complete
  from battery-operator's Host Image (its HI-001 to HI-008).
- **FL-011** (withdrawn) Nothing is pushed to a Host, as for FL-010.
- **FL-012** (withdrawn) Replaced by battery-operator HP-001: a Host pool is
  a Cluster API MachineDeployment, which makes its Hosts in parallel.
- **FL-013** (withdrawn) Replaced by battery-operator HP-005: a
  MachineHealthCheck replaces a Host whose Node stays not ready.
- **FL-014** (withdrawn) Replaced by battery-operator EA-034: the Exec
  Agent publishes each Host's readiness and its reason on the Host's Node.

## Host provisioning {#host-provisioning}

- **FL-020** (withdrawn) Replaced by battery-operator HI-003: the Host
  Image contains containerd, Firecracker, Cloud Hypervisor and `flintlockd`
  at the versions its versions file pins.
- **FL-021** (withdrawn) Replaced by battery-operator HI-001: the Host
  Image is built from one Containerfile, and no provisioner runs on a Host.
- **FL-022** (withdrawn) Replaced by battery-operator HI-020.
- **FL-023** (withdrawn) Replaced by battery-operator HI-021.
- **FL-024** (withdrawn) Replaced by battery-operator HI-067 and HI-068:
  `flintlockd` serves only TLS and requires a client certificate, with the
  certificates the Exec Agent obtains (its ADR 0002 and ADR 0003).
- **FL-025** (withdrawn) Replaced by battery-operator HI-043 for the exec
  API. The SSH proxy API is not enabled: the Runner reaches a guest through
  the Exec Agent with the `agent-exec` Guest Transport (KF-185 to KF-188).
- **FL-026** (withdrawn) Replaced by battery-operator HI-067: `flintlockd`
  has no insecure mode on a Host.
- **FL-027** (withdrawn) Replaced by battery-operator HI-002 and HI-009:
  the image is built for each architecture, and an AMI has one.
- **FL-028** (withdrawn) Nothing pre-pulls images on a Host. battery's warm
  MicroVMs pull their own images.
- **FL-029** (withdrawn) Replaced by battery-operator HI-005 and HI-060:
  the Host Image records its pinned versions as image labels and in a
  versions file, and the kubelet registers one label per hypervisor.
- **FL-030** (withdrawn) Nothing changes a Host in place. A Host is replaced
  from a new image (battery-operator HI-062).

## Host networking {#host-networking}

- **FL-040** (withdrawn) Replaced by battery-operator HI-030 and HI-083:
  the Host Image makes the bridge `virbr-battery`.
- **FL-041** (withdrawn) Replaced by battery-operator HI-031.
- **FL-042** (withdrawn) Replaced by battery-operator HI-032.
- **FL-043** (withdrawn) Replaced by battery-operator HI-032.
- **FL-044** (withdrawn) Replaced by battery-operator HI-033.
- **FL-045** (withdrawn) Replaced by battery-operator HI-034.
- **FL-046** (withdrawn) Replaced by battery-operator HI-035 and HI-075 to
  HI-077: the Host drops guest traffic to the protected CIDRs its Host
  configuration file lists, which the Host Pool Templates set (its HP-011).
- **FL-047** (withdrawn) Replaced by battery-operator EA-030 and IN-015: the
  Exec Agent reports a Host not ready while its `flintlockd` does not
  answer, and the Inventory Controller cordons that Host in battery.

## Pool Manager {#pool-manager-install}

- **FL-050** (withdrawn) battery removed its Pool Manager host agent on
  2026-09-07 (commit b675a46a) because flintlock's native MicroVMExec and
  MicroVMSSHProxy services superseded it, and no battery release ships it.
- **FL-051** (withdrawn) Replaced by battery-operator DP-001 and IN-017:
  battery runs as a container of the Operator's pod, and the Inventory
  Controller registers each Host with `AddHost` through battery's HostAdmin
  API (its ADR 0008).
- **FL-052** (withdrawn) Replaced by battery-operator IN-003 and KF-151:
  battery holds each Host under its Node's name, and the Runner takes the
  Host from the Bound claim.
- **FL-053** (withdrawn) Replaced by battery-operator HI-008 and EA-030 to
  EA-033: the image's check stage verifies every unit, and the Exec Agent
  reports a Host not ready while a check fails.
- **FL-054** (withdrawn) Replaced by battery-operator IN-018: the Inventory
  Controller gives the Pool Controller a Host only once battery holds it
  uncordoned.
- **FL-055** (withdrawn) Replaced by battery-operator IN-017: battery's
  Hosts change through `AddHost`, `UpdateHost` and `RemoveHost` while it
  runs, and nothing reloads it (its ADR 0008).

## Inventory and Runner configuration {#inventory-and-runner-configuration}

- **FL-060** (withdrawn) The Runner reads no Inventory on the claim backend
  (KF-063). battery's Hosts come from the Inventory Controller
  (battery-operator IN-017).
- **FL-061** (withdrawn) Replaced by battery-operator HI-050 and HI-061: the
  Host reserve is a setting of the Host configuration file, and the kubelet
  reserves everything beyond it for the MicroVMs.
- **FL-062** (withdrawn) Replaced by the claim backend's configuration,
  KF-150 to KF-156: the Runner's configuration names the cluster's `Pool`
  resources, not an Inventory or a Pool Manager endpoint (KF-063).
- **FL-063** (withdrawn) Replaced by battery-operator IN-017: a new Host
  joins battery when its Node becomes a Host.
- **FL-064** (withdrawn) Replaced by battery-operator IN-016: the Inventory
  Controller cordons and removes a Host whose Node is gone.

## Verification {#verification}

- **FL-070** (withdrawn) Replaced by the claim harness (KF-192, TD-051 over
  the claim stack) and `hack/real-host`, which runs Jobs through the Runner
  on a real Host next to battery-operator.
- **FL-071** (withdrawn) Replaced by the claim harness and
  `hack/real-host`, as for FL-070.
- **FL-072** (withdrawn) Replaced by the claim harness and
  `hack/real-host`: each Job claims a MicroVM with a `MicroVMClaim` and runs
  its script through the Exec Agent.
- **FL-073** (withdrawn) Replaced by battery-operator EA-030 to EA-033 and
  IN-015: a Host that cannot serve reports itself not ready, and the
  Inventory Controller cordons it in battery.

## Drain and teardown {#drain-and-teardown}

- **FL-080** (withdrawn) Replaced by battery-operator's Node drain: the
  Inventory Controller cordons a Host in battery when its Node is cordoned
  (its IN-015), and the Exec Agent's guard holds the drain open while
  claims are Bound on the Host (its EA-040).
- **FL-084** (withdrawn) Replaced by battery-operator EA-040: the drain
  completes only when no claim is Bound on the Host or the drain timeout
  elapses, and nothing stops `flintlockd` before that.
- **FL-081** (withdrawn) Replaced by deleting the Fleet Manifests and the
  Host pool's MachineDeployment (battery-operator HP-001). battery removes
  a Host once no Pool names it and no MicroVM is on it (its IN-016 and ADR
  0008).
- **FL-082** (withdrawn) Replaced by Cluster API: a Host pool's
  MachineDeployment owns its instances (battery-operator HP-001), and
  nothing here terminates one.
- **FL-083** (withdrawn) Nothing purges a Host. Its thin pool is on its
  instance store (battery-operator HI-020) and goes with the instance.

## Launch template mode {#launch-template-mode}

- **FL-090** (withdrawn) Replaced by battery-operator HP-001 and HP-010:
  every Host boots complete from the Host Image, and the pool's
  KubeadmConfigTemplate writes its Host configuration file. There is no
  user-data script.
- **FL-091** (withdrawn) Replaced by battery-operator HI-052 and HP-030: a
  Host reads no secret from user-data and has no instance profile. The Exec
  Agent obtains the Host's certificates through certificate signing
  requests (its ADR 0003).
- **FL-092** (withdrawn) Replaced by battery-operator's Inventory
  Controller (its IN-017): a Host joins battery when its Node becomes a
  Host, and the Runner reads no Inventory (KF-063).

## One-shot operation {#one-shot-operation}

- **FL-118** (withdrawn) Nothing replaces it. The provision command is gone.
- **FL-119** (withdrawn) Nothing replaces it. The up command is gone.
- **FL-120** (withdrawn) Nothing replaces it. The up command is gone.
- **FL-121** (withdrawn) Nothing replaces it. The up command is gone.
- **FL-122** (withdrawn) Nothing replaces it. The Fleet Manifests run the
  Runner as a Deployment (KF-080); nothing installs it as a systemd service.
- **FL-123** (withdrawn) Nothing replaces it. The up command is gone.
- **FL-124** (withdrawn) Nothing replaces it. The up command is gone.

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
  Host Service ports and the ports of the DHCP and DNS service bound to the
  bridge, and SHALL keep every other port on the gateway closed to guests.
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

Since #148 no Fleet Controller installs anything. The Host Agent of
`deploy/host-agent` runs the Host Services as a DaemonSet on every Host
(KF-070, KF-071) and configures each one as FL-102 to FL-107, FL-114 and
FL-115 say (KF-074). It keeps their storage in a volume of its own pod
(KF-197) and publishes their addresses on the Host's Node (KF-194), where
the Executor reads them, in place of the Inventory entry of FL-110. Its
credentials come from Kubernetes Secrets (KF-073), not from Systems
Manager parameters. battery-operator's Host Image opens the gateway to the
Host Service ports (its HI-078) and keeps their user ids from the metadata
service and the control ports (its HI-080), which is FL-108 on the Host.
The verification MicroVM of FL-109 has no command to run it; the Job in
`hack/real-host/job-host-services.json` exercises the four services on a
real Host instead.

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
