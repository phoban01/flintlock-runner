#!/usr/bin/env bash
# Publish the Host Image that `make image` built and checked (KF-142).
#
# usage: push-host-image.sh LOCAL_IMAGE REPOSITORY VERSION [DIGEST_FILE]
#
#   LOCAL_IMAGE  the built image, e.g. localhost/flintlock-runner-host:dev
#   REPOSITORY   e.g. ghcr.io/phoban01/flintlock-runner/host
#   VERSION      the release version without its v, e.g. 1.2.3-rc.1
#   DIGEST_FILE  where REPOSITORY@sha256:... is written
#
# It pushes two tags, VERSION and VERSION-k8s-<KUBERNETES_VERSION>, the
# latter read from image/versions.env, both naming the same manifest. Both
# are specific to this release, so neither ever moves: the script refuses
# when either already exists, rather than re-point it at a new build.
#
# CONTAINER_ENGINE (podman or docker, as for `make image`) says where
# LOCAL_IMAGE is. The pushing is done by skopeo; see registry.sh.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=registry.sh
. "$here/registry.sh"

if [ $# -lt 3 ] || [ $# -gt 4 ]; then
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; /^set -euo/d' >&2
	exit 2
fi
local_image=$1 repo=$2 version=$3 digest_file=${4:-}

if ! [[ $version =~ ^${release_version_re}$ ]]; then
	echo "push-host-image: $version is not a release version (1.2.3 or 1.2.3-rc.4)" >&2
	exit 2
fi
k8s=$(kubernetes_version)

case $(basename "${CONTAINER_ENGINE:-podman}") in
podman) source_ref=containers-storage:$local_image ;;
docker) source_ref=docker-daemon:$local_image ;;
*) echo "push-host-image: CONTAINER_ENGINE must be podman or docker" >&2; exit 2 ;;
esac

#= docs/requirements/12-cluster-fleet.md#cluster-release
#/ The Release SHALL publish the Host Image to the project's
#/ container registry, tagged with the release version and with the release
#/ version joined to the Kubernetes version it carries.
tags=("$version" "$version-k8s-$k8s")

#= docs/requirements/12-cluster-fleet.md#cluster-release
#/ The Release SHALL NOT publish a `latest` tag or any other tag
#/ that moves.
for tag in "${tags[@]}"; do
	rc=0
	tag_exists "$repo:$tag" || rc=$?
	case $rc in
	0) echo "push-host-image: $repo:$tag already exists; a release tag is never moved, so cut a new version" >&2; exit 1 ;;
	1) ;;
	*) echo "push-host-image: could not tell whether $repo:$tag exists" >&2; exit 1 ;;
	esac
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "push-host-image: $local_image -> $repo:${tags[0]}"
skopeo_cmd copy "$(tls_flag dest-tls-verify)" --digestfile "$work/digest" \
	"$source_ref" "docker://$repo:${tags[0]}"
digest=$(cat "$work/digest")
if ! [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
	echo "push-host-image: skopeo reported digest \"$digest\"" >&2
	exit 1
fi

# The second tag is the same manifest, copied within the registry by
# digest, so it cannot name a different build.
for tag in "${tags[@]:1}"; do
	echo "push-host-image: $repo@$digest -> $repo:$tag"
	skopeo_cmd copy "$(tls_flag src-tls-verify)" "$(tls_flag dest-tls-verify)" --preserve-digests \
		"docker://$repo@$digest" "docker://$repo:$tag"
done

for tag in "${tags[@]}"; do
	got=$(manifest_digest "$repo:$tag")
	if [ "$got" != "$digest" ]; then
		echo "push-host-image: $repo:$tag is $got, not the pushed $digest" >&2
		exit 1
	fi
done

echo "push-host-image: $repo@$digest tagged ${tags[*]}"
if [ -n "$digest_file" ]; then
	printf '%s@%s\n' "$repo" "$digest" >"$digest_file"
fi
