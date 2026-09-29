#!/usr/bin/env bash
# Render the manifests asset of a release (docs/RELEASING.md):
#
#   flintlock-runner-fleet.yaml   kustomize build deploy/
#
# with every image of this project rewritten to the digest this release
# published. The kustomizations under deploy/ name the images by tag
# (ghcr.io/phoban01/flintlock-runner/flr:dev); this script never edits them.
# It builds deploy/ through a throwaway overlay whose only content is
# `kustomize edit set image`, then runs check-manifests.sh over the result,
# so an asset with a tag reference in it is never written.
#
# The Runner's Profiles name the Guest Image, guest-kernel and guest-rootfs,
# inside its configuration file, where kustomize's image transformer does
# not reach. With --images, the render first copies the kustomization root
# and writes the digests of the release's images.txt over every reference
# to those two images in the copy, so that the configuration's ConfigMap
# is generated with them.
#
# The Hosts boot battery-operator's Host Image, and battery-operator's
# Cluster API templates make them, so this project publishes neither.
#
# usage: render-manifests.sh --flr REF@sha256:...
#                            [--images FILE] [--deploy DIR] [--out DIR]
#
#   --flr          the published flr image, by digest
#   --images       the release's images.txt: one line per pushed tag, the
#                  tag and then the same image by digest. The guest-kernel
#                  and guest-rootfs digests are taken from it
#   --deploy       the kustomization root, default deploy
#   --out          where the file goes, default dist
#
# KUSTOMIZE names the kustomize binary (default kustomize).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
kustomize=${KUSTOMIZE:-kustomize}
deploy=deploy
out=dist
flr=
images=

usage() { sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; /^set -euo/d' >&2; exit 2; }

while [ $# -gt 0 ]; do
	case $1 in
	--flr) flr=${2:?}; shift 2 ;;
	--images) images=${2:?}; shift 2 ;;
	--deploy) deploy=${2:?}; shift 2 ;;
	--out) out=${2:?}; shift 2 ;;
	-h | --help) usage ;;
	*) echo "render-manifests: unknown argument $1" >&2; usage ;;
	esac
done

# A reference by digest, never by tag: registry/repository@sha256:<64 hex>.
by_digest='^[a-z0-9.:-]+(/[a-z0-9._-]+)+@sha256:[0-9a-f]{64}$'
if [ -z "$flr" ]; then
	echo "render-manifests: --flr is required" >&2
	exit 2
fi
if ! [[ $flr =~ $by_digest ]]; then
	echo "render-manifests: --flr $flr is not a reference by digest (REPO@sha256:<64 hex>)" >&2
	exit 2
fi

# The names the images carry in deploy/, which are the published
# repositories; kustomize matches on the name and replaces the tag with the
# digest. Every image of this project is listed here.
prefix=ghcr.io/phoban01/flintlock-runner
flr_name=$prefix/flr
guest_names=("$prefix/guest-kernel" "$prefix/guest-rootfs")
if [ "${flr%@*}" != "$flr_name" ]; then
	echo "render-manifests: --flr must be in $flr_name, not ${flr%@*}" >&2
	exit 2
fi

# The Guest Image by digest, from the release's images.txt: for each
# repository, exactly one digest, whatever tags point at it.
guest_refs=()
if [ -n "$images" ]; then
	if [ ! -s "$images" ]; then
		echo "render-manifests: --images $images is missing or empty" >&2
		exit 2
	fi
	for name in "${guest_names[@]}"; do
		mapfile -t found < <(awk -v n="$name@" 'index($2, n) == 1 { print $2 }' "$images" | sort -u)
		if [ "${#found[@]}" -ne 1 ] || ! [[ ${found[0]} =~ $by_digest ]]; then
			echo "render-manifests: $images names ${#found[@]} reference(s) by digest to $name, not one: ${found[*]}" >&2
			exit 2
		fi
		guest_refs+=("${found[0]}")
	done
fi

deploy=$(cd "$deploy" && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

#= docs/requirements/12-cluster-fleet.md#cluster-release
#/ The Release SHALL publish the Fleet Manifests as one release asset
#/ for the workload cluster, in which every container image of this project
#/ is referenced by digest.
#
# The copy of the kustomization root that is rendered: deploy/ itself, with
# every reference to a Guest Image, by any tag or digest, replaced by the
# release's digest. Nothing under deploy/ changes.
src=$work/src
mkdir -p "$src"
cp -R "$deploy/." "$src/"
for i in "${!guest_refs[@]}"; do
	name=${guest_names[$i]}
	ref=${guest_refs[$i]}
	pattern="${name//./\\.}(:[A-Za-z0-9._-]+|@sha256:[0-9a-f]{64})"
	grep -rlE "$pattern" "$src" | while IFS= read -r file; do
		sed -E "s#$pattern#$ref#g" "$file" >"$file.render" && mv "$file.render" "$file"
	done
done

render() { # render <kustomization root> <asset name> [check-manifests options]
	local root=$1 asset=$2 overlay
	shift 2
	if [ ! -f "$root/kustomization.yaml" ]; then
		echo "render-manifests: $root has no kustomization.yaml" >&2
		return 1
	fi
	overlay=$work/${asset%.yaml}
	mkdir -p "$overlay"
	# A relative path: kustomize resolves resources against the overlay.
	(
		cd "$overlay"
		"$kustomize" create --resources "$(realpath --relative-to="$overlay" "$root")"
		"$kustomize" edit set image \
			"$flr_name=$flr"
	)
	"$kustomize" build "$overlay" >"$work/$asset"
	"$here/check-manifests.sh" --flr "$flr" "$@" "$work/$asset"
	mv "$work/$asset" "$out/$asset"
	echo "render-manifests: wrote $out/$asset"
}

guest_checks=()
for ref in "${guest_refs[@]}"; do
	guest_checks+=(--image "$ref")
done

# The fleet runs flr, so its asset has to reference the flr image.
render "$src" flintlock-runner-fleet.yaml --require-flr "${guest_checks[@]}"
