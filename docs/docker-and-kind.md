# Docker, Go and kind in a Job

Two root filesystem images of the Guest Image let a Job build and run
containers, and make a Kubernetes cluster, inside its MicroVM:

| Image | Profile image name | What it adds |
|---|---|---|
| `guest-rootfs-docker` | `flr/docker` | Docker Engine, containerd, runc and buildx; iptables on the legacy backend; Go with `GOTOOLCHAIN=auto` |
| `guest-rootfs-kind` | `flr/kind` | everything in `guest-rootfs-docker`, plus kind and kubectl, the kind node image, `flr-kind-cluster`, `/etc/default/kind-bootstrap` and `/etc/kind/kind-config.yaml` |
| `guest-rootfs-kind` | `flr/kind-ready` | the same image; its Pool makes the kind cluster before a Job claims the MicroVM |

`guest/README.md` lists the versions. `docs/requirements/13-guest-image.md`
(GI-050 to GI-071) has the requirements. A Job asks for one with its
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

### MicroVM size for kind

The tmpfs size is a ceiling, not a reservation. Its pages count against
`containers.slice` once Docker Engine writes them, so the cluster, the
node's `/var` and etcd's tmpfs share that slice's 90 percent. An idle
one-node cluster uses about 1 GiB of memory besides its `/var`.

- 8 GiB, as the example Profiles: a 2 GiB tmpfs, room for the node's
  `/var` and about 1.5 GB of images pulled into the cluster.
- 4 GiB: the smallest size we recommend. A 1 GiB tmpfs holds the node's
  `/var` and a few small images.
- 2 GiB: a 512 MiB tmpfs holds the node's `/var` with little to spare. A
  pull into the cluster can fill it, and the pull then fails with no
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
  nested VM with a slow disk, kind cannot make its node: Docker Engine
  copies the node image's `/var` into a new volume for minutes, and kind
  gives up waiting for the node's systemd. The kind Job has not run to the
  end there, before or after these changes. A Host on real hardware has
  not run it yet either (#133).
- A MicroVM's root disk is 10 GiB on the Host Image. The kind node image
  takes about 1 GiB of it, unpacked.
- `/etc/default/kind-bootstrap` pins the kind node image by digest. A Job
  that wants another Kubernetes version passes its own `--image`, and
  should take it by digest from kind's release notes.
