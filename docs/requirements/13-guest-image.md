# Guest Image {#guest-image}

This document specifies the Guest Image: the kernel image and the root
filesystem image that a Profile names, from which every MicroVM of a Pool
boots. The Executor runs each Stage in the guest as a bash script, through
the Guest Transport and flintlock's guest agent, and the artifact and cache
scripts call `gitlab-runner-helper` (`02-executor.md`, EX-018). The Guest
Image carries what those scripts need, and a network for a Job that clones a
repository or uses the Host Services.

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

- **GI-010** The Guest Image's kernel image SHALL contain Firecracker's CI
  kernel of the 6.1 series for its architecture at `boot/vmlinux`, as an
  ELF `vmlinux` for `linux/amd64` and as an uncompressed arm64 `Image` for
  `linux/arm64`.

Firecracker's CI kernel has virtio-vsock, virtio-blk, ext4 and devtmpfs
built in. The guest agent is reachable over vsock and systemd boots without
an initrd, so a Profile names no initrd. A Profile names the kernel file
with `kernel.filename: boot/vmlinux`.

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
of those interfaces: battery-operator's trial Host with dnsmasq on `flbr0`
(battery-operator#162), and the Host Image later (#73). A guest with no
answer still boots, and the Executor still reaches it through the guest
agent over vsock; only a Job that needs the network fails. systemd derives
an interface's MAC address from the machine ID, so with one machine ID for
every guest, every guest would ask DHCP for the same address.

## Release {#guest-release}

- **GI-040** The Release SHALL publish the Guest Image's kernel image and
  root filesystem image to the project's container registry, each as one
  image index for `linux/amd64` and `linux/arm64`, tagged with the release
  version.

The release lists the digest of each image in `images.txt`. A Profile names
the images by that digest (CF-027).
