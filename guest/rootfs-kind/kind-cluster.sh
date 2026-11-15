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
# With --spec FILE it makes the cluster that a project's kind cluster spec
# describes instead (docs/docker-and-kind.md, "A project's own kind
# cluster"), and writes the kubeconfig to ~/.kube/config, or to
# FLR_KIND_KUBECONFIG when that is set. A Pool's create hook runs it on the
# spec that the project's guest image has baked in,
# /etc/flr/kind-cluster.yaml; the Job runs it on the spec in its checkout,
# .flr/kind-cluster.yaml, and takes the cluster as it is when the two specs
# have the same digest.
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
# `FLR_KIND_VOLUMES=disk /usr/local/bin/flr-kind-cluster`. With --spec, the
# spec's `volumes` takes its place.
set -euo pipefail

log() { printf 'flr-kind-cluster: %s\n' "$*" >&2; }
spec=''
while [ $# -gt 0 ]; do
	case $1 in
	--spec)
		[ $# -ge 2 ] || { log "--spec needs a file"; exit 2; }
		spec=$2
		shift 2
		;;
	--spec=*) spec=${1#--spec=}; shift ;;
	*) log "usage: flr-kind-cluster [--spec FILE]"; exit 2 ;;
	esac
done

# shellcheck source=/dev/null
. "${KIND_BOOTSTRAP:-/etc/default/kind-bootstrap}"
wait=${KIND_WAIT:-5m}
volumes=${FLR_KIND_VOLUMES:-tmpfs}
volumes_dir=/var/lib/docker/volumes
volumes_unit=var-lib-docker-volumes.mount
volumes_flag=${FLR_ROOT:-}/run/flr-guest/volumes-on-disk
kubeconfig=$KUBECONFIG_PATH

# With --spec: what flr-kind plan makes of the spec (cmd/flr-kind/plan.go).
# The plan has a line for each step, with the fields separated by tabs, so
# that nothing here reads YAML. The merged kind configuration goes in
# merged_config.
root=${FLR_ROOT:-}
run_dir=$root/run/flr-guest
digest_file=$run_dir/kind-cluster.digest
images_dir=$root/var/lib/flr/images
merged_config=$run_dir/kind-cluster-config.yaml
if [ -n "$spec" ]; then
	if [ ! -f "$spec" ]; then
		log "--spec $spec: no such file"
		exit 1
	fi
	plan_args=()
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ `flr-kind-cluster --spec FILE` SHALL take as the spec's digest the
	#/ content of the file beside FILE with the same name and the extension
	#/ `.digest`, when there is one, and the digest that `flr-kind digest`
	#/ prints for FILE when there is none.
	#
	# A baked spec, /etc/flr/kind-cluster.yaml, has the digest of the
	# project's own spec beside it: the baked copy has its files inline or
	# under /var/lib/flr, so its own digest would differ. Its paths are
	# relative to /etc/flr and inside /, and it has to work without the
	# network.
	if [ -f "${spec%.yaml}.digest" ]; then
		want=$(cat "${spec%.yaml}.digest")
		if ! [[ $want =~ ^sha256:[0-9a-f]{64}$ ]]; then
			log "${spec%.yaml}.digest does not hold a digest: $want"
			exit 1
		fi
		#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
		#/ If `flr-kind-cluster --spec FILE` takes the spec's digest from a
		#/ `.digest` file and the spec has an entry under `manifests` with a
		#/ `url`, or a `helm` entry whose `chart` is not a file in the guest,
		#/ then `flr-kind-cluster` SHALL fail before it makes the cluster.
		plan_args=(-root "${root:-/}" -offline)
	else
		want=$(flr-kind digest "$spec")
	fi
	if ! plan=$(flr-kind plan "${plan_args[@]}" -base "$root$KIND_CONFIG" -kind-config "$merged_config" \
		-node-image "$KIND_NODE_IMAGE" "$spec"); then
		log "--spec $spec: flr-kind refused the spec; nothing was changed"
		exit 1
	fi
	field() { awk -F'\t' -v k="$1" '$1 == k { print $2; exit }' <<<"$plan"; }
	name=$(field name)
	nodes=$(field nodes)
	if [ -n "${FLR_KIND_VOLUMES:-}" ] && [ "$FLR_KIND_VOLUMES" != "$(field volumes)" ]; then
		log "FLR_KIND_VOLUMES=$FLR_KIND_VOLUMES does nothing with --spec; the spec says volumes: $(field volumes)"
	fi
	volumes=$(field volumes)
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ `flr-kind-cluster --spec` SHALL write the cluster's kubeconfig to
	#/ `FLR_KIND_KUBECONFIG` when it is set, and to `.kube/config` in the
	#/ home directory when it is not.
	kubeconfig=${FLR_KIND_KUBECONFIG:-${HOME:-/root}/.kube/config}
