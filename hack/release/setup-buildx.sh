#!/usr/bin/env bash
# Prepare a CI runner's Docker for the flr image and the Guest Image: binfmt
# emulation, so that check-flr-image.sh can run the other platform's flr and
# the Guest Image's check stages run on both platforms, and a buildx builder
# with the docker-container driver, because the default docker driver cannot
# push a multi-platform image. Both images are pinned by digest; resolve a
# new one with `crane digest IMAGE:TAG`.
#
# usage: setup-buildx.sh [--host-network]
#
# --host-network puts the builder on the runner's network, so that it can
# push to a registry on localhost, as the packaging dry run does.
set -euo pipefail

binfmt=docker.io/tonistiigi/binfmt:qemu-v10.2.3@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0
buildkit=docker.io/moby/buildkit:v0.29.0@sha256:0039c1d47e8748b5afea56f4e85f14febaf34452bd99d9552d2daa82262b5cc5

opts=(--driver-opt "image=$buildkit")
case ${1:-} in
'') ;;
--host-network) opts+=(--driver-opt network=host) ;;
*) echo "usage: setup-buildx.sh [--host-network]" >&2; exit 2 ;;
esac

docker run --privileged --rm "$binfmt" --install amd64,arm64
docker buildx create --name flr --driver docker-container "${opts[@]}" --use --bootstrap
docker buildx inspect flr
