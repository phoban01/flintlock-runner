# Cluster fleet {#cluster-fleet}

This document specifies how a fleet is run from a Kubernetes cluster, with
every MicroVM visible to the cluster as a pod. Hosts are nodes of an existing
workload cluster, created by a Cluster API MachineDeployment from the Host
Image (`11-host-image.md`). On each Host a Pod Provider, built on virtual
kubelet, registers a second Node, the Virtual Node, and realises every pod
bound to it as one flintlock MicroVM through the `flintlockd` on that Host.

A Pool is then a ReplicaSet of idle MicroVM pods, and the Kubernetes API
does the work that `04-pool-manager.md` asks of battery: the ReplicaSet
controller keeps the Pool at size and replaces a claimed MicroVM at once,
the scheduler places MicroVMs on Hosts against their real capacity, and a
claim is a compare-and-swap on a pod label. The Runner keeps its Scheduler
and Executor unchanged above the `poolmgr.Client` interface; this document
specifies a second implementation of that interface and a Guest Transport
that reaches the guest through the pod's exec subresource.

Nothing here runs a Job in a container. The pod is the cluster's handle on a
MicroVM: it carries the MicroVM's resource requests, its placement, its
lifecycle and its claim state, so that quotas, metrics, drains and
`kubectl get pods` all tell the truth about what a Host is doing.

This mode is an alternative to `04-pool-manager.md`, `05-hosts.md` and
`06-fleet.md`, which stay in force for fleets that are not run from a
cluster. The table at the end maps each of them to its counterpart here.

**Superseded on 2026-09-22.** The cluster fleet is moving from the Virtual
Node design described first in this document to battery's claim resources,
specified in the sections from `#battery-claims` onwards: battery stays the
scheduler and the authority over Pools, the Runner claims a MicroVM through a
`MicroVMClaim`, and a per-Host Exec Agent relays each Stage to `flintlockd`.
The resources, the Exec Agent and the Inventory Controller ship in
battery-operator v0.1.0 (`#battery-claims`).
These sections are superseded by that design: Virtual Node, MicroVM pods,
Pools, Allocation, Guest Transport, Drain, Verification, Least privilege
(KF-110, KF-111), Hardening (KF-130 to KF-137) and Test doubles (KF-120 to
KF-125). battery-operator's Manifests now run the Exec Agent in the Pod
Provider's place (`#exec-agent`). Their code is still on `main`
and still cites them, so they are withdrawn in the change that removes that
code, once the claim design passes the harness, rather than now.

## Host pool {#host-pool}

- **KF-001** The Fleet Manifests SHALL define each Host pool as one Cluster
  API MachineDeployment with a single instance type and an AMI given by
  explicit id.
- **KF-002** The Fleet Manifests SHALL join every Host with the taint
  `gitlab-runner.flintlock.dev/host=true:NoSchedule`, so that only pods that
  tolerate it run on a Host's own Node.
- **KF-003** The Fleet Manifests SHALL write the Host configuration file of
  HI-050 through the bootstrap configuration of the MachineDeployment.
- **KF-004** The Fleet Manifests SHALL set no node drain timeout shorter than
  the configured drain timeout on a Host pool's Machines.
- **KF-005** The Fleet Manifests SHALL define a MachineHealthCheck for each
  Host pool that replaces a Machine whose Node stays not ready beyond the
  configured interval.

A known gap: the MachineHealthCheck of KF-005 watches the Host's own Node,
and a Host that cannot run MicroVMs, for want of KVM (HI-011) or of a thin
pool device (HI-022), reports that on its Virtual Node instead (KF-016). Such
a Host stays in the pool, advertising no ready capacity, until an operator
replaces it. Closing the gap needs the Pod Provider to set a condition on
the Host's Node, which KF-133 forbids today.

One instance type per pool keeps capacity arithmetic trivial today and is
what a snapshot compatibility class will need later. The number of Hosts is
the replica count of the MachineDeployment. Because MicroVMs are now pods
with requests, pending Pool pods are a real demand signal that an autoscaler
could act on, but nothing in this document requires one.

