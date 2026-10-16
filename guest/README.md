# The Guest Image

The Guest Image is what every MicroVM of a Pool boots: a kernel image and a
root filesystem image, both OCI images that a Profile names and that
`flintlockd` pulls. The requirements are in
[docs/requirements/13-guest-image.md](../docs/requirements/13-guest-image.md)
(GI-001 to GI-064).

The kernel and the first root filesystem image start from
battery-operator's guest images (`guestimage/`, first in its
`hack/real-hosts/images`). The network setup and the empty machine ID come
from its real hosts trial's guest networking (phoban01/battery-operator#163).

## Which guest images

Use these images for every Pool that flintlock-runner claims. There is one
kernel image and three root filesystem images. Each root filesystem image
builds on the one above it, so they share layers:

| Root filesystem image | Profile image name | For Jobs that | Example Profile size |
|---|---|---|---|
| `guest-rootfs` | `flr/ubuntu` | run scripts: bash, git, curl, the CA certificates and `gitlab-runner-helper` | 2 vCPU, 4 GiB |
| `guest-rootfs-docker` | `flr/docker` | build and run containers, and build Go: adds Docker Engine with buildx, and Go | 2 to 4 vCPU, 4 GiB |
| `guest-rootfs-kind` | `flr/kind` | need a Kubernetes cluster: adds kind and kubectl to `guest-rootfs-docker` | 4 vCPU, 8 GiB |

Every Profile names `guest-kernel` as its kernel.
`deploy/runner/config.yaml` has a Profile for each, and
[docs/docker-and-kind.md](../docs/docker-and-kind.md) has example Jobs for
the Docker and kind images.

