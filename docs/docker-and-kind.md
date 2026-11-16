# Docker, Go and kind in a Job

Two root filesystem images of the Guest Image let a Job build and run
containers, and make a Kubernetes cluster, inside its MicroVM:

| Image | Profile image name | What it adds |
|---|---|---|
| `guest-rootfs-docker` | `flr/docker` | Docker Engine, containerd, runc and buildx; iptables on the legacy backend; Go with `GOTOOLCHAIN=auto` |
| `guest-rootfs-kind` | `flr/kind` | everything in `guest-rootfs-docker`, plus kind and kubectl, the kind node image, `flr-kind-cluster`, `/etc/default/kind-bootstrap` and `/etc/kind/kind-config.yaml` |
| `guest-rootfs-kind` | `flr/kind-ready` | the same image; its Pool makes the kind cluster before a Job claims the MicroVM |

`guest/README.md` lists the versions. `docs/requirements/13-guest-image.md`
(GI-050 to GI-072) has the requirements. A Job asks for one with its
`image:`, and the Runner's Profile for that name boots it
(`deploy/runner/config.yaml` has examples).

## What the guest finds at boot

Each MicroVM is new, so every Job starts with an empty Docker image store
and an empty Go module cache. The Host Services keep what a Job fetched,
so the next Job on the same Host fetches it from the Host:

- **Images.** A boot unit, `flr-registry-mirror.service`, reads the
  default gateway of the DHCP lease once the network is up. The Host
  Services listen there. It writes `/etc/docker/daemon.json` with the
  Host's registry mirror, `http://<gateway>:5000`, before Docker Engine
  starts. `docker pull` of a Docker Hub image goes through the mirror. In
  the kind image the same unit writes `/etc/kind/kind-config.yaml`, which
  gives the kind node's containerd the same mirror.
- **Go modules.** The Executor sets `GOPROXY` to the Host's Go module
  proxy. With `GOTOOLCHAIN=auto`, a newer Go toolchain that a `go.mod`
  asks for comes through it too.
- **Image builds.** The Executor sets `BUILDKIT_HOST` to the Host's
  `buildkitd`. A build there keeps its layer cache on the Host.

If the guest has no lease, Docker Engine and kind start with no mirror
and pull from Docker Hub. `docs/host-services.md` says what the Jobs on
one Host share through these caches.

## A kind cluster, fast

`flr-kind-cluster` makes the cluster that `/etc/default/kind-bootstrap`
describes, or takes the one that is running, and waits until its node is
Ready. The kubeconfig is at `$KUBECONFIG_PATH`, which `$KUBECONFIG` names.

- `flr/kind` boots the kind image, and the Job makes its cluster.
  Nothing is pulled: the build put the kind node image in Docker Engine's
  image store, and etcd keeps its data on a tmpfs.
- `flr/kind-ready` boots the same image. Its Pool runs
  `flr-kind-cluster` as a create hook, so battery hands out each MicroVM
  with the cluster running, and the Job's own `flr-kind-cluster` returns
  at once. Each MicroVM keeps its cluster running while it waits in the
  Pool, which costs it CPU and memory. battery waits 30 seconds for a new
  MicroVM's guest agent, then runs the hook with no time limit; a hook
  that fails gets the MicroVM replaced (`hook_failure_policy`).

A Job that runs `kind create cluster --image "$KIND_NODE_IMAGE"` itself
still works on `flr/kind`, and pulls nothing either. On `flr/kind-ready` a
second `kind create cluster` of the same name fails, because the cluster
is there.

In the Docker and kind images, the guest agent runs in a slice of its own
(`agent.slice`) with 128 MiB of memory kept for it and a high CPU
weight. containerd, Docker Engine and every container run in
`containers.slice`, limited to 90 percent of the MicroVM's memory. A
cluster that takes all the memory or CPU then slows or kills containers,
not the guest agent, whose exec stream flintlock drops after 15 seconds
of silence. A Stage's own processes, which the guest agent starts, share
its slice.

