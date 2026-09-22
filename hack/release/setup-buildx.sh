#!/usr/bin/env bash
# Prepare a CI runner's Docker for the flr image: binfmt emulation, so that
# check-flr-image.sh can run the other platform's flr, and a buildx builder
# with the docker-container driver, because the default docker driver cannot
# push a multi-platform image. Both images are pinned by digest; resolve a
# new one with `crane digest IMAGE:TAG`.
set -euo pipefail

binfmt=docker.io/tonistiigi/binfmt:qemu-v10.2.3@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0
buildkit=docker.io/moby/buildkit:v0.29.0@sha256:0039c1d47e8748b5afea56f4e85f14febaf34452bd99d9552d2daa82262b5cc5

docker run --privileged --rm "$binfmt" --install amd64,arm64
docker buildx create --name flr --driver docker-container --driver-opt "image=$buildkit" --use --bootstrap
docker buildx inspect flr
