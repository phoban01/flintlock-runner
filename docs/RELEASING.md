# Releasing

flintlock-runner is released from tags on `main`. A tag of the form
`vX.Y.Z-rc.N` publishes a **release candidate** as a GitHub pre-release; a tag
of the form `vX.Y.Z` publishes a **release**. The release workflow,
`.github/workflows/release.yml`, does the rest, and refuses anything that does
not follow this procedure.

## What a release contains

For `linux/amd64`, `linux/arm64`, `darwin/amd64` and `darwin/arm64`:

| Archive | Contents |
|---|---|
| `flr_<version>_<os>_<arch>.tar.gz` | `flr`: the Runner, which a Control Node installs, and the `fleet` commands |
| `flintlock-devtools_<version>_<os>_<arch>.tar.gz` | `fake-poolmgr`, the stand-in Pool Manager an early fleet can run without battery (TD-009), and `flintlock-devstack`, the local demo stack |
| `flintlock-runner-fleet.yaml` | the Fleet Manifests: `kustomize build deploy/`, with every image of this project by the digest this release pushed |
| `flintlock-runner-capi.yaml` | the Cluster API objects of a cluster fleet: `kustomize build deploy/capi`, likewise by digest |
| `images.txt` | each image tag this release pushed, next to the digest it names |
| `checksums.txt` | SHA-256 of every archive, both manifests assets and `images.txt` |
| `requirements-report.html`, `requirements-report.json` | the duvet report for the tagged commit: every requirement in `docs/requirements/` and whether this build implements and tests it |

And two container images, in the project's registry
(`docs/requirements/12-cluster-fleet.md#cluster-release`):

| Image | Tags | Contents |
|---|---|---|
| `ghcr.io/phoban01/flintlock-runner/flr` | `<version>` | one index for `linux/amd64` and `linux/arm64`: the same `flr` binaries as the archives, the CA certificates and the time zone database, on `scratch`, running as uid 65532. No shell, no package manager, no `/etc/passwd` (KF-140, KF-141) |
| `ghcr.io/phoban01/flintlock-runner/host` | `<version>` and `<version>-k8s-<kubernetes version>` | the bootc Host Image of `image/`, `linux/amd64` only, the same manifest under both tags. The Kubernetes version is `KUBERNETES_VERSION` in `image/versions.env` (KF-142) |

`<version>` is the tag without its `v`: `v1.2.0-rc.1` publishes
`flr:1.2.0-rc.1`. No other tag is ever pushed: no `latest`, no `1` or `1.2`,
no bare Kubernetes version, because each of those would move from one
release to the next (KF-145). A tag, once pushed, is never pushed again: the
workflow refuses a version any of whose tags already exist. Use the digests
from the manifests or `images.txt` wherever a reference has to stay put;
the tags are for people.

The flr image's CA certificates and time zone data are copied from
`gcr.io/distroless/static-debian12:nonroot`, pinned by digest in
`hack/release/flr.Containerfile`; distroless itself is not the base because
it carries a passwd file, a dpkg database and more that KF-141 leaves out.

The Release does not publish an AMI. Making one needs an AWS account, which
the release workflow does not have, so an operator makes it from the
published Host Image in their own account (HI-009, `image/README.md`):

```sh
git checkout v0.1.0-rc.1          # image/versions.env must be the release's
digest=sha256:...                 # the host digest from images.txt
sudo podman pull "ghcr.io/phoban01/flintlock-runner/host@$digest"
make image-ami HOST_IMAGE="ghcr.io/phoban01/flintlock-runner/host@$digest" \
  IMAGE_AMI_ARGS="--bucket my-import-bucket --region eu-west-1"
```

bootc-image-builder reads the image from root's podman storage, hence
`sudo`. The AMI is tagged with the image digest and the Kubernetes version,
and a MachineDeployment has to ask for that Kubernetes version.

Up to `v1.0.0-rc.1` the Runner's archive and binary were called
`flintlock-runner`; from then on they are `flr`. The configuration, paths
and identifiers keep the `flintlock-runner` name.

Binaries are static (`CGO_ENABLED=0`), built with `-trimpath`, stamped with
the version (`flr --version`), and carry the commit's timestamp
rather than the build's, so the same tag builds the same bytes.

