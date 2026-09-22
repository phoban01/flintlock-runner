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
- **HI-065** The Host Image SHALL label the Host environment file it writes
  under `/run/flr` so that the Host Agent's containers can read it, and the
  Host Service cache directory so that they can write it, under the base
  image's SELinux policy and without changing the domain of any container
  or the label of any other path.

HI-011 is the successor of FL-117: whether an instance can run MicroVMs is
still decided on the host, but by a unit at boot rather than by a remote
command. The SELinux requirements exist because the quick fix, setting the
whole host permissive, removes a layer of isolation between jobs' Firecracker
processes and the Host, which is the layer this project is built on.

HI-065 exists because the Host Agent's containers run in the ordinary
container domain, which may read and write only files labelled for
containers: without it the Host Agent cannot read the bridge gateway the
Host Image writes at boot, nor keep its caches, and fails on every enforcing
Host. Labelling those two paths for containers is the narrow fix. Running
the Host Agent as a super-privileged container would work too, and would
remove SELinux from between the Host Services and the Host entirely.

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

FL-046 dropped guest traffic to the addresses of the other Hosts in the
Inventory, which the Fleet Controller knew because it wrote the Inventory.
A Host that configures itself at boot does not know its peers, so HI-035
takes ranges instead: the operator lists the node and pod CIDRs of the
cluster, and anything else a job has no business reaching.

HI-064 carries SE-030 and SE-031 over to the Host Services. In a cluster
fleet they run in the Host's own network namespace, so the guest-subnet rules
above do not apply to them, and `buildkitd` runs the `RUN` steps of every
Job's image builds as its own user. Those steps must not reach the metadata
service or the Host's control ports any more than the Job's MicroVM may. The
user ids are the ones the Fleet Manifests run the Host Services as, and the
two have to agree, as the Pod Provider's user id of HI-063 does.

## flintlockd {#image-flintlockd}

- **HI-040** The Host Image SHALL run `flintlockd`, its containerd and every
  hypervisor process they start as systemd services outside the cgroup of
  any Kubernetes pod.
- **HI-041** The Host Image SHALL start `flintlockd` only after the thin
  pool and the bridge are present.
- **HI-042** The Host Image SHALL configure `flintlockd` to listen only on a
  local endpoint, a unix socket or a loopback address, that the Pod Provider
  of `12-cluster-fleet.md` can reach and a guest cannot.
- **HI-043** The Host Image SHALL enable the `flintlockd` exec API.
- **HI-044** The Host Image SHALL NOT expose `flintlockd` on any address
  reachable from outside the Host.
- **HI-063** The Host Image SHALL admit connections to the local
  `flintlockd` endpoint only from the Pod Provider's user id, which it reads
  from the Host configuration file with a default when none is set, and
  SHALL refuse them from every other process on the Host.

Because its only client is the Pod Provider on the same Host, `flintlockd`
needs no certificate and no token here, and the fleet certificate authority
of SE-023 has nothing left to sign. The authenticated network surface of a
Host is the Pod Provider's kubelet endpoint (KF-031) instead. The pinned
`flintlockd` listens on TCP only, so the endpoint is a loopback port, and a
loopback port is open to every process in the Host's network namespace;
HI-063 narrows it to one user id with a firewall rule on the socket's owner.
A unix socket with file permissions would be the better boundary, and is
the one to move to when `flintlockd` can listen on one.

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

The version labels are what a later snapshot feature will place against: a
Firecracker snapshot restores only on the same CPU model, Firecracker
version and host kernel, and the image label names the last two. The taint
that keeps ordinary pods off a Host is applied by the kubeadm join
configuration of KF-002 rather than by the image, because a taint is policy
of the cluster the Host joins and the labels are facts about the image.

In-place upgrade, draining a Host and then running `bootc upgrade` and
rebooting, is the reason to build on bootc rather than on a plain AMI, but
its orchestration is not specified yet. Until it is, an upgrade is a new AMI
and a MachineDeployment rollout.
