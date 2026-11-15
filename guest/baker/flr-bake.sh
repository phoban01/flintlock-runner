#!/bin/sh
# flr-bake SPEC OUT: bake a project's kind cluster spec into OUT, for a
# project's guest image (docs/docker-and-kind.md). It is flr-kind bake,
# under the name the proposal gives it; flr-kind bake -h lists its options.
exec /usr/local/bin/flr-kind bake "$@"
