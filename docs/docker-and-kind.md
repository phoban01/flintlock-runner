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
