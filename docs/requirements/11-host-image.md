# Host Image {#host-image}

This document specifies the Host Image: the bootable container image, built
with bootc, from which every Host boots when the fleet is managed through
Kubernetes (`12-cluster-fleet.md`). The image carries everything that
`06-fleet.md` installs on a running instance over Systems Manager or SSH, so
that a Host is complete when it boots and nothing is pushed to it afterwards.
The intent is that the shell templates under `internal/fleet/scripts` are
replaced by one Containerfile and a set of systemd units, which can be built
and checked without EC2 and without KVM.

## Build {#image-build}

- **HI-001** The Host Image SHALL be defined by one Containerfile whose base
  is a bootc base image pinned by digest.
- **HI-002** The Host Image SHALL be built for the `x86_64` architecture.
- **HI-003** The Host Image SHALL contain containerd, Firecracker with its
  jailer, Cloud Hypervisor, `flintlockd`, the kubelet, `kubeadm` and
  cloud-init at the versions pinned in one versions file that the
  Containerfile reads.
- **HI-004** The Host Image build SHALL verify the checksum of every binary
  it downloads against a checksum recorded in the versions file and SHALL
  fail when one does not match.
- **HI-005** The Host Image SHALL record the pinned version of each component
  of HI-003 as an OCI image label and in a versions file on the image's
  `/usr` tree.
- **HI-006** The Host Image SHALL NOT contain any credential, private key or
  token.
- **HI-007** The Host Image build SHALL complete on a machine that has
  neither `/dev/kvm` nor access to AWS.
- **HI-008** The Host Image build SHALL run a check stage inside the built
  container image that fails unless every component of HI-003 reports its
  pinned version and every unit this document requires is enabled.
- **HI-009** Where publishing an AMI is requested, the Host Image build SHALL
  convert the container image to an AMI with `bootc-image-builder` and SHALL
  tag the AMI with the container image digest and the pinned Kubernetes
  version.

The Kubernetes version is part of the image because the kubelet is, so an
AMI is only usable by a MachineDeployment whose `version` matches its tag.
Converting to an AMI is the only step that needs AWS: it imports a snapshot
through an S3 bucket and the `vmimport` service role, both of which the
operator creates once.

## Kernel and KVM {#kernel-and-kvm}

- **HI-010** The Host Image SHALL load the `kvm`, `vhost_vsock`,
  `dm_thin_pool`, `tun` and `bridge` kernel modules at boot.
- **HI-011** If `/dev/kvm` is absent or unusable at boot, then the Host Image
  SHALL NOT start `flintlockd` and SHALL report the Host as not ready with
  the reason that KVM is unavailable.
- **HI-012** The Host Image SHALL NOT disable SELinux globally.
- **HI-013** Where a component cannot run under the base image's SELinux
  policy, the Host Image SHALL ship a policy module that relaxes confinement
  for that component's domain only.
- **HI-065** The Host Image SHALL label the Host environment file and the
  not ready reason directory it writes under `/run/flr` so that the Host
  Agent's containers can read them, and the Host Service cache directory so
  that they can write it, under the base image's SELinux policy and without
  changing the domain of any container or the label of any other path.
- **HI-066** The Host Image SHALL configure the container runtime interface
  of containerd to run every unprivileged container of a Kubernetes pod under
  SELinux confinement, in the container domain the base image's policy
  assigns or the one the pod names, rather than unconfined.
- **HI-072** The Host Image SHALL create `/etc/battery/flintlockd` owned by
  the Exec Agent's user id, and SHALL label that directory and every file in
  it so that the Exec Agent's containers can write them and `flintlockd` can
  read them, under the base image's SELinux policy and without changing the
  domain of any container.

HI-011 is the successor of FL-117: whether an instance can run MicroVMs is
still decided on the host, but by a unit at boot rather than by a remote
command. The SELinux requirements exist because the quick fix, setting the
whole host permissive, removes a layer of isolation between jobs' Firecracker
processes and the Host, which is the layer this project is built on.

