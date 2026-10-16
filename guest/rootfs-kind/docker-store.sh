#!/usr/bin/env bash
# Write an image into Docker Engine's image store, as Docker Engine 29 keeps
# it with the overlay2 storage driver and without the containerd image
# store, in the node-image stage of guest/rootfs-kind/Containerfile.
#
# usage: docker-store.sh ARCHIVE REF ROOT
#
# ARCHIVE is a docker-archive of one image with no whiteouts in its layers,
# REF the reference to give it, NAME:TAG@sha256:DIGEST, and ROOT the
# directory that becomes /var/lib/docker. Docker Engine then has the image
# under NAME:TAG and NAME@sha256:DIGEST when it first starts, so nothing
# pulls or loads it at boot, and the guest's copy of the root filesystem
# is the only copy of its files.
#
# What it writes, under ROOT:
#   image/overlay2/repositories.json             the tag and the digest
#   image/overlay2/imagedb/content/sha256/<id>   the image configuration
#   image/overlay2/layerdb/sha256/<chain id>/    diff, size, cache-id, parent
#   overlay2/<cache id>/                         diff/, link, lower, work/
#   overlay2/l/<link>                            -> ../<cache id>/diff
# The cache ids and links are derived from the chain ids, so two builds of
# one image write the same tree. There is no tar-split metadata: Docker
# Engine runs the image, and only `docker save` or `docker push` of it
# would need that.
set -euo pipefail

archive=${1:?usage: docker-store.sh ARCHIVE REF ROOT}
ref=${2:?usage: docker-store.sh ARCHIVE REF ROOT}
root=${3:?usage: docker-store.sh ARCHIVE REF ROOT}
die() {
	echo "docker-store: $*" >&2
	exit 1
}
sha() { sha256sum | cut -d' ' -f1; }

[[ $ref =~ ^([a-z0-9./_-]+):([A-Za-z0-9._-]+)@(sha256:[0-9a-f]{64})$ ]] || die "$ref is not NAME:TAG@sha256:DIGEST"
name=${BASH_REMATCH[1]} tag=${BASH_REMATCH[2]} digest=${BASH_REMATCH[3]}
# Docker Engine keeps the short form of a Docker Hub name.
name=${name#docker.io/}
name=${name#library/}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tar -xf "$archive" -C "$work"
manifest=$(cat "$work/manifest.json")
config=$(sed -n 's/.*"Config":"\([0-9a-f]\{64\}\)\.json".*/\1/p' <<<"$manifest")
[[ $config =~ ^[0-9a-f]{64}$ ]] || die "the archive's manifest.json names no configuration"
[ "$(sha <"$work/$config.json")" = "$config" ] || die "the configuration is not $config"
mapfile -t layers < <(grep -o '"Layers":\[[^]]*\]' <<<"$manifest" | grep -o '[0-9a-f]\{64\}\.tar')
[ ${#layers[@]} -gt 0 ] || die "the archive has no layers"
# The configuration lists the layers' diff ids in order.
mapfile -t diff_ids < <(grep -o '"diff_ids":\[[^]]*\]' "$work/$config.json" | grep -o 'sha256:[0-9a-f]\{64\}')
[ ${#diff_ids[@]} -eq ${#layers[@]} ] || die "the configuration lists ${#diff_ids[@]} layers, the archive has ${#layers[@]}"

image=$root/image/overlay2
mkdir -p "$image/imagedb/content/sha256" "$image/imagedb/metadata/sha256" "$image/layerdb/sha256" "$root/overlay2/l"
cp "$work/$config.json" "$image/imagedb/content/sha256/$config"
chmod 0600 "$image/imagedb/content/sha256/$config"

chain='' parent='' lower=''
for i in "${!layers[@]}"; do
	tarball=$work/${layers[$i]}
	diff=sha256:$(sha <"$tarball")
	[ "$diff" = "${diff_ids[$i]}" ] || die "layer $i is $diff, and the configuration says ${diff_ids[$i]}"
	if tar -tf "$tarball" | grep -q '\.wh\.'; then
		die "layer $i has whiteouts, which this script does not write"
	fi
	if [ -z "$chain" ]; then
		chain=$diff
	else
		chain=sha256:$(printf '%s %s' "$chain" "$diff" | sha)
	fi
	cache=$(printf 'flr-guest docker-store %s' "$chain" | sha)
	link=$(printf '%s' "$cache" | sha | cut -c1-26 | tr 'a-f' 'A-F')
	dir=$root/overlay2/$cache
	mkdir -p "$dir/diff"
	tar -xpf "$tarball" -C "$dir/diff" --numeric-owner --xattrs --xattrs-include='*'
	printf '%s' "$link" >"$dir/link"
	ln -s "../$cache/diff" "$root/overlay2/l/$link"
	if [ -n "$lower" ]; then
		printf '%s' "$lower" >"$dir/lower"
		mkdir -p "$dir/work"
	fi
	lower=l/$link${lower:+:$lower}

	meta=$image/layerdb/sha256/${chain#sha256:}
	mkdir -p "$meta"
	printf '%s' "$diff" >"$meta/diff"
	printf '%s' "$cache" >"$meta/cache-id"
	du -sb "$dir/diff" | cut -f1 | tr -d '\n' >"$meta/size"
	[ -z "$parent" ] || printf '%s' "$parent" >"$meta/parent"
	parent=$chain
done

printf '{"Repositories":{"%s":{"%s:%s":"sha256:%s","%s@%s":"sha256:%s"}}}' \
	"$name" "$name" "$tag" "$config" "$name" "$digest" "$config" >"$image/repositories.json"
chmod 0600 "$image/repositories.json"
chmod 0710 "$root"
chmod 0710 "$root/overlay2"
echo "docker-store: $name:$tag and $name@$digest are image sha256:$config, ${#layers[@]} layers, under $root"
