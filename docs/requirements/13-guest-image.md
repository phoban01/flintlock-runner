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

## kind and Cluster API {#guest-kind-capi}

- **GI-080** The Guest Image's kind image SHALL write `/etc/kind/kind-config.yaml`
  with the guest's Docker socket, `/var/run/docker.sock`, mounted at the
  same path in the control-plane node, with a registry mirror and
  without one.

A Cluster API provider's e2e suite can use the kind cluster, `flr`, as its
management cluster. Cluster API's Docker infrastructure provider, CAPD,
makes a workload cluster's machines as containers through the Docker
socket of the node it runs on. Cluster API's own test framework mounts the
socket into a kind cluster that it makes. kind cannot add a mount to a
node that is running, and on `flr/kind-ready` the node is running before
the Job starts, so the kind configuration has the mount.

The mount gives every workload in the cluster the guest's Docker Engine,
which means root in the guest. A Job already has root in the guest, and
its MicroVM serves that one Job and is then deleted. So the mount gives a
Job no access that it does not already have.

The kind image carries one node image: the one that the pinned kind
release uses by default, `kindest/node:v1.37.0` for kind 0.33.0. Cluster
API supports Kubernetes v1.37 as a management cluster from v1.13.6 and
v1.14.1. clusterctl checks only a minimum Kubernetes version, so an
earlier release such as v1.11 still installs, but Cluster API does not
test it on v1.37. Two other choices were rejected:

- A Profile setting cannot change the node image. The image is in Docker
  Engine's store in the root filesystem image (GI-065), and a node image
  that a Profile names would come by a pull or a load at boot, which takes
  minutes on a slow Host.
- A second kind image doubles the build, the checks and the release for
  one kind of suite.

A Job that needs another Kubernetes version makes its own cluster on
`flr/kind`, with `--image`.

The kind configuration keeps etcd's default request-size limit and
backend quota. Some Cluster API suites raise them, to 8 MiB and 4 GiB, on
the clusters they make. etcd keeps its data on the node's tmpfs `/tmp`,
so a larger quota lets etcd take more of the MicroVM's memory before it
refuses a write. Few Jobs need the larger limits, so a Job that needs them
sets them on `flr` itself.

## Where Docker's volumes go {#guest-kind-volumes}

- **GI-081** When `flr-kind-cluster` makes the cluster and `FLR_KIND_VOLUMES`
  is `disk`, `flr-kind-cluster` SHALL stop Docker Engine, unmount
  the tmpfs at `/var/lib/docker/volumes` and start Docker Engine
  again before it makes the cluster, so that Docker Engine's
  volumes are on the root disk.
- **GI-082** When `flr-kind-cluster` makes the cluster and `FLR_KIND_VOLUMES`
  is `tmpfs:SIZE`, `flr-kind-cluster` SHALL stop Docker Engine,
  resize the tmpfs at `/var/lib/docker/volumes` to SIZE and start
  Docker Engine again before it makes the cluster.
- **GI-083** If `FLR_KIND_VOLUMES` is `disk` or `tmpfs:SIZE` and Docker Engine
  has a container or a volume, then `flr-kind-cluster` SHALL fail
  without making the cluster and SHALL leave Docker Engine
  running.
- **GI-084** If `FLR_KIND_VOLUMES` is set to a value other than `tmpfs`,
  `tmpfs:SIZE` or `disk`, then `flr-kind-cluster` SHALL fail without
  making the cluster.