### On macOS

The darwin builds are for operating a fleet from a Mac: `config show` and
the `fleet` commands that reach Hosts over SSH or Systems Manager. Two things
still need Linux. `fleet provision` installs the Pool Manager daemon on the
machine it runs on, as a systemd service, so run it on the Control Node.
And the Hosts themselves need Linux with KVM. The unit tests run on macOS on
every pull request that changes the packaging; the end-to-end harness runs
on Linux only.

The binaries are not signed or notarized. A file downloaded with a browser
carries the quarantine attribute, which Gatekeeper refuses to run; clear it
with `xattr -d com.apple.quarantine flr`. A file fetched with
`curl` is not quarantined.

## Cutting a release candidate

A release candidate is how a version gets tried before it is called done.

1. Make sure the commit you want is on `main` and its CI is green.
2. Tag it and push the tag:

   ```sh
   git fetch origin
   git tag -a v0.1.0-rc.1 origin/main -m "v0.1.0-rc.1"
   git push origin v0.1.0-rc.1
   ```

3. The workflow checks the tag, reruns `go vet`, the race-enabled tests and the
   end-to-end harness on the tagged commit, then publishes, in this order:
   - **the Host Image** (job `host image`): `make image` and
     `make image-check` with podman, exactly as CI's `image` job builds it,
     then `hack/release/push-host-image.sh` pushes it under both of its tags;
   - **the flr image and the archives** (job `publish`): goreleaser builds
     the binaries once, archives them, builds the image for both platforms
     from them, pushes it, and creates the GitHub release as a *draft*;
   - **the manifests assets**: `hack/release/render-manifests.sh` renders
     `deploy/` and `deploy/capi` through a throwaway overlay that sets every
     image of this project to the digest just pushed, and refuses any
     reference that is still by tag. They are attached with `images.txt`
     and the extended `checksums.txt`, and only then is the draft
     published as a pre-release.

   Before the release is published, the workflow checks the pushed flr
   image (`hack/release/check-flr-image.sh`: both platforms, nothing but
   flr, certificates and time zone data, non-root, no shell) and every tag in
   both repositories (`hack/release/check-tags.sh`).

A release candidate publishes images too, so that a candidate can be tried
on a cluster before its version is released. Cut `-rc.2`, `-rc.3` and so on
from later commits as fixes land. A candidate ships whatever requirement
coverage its commit has.

If the workflow fails after it pushed an image, the pushed tags stay and the
draft release stays a draft. Re-running it fails at "no tag of this version
exists yet", on purpose: rather than re-point a tag, delete the draft
release and cut the next candidate.

### The first time

GitHub creates the two packages on the first push, linked to this
repository. Check in the packages' settings that they are public and that
this repository's workflows have write access; a package created private
has to be made public once by hand before anyone can pull without logging
in.

## Cutting a release

A release is a milestone. Two things are required that a candidate does not
need, and the workflow enforces both:

- **A release candidate for the same version must already exist.** Tagging
  `v0.1.0` fails unless some `v0.1.0-rc.N` tag does.
- **The committed requirements snapshot must be current.** The workflow runs
  `make duvet-ci`, which fails if `.duvet/snapshot.txt` differs from what the
  tagged commit produces. So before tagging, commit the milestone snapshot
  through a pull request whose body contains a line reading
  `Snapshot: milestone` (CI rejects a snapshot change without it):

  ```sh
  git checkout -b lead/snapshot-v0.1.0 origin/main
  make duvet
  git commit -am "Requirements snapshot for v0.1.0"
  # open the PR with "Snapshot: milestone" in its body, merge it, then:
  git fetch origin
  git tag -a v0.1.0 origin/main -m "v0.1.0"
  git push origin v0.1.0
  ```

## What the workflow refuses

