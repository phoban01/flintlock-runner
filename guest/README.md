# The Guest Image

The Guest Image is what every MicroVM of a Pool boots: a kernel image and a
root filesystem image, both OCI images that a Profile names and that
`flintlockd` pulls. The requirements are in
[docs/requirements/13-guest-image.md](../docs/requirements/13-guest-image.md)
(GI-001 to GI-040).

Both images start from battery-operator's real hosts trial images
(`hack/real-hosts/images` in battery-operator v0.1.0). The network setup
and the empty machine ID come from that trial's guest networking
(phoban01/battery-operator#163).

## What each image carries, and why

### `guest-kernel` (`kernel/`)

| What | Version | Why |
|---|---|---|
| Firecracker's CI kernel, at `boot/vmlinux` | 6.1.128, from Firecracker's `firecracker-ci/v1.12` bucket | It has virtio-vsock, virtio-blk, ext4 and devtmpfs built in, so the guest agent is reachable over vsock and systemd boots with no initrd. On amd64 it is an ELF `vmlinux`, on arm64 the uncompressed `Image` that Firecracker loads. |

Firecracker's bucket publishes no checksum, so the sha256 of each download
is pinned in `kernel/Containerfile`. The image holds the kernel and nothing
else. A Profile names the file with `kernel.filename: boot/vmlinux`.

### `guest-rootfs` (`rootfs/`)

| What | Version | Why |
|---|---|---|
| Ubuntu with systemd as init | 24.04, base image pinned by digest | The same base as the trial's guest. `flintlockd` unpacks the image onto an ext4 block device, and the kernel boots it with `root=/dev/vda`. |
| flintlock's guest agent, enabled at boot | 0.4.0, `.deb` checked against the release's `checksums.txt` | The Executor runs every Stage through it, over vsock (the Guest Transport). |
| bash at `/bin/bash` | Ubuntu 24.04's | The Profile's default shell: every generated Stage script is bash (EX-020). |
| git, curl and the CA certificates | Ubuntu 24.04's | The scripts clone the repository over HTTPS, and Jobs fetch over HTTPS. |
| `gitlab-runner-helper` at `/usr/local/bin/gitlab-runner-helper` | the gitlab-runner version `go.mod` pins | The Profile's default helper path. The artifact and cache scripts call it (EX-018). It is built from the same module version as `flr`, so the two agree on its commands. |
| DHCP on every Ethernet interface except `eth0`, with systemd-networkd and systemd-resolved | Ubuntu 24.04's | A Job that clones a repository or uses the Host Services needs a network. `eth0` is flintlock's interface for Firecracker's metadata service; the Pool's interface is `eth1`. |
| a 10 second limit on the wait for the network | | A guest with no DHCP answer still boots quickly. The guest agent does not wait for the network at all, so the Executor reaches the guest either way. |
| an empty `/etc/machine-id` | | Each MicroVM makes its own at boot. udev derives each interface's MAC address from it, so one ID for every guest would give every guest the same DHCP lease. |
| a root console on the serial port | | For debugging from the Host, which alone can reach it. |

The build records the guest agent and helper versions in
`/usr/share/flr-guest/versions.env`, and as OCI labels.

`gitlab-runner-helper` is built in the `helper` stage of
`rootfs/Containerfile` with `rootfs/build-helper.sh`, from this repository's
`go.mod` and `go.sum`. `go.sum` has no hashes for the few modules only the
helper imports, because `go mod tidy` keeps only those the Runner needs. The
build adds them with `-mod=mod`, and the go command checks each against the
checksum database. The build fails when `go.mod` and the version on the
image's label disagree.

The Ubuntu packages come from Ubuntu's signed archive, at the versions it
has on the day of the build.

The image has no cloud-init. The Pool's MicroVMs do not need it: the
network is DHCP, and the Executor reaches the guest through the guest
agent. So a Profile's `user_data` does nothing in this image.

## Checks

Each image has a check stage that the published image depends on, so an
image that fails its checks is never produced (GI-003):

- `kernel/check.sh`: the kernel is at `boot/vmlinux`, of the image's
  architecture, a 6.1 kernel, and alone in the image.
- `rootfs/check.sh`: runs inside the built root filesystem, under QEMU for
  the platform that is not the build machine's. It checks everything in
  the table above: the tools run, the helper reports the pinned version and
  architecture and has the artifact and cache commands, and the network
  files and units are as described.
- `lint.sh`: without building, every download is checked with
  `sha256sum -c` against a recorded sha256, and every base image is pinned
  by digest.

What the checks cannot show is a guest that boots and takes a lease. That
needs a Host: battery-operator's real hosts trial (`smoke.sh network`), and
the first run of a Job on a real Host (#79).

## Building

From the repository root, with Docker and buildx:

```sh
make guest-lint                                         # shellcheck and lint.sh
make guest-images                                       # both images, amd64 and arm64
make guest-images GUEST_PLATFORMS=linux/arm64 GUEST_LOAD=1   # one platform, into Docker
```

`make guest-images` runs `guest/build.sh`. It builds both images with their
check stages and, without `GUEST_LOAD`, keeps them in the builder's cache.
A platform other than the machine's needs QEMU emulation, for example
`hack/release/setup-buildx.sh`. With `GUEST_LOAD=1` the images are
`localhost/flr-guest-kernel:dev` and `localhost/flr-guest-rootfs:dev`.

CI's `guest` job builds both images for both platforms on every pull request
that changes `guest/`, `go.mod` or `go.sum`, and pushes nothing. The release
workflow's `guest images` job builds them the same way on a tag and pushes
`ghcr.io/phoban01/flintlock-runner/guest-kernel:<version>` and
`ghcr.io/phoban01/flintlock-runner/guest-rootfs:<version>`, each one index
for both platforms. The release lists their digests in `images.txt`
(docs/RELEASING.md).

## Using them

A Profile names the images by digest (CF-027):

```yaml
kernel:
  image: ghcr.io/phoban01/flintlock-runner/guest-kernel@sha256:<digest>
  filename: boot/vmlinux
rootfs: ghcr.io/phoban01/flintlock-runner/guest-rootfs@sha256:<digest>
```

The shell, the helper path and the builds and cache directories keep their
defaults. `deploy/runner/config.yaml` has an amd64 Profile and an arm64
Profile for a Lima Host, with placeholders until a release publishes the
images. The registry's packages have to be public, or readable by the
Hosts, for `flintlockd` to pull them.

## Changing a version

- Kernel: change `KERNEL_VERSION` or `FIRECRACKER_CI_VERSION` in
  `kernel/Containerfile`, download both architectures' files, and pin their
  sha256s there.
- Guest agent: change `GUEST_AGENT_VERSION` in `rootfs/Containerfile`, and
  take both sha256s from the release's `checksums.txt`.
- Helper: follows `go.mod`; nothing to change here.
- Base images: resolve the new digest with
  `docker buildx imagetools inspect IMAGE:TAG`.
