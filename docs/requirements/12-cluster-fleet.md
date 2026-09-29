# Cluster fleet {#cluster-fleet}

This document specifies how a fleet is run from a Kubernetes cluster. Hosts
are nodes of an existing workload cluster, made by battery-operator's
Cluster API templates from its Host Image (see below). battery is the
scheduler and the authority over Pools. It runs in front of the Hosts'
`flintlockd`s, behind the `Pool` and `MicroVMClaim` resources of
battery-operator (`#battery-claims`). The Runner claims a MicroVM with a
`MicroVMClaim`, and runs each Stage through the Exec Agent on the claim's
Host (`#agent-exec-transport`). The Runner keeps its Scheduler and Executor
unchanged above the `poolmgr.Client` interface; this document specifies a
second implementation of that interface and a Guest Transport that reaches
the guest through the Exec Agent.

Nothing here runs a Job in a container. The Host Agent runs the Host
Services on every Host, and the Runner runs as a Deployment off the Hosts.

This mode is an alternative to `04-pool-manager.md`, `05-hosts.md` and
`06-fleet.md`, which stay in force for fleets that are not run from a
cluster. The table at the end maps each of them to its counterpart here.

**The Virtual Node design is withdrawn.** This document first specified a
different design. On each Host a Pod Provider registered a Virtual Node and
ran every pod bound to it as a MicroVM. A Pool was a ReplicaSet of idle
MicroVM pods, and the Runner reached a guest through the pod's exec
subresource, the `kube-exec` Guest Transport. On 2026-09-22 the cluster
fleet moved to battery's claim resources, and #101 removed the Virtual Node
design once the claim stack passed the harness. The sections Virtual Node,
MicroVM pods, Pools, Allocation, Guest Transport, Drain, Verification and
Test doubles keep the numbers of that design, withdrawn. Each withdrawn
requirement names what replaced it. KF-063 and KF-126 are reworded for the
claim design, which still needs them.

**The Host Image and the Host pools are battery-operator's.** On
2026-09-29 (#107) the Host Image and the Cluster API templates of the Host
pools moved to battery-operator (its ADR 0007). flintlock-runner removed
`image/` and `deploy/capi`, and withdrew `11-host-image.md` and KF-001 to
KF-005. The Hosts now boot battery-operator's reference Host Image, and
battery-operator's Host Pool Templates (its `config/capi/`, specified in
its `12-host-pool.md`) make them. Each withdrawn requirement names the
battery-operator requirement that replaces it; "battery-operator HP-0nn"
and "battery-operator HI-0nn" are the requirements of those numbers there.
What stays here is what a Host pool sets for this project's Host
Services (KF-198), and the Host Service cache, which is now the Host
Agent's own (KF-197).

## Host pool {#host-pool}

- **KF-001** (withdrawn) Replaced by battery-operator HP-001: the Host Pool
  Templates define each Host pool as one MachineDeployment with one
  instance type, an AMI by id and a subnet by id.
- **KF-002** (withdrawn) Replaced by battery-operator HP-021: every Host
  joins with the taint `battery.liquidmetal-x.dev/host=true:NoSchedule`,
  which is now the Host taint.
- **KF-003** (withdrawn) Replaced by battery-operator HP-010 and HP-011:
  the Host Pool Templates write `/etc/battery/host.conf` through the
  KubeadmConfigTemplate's files.
- **KF-004** (withdrawn) Replaced by battery-operator HP-004.
- **KF-005** (withdrawn) Replaced by battery-operator HP-005.
- **KF-198** The Fleet Manifests SHALL provide the gateway service ports and
  gateway service user ids that a Host pool sets for the Host Services,
  listing every port a Host Service serves on and every user id a Host
  Service runs as.

The Host Services serve guests on the bridge gateway, and they run in the
Host's own network namespace. battery-operator's Host Image opens only the
gateway service ports of the Host configuration file to guests (its
HI-078), and drops the traffic of the gateway service user ids to the
metadata service and the Host's control ports (its HI-080). Both are empty
by default. So every Host pool of a cluster fleet sets them, in its
`host.conf`, to the values that `deploy/host-agent/config/host-pool.conf`
gives. That file is KF-198's: its ports are the Host Services' ports, and
its user ids are the Host Services' user ids and rootless `buildkitd`'s
subordinate ids, which the `RUN` steps of a Job's image build run as.
This carries SE-030 and SE-031 over to the Host Services: those steps
reach the metadata service and the Host's control ports no more than the
Job's MicroVM does.

A Host pool also sets `FLINTLOCKD_CLIENT_CIDRS` and a security group for
`flintlockd`'s port (battery-operator HP-006, HP-011), as any Host pool of
battery-operator does. `deploy/README.md` says what a pool sets.

## Virtual Node {#virtual-node}

- **KF-010** (withdrawn) The cluster fleet registers no Virtual Node.
  battery-operator's Inventory Controller gives battery each Host under its
  own Node's name (battery-operator IN-001, IN-003). It was implemented by
  `flr kubelet`, removed in #101.
- **KF-011** (withdrawn) There is no Virtual Node to own (KF-010). It was
  implemented by `flr kubelet`, removed in #101.
- **KF-012** (withdrawn) battery places MicroVMs against the capacity of
  its Hosts, and no Node advertises MicroVM capacity. It was implemented by
  `flr kubelet`, removed in #101.