## Virtual Node {#virtual-node}

- **KF-010** The Pod Provider SHALL register one Virtual Node for its Host,
  named after the Host's Node with the suffix `-microvms`, and SHALL renew
  its node lease while it runs.
- **KF-011** The Pod Provider SHALL set the Host's Node as the owner of the
  Virtual Node, so that the Virtual Node is removed when the Host's Node is.
- **KF-012** The Pod Provider SHALL advertise as the Virtual Node's capacity
  the Host's CPU and memory minus the configured Host reserve, and a pod
  limit equal to the configured maximum number of MicroVMs per Host.
- **KF-013** The Pod Provider SHALL copy the architecture and the
  `gitlab-runner.flintlock.dev` labels of the Host's Node to the Virtual
  Node and SHALL add the label `gitlab-runner.flintlock.dev/virtual-node`
  set to `true` and the label `gitlab-runner.flintlock.dev/host-node` set to
  the name of the Host's Node.
- **KF-014** The Pod Provider SHALL taint the Virtual Node with
  `gitlab-runner.flintlock.dev/microvm=true:NoSchedule`, so that only pods
  meant to be MicroVMs are bound to it.
- **KF-015** The Pod Provider SHALL report the Virtual Node ready only while
  the local `flintlockd` answers `ServerInfo` with the exec service enabled
  and every enabled Host Service accepts connections on the bridge gateway
  address.
- **KF-016** If a unit of the Host Image reports a not ready reason, then the
  Pod Provider SHALL report the Virtual Node not ready with that reason in
  the condition's message.
- **KF-017** The Pod Provider SHALL publish the address and port of each
  enabled Host Service as annotations on the Virtual Node.
- **KF-018** (withdrawn) battery-operator's Exec Agent takes the Pod
  Provider's place as the client of `flintlockd` on a Host, and reaches it
  over mutual TLS on the Host's address (battery-operator EA-001), not
  through the local endpoint of HI-042. It was implemented by
  `flr kubelet`, whose citation is removed.

A Virtual Node per Host, rather than one for the fleet, is what lets the
scheduler place MicroVMs: each Virtual Node has the real capacity of one
machine, so bin-packing, spreading and selectors mean what they usually
mean. The Host's own Node keeps only the Host reserve as allocatable
(HI-061), which is where the Host Agent's pods run, so the two Nodes never
promise the same core twice.

## MicroVM pods {#microvm-pods}

- **KF-020** When a pod is bound to the Virtual Node, the Pod Provider SHALL
  create one MicroVM for it through `flintlockd`, using the image of the
  pod's only container as the root filesystem image, the container's CPU
  and memory limits as the MicroVM's vCPU count and memory, and the pod's
  `gitlab-runner.flintlock.dev` annotations for the kernel image, the
  kernel command line and the hypervisor.
- **KF-021** The Pod Provider SHALL label the MicroVM with the pod's UID,
  namespace and name and SHALL set `allow_guest_agent` on it.
- **KF-022** If a pod has more than one container, an init container, a
  volume, a host namespace or a mounted service account token, then the Pod
  Provider SHALL mark the pod failed with a reason naming the unsupported
  field and SHALL NOT create a MicroVM for it.
- **KF-023** Where the pod names a cloud-init ConfigMap in its annotations,
  the Pod Provider SHALL pass that ConfigMap's user data to the MicroVM.
- **KF-024** The Pod Provider SHALL report the pod running and ready only
  when `flintlockd` reports the MicroVM created and a command run through
  `MicroVMExec` in the guest succeeds.
- **KF-025** The Pod Provider SHALL report the Host's internal address as
  the pod's host address and the guest's bridge address as the pod's
  address.
- **KF-026** If the MicroVM fails or disappears from `flintlockd`, then the
  Pod Provider SHALL mark its pod failed with the reason `flintlockd`
  gives.
