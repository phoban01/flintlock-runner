#!/usr/bin/env bash
# Build the Guest Image (docs/requirements/13-guest-image.md): the kernel
# image of guest/kernel, the root filesystem image of guest/rootfs, and the
# Docker and kind images of guest/rootfs-docker and guest/rootfs-kind, each
# with its check stage, for every platform asked for.
#
# usage: guest/build.sh [--platforms LIST] [--load]
#        guest/build.sh [--platforms LIST] --push PREFIX --version V [--refs FILE]
#
#   --platforms LIST  comma-separated, default linux/amd64,linux/arm64
#   --load            load the images into Docker as localhost/flr-guest-kernel:dev,
#                     localhost/flr-guest-rootfs:dev, localhost/flr-guest-rootfs-docker:dev
#                     and localhost/flr-guest-rootfs-kind:dev; one platform only
#   --push PREFIX     push PREFIX/guest-kernel:V, PREFIX/guest-rootfs:V,
#                     PREFIX/guest-rootfs-docker:V and PREFIX/guest-rootfs-kind:V,
#                     each as one index for every platform, for example
#                     PREFIX=ghcr.io/phoban01/flintlock-runner
#   --version V       the release version without its v, e.g. 1.2.3-rc.1
#   --refs FILE       where REPOSITORY@sha256:... of each pushed image is
#                     written, one per line, in the order above
#
# Without --load or --push the images stay in the builder's cache: the
# build and its checks run, and nothing leaves the machine.
#
# The images build in layers: the Docker image starts FROM the root
# filesystem image of the same build, and the kind image FROM the Docker
# image. Each lower image is also written as an OCI layout in a temporary
# directory and passed to the next build as a named build context. That
# needs a builder with the OCI exporter and OCI layout contexts, such as
# the docker-container driver that hack/release/setup-buildx.sh makes.
#
# It runs from the repository root and uses `docker buildx`. BUILDX_BUILDER
# names the builder; a platform other than the builder's own needs QEMU
# emulation (hack/release/setup-buildx.sh installs it). --push needs skopeo
# and jq (hack/release/registry.sh); REGISTRY_TLS_VERIFY=false is for a plain-HTTP
# registry on localhost.
set -euo pipefail

usage() {
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; /^set -euo/d' >&2
	exit 2
}

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/.." && pwd)

#= docs/requirements/13-guest-image.md#guest-build
#/ The Guest Image SHALL consist of a kernel image and a root
#/ filesystem image, each built for `linux/amd64` and `linux/arm64`.
platforms=linux/amd64,linux/arm64
load='' prefix='' version='' refs=''
while [ $# -gt 0 ]; do
	case $1 in
	--platforms) platforms=${2:?}; shift 2 ;;
	--load) load=1; shift ;;
	--push) prefix=${2:?}; shift 2 ;;
	--version) version=${2:?}; shift 2 ;;
	--refs) refs=${2:?}; shift 2 ;;
	*) usage ;;
	esac
done
if [ -n "$load" ] && [ -n "$prefix" ]; then
	echo "guest/build.sh: --load and --push do not go together" >&2
	exit 2
fi
if [ -n "$load" ] && [[ $platforms == *,* ]]; then
	echo "guest/build.sh: --load takes one platform, not $platforms" >&2
	exit 2
fi

# The helper is built from the gitlab-runner version go.mod pins; the rootfs
# build checks that go.mod agrees and puts the version on a label.
gitlab_runner_version=$(sed -n 's#^[[:space:]]*gitlab.com/gitlab-org/gitlab-runner \(v[^[:space:]]*\).*#\1#p' "$root/go.mod")
if ! [[ $gitlab_runner_version =~ ^v[0-9] ]]; then
	echo "guest/build.sh: no gitlab.com/gitlab-org/gitlab-runner version in go.mod" >&2
	exit 1
fi

# Every image this script builds, in the order it builds them.
names=(guest-kernel guest-rootfs guest-rootfs-docker guest-rootfs-kind)

if [ -n "$prefix" ]; then
	# shellcheck source-path=SCRIPTDIR source=../hack/release/registry.sh
	. "$root/hack/release/registry.sh"
	if ! [[ $version =~ ^${release_version_re}$ ]]; then
		echo "guest/build.sh: --push needs --version, a release version (1.2.3 or 1.2.3-rc.4), not \"$version\"" >&2
		exit 2
	fi
	# A release tag is never moved: refuse before anything is built.
	for name in "${names[@]}"; do
		rc=0
		tag_exists "$prefix/$name:$version" || rc=$?
		case $rc in
		0) echo "guest/build.sh: $prefix/$name:$version already exists; a release tag is never moved, so cut a new version" >&2; exit 1 ;;
		1) ;;
		*) echo "guest/build.sh: could not tell whether $prefix/$name:$version exists" >&2; exit 1 ;;
		esac
	done
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
[ -z "$refs" ] || : >"$refs"