fi

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

# mib BYTES prints BYTES in MiB, rounded up.
mib() { echo $((($1 + 1048575) / 1048576)); }

# load_archives loads each image archive under /var/lib/flr/images into
# every node of the cluster, once it has checked that the nodes have room
# for them.
load_archives() {
	local archives=() a total=0 need avail
	shopt -s nullglob
	archives=("$images_dir"/*.tar)
	shopt -u nullglob
	if [ ${#archives[@]} -eq 0 ]; then
		log "there are no image archives in $images_dir to load"
		return
	fi
	for a in "${archives[@]}"; do
		total=$((total + $(stat -c %s "$a")))
	done
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ If the space free at `/var/lib/docker/volumes` is less than twice the
	#/ size of the archives under `/var/lib/flr/images` times the number of
	#/ nodes, then `flr-kind-cluster --spec` SHALL fail before it loads an
	#/ archive, with a message that names `volumes: disk` and
	#/ `volumes: tmpfs:SIZE`.
	#
	# kind load puts a copy of each image in every node, and the node's
	# containerd keeps each layer both as it came and unpacked.
	need=$((total * 2 * nodes))
	avail=$(df -B1 --output=avail "$volumes_dir" | tail -n 1 | tr -d ' ')
	if ! [[ $avail =~ ^[0-9]+$ ]]; then
		log "cannot read the space free at $volumes_dir: $avail"
		return 1
	fi
	if [ "$need" -gt "$avail" ]; then
		log "the $nodes node(s) need about $(mib "$need") MiB at $volumes_dir for $(mib "$total") MiB of image archives, but it has $(mib "$avail") MiB free with volumes: $volumes; set volumes: disk, or a larger volumes: tmpfs:SIZE, in $spec"
		return 1
	fi
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ When `flr-kind-cluster --spec` has made the cluster and every node is
	#/ Ready, `flr-kind-cluster` SHALL load each archive under
	#/ `/var/lib/flr/images` into every node with `kind load image-archive`.
	for a in "${archives[@]}"; do
		log "loading $a into every node"
		kind load image-archive "$a" --name "$name"
	done
}

# wait_for RESOURCE NAMESPACE waits for one item of an entry's wait list,
# with NAMESPACE "-" for none. The resource may not exist at once, for
# example when an operator makes it, so it waits for that first.
wait_for() {
	local res=$1 ns=()
	[ "$2" = - ] || ns=(-n "$2")
	log "waiting for $res${ns[*]:+ in ${ns[1]}}"
	for _ in $(seq 1 150); do
		kubectl --kubeconfig "$kubeconfig" get "$res" "${ns[@]}" >/dev/null 2>&1 && break
		sleep 2
	done
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ After `flr-kind-cluster --spec` applies an entry under `manifests`,
	#/ it SHALL wait, for up to `KIND_WAIT` each, for each item of the
	#/ entry's `wait` list before it applies the next entry: with
	#/ `kubectl rollout status` for a Deployment, DaemonSet or StatefulSet,
	#/ for the condition Established for a CustomResourceDefinition, and for
	#/ the condition Ready for anything else.
	case ${res%%/*} in
	deploy* | daemonset* | ds | statefulset* | sts)
		kubectl --kubeconfig "$kubeconfig" rollout status "$res" "${ns[@]}" --timeout="$wait"
		;;
	crd | crds | customresourcedefinition*)
		kubectl --kubeconfig "$kubeconfig" wait --for=condition=Established "$res" "${ns[@]}" --timeout="$wait"
		;;
	*)
		kubectl --kubeconfig "$kubeconfig" wait --for=condition=Ready "$res" "${ns[@]}" --timeout="$wait"
		;;
	esac
}

# apply_manifests applies the plan's steps in order.
apply_manifests() {
	local f args v tmp
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ When `flr-kind-cluster --spec` has loaded the archives, it SHALL
	#/ apply each entry under `manifests` in the spec's order: a `path` with
	#/ `kubectl apply -f`, a `kustomize` directory with `kubectl apply -k`,
	#/ and a `helm` chart with `helm install`, with its values files, and
	#/ with its namespace and `--create-namespace` when it has one.
	while IFS=$'\t' read -r -u 3 -a f; do
		case ${f[0]} in
		apply)
			log "applying ${f[1]}"
			kubectl --kubeconfig "$kubeconfig" apply -f "${f[1]}"
			;;
		apply-k)
			log "applying the kustomize directory ${f[1]}"
			kubectl --kubeconfig "$kubeconfig" apply -k "${f[1]}"
			;;
		url)
			#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
			#/ When `flr-kind-cluster --spec` applies an entry with a `url`,
			#/ it SHALL download the file, and SHALL fail without applying
			#/ it unless its sha256 is the entry's `sha256`.
			log "downloading ${f[1]}"
			tmp=$(mktemp)
			curl -fsSL -o "$tmp" "${f[1]}"
			printf '%s  %s\n' "${f[2]}" "$tmp" | sha256sum -c --quiet - >&2
			kubectl --kubeconfig "$kubeconfig" apply -f "$tmp"
			rm -f "$tmp"
			;;
		helm)
			args=(install "${f[1]}" "${f[3]}" --kubeconfig "$kubeconfig")
			[ "${f[4]}" = - ] || args+=(--version "${f[4]}")
			[ "${f[2]}" = - ] || args+=(--namespace "${f[2]}" --create-namespace)
			for v in "${f[@]:5}"; do
				args+=(-f "$v")
			done
			log "installing the chart ${f[3]} as ${f[1]}"
			helm "${args[@]}"
			;;
		wait) wait_for "${f[1]}" "${f[2]}" ;;
		esac
	done 3<<<"$plan"
}

# spec_cluster makes the cluster of the spec, or takes the running one when
# it was made from a spec with the same digest.
spec_cluster() {
	local clusters have=''
	mkdir -p "$(dirname "$kubeconfig")" "$run_dir"
	clusters=$(kind get clusters 2>/dev/null | sed '/^$/d' || true)
	[ ! -f "$digest_file" ] || have=$(cat "$digest_file")
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ When the cluster of the spec's `name` is running and
	#/ `/run/flr-guest/kind-cluster.digest` holds the spec's digest,
	#/ `flr-kind-cluster --spec` SHALL write the cluster's kubeconfig and
	#/ make no cluster.
	if grep -qxF "$name" <<<"$clusters" && [ "$have" = "$want" ]; then
		log "the cluster $name is running already, from a spec with the digest $want; writing its kubeconfig to $kubeconfig"
		kind export kubeconfig --name "$name" --kubeconfig "$kubeconfig"
		kubectl --kubeconfig "$kubeconfig" wait --for=condition=Ready nodes --all --timeout="$wait"
		log "the cluster $name is ready; KUBECONFIG=$kubeconfig"
		return
	fi
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ When a kind cluster is running and `/run/flr-guest/kind-cluster.digest`
	#/ is missing or does not hold the spec's digest, `flr-kind-cluster --spec`
	#/ SHALL log both digests, delete every kind cluster and make the cluster
	#/ from the spec.
	if [ -n "$clusters" ]; then
		log "WARNING: the spec $spec has the digest $want, but the running cluster was made from ${have:-a spec with no digest}; deleting it and making a new cluster from $spec"
		rm -f "$digest_file"
		while read -r c; do
			kind delete cluster --name "$c"
		done <<<"$clusters"
	fi
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ When `flr-kind-cluster --spec` makes the cluster, it SHALL put Docker
	#/ Engine's volumes where the spec's `volumes` says, as GI-081 to
	#/ GI-084 say for the same value of `FLR_KIND_VOLUMES`.
	place_volumes
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ When `flr-kind-cluster --spec` makes the cluster, it SHALL make it with
	#/ the spec's `name`, from the spec's `kindConfig`, or from
	#/ `/etc/kind/kind-config.yaml` when the spec has no `kindConfig`.
	log "making the cluster $name, with $nodes node(s), from $spec"
	kind create cluster --name "$name" --config "$merged_config" --kubeconfig "$kubeconfig" --wait "$wait"
	kubectl --kubeconfig "$kubeconfig" wait --for=condition=Ready nodes --all --timeout="$wait"
	load_archives
	apply_manifests
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ When `flr-kind-cluster --spec` has applied every entry under
	#/ `manifests` and waited for each item of their `wait` lists, it SHALL
	#/ write the spec's digest to `/run/flr-guest/kind-cluster.digest`.
	#
	#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
	#/ If a step of `flr-kind-cluster --spec` fails, then `flr-kind-cluster`
	#/ SHALL exit with a non-zero status and SHALL NOT write
	#/ `/run/flr-guest/kind-cluster.digest`.
	#
	# set -e ends the script at the first step that fails, before this.
	printf '%s\n' "$want" >"$digest_file.tmp"
	mv "$digest_file.tmp" "$digest_file"
	log "the cluster $name is ready, from a spec with the digest $want; KUBECONFIG=$kubeconfig"
}

wait_docker

if [ -n "$spec" ]; then
	spec_cluster
	exit
fi

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