HI-066 exists because containerd applies no SELinux label to a pod unless
its container runtime interface is told to, and an unlabelled container runs
unconfined: an enforcing Host would then enforce nothing between its pods,
`buildkitd` running Jobs' image builds among them, and itself. The setting
reaches only the pods the kubelet starts; `flintlockd` drives containerd
through its own API, so the MicroVMs keep the confinement HI-013 gives them.
containerd leaves a privileged container unlabelled by design, which is why
HI-066 speaks of unprivileged containers: nothing in the Host Agent is
privileged, and a privileged pod is unconfined whatever the Host does. A pod
may name a container domain of its own, which KF-139 does for `buildkitd`
alone.

HI-065 exists because, with HI-066, the Host Agent's containers run in the
ordinary container domain, which may read and write only files labelled for
containers: without it the Host Agent cannot read the bridge gateway the
Host Image writes at boot, nor keep its caches, and fails on every enforcing
Host, and the Pod Provider cannot read the not ready reasons of HI-011 and
HI-022, which it treats as not ready, so no Virtual Node would ever become
ready. Labelling those paths for containers is the narrow fix. Running
the Host Agent as a super-privileged container would work too, and would
remove SELinux from between the Host Services and the Host entirely.

HI-072 is HI-065 for the Exec Agent's certificate directory
(battery-operator ADR 0003, consequence 7). The Exec Agent runs in the
ordinary container domain and writes the directory through a hostPath
mount, so the directory and its files carry the container file type that
domain may write, at level `s0`. A file the Exec Agent creates carries the
level of the pod that created it, which a later pod of the DaemonSet, with
categories of its own, could neither read nor replace; so the Host Image
labels the files again whenever they change, which returns them to `s0`.
`flintlockd` runs unconfined, as a systemd service without a policy module
of its own, and reads them whatever their label.

## Storage {#image-storage}

- **HI-020** The Host Image SHALL create the containerd devicemapper thin
  pool at boot on the block device named in the Host configuration file, or,
  when none is named, on the unused instance-store device it detects.
- **HI-021** If the thin pool already exists at boot, then the Host Image
  SHALL NOT recreate it or wipe its device.
- **HI-022** If no device is named and none is detected, then the Host Image
  SHALL NOT start `flintlockd` and SHALL report the Host as not ready with
  the reason that no thin pool device is available.
- **HI-023** The Host Image SHALL NOT select a device that holds a mounted
  filesystem or the root volume, and SHALL treat a device that already backs
  the thin pool as the thin pool device rather than as in use.
- **HI-024** The Host Image SHALL create the Host Service cache directory on
  instance-store storage and SHALL keep all `flintlockd` and containerd
  state under `/var`.

Instance-store devices survive a reboot and do not survive a stop or a
terminate. HI-021 is therefore what makes an in-place upgrade cheap: after
`bootc upgrade` and a reboot the thin pool, the pre-pulled images and the
Host Service caches are all still there, where replacing the Machine starts
cold. HI-023 restates the fix of pull request #43 as a requirement.

## Networking {#image-networking}

- **HI-030** The Host Image SHALL create a Linux bridge at boot with the
  guest subnet from the Host configuration file and SHALL configure
  `flintlockd` to attach TAP interfaces to it.
- **HI-031** The Host Image SHALL run a DHCP and DNS service bound to the
  bridge so that guests obtain an address, gateway and resolver without
  static configuration.
- **HI-032** The Host Image SHALL enable IPv4 forwarding and SHALL configure
  source NAT from the guest subnet to the Host's primary interface.
- **HI-033** The Host Image SHALL drop traffic from the guest subnet to the
  EC2 instance metadata service address.
- **HI-034** The Host Image SHALL drop traffic from the guest subnet to the
  Host's own kubelet, Pod Provider and metrics ports.
- **HI-035** The Host Image SHALL drop traffic from the guest subnet to every
  protected CIDR listed in the Host configuration file.
- **HI-036** The Host Image SHALL allow traffic from the guest subnet to the
  bridge gateway address only on the Host Service ports and the DHCP and DNS
  ports, and SHALL keep every other port on the gateway closed to guests.
