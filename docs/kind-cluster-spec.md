# A kind cluster ready at job start

An e2e job on `flr/kind` makes its kind cluster, then installs what its
tests need: cert-manager, a chart or two, and the project's own images.
That takes minutes before the first test runs.

A kind cluster spec moves that work to an image build. The project
describes its cluster in `.flr/kind-cluster.yaml`. It then builds its own
guest image, which holds the spec, every image the cluster needs and every
manifest it applies. battery makes the cluster when it creates each
MicroVM, before a job claims it. The job's first command finds the cluster
ready and returns at once.

This guide shows the steps for a project. `docs/docker-and-kind.md` has
the details of each step, and `docs/proposals/kind-cluster-spec.md` has
the design and the reasons for it.

## The steps

1. Write the spec, and check it with `flr-kind check`.
2. Build the project's guest image with `flr-bake`, and push it.
3. Add a Profile that boots the image and makes the cluster in its create
   hook.
4. Ask for the Profile in the job, and run `flr-kind-cluster --spec` before
   the tests.

## 1. Write the spec

A suite that needs one node and cert-manager has this spec:

```yaml
apiVersion: flintlock-runner.dev/v1alpha1
kind: KindCluster
name: my-operator-e2e
manifests:
  - url: https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml
    sha256: 0123...cdef
    wait:
      - deployment/cert-manager-webhook -n cert-manager
```

Give the cluster the name that the suite expects, so that the suite uses
the cluster that is there. Put the spec at `.flr/kind-cluster.yaml`. Paths
in the spec are relative to the spec's directory, and must stay inside
the repository. The repository root is the parent of `.flr`.

### The fields

| Field | Required | What |
|---|---|---|
| `apiVersion` | yes | `flintlock-runner.dev/v1alpha1` |
| `kind` | yes | `KindCluster` |
| `name` | yes | The kind cluster's name: at most 50 characters, from `a-z`, `0-9`, `.` and `-` |
| `volumes` | no | Where Docker Engine keeps its volumes: `tmpfs` (the default), `tmpfs:SIZE` such as `tmpfs:6G` or `tmpfs:50%`, or `disk` |
| `kindConfig` | no | A kind configuration, inline or as the path of a file. Without it, the cluster has one node. |
| `images` | no | Images to load into every node, each by digest |
| `manifests` | no | What to install, in order, once the nodes are Ready |

Each entry under `manifests` has one source, and an optional `wait` list:

| Source | Fields | Applied with |
|---|---|---|
| A file by URL | `url`, and `sha256`: the file's sha256 as 64 lowercase hexadecimal digits | `kubectl apply -f` |
| A file in the repository | `path` | `kubectl apply -f` |
| A kustomize directory | `kustomize`: the directory's path | `kubectl apply -k` |
| A Helm chart | `helm`: `chart`, `version`, `digest`, `release`, and optional `namespace` and `values` | `helm install` |

A `chart` is an `oci://` reference, the URL of a chart's archive (`.tgz`),
or `REPOSITORY_URL/CHART` for a chart in a chart repository, for example
`https://charts.jetstack.io/cert-manager`. Its `digest` is `sha256:` and
the sha256 of the chart's archive.
`values` is a list of values files in the repository.

Each item of `wait` is `KIND/NAME`, with `-n NAMESPACE` for a namespaced
object, for example `deployment/cert-manager -n cert-manager`. The next
entry starts only when every item is ready:

- A Deployment, DaemonSet or StatefulSet is ready when its rollout is
  done.
- A CustomResourceDefinition is ready when it is Established.
- Anything else is ready when its condition Ready is true.

Each item waits for up to 5 minutes. Set `KIND_WAIT`, for example
`KIND_WAIT=10m`, to change this.

### The rules

- Every image is named by digest, as `NAME:TAG@sha256:...`. This includes
  the images under `images` and each node's `image` in the kind
  configuration. A tag alone is refused.
- Do not list the images that the manifests and charts start. The build
  finds those itself. Under `images`, list the images that the suite
  deploys later, such as the project's dependencies.
- Do not add the registry mirror or the Docker socket to the kind
  configuration. `flr-kind-cluster` adds both.
- A kustomize directory with a remote base fetches that base at build
  time. Pin the base by a tag or a commit.

### More than one node

A larger cluster names a kind configuration and puts the volumes on the
disk:

```yaml
apiVersion: flintlock-runner.dev/v1alpha1
kind: KindCluster
name: battery-operator-e2e
volumes: disk
kindConfig: ../test/e2e/kind-config.yaml
images:
  - ghcr.io/liquidmetal-dev/poolmgrd:0.6.0@sha256:...
manifests:
  - url: https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml
    sha256: ...
    wait:
      - deployment/cert-manager -n cert-manager
      - deployment/cert-manager-cainjector -n cert-manager
      - deployment/cert-manager-webhook -n cert-manager
  - kustomize: ../test/e2e/dependencies
  - helm:
      chart: oci://ghcr.io/example/charts/thing
      version: 1.4.2
      digest: sha256:...
      release: thing
      namespace: thing
      values: [../test/e2e/thing-values.yaml]
    wait:
      - deployment/thing -n thing
```