- **KF-013** (withdrawn) A Pool selects its Hosts by the labels of their own
  Nodes (KF-150, battery-operator PO-010). It was implemented by `flr
  kubelet`, removed in #101.
- **KF-014** (withdrawn) No pod stands for a MicroVM, so no Node carries the
  MicroVM taint. It was implemented by `flr kubelet`, removed in #101.
- **KF-015** (withdrawn) battery-operator's Exec Agent reports its Host not
  ready while `flintlockd` does not answer `ServerInfo` with the exec
  service enabled (battery-operator EA-030). It checks no Host Service. It
  was implemented by `flr kubelet`, removed in #101.
- **KF-016** (withdrawn) battery-operator's Exec Agent reports the not ready
  reasons of the Host Image units (battery-operator EA-033, EA-036). It was
  implemented by `flr kubelet`, removed in #101.
- **KF-017** (withdrawn) The Host Agent publishes the Host Services on its
  Host's own Node (KF-194). It was implemented by `flr kubelet`, removed in
  #101.
- **KF-018** (withdrawn) battery-operator's Exec Agent takes the Pod
  Provider's place as the client of `flintlockd` on a Host, and reaches it
  over mutual TLS on the Host's address (battery-operator EA-001), not
  through the local endpoint of the withdrawn HI-042. It was implemented by
  `flr kubelet`, whose citation is removed.

## MicroVM pods {#microvm-pods}

- **KF-020** (withdrawn) battery creates the MicroVMs of a Pool from the
  Pool's spec, which the claim backend declares from the Profile (KF-150).
  No pod stands for a MicroVM. It was implemented by `flr kubelet`, removed
  in #101.
- **KF-021** (withdrawn) The Runner takes a MicroVM's uid from its claim
  (KF-151), not from labels. It was implemented by `flr kubelet`, removed in
  #101.
- **KF-022** (withdrawn) There is no MicroVM pod to refuse (KF-020). It was
  implemented by `flr kubelet`, removed in #101.
- **KF-023** (withdrawn) No pod names a cloud-init ConfigMap (KF-020). It
  was implemented by `flr kubelet`, removed in #101.
- **KF-024** (withdrawn) A claim is `Bound` only when battery has granted a
  warm MicroVM to it (KF-150, battery-operator CL-002). It was implemented
  by `flr kubelet`, removed in #101.
- **KF-025** (withdrawn) The claim names the Host and the address of its
  Exec Agent (KF-151). It was implemented by `flr kubelet`, removed in #101.
- **KF-026** (withdrawn) battery-operator sets a claim `Expired` when battery
  reports its MicroVM deleted (battery-operator CL-013), and the Scheduler
  treats that as a lost Lease (KF-154). It was implemented by `flr kubelet`,
  removed in #101.
- **KF-027** (withdrawn) Deleting a claim releases its MicroVM to battery
  (KF-153, battery-operator CL-020). It was implemented by `flr kubelet`,
  removed in #101.
- **KF-028** (withdrawn) battery-operator reconciles its claims with
  battery's Leases when it starts (battery-operator CL-030). It was
  implemented by `flr kubelet`, removed in #101.
- **KF-029** (withdrawn) battery owns the MicroVMs, and the Exec Agent only
  relays commands to them (battery-operator EA-020), so a restart of the
  Exec Agent deletes none. It was implemented by `flr kubelet`, removed in
  #101.
- **KF-030** (withdrawn) battery-operator's Exec Agent relays each request
  to `MicroVMExec.ExecCommand` (battery-operator EA-020), and the
  `agent-exec` Guest Transport calls it (KF-185). It was implemented by
  `flr kubelet`, removed in #101.
- **KF-031** (withdrawn) battery-operator's Exec Agent serves its exec API
  over TLS and authenticates every request with a TokenReview
  (battery-operator EA-002, EA-010). It was implemented by `flr kubelet`,
  removed in #101.
- **KF-032** (withdrawn) battery expires a Lease that the Holder does not
  renew, and the claim shows it (battery-operator CL-013, CL-032, KF-154).
  It was implemented by `flr kubelet`, removed in #101.

## Pools {#kube-pools}

- **KF-040** (withdrawn) The claim backend declares one `Pool` resource per
  Profile (KF-150, battery-operator PO-001). It was implemented by
  `internal/poolmgr/kube`, removed in #101.
- **KF-041** (withdrawn) The claim backend gives each Pool a node selector
  from the Profile's architecture and Host selector, and battery-operator
  places the Pool on the matching Hosts (battery-operator PO-010). It was
  implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-042** (withdrawn) battery spreads a Pool's MicroVMs over the Pool's
  Hosts. It was implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-043** (withdrawn) There are no Pool pods to label (KF-040). It was
  implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-044** (withdrawn) The Scheduler claims a MicroVM with a
  `MicroVMClaim` (KF-150), and battery grants each MicroVM to one claim
  only. It was implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-045** (withdrawn) battery-operator retries a claim on an exhausted
  Pool (battery-operator CL-003), and the Scheduler waits as SC-021 requires
  (KF-155). It was implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-046** (withdrawn) The Lease id and the Host come from the Bound claim
  (KF-151). It was implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-047** (withdrawn) The Scheduler renews a claim's Lease through its
  `spec.renewTime` (KF-152). It was implemented by `internal/poolmgr/kube`,
  removed in #101.