battery-operator publishes a smaller pair,
`ghcr.io/phoban01/battery-operator/guest-kernel` and `guest-rootfs`
([guestimage/README.md](https://github.com/phoban01/battery-operator/blob/main/guestimage/README.md)),
with only the guest agent. Those suit a Pool that runs no Jobs, such as
battery-operator's demo. A Job on them fails, because they have no
`gitlab-runner-helper` and no git. The kernel is the same in both projects.

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

### `guest-rootfs-docker` (`rootfs-docker/`)

Everything in `guest-rootfs`, and:

| What | Version | Why |
|---|---|---|
| Docker Engine, the docker CLI, containerd, runc, the buildx plugin | 29.9.1, containerd 2.4.2, buildx 0.38.0, from Docker's apt repository for Ubuntu 24.04 | Jobs that build and run containers. apt checks every package against the index that Docker's signing key signs, and the key is pinned by its sha256. runc comes with containerd.io. |
| Docker Engine on the overlay2 storage driver | | Docker Engine 29 would keep images in containerd's image store; `/etc/docker/daemon.json` turns that off. overlay2 is what most Jobs expect. |
| iptables and ip6tables on the legacy backend | Ubuntu 24.04's | The kernel has no `nf_tables`, which the default nft backend needs. Docker Engine's bridge network and NAT work on the legacy backend. |
| Go at `/usr/local/go`, with `go` and `gofmt` in `/usr/local/bin` | 1.27.2, from go.dev, by sha256 | `go build` in a Job. `GOTOOLCHAIN` is `auto` (Go's `go.env`, and `/etc/environment`), so a module that asks for a newer Go fetches it through `GOPROXY`, the Host's Go module proxy. |
| `flr-registry-mirror.service` | | Once the network is up, it reads the default gateway, where the Host Services listen, and writes `/etc/docker/daemon.json` with the registry mirror `http://<gateway>:5000`. Docker Engine starts after it. With no gateway, Docker Engine starts with no mirror. |

The unit runs `/usr/libexec/flr-guest/registry-mirror`
(`rootfs-docker/registry-mirror.sh`), which runs each hook in
`/usr/libexec/flr-guest/registry-mirror.d`: `10-docker` here, and `20-kind`
in the kind image. The guest agent waits for neither the unit nor Docker
Engine (GI-033). Docker Engine is socket activated too, so a Job's first
`docker` command waits for it if the guest is still booting.

The build adds the Docker Engine, containerd, runc, buildx and Go versions
to `/usr/share/flr-guest/versions.env`.

The kernel also has no `xt_comment`, `xt_mark`, `xt_statistic` or
`xt_multiport` iptables modules, no IPVS and no VXLAN. Docker Engine needs
none of them. kube-proxy in a kind cluster uses `comment` and `mark` rules,
so a kind cluster's Services may not work on this kernel.

### `guest-rootfs-kind` (`rootfs-kind/`)

Everything in `guest-rootfs-docker`, and:

| What | Version | Why |
|---|---|---|
| kind at `/usr/local/bin/kind` | 0.33.0, by sha256 from the release's `.sha256sum` files | A Job makes a Kubernetes cluster in the guest's Docker Engine. |
| kubectl at `/usr/local/bin/kubectl` | 1.37.1, by sha256 from dl.k8s.io | A Job talks to that cluster. |
| `/etc/default/kind-bootstrap` | | A Job sources it. It exports `KIND_CLUSTER_NAME` (`flr`), `KIND_NODE_IMAGE`, `KIND_CONFIG`, `KUBECONFIG_PATH` and `KUBECONFIG`. `KIND_NODE_IMAGE` is `kindest/node:v1.37.0` by the digest that kind 0.33.0's release notes list. |
| `/etc/kind/kind-config.yaml` | | The boot unit's `20-kind` hook writes it. With a gateway, a `containerdConfigPatches` entry points the node's containerd at a hosts directory, and the config mounts `/etc/kind/certs.d` there. Its `docker.io/hosts.toml` names the Host's mirror over plain HTTP. With no gateway, the config has no mirror. |

The kind node runs its own containerd 2, which does not use Docker
Engine's mirror and takes mirrors only from a hosts directory, hence the
mount. Docker Engine pulls the node image itself, through its own mirror.

The build adds the kind, kubectl and node image versions to
`/usr/share/flr-guest/versions.env`.

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
- `rootfs-docker/check.sh`: runs after `rootfs/check.sh` in the Docker
  image. The Docker Engine, containerd, runc, buildx and Go versions,
  iptables on the legacy backend, `GOTOOLCHAIN`, the units and their order.
  It runs the boot unit's script with a stand-in `ip` command, once with a
  default route and once without, and checks the `daemon.json` it writes,
  with `dockerd --validate`.
- `rootfs-kind/check.sh`: runs after both of those in the kind image. The
  kind and kubectl versions, `/etc/default/kind-bootstrap`, the node image
  by digest and of kind's Kubernetes minor version, and the boot unit's
  kind configuration with and without a default route.
- `lint.sh`: without building, every download is checked with
  `sha256sum -c` against a recorded sha256, and every base image is pinned
  by digest or is an image of the same build.

What the checks cannot show is a guest that boots and takes a lease, or a
Docker Engine and a kind cluster that run. That needs a Host:
`hack/real-host` (its `cache-demo`, `build-demo` and `kind` steps).

## Building

From the repository root, with Docker and buildx:

```sh
make guest-lint                                         # shellcheck and lint.sh
make guest-images                                       # every image, amd64 and arm64
make guest-images GUEST_PLATFORMS=linux/arm64 GUEST_LOAD=1   # one platform, into Docker
```

`make guest-images` runs `guest/build.sh`. It builds the kernel image and
the three root filesystem images with their check stages and, without
`GUEST_LOAD`, keeps them in the builder's cache. The Docker image builds
FROM the root filesystem image of the same build, and the kind image FROM
the Docker image: `build.sh` writes each lower image as an OCI layout in a
temporary directory and passes it to the next build as a named build
context. That needs a docker-container builder, such as the one
`hack/release/setup-buildx.sh` makes; set `BUILDX_BUILDER` to use another.
A platform other than the machine's needs QEMU emulation, which
`hack/release/setup-buildx.sh` installs. With `GUEST_LOAD=1` the images are
`localhost/flr-guest-kernel:dev`, `localhost/flr-guest-rootfs:dev`,
`localhost/flr-guest-rootfs-docker:dev` and
`localhost/flr-guest-rootfs-kind:dev`.

CI's `guest` job builds every image for both platforms on every pull
request that changes `guest/`, `go.mod` or `go.sum`, and pushes nothing.
The release workflow's `guest images` job builds them the same way on a tag
and pushes `guest-kernel`, `guest-rootfs`, `guest-rootfs-docker` and
`guest-rootfs-kind` under `ghcr.io/phoban01/flintlock-runner`, tagged with
the version, each one index for both platforms (GI-040, GI-041). The
release lists their digests in `images.txt` (docs/RELEASING.md).

## Using them

A Profile names the images by digest (CF-027):

```yaml
kernel:
  image: ghcr.io/phoban01/flintlock-runner/guest-kernel@sha256:<digest>
  filename: boot/vmlinux
rootfs: ghcr.io/phoban01/flintlock-runner/guest-rootfs-docker@sha256:<digest>
```

The shell, the helper path and the builds and cache directories keep their
defaults. `deploy/runner/config.yaml` has an amd64 Profile for each root
filesystem image and an arm64 Profile for a Lima Host, with placeholders
that a release fills in. The registry's packages have to be public, or
readable by the Hosts, for `flintlockd` to pull them.

The Docker and kind images expect the Host's registry mirror on port 5000
of the guest's gateway, the default of `host_services.registry_mirror.port`.
A Host whose mirror is on another port needs a derived image with
`REGISTRY_MIRROR_PORT=<port>` in `/etc/default/flr-registry-mirror`.

## Changing a version

- Kernel: change `KERNEL_VERSION` or `FIRECRACKER_CI_VERSION` in
  `kernel/Containerfile`, download both architectures' files, and pin their
  sha256s there.
- Guest agent: change `GUEST_AGENT_VERSION` in `rootfs/Containerfile`, and
  take both sha256s from the release's `checksums.txt`.
- Helper: follows `go.mod`; nothing to change here.
- Docker: change `DOCKER_VERSION` and the `*_APT_VERSION` arguments in
  `rootfs-docker/Containerfile` to versions from Docker's `Packages` index
  for noble. A new signing key needs a new `DOCKER_APT_KEY_SHA256`.
- Go: change `GO_VERSION` and both sha256s in `rootfs-docker/Containerfile`,
  from `https://go.dev/dl/?mode=json`.
- kind: change `KIND_VERSION` and its sha256s in `rootfs-kind/Containerfile`,
  and `KIND_NODE_IMAGE` to a node image, by digest, that the release notes
  list. Move `KUBECTL_VERSION` to the same Kubernetes minor version.
- Base images: resolve the new digest with
  `docker buildx imagetools inspect IMAGE:TAG`.
