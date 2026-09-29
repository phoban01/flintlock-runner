#!/usr/bin/env bash
# Check the tags of the project's flr image (KF-145).
#
# usage:
#   check-tags.sh absent REPO:TAG...
#       Fail if any of the tags already exists. The release workflow runs it
#       before it pushes anything, so that a re-run never moves a tag.
#   check-tags.sh planned --version V [--snapshot] IMAGE...
#       Fail unless every IMAGE (repo:tag, as goreleaser records it in
#       dist/artifacts.json) is tagged exactly V. With --snapshot, the
#       per-platform suffix -amd64 or -arm64 of a snapshot build is allowed.
#   check-tags.sh published --version V --flr REPO@DIGEST
#       After a release pushed: every tag in the repository is a full
#       release version, none is latest or a floating major or minor, and
#       this release's tag names exactly the digest it pushed: flr:V.
#
# SKOPEO and REGISTRY_TLS_VERIFY are as for registry.sh.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=registry.sh
. "$here/registry.sh"

status=0
fail() { echo "check-tags: $*" >&2; status=1; }

cmd=${1:-}
[ $# -gt 0 ] && shift
version='' snapshot='' flr=''
if [ "$cmd" != absent ]; then
	while [ $# -gt 0 ]; do
		case $1 in
		--version) version=${2:?}; shift 2 ;;
		--snapshot) snapshot=1; shift ;;
		--flr) flr=${2:?}; shift 2 ;;
		--) shift; break ;;
		-*) echo "check-tags: unknown option $1" >&2; exit 2 ;;
		*) break ;;
		esac
	done
	if [ -z "$version" ]; then
		echo "check-tags: $cmd needs --version" >&2
		exit 2
	fi
fi

#= docs/requirements/12-cluster-fleet.md#cluster-release
#= type=test
#/ The Release SHALL NOT publish a `latest` tag or any other tag
#/ that moves.
case $cmd in
absent)
	[ $# -gt 0 ] || { echo "check-tags: absent needs REPO:TAG..." >&2; exit 2; }
	for ref in "$@"; do
		rc=0
		tag_exists "$ref" || rc=$?
		case $rc in
		0) fail "$ref already exists; a release tag is never moved, so cut a new version" ;;
		1) echo "check-tags: $ref does not exist yet" ;;
		*) fail "could not tell whether $ref exists" ;;
		esac
	done
	;;

planned)
	[ $# -gt 0 ] || { echo "check-tags: planned needs IMAGE..." >&2; exit 2; }
	for image in "$@"; do
		tag=${image##*:}
		if [ "$image" = "$tag" ] || [[ $tag == */* ]]; then
			fail "$image has no tag"
			continue
		fi
		if [ -n "$snapshot" ]; then
			tag=${tag%-amd64}
			tag=${tag%-arm64}
		fi
		if [ "$tag" = "$version" ]; then
			echo "check-tags: $image is tagged with the version only"
		else
			fail "$image is tagged ${image##*:}, want exactly $version"
		fi
	done
	;;

published)
	if [ -z "$flr" ]; then
		echo "check-tags: published needs --flr" >&2
		exit 2
	fi
	# Any tag other than a full release version would move or be ambiguous:
	# latest, 1, 1.2, v1.2.3, main, a commit.
	ok="^${release_version_re}\$"
	repo=${flr%@*}
	if tags=$(list_tags "$repo"); then
		while IFS= read -r t; do
			[ -n "$t" ] || continue
			[[ $t =~ $ok ]] || fail "$repo:$t is not a release version tag"
		done <<<"$tags"
		echo "check-tags: checked the $(grep -c . <<<"$tags") tag(s) of $repo"
	else
		fail "could not list the tags of $repo"
	fi
	ref=$repo:$version
	if got=$(manifest_digest "$ref"); then
		if [ "$got" = "${flr#*@}" ]; then
			echo "check-tags: $ref is $got"
		else
			fail "$ref is $got, want ${flr#*@}"
		fi
	else
		fail "$ref was not published"
	fi
	;;

*)
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; /^set -euo/d' >&2
	exit 2
	;;
esac
exit "$status"