| Condition | Why |
|---|---|
| a tag that is neither `vX.Y.Z-rc.N` nor `vX.Y.Z` | nothing else is a version this project publishes |
| a tag on a commit that is not on `main` | releases are only ever built from reviewed, merged code |
| `vX.Y.Z` with no `vX.Y.Z-rc.N` before it | every release is tried as a candidate first |
| `vX.Y.Z` with a stale requirements snapshot | the snapshot is the milestone record of what the release implements |
| any failing test, including the end-to-end harness | a release is rebuilt from scratch, so it is retested on exactly what ships |
| a module graph that `go mod tidy` would change | the build must be of exactly what the tag points at |
| a version any of whose image tags already exists | a pushed tag never moves (KF-145) |
| a Host Image that fails its check stage or its label check | the image that is pushed is the one that was checked |
| an flr image with anything but flr, certificates and time zone data in it, a root or non-numeric user, or a platform missing | KF-140, KF-141 |
| a tag in either repository that is not a full release version, `latest` included | KF-145 |
| a manifests asset with a reference to this project's images that is not by this release's digest, or a fleet asset without the flr image | KF-143 |

Only the jobs that push, `host image` and `publish`, hold `packages: write`.

### The packaging dry run

A pull request that changes `.goreleaser.yaml`, the release workflow,
`hack/release/` or `deploy/` runs the whole of the above without publishing
anything:

- the Host Image is built and checked as on a tag, and pushed to a registry
  that exists only inside the job, where its tags are checked;
- goreleaser builds every archive and the flr image for both platforms with
  `--snapshot`; each platform's image is checked, then pushed as one index
  to a registry inside the job and checked again the way a release checks
  the published one;
- `hack/release/test-render.sh` tests rendering and its checks against the
  stand-in kustomizations in `hack/release/testdata/`, and the real
  `deploy/` is rendered against the digests of those two throwaway pushes.
  The result is kept as the workflow artifact `manifests-dry-run`; its
  digests do not exist in ghcr.io. Until `deploy/kustomization.yaml`
  exists this step only warns, but a tag without it fails.

Locally, with goreleaser, Docker and kustomize installed:

```sh
goreleaser release --snapshot --clean
hack/release/check-flr-image.sh --version "$(jq -r .version dist/metadata.json)" \
  --platforms linux/amd64,linux/arm64 \
  $(jq -r '.[] | select(.type == "Docker Image") | .name' dist/artifacts.json)
hack/release/test-render.sh
```

## Checking a download

```sh
sha256sum --check --ignore-missing checksums.txt
tar -xzf flr_0.1.0-rc.1_linux_amd64.tar.gz
./flr --version
```

## Checking the images

`checksums.txt` covers the manifests assets and `images.txt`, so check those
first as above. Then confirm that the registry serves the digests they name,
with `skopeo` (or `crane digest`):

```sh
cat images.txt
# ghcr.io/phoban01/flintlock-runner/flr:0.1.0-rc.1 ghcr.io/phoban01/flintlock-runner/flr@sha256:...

# the digest of a manifest is the sha256 of its bytes, so this recomputes it
skopeo inspect --raw docker://ghcr.io/phoban01/flintlock-runner/flr:0.1.0-rc.1 | sha256sum
skopeo inspect --raw docker://ghcr.io/phoban01/flintlock-runner/host:0.1.0-rc.1 | sha256sum

# every reference to this project's images in the manifests, all by digest
grep -ho 'ghcr.io/phoban01/flintlock-runner/[^"[:space:]]*' \
  flintlock-runner-fleet.yaml flintlock-runner-capi.yaml | sort | uniq -c
```

`hack/release/check-manifests.sh --flr <ref> --host <ref> FILE...` from a
checkout does the last check strictly. The flr image's contents can be
checked from a checkout too:

```sh
hack/release/check-flr-image.sh --version 0.1.0-rc.1 \
  --platforms linux/amd64,linux/arm64 \
  --pull ghcr.io/phoban01/flintlock-runner/flr@sha256:<digest from images.txt>
```

## Not yet done

- Nothing is signed: not the archives, not the images. Signing with
  Sigstore's keyless `cosign` is the natural next step and needs only an
  `id-token: write` permission, a `signs` and a `docker_signs` section in
  `.goreleaser.yaml`, and a `cosign sign` of the Host Image by digest.
- No AMI is published; an operator makes one with `make image-ami` (above).
- The image pushes and the release publication have not run yet: before the
  first candidate that has them, only the dry run has.