- **KF-027** When a pod is deleted, the Pod Provider SHALL delete its
  MicroVM and SHALL report the pod terminated only after `flintlockd`
  no longer lists the MicroVM.
- **KF-028** When starting, the Pod Provider SHALL adopt every MicroVM whose
  labelled pod still exists and is bound to its Virtual Node, without
  restarting it, and SHALL delete every MicroVM in its namespace whose pod
  does not.
- **KF-029** The Pod Provider SHALL NOT delete or restart a MicroVM because
  the Pod Provider itself stops, restarts or loses its connection to the
  Kubernetes API.
- **KF-030** The Pod Provider SHALL serve the pod exec endpoint of the
  kubelet API by relaying the streams to `MicroVMExec.ExecCommand` on the
  local `flintlockd` and SHALL return the command's exit status.
- **KF-031** The Pod Provider SHALL serve its kubelet API over TLS and SHALL
  reject every request that does not authenticate with a client certificate
  issued by the cluster's kubelet client certificate authority.
- **KF-032** If a claimed pod's lease annotation is older than the
  configured lease duration, then the Pod Provider SHALL delete that pod.

The provider only has to support the pods this project creates, which is why
KF-022 refuses everything else rather than approximating it. A MicroVM is a
machine with its own root filesystem and kernel, not a sandbox around
containers, so there is nowhere for a second container or a projected volume
to go. KF-029 is the counterpart of HI-040: a job outlives a restart of
every component between it and the cluster. KF-031 matters more than it
looks, because the exec endpoint is a shell in every job on the Host.

Pods on a Virtual Node have no cluster networking. Guests stay on the
Host-local NAT'd bridge of `11-host-image.md`, which is all a CI job needs,
and the address of KF-025 is informational.

## Pools {#kube-pools}

- **KF-040** Where the Kubernetes pool backend is configured, the Scheduler
  SHALL create or update one ReplicaSet per Profile in the Runner's
  namespace, with the Pool size as its replica count and a pod template
  derived from the Profile as KF-020 expects.
- **KF-041** The Scheduler SHALL give every Pool pod a toleration for the
  taint of KF-014 and a node selector built from the Profile's architecture
  and Host selector and the label `gitlab-runner.flintlock.dev/virtual-node`.
- **KF-042** The Scheduler SHALL give every Pool pod a topology spread
  constraint over Virtual Nodes, so that a Pool's warm MicroVMs are spread
  across Hosts.
- **KF-043** The Scheduler SHALL label every Pool pod with the Runner name,
  the Profile name and `gitlab-runner.flintlock.dev/state` set to `idle`,
  and SHALL select the ReplicaSet's pods by all three.
- **KF-044** When claiming from a Pool, the Scheduler SHALL choose a ready
  idle pod of that Pool's current template and SHALL set its state label to
  `claimed`, its lease annotation to the current time and its active
  deadline to the Job timeout in one update conditioned on the pod's
  resource version.
- **KF-045** If the claim update is rejected because the pod changed, then
  the Scheduler SHALL try another ready idle pod, and SHALL treat the Pool
  as exhausted only when none is left.
- **KF-046** The Scheduler SHALL return the claimed pod's name as the Lease
  id and the pod's Virtual Node as the Placement.
- **KF-047** When a Lease heartbeat is due, the Scheduler SHALL update the
  pod's lease annotation.
- **KF-048** When a Lease is released, the Scheduler SHALL delete the pod.
- **KF-049** The Scheduler SHALL derive each Pool's available count from a
  watch on the Pool's ready idle pods and SHALL wake Jobs waiting on an
  exhausted Pool when that count rises.
- **KF-050** When a Profile's pod template changes, the Scheduler SHALL
  delete the idle pods of the previous template at no more than the
  configured rate and SHALL NOT delete or modify a claimed pod.
- **KF-051** When a Profile is removed by a configuration reload, the
  Scheduler SHALL NOT delete its ReplicaSet and SHALL log that the Pool is
  no longer referenced.
- **KF-052** The Kubernetes pool backend SHALL implement the same
  `poolmgr.Client` interface as the battery client, so that the Scheduler's
  requirements in `03-scheduler.md` hold unchanged over either.

