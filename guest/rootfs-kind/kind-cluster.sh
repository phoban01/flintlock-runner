#!/usr/bin/env bash
# flr-kind-cluster, installed as /usr/local/bin/flr-kind-cluster in the
# Guest Image's kind image: make the kind cluster that
# /etc/default/kind-bootstrap describes, or take the one that is there.
#
# A Job on flr/kind runs it to make its cluster. A Pool of the warm Profile,
# flr/kind-ready, runs it as its create hook, so each MicroVM is handed out
# with the cluster running; the Job's run then only writes the kubeconfig.
# Either way it ends once every node is Ready, with the kubeconfig at
# KUBECONFIG_PATH.
#
# KIND_WAIT bounds each wait, 5m by default.
#
# FLR_KIND_VOLUMES says where Docker Engine keeps its volumes, which hold
# each kind node's /var and its containerd image store. It applies only
# when this makes the cluster:
#   tmpfs        the tmpfs that var-lib-docker-volumes.mount mounts at
#                boot, 25 percent of the MicroVM's memory (the default)
#   tmpfs:SIZE   the same tmpfs, resized to SIZE, for example 50% or 4G
#   disk         the root disk
# The last two restart Docker Engine, so they work only before anything
# else has used it: run flr-kind-cluster first.
# A Profile sets it in its create hook, for example
# `FLR_KIND_VOLUMES=disk /usr/local/bin/flr-kind-cluster`.
set -euo pipefail

# shellcheck source=/dev/null
. "${KIND_BOOTSTRAP:-/etc/default/kind-bootstrap}"
wait=${KIND_WAIT:-5m}
volumes=${FLR_KIND_VOLUMES:-tmpfs}
volumes_dir=/var/lib/docker/volumes
volumes_unit=var-lib-docker-volumes.mount
volumes_flag=${FLR_ROOT:-}/run/flr-guest/volumes-on-disk
log() { printf 'flr-kind-cluster: %s\n' "$*" >&2; }

# Docker Engine is socket activated, and may still be starting.
wait_docker() {
	for _ in $(seq 1 300); do
		docker info >/dev/null 2>&1 && return
		sleep 1
	done
	docker info >/dev/null
}

