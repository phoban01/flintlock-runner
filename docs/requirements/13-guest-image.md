# Guest Image {#guest-image}

This document specifies the Guest Image: the kernel image and the root
filesystem image that a Profile names, from which every MicroVM of a Pool
boots. The Executor runs each Stage in the guest as a bash script, through
the Guest Transport and flintlock's guest agent, and the artifact and cache
scripts call `gitlab-runner-helper` (`02-executor.md`, EX-018). The Guest
Image carries what those scripts need, and a network for a Job that clones a
repository or uses the Host Services.

Two more root filesystem images build on that one, in layers: the Docker
image adds Docker Engine and Go, and the kind image adds kind and kubectl
to the Docker image. A Profile names one root filesystem image and the
kernel image.

The sources are under `guest/` in this repository. They start from the
images of battery-operator's real hosts trial (`hack/real-hosts/images`
there), which have the kernel, systemd and the guest agent and nothing else.

## Build {#guest-build}

- **GI-001** The Guest Image SHALL consist of a kernel image and a root
  filesystem image, each built for `linux/amd64` and `linux/arm64`.
- **GI-002** The Guest Image build SHALL verify the checksum of every file
  it downloads against a checksum recorded in the Guest Image's sources and
  SHALL fail when one does not match.
- **GI-003** The Guest Image build SHALL check the contents of each image it
  builds in a check stage that fails unless the image carries everything
  this document requires of it.
- **GI-004** The Guest Image build SHALL complete on a machine that has
  neither `/dev/kvm` nor a Host.

The base images of the build are pinned by digest, and the downloads by
checksum, so two builds of one commit take the same inputs. The Ubuntu
packages of the root filesystem come from Ubuntu's signed archive at the
version the archive has on the day of the build, as the Host Image's
packages do.

## Kernel {#guest-kernel}

- **GI-010** (withdrawn) Replaced by GI-011: the kernel is built from
  source with Firecracker's CI configuration, no longer Firecracker's CI
  kernel itself.
- **GI-011** The Guest Image's kernel image SHALL contain, at `boot/vmlinux`, a
  Linux kernel of the 6.1 series built from a kernel.org source tarball with
  the Firecracker CI kernel configuration of the Firecracker release that
  the Host Image runs, as an ELF `vmlinux` for `linux/amd64` and as an
  uncompressed arm64 `Image` for `linux/arm64`.
- **GI-012** The Guest Image's kernel SHALL have the iptables `raw` table, the
  iptables `comment`, `mark` and `statistic` matches and the `MARK` target
  built in.
- **GI-013** The Guest Image's kernel SHALL have virtio-pci built in beside
  virtio-mmio, with the virtio block, network and vsock drivers.
- **GI-014** The Guest Image's kernel SHALL have pressure stall information
  built in and turned on without a boot option, so that a guest has
  `/proc/pressure/cpu`, `/proc/pressure/memory` and `/proc/pressure/io`.

Firecracker's CI configuration has virtio-vsock, virtio-blk, ext4 and
devtmpfs built in, and no loadable modules. The guest agent is reachable
over vsock and systemd boots without an initrd, so a Profile names no
initrd. A Profile names the kernel file with `kernel.filename:
boot/vmlinux`.

kube-proxy, in a kind cluster's node, writes rules with the `comment`,
`mark` and `statistic` matches, and Docker Engine 28 and later put rules in
the `raw` table. Firecracker's devices are virtio-mmio; Cloud Hypervisor's
are virtio-pci, so one kernel serves a Pool of either hypervisor.

Firecracker's CI configuration builds pressure stall information in, but
turns it off unless the kernel boots with `psi=1`. A Job reads
`/proc/pressure` to tell whether CPU, memory or I/O slowed it, and the same
figures help size a Profile. The build turns it on by default, so no
Profile has to add a boot option.

## Root filesystem {#guest-rootfs}

- **GI-020** The Guest Image's root filesystem SHALL be Ubuntu 24.04, with
  systemd as its init.