Changing the state label takes the pod out of the ReplicaSet's selector. The
ReplicaSet controller sees one pod too few and creates a replacement
immediately, which is battery's immediate-on-lease replenishment with no
code. The resource version makes the claim atomic: of two Runners racing for
one pod exactly one update succeeds. The active deadline is the backstop for
a Runner that dies without releasing, and KF-032 is the prompt path for the
same failure.

## Allocation {#kube-allocation}

- **KF-126** Where the Kubernetes pool backend is configured, the Scheduler
  SHALL record the claimed pod's Virtual Node as the Placement without
  looking it up in the Inventory, and SC-034 SHALL NOT apply.
- **KF-127** When claiming for a Job, the Scheduler SHALL pass the Job's own
  timeout to the Kubernetes pool backend, and the backend SHALL set the
  active deadline of KF-044 to that timeout plus the configured cleanup
  margin, using its configured default only for a Job that has none.
- **KF-128** Where the Kubernetes pool backend is configured, the Executor
  SHALL use the `kube-exec` Guest Transport for every Profile, and the
  Runner SHALL reject a configuration that names any other Guest Transport.

A cluster fleet has no Inventory, so SC-034, which fails an allocation whose
Host the Inventory does not know, would fail every one; KF-126 takes its
place. The Virtual Node is enough to run the Job, because `kube-exec` reaches
the guest through the pod and KF-062 finds the Host Services on the Virtual
Node. KF-127 matters because the active deadline ends the pod: a Job that is
allowed three hours must not have its MicroVM deleted at the two-hour
default. The margin is there because the deadline must outlast the Job, not
equal it: the claim is made before the Job's own clock starts, and a Job
that times out still runs its `after_script` and uploads its failure
artifacts in the same MicroVM.

## Guest Transport {#kube-exec-transport}

- **KF-060** Where the `kube-exec` Guest Transport is configured, the
  Executor SHALL run each Stage by opening the exec subresource of the
  claimed pod through the Kubernetes API, with the Stage script on standard
  input.
- **KF-061** The `kube-exec` Guest Transport SHALL meet every requirement
  that `02-executor.md` places on the `exec` Guest Transport for output
  streaming, exit status, cancellation and timeouts.
- **KF-062** The Executor SHALL read the Host Service addresses for a Job
  from the annotations of the Virtual Node named in the Placement.
- **KF-063** Where the `kube-exec` Guest Transport is configured, the Runner
  SHALL NOT open any connection to a Host.

With this transport the Runner needs no Inventory, no Host certificates and
no route to the Hosts: its only dependency is the API server. Job output
passes through the API server and the Pod Provider's kubelet endpoint, the
same path the GitLab Kubernetes executor uses.

## Host Agent {#cluster-host-agent}

- **KF-070** The Fleet Manifests SHALL run the Host Agent as a DaemonSet that
  tolerates the Host taint, selects the Host label and contains the Host
  Services of FL-100.
- **KF-071** The Host Agent SHALL run in the Host's network namespace and
  SHALL bind every Host Service only to the guest bridge gateway address.
- **KF-072** The Host Agent SHALL keep all Host Service storage in the Host
  Service cache directory of HI-024, so that it outlives the pod and a
  reboot of the Host.
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

The Host Agent does not contain the Exec Agent. battery-operator's
Manifests run it, as a DaemonSet of their own (`#exec-agent`). That Exec
Agent knows nothing of the Host Services, so the Host Agent, which runs
them, publishes where they are (KF-194), for the Executor to read
(KF-189). It keeps the annotation names that `flr agent` used (KF-179), so
nothing that reads them changes. An admission policy lets the Host Agent's
identity change only these annotations, and only on its own Host's Node.

Pre-pulling Profile images needs no requirement of its own any more: a Pool
pod on every Host pulls its root filesystem and kernel images when it is
created, and KF-042 puts one there.

## Runner {#cluster-runner}