#= docs/requirements/13-guest-image.md#guest-kind-volumes
#/ If `FLR_KIND_VOLUMES` is set to a value other than `tmpfs`,
#/ `tmpfs:SIZE` or `disk`, then `flr-kind-cluster` SHALL fail without
#/ making the cluster.
case $volumes in
tmpfs | disk) ;;
tmpfs:*)
	if ! [[ ${volumes#tmpfs:} =~ ^[1-9][0-9]*[kKmMgG%]?$ ]]; then
		log "FLR_KIND_VOLUMES=$volumes: SIZE is not a tmpfs size such as 50% or 4G"
		exit 1
	fi
	;;
*)
	log "FLR_KIND_VOLUMES=$volumes is not tmpfs, tmpfs:SIZE or disk"
	exit 1
	;;
esac


# place_volumes puts Docker Engine's volumes where FLR_KIND_VOLUMES says,
# before there is a cluster. Either change restarts Docker Engine: the
# kernel can refuse to resize a tmpfs while Docker Engine runs.
place_volumes() {
	[ "$volumes" != tmpfs ] || return 0
	if [ "$volumes" = disk ] && ! mountpoint -q "$volumes_dir"; then
		log "Docker Engine's volumes are on the root disk already"
		return
	fi
	if [ "$volumes" != disk ] && ! mountpoint -q "$volumes_dir"; then
		log "FLR_KIND_VOLUMES=$volumes, but $volumes_dir is not a tmpfs"
		return 1
	fi
	#= docs/requirements/13-guest-image.md#guest-kind-volumes
	#/ If `FLR_KIND_VOLUMES` is `disk` or `tmpfs:SIZE` and Docker Engine
	#/ has a container or a volume, then `flr-kind-cluster` SHALL fail
	#/ without making the cluster and SHALL leave Docker Engine
	#/ running.
	#
	# The restart would stop the containers, and with `disk` the volumes
	# would stay behind on the tmpfs, out of Docker Engine's sight.
	if [ -n "$(docker ps -aq)" ] || [ -n "$(docker volume ls -q)" ]; then
		log "FLR_KIND_VOLUMES=$volumes, but Docker Engine has containers or volumes already; run flr-kind-cluster before anything else uses Docker Engine"
		return 1
	fi
	systemctl stop docker.socket docker.service
	case $volumes in
	tmpfs:*)
		#= docs/requirements/13-guest-image.md#guest-kind-volumes
		#/ When `flr-kind-cluster` makes the cluster and `FLR_KIND_VOLUMES`
		#/ is `tmpfs:SIZE`, `flr-kind-cluster` SHALL stop Docker Engine,
		#/ resize the tmpfs at `/var/lib/docker/volumes` to SIZE and start
		#/ Docker Engine again before it makes the cluster.
		log "resizing the tmpfs at $volumes_dir to ${volumes#tmpfs:}"
		mount -o "remount,size=${volumes#tmpfs:}" "$volumes_dir"
		;;
	disk)
		#= docs/requirements/13-guest-image.md#guest-kind-volumes
		#/ When `flr-kind-cluster` makes the cluster and `FLR_KIND_VOLUMES`
		#/ is `disk`, `flr-kind-cluster` SHALL stop Docker Engine, unmount
		#/ the tmpfs at `/var/lib/docker/volumes` and start Docker Engine
		#/ again before it makes the cluster, so that Docker Engine's
		#/ volumes are on the root disk.
		#
		# docker.service requires the tmpfs, but the unit's condition skips
		# it while the flag exists, and a skipped unit does not stop
		# docker.service. A runtime mask would not do: the unit is in
		# /etc/systemd/system, which wins over /run. /run is empty at the
		# next boot, so the tmpfs comes back then.
		log "moving Docker Engine's volumes from the tmpfs to the root disk"
		mkdir -p "$(dirname "$volumes_flag")"
		: >"$volumes_flag"
		systemctl stop "$volumes_unit"
		;;
	esac
	systemctl start docker.socket docker.service
	wait_docker
}

wait_docker

#= docs/requirements/13-guest-image.md#guest-kind-fast
#/ The Guest Image's kind image SHALL contain `flr-kind-cluster`, a
#/ command that makes the kind cluster of `/etc/default/kind-bootstrap` when
#/ there is none, writes the existing cluster's kubeconfig to
#/ `KUBECONFIG_PATH` when there is one, and ends once every node of the
#/ cluster is Ready.
mkdir -p "$(dirname "$KUBECONFIG_PATH")"
if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER_NAME"; then
	log "the cluster $KIND_CLUSTER_NAME is running already; writing its kubeconfig to $KUBECONFIG_PATH"
	[ "$volumes" = tmpfs ] || log "FLR_KIND_VOLUMES=$volumes does nothing to a cluster that is running"
	kind export kubeconfig --name "$KIND_CLUSTER_NAME" --kubeconfig "$KUBECONFIG_PATH"
else
	place_volumes
	# The node image is in Docker Engine's image store from the build
	# (docker-store.sh), so kind pulls nothing; this says so in the log.
	if id=$(docker image inspect -f '{{.Id}}' "$KIND_NODE_IMAGE" 2>/dev/null); then
		log "the node image is in Docker Engine already: $KIND_NODE_IMAGE ($id)"
	else
		log "the node image is not in Docker Engine; kind pulls $KIND_NODE_IMAGE"
	fi
	log "making the cluster $KIND_CLUSTER_NAME"
	kind create cluster --name "$KIND_CLUSTER_NAME" --image "$KIND_NODE_IMAGE" \
		--config "$KIND_CONFIG" --kubeconfig "$KUBECONFIG_PATH" --wait "$wait"
fi
kubectl --kubeconfig "$KUBECONFIG_PATH" wait --for=condition=Ready nodes --all --timeout="$wait"
log "the cluster $KIND_CLUSTER_NAME is ready; KUBECONFIG=$KUBECONFIG_PATH"