- **GI-021** The Guest Image SHALL contain flintlock's guest agent at the
  version pinned in its sources and SHALL start it at boot.
- **GI-022** The Guest Image SHALL contain bash at `/bin/bash`, git, curl
  and the certificate authority certificates of the Ubuntu archive.
- **GI-023** The Guest Image SHALL contain `gitlab-runner-helper` at
  `/usr/local/bin/gitlab-runner-helper`, built for the image's architecture
  from the gitlab-runner module at the version that `go.mod` pins.
- **GI-024** The Guest Image SHALL record the versions of the guest agent
  and of `gitlab-runner-helper` it carries in a versions file on the root
  filesystem's `/usr` tree.

`/bin/bash` is the Profile's default shell (EX-020), and
`/usr/local/bin/gitlab-runner-helper` its default helper path (EX-018). The
generated scripts clone the repository with git, and the helper uploads and
downloads artifacts and caches over HTTPS. The helper is built from the same
module version as the Runner, so the two agree on the helper's commands and
their arguments.

## Network {#guest-network}

- **GI-030** The Guest Image SHALL configure every Ethernet interface
  except `eth0` by DHCP for IPv4, with the default route and the DNS servers
  of the lease.
- **GI-031** The Guest Image SHALL leave `eth0` unconfigured.
- **GI-032** If no interface has an IPv4 address 10 seconds after the guest
  starts to wait for the network, then the Guest Image SHALL end the wait
  and continue to boot.
- **GI-033** The Guest Image SHALL NOT start the guest agent after the wait
  for the network.
- **GI-034** The Guest Image SHALL contain an empty machine ID, so that each
  MicroVM makes its own at boot.

flintlock gives a MicroVM `eth0` for Firecracker's metadata service, and the
Pool's interfaces after it, from `eth1`. The Host answers DHCP on the bridge
of those interfaces, `virbr-battery` on battery-operator's Host Image, with
its `battery-dnsmasq` unit (its HI-031 and HI-083). A guest with no
answer still boots, and the Executor still reaches it through the guest
agent over vsock; only a Job that needs the network fails. systemd derives
an interface's MAC address from the machine ID, so with one machine ID for
every guest, every guest would ask DHCP for the same address.

## Docker image {#guest-docker}

- **GI-050** The Guest Image SHALL include a Docker image, a root filesystem
  image built on the Guest Image's root filesystem image for `linux/amd64`
  and `linux/arm64`, which carries everything this document requires of
  that image.
- **GI-051** The Guest Image's Docker image SHALL contain Docker Engine,
  containerd, runc and the Docker buildx plugin, each at the version pinned
  in its sources, and SHALL start Docker Engine at boot.
- **GI-052** The Guest Image's Docker image SHALL set iptables and ip6tables to
  their legacy backend.
- **GI-053** The Guest Image's Docker image SHALL contain Go at the version
  pinned in its sources, on the `PATH`, with `GOTOOLCHAIN` set to `auto`.
- **GI-054** The Guest Image's Docker image SHALL start Docker Engine only after
  the boot unit that writes its configuration has finished.
- **GI-055** When the guest has a default route once the wait for the network
  has ended, the Guest Image's Docker image SHALL configure Docker Engine,
  before it starts, with a registry mirror over plain HTTP at port 5000 of
  the default route's gateway.
- **GI-056** If the guest has no default route once the wait for the network has
  ended, then the Guest Image's Docker image SHALL start Docker Engine with
  no registry mirror.
- **GI-057** The Guest Image's Docker image SHALL configure Docker Engine with
  the overlay2 storage driver.
- **GI-058** (withdrawn) Replaced by GI-012: the kernel has the iptables
  `raw` table, and Docker Engine writes its rules there. The Docker image
  needs the kernel of the same release.

A Profile that names the Docker image serves Jobs that build and run
containers: `docker build`, `docker buildx build` against the Host's
`buildkitd` through `BUILDKIT_HOST`, `docker run`, and `go build`. The Host
Services listen on the guest bridge's gateway, which is the guest's default
gateway, so the boot unit finds the registry mirror there. The mirror is
the Host's pull-through cache of Docker Hub, so an image a Job pulled once
on a Host comes from the Host the next time. A guest with no lease still
boots, and Docker Engine pulls from Docker Hub directly.