- **HI-037** The Host Image SHALL use a default guest subnet that the Host
  configuration file can override, so that it can be kept clear of the
  cluster's node, pod and service ranges.
- **HI-064** The Host Image SHALL drop traffic from the user ids that run the
  Host Services, which it reads from the Host configuration file with
  defaults when none are set, to the instance metadata service address and
  to the Host's own kubelet, Pod Provider, `flintlockd` and metrics ports.
- **HI-075** The Host Image SHALL forward traffic from the guest subnet only
  out of the Host's primary interface, and SHALL drop traffic from the guest
  subnet to every other interface of the Host.
- **HI-076** The Host Image SHALL drop traffic from the guest subnet whose
  destination before any destination NAT on the Host is in a protected CIDR.
- **HI-077** The Host Image SHALL drop traffic from the guest subnet to every
  address of the Host other than the bridge gateway address.

FL-046 dropped guest traffic to the addresses of the other Hosts in the
Inventory, which the Fleet Controller knew because it wrote the Inventory.
A Host that configures itself at boot does not know its peers, so HI-035
takes ranges instead: the operator lists the node, pod and Service CIDRs of
the cluster, and anything else a job has no business reaching.

A MicroVM runs a CI Job's code, so it may reach the outside and the Host
Services, and nothing of the cluster. HI-075 to HI-077 close the ways round
HI-035 that a Host in a cluster has:

- A pod on the same Host is reached through its own interface on the Host,
  not through the primary interface, so HI-075 drops it whatever its
  address. So are the tunnel interfaces of an overlay network.
- kube-proxy translates the address of a Service into the address of one of
  its pods before the Host forwards the packet. HI-076 compares the address
  the guest asked for, so a protected Service range drops a Service's
  traffic even when its pods are outside every protected range. The same
  holds for a NodePort on another Node.
- The Host's own addresses, the primary one included, answer a guest only on
  the gateway (HI-036). HI-077 drops the rest, which also keeps the
  Kubernetes API away from a guest where the Host reaches it on an address
  of its own.

The Host still forwards a guest's traffic to any address outside the
protected ranges, private addresses included, so the protected CIDRs of a
pool name every range of the cluster: its nodes, its pods and its Services.

HI-064 carries SE-030 and SE-031 over to the Host Services. In a cluster
fleet they run in the Host's own network namespace, so the guest-subnet rules
above do not apply to them, and `buildkitd` runs the `RUN` steps of every
Job's image builds as its own user. Those steps must not reach the metadata
service or the Host's control ports any more than the Job's MicroVM may. The
user ids are the ones the Fleet Manifests run the Host Services as. The two
have to agree, and none of them may be the Exec Agent's user id of HI-070.

## flintlockd {#image-flintlockd}

- **HI-040** The Host Image SHALL run `flintlockd`, its containerd and every
  hypervisor process they start as systemd services outside the cgroup of
  any Kubernetes pod.
- **HI-041** The Host Image SHALL start `flintlockd` only after the thin
  pool and the bridge are present.
- **HI-042** (withdrawn) Replaced by HI-067 and HI-068: `flintlockd` no
  longer listens on loopback for a Pod Provider on the same Host, because
  battery reaches it over the network (battery-operator ADR 0002).
- **HI-043** The Host Image SHALL enable the `flintlockd` exec API.
- **HI-044** (withdrawn) Replaced by HI-067 to HI-070: `flintlockd` is now
  reachable from outside the Host, by battery in the Operator's pod, with
  mutual TLS and behind the Host firewall.
- **HI-063** (withdrawn) Replaced by HI-070: the one user id admitted over
  loopback is the Exec Agent's, and the network path is HI-069's.
- **HI-067** The Host Image SHALL configure `flintlockd` to serve its gRPC
  API only with TLS, on port 9090 of the Host's internal address, with the
  serving certificate and key that the Exec Agent writes to
  `/etc/battery/flintlockd`.
- **HI-068** The Host Image SHALL configure `flintlockd` to require a client
  certificate on every connection and to verify it against the `flintlockd`
  client CA bundle that the Exec Agent writes to `/etc/battery/flintlockd`.
