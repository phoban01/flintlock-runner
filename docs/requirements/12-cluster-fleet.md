# Cluster fleet {#cluster-fleet}

This document specifies how a fleet is run from a Kubernetes cluster. Hosts
are nodes of an existing workload cluster, created by a Cluster API
MachineDeployment from the Host Image (`11-host-image.md`); the Runner and
the Pool Manager daemon are Deployments; the Host Services are a DaemonSet;
cert-manager is the fleet certificate authority. Kubernetes is the
management plane only. Jobs are never pods: the Scheduler still claims warm
MicroVMs from the Pool Manager, the Pool Manager still places them, and the
Guest Transport still reaches them through `flintlockd` on the Host, exactly
as `02-executor.md` to `05-hosts.md` specify.

The design adds one process, the Fleet Operator, and no custom resources.
Its whole job is to turn the cluster's view of the Hosts into the two files
the rest of the system already reads, the Inventory and the Pool Manager's
host list, and to hold a node drain open while jobs finish.

This mode is an alternative to the push provisioning of `06-fleet.md`, which
stays in force for fleets that are not run from a cluster. The table at the
end maps each part of that document to its counterpart here.

## Host pool {#host-pool}

- **KF-001** The Fleet Manifests SHALL define each Host pool as one Cluster
  API MachineDeployment with a single instance type and an AMI given by
  explicit id.
- **KF-002** The Fleet Manifests SHALL join every Host with the taint
  `gitlab-runner.flintlock.dev/host=true:NoSchedule`, so that only pods that
  tolerate it run on a Host.
- **KF-003** The Fleet Manifests SHALL write the Host configuration file of
  HI-050 through the bootstrap configuration of the MachineDeployment.
- **KF-004** The Fleet Manifests SHALL set no node drain timeout shorter than
  the configured drain timeout on a Host pool's Machines.
- **KF-005** The Fleet Manifests SHALL define a MachineHealthCheck for each
  Host pool that replaces a Machine whose Node stays not ready beyond the
  configured interval.

One instance type per pool keeps capacity arithmetic trivial today and is
what a snapshot compatibility class will need later. The number of Hosts is
the replica count of the MachineDeployment; nothing in this design scales it
automatically, because MicroVMs are not pods and no cluster autoscaler can
see demand for them.

## Host readiness {#host-readiness}

- **KF-010** The Host Agent SHALL report ready only while `flintlockd` on its
  Host answers `ServerInfo` over mutual TLS with the exec service enabled
  and every enabled Host Service accepts connections on the bridge gateway
  address.
- **KF-011** If a unit of the Host Image reports a not ready reason, then the
  Host Agent SHALL report not ready and SHALL expose that reason in its
  status.
- **KF-012** The Fleet Operator SHALL treat a Node as an eligible Host only
  while the Node carries the Host label of HI-060, is ready, is schedulable
  and runs a ready Host Agent.

## Inventory publication {#inventory-publication}

- **KF-020** The Fleet Operator SHALL publish an Inventory of the eligible
  Hosts as a ConfigMap in its own namespace, in the Inventory file format of
  `07-configuration.md`.
- **KF-021** The Fleet Operator SHALL name each Host in the Inventory after
  its Node and SHALL use the Node's internal address as the Host endpoint
  address.
- **KF-022** The Fleet Operator SHALL compute each Host's capacity as the
  Node's allocatable CPU and memory minus the configured Host reserve.
- **KF-023** The Fleet Operator SHALL copy the Node's architecture and its
  `gitlab-runner.flintlock.dev` labels into the Host's Inventory entry, and
  SHALL record the Host Service addresses the Host Agent reports.
- **KF-024** The Fleet Operator SHALL publish the Pool Manager's host list as
  a second object generated from the same set of eligible Hosts, naming each
  Host identically in both.
- **KF-025** When the set of eligible Hosts changes, the Fleet Operator SHALL
  update both objects within the configured publication delay.
- **KF-026** If the Fleet Operator cannot list Nodes, then it SHALL leave
  both published objects unchanged and SHALL report the failure in its
  health status.
- **KF-027** When the mounted Inventory file changes on disk, the Runner
  SHALL reload it as it does on `SIGHUP`.
- **KF-028** When the published host list changes, the Fleet Manifests SHALL
  make the Pool Manager daemon reload it without interrupting existing
  Leases.

