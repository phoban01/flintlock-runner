# The Guest Image

The Guest Image is what every MicroVM of a Pool boots: a kernel image and a
root filesystem image, both OCI images that a Profile names and that
`flintlockd` pulls. The requirements are in
[docs/requirements/13-guest-image.md](../docs/requirements/13-guest-image.md)
(GI-001 to GI-072).

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
| `guest-rootfs-kind` | `flr/kind` | need a Kubernetes cluster: adds kind and kubectl to `guest-rootfs-docker` | 4 vCPU, 8 GiB; at least 4 GiB |

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
| Linux, at `boot/vmlinux` | 6.1.190, from kernel.org's tarball | The 6.1 long-term series, which Firecracker's CI kernels follow. On amd64 it is an ELF `vmlinux`, on arm64 the uncompressed `Image` that Firecracker loads. |
| Firecracker's CI kernel configuration | `microvm-kernel-ci-<arch>-6.1.config` of Firecracker v1.16.1, the Host Image's Firecracker | virtio-mmio, virtio-vsock, virtio-blk, ext4 and devtmpfs built in, and no loadable modules, so the guest agent is reachable over vsock and systemd boots with no initrd. |
| `kernel/flr.config`, and `flr-amd64.config` or `flr-arm64.config` | | The iptables `raw` table and the `comment`, `mark`, `statistic` and `multiport` matches and `MARK` target, for kube-proxy and Docker Engine. virtio-pci with the block, network and vsock drivers, and on arm64 the PCI host bridge, GICv3 ITS and PL011 console, for Cloud Hypervisor. Pressure stall information on without a boot option, so a Job can read `/proc/pressure`. |

