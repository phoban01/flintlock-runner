#!/usr/bin/env bash
# Check the Guest Image as a release pushed it (GI-040).
#
# usage: check-guest-images.sh --version V REPO@DIGEST...
#
# For each image, kernel and root filesystem: REPO:V names DIGEST; DIGEST is
# one image index with exactly linux/amd64 and linux/arm64; and every tag of
# REPO is a full release version, so none is latest or moves.
#
# SKOPEO and REGISTRY_TLS_VERIFY are as for registry.sh.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=registry.sh
. "$here/registry.sh"

status=0
fail() { echo "check-guest-images: $*" >&2; status=1; }

version=''
while [ $# -gt 0 ]; do
	case $1 in
	--version) version=${2:?}; shift 2 ;;
	--) shift; break ;;
	-*) echo "check-guest-images: unknown option $1" >&2; exit 2 ;;
	*) break ;;
	esac
done
if [ -z "$version" ] || [ $# -eq 0 ]; then
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; /^set -euo/d' >&2
	exit 2
fi

#= docs/requirements/13-guest-image.md#guest-release
#= type=test
#/ The Release SHALL publish the Guest Image's kernel image and
#/ root filesystem image to the project's container registry, each as one
#/ image index for `linux/amd64` and `linux/arm64`, tagged with the release
#/ version.
#
#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Image SHALL consist of a kernel image and a root
#/ filesystem image, each built for `linux/amd64` and `linux/arm64`.
names=()
for ref in "$@"; do
	repo=${ref%@*} digest=${ref#*@}
	if [ "$repo" = "$ref" ] || ! [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
		fail "$ref is not REPO@sha256:DIGEST"
		continue
	fi
	names+=("${repo##*/}")

	got=$(manifest_digest "$repo:$version") || { fail "$repo:$version was not published"; continue; }
	if [ "$got" = "$digest" ]; then
		echo "check-guest-images: $repo:$version is $digest"
	else
		fail "$repo:$version is $got, want $digest"
	fi

	manifest=$(skopeo_cmd inspect "$(tls_flag tls-verify)" --raw "docker://$repo@$digest") || { fail "could not read $repo@$digest"; continue; }
	case $(jq -r '.mediaType // empty' <<<"$manifest") in
	application/vnd.oci.image.index.v1+json | application/vnd.docker.distribution.manifest.list.v2+json) ;;
	*) fail "$repo@$digest is not an image index"; continue ;;
	esac
	platforms=$(jq -r '[.manifests[] | .platform | "\(.os)/\(.architecture)"] | sort | join(",")' <<<"$manifest")
	if [ "$platforms" = "linux/amd64,linux/arm64" ]; then
		echo "check-guest-images: $repo@$digest is for $platforms"
	else
		fail "$repo@$digest is for \"$platforms\", want linux/amd64,linux/arm64"
	fi

	tags=$(list_tags "$repo") || { fail "could not list the tags of $repo"; continue; }
	while IFS= read -r t; do
		[ -n "$t" ] || continue
		[[ $t =~ ^${release_version_re}$ ]] || fail "$repo:$t is not a release version tag"
	done <<<"$tags"
done

# Both images, the kernel and the root filesystem.
if [ "$(printf '%s\n' "${names[@]}" | sort | xargs)" != "guest-kernel guest-rootfs" ]; then
	fail "want guest-kernel and guest-rootfs, got: ${names[*]}"
fi
exit "$status"