Publishing files rather than teaching the Runner to watch Nodes keeps the
Runner ignorant of Kubernetes: the same binary runs against a hand-written
Inventory on a single KVM machine and against a published one in a cluster.
KF-026 matters because an empty Inventory is not a safe failure; it would
make the Scheduler empty every Pool's host list. A mounted ConfigMap takes
up to a minute to reach a pod, which is acceptable for adding Hosts and is
covered for removing them by the drain guard below.

## Certificates {#cluster-certificates}

- **KF-030** The Fleet Manifests SHALL obtain each Host's `flintlockd`
  serving certificate from a cert-manager Issuer that acts as the fleet
  certificate authority, with the Host's Inventory endpoint address as a
  subject alternative name.
- **KF-031** The Host Agent SHALL write the Host certificate, its key and the
  certificate authority certificate to the path `flintlockd` reads them
  from, readable by `flintlockd` only.
- **KF-032** The Fleet Manifests SHALL issue client certificates for the
  Runner, the Pool Manager daemon and the Fleet Operator from the same
  Issuer and SHALL mount each only into the workload it belongs to.
- **KF-033** The Fleet Manifests SHALL NOT place any Host private key in an
  object that a workload other than that Host's Host Agent can read.
- **KF-034** The Fleet Manifests SHALL configure every workload so that it
  presents a renewed certificate without operator action.

These replace the certificate authority the Fleet Controller generates under
SE-023 and the distribution of its output by FL-024 and FL-051. The
cert-manager CSI driver is the expected mechanism for KF-030 and KF-033,
because it generates the key on the node and never stores it in a Secret;
whether it can put a host-network pod's node address in the certificate has
to be confirmed when this is built, and the fallback is a name the clients
verify instead of an address.

## Host Services {#cluster-host-services}

- **KF-040** The Fleet Manifests SHALL run the Host Services of FL-100 in the
  Host Agent, a DaemonSet that tolerates the Host taint and selects the Host
  label.
- **KF-041** The Host Agent SHALL run in the Host's network namespace and
  SHALL bind every Host Service only to the guest bridge gateway address.
- **KF-042** The Host Agent SHALL keep all Host Service storage in the Host
  Service cache directory of HI-024, so that it outlives the pod and a
  reboot of the Host.
- **KF-043** The Fleet Manifests SHALL supply the registry mirror's upstream
  credentials and the Go module proxy's private module credential from
  Kubernetes Secrets mounted only into the Host Agent.
- **KF-044** The Host Agent SHALL configure each Host Service as FL-102 to
  FL-107, FL-114 and FL-115 require.
- **KF-045** Where a Host Service is disabled in the configuration, the Host
  Agent SHALL NOT run it.
- **KF-046** Before reporting ready for the first time on a Host, the Host
  Agent SHALL pull the kernel and root filesystem images of every Profile
  into the flintlock containerd namespace.
- **KF-047** After reporting ready, the Host Agent SHALL pre-warm the Go
  module proxy and the registry mirror with the modules and images listed in
  the configuration.

## Runner and Pool Manager {#cluster-workloads}

- **KF-050** The Fleet Manifests SHALL run the Runner as a Deployment that
  mounts the published Inventory and reads the Profiles from a ConfigMap and
  the GitLab runner token from a Secret.
- **KF-051** The Fleet Manifests SHALL run the Pool Manager daemon as a
  Deployment of one replica that is replaced by recreation, never by a
  rolling update that would run two daemons at once.
- **KF-052** The Fleet Manifests SHALL give the Runner's pod a termination
  grace period no shorter than the configured job timeout, so that a rollout
  does not cut running Jobs short.
- **KF-053** The Fleet Manifests SHALL NOT schedule the Runner, the Pool
  Manager daemon or the Fleet Operator onto Hosts.
- **KF-054** The Fleet Manifests SHALL restrict ingress to the Pool Manager
  daemon to the Runner and the Fleet Operator with a NetworkPolicy.

## Drain {#cluster-drain}

- **KF-060** When a Host's Node becomes unschedulable, the Fleet Operator
  SHALL remove the Host from both published objects, so that the Scheduler
  removes it from every Pool's `flintlock_hosts` and the Pool Manager places
  nothing new there.
- **KF-061** While the Pool Manager reports a leased MicroVM on a Host, the
  Fleet Operator SHALL prevent an eviction-based drain of that Host's Node
  from completing.