The build stage cross-compiles on the build machine, so no platform is
emulated; it takes a few minutes per architecture. The tarball's sha256 is
from kernel.org's signed `sha256sums.asc`, and the configuration is taken
from the commit of Firecracker's release tag, by sha256. The image holds
the kernel and nothing else. A Profile names the file with
`kernel.filename: boot/vmlinux`.

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
| iptables and ip6tables on the legacy backend | Ubuntu 24.04's | Firecracker's older CI kernels had no `nf_tables`, which the default nft backend needs. Docker Engine's bridge network and NAT work on the legacy backend, on every kernel. |
| Go at `/usr/local/go`, with `go` and `gofmt` in `/usr/local/bin` | 1.27.2, from go.dev, by sha256 | `go build` in a Job. `GOTOOLCHAIN` is `auto` (Go's `go.env`, and `/etc/environment`), so a module that asks for a newer Go fetches it through `GOPROXY`, the Host's Go module proxy. |
| `flr-registry-mirror.service` | | Once the network is up, it reads the default gateway, where the Host Services listen, and writes `/etc/docker/daemon.json` with the registry mirror `http://<gateway>:5000`. Docker Engine starts after it. With no gateway, Docker Engine starts with no mirror. |
| `agent.slice` and `containers.slice` | | The guest agent runs in `agent.slice`, with 128 MiB of memory protected, a CPU weight of 1000 and an OOM score adjustment of -999. containerd, Docker Engine and, through `cgroup-parent` in `daemon.json`, every container run in `containers.slice`, limited to 90 percent of the MicroVM's memory. Containers take the pressure, and the guest agent keeps sending its heartbeats (GI-070, GI-071). |
| block I/O limits on `containers.slice` | | `IOReadIOPSMax`, `IOWriteIOPSMax`, `IOReadBandwidthMax` and `IOWriteBandwidthMax` on `/`, which systemd resolves to the root disk when it starts the slice: 1500 read and 1500 write IOPS, and 60 MB/s each way, below a gp3 EBS volume's baseline. A container cannot keep the MicroVM's block queue full, which starves the guest agent's heartbeats (#133). Stage scripts are in `agent.slice`, unlimited (GI-072). |
| `/etc/default/flr-io` and the `flr-io` generator | | The image has no such file. A derived image for a Host with a faster thin pool adds one with `FLR_IO_READ_IOPS`, `FLR_IO_WRITE_IOPS`, `FLR_IO_READ_BPS` and `FLR_IO_WRITE_BPS`, each a number with an optional K, M, G or T (powers of 1000), or `infinity` for no limit. The generator at `/usr/lib/systemd/system-generators/flr-io` writes the keys it finds into a drop-in of `containers.slice` at boot. |

The unit runs `/usr/libexec/flr-guest/registry-mirror`
(`rootfs-docker/registry-mirror.sh`), which runs each hook in
`/usr/libexec/flr-guest/registry-mirror.d`: `10-docker` here, and `20-kind`
in the kind image. The guest agent waits for neither the unit nor Docker
Engine (GI-033). Docker Engine is socket activated too, so a Job's first
`docker` command waits for it if the guest is still booting.

Docker Engine starts as early as the registry mirror lets it.
`/etc/systemd/system/docker.service` is the package's unit without its
waits for `network-online.target` and `containerd.service`: Docker Engine
waits for containerd's socket itself, and reads its binary from disk while
containerd starts. `/etc/containerd/config.toml` is in containerd's own
configuration version, so containerd does not migrate it at each start.
On the Mac's nested Host this took Docker Engine's start from 109 s to 71 s
after the kernel started; most of what is left is reading the two binaries
from a cold disk.

The build adds the Docker Engine, containerd, runc, buildx and Go versions
to `/usr/share/flr-guest/versions.env`.

The kernel has the iptables `raw` table, and the `comment`, `mark`,
`statistic` and `multiport` matches that kube-proxy uses in a kind
cluster. It has no IPVS and no VXLAN. It has the block cgroup, its
throttling and cgroup writeback, from Firecracker's configuration, which
the block I/O limits need: `io.max` of `containers.slice`, with a
container's buffered writes charged to its cgroup.

Why the block I/O limits: Firecracker serves a MicroVM's block device and
its vsock from one thread, and flintlock leaves the block engine
synchronous with no rate limit. While the guest keeps the block queue
full, that thread serves the queue and nothing else, so the guest agent's
heartbeats wait, and after 15 s flintlock drops the exec stream. On an
EBS gp3 thin pool at its baseline, kind's copy of its node's `/var` did
that every time (#133). With the limits, the containers' I/O stays below
what the pool completes, the queue empties, and the thread returns to the
vsock. The cost: a container that reads or writes the root disk hard runs
at the limit. A Pool on a Host with a faster thin pool raises the limits
in a derived image, through `/etc/default/flr-io`. The kind image's
volumes tmpfs is not limited: it is memory.

### `guest-rootfs-kind` (`rootfs-kind/`)

Everything in `guest-rootfs-docker`, and:

| What | Version | Why |
|---|---|---|
| kind at `/usr/local/bin/kind` | 0.33.0, by sha256 from the release's `.sha256sum` files | A Job makes a Kubernetes cluster in the guest's Docker Engine. |
| kubectl at `/usr/local/bin/kubectl` | 1.37.1, by sha256 from dl.k8s.io | A Job talks to that cluster. |
| `/etc/default/kind-bootstrap` | | A Job sources it. It exports `KIND_CLUSTER_NAME` (`flr`), `KIND_NODE_IMAGE`, `KIND_CONFIG`, `KUBECONFIG_PATH` and `KUBECONFIG`. `KIND_NODE_IMAGE` is `kindest/node:v1.37.0` by the digest that kind 0.33.0's release notes list. |
| `/etc/kind/kind-config.yaml` | | The boot unit's `20-kind` hook writes it. With a gateway, a `containerdConfigPatches` entry points the node's containerd at a hosts directory, and the config mounts `/etc/kind/certs.d` there. Its `docker.io/hosts.toml` names the Host's mirror over plain HTTP. With no gateway, the config has no mirror. |
| the kind node image, in Docker Engine's store at `/var/lib/docker` | `KIND_NODE_IMAGE`, for the image's architecture, fetched by skopeo by digest | `rootfs-kind/docker-store.sh` unpacks it at build time as Docker Engine 29 keeps an image with the overlay2 storage driver, under its tag and its digest. Docker Engine has it at its first start, so a cluster start neither pulls nor loads it. The image ID is `KIND_NODE_IMAGE_ID` in `versions.env`. |
| `/usr/local/bin/flr-kind-cluster` | | Makes the cluster from the loaded node image, or writes the kubeconfig of the cluster that is running, then waits for the node to be Ready. A Job on `flr/kind` runs it, and so does the create hook of `flr/kind-ready`'s Pool. |
| `var-lib-docker-volumes.mount`, a tmpfs at `/var/lib/docker/volumes` | | kind starts its node with `/var` as an anonymous Docker volume, and Docker Engine copies the node image's `/var`, about 400 MB, into it. On the root disk that copy can stall the MicroVM's disk long enough for flintlock to drop the exec stream (#133). The tmpfs takes it into memory instead. It is 25 percent of the MicroVM's memory, mode 0700, owned by root, and mounted before Docker Engine starts, which requires it (GI-069). |

The kind node runs its own containerd 2, which does not use Docker
Engine's mirror and takes mirrors only from a hosts directory, hence the
mount. The kind configuration also has one node only, and puts etcd's data
on the node's `/tmp`, a tmpfs: the cluster lives as long as one Job.

The build adds the kind, kubectl and node image versions to
`/usr/share/flr-guest/versions.env`.

## Checks

Each image has a check stage that the published image depends on, so an
image that fails its checks is never produced (GI-003):

- `kernel/check.sh`: the kernel is at `boot/vmlinux`, of the image's
  architecture, the pinned 6.1 version, and alone in the image. Its
  `.config` has the options of Firecracker's configuration that the guest
  needs, among them the block cgroup options the I/O limits need, no
  loadable modules, pressure stall information on without a boot option,
  and every option of the fragments.
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
  with `dockerd --validate`. The slices: the guest agent's and the
  containers' settings, the block I/O limits of `containers.slice`, which
  `systemd-analyze verify` accepts, and the `flr-io` generator run with a
  stand-in `/etc/default/flr-io`: no file, every key, and malformed
  values, which it reports and skips.
- `rootfs-kind/check.sh`: runs after both of those in the kind image. The
  kind and kubectl versions, `/etc/default/kind-bootstrap`, the node image
  by digest and of kind's Kubernetes minor version, the baked node image's
  tag, ID and architecture, the boot unit's kind configuration with and
  without a default route, and `flr-kind-cluster` with stand-ins for
  docker, kind and kubectl, with and without a running cluster. The
  volumes tmpfs unit: its options, enabled, required by `docker.service`
  and ordered before it, over an empty mount point.
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

The Docker and kind images limit the containers' block I/O on the root
disk to 1500 read and 1500 write IOPS and 60 MB/s each way. A Pool on a
Host whose thin pool completes more, say a gp3 volume with provisioned
IOPS or an NVMe instance store, raises them in a derived image:

```dockerfile
FROM ghcr.io/phoban01/flintlock-runner/guest-rootfs-docker@sha256:<digest>
RUN printf 'FLR_IO_READ_IOPS=6000\nFLR_IO_WRITE_IOPS=6000\nFLR_IO_READ_BPS=250M\nFLR_IO_WRITE_BPS=250M\n' >/etc/default/flr-io
```

`infinity` removes a limit. The image has no cloud-init, so a Profile's
`user_data` cannot set the file.

## Changing a version

- Kernel: change `KERNEL_VERSION` and `KERNEL_SHA256` in
  `kernel/Containerfile`, from kernel.org's signed `sha256sums.asc`. For a
  new Firecracker on the Host Image, change `FIRECRACKER_VERSION`,
  `FIRECRACKER_COMMIT` (the commit of its tag) and the configuration
  files' sha256s.
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
