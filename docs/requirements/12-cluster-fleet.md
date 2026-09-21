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
- **KF-018** The Pod Provider SHALL reach `flintlockd` only through the
  local endpoint of HI-042.

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
  tolerates the Host taint, selects the Host label and contains the Pod
  Provider and the Host Services of FL-100.
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

## Relation to the other documents {#relation-to-other-documents}

This section is not normative.

| Elsewhere | Cluster fleet |
|-----------|---------------|
| Pool declaration, PL-010 to PL-017 | ReplicaSets, KF-040 to KF-043, KF-050, KF-051 |
| Claim, heartbeat, release and lease expiry in `04-pool-manager.md` | KF-044 to KF-048, KF-032 |
| Pool availability and events in `04-pool-manager.md` | pod watch, KF-049 |
| Placement resolution in `03-scheduler.md` | the claimed pod's Virtual Node, KF-046 |
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
| Host services, FL-100 to FL-115 | KF-070 to KF-076, HI-036 |
| Certificate authority and Host mutual TLS, SE-023, FL-024 | none; `flintlockd` is local, KF-018, KF-031 |
| IAM policy, SE-040 to SE-042 | KF-110 to KF-112 |

No existing requirement is withdrawn by this document. Once a cluster fleet
has passed the hardware tier, the battery client, the Host client, push
provisioning and their requirements can be retired together, in the change
that deletes the code, because a withdrawn requirement cannot keep its
citations.