- **HI-069** The Host Image SHALL drop every connection to `flintlockd`'s
  port that arrives from outside the Host unless its source address is in
  the Operator's pod network, which it reads from the Host configuration
  file.
- **HI-070** The Host Image SHALL refuse connections to `flintlockd`'s port
  from the Host's own processes unless they belong to the Exec Agent's user
  id, which it reads from the Host configuration file with a default when
  none is set.
- **HI-071** The Host Image SHALL start `flintlockd` only once the serving
  certificate, its key and the client CA bundle exist in
  `/etc/battery/flintlockd`, and SHALL restart `flintlockd` when the serving
  certificate or the client CA bundle changes.
- **HI-073** When the Host Image stops or restarts `flintlockd`, it SHALL
  stop the `flintlockd` process alone and SHALL leave every hypervisor
  process that `flintlockd` started running.

battery creates and deletes MicroVMs by calling every Host's `flintlockd`
from the Operator's pod, so `flintlockd` serves on the Host's internal
address with mutual TLS (battery-operator ADR 0002). The Exec Agent is
battery-operator's since battery-operator#15; it is what obtains the Host's
certificates, through Kubernetes certificate signing requests the Operator
signs (ADR 0003), and it connects to its own Host's `flintlockd` as battery
does, with a client certificate of its own. The Host Image holds no key and
fetches nothing: it reads what the Exec Agent writes, and it follows the
Exec Agent's conventions exactly, from battery-operator's
`config/exec-agent/daemonset.yaml` and `internal/execagent`: the directory
`/etc/battery/flintlockd`; in it `tls.crt`, `tls.key` and `client-ca.crt`,
of which `tls.crt` is always written last; the endpoint
`$(HOST_IP):9090`, the Node's internal address; and user id 65532, the
Exec Agent image's user. The internal address is the one the Node reports,
which is the address of the Host's interface with the default route unless
the kubelet is told otherwise.