# build NAME CONTEXT CONTAINERFILE [OPTION...]
#
# OPTION is --arg NAME=VALUE, a build argument; --from NAME, an image this
# script built before, passed as the named build context NAME; or --base,
# which also writes this image as an OCI layout for a later --from.
build() {
	local name=$1 context=$2 file=$3
	shift 3
	local args=(--platform "$platforms" -f "$file" --provenance=false --metadata-file "$work/$name.json")
	local base='' ref
	while [ $# -gt 0 ]; do
		case $1 in
		--arg) args+=(--build-arg "$2"); shift 2 ;;
		--from)
			ref=$(layout_ref "$2")
			args+=(--build-context "$2=$ref")
			shift 2
			;;
		--base) base=1; shift ;;
		*) echo "guest/build.sh: build: unknown option $1" >&2; exit 2 ;;
		esac
	done
	local out=()
	if [ -n "$prefix" ]; then
		#= docs/requirements/13-guest-image.md#guest-release
		#/ The Release SHALL publish the Guest Image's kernel image and
		#/ root filesystem image to the project's container registry, each as one
		#/ image index for `linux/amd64` and `linux/arm64`, tagged with the release
		#/ version.
		#
		#= docs/requirements/13-guest-image.md#guest-release
		#/ The Release SHALL publish the Guest Image's Docker image and
		#/ kind image to the project's container registry, each as one image index
		#/ for `linux/amd64` and `linux/arm64`, tagged with the release version.
		out=(-t "$prefix/$name:$version" --label "org.opencontainers.image.version=$version"
			--output "type=image,push=true,oci-mediatypes=true")
	elif [ -n "$load" ]; then
		out=(-t "localhost/flr-$name:dev" --load)
	elif [ -z "$base" ]; then
		out=(--output type=cacheonly)
	fi
	echo "guest/build.sh: building $name for $platforms"
	# The OCI layout for a later --from is a build of its own: buildx does
	# not write it correctly beside a --load. The second build takes every
	# step from the cache.
	if [ -n "$base" ]; then
		docker buildx build "${args[@]}" --output "type=oci,dest=$work/layout/$name,tar=false" "$context"
	fi
	if [ ${#out[@]} -gt 0 ]; then
		docker buildx build "${args[@]}" "${out[@]}" "$context"
	fi
	if [ -n "$prefix" ]; then
		local digest got
		digest=$(jq -r '."containerimage.digest" // empty' "$work/$name.json")
		if ! [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
			echo "guest/build.sh: buildx reported no digest for $name" >&2
			exit 1
		fi
		got=$(manifest_digest "$prefix/$name:$version")
		if [ "$got" != "$digest" ]; then
			echo "guest/build.sh: $prefix/$name:$version is $got, not the pushed $digest" >&2
			exit 1
		fi
		echo "guest/build.sh: $prefix/$name@$digest tagged $version"
		[ -z "$refs" ] || printf '%s@%s\n' "$prefix/$name" "$digest" >>"$refs"
	fi
}

# layout_ref NAME prints the named build context of the OCI layout that
# build NAME ... --base wrote: the layout's one image, by digest.
layout_ref() {
	local dir=$work/layout/$1 digest
	digest=$(grep -oE '"digest":"sha256:[0-9a-f]{64}"' "$dir/index.json" 2>/dev/null | head -1 | cut -d'"' -f4)
	if ! [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
		echo "guest/build.sh: $1 was not written as an OCI layout in $dir" >&2
		exit 1
	fi
	echo "oci-layout://$dir@$digest"
}

cd "$root"
build guest-kernel guest/kernel guest/kernel/Containerfile
build guest-rootfs . guest/rootfs/Containerfile --arg "GITLAB_RUNNER_VERSION=$gitlab_runner_version" --base
#= docs/requirements/13-guest-image.md#guest-docker
#/ The Guest Image SHALL include a Docker image, a root filesystem
#/ image built on the Guest Image's root filesystem image for `linux/amd64`
#/ and `linux/arm64`, which carries everything this document requires of
#/ that image.
build guest-rootfs-docker guest guest/rootfs-docker/Containerfile --from guest-rootfs --base
#= docs/requirements/13-guest-image.md#guest-kind
#/ The Guest Image SHALL include a kind image, a root filesystem image
#/ built on its Docker image for `linux/amd64` and `linux/arm64`, which
#/ carries everything this document requires of the Docker image.
build guest-rootfs-kind guest guest/rootfs-kind/Containerfile --from guest-rootfs-docker