kind starts its node with `/var` as an anonymous Docker volume, and
Docker Engine first copies the node image's `/var`, about 400 MB, into
it. In the kind image, Docker Engine's volumes are on a tmpfs
(`/var/lib/docker/volumes`, 25 percent of the MicroVM's memory), so that
copy goes to memory and not to the disk (GI-069). A heavy write to the
root disk can stall the MicroVM long enough for flintlock to drop the
exec stream (#133). The node image stays on disk, where it is mostly
read. The node's `/var` also holds the images the cluster pulls, so they
are in memory too.

### Block I/O in containers

`containers.slice` also limits the containers' block I/O on the root
disk: 1500 read and 1500 write IOPS, and 60 MB/s each way (GI-072).
Firecracker serves a MicroVM's disk and its vsock from one thread. While
a container keeps the disk's queue full, the guest agent's heartbeats wait
behind the queue, and flintlock drops the exec stream after 15 s. On a
Host whose thin pool is an EBS gp3 volume at its baseline, 3,000 IOPS and
125 MB/s, a burst of small writes does that (#133). Below the limits the
queue empties between requests, and the heartbeats get through.

What this means for a Job:

- `docker run`, `docker build` in the guest, and the kind node write at
  the limit at most. A container that writes 50,000 small files takes at
  least 33 s. `fio --rw=randwrite --bs=4k --direct=1` in a container
  reports about 1500 IOPS.
- The kind node's copy of its `/var` reads the node image from the root
  disk at the limit. The volume it writes is on the tmpfs, which is not
  limited.
- A Stage's own processes, such as `go build` and `git clone`, run in the
  guest agent's slice and are not limited. `docker buildx build` with
  `BUILDKIT_HOST` builds on the Host, not in the guest.
- `io.stat` and `io.max` of `/sys/fs/cgroup/containers.slice` show the
  I/O and the limits in the guest.

A Host with a faster thin pool, such as a gp3 volume with provisioned
IOPS and throughput or an NVMe instance store, can take more. A derived
image sets `/etc/default/flr-io` (`guest/README.md`, "Using them"), and a
Profile names that image.

### MicroVM size for kind

The tmpfs size is a ceiling, not a reservation. Its pages count against
`containers.slice` once Docker Engine writes them. So the node's `/var`,
etcd's tmpfs and the cluster's processes share that slice's limit of 90
percent of the MicroVM's memory. The node's `/var` takes 390 MB of the
tmpfs as soon as kind starts the node. Each image the cluster pulls adds
its compressed and its unpacked size.

- 8 GiB, as the example Profiles: a 2 GiB tmpfs, room for the node's
  `/var` and about 1.5 GB of images pulled into the cluster.
- 4 GiB: the smallest size we recommend. A 1 GiB tmpfs holds the node's
  `/var` and a few small images.
- 2 GiB: a 498 MiB tmpfs holds the node's `/var` with 108 MiB to spare.
  A pull into the cluster can fill it, and the pull then fails with no
  space left.

### Where Docker's volumes go

`FLR_KIND_VOLUMES` chooses where Docker Engine keeps its volumes, and so
each kind node's `/var` and containerd image store.
`flr-kind-cluster` applies it when it makes the cluster:

| Value | Where | Size |
|---|---|---|
| `tmpfs`, or unset | the tmpfs | 25 percent of the MicroVM's memory |
| `tmpfs:SIZE`, for example `tmpfs:50%` or `tmpfs:4G` | the tmpfs, resized | SIZE |
| `disk` | the root disk | what the disk has free |

On `flr/kind`, set it where the Job runs `flr-kind-cluster`:

```sh
. /etc/default/kind-bootstrap
FLR_KIND_VOLUMES=disk flr-kind-cluster
```

On `flr/kind-ready` the Pool makes the cluster, so a Profile sets it in
its create hook:

```yaml
pool:
  create_hooks:
    - FLR_KIND_VOLUMES=disk /usr/local/bin/flr-kind-cluster
```

- `tmpfs:SIZE` and `disk` restart Docker Engine before the cluster is
  made. So `flr-kind-cluster` refuses them once anything else has used
  Docker Engine: run it first.
- A cluster that is running keeps its volumes where they are. A Job on
  `flr/kind-ready` cannot move them, because a restart of the node loses
  etcd's data on the node's `/tmp`.
- `disk` lasts until the MicroVM stops, and MicroVMs are not reused.
- A larger tmpfs is memory that the cluster's processes then cannot use.
  On the disk, a node's `/var` copy goes through the containers' I/O
  limit, and a cluster can start more slowly. No Host has measured how
  much more yet (#157).

## A Cluster API management cluster

A Cluster API provider's e2e suite can use `flr` as its management cluster
and make no cluster of its own. Set `KIND_CLUSTER_NAME=flr`, and for
Cluster API's test framework, `SKIP_CREATE_MGMT_CLUSTER=true`.

- The node has the guest's Docker socket at `/var/run/docker.sock`, so
  CAPD, Cluster API's Docker provider, can make a workload cluster's
  machines as containers in the guest. Every workload in the cluster can
  use that socket, which means root in the guest. A Job has root in the
  guest already, and its MicroVM is deleted after the Job.
- The node is `kindest/node:v1.37.0`. Cluster API supports v1.37 as a
  management cluster from v1.13.6 and v1.14.1. clusterctl checks only a
  minimum Kubernetes version, so an earlier Cluster API still installs,
  but Cluster API does not test that combination.
- etcd has its default request-size limit and quota. A suite that raises
  them, with `--max-request-bytes` and `--quota-backend-bytes`, sets them
  on `flr` itself.
- A suite needs more room for images than the default tmpfs has. Give
  its Profile `FLR_KIND_VOLUMES=disk` in the create hook, as in
  [Where Docker's volumes go](#where-dockers-volumes-go).

### Space for a Cluster API suite

On a 6 GiB MicroVM, one suite filled a 1.5 GiB tmpfs once it had loaded
its images into `flr`, and its next pulls failed. At 3 GiB it filled 97
percent of the tmpfs, and less than 900 MiB of the guest's memory was
left (#157). The root disk had 5.9 GB free at the same time.

CAPD makes a kind node container for each Machine of a workload cluster.
Each node has a `/var` volume of its own, about 390 MB before it pulls an
image, and its own processes. A workload cluster of three control-plane
Machines and one worker adds four such nodes. On the default tmpfs it
does not fit. Use `disk`, and size the MicroVM's memory for the nodes'
processes.

## A project's own kind cluster

A project can describe the kind cluster its e2e suite needs in a kind
cluster spec, `.flr/kind-cluster.yaml`, and build its own guest image with
the spec and everything it names inside
(`docs/proposals/kind-cluster-spec.md`). `flr-kind check` checks a spec,
and `flr-kind digest` prints its digest (GI-090 to GI-099).

[A kind cluster ready at job start](kind-cluster-spec.md) is the guide for
a project. This section has the details.

The build starts from the kind image and the `guest-baker` image of one
release. Take both digests from that release's `images.txt`. This
Containerfile, for example as `.flr/Containerfile`, builds the image:

```dockerfile
FROM ghcr.io/phoban01/flintlock-runner/guest-rootfs-kind@sha256:... AS base

FROM --platform=$BUILDPLATFORM ghcr.io/phoban01/flintlock-runner/guest-baker@sha256:... AS bake
ARG TARGETARCH
COPY . /src
RUN --mount=type=bind,from=base,target=/base \
    flr-bake -base /base -arch "$TARGETARCH" /src/.flr/kind-cluster.yaml /out

FROM base
COPY --from=bake /out/ /
```

Build it from the repository root, for each architecture of the project's
Hosts:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -f .flr/Containerfile \
  -t registry.example.com/my-project/e2e-guest:1 --push .
```

- The bake stage runs on the build machine's platform, and `-arch` picks
  the architecture of every image it fetches. Nothing runs under
  emulation.
- `-base` mounts the kind image. `flr-bake` reads the node image that the
  kind image has from it, and Docker Engine's `repositories.json`, which
  the bake extends. Without node images in the kind configuration, the
  bake does not need it.
- `COPY . /src` copies the build context. Every file that the spec names
  has to be in it. A `.dockerignore` keeps the rest out.
- The bake needs the network: it fetches the images, the manifests by URL,
  the charts and any remote kustomize base.

`flr-bake` is `flr-kind bake` (GI-100 to GI-117). It writes these files,
which the last stage copies to the same paths in the guest:

| Path in the guest | What |
|---|---|
| `/etc/flr/kind-cluster.yaml` | The spec, with the kind configuration inline and every other file by a path relative to `/etc/flr` |
| `/etc/flr/kind-cluster.digest` | The digest of the project's spec, as `flr-kind digest` prints it |
| `/etc/flr/images.lock` | `IMAGE DIGEST` for each image that the manifests and charts start |
| `/var/lib/docker/...` | Each node image that the kind image does not have, in Docker Engine's image store |
| `/var/lib/flr/images/N.tar` | Each image under `images`, then each image found in the manifests, as a docker-archive |
| `/var/lib/flr/manifests/N.yaml` | Manifest entry N by URL or path, or the output of `kustomize build` |
| `/var/lib/flr/charts/N.tgz` | Chart entry N's archive, with its values files as `N-values-M.yaml` |

`flr-bake` prints the total size of what it wrote. A MicroVM's root disk is
10 GiB, and a MicroVM of the kind image had 5.9 GB of it free (#157). The
baked files take their size from that. Each image also goes into every
node when the cluster starts, on the tmpfs or on the disk as `volumes`
says.

An image that a manifest names by tag gets the digest that the tag has at
build time. `images.lock` shows which. To pin it, list it under `images`
with the same name and tag and its digest: the bake then uses that digest.
An archive names its image by tag, so a Pod that names an image by digest
does not find it (#169).

### The Profile

The project's Profile boots that image, and its create hook makes the
cluster:

```yaml
- name: battery-operator-e2e
  arch: amd64
  vcpu: 4
  memory_mb: 8192
  images: [flr/battery-operator-e2e]
  kernel:
    image: ghcr.io/phoban01/flintlock-runner/guest-kernel@sha256:...
    filename: boot/vmlinux
  rootfs: ghcr.io/phoban01/battery-operator/e2e-guest@sha256:...
  pool:
    size: 1
    create_hooks:
      - /usr/local/bin/flr-kind-cluster --spec /etc/flr/kind-cluster.yaml
```

In the create hook, `flr-kind-cluster --spec` takes these steps. None of
them uses the network.

1. It puts Docker Engine's volumes where the spec's `volumes` says, as
   `FLR_KIND_VOLUMES` does without a spec. With `--spec`,
   `FLR_KIND_VOLUMES` does nothing.
2. It makes the cluster, with the spec's `name`, from the spec's
   `kindConfig`, or from the kind image's own configuration when there is
   none. A project's `kindConfig` gets the kind image's additions: the
   containerd patch for the Host's registry mirror and the mirror's hosts
   directory on every node, and the guest's Docker socket on each
   control-plane node. A node with no `image` gets the node image that
   Docker Engine has already. The project's file needs to know about none
   of this.
3. It checks the space free where Docker Engine keeps its volumes, then
   loads each archive under `/var/lib/flr/images` into every node with
   `kind load image-archive`.
4. It applies each entry under `manifests` in order: `kubectl apply -f`
   for a file, `kubectl apply -k` for a kustomize directory, and
   `helm install` for a chart, with its values files and its namespace.
   After each entry it waits for each item of its `wait` list, for up to
   `KIND_WAIT`: `kubectl rollout status` for a Deployment, DaemonSet or
   StatefulSet, the condition Established for a CRD, and Ready for
   anything else.
5. It writes the spec's digest to `/run/flr-guest/kind-cluster.digest`.

A step that fails makes the hook fail, with no digest written, and
battery replaces the MicroVM. With `--spec`, the kubeconfig goes to
`~/.kube/config`, where kind and kubectl look by default, or to
`FLR_KIND_KUBECONFIG` when that is set. A Job that sources
`/etc/default/kind-bootstrap` gets a `KUBECONFIG` for `flr`, so it
unsets it or sets `FLR_KIND_KUBECONFIG` to the same file.

### In the job

The job runs the same command on the spec in its checkout, before its
tests:

```sh
flr-kind-cluster --spec .flr/kind-cluster.yaml
```

It finds the digest of that spec with `flr-kind digest`, and compares it
with `/run/flr-guest/kind-cluster.digest`:

- If they are the same, it writes the kubeconfig and returns at once.
- If they are different, or there is no digest, it logs a warning that
  names both digests, deletes every kind cluster, and makes a new one from
  the job's spec with the same steps. This happens on a branch that
  changes the spec, or on a MicroVM that booted an older image.

A new cluster in the job gets the images under `/var/lib/flr/images`, as
in the hook. Other images come through the Host's registry mirror, or
from their own registries, when a pod starts. A `path` or `kustomize`
entry comes from the checkout. A `url` entry is downloaded and checked
against its `sha256`. A chart by reference is fetched by `helm install`,
at its `version`; helm does not check the chart's `digest`. The job is
slower, but it tests the cluster that its branch describes.

### What the hook reads

`flr-bake` writes the spec into the project's image. The hook
reads these files:

| Path | What |
|---|---|
| `/etc/flr/kind-cluster.yaml` | The baked spec, as `internal/kindspec` reads it |
| `/etc/flr/kind-cluster.digest` | The digest of the project's own spec, `sha256:` and 64 hexadecimal digits |
| `/var/lib/flr/images/*.tar` | Docker archives, each loaded into every node |
| `/var/lib/flr/manifests/*`, `/var/lib/flr/charts/*` | The files that the baked spec names by path |

A spec with a `.digest` file beside it, of the same name, is a baked spec.
`flr-kind-cluster` takes its digest from that file, and does not compute
one, because the baked spec is not the project's spec. The paths in a
baked spec are relative to `/etc/flr` and stay inside `/`, for example
`path: ../../var/lib/flr/manifests/0.yaml`. A chart's `chart` is the path
of its archive, for example `../../var/lib/flr/charts/1.tgz`, with its
`version`, `digest` and `release` kept. A baked spec with a `url`, or with
a chart that is not a file in the guest, is refused before the hook
changes anything.

`flr-kind-cluster` reads no YAML itself. `flr-kind plan` reads the spec,
writes the kind configuration to make the cluster from, and prints one
line for each step, with the fields separated by tabs and `-` for an
empty field:

| Line | Step |
|---|---|
| `name NAME` | The cluster's name |
| `volumes VOLUMES` | Where Docker Engine's volumes go |
| `nodes N` | The number of nodes, for the space check |
| `apply FILE` | `kubectl apply -f FILE` |
| `apply-k DIR` | `kubectl apply -k DIR` |
| `url URL SHA256` | Download, check, then `kubectl apply -f` |
| `helm RELEASE NAMESPACE CHART VERSION [VALUES...]` | `helm install`; VERSION is `-` for a local archive |
| `wait RESOURCE NAMESPACE` | Wait for one item of the entry before it |

### Space for a project's cluster

`kind load` puts a copy of each image in every node, and each node's
containerd keeps both the layers and the unpacked image. The hook
refuses to load the archives when the space free at
`/var/lib/docker/volumes` is less than twice their size times the number
of nodes. Its message names the two ways out: `volumes: disk`, or a
larger `volumes: tmpfs:SIZE`, in the spec. Each node also takes about
390 MB for its `/var` before the load, so a spec with more than one node
needs one of the two.

## Example jobs

```yaml
# A kind cluster in the MicroVM: make it, check it, run a pod in it.
# With image: flr/kind-ready the cluster is running already, and
# flr-kind-cluster only writes the kubeconfig.
verify:kubectl-cluster-info:
  image: flr/kind
  script:
    - . /etc/default/kind-bootstrap
    - flr-kind-cluster
    - kubectl cluster-info
    - kubectl run nginx --image=docker.io/library/nginx:1.29 --restart=Never
    - kubectl wait --for=condition=Ready pod/nginx --timeout=5m
    - kubectl get pods -A

# The Host's caches: an image through the registry mirror and Go modules
# through GOPROXY. Run it twice on one Host and the second run is faster.
verify:cache-demo:
  image: flr/docker
  script:
    - docker info --format '{{.Driver}} cgroup v{{.CgroupVersion}} mirrors {{.RegistryConfig.Mirrors}}'
    - time docker pull docker.io/library/python:3.13-slim
    - mkdir -p /tmp/cachedemo && cd /tmp/cachedemo && go mod init example.com/cachedemo
    - time go get k8s.io/client-go@v0.34.1

# Build with Go, build the image on the Host's buildkitd, and run it.
verify:build-demo:
  image: flr/docker
  script:
    - CGO_ENABLED=0 go build -trimpath -o hello .
    - docker buildx create --name flr --driver remote "$BUILDKIT_HOST" --use
    - docker buildx build --load -t hello:latest .
    - docker run --rm hello:latest
```

The build job expects a `main.go` and a `Dockerfile` in the repository,
for example:

```dockerfile
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache ca-certificates
COPY hello /usr/local/bin/hello
ENTRYPOINT ["/usr/local/bin/hello"]
```

`docker buildx build --load` sends the result back from `buildkitd` into
the guest's Docker Engine. To push instead, log in and use `--push`.

## Limits

- iptables is on its legacy backend. Docker Engine and kind work with
  that. Tools that need `nft` may not.
- The kernel has the iptables `raw` table and the `comment`, `mark`,
  `statistic` and `multiport` matches that kube-proxy uses. It has no IPVS
  and no VXLAN.
- On battery-operator's `hack/kubeadm-host` on a Mac, whose Host is a
  nested VM with a slow disk, kind cannot make its node. The volume on a
  tmpfs did not change that: the copy of the node image's `/var` still
  took 4 minutes, because the slow part is the cold read of the node
  image from the guest's root disk, not the write. The node's systemd
  then starts slowly too, and kind gives up waiting for it. The kind Job
  has not run to the end there. A Host on real hardware has not run it
  yet either (#133, #136).
- A MicroVM's root disk is 10 GiB on the Host Image. The kind node image
  takes about 1 GiB of it, unpacked.
- Containers read and write the root disk at 1500 IOPS and 60 MB/s at
  most, each way, unless a derived image raises the limits
  (`/etc/default/flr-io`). A Stage's own processes are not limited, so a
  Stage that writes faster than the Host's thin pool completes can still
  stall the MicroVM (#133).
- `/etc/default/kind-bootstrap` pins the kind node image by digest. A Job
  that wants another Kubernetes version passes its own `--image`, and
  should take it by digest from kind's release notes.