iptables uses its legacy backend: Firecracker's CI kernel of earlier
releases had no `nf_tables`, and the legacy backend works on every kernel.
The kernel has the bridge, veth, overlayfs, NAT, conntrack and the
`addrtype` and `conntrack` matches, which Docker Engine needs. Docker
Engine 28 and later put the rules that keep other hosts off a container's
address in the iptables `raw` table, which the kernel has (GI-012). On a
kernel without one, every `docker run` with a network fails, so the Docker
image needs the kernel of the same release. The images of one release ship
together, so a Profile mixes releases only by mistake.
Docker Engine 29 would keep images in containerd's image store; overlay2
keeps them where its registry mirror applies.

With `GOTOOLCHAIN` set to `auto`, a module whose `go.mod` asks for a newer
Go fetches that toolchain through `GOPROXY`, the Host's Go module proxy.

## kind image {#guest-kind}

- **GI-060** The Guest Image SHALL include a kind image, a root filesystem image
  built on its Docker image for `linux/amd64` and `linux/arm64`, which
  carries everything this document requires of the Docker image.
- **GI-061** The Guest Image's kind image SHALL contain kind and kubectl, each
  at the version pinned in its sources.
- **GI-062** The Guest Image's kind image SHALL contain
  `/etc/default/kind-bootstrap`, which exports `KIND_CLUSTER_NAME`,
  `KIND_NODE_IMAGE`, `KUBECONFIG_PATH` and `KUBECONFIG`, with
  `KIND_NODE_IMAGE` naming by digest a kindest/node image that the pinned
  kind release lists.
- **GI-063** When the guest has a default route once the wait for the network
  has ended, the Guest Image's kind image SHALL write
  `/etc/kind/kind-config.yaml` with a containerd mirror of `docker.io` over
  plain HTTP at port 5000 of the default route's gateway.
- **GI-064** If the guest has no default route once the wait for the network has
  ended, then the Guest Image's kind image SHALL write
  `/etc/kind/kind-config.yaml` with no registry mirror.

A Job sources `/etc/default/kind-bootstrap` and runs `flr-kind-cluster`,
which passes its values and `/etc/kind/kind-config.yaml` to `kind create
cluster`. The kind node runs its own containerd, which does not use Docker
Engine's mirror, so the kind configuration gives it the same mirror.

## A fast kind cluster {#guest-kind-fast}

- **GI-065** The Guest Image's kind image SHALL contain the node image that
  `KIND_NODE_IMAGE` names, for the image's architecture, in Docker
  Engine's image store under that reference, so that a kind cluster
  starts without a pull or a load of the node image.
- **GI-066** The Guest Image's kind image SHALL write `/etc/kind/kind-config.yaml`
  with one control-plane node and no other, and with etcd's data
  directory on the node's `/tmp`, which kind mounts as a tmpfs.
- **GI-067** The Guest Image's kind image SHALL contain `flr-kind-cluster`, a
  command that makes the kind cluster of `/etc/default/kind-bootstrap` when
  there is none, writes the existing cluster's kubeconfig to
  `KUBECONFIG_PATH` when there is one, and ends once every node of the
  cluster is Ready.
- **GI-068** The Fleet Manifests SHALL include a Profile for the Job Image
  `flr/kind-ready` that boots the kind image and whose Pool runs
  `flr-kind-cluster` as its create hook.
- **GI-069** The Guest Image's kind image SHALL mount a tmpfs of 25 percent
  of the MicroVM's memory, with mode 0700 and owned by root, at
  `/var/lib/docker/volumes` before Docker Engine starts, so that the `/var`
  volume of a kind node is in memory.

