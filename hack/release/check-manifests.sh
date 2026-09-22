#!/usr/bin/env bash
# Check a rendered manifests asset: every reference to an image of this
# project (anything under ghcr.io/phoban01/flintlock-runner/) is by digest,
# and the flr and Host Image references are to exactly the digests of this
# release. A tag, a missing digest or another release's digest fails.
#
# usage: check-manifests.sh [--flr REF@DIGEST] [--host REF@DIGEST]
#                           [--require-flr] FILE...
#
# --require-flr fails a file that does not reference the --flr image at all,
# which the fleet asset must: the Runner and the Host Agent run it.
#
# render-manifests.sh runs it on every asset before writing it, and the
# release workflow runs it again on the files it uploads.
set -euo pipefail

flr=
host=
require_flr=
while [ $# -gt 0 ]; do
	case $1 in
	--flr) flr=${2:?}; shift 2 ;;
	--host) host=${2:?}; shift 2 ;;
	--require-flr) require_flr=1; shift ;;
	--) shift; break ;;
	-*) echo "check-manifests: unknown option $1" >&2; exit 2 ;;
	*) break ;;
	esac
done
if [ $# -eq 0 ]; then
	echo "usage: check-manifests.sh [--flr REF@DIGEST] [--host REF@DIGEST] FILE..." >&2
	exit 2
fi

project=ghcr.io/phoban01/flintlock-runner/
by_digest='^ghcr\.io/phoban01/flintlock-runner/[a-z0-9._-]+(/[a-z0-9._-]+)*@sha256:[0-9a-f]{64}$'

#= docs/requirements/12-cluster-fleet.md#cluster-release
#= type=test
#/ The Release SHALL publish the Fleet Manifests as one release
#/ asset in which every container image of this project is referenced by
#/ digest.
status=0
for file in "$@"; do
	if [ ! -s "$file" ]; then
		echo "check-manifests: $file is missing or empty" >&2
		status=1
		continue
	fi
	# Every occurrence of the project's registry path, wherever it is: an
	# image field, an annotation, an argument or a comment. A reference ends
	# at whitespace, a quote, a comma or a closing bracket.
	refs=$(grep -oE "${project//./\\.}[^]\"',}[:space:]]*" "$file" || true)
	before=$status
	count=0
	found_flr=
	while IFS= read -r ref; do
		[ -n "$ref" ] || continue
		count=$((count + 1))
		if ! [[ $ref =~ $by_digest ]]; then
			echo "check-manifests: $file: $ref is not a reference by digest" >&2
			status=1
			continue
		fi
		[ "$ref" != "$flr" ] || found_flr=1
		for want in "$flr" "$host"; do
			[ -n "$want" ] || continue
			if [ "${ref%@*}" = "${want%@*}" ] && [ "$ref" != "$want" ]; then
				echo "check-manifests: $file: $ref is not this release's $want" >&2
				status=1
			fi
		done
	done <<<"$refs"
	if [ -n "$require_flr" ] && [ -z "$found_flr" ]; then
		echo "check-manifests: $file does not reference ${flr:-the flr image (--require-flr needs --flr)}" >&2
		status=1
	fi
	if [ "$status" = "$before" ]; then
		echo "check-manifests: $file: $count reference(s) to this project's images, all by this release's digests"
	fi
done
exit "$status"
