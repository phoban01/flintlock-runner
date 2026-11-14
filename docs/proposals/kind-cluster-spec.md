# Proposal: a kind cluster spec that a project bakes into its own guest image

**Status:** draft
**From:** flintlock-runner
**Date:** 2026-11-14

## Summary

A project describes the kind cluster its e2e suite needs in one file, the
cluster spec. The file names the kind configuration, the images to load into
the nodes, and the manifests to apply. The project builds its own guest
image from the kind image, with the spec and every image and manifest it
names inside. The Runner's Profile for that project boots the image, and its
create hook makes the cluster from the spec. battery then hands out each
MicroVM with the cluster ready, and the job does no setup for the suite's
dependencies.

## The problem

An e2e suite spends much of its time before the first test. battery-operator's
suite is an example. On each run it does these steps:

1. It builds three images.
2. It makes a 3-node kind cluster from `test/e2e/kind-config.yaml`.
3. It loads the three images into the nodes.
4. It installs cert-manager and waits for its webhook, for up to 5 minutes.
5. It deploys the Manifests and waits for them.

`flr/kind-ready` makes a cluster in advance, but only the one cluster that
the kind image describes. battery-operator's suite needs three nodes, node
labels and a pinned node image. It cannot use that cluster, so it saves
nothing. Each new option on the kind image, such as the Docker socket for
CAPD (#153) or `FLR_KIND_VOLUMES` (#157), fits one more suite. The options
are guesses, in the guest image, about each project's tests.

This proposal moves the cluster's description to the project, and moves
steps 2 and 4 into the Pool. Steps 1, 3 and 5 stay in the job, because they
work on the code under test, which changes on each commit.

## The cluster spec

The project commits the spec, for example as `.flr/kind-cluster.yaml`:

```yaml
apiVersion: flintlock-runner.dev/v1alpha1
kind: KindCluster
# The kind cluster's name: the name the suite expects.
name: battery-operator-e2e
# Where Docker Engine keeps its volumes: tmpfs, tmpfs:SIZE or disk
# (FLR_KIND_VOLUMES, GI-081 to GI-084).
volumes: disk
# The kind configuration, inline or as a path relative to this file.
kindConfig: ../test/e2e/kind-config.yaml
# Images to load into every node, each by digest.
images:
  - ghcr.io/liquidmetal-dev/poolmgrd:0.6.0@sha256:...
# Manifests to apply, in order, once every node is Ready. Each is a URL
# with its sha256, or a path relative to this file.
manifests:
  - url: https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml
    sha256: ...
    # What to wait for before the next manifest, and before the cluster
    # counts as ready.
    wait:
      - deployment/cert-manager -n cert-manager
      - deployment/cert-manager-cainjector -n cert-manager
      - deployment/cert-manager-webhook -n cert-manager
```

The rules:

- Every image is named by digest: the node images in the kind configuration,
  and the images under `images`. A tag alone is refused, so that two builds
  of one spec give the same guest image.
- Every manifest by URL has a `sha256`. The build checks it.
- The images that the manifests start, such as cert-manager's, go under
  `images` too. Otherwise the nodes pull them when the hook applies the
  manifest. That still works, through the Host's registry mirror, but it is
  slower and needs the network.
- The spec's **digest** is the sha256 of the spec and of every file it names
  by path, in a fixed order. It identifies the cluster that the spec makes.

## The project's guest image

The project builds a guest image from the kind image of a flintlock-runner
release:

```dockerfile
FROM ghcr.io/phoban01/flintlock-runner/guest-rootfs-kind@sha256:... AS base

FROM ghcr.io/phoban01/flintlock-runner/guest-baker@sha256:... AS bake
COPY . /src
RUN flr-bake /src/.flr/kind-cluster.yaml /out

FROM base
COPY --from=bake /out/ /
```

`guest-baker` is a new image in each release. It has skopeo, curl and
`flr-bake`. `flr-bake` reads the spec and writes these files under `/out`:

| Path in the guest | What |
|---|---|
| `/etc/flr/kind-cluster.yaml` | The spec, with the kind configuration and the manifests by path made inline |
| `/etc/flr/kind-cluster.digest` | The spec's digest |
| `/var/lib/docker/...` | Each node image that the kind image does not have already, in Docker Engine's image store, through `docker-store.sh` |
| `/var/lib/flr/images/<n>.tar` | Each image under `images`, as a docker-archive |
| `/var/lib/flr/manifests/<n>.yaml` | Each manifest, after its sha256 is checked |

The node images go into Docker Engine's image store, because Docker Engine
runs the nodes. The other images go in as archives, because they go into
the nodes' containerd, and `kind load image-archive` puts them there. This
also means that `docker-store.sh`, which refuses layers with whiteouts,
only has to handle node images, as it does now.

The project's CI builds and pushes the image when the spec or a file it
names changes. A project with arm64 Hosts builds it for both architectures,
as the release builds the kind image.

## The Profile

The Runner's configuration gets one Profile for the project. It boots the
project's image, and its create hook makes the cluster:

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

With `--spec`, `flr-kind-cluster` does these steps:

1. It puts Docker Engine's volumes where `volumes` says (GI-081 to GI-084).
2. It makes the cluster from the kind configuration.
3. It loads each archive under `/var/lib/flr/images` into every node.
4. It applies each manifest in order, and waits for what it names.
5. It writes the spec's digest to `/run/flr-guest/kind-cluster.digest`.

Nothing in these steps uses the network. A step that fails makes the hook
fail, and battery replaces the MicroVM (`hook_failure_policy`).

## The job

The job asks for `image: flr/battery-operator-e2e` and runs one command
before its tests:

```sh
flr-kind-cluster --spec .flr/kind-cluster.yaml
```

The command finds the digest of the project's spec, as the job's checkout
has it, and compares it with the digest in `/run/flr-guest`:

- If they are the same, it writes the kubeconfig and returns at once.
- If they are different, it logs a warning that names both digests, deletes
  the cluster, and makes a new one from the job's spec. This happens on a
  branch that changes the spec. The job is slow, but it tests what the
  branch asks for.

The suite then runs as it does on a laptop. battery-operator's suite uses
an existing cluster of its name as it is (`test/e2e/setup_test.go`).
cert-manager's apply changes nothing, and its waits pass at once.

## Costs and limits

- **Memory.** Each warm MicroVM keeps its cluster running while it waits.
  For battery-operator's three nodes that is most of 8 GiB for each warm
  MicroVM. Keep these Pools small.
- **Space in the nodes.** `kind load` puts a copy of each image in every
  node, and each node's `/var` takes about 390 MB at the start. On the
  default tmpfs (25 percent of the MicroVM's memory) three nodes fill it
  fast. A spec with more than one node should say `volumes: disk` or a
  larger `tmpfs:SIZE`. The hook checks the space before it loads the
  images, and fails with a message that says so.
- **The root disk.** A MicroVM's root disk is 10 GiB. The kind node image
  takes about 1 GiB. The archives and their copies in the nodes take the
  rest. `flr-bake` prints the total, so that a project sees it at build
  time.
- **A new digest in the Runner's configuration.** When the project's image
  changes, the Profile needs its new digest (CF-027). A tag works, with the
  warning that CF-027 gives, and then the Pool may boot an older image than
  the job's spec. The digest check in the job catches that.
- **The images under test.** The job still builds and loads them. A later
  step can avoid the load: the job pushes them to a registry on the Host,
  and the nodes pull from it.

## Not in this proposal

- The Runner reading the spec from the project's repository through
  GitLab's API, and rolling the Pool when it changes. That needs no image
  build, but a token and new Runner code. The spec format here works for it
  too, so it can come later.
- Snapshots of a ready cluster (#152).
- `flr/kind-ready` stays as it is, for jobs that are happy with the kind
  image's own cluster.

## Open questions

1. Is `.flr/kind-cluster.yaml` the right place for the spec?
2. Should `manifests` take kustomize directories or Helm charts, or only
   plain YAML? Plain YAML keeps `flr-bake` small: a project can render a
   chart to YAML and commit that.
3. Should a spec be able to load an image into some nodes only, to save
   space? kind's `load` takes `--nodes`.
4. Should `flr-bake` run outside a container build too, so that a project
   can try its spec on a laptop with kind?