Docker Engine's volumes hold each kind node's `/var`, and with it the
node's containerd image store. A Cluster API suite fills the tmpfs of
GI-069 (#157). It loads its images into `flr`, and CAPD makes a kind node,
with a `/var` volume of its own, for each Machine of a workload cluster. A
larger tmpfs is guest memory that the cluster itself needs. The root disk
has room, but on a slow Host the copy of a node's `/var` to the disk can
stall the MicroVM (#133).

So the tmpfs stays the default, and a Profile chooses otherwise in the
command that makes the cluster. On `flr/kind-ready` that command is the
Pool's create hook, which battery runs through a shell, for example
`FLR_KIND_VOLUMES=disk /usr/local/bin/flr-kind-cluster`. On `flr/kind` the
Job runs `flr-kind-cluster` itself and sets the variable there. The choice
applies only when `flr-kind-cluster` makes the cluster: a running cluster
keeps its volumes where they are.

The tmpfs is the default because it is the path that ran on a Host after
#133. The containers' I/O limit (GI-072) came later, and it may make the
disk safe on its own. No Host has measured a cluster's start with the
volumes on the disk under that limit yet. When one has, the default can
change.

Both choices restart Docker Engine. A kernel can refuse to resize a tmpfs
while Docker Engine runs, and the restart makes both choices work the
same way. The restart would stop every container, and with `disk` the
volumes on the tmpfs would stay there, out of Docker Engine's sight. So
`flr-kind-cluster` refuses either choice once anything has used Docker
Engine. For `disk` it writes `/run/flr-guest/volumes-on-disk`, and the
tmpfs unit's condition skips the unit while that file exists.
`docker.service` still requires the unit, but a skipped unit does not
stop it. `/run` is empty at the next boot, so the tmpfs comes back.

## A project's kind cluster spec {#guest-kind-spec}

- **GI-090** If a kind cluster spec has a field that `flr-kind` does not
  know, an `apiVersion` other than `flintlock-runner.dev/v1alpha1`, a
  `kind` other than `KindCluster` or no valid kind cluster name in `name`,
  then `flr-kind` SHALL refuse the spec.
- **GI-091** If an image that a kind cluster spec names under `images`, or
  as a node's `image` in its kind configuration, is not named by a `sha256`
  digest, then `flr-kind` SHALL refuse the spec.
- **GI-092** If an entry under `manifests` has a `url` and no `sha256` of 64
  hexadecimal digits, then `flr-kind` SHALL refuse the spec.
- **GI-093** If a `helm` entry under `manifests` has no `chart`, `version`
  or `release`, or no `digest` of the form `sha256:` and 64 hexadecimal
  digits, then `flr-kind` SHALL refuse the spec.
- **GI-094** If an entry under `manifests` has no source, or more than one of
  `url`, `path`, `kustomize` and `helm`, then `flr-kind` SHALL refuse the
  spec.
- **GI-095** If a kind cluster spec sets `volumes` to a value other than
  `tmpfs`, `tmpfs:SIZE` or `disk`, then `flr-kind` SHALL refuse the spec.
- **GI-096** `flr-kind` SHALL read each path that a kind cluster spec names
  relative to the directory that holds the spec.
- **GI-097** If a path that a kind cluster spec names is absolute, or leads
  out of the repository root, then `flr-kind` SHALL refuse the spec.
- **GI-098** `flr-kind` SHALL take a `kindConfig` that holds a line break as
  the kind configuration itself, and any other `kindConfig` as the path of
  a file that holds it.
- **GI-099** `flr-kind digest` SHALL print `sha256:` and the sha256 of a
  list with one line for the spec and for each file that the spec names by
  path, the files under its kustomize directories included, where each line
  holds the file's sha256 and its path relative to the repository root, and
  the lines are in the byte order of the paths.

A project describes the kind cluster that its e2e suite needs in one file,
the kind cluster spec, by convention `.flr/kind-cluster.yaml`
(`docs/proposals/kind-cluster-spec.md`). `flr-bake` and
`flr-kind-cluster --spec` are to read it through one Go package,
`internal/kindspec`, so that they agree on what a spec means and on its
digest. `flr-kind check SPEC` checks a spec, and `flr-kind digest SPEC`
prints its digest, for a shell script or a test. A refused spec makes
`flr-kind` exit 1, with one line for each fault, and each line names the
field, for example `manifests[2].helm.digest`.

After `apiVersion` and `kind`, only `name` is required. Without `volumes`,
the spec takes `tmpfs`, as `flr-kind-cluster` does without
`FLR_KIND_VOLUMES`. `volumes` takes the values of GI-081 to GI-084, with
the same rule for SIZE. Without `kindConfig`, the cluster is the kind
image's own. A YAML block scalar (`kindConfig: |`) gives the kind
configuration inline; a value on one line is a path. Each entry under
`manifests` has one source and an optional `wait` list, each item as
`KIND/NAME` with an optional `-n NAMESPACE`.

The repository root is the parent of the spec's directory, so a spec at
`.flr/kind-cluster.yaml` can name any file in the repository, such as
`../test/e2e/kind-config.yaml`, and no file outside it. `flr-kind check -root DIR`
names another root. A symbolic link that leads out of the root counts as a
path that leads out of it. Under a kustomize directory, a symbolic link to
a file in the root counts as that file, and a symbolic link to a directory
is refused, so the digest covers what kustomize reads.

The digest identifies the cluster that a spec makes. Its list is what
`sha256sum` prints, so `sha256sum FILE... | sha256sum`, run from the root
with the files in byte order, gives the same sha256. Each line has the
file's path, so a file that moves changes the digest, and a checkout in a
different directory does not. The list has each file once, in byte order,
not in the order of the entries; the spec holds that order, and the spec
is in the list. A path with a line break or a backslash is refused,
because `sha256sum` escapes those characters.

## Baking a project's kind cluster {#guest-kind-bake}

- **GI-100** `flr-kind bake SPEC OUT` SHALL write to
  `OUT/etc/flr/kind-cluster.yaml` a kind cluster spec that has its kind
  configuration inline and that names, by a path relative to `/etc/flr`,
  the file under `OUT/var/lib/flr` that it wrote for each manifest, chart
  and values file.
- **GI-101** `flr-kind bake` SHALL write the digest of the spec, as
  `flr-kind digest` prints it, to `OUT/etc/flr/kind-cluster.digest`.
- **GI-102** If OUT holds a file, then `flr-kind bake` SHALL fail without
  writing to it.
- **GI-103** When a node of the kind configuration names an image other
  than the one that the kind image's `KIND_NODE_IMAGE` names, `flr-kind
  bake` SHALL write that image, for the target architecture, into Docker
  Engine's image store under `OUT/var/lib/docker` with `docker-store.sh`.
- **GI-104** When `flr-kind bake` writes an image into Docker Engine's
  image store, `flr-kind bake` SHALL write a `repositories.json` that holds
  every reference of the kind image's `repositories.json` and every
  reference that it wrote.
- **GI-105** `flr-kind bake` SHALL leave out of `OUT/var/lib/docker` each
  layer and image configuration that the kind image's image store holds
  already.
- **GI-106** If a node image that `flr-kind bake` has to write into Docker
  Engine's image store has no tag, or `flr-kind bake` has no kind image
  root to read, then `flr-kind bake` SHALL fail.
- **GI-107** `flr-kind bake` SHALL write each image under `images`, for the
  target architecture, as a docker-archive in `OUT/var/lib/flr/images`.
- **GI-108** `flr-kind bake` SHALL write each manifest by URL or by path,
  and the output of `kustomize build` for each kustomize directory, to
  `OUT/var/lib/flr/manifests/N.yaml`, where N is the entry's index under
  `manifests`.
- **GI-109** If the sha256 of a manifest that `flr-kind bake` downloads is
  not the entry's `sha256`, then `flr-kind bake` SHALL fail.
- **GI-110** `flr-kind bake` SHALL write each chart's archive to
  `OUT/var/lib/flr/charts/N.tgz`, and its values files to
  `OUT/var/lib/flr/charts/N-values-M.yaml`, where N is the entry's index
  under `manifests` and M the file's index under `values`.
- **GI-111** If a chart's `digest` is neither the sha256 of the chart's
  archive nor, for a chart from an OCI registry, the digest of an OCI
  manifest whose chart layer is that archive, then `flr-kind bake` SHALL
  fail.
- **GI-112** `flr-kind bake` SHALL find the image of each container, init
  container and ephemeral container in each manifest, in the output of
  `kustomize build` for each kustomize directory, and in the output of
  `helm template` for each chart with its values.
- **GI-113** `flr-kind bake` SHALL write each image that it finds, by the
  digest that its reference resolves to and for the target architecture,
  as a docker-archive in `OUT/var/lib/flr/images`, unless an image under
  `images` has the same name and the same tag or digest.
- **GI-114** `flr-kind bake` SHALL write `OUT/etc/flr/images.lock` with one
  line for each image that it finds, which holds the image as the manifest
  names it and the digest of the image that it baked, in the byte order of
  the images.
- **GI-115** `flr-kind bake` SHALL print the total size of the files that it
  wrote under OUT.
- **GI-116** The Guest Image SHALL include a baker image, for `linux/amd64`
  and `linux/arm64`, that contains `flr-kind`, `flr-bake`,
  `docker-store.sh`, skopeo and curl, and kustomize and helm at the
  versions pinned in its sources.
- **GI-117** The Release SHALL publish the Guest Image's baker image to the
  project's container registry as one image index for `linux/amd64` and
  `linux/arm64`, tagged with the release version.

A project builds its own guest image from the kind image, with its spec
and everything the spec names inside
(`docs/proposals/kind-cluster-spec.md`). The baker image, `guest-baker`,
does the work in one stage of the project's build, and the last stage
copies the result onto the kind image. `flr-bake` in the baker image runs
`flr-kind bake`. The release lists the baker image in `images.txt` with
the other images. `docs/docker-and-kind.md` has the Containerfile.

`flr-kind bake` writes these files under OUT, at their paths in the guest:

| Path | What |
|---|---|
| `etc/flr/kind-cluster.yaml` | The spec, made to stand alone |
| `etc/flr/kind-cluster.digest` | The digest of the project's spec, and a line feed |
| `etc/flr/images.lock` | `IMAGE DIGEST` for each image found in the manifests |
| `var/lib/docker/...` | Each node image that the kind image does not have |
| `var/lib/flr/images/N.tar` | Each image, as a docker-archive |
| `var/lib/flr/manifests/N.yaml` | Each entry N by URL, path or kustomize |
| `var/lib/flr/charts/N.tgz`, `N-values-M.yaml` | Each chart entry N, and its values files |

The baked spec keeps the order of `manifests`, each `wait` list, `name`,
`volumes` and `images`. An entry by URL or kustomize becomes an entry by
path, and a chart's `chart` becomes the path of its archive, with `digest`
set to the archive's sha256. The paths lead from `/etc/flr` to
`/var/lib/flr`, such as `../../var/lib/flr/manifests/0.yaml`, so the spec
loads with `/` as its repository root. Its own digest is not the
project's: the create hook reads the project's digest from
`kind-cluster.digest`, and the job compares that with the digest of its
checkout.

The kind image's node image is in Docker Engine's store already (GI-065).
`flr-kind bake -base DIR` reads `KIND_NODE_IMAGE` and the store from DIR,
the kind image's root filesystem as a mount, and writes each other node
image with `docker-store.sh`, which writes a node image under its tag and
its digest. `repositories.json` is one file, so the copy onto the kind
image replaces it. The baked one holds the kind image's references as well
as the new ones. `docker-store.sh` names each layer after its chain of
layer digests, so a layer that the kind image has is the same directory
in both, and the bake leaves it out.

Images go in as archives because the create hook loads them into the
nodes with `kind load image-archive`. Each archive names the image by its
tag, as `NAME:TAG`. A docker-archive does not keep the registry's manifest,
so an image by digest alone has no name in its archive, and a Pod that
names an image by digest does not find the archive. `images` also pins
the image that a manifest names by tag: if an image under `images` has the
same name and tag, the bake uses that image and its digest, and does not
look up the tag. The lock file shows the digest that each image got.

A chart is pulled with `helm pull`: an `oci://` reference, the URL of a
chart's archive (`.tgz`), or `REPOSITORY_URL/CHART`, which becomes
`helm pull CHART --repo REPOSITORY_URL`. For a chart from an OCI registry,
`digest` can be the digest that `helm push` prints, which is the OCI
manifest's: the bake reads that manifest by the chart's version, checks
its digest, and checks that its chart layer is the archive that helm
pulled. `helm template` renders a chart without its tests, which only
`helm test` runs, so the images of its test hooks are not baked.

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
