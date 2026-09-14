# Releasing

flintlock-runner is released from tags on `main`. A tag of the form
`vX.Y.Z-rc.N` publishes a **release candidate** as a GitHub pre-release; a tag
of the form `vX.Y.Z` publishes a **release**. The release workflow,
`.github/workflows/release.yml`, does the rest, and refuses anything that does
not follow this procedure.

## What a release contains

For `linux/amd64` and `linux/arm64`:

| Archive | Contents |
|---|---|
| `flintlock-runner_<version>_linux_<arch>.tar.gz` | the Runner, which a Control Node installs |
| `flintlock-devtools_<version>_linux_<arch>.tar.gz` | `fake-poolmgr`, the stand-in Pool Manager an early fleet can run without battery (TD-009), and `flintlock-devstack`, the local demo stack |
| `checksums.txt` | SHA-256 of every archive |
| `requirements-report.html`, `requirements-report.json` | the duvet report for the tagged commit: every requirement in `docs/requirements/` and whether this build implements and tests it |

Binaries are static (`CGO_ENABLED=0`), built with `-trimpath`, stamped with
the version (`flintlock-runner --version`), and carry the commit's timestamp
rather than the build's, so the same tag builds the same bytes.

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
   end-to-end harness on the tagged commit, then publishes a pre-release with
   the archives, the checksums and the requirements report.

Cut `-rc.2`, `-rc.3` and so on from later commits as fixes land. A candidate
ships whatever requirement coverage its commit has.

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

A pull request that changes `.goreleaser.yaml` or the release workflow runs a
packaging dry run that builds every archive without publishing anything.

## Checking a download

```sh
sha256sum --check --ignore-missing checksums.txt
tar -xzf flintlock-runner_0.1.0-rc.1_linux_amd64.tar.gz
./flintlock-runner --version
```

## Not yet done

- The archives are not signed. Signing with Sigstore's keyless `cosign` is the
  natural next step and needs only an `id-token: write` permission and a
  `signs` section in `.goreleaser.yaml`.
- There is no container image.
- The repository has no licence file. Until one is added, anyone downloading a
  release has no licence to use it.