- **KF-080** The Fleet Manifests SHALL run the Runner as a Deployment that
  reads the Profiles from a ConfigMap and the GitLab runner token from a
  Secret.
- **KF-081** The Fleet Manifests SHALL give the Runner's pod a termination
  grace period no shorter than the configured job timeout, so that a rollout
  does not cut running Jobs short.
- **KF-082** The Fleet Manifests SHALL NOT schedule the Runner onto Hosts.

## Drain {#cluster-drain}

- **KF-090** When the Host's Node becomes unschedulable, the Pod Provider
  SHALL mark the Virtual Node unschedulable and SHALL delete the idle pods
  bound to it, so that their ReplicaSets replace them on other Hosts.
- **KF-091** While a claimed pod is bound to the Virtual Node, the Pod
  Provider SHALL prevent an eviction-based drain of the Host's Node from
  completing.
- **KF-092** When no claimed pod remains on an unschedulable Host, the Pod
  Provider SHALL let the drain of the Host's Node complete.
- **KF-093** If the configured drain timeout elapses while claimed pods
  remain, then the Pod Provider SHALL let the drain complete and SHALL log
  each pod it abandoned.
- **KF-094** When the Host's Node becomes schedulable again, the Pod
  Provider SHALL mark the Virtual Node schedulable.

Draining a Host's Node does not touch the pods of its Virtual Node, which is
a different Node object, so something has to tie the two together. The
expected mechanism for KF-091 is a guard pod that the Pod Provider keeps on
the Host's own Node while claimed pods exist, covered by a
PodDisruptionBudget that allows no disruption; a drain cannot finish while
it is there. Cordoning is the trigger because a MachineDeployment rollout or
scale-down, a MachineHealthCheck remediation and an operator's
`kubectl drain` all begin with it. Cluster API's pre-drain hook would do the
same, but Machines live in the management cluster and the Pod Provider
would need credentials for it.

## Verification {#cluster-verification}

- **KF-100** Where the Kubernetes pool backend is configured, the
  verification command SHALL create one verification pod bound by name to
  each ready Virtual Node, run a trivial command in it through the
  `kube-exec` Guest Transport, report the time from creation to readiness
  per Host and delete the pod.
- **KF-101** The verification command SHALL perform the Host Service checks
  of FL-109 from inside each verification pod.
- **KF-102** If a Virtual Node is not ready or its verification pod does not
  become ready within the verification timeout, then the verification
  command SHALL exit with a non-zero status naming the Host and the
  reason.

## Least privilege {#cluster-least-privilege}

- **KF-110** The Runner SHALL operate with a Role in its own namespace
  limited to managing ReplicaSets, reading, updating and deleting pods and
  creating pod exec sessions, and with read access to Nodes.
- **KF-111** The Pod Provider SHALL operate with permissions limited to its
  own Virtual Node and node lease, the pods bound to that Virtual Node, the
  ConfigMaps those pods name, reading its Host's Node and managing its guard
  pod.
- **KF-112** The Fleet Manifests SHALL NOT grant any workload an AWS
  permission.
- **KF-113** The Host Agent SHALL hold only the privileges it needs to bind
  to the bridge gateway, to write the cache directory and to reach the
  local `flintlockd` endpoint.

## Hardening {#cluster-hardening}

- **KF-130** The Pod Provider SHALL authorize every kubelet API request with
  a SubjectAccessReview of the requesting identity for the `nodes/proxy`
  resource on its Virtual Node and the verb the request maps to, and SHALL
  refuse the request unless the review allows it.
- **KF-131** If the SubjectAccessReview of a request cannot be completed,
  then the Pod Provider SHALL refuse the request.
- **KF-132** The Fleet Manifests SHALL grant the Pod Provider permission to
  create SubjectAccessReviews in addition to the permissions of KF-111.
- **KF-133** The Fleet Manifests SHALL include a ValidatingAdmissionPolicy
  that rejects any request by a Pod Provider to create, update, patch or
  delete a Node other than its own Virtual Node, or a pod that is neither
  bound to its own Virtual Node nor its own guard pod.
