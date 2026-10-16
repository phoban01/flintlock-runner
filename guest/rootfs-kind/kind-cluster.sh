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
set -euo pipefail

# shellcheck source=/dev/null
. "${KIND_BOOTSTRAP:-/etc/default/kind-bootstrap}"
wait=${KIND_WAIT:-5m}
log() { printf 'flr-kind-cluster: %s\n' "$*" >&2; }

# Docker Engine is socket activated, and may still be starting.
for _ in $(seq 1 300); do
	docker info >/dev/null 2>&1 && break
	sleep 1
done
docker info >/dev/null

#= docs/requirements/13-guest-image.md#guest-kind-fast
#/ The Guest Image's kind image SHALL contain `flr-kind-cluster`, a
#/ command that makes the kind cluster of `/etc/default/kind-bootstrap` when
#/ there is none, writes the existing cluster's kubeconfig to
#/ `KUBECONFIG_PATH` when there is one, and ends once every node of the
#/ cluster is Ready.
mkdir -p "$(dirname "$KUBECONFIG_PATH")"
if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER_NAME"; then
	log "the cluster $KIND_CLUSTER_NAME is running already; writing its kubeconfig to $KUBECONFIG_PATH"
	kind export kubeconfig --name "$KIND_CLUSTER_NAME" --kubeconfig "$KUBECONFIG_PATH"
else
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
