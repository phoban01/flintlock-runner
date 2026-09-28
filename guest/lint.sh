#!/usr/bin/env bash
# Lint the Guest Image's sources without building them: every download is
# checked against a recorded sha256, and every base image is pinned by
# digest. `make guest-lint` runs it after shellcheck. Every failure is
# reported before it exits non-zero.
#
# usage: guest/lint.sh [GUEST_DIR]
set -uo pipefail

dir=${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}
# code FILE: FILE without its comment lines.
code() { grep -vE '^[[:space:]]*#' "$1"; }

sources=("$dir"/kernel/Containerfile "$dir"/rootfs/Containerfile "$dir"/rootfs/*.sh "$dir"/kernel/*.sh)

#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Image build SHALL verify the checksum of every file
#/ it downloads against a checksum recorded in the Guest Image's sources and
#/ SHALL fail when one does not match.
#
# A download is curl or wget writing a file. Each file that downloads has to
# check what it wrote with `sha256sum -c` after the download, and every
# checksum it records has to be a sha256. Go modules are not downloads of
# this kind: the go command checks them against go.sum and the checksum
# database.
for f in "${sources[@]}"; do
	[ -f "$f" ] || continue
	rel=${f#"$dir"/}
	if code "$f" | grep -qE '\b(wget|curl)\b.*(-o|-O)'; then
		dl=$(code "$f" | grep -nE '\b(wget|curl)\b.*(-o|-O)' | tail -1 | cut -d: -f1)
		chk=$(code "$f" | grep -nE 'sha256sum -c' | tail -1 | cut -d: -f1)
		if [ -n "$chk" ] && [ "$chk" -gt "$dl" ]; then
			ok "$rel checks its downloads with sha256sum -c"
		else
			fail "$rel downloads a file and does not check its sha256 after it"
		fi
		if code "$f" | grep -qE '\bsha256sum -c\b.*\|\|'; then
			fail "$rel ignores a failed checksum"
		fi
	fi
	while IFS= read -r line; do
		name=${line%%=*} value=${line#*=}
		if [[ $value =~ ^[0-9a-f]{64}$ ]]; then
			ok "$rel records $name as a sha256"
		else
			fail "$rel records $name as \"$value\", not a sha256"
		fi
	done < <(code "$f" | sed -nE 's/^ARG ([A-Z0-9_]*SHA256[A-Z0-9_]*)=?(.*)$/\1=\2/p')
done

# Two builds of one commit start from the same base images.
for f in "$dir"/*/Containerfile; do
	rel=${f#"$dir"/}
	while IFS= read -r from; do
		image=$(awk '{for (i = 2; i <= NF; i++) if ($i !~ /^--/) { print $i; exit }}' <<<"$from")
		case $image in
		scratch) ok "$rel: FROM scratch" ;;
		*@sha256:*)
			if [[ ${image#*@sha256:} =~ ^[0-9a-f]{64}$ ]]; then ok "$rel: ${image%@*} is pinned by digest"; else fail "$rel: $image has a malformed digest"; fi
			;;
		*)
			# A stage of the same file is not a base image.
			if code "$f" | grep -qiE "^FROM .* AS $image\$"; then
				ok "$rel: FROM stage $image"
			else
				fail "$rel: $image is not pinned by digest"
			fi
			;;
		esac
	done < <(code "$f" | grep -E '^FROM ')
done

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "every check passed"