- **KF-134** The Fleet Manifests SHALL give each Pod Provider an identity
  that names its Host, so that the policy of KF-133 can tell one Host's Pod
  Provider from another's.
- **KF-135** (withdrawn) No container of the Host Agent reaches
  `flintlockd` any more: battery-operator's Exec Agent does, from a
  DaemonSet of its own and over mutual TLS (battery-operator EA-001,
  EA-004). The Fleet Manifests still run no container as the user id that
  HI-063 admits. It was never cited.
- **KF-138** (withdrawn) Node Leases belong to the Virtual Nodes, which the
  cluster fleet no longer uses after the change to battery's claim resources
  of 2026-09-22; it was never implemented.
- **KF-139** The Fleet Manifests SHALL run the `buildkitd` container of the
  Host Agent in the `container_engine_t` SELinux domain, and SHALL NOT name a
  domain for any other container.

KF-031 checks who a client is; KF-130 checks what that client may do, as a
real kubelet does. Without it any certificate from the kubelet client
certificate authority, which in most clusters is the cluster's own, opens a
shell in every Job on the Host.

Kubernetes RBAC cannot express "only the pods bound to this node", so the
Role behind KF-111 is necessarily cluster-wide and KF-111 is met by the code
rather than by the grant. KF-133 moves that boundary into the API server,
where a compromised Host can no longer cross it. The expected mechanism is a
bound ServiceAccount token, whose user information carries the name of the
node the Host Agent pod runs on
(`authentication.kubernetes.io/node-name`), compared in the policy with the
object's node; the Virtual Node's name is that node's name with the
`-microvms` suffix of KF-010.

KF-139 is the one exception to the container domain HI-066 assigns.
Rootless `buildkitd` runs every `RUN` step of a Job's image build as a
container of its own, and each step mounts a fresh `devpts`, which the
ordinary container domain may not do, so under enforcing SELinux every build
step would be refused. `container_engine_t` is the domain the base policy
provides for exactly this, a container engine inside a container: it may
create user namespaces and mount what a nested container needs, and it is
still confined, with no access to the Host's files beyond those labelled for
containers. Naming it for `buildkitd` alone keeps every other Host Service
in the ordinary domain.

KF-135 and HI-063 close the last way round KF-031: `flintlockd` has no
authentication of its own, and a loopback listener is reachable by every
process in the Host's network namespace, including any pod with host
networking that a DaemonSet puts there. Admitting one user id keeps out
unprivileged host-network pods; a privileged pod can already do anything on
the Host, so it is not the threat this addresses.

## Test doubles {#cluster-test-doubles}

- **KF-120** The Pod Provider SHALL be tested against a Kubernetes API
  server test environment and the fake Host, with no kubelet, no KVM and no
  AWS.
- **KF-121** The Kubernetes pool backend SHALL be tested against a
  Kubernetes API server test environment, with a test reconciler standing in
  for the ReplicaSet controller where no controller manager runs.
- **KF-122** The harness SHALL run every scenario of TD-051 over the
  Kubernetes pool backend, the `kube-exec` Guest Transport, the Pod Provider
  and the fake Host.
- **KF-123** The harness SHALL include a scenario in which two Runners claim
  from a Pool of one, and SHALL assert that exactly one obtains the pod.
- **KF-124** The harness SHALL include a scenario in which a Host's Node is
  cordoned while it runs a Job, and SHALL assert that the Job finishes, that
  the idle pods leave the Host and that the drain completes afterwards.
- **KF-125** The harness SHALL include a scenario in which the Pod Provider
  restarts while a Job runs, and SHALL assert that the Job's MicroVM is
  adopted and the Job finishes.
- **KF-136** The Pod Provider's authorization SHALL be tested against a
  Kubernetes API server test environment with one client identity that the
  review allows and one that it refuses, and SHALL be shown to run nothing
  for the second.
