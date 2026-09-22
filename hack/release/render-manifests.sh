#!/usr/bin/env bash
# Render the two manifests assets of a release (docs/RELEASING.md):
#
#   flintlock-runner-fleet.yaml   kustomize build deploy/
#   flintlock-runner-capi.yaml    kustomize build deploy/capi
#
# with every image of this project rewritten to the digest this release
# published. The kustomizations under deploy/ name the images by tag
# (ghcr.io/phoban01/flintlock-runner/flr:dev); this script never edits them.
# It builds each one through a throwaway overlay whose only content is
# `kustomize edit set image`, then runs check-manifests.sh over the result,
# so an asset with a tag reference in it is never written.
#
# usage: render-manifests.sh --flr REF@sha256:... --host REF@sha256:...
#                            [--deploy DIR] [--out DIR]
#
#   --flr, --host  the published flr image and Host Image, by digest
#   --deploy       the kustomization root, default deploy (its capi/
#                  subdirectory is the Cluster API root)
#   --out          where the two files go, default dist
#
# KUSTOMIZE names the kustomize binary (default kustomize).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
kustomize=${KUSTOMIZE:-kustomize}
deploy=deploy
out=dist
flr=
host=

usage() { sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; /^set -euo/d' >&2; exit 2; }

while [ $# -gt 0 ]; do
	case $1 in
	--flr) flr=${2:?}; shift 2 ;;
	--host) host=${2:?}; shift 2 ;;
	--deploy) deploy=${2:?}; shift 2 ;;
	--out) out=${2:?}; shift 2 ;;
	-h | --help) usage ;;
	*) echo "render-manifests: unknown argument $1" >&2; usage ;;
	esac
done

# A reference by digest, never by tag: registry/repository@sha256:<64 hex>.
by_digest='^[a-z0-9.:-]+(/[a-z0-9._-]+)+@sha256:[0-9a-f]{64}$'
for pair in "flr=$flr" "host=$host"; do
	ref=${pair#*=}
	if [ -z "$ref" ]; then
		echo "render-manifests: --${pair%%=*} is required" >&2
		exit 2
	fi
	if ! [[ $ref =~ $by_digest ]]; then
		echo "render-manifests: --${pair%%=*} $ref is not a reference by digest (REPO@sha256:<64 hex>)" >&2
		exit 2
	fi
done

# The names the images carry in deploy/, which are the published
# repositories; kustomize matches on the name and replaces the tag with the
# digest. Every image of this project is listed here.
flr_name=ghcr.io/phoban01/flintlock-runner/flr
host_name=ghcr.io/phoban01/flintlock-runner/host
if [ "${flr%@*}" != "$flr_name" ]; then
	echo "render-manifests: --flr must be in $flr_name, not ${flr%@*}" >&2
	exit 2
fi
if [ "${host%@*}" != "$host_name" ]; then
	echo "render-manifests: --host must be in $host_name, not ${host%@*}" >&2
	exit 2
fi

deploy=$(cd "$deploy" && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

#= docs/requirements/12-cluster-fleet.md#cluster-release
#/ The Release SHALL publish the Fleet Manifests as two release
#/ assets, one for the workload cluster and one for the Cluster API objects
#/ of the management cluster, in which every container image of this project
#/ is referenced by
#/ digest.
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
			"$flr_name=$flr" \
			"$host_name=$host"
	)
	"$kustomize" build "$overlay" >"$work/$asset"
	"$here/check-manifests.sh" --flr "$flr" --host "$host" "$@" "$work/$asset"
	mv "$work/$asset" "$out/$asset"
	echo "render-manifests: wrote $out/$asset"
}

# The fleet runs flr, so its asset has to reference the flr image; the
# Cluster API objects reference the Host Image only where they name one.
render "$deploy" flintlock-runner-fleet.yaml --require-flr
render "$deploy/capi" flintlock-runner-capi.yaml