A Pool of the `flr/kind-ready` Profile makes the cluster in each MicroVM
before battery marks the MicroVM available, so a Job claims a MicroVM with
a running cluster. The Job runs `flr-kind-cluster`, as a Job on `flr/kind`
does; there it only writes the kubeconfig. battery waits 30 seconds for a
new MicroVM's guest agent, and gives a create hook no time limit of its
own.

The build unpacks the node image into `/var/lib/docker` as Docker Engine
keeps it, under its tag and its digest. A MicroVM's root disk is a
copy-on-write snapshot of the root filesystem image, so each MicroVM gets
the unpacked image without writing it again. A `docker load` or a pull at
boot writes the whole image to the guest's disk, which on a slow Host takes
minutes.

kind starts its node with `/var` as an anonymous Docker volume, and Docker
Engine first copies the node image's `/var`, about 400 MB, into that
volume. On the root disk that copy took minutes on a slow Host, and it can
stall the MicroVM's block device long enough for flintlock to drop the
exec stream (#133, #136). On a tmpfs it is a copy in memory. The tmpfs
size is a ceiling: its pages count against `containers.slice` only once
they are written. At 25 percent of a 4 GiB MicroVM it holds the node's
`/var` and the images a small Job pulls into the cluster. The example
kind Profiles have 8 GiB.

## Guest agent protection {#guest-agent-protection}

- **GI-070** The Guest Image's Docker image SHALL run the guest agent with an OOM
  score adjustment of -999, in a slice of its own with at least 128 MiB of
  memory protected and a CPU weight of 1000.
- **GI-071** The Guest Image's Docker image SHALL run containerd, Docker Engine
  and every container in a slice whose memory limit is 90 percent of the
  MicroVM's memory and whose CPU weight is the default of 100.
- **GI-072** The Guest Image's Docker image SHALL limit the block I/O of the
  containers' slice on the root filesystem's block device to 1500 read and
  1500 write operations per second and to 60 MB/s of reads and 60 MB/s of
  writes, or to the limits that `/etc/default/flr-io` sets in their place.

flintlock drops an exec stream that sends nothing for 15 seconds, and the
guest agent sends a heartbeat every 5 seconds. A guest agent that
containers starve of CPU or memory misses its heartbeats, and the Job fails
as a system failure. The guest agent starts every Stage script, which
inherits its slice and its OOM score adjustment, so a Stage's own processes
keep memory and CPU too, and the containers take the pressure. At -999, not
-1000, the kernel can still kill a Stage's process when nothing else is
left.

Block I/O starves the guest agent another way. Firecracker serves a
MicroVM's block device and its vsock from one thread, and flintlock leaves
the block engine synchronous with no rate limit. While the guest keeps the
block queue full, that thread serves the queue and nothing else, and the
guest agent's heartbeats wait behind it (#133). The limit keeps the
containers' I/O below what a gp3 EBS volume completes at its baseline of
3,000 IOPS and 125 MB/s, so the queue empties and the thread returns to the
vsock. The limit is `io.max` of the slice's cgroup, which the kernel's
block cgroup throttling enforces; cgroup writeback charges a container's
buffered writes to its cgroup. Stage scripts run in the guest agent's slice
and are not limited. The limit also slows a container that reads the root
disk, such as the kind node's copy of its `/var`. `/etc/default/flr-io`
sets `FLR_IO_READ_IOPS`, `FLR_IO_WRITE_IOPS`, `FLR_IO_READ_BPS` and
`FLR_IO_WRITE_BPS`, with `infinity` for no limit; a generator writes them
into a drop-in of the slice at boot. The image has no such file, so a Pool
on a Host with a faster thin pool raises the limits in a derived image.

## Release {#guest-release}

- **GI-040** The Release SHALL publish the Guest Image's kernel image and
  root filesystem image to the project's container registry, each as one
  image index for `linux/amd64` and `linux/arm64`, tagged with the release
  version.
- **GI-041** The Release SHALL publish the Guest Image's Docker image and
  kind image to the project's container registry, each as one image index
  for `linux/amd64` and `linux/arm64`, tagged with the release version.

The release lists the digest of each image in `images.txt`. A Profile names
the images by that digest (CF-027).