- **KF-048** (withdrawn) The Scheduler deletes the claim when a Job ends
  (KF-153). It was implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-049** (withdrawn) The claim backend takes each Pool's available count
  from a watch on its `Pool` resources (battery-operator PO-020). It was
  implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-050** (withdrawn) battery-operator sends a changed Pool spec to
  battery (battery-operator PO-002), and battery replaces the Pool's
  MicroVMs. It was implemented by `internal/poolmgr/kube`, removed in #101.
- **KF-051** (withdrawn) The claim backend deletes no Pool on a
  configuration reload; the Scheduler's rules for a removed Profile apply to
  it unchanged (KF-156). It was implemented by `internal/poolmgr/kube`,
  removed in #101.
- **KF-052** (withdrawn) The claim backend implements the `poolmgr.Client`
  interface (KF-156). It was implemented by `internal/poolmgr/kube`, removed
  in #101.

## Allocation {#kube-allocation}

- **KF-126** Where the claim backend is configured, the Scheduler SHALL
  record the Host that the Bound claim names as the Placement without
  looking it up in the Inventory, and SC-034 SHALL NOT apply.
- **KF-127** (withdrawn) A claim has no active deadline: battery expires a
  Lease that is not renewed, and the Scheduler renews it while the Job runs
  (KF-152). It was implemented by `internal/scheduler` and
  `internal/poolmgr/kube`, removed in #101.
- **KF-128** (withdrawn) Under the claim backend the Executor runs every
  Profile over the `agent-exec` Guest Transport (KF-185), and the Runner
  refuses a Profile that names `ssh` (KF-195). It was implemented by
  `internal/config` and `internal/executor`, removed in #101.

A cluster fleet has no Inventory, so SC-034, which fails an allocation whose
Host the Inventory does not know, would fail every one; KF-126 takes its
place. KF-151 already takes the Host from the claim and from nothing else.
KF-126 says what that means for SC-034. It first said the same of the
Virtual Node of a claimed pod.

## Guest Transport {#kube-exec-transport}

- **KF-060** (withdrawn) The Executor runs each Stage through the Exec Agent
  of the claim's Host (KF-185). It was implemented by the `kube-exec` Guest
  Transport, removed in #101.
- **KF-061** (withdrawn) The `agent-exec` Guest Transport meets the
  requirements of the `exec` Guest Transport (KF-188). It was implemented by
  the `kube-exec` Guest Transport, removed in #101.
- **KF-062** (withdrawn) The Executor reads the Host Service addresses from
  the Node of the Job's Host (KF-189). It was implemented by
  `internal/executor`, removed in #101.
- **KF-063** Where the claim backend is configured, the Runner SHALL read no
  Inventory and SHALL open no connection to a `flintlockd`.

KF-063 first said that the Runner of the `kube-exec` Guest Transport opens
no connection to a Host. The claim design keeps the point: the Runner has no
Inventory, no Host certificates and no route to any `flintlockd`. It
reaches only the Exec Agent that each claim names, on the Host's internal
address.

## Host Agent {#cluster-host-agent}

- **KF-070** The Fleet Manifests SHALL run the Host Agent as a DaemonSet that
  tolerates the Host taint, selects the Host label and contains the Host
  Services of FL-100.
- **KF-071** The Host Agent SHALL run in the Host's network namespace and
  SHALL bind every Host Service only to the guest bridge gateway address.
- **KF-072** (withdrawn) Replaced by KF-197: the Host Service cache is no
  longer a directory of the Host Image, which battery-operator's Host Image
  does not make, but a volume of the Host Agent's pod.
- **KF-073** The Fleet Manifests SHALL supply the registry mirror's upstream
  credentials and the Go module proxy's private module credential from
  Kubernetes Secrets mounted only into the Host Agent.
- **KF-074** The Host Agent SHALL configure each Host Service as FL-102 to
  FL-107, FL-114 and FL-115 require.
- **KF-075** Where a Host Service is disabled in the configuration, the Host
  Agent SHALL NOT run it.
- **KF-076** The Host Agent SHALL pre-warm the Go module proxy and the
  registry mirror with the modules and images listed in the configuration.
- **KF-194** The Host Agent SHALL publish the address and port of each
  enabled Host Service on the guest bridge gateway, under the names
  `buildkit`, `go_proxy`, `registry_mirror` and `http_cache`, as the
  annotations `host-service.gitlab-runner.flintlock.dev/<name>` on its Host's
  Node.
- **KF-197** The Host Agent SHALL keep all Host Service storage in one
  `emptyDir` volume of its pod, on the Node's disk rather than in
  memory.
- **KF-199** If the Host's gateway service ports or gateway service user ids
  leave out any that the Fleet Manifests provide, then the Host Agent
  SHALL NOT start the Host Services.

The Host Agent does not contain the Exec Agent. battery-operator's
Manifests run it, as a DaemonSet of their own (`#exec-agent`). That Exec
Agent knows nothing of the Host Services, so the Host Agent, which runs
them, publishes where they are (KF-194), for the Executor to read
(KF-189). It keeps the annotation names that `flr agent` used (KF-179), so
nothing that reads them changes. An admission policy lets the Host Agent's
identity change only these annotations, and only on its own Host's Node.

Pre-pulling Profile images needs no requirement of its own: battery creates
a Pool's warm MicroVMs on its Hosts before any claim, and `flintlockd`
pulls their images then.