`flintlockd` has one listening endpoint and one client CA, and it admits any
certificate that CA signed, with the whole API. The client CA signs only for
battery and the Exec Agents, but a certificate stolen from one Host's Exec
Agent would open every other Host's `flintlockd`. HI-069 and HI-070 narrow
that to the two places a legitimate client connects from: battery, in the
Operator's pod, and the Exec Agent on the Host itself, which reaches the
internal address over loopback. The Operator's pod network is a list of
CIDRs in the Host configuration file and is empty by default, which admits
nothing from outside the Host: it depends on the cluster the Host joins, so
the Fleet Manifests set it. A `flintlockd` that authorized clients by the
identity in their certificate would remove the exposure; that is for
flintlock (flintlock#1242).

The Exec Agent's user id is 65532 by default because that is the user of
the Exec Agent's image, which battery-operator's DaemonSet does not
override. It is also the user of many distroless images, so HI-070 keeps out
every process of the Host except those that run as the Exec Agent's user id,
not every process but the Exec Agent. It still refuses root, the Host
Services and every other pod in the Host's network namespace, and each
connection it lets through must present a certificate from the client CA.

HI-071 exists because `flintlockd` reads its certificate, key and client CA
once, when it starts (`pkg/auth/tls.go:18-48` of flintlock v0.15.2), and
reloads nothing until flintlock#1235. So the Host Image does not start it
before the Exec Agent has written them, and restarts it when the Exec Agent
renews the serving certificate or the client CA bundle changes. Until then
the Exec Agent finds no `flintlockd` answering and reports the Host not
ready, so battery is never given a Host without certificates (ADR 0003,
consequence 5).

HI-073 exists because HI-071 restarts `flintlockd` each time the Exec Agent
renews the serving certificate, and a restart must not end the Jobs on the
Host. battery-operator's trial on a real Host saw a MicroVM keep running
through a restart of `flintlockd` with `KillMode=process`.

A restart leaves the MicroVMs running. In flintlock v0.15.2, the version the
Host Image pins, Firecracker and Cloud Hypervisor are started detached by
default (`pkg/defaults/defaults.go:28` and `:38`), in a session of their own
(`pkg/process/process.go:16-24`, called from
`infrastructure/microvm/firecracker/create.go:97` and
`infrastructure/microvm/cloudhypervisor/create.go:116`), with nothing tying
their lives to `flintlockd`'s. `flintlockd.service` has `KillMode=process`,
so systemd stops `flintlockd` alone and leaves the hypervisor processes in
the unit's cgroup. When `flintlockd` starts again it resyncs every MicroVM
spec (`internal/command/run/run.go:262`,
`infrastructure/controllers/microvm_controller.go:56-63`). A MicroVM whose
hypervisor process named in its pid file is alive is reported as running
(`infrastructure/microvm/firecracker/provider.go:127-166`;
`infrastructure/microvm/cloudhypervisor/provider.go:76-149`), so the plan
neither creates it again (`core/steps/microvm/create.go:45`) nor starts it
(`core/steps/microvm/start.go:52`), and its tap device is left alone because
it exists (`core/steps/network/interface_create.go:61`). Its sockets are
under `/run/flintlock`, which a restart does not touch.

What a restart does cut is every gRPC stream open at the time. systemd stops
`flintlockd` with `SIGTERM`, which `flintlockd` does not handle (it waits
for `os.Interrupt` only, `internal/command/run/run.go:110`), so it exits at
once rather than through `GracefulStop`. A command running through the exec
API when the certificate is renewed therefore loses its stream, and the Exec
Agent reports that as a stream failure, never as success (battery-operator
EA-020). What happens to the command inside the guest then is the guest
agent's business and has not been established. Renewal is rare, at about
three fifths of a certificate's lifetime, but a Stage that is running at
that moment fails.

HI-040 is the one place where this design refuses to be Kubernetes-native.
Firecracker processes are children of `flintlockd`; inside a pod they would
share its cgroup, and a pod restart or an eviction would kill every job on
the Host. Kubernetes manages the node, and systemd manages what runs the
MicroVMs.

## Host configuration {#host-configuration}

- **HI-050** The Host Image SHALL read per-Host settings from one Host
  configuration file that cloud-init writes at first boot, containing the
  guest subnet, the thin pool device, the protected CIDRs and the Host
  reserve.
- **HI-051** If the Host configuration file is absent, then the Host Image
  SHALL boot with its defaults for every setting.
- **HI-052** The Host Image SHALL NOT read any secret from the Host
  configuration file or from instance user-data.

## Kubernetes node {#kubernetes-node}

- **HI-060** The Host Image SHALL register its kubelet with the labels
  `gitlab-runner.flintlock.dev/host` set to `true`,
  `gitlab-runner.flintlock.dev/image` set to a value derived from the image
  digest and one label per hypervisor carrying its pinned version.
- **HI-061** The Host Image SHALL configure the kubelet to reserve all CPU
  and memory beyond the configured Host reserve, so that the Host's Node
  offers only the Host reserve to pods and the Virtual Node of KF-012 offers
  the rest to MicroVMs.
- **HI-062** The Host Image SHALL disable automatic bootc updates, so that
  an operating system update is applied only to a drained Host.
- **HI-074** The Host Image SHALL register its kubelet with the label
  `battery.liquidmetal-x.dev/host` set to `true`.

The version labels are what a later snapshot feature will place against: a
Firecracker snapshot restores only on the same CPU model, Firecracker
version and host kernel, and the image label names the last two. The taint
that keeps ordinary pods off a Host is applied by the kubeadm join
configuration of KF-002 rather than by the image, because a taint is policy
of the cluster the Host joins and the labels are facts about the image.

HI-074 is battery-operator's Host label. Its Exec Agent runs only on Nodes
with that label (battery-operator EA-004), and its Inventory Controller
gives battery only those Nodes. Every Host that boots this image has
`flintlockd`, KVM and the thin pool that the Exec Agent checks for, so the
image sets the label itself, next to its own Host label. The Exec Agent
still reports a Host not ready when a check fails.

In-place upgrade, draining a Host and then running `bootc upgrade` and
rebooting, is the reason to build on bootc rather than on a plain AMI, but
its orchestration is not specified yet. Until it is, an upgrade is a new AMI
and a MachineDeployment rollout.
