#!/usr/bin/env bash
# Check the container image of flr (KF-140, KF-141): one image per platform,
# each of which holds nothing but /usr/bin/flr, the CA certificates and the
# time zone database, runs as a numeric non-root user, has no shell, and
# whose flr reports the release version.
#
# usage: check-flr-image.sh --version V --platforms P[,P...] IMAGE...
#        check-flr-image.sh --version V --platforms P[,P...] --pull REF@DIGEST
#
# The first form checks images already in the local Docker daemon, one per
# platform, which is what goreleaser --snapshot leaves there. The second
# pulls every platform of a published multi-platform index and also checks
# that the index has exactly the platforms asked for. It needs Docker, jq
# and GNU tar, and skopeo for --pull (SKOPEO and REGISTRY_TLS_VERIFY are as
# for registry.sh); running another platform's flr needs binfmt emulation.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=registry.sh
. "$here/registry.sh"

version=
platforms=
pull=
while [ $# -gt 0 ]; do
	case $1 in
	--version) version=${2:?}; shift 2 ;;
	--platforms) platforms=${2:?}; shift 2 ;;
	--pull) pull=${2:?}; shift 2 ;;
	--) shift; break ;;
	-*) echo "check-flr-image: unknown option $1" >&2; exit 2 ;;
	*) break ;;
	esac
done
if [ -z "$version" ] || [ -z "$platforms" ]; then
	echo "check-flr-image: --version and --platforms are required" >&2
	exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
status=0
fail() { echo "check-flr-image: $*" >&2; status=1; }

want_platforms=$(tr ',' '\n' <<<"$platforms" | sort -u)
images=("$@")

#= docs/requirements/12-cluster-fleet.md#cluster-release
#= type=test
#/ The Release SHALL publish a container image of `flr` for
#/ `linux/amd64` and `linux/arm64` to the project's container registry,
#/ tagged with the release version.
if [ -n "$pull" ]; then
	if [ ${#images[@]} -ne 0 ]; then
		echo "check-flr-image: --pull takes no further images" >&2
		exit 2
	fi
	# The index's platforms, leaving out buildx's attestation manifests,
	# which are for platform unknown/unknown.
	skopeo_cmd inspect "$(tls_flag tls-verify)" --raw "docker://$pull" >"$work/index.json"
	got=$(jq -r '.manifests[] | select(.platform.os != "unknown") | "\(.platform.os)/\(.platform.architecture)"' "$work/index.json" | sort -u)
	if [ "$got" != "$want_platforms" ]; then
		fail "$pull has platforms [${got//$'\n'/ }], want [${want_platforms//$'\n'/ }]"
	fi
	# Each platform by its own manifest digest from the index: Docker's
	# classic image store cannot hold two platforms under one reference.
	for p in $got; do
		digest=$(jq -r --arg os "${p%/*}" --arg arch "${p#*/}" \
			'[.manifests[] | select(.platform.os == $os and .platform.architecture == $arch)][0].digest' "$work/index.json")
		docker pull --quiet "${pull%@*}@$digest" >/dev/null
		local_tag=flr-image-check:${p//\//-}
		docker tag "${pull%@*}@$digest" "$local_tag"
		images+=("$local_tag")
	done
fi
if [ ${#images[@]} -eq 0 ]; then
	echo "check-flr-image: no image to check" >&2
	exit 2
fi

got_platforms=
for image in "${images[@]}"; do
	inspect=$(docker image inspect "$image")
	platform=$(jq -r '.[0] | "\(.Os)/\(.Architecture)"' <<<"$inspect")
	got_platforms+="$platform"$'\n'
	echo "check-flr-image: $image is $platform"

	label=$(jq -r '.[0].Config.Labels["org.opencontainers.image.version"] // ""' <<<"$inspect")
	[ "$label" = "$version" ] || fail "$image: version label is \"$label\", want $version"

	#= docs/requirements/12-cluster-fleet.md#cluster-release
	#= type=test
	#/ The container image of `flr` SHALL run as a non-root user and
	#/ SHALL contain nothing but the `flr` binary, certificate authority
	#/ certificates and time zone data.
	#
	# A numeric uid, so that runAsNonRoot can be verified without a passwd
	# file, and not 0.
	user=$(jq -r '.[0].Config.User // ""' <<<"$inspect")
	if ! [[ $user =~ ^[1-9][0-9]*(:[0-9]+)?$ ]]; then
		fail "$image: user is \"$user\", want a numeric non-root uid"
	fi
	entrypoint=$(jq -c '.[0].Config.Entrypoint' <<<"$inspect")
	[ "$entrypoint" = '["/usr/bin/flr"]' ] || fail "$image: entrypoint is $entrypoint, want [\"/usr/bin/flr\"]"

	# Every path in every layer, read from the saved image rather than from
	# a container, which would add the runtime's own files.
	dir=$work/${image//[\/:]/_}
	mkdir -p "$dir"
	docker save "$image" -o "$dir/image.tar"
	tar -xf "$dir/image.tar" -C "$dir"
	: >"$dir/paths"
	while IFS= read -r layer; do
		tar -tf "$dir/$layer" >>"$dir/paths"
	done < <(jq -r '.[0].Layers[]' "$dir/manifest.json")
	sed -i 's|^\./||; s|/$||; /^$/d' "$dir/paths"
	sort -u -o "$dir/paths" "$dir/paths"
	extra=$(grep -vxE 'etc|etc/ssl|etc/ssl/certs|etc/ssl/certs/ca-certificates\.crt|usr|usr/bin|usr/bin/flr|usr/share|usr/share/zoneinfo(/.+)?' "$dir/paths" || true)
	if [ -n "$extra" ]; then
		fail "$image contains more than flr, the CA certificates and time zone data:"
		# shellcheck disable=SC2001 # indent every line
		sed 's/^/    /' <<<"$extra" >&2
	fi
	for need in usr/bin/flr etc/ssl/certs/ca-certificates.crt usr/share/zoneinfo/UTC usr/share/zoneinfo/Europe/Dublin; do
		grep -qxF "$need" "$dir/paths" || fail "$image has no $need"
	done
	if grep -qE '^(usr/)?s?bin/(sh|bash|ash|dash|busybox)$' "$dir/paths"; then
		fail "$image has a shell"
	fi
	if docker run --rm --platform "$platform" --entrypoint /bin/sh "$image" -c true >/dev/null 2>&1; then
		fail "$image ran /bin/sh"
	fi

	# The binary runs, as the image's user, and is this release's.
	if out=$(docker run --rm --platform "$platform" "$image" --version 2>&1); then
		grep -qF "$version" <<<"$out" || fail "$image: flr --version says \"$out\", want $version"
	else
		fail "$image: flr --version failed: $out"
	fi
	# flr keeps no file in the image, so a read-only root filesystem and an
	# arbitrary non-root uid are enough to start it.
	docker run --rm --platform "$platform" --read-only --user 65532:65532 "$image" --help >/dev/null 2>&1 ||
		fail "$image: flr --help failed read-only as 65532"
done

got_platforms=$(sort -u <<<"${got_platforms%$'\n'}")
if [ "$got_platforms" != "$want_platforms" ]; then
	fail "images are for [${got_platforms//$'\n'/ }], want [${want_platforms//$'\n'/ }]"
fi

if [ "$status" -eq 0 ]; then
	echo "check-flr-image: ${#images[@]} image(s) for [${want_platforms//$'\n'/ }]: non-root, flr, CA certificates and time zone data only"
fi
exit "$status"