KF-197 replaces the cache volume of the withdrawn Host Image. It is an
`emptyDir` rather than a hostPath for two reasons. battery-operator's
Host Image enforces SELinux and runs the Host Agent's containers in the
ordinary container domain (its HI-066), which may write only files
labelled for containers. The kubelet labels an `emptyDir` for the pod that
mounts it, but it labels no hostPath, and battery-operator's image labels
no directory for this project, so a hostPath cache would be refused on
every enforcing Host. And an `emptyDir` leaves nothing behind on the Host
when the Host Agent goes. The cost is a cold cache: the cache lives as
long as the pod, through container restarts but not through a rollout of
the DaemonSet or an eviction. It is on the Node's disk, in the kubelet's
directory, so it counts against the Node's ephemeral storage rather than
its memory. The thin pool takes the whole instance-store disk.

KF-199 keeps a Host pool that does not set KF-198's values from running
the Host Services open. Without the ports, guests could not reach them.
Without the user ids, a `RUN` step of a Job's image build could reach the
instance metadata service and the Host's control ports from the Host's own
network namespace. The Host Agent's `render` init container compares the
Host's settings in `/run/battery/host.env` with KF-198's, and fails, so
that no container of the pod starts.

## Runner {#cluster-runner}

- **KF-080** The Fleet Manifests SHALL run the Runner as a Deployment that
  reads the Profiles from a ConfigMap and the GitLab runner token from a
  Secret.
- **KF-081** The Fleet Manifests SHALL give the Runner's pod a termination
  grace period no shorter than the configured job timeout, so that a rollout
  does not cut running Jobs short.
- **KF-082** The Fleet Manifests SHALL NOT schedule the Runner onto Hosts.

## Drain {#cluster-drain}

- **KF-090** (withdrawn) battery-operator's Inventory Controller removes a
  cordoned Host from battery's Hosts (battery-operator IN-002), and battery
  puts no new MicroVM there. It was implemented by `flr kubelet`, removed in
  #101.
- **KF-091** (withdrawn) battery-operator's Exec Agent holds a drain of its
  Host's Node open while claims are Bound on the Host (battery-operator
  EA-040, EA-041). It was implemented by `flr kubelet`, removed in #101.
- **KF-092** (withdrawn) battery-operator's Exec Agent lets the drain
  complete when no claim remains (battery-operator EA-040). It was
  implemented by `flr kubelet`, removed in #101.
- **KF-093** (withdrawn) battery-operator's Exec Agent lets the drain
  complete when its drain timeout elapses (battery-operator EA-040). It was
  implemented by `flr kubelet`, removed in #101.
- **KF-094** (withdrawn) battery-operator's Inventory Controller gives the
  Host back to battery when its Node is schedulable again (battery-operator
  IN-001). It was implemented by `flr kubelet`, removed in #101.

## Verification {#cluster-verification}

- **KF-100** (withdrawn) Verification of a cluster fleet is not yet
  specified for the claim design. It was never implemented.
- **KF-101** (withdrawn) See KF-100. It was never implemented.
- **KF-102** (withdrawn) See KF-100. It was never implemented.

## Least privilege {#cluster-least-privilege}

- **KF-110** (withdrawn) KF-196 gives the Runner of the claim design its
  permissions. It was implemented by `deploy/runner/role.yaml`, removed in
  #101.
- **KF-111** (withdrawn) battery-operator's Manifests grant its Exec Agent
  its permissions, and its admission policy narrows them to its own Host
  (battery-operator EA-051, EA-053, EA-067). It was implemented by
  `flr kubelet` and `deploy/host-agent/rbac.yaml`, removed in #101.
- **KF-112** The Fleet Manifests SHALL NOT grant any workload an AWS
  permission.
- **KF-113** The Host Agent SHALL hold only the privileges it needs to bind
  to the bridge gateway in the Host's network namespace and to write the
  cache directory.
- **KF-196** The Fleet Manifests SHALL grant the Runner only the
  permissions to manage `MicroVMClaim` and `Pool` resources and to create
  Secrets in its own namespace, to request tokens of the Holder alone, to
  read Nodes and to read the serving CA ConfigMap in battery-operator's
  namespace, and SHALL grant the Holder no permission.

KF-196 is the claim design's successor to KF-110. Each permission has one
user. The claim backend creates, renews, watches and deletes its claims,
and creates the Secret of each claim (KF-150 to KF-155). It declares and
updates the Pools of its Profiles, and watches them for the available
count. It requests each claim token for the Holder, with `TokenRequest` on
`serviceaccounts/token` (KF-186). The Executor reads the Host Services
from the Node of the Job's Host (KF-189). The claim backend reads the
serving CA from the ConfigMap that battery-operator writes in its own
namespace. The Holder needs no permission: the Exec Agent checks a claim
token with a TokenReview, which asks nothing of the Holder.

## Hardening {#cluster-hardening}

- **KF-130** (withdrawn) battery-operator's Exec Agent runs a command only
  with a claim token of a Bound claim for that MicroVM (battery-operator
  EA-010 to EA-013). There is no kubelet API to authorize. It was
  implemented by `flr kubelet`, removed in #101.
- **KF-131** (withdrawn) battery-operator's Exec Agent refuses a request
  whose TokenReview or claim lookup cannot be completed (battery-operator
  EA-014). It was implemented by `flr kubelet`, removed in #101.
- **KF-132** (withdrawn) Nothing in the Fleet Manifests makes a
  SubjectAccessReview any more (KF-130). It was implemented by
  `deploy/host-agent/rbac.yaml`, removed in #101.