On the default tmpfs there is room for one node. Each node takes about
390 MB before any image goes in, and each image goes into every node. For
more than one node, set `volumes: disk` or a larger `tmpfs:SIZE`.

### Check the spec

The `guest-baker` image has `flr-kind`. Run it from the repository root:

```sh
docker run --rm -v "$PWD:/src" -w /src \
  ghcr.io/phoban01/flintlock-runner/guest-baker@sha256:... \
  flr-kind check .flr/kind-cluster.yaml
```

`flr-kind check` names each field that is wrong, and exits 1. It does not
use the network. `flr-kind digest` prints the spec's digest: the sha256 of
the spec and of every file that it names by path. A change to any of
these files gives a new digest.

## 2. Build the guest image

Take the digests of the `guest-rootfs-kind` and `guest-baker` images from
one release's `images.txt`. Use the same release for both. Then add this
Containerfile as `.flr/Containerfile`:

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

Build and push it from the repository root, for the architectures of your
Hosts:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -f .flr/Containerfile \
  -t registry.example.com/my-project/e2e-guest:1 --push .
```

The bake stage needs the network. It downloads every image, file and
chart, and checks each digest. It renders each kustomize directory and
chart, and finds the images that they start. It runs on the build
machine's platform, so nothing runs under emulation.

`flr-bake` prints the size of what it wrote. A MicroVM's root disk is
10 GiB, and the kind image leaves about 5.9 GB of it free. The baked files
must fit in that.

An image that a manifest names by tag gets the digest that the tag has at
build time. `/etc/flr/images.lock` in the image shows which digest each
image got. To pin one, list it under `images` with the same name and tag,
and its digest.

Rebuild the image when the spec or a file that it names changes. Until
then, jobs still pass, but slowly: see step 4.

## 3. Add a Profile

Add a Profile to the runner's configuration. It boots the project's image,
and its create hook makes the cluster:

```yaml
- name: my-operator-e2e
  arch: amd64
  vcpu: 4
  memory_mb: 8192
  images: [flr/my-operator-e2e]
  kernel:
    image: ghcr.io/phoban01/flintlock-runner/guest-kernel@sha256:...
    filename: boot/vmlinux
  rootfs: registry.example.com/my-project/e2e-guest@sha256:...
  pool:
    size: 1
    create_hooks:
      - /usr/local/bin/flr-kind-cluster --spec /etc/flr/kind-cluster.yaml
```

Take the kernel's digest from the same release's `images.txt`. Name the
rootfs by the digest that the push printed.

The hook uses no network. It loads the baked images into every node,
installs each manifest and chart, and waits for each `wait` item. If a
step fails, battery replaces the MicroVM. `pool.size` is the number of
MicroVMs that battery keeps ready, each with a running cluster.

A kind guest keeps the memory it touches. To return it to the Host, see
[Host memory for kind guests](docker-and-kind.md#host-memory-for-kind-guests).

## 4. Use it in the job

Ask for the Profile's image, and run `flr-kind-cluster --spec` on the spec
in the checkout before the tests:

```yaml
e2e:
  image: flr/my-operator-e2e
  script:
    - flr-kind-cluster --spec .flr/kind-cluster.yaml
    - make test-e2e
```

`flr-kind-cluster --spec` compares the digest of the checkout's spec with
the digest of the spec that made the cluster:

- If they are the same, it writes the kubeconfig to `~/.kube/config` and
  returns.
- If they are different, it logs a warning with both digests, deletes the
  cluster and makes a new one from the checkout's spec. The job is slow,
  but it tests the cluster that its branch asks for. This happens on a
  branch that changes the spec, and after a change to the spec until the
  image is rebuilt.

Set `FLR_KIND_KUBECONFIG` to write the kubeconfig to another file. Don't
source `/etc/default/kind-bootstrap` in this job: it sets a `KUBECONFIG`
for the kind image's own `flr` cluster.

## Problems

| Problem | What to do |
|---|---|
| `flr-kind check` says an image `is not named by digest` | Add `@sha256:...` to the image. `docker buildx imagetools inspect IMAGE` prints the digest. |
| The hook says there is not enough space for the images | Set `volumes: disk`, or a larger `volumes: tmpfs:SIZE`. |
| Every job makes a new cluster | The image is older than the spec. Rebuild it, and put the new digest in the Profile. |
| A pod that names its image by digest pulls the image | An archive names its image by tag, so kind does not find it by digest (#169). The pod pulls it through the Host's registry mirror. |
| A wait times out in the hook | Set `KIND_WAIT` in the create hook, for example `KIND_WAIT=10m /usr/local/bin/flr-kind-cluster --spec ...`. |
