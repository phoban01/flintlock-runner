#!/usr/bin/env bash
# Build gitlab-runner-helper for the Guest Image's root filesystem, in the
# helper stage of guest/rootfs/Containerfile.
#
# usage: build-helper.sh MODULE_DIR OUT_DIR
#
# MODULE_DIR holds this repository's go.mod and go.sum, and nothing else.
# The helper is built from the gitlab-runner module at the version go.mod
# pins, as `go build` resolves it for flr, so the helper and the Runner come
# from one commit. go.sum has no hashes for the few modules only the helper
# imports, because `go mod tidy` keeps only those the Runner's packages
# need. -mod=mod lets the build add them, and the go command checks each
# against the checksum database (sum.golang.org) before it uses it.
#
# TARGETARCH is the architecture to build for; GITLAB_RUNNER_VERSION is the
# version guest/build.sh read from go.mod for the image's label, and the
# build fails when go.mod says otherwise.
set -euo pipefail

module_dir=${1:?usage: build-helper.sh MODULE_DIR OUT_DIR}
out=${2:?usage: build-helper.sh MODULE_DIR OUT_DIR}
: "${TARGETARCH:?}" "${GITLAB_RUNNER_VERSION:?}"

cd "$module_dir"
pkg=gitlab.com/gitlab-org/gitlab-runner
version=$(GOFLAGS=-mod=mod go list -m -f '{{.Version}}' "$pkg")
if [ "$version" != "$GITLAB_RUNNER_VERSION" ]; then
	echo "build-helper: go.mod pins $pkg $version, not $GITLAB_RUNNER_VERSION" >&2
	exit 1
fi
# A pseudo-version ends in the commit's first 12 hex digits; a release
# version names its own tag.
revision=${version##*-}
[[ $revision =~ ^[0-9a-f]{12}$ ]] || revision=$version

#= docs/requirements/13-guest-image.md#guest-rootfs
#/ The Guest Image SHALL contain `gitlab-runner-helper` at
#/ `/usr/local/bin/gitlab-runner-helper`, built for the image's architecture
#/ from the gitlab-runner module at the version that `go.mod` pins.
mkdir -p "$out/usr/local/bin"
CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOFLAGS=-mod=mod \
	go build -trimpath \
	-ldflags "-s -w -X $pkg/common.NAME=gitlab-runner-helper -X $pkg/common.VERSION=$version -X $pkg/common.REVISION=$revision" \
	-o "$out/usr/local/bin/gitlab-runner-helper" "$pkg/apps/gitlab-runner-helper"
chmod 0755 "$out/usr/local/bin/gitlab-runner-helper"
echo "build-helper: built gitlab-runner-helper $version ($revision) for linux/$TARGETARCH"