- **KF-133** (withdrawn) battery-operator's Manifests include the admission
  policy of its Exec Agent (battery-operator EA-051, EA-053), and the Host
  Agent has one of its own for KF-194. It was implemented by
  `deploy/host-agent/admission-policy.yaml`, removed in #101.
- **KF-134** (withdrawn) battery-operator's Exec Agent confirms that its
  identity names its own Host (battery-operator EA-050). It was implemented
  by `flr kubelet` and `deploy/host-agent/admission-policy.yaml`, removed in
  #101.
- **KF-135** (withdrawn) No container of the Host Agent reaches
  `flintlockd` any more: battery-operator's Exec Agent does, from a
  DaemonSet of its own and over mutual TLS (battery-operator EA-001,
  EA-004). The Fleet Manifests still run no container as the user id that
  battery-operator HI-070 admits. It was never cited.
- **KF-138** (withdrawn) Node Leases belong to the Virtual Nodes, which the
  cluster fleet no longer uses after the change to battery's claim resources
  of 2026-09-22; it was never implemented.
- **KF-139** The Fleet Manifests SHALL run the `buildkitd` container of the
  Host Agent in the `container_engine_t` SELinux domain, and SHALL NOT name a
  domain for any other container.

KF-139 is the one exception to the container domain that battery-operator
HI-066 assigns. Rootless `buildkitd` runs every `RUN` step of a Job's image
build as a container of its own, and each step mounts a fresh `devpts`,
which the ordinary container domain may not do, so under enforcing SELinux
every build step would be refused. `container_engine_t` is the domain the base policy
provides for exactly this, a container engine inside a container: it may
create user namespaces and mount what a nested container needs, and it is
still confined, with no access to the Host's files beyond those labelled for
containers. Naming it for `buildkitd` alone keeps every other Host Service
in the ordinary domain.

KF-113 keeps the Host Agent in the Host's network namespace for one reason:
the Host Services bind to the bridge gateway, and the bridge exists only
there. The Host Agent no longer reaches `flintlockd`. battery-operator's
Exec Agent does, from a DaemonSet of its own, over mutual TLS.

Every process in the Host's network namespace, any pod with host networking
included, can open a connection to the Host's own addresses. `flintlockd`
admits any certificate from its client CA, so battery-operator HI-070
admits one user id, the Exec Agent's, to its port from the Host, and the
Fleet Manifests run no container of the Host Agent as that user id. This
keeps out unprivileged host-network pods. A privileged pod can already do
anything on the Host, so it is not the threat this addresses.

## Test doubles {#cluster-test-doubles}

- **KF-120** (withdrawn) The `agent-exec` Guest Transport is tested against
  a test double of battery-operator's Exec Agent (KF-190). Its tests were in
  `internal/kubelet`, removed in #101.
- **KF-121** (withdrawn) The claim backend is tested against a fake battery
  that serves the claim resources (KF-191). Its tests were in
  `internal/poolmgr/kube`, removed in #101.
- **KF-122** (withdrawn) The harness runs every scenario of TD-051 over the
  claim stack (KF-192). It was never implemented.
- **KF-123** (withdrawn) battery, not the Runner, decides which claim gets a
  MicroVM, and battery-operator's tests cover it (battery-operator TD-026).
  Its tests were in `internal/poolmgr/kube`, removed in #101.
- **KF-124** (withdrawn) battery-operator's tests cover the drain guard of
  its Exec Agent (battery-operator EA-040, TD-025). Its tests were in
  `internal/kubelet`, removed in #101.
- **KF-125** (withdrawn) The harness restarts the Exec Agent while a Stage
  runs (KF-193). Its tests were in `internal/kubelet`, removed in #101.
- **KF-136** (withdrawn) battery-operator's e2e suite tests the TokenReview
  of its Exec Agent (battery-operator TD-025). Its tests were in
  `internal/kubelet`, removed in #101.
- **KF-137** (withdrawn) battery-operator's e2e suite tests its admission
  policies (battery-operator TD-025). Its tests were in `internal/kubelet`,
  removed in #101.

## Battery claim resources {#battery-claims}

- **KF-150** Where the claim backend is configured, the Scheduler SHALL
  obtain a MicroVM for a Job by creating a `MicroVMClaim` of
  `battery.liquidmetal-x.dev/v1alpha1` whose `spec.poolRef.name` names the
  Pool of the Job's Profile and whose `spec.serviceAccountName` names the
  configured Holder, and SHALL treat the claim as granted only once its
  `status.phase` is `Bound`.
- **KF-151** The Scheduler SHALL take the claimed MicroVM's uid, the Host's
  node name and the Exec Agent's address from the Bound claim's
  `status.microVM.uid`, `status.host.nodeName` and `status.host.agentAddress`,
  and from nothing else.
- **KF-152** While a Job holds a Bound claim, the Scheduler SHALL renew the
  claim's Lease by setting its `spec.renewTime` at the Profile's heartbeat
  interval.
- **KF-153** When a Job ends, the Scheduler SHALL delete its claim, which
  releases the MicroVM to battery.
- **KF-154** If a claim's `status.phase` is `Expired`, or the claim
  disappears while its Job runs, then the Scheduler SHALL treat the Lease as
  lost as SC-061 requires.
- **KF-155** If a claim is in the phase `Pending` with the condition
  `Bound` false and the reason `PoolExhausted`, then the Scheduler SHALL
  treat the Pool as exhausted as SC-021 requires and SHALL delete the claim
  when it stops waiting.