- **KF-137** The Fleet Manifests SHALL include a test, run against a
  Kubernetes API server test environment, in which the policy of KF-133
  admits a Pod Provider's change to its own Host's Virtual Node and pods and
  refuses the same change to another Host's.

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
- **KF-142** The Release SHALL publish the Host Image to the project's
  container registry, tagged with the release version and with the release
  version joined to the Kubernetes version it carries.
- **KF-143** The Release SHALL publish the Fleet Manifests as two release
  assets, one for the workload cluster and one for the Cluster API objects
  of the management cluster, in which every container image of this project
  is referenced by digest.
- **KF-144** The Fleet Manifests SHALL reference every third-party container
  image they use, including those of the Host Services, by digest.
- **KF-145** The Release SHALL NOT publish a `latest` tag or any other tag
  that moves.

The Host Image carries the Kubernetes version in a tag such as
`1.1.0-k8s-v1.35.8` rather than a bare `v1.35.8`: the bare tag would point at
a different image with every release, which KF-145 forbids, while the joined
one names exactly one build. The Cluster API objects are a separate asset
because they are applied to the management cluster, and the rest to the
workload cluster the Hosts join. The registry's packages have to be public,
or readable by both clusters, for a fleet to pull them; that is set once on
the registry and is not something a release can check.

A release candidate publishes images too, so that a candidate can be tried
on a cluster before its version is released. The Release does not publish
an AMI: making one needs an AWS account, which the release workflow does not
have, so an operator runs `make image-ami` against the published Host Image
in their own account (HI-009). Every reference that a cluster fleet pulls is
immutable, so a Host or a Runner started from a release's manifests runs
exactly what that release tested.

## Relation to the other documents {#relation-to-other-documents}

This section is not normative.

| Elsewhere | Cluster fleet |
|-----------|---------------|
| Pool declaration, PL-010 to PL-017 | ReplicaSets, KF-040 to KF-043, KF-050, KF-051 |
| Claim, heartbeat, release and lease expiry in `04-pool-manager.md` | KF-044 to KF-048, KF-032 |
| Pool availability and events in `04-pool-manager.md` | pod watch, KF-049 |
| Placement resolution in `03-scheduler.md`, SC-034 | the claimed pod's Virtual Node, KF-046, KF-126 |
| Host client and Inventory, `05-hosts.md` | none; the Runner reaches no Host, KF-063 |
| `exec` Guest Transport in `02-executor.md` | `kube-exec`, KF-060, KF-061 |
| Discovery, FL-001 to FL-006, FL-116 | the Virtual Nodes |
| KVM check, FL-117 | HI-011, KF-016 |
| Remote execution, FL-010 to FL-014 | none; nothing is pushed to a Host |
| Host provisioning, FL-020 to FL-027, FL-029, FL-030 | Host Image, `11-host-image.md` |
| Pre-pull, FL-028 | Pool pods pull their own images |
| Host networking, FL-040 to FL-046 | HI-030 to HI-037 |
| Pool Manager install, FL-051 to FL-055 | none; there is no Pool Manager daemon |
| Inventory, FL-060 to FL-064 | none; Virtual Nodes carry capacity, KF-012 |
| Verification, FL-070 to FL-073, FL-109 | KF-100 to KF-102 |
| Drain, FL-080, FL-084 | KF-090 to KF-094 |
| Teardown, FL-081 to FL-083 | deleting the manifests and the MachineDeployment |
| Launch template mode, FL-090 to FL-092 | the MachineDeployment, KF-001 to KF-005 |
| Host services, FL-100 to FL-115 | KF-070 to KF-076, KF-194, HI-036 |
| Certificate authority and Host mutual TLS, SE-023, FL-024 | battery-operator's Host certificates, its EA-060 to EA-068 |
| IAM policy, SE-040 to SE-042 | KF-110 to KF-112 |

No existing requirement is withdrawn by this document. Once a cluster fleet
has passed the hardware tier, the battery client, the Host client, push
provisioning and their requirements can be retired together, in the change
that deletes the code, because a withdrawn requirement cannot keep its
citations.