- **KF-062** When the Pool Manager reports no leased MicroVM on an
  unschedulable Host, the Fleet Operator SHALL let the drain of its Node
  complete.
- **KF-063** If the configured drain timeout elapses while leased MicroVMs
  remain on an unschedulable Host, then the Fleet Operator SHALL let the
  drain complete and SHALL log each Lease it abandoned.
- **KF-064** While a removed Host still runs a Job, the Runner SHALL keep
  serving that Job as HO-014 requires.
- **KF-065** If the Fleet Operator cannot reach the Pool Manager, then it
  SHALL keep holding every drain it holds until the drain timeout elapses.

Cordoning is the trigger because every way of removing a node starts with
it: a MachineDeployment rollout or scale-down, a MachineHealthCheck
remediation and an operator's `kubectl drain` alike. The expected mechanism
for KF-061 is a guard pod per Host, covered by a PodDisruptionBudget that
allows no disruption, which the Fleet Operator deletes when the Host has no
Leases; a drain cannot finish while the guard is there. This works entirely
inside the workload cluster. Cluster API's pre-drain hook annotation does
the same job more directly, but the Machine objects live in the management
cluster and the Fleet Operator would need credentials for it.

## Verification {#cluster-verification}

- **KF-070** The verification command of FL-070 to FL-073 and FL-109 SHALL
  accept the published Inventory as its input and SHALL run unchanged
  against a cluster fleet.
- **KF-071** The Fleet Manifests SHALL include a Job that runs the
  verification command in the cluster with the Runner's client certificate.

## Least privilege {#cluster-least-privilege}

- **KF-080** The Fleet Operator SHALL operate with a Role limited to reading
  Nodes and pods, writing the two published objects, and creating and
  deleting its guard pods.
- **KF-081** The Runner and the Pool Manager daemon SHALL NOT require any
  Kubernetes API permission.
- **KF-082** The Fleet Manifests SHALL NOT grant any workload an AWS
  permission.
- **KF-083** The Host Agent SHALL hold only the privileges it needs to bind
  to the bridge gateway, to write the Host certificate path and the cache
  directory, and to reach the Host's flintlock containerd socket.

## Test doubles {#cluster-test-doubles}

- **KF-090** The Fleet Operator SHALL be tested against a Kubernetes API
  server test environment and the fake Pool Manager, with no cluster nodes
  and no AWS.
- **KF-091** The harness SHALL include a scenario in which a Host
  is cordoned while it runs a Job, and SHALL assert that the Job finishes,
  that no new MicroVM is placed on the Host and that the drain completes
  afterwards.

## Relation to push provisioning {#relation-to-push-provisioning}

This section is not normative.

| `06-fleet.md` | Cluster fleet |
|---------------|---------------|
| Discovery, FL-001 to FL-006, FL-116 | Node watch, KF-012 |
| KVM check, FL-117 | HI-011 |
| Remote execution, FL-010 to FL-014 | none; nothing is pushed to a Host |
| Host provisioning, FL-020 to FL-027, FL-029, FL-030 | Host Image, HI-001 to HI-024 and HI-040 to HI-045 |
| Pre-pull, FL-028 | KF-046 |
| Host networking, FL-040 to FL-046 | HI-030 to HI-037 |
| Reachability check, FL-047 | KF-010 |
| Pool Manager install, FL-051 to FL-055 | KF-024, KF-028, KF-051 |
| Inventory, FL-060 to FL-064 | KF-020 to KF-026 |
| Verification, FL-070 to FL-073, FL-109 | unchanged, KF-070 |
| Drain, FL-080, FL-084 | KF-060 to KF-065 |
| Teardown, FL-081 to FL-083 | deleting the MachineDeployment and the manifests |
| Launch template mode, FL-090 to FL-092 | the MachineDeployment, KF-001 to KF-005 |
| One-shot operation, FL-118 to FL-124 | applying the manifests, then KF-071 |
| Host services, FL-100 to FL-115 | KF-040 to KF-047, HI-036 |
| Certificate authority, SE-023 | KF-030 to KF-034 |
| IAM policy, SE-040 to SE-042 | KF-080 to KF-082 |

No FL requirement is withdrawn by this document. Once a cluster fleet has
passed the hardware tier, the packages that only push provisioning needs
(`remote`, `scripts`, `provision`, `launchtemplate`, `ca`, `awsclient`) can
be retired together with their requirements, in the change that deletes
them, because a withdrawn requirement cannot keep its citations.