- **KF-156** The claim backend SHALL implement the `poolmgr.Client`
  interface, so that the Scheduler's requirements in `03-scheduler.md` hold
  unchanged over it.

battery keeps its MicroVMs internal: the Kubernetes surface is `Pool` and
`MicroVMClaim` only, and there is no `MicroVM` resource. A claim names what
it gets, a MicroVM, and where from, a Pool, the way a PersistentVolumeClaim
names a volume and its storage class.

The shapes of both resources are fixed in
[battery-operator v0.1.0](https://github.com/phoban01/battery-operator/releases/tag/v0.1.0),
an operator in front of an unmodified battery v0.3.3. Its
`01-resources.md` and `02-claims.md` settle what this section left open
when it was written:

- The API group and version are `battery.liquidmetal-x.dev/v1alpha1`.
- The Holder renews a Lease by setting the claim's `spec.renewTime`. The
  operator relays each change to battery as a `Heartbeat` and writes
  battery's expiry to `status.leaseExpiresAt`.
- A claim that waits for an exhausted Pool stays `Pending`, with the
  condition `Bound` false and the reason `PoolExhausted`.
- Anyone who may write `Pool` resources declares Pools. The Runner declares
  them from its Profiles, as PL-010 does over gRPC.
- A claim names its Holder in `spec.serviceAccountName`, and this field
  cannot change. The Exec Agent admits a request only with a claim token:
  a token of the Holder bound to the claim's Secret `<claim name>-exec`.
- battery chooses the lease id, and the claim records it in
  `status.leaseID`.

The claim backend builds on battery-operator's `api/v1alpha1` and its
Client Library, `pkg/claimclient`, at v0.1.0. The Client Library creates the
claim, its Secret and its claim tokens, waits for `Bound`, renews the claim
and deletes it.

## Host faults {#claim-host-faults}

- **KF-200** Where the claim backend is configured, the Scheduler SHALL
  probe the Exec Agent of each claim that a Job holds at the configured
  Host health interval, by calling `GetMicroVM` for the claim's MicroVM
  with a claim token of that claim, bounded by the configured Host call
  deadline.
- **KF-201** If the Exec Agent of a claim that a Job holds fails as many
  consecutive probes as the configured Host unhealthy threshold, then the
  Scheduler SHALL abort the Job with the failure reason
  `runner_system_failure` and delete the claim.
- **KF-202** The Scheduler SHALL abort a Job under KF-201 no later than the
  Host unhealthy threshold times the sum of the Host health interval and
  the Host call deadline after the claim's Exec Agent stops answering.

These requirements take the place of SC-040 to SC-043 for a claim. The
Runner of the claim design has no Inventory and reaches no `flintlockd`
(KF-063), so it cannot probe Hosts. Without them, a Job whose Host goes bad
fails only when its own timeout runs out, as `job_execution_timeout`.

The Runner learns of a fault on a claim's Host in two ways:

- A claim that goes `Expired`, or is deleted, ends the Job through KF-154
  at the next heartbeat.
- The Exec Agent stops answering for the claim's MicroVM: the Host is
  down or cut off, `flintlockd` hangs, or the MicroVM is gone. KF-200 to
  KF-202 cover this.

battery-operator's Exec Agent names `GetMicroVM` as a client's liveness
probe. It relays the call to the Host's `flintlockd` for the claim's
MicroVM, so an answer proves the Host, `flintlockd` and the MicroVM at
once. Each probe dials the Exec Agent afresh, as the claim's Holder, so a
Host that stops answering is not hidden by a connection that is still
open. A failed probe counts whatever the cause, and a single passing probe
resets the count, so an Exec Agent restart (KF-193) does not abort a Job.
With the defaults, a threshold of 3, an interval of 10 seconds and a
deadline of 10 seconds, a Job fails within 60 seconds of its Exec Agent
going quiet.

battery-operator does not mark a claim when its Host goes not ready. The
Exec Agent reports the Host not ready on its Node, and the Inventory
Controller then stops placing MicroVMs there, but a claim already Bound on
the Host stays `Bound` (battery-operator#186). The Runner does not watch
Nodes for it: that needs permissions beyond KF-196, and the probe already
catches the faults that stop a Job. It misses only a Host that reports
itself not ready while `flintlockd` still answers, such as one whose KVM
device or thin pool has gone. A Job that is running there may still finish.

## Inventory {#battery-inventory}

- **KF-160** (withdrawn) battery-operator's Inventory Controller decides
  which Nodes are Hosts, from each Node's schedulability and the Host
  readiness its Exec Agent reports (battery-operator IN-001, IN-003), so
  this repository has no Inventory Controller. It was never implemented.
- **KF-161** (withdrawn) battery-operator's Inventory Controller removes a
  cordoned, deleted or not ready Host from battery's Hosts
  (battery-operator IN-002). It was never implemented.

The Inventory Controller runs in battery-operator from v0.1.0. It gives
battery its Hosts by rewriting battery's configuration and restarting it,
because battery v0.3.3 has no call to change them.

## Exec Agent {#exec-agent}

- **KF-170** (withdrawn) battery-operator's Manifests run its Exec Agent on
  every Host, as a DaemonSet of their own (battery-operator EA-004); the
  Host Agent no longer contains one (KF-070). It was implemented by
  `flr agent`, removed in #74.
- **KF-171** (withdrawn) battery-operator's Exec Agent reaches only its own
  Host's `flintlockd`, over mutual TLS (battery-operator EA-001). It was
  implemented by `flr agent`, removed in #74.
- **KF-172** (withdrawn) battery-operator's Exec Agent serves its exec API
  over TLS on the Host's internal address (battery-operator EA-002, EA-068).
  It was implemented by `flr agent`, removed in #74.
- **KF-173** (withdrawn) battery-operator's Exec Agent authenticates every
  request with a TokenReview (battery-operator EA-010). It was implemented by
  `flr agent`, removed in #74.
- **KF-174** (withdrawn) battery-operator's Exec Agent runs a command only
  with a claim token of a Bound claim that names the MicroVM and the Host
  (battery-operator EA-011 to EA-013). It was implemented by `flr agent`,
  which checked the claim's creator instead, removed in #74.
- **KF-175** (withdrawn) battery-operator's Exec Agent refuses a request
  whose TokenReview or claim lookup cannot be completed (battery-operator
  EA-014). It was implemented by `flr agent`, removed in #74.
- **KF-176** (withdrawn) battery-operator's Exec Agent relays each request
  to `MicroVMExec.ExecCommand` and ends every response with an exit status
  frame (battery-operator EA-020). It was implemented by `flr agent`, removed
  in #74.
- **KF-177** (withdrawn) battery-operator's Exec Agent ends a response as a
  stream failure when `flintlockd` does not open the exec stream in time
  (battery-operator EA-021). It was implemented by `flr agent`, removed in
  #74.
- **KF-178** (withdrawn) battery-operator's Exec Agent checks its Host
  (battery-operator EA-030 to EA-033). It checks no Host Service; a unit of
  the Host Image can still report one through the not ready reason
  directory (EA-033). It was implemented by `flr agent`, removed in #74.
- **KF-179** (withdrawn) The Host Agent publishes the Host Services on its
  Host's Node under the same annotations (KF-194). It was implemented by
  `flr agent`, removed in #74.
- **KF-180** (withdrawn) battery-operator's Manifests include the admission
  policy of its Exec Agent (battery-operator EA-051); the Host Agent has one
  of its own for KF-194. It was implemented in `deploy/agent`, removed in
  #74.
- **KF-181** (withdrawn) battery-operator's Exec Agent holds a drain open
  while claims are Bound on its Host (battery-operator EA-040). It was
  implemented by `flr agent`, removed in #74.
- **KF-182** (withdrawn) battery-operator's Exec Agent publishes its Node
  report under the prefix `battery.liquidmetal-x.dev/` (battery-operator
  EA-034), which its Inventory Controller reads. It was implemented by
  `flr agent`, removed in #74.

The Exec Agent is battery-operator's from its v0.1.0. It began here as
KF-170 to KF-182, and battery-operator's `05-exec-agent.md` took them over,
EA-001 to EA-068, in the order they appear. What the Runner needs of it is
its protocol: flintlock's `MicroVMExec` service with the `ServerInfo` and
`GetMicroVM` calls of the `MicroVM` service, over TLS verified against the
Operator's serving CA, with a claim token as the bearer token of every call
(battery-operator EA-003, EA-010 to EA-013). The `agent-exec` Guest
Transport speaks it (`#agent-exec-transport`).

Authorization is finer than here. `flr agent` asked whether the caller had
created a claim on the MicroVM; battery-operator's Exec Agent asks for a
token bound to that one claim, so a Runner's credentials reach each MicroVM
through its own claim, and a token dies with its claim.

The Runner reaches each Exec Agent on the Host's internal address, which is
a network route the operator has to allow from wherever the Runner runs.

## Agent exec transport {#agent-exec-transport}

- **KF-185** Where the claim backend is configured, the Executor SHALL run
  each Stage through the Exec Agent of the Host named in the Job's claim,
  with the Stage script on standard input.
- **KF-186** The `agent-exec` Guest Transport SHALL authenticate to the Exec
  Agent with a claim token of the Job's claim and SHALL verify the agent's
  serving certificate against the configured certificate authority.
- **KF-187** The `agent-exec` Guest Transport SHALL treat a response that
  ends without an exit status frame as a stream failure, as EX-023 requires.
- **KF-188** The `agent-exec` Guest Transport SHALL meet every requirement
  that `02-executor.md` places on the `exec` Guest Transport for output
  streaming, exit status, cancellation and timeouts.
- **KF-189** The Executor SHALL read the Host Service addresses for a Job
  from the annotations of KF-194 that the Host Agent publishes on the Node
  of the Job's Host.
- **KF-195** Where the claim backend is configured, the Runner SHALL
  reject a configuration in which a Profile names the `ssh` Guest
  Transport.

battery-operator's Exec Agent relays flintlock's `MicroVMExec` service
and nothing else; it has no SSH proxy. So under the claim backend a Profile
names `exec` or no Guest Transport, and the Executor runs it over
`agent-exec` (KF-185). An `ssh` Profile is refused when the configuration
loads, not when its first Job fails.

A claim token is the token that battery-operator's Client Library requests
for a claim (battery-operator CC-002, CC-011, CC-020). It is a
`TokenRequest` for the claim's Holder, bound to the claim's Secret
`<claim name>-exec` by name and uid, with the Exec Agent's audience
`battery.liquidmetal-x.dev/exec-agent`. battery-operator's Exec Agent admits
a request only with such a token (battery-operator EA-010 to EA-013). The
Runner's own ServiceAccount token opens nothing, and a token of one claim
opens no other claim's MicroVM. So the Runner holds one connection to an
Exec Agent for each claim, not one for each Host.

## Claim test doubles {#claim-test-doubles}

- **KF-190** The `agent-exec` Guest Transport SHALL be tested with
  battery-operator's Client Library against a test double of
  battery-operator's Exec Agent in front of the fake Host, with no KVM and
  no battery.
- **KF-191** The claim backend SHALL be tested against a fake battery that
  serves the `Pool` and `MicroVMClaim` resources of
  `battery.liquidmetal-x.dev/v1alpha1` and binds claims from warm MicroVMs on
  the fake Hosts.
- **KF-192** The harness SHALL run every scenario of TD-051 over the claim
  backend, the `agent-exec` Guest Transport, the Exec Agent and the fake
  Host.
- **KF-193** The harness SHALL include a scenario in which the Exec Agent
  restarts while a Stage runs, and SHALL assert that the Job fails with a
  stream failure and is never reported as having succeeded.

## Release {#cluster-release}

- **KF-140** The Release SHALL publish a container image of `flr` for
  `linux/amd64` and `linux/arm64` to the project's container registry,
  tagged with the release version.
- **KF-141** The container image of `flr` SHALL run as a non-root user and
  SHALL contain nothing but the `flr` binary, certificate authority
  certificates and time zone data.
- **KF-142** (withdrawn) The Release no longer publishes a Host Image.
  battery-operator builds, checks and publishes its reference Host Image
  (its ADR 0007, decision 5; the image's own requirements are its HI-001 to
  HI-008). battery-operator has no requirement for the publishing itself.
- **KF-143** The Release SHALL publish the Fleet Manifests as one release
  asset for the workload cluster, in which every container image of this
  project is referenced by digest.
- **KF-144** The Fleet Manifests SHALL reference every third-party container
  image they use, including those of the Host Services, by digest.
- **KF-145** The Release SHALL NOT publish a `latest` tag or any other tag
  that moves.

KF-143 had a second asset, for the Cluster API objects of the management
cluster. Those objects are now battery-operator's Host Pool Templates, so
the Release publishes the workload cluster's asset alone. The registry's
packages have to be public, or readable by the cluster, for a fleet to pull
them; that is set once on the registry and is not something a release can
check.

A release candidate publishes images too, so that a candidate can be tried
on a cluster before its version is released. Every reference that a
cluster fleet pulls from this project is immutable, so a Runner or a Host
Agent started from a release's manifests runs exactly what that release
tested. The Hosts run the Host Image and AMI that the operator chose from
battery-operator's releases (its HI-009).

## Relation to the other documents {#relation-to-other-documents}

This section is not normative.

| Elsewhere | Cluster fleet |
|-----------|---------------|
| Pool declaration, PL-010 to PL-017 | `Pool` resources, KF-150, KF-156 |
| Claim, heartbeat, release and lease expiry in `04-pool-manager.md` | `MicroVMClaim` resources, KF-150 to KF-155 |
| Pool availability and events in `04-pool-manager.md` | a watch on the `Pool` resources, KF-155 |
| Placement resolution in `03-scheduler.md`, SC-034 | the Host the Bound claim names, KF-151, KF-126 |
| Host client and Inventory, `05-hosts.md` | none; the Runner reaches only the Exec Agents, KF-063 |
| Host health, SC-040 to SC-043 | a probe of each claim's Exec Agent, KF-200 to KF-202 |
| `exec` Guest Transport in `02-executor.md` | `agent-exec`, KF-185 to KF-188 |
| Discovery, FL-001 to FL-006, FL-116 | battery-operator's Inventory Controller |
| KVM check, FL-117 | battery-operator HI-011 and its Exec Agent |
| Remote execution, FL-010 to FL-014 | none; nothing is pushed to a Host |
| Host provisioning, FL-020 to FL-027, FL-029, FL-030 | battery-operator's Host Image (its `11-host-image.md`) |
| Pre-pull, FL-028 | battery's warm MicroVMs pull their own images |
| Host networking, FL-040 to FL-046 | battery-operator HI-030 to HI-037, HI-075 to HI-080 |
| Pool Manager install, FL-051 to FL-055 | battery-operator's Manifests |
| Inventory, FL-060 to FL-064 | none; battery-operator's Inventory Controller gives battery its Hosts |
| Verification, FL-070 to FL-073, FL-109 | not yet specified for the claim design |
| Drain, FL-080, FL-084 | battery-operator's Exec Agent and Inventory Controller |
| Teardown, FL-081 to FL-083 | deleting the manifests and the Host pool's MachineDeployment |
| Launch template mode, FL-090 to FL-092 | battery-operator's Host Pool Templates, its HP-001 to HP-031 |
| Host services, FL-100 to FL-115 | KF-070 to KF-076, KF-194, KF-197 to KF-199, battery-operator HI-078 to HI-080 |
| Certificate authority and Host mutual TLS, SE-023, FL-024 | battery-operator's Host certificates, its EA-060 to EA-068 |
| IAM policy, SE-040 to SE-042 | KF-112, KF-196, and battery-operator HP-030 for the Hosts |

This document withdraws no requirement of another document. Once a cluster
fleet has passed the hardware tier, the battery client, the Host client,
push provisioning and their requirements can be retired together, in the
change that deletes the code, because a withdrawn requirement cannot keep
its citations.
