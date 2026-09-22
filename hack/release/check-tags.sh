#!/usr/bin/env bash
# Check the tags of the project's images (KF-142, KF-145).
#
# usage:
#   check-tags.sh absent REPO:TAG...
#       Fail if any of the tags already exists. The release workflow runs it
#       before it pushes anything, so that a re-run never moves a tag.
#   check-tags.sh planned --version V [--snapshot] IMAGE...
#       Fail unless every IMAGE (repo:tag, as goreleaser records it in
#       dist/artifacts.json) is tagged exactly V. With --snapshot, the
#       per-platform suffix -amd64 or -arm64 of a snapshot build is allowed.
#   check-tags.sh published --version V [--flr REPO@DIGEST] [--host REPO@DIGEST]
#       After a release pushed: every tag in both repositories is a full
#       release version (the Host Image's may carry -k8s-vX.Y.Z), none is
#       latest or a floating major or minor, and this release's tags name
#       exactly the digests it pushed: flr:V, host:V and
#       host:V-k8s-<KUBERNETES_VERSION of image/versions.env>.
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
version='' snapshot='' flr='' host=''
if [ "$cmd" != absent ]; then
	while [ $# -gt 0 ]; do
		case $1 in
		--version) version=${2:?}; shift 2 ;;
		--snapshot) snapshot=1; shift ;;
		--flr) flr=${2:?}; shift 2 ;;
		--host) host=${2:?}; shift 2 ;;
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
	if [ -z "$flr" ] && [ -z "$host" ]; then
		echo "check-tags: published needs --flr, --host or both" >&2
		exit 2
	fi
	k8s=$(kubernetes_version)
	# Any tag other than a full release version would move or be ambiguous:
	# latest, 1, 1.2, v1.2.3, main, a commit.
	flr_ok="^${release_version_re}\$"
	host_ok="^${release_version_re}(-k8s-v[0-9]+\\.[0-9]+\\.[0-9]+)?\$"
	for pair in "flr|$flr|$flr_ok" "host|$host|$host_ok"; do
		IFS="|" read -r _ ref ok <<<"$pair"
		[ -n "$ref" ] || continue
		repo=${ref%@*}
		tags=$(list_tags "$repo") || { fail "could not list the tags of $repo"; continue; }
		while IFS= read -r t; do
			[ -n "$t" ] || continue
			[[ $t =~ $ok ]] || fail "$repo:$t is not a release version tag"
		done <<<"$tags"
		echo "check-tags: checked the $(grep -c . <<<"$tags") tag(s) of $repo"
	done

	#= docs/requirements/12-cluster-fleet.md#cluster-release
	#= type=test
	#/ The Release SHALL publish the Host Image to the project's
	#/ container registry, tagged with the release version and with the release
	#/ version joined to the Kubernetes version it carries.
	wants=()
	[ -z "$flr" ] || wants+=("${flr%@*}:$version|${flr#*@}")
	[ -z "$host" ] || wants+=("${host%@*}:$version|${host#*@}" "${host%@*}:$version-k8s-$k8s|${host#*@}")
	for want in "${wants[@]}"; do
		ref=${want%|*} digest=${want#*|}
		got=$(manifest_digest "$ref") || { fail "$ref was not published"; continue; }
		if [ "$got" = "$digest" ]; then
			echo "check-tags: $ref is $digest"
		else
			fail "$ref is $got, want $digest"
		fi
	done
	;;

*)
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; /^set -euo/d' >&2
	exit 2
	;;
esac
exit "$status"
