#!/usr/bin/env bash
# The check stage of the Guest Image's kind image
# (guest/rootfs-kind/Containerfile), after the check.sh of the root
# filesystem image and of the Docker image. It runs inside the built root
# filesystem, under emulation on a platform other than the build machine's,
# and needs neither systemd running, nor KVM, nor a network. Every failure
# is reported before it exits non-zero.
#
#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Image build SHALL check the contents of each image it
#/ builds in a check stage that fails unless the image carries everything
#/ this document requires of it.
set -uo pipefail
export PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

VERSIONS=/usr/share/flr-guest/versions.env
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}
same() {
	if [ -n "$2" ] && [ "$2" = "$3" ]; then ok "$1 is $2"; else fail "$1 is '$2', want '$3'"; fi
}

# shellcheck source=/dev/null
. "$VERSIONS" 2>/dev/null || {
	echo "FAIL  $VERSIONS is missing"
	exit 1
}
arch=$(dpkg --print-architecture)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "== kind image"

#= docs/requirements/13-guest-image.md#guest-kind
#= type=test
#/ The Guest Image SHALL include a kind image, a root filesystem image
#/ built on its Docker image for `linux/amd64` and `linux/arm64`, which
#/ carries everything this document requires of the Docker image.
#
# The Docker image's check.sh has run in this stage first.
case $arch in
amd64 | arm64) ok "the kind image is built for linux/$arch" ;;
*) fail "the kind image is built for $arch, not amd64 or arm64" ;;
esac
for v in DOCKER_VERSION GO_VERSION; do
	if [ -n "${!v:-}" ]; then ok "the Docker image's $v=${!v} is here"; else fail "$VERSIONS lost $v"; fi
done

#= docs/requirements/13-guest-image.md#guest-kind
#= type=test
#/ The Guest Image's kind image SHALL contain kind and kubectl, each
#/ at the version pinned in its sources.
same "kind" "$(kind version 2>/dev/null | awk '{print $2}')" "v${KIND_VERSION:-}"
if [[ $(kind version 2>/dev/null) == *"linux/$arch" ]]; then ok "kind is built for linux/$arch"; else fail "kind is not for linux/$arch: $(kind version 2>&1)"; fi
same "kubectl" "$(kubectl version --client 2>/dev/null | sed -n 's/^Client Version: //p')" "v${KUBECTL_VERSION:-}"

#= docs/requirements/13-guest-image.md#guest-kind
#= type=test
#/ The Guest Image's kind image SHALL contain
#/ `/etc/default/kind-bootstrap`, which exports `KIND_CLUSTER_NAME`,
#/ `KIND_NODE_IMAGE`, `KUBECONFIG_PATH` and `KUBECONFIG`, with
#/ `KIND_NODE_IMAGE` naming by digest a kindest/node image that the pinned
#/ kind release lists.
# shellcheck disable=SC2016 # expanded by the inner shell
exported=$(env -i HOME=/home/job bash -c '. /etc/default/kind-bootstrap && env')
for v in KIND_CLUSTER_NAME KIND_NODE_IMAGE KUBECONFIG_PATH KUBECONFIG KIND_CONFIG; do
	val=$(sed -n "s/^$v=//p" <<<"$exported")
	if [ -n "$val" ]; then ok "kind-bootstrap exports $v=$val"; else fail "kind-bootstrap does not export $v"; fi
done
node=$(sed -n 's/^KIND_NODE_IMAGE=//p' <<<"$exported")
if [[ $node =~ ^kindest/node:v[0-9.]+@sha256:[0-9a-f]{64}$ ]]; then ok "the node image is by digest"; else fail "the node image $node is not kindest/node by digest"; fi
same "the node image" "$node" "${KIND_NODE_IMAGE:-}"
# The build cannot read kind's release notes, which list the node images.
# It checks that the pinned node image is of the same Kubernetes minor
# version as kind's own default node image.
default_minor=$(grep -aoE 'kindest/node:v1\.[0-9]+' /usr/local/bin/kind 2>/dev/null | head -1 | sed 's/.*:v//')
if [ -n "$default_minor" ] && [[ $node == "kindest/node:v$default_minor."* ]]; then
	ok "the node image is of kind ${KIND_VERSION:-}'s Kubernetes ${default_minor:-version}"
else
	fail "the node image $node is not of kind ${KIND_VERSION:-}'s default Kubernetes $default_minor"
fi
same "KUBECONFIG" "$(sed -n 's/^KUBECONFIG=//p' <<<"$exported")" "$(sed -n 's/^KUBECONFIG_PATH=//p' <<<"$exported")"

# mirror ROUTE runs the boot unit's script with ROUTE as the output of
# `ip -4 route show default`, writing under $work/root.
mirror() {
	local route=$1
	rm -rf "${work:?}/root" "${work:?}/bin"
	mkdir -p "$work/root" "$work/bin"
	printf '#!/bin/sh\nprintf "%%s\\n" "%s"\n' "$route" >"$work/bin/ip"
	chmod +x "$work/bin/ip"
	PATH=$work/bin:$PATH FLR_ROOT=$work/root /usr/libexec/flr-guest/registry-mirror >"$work/log" 2>&1 ||
		fail "the boot unit's script failed: $(cat "$work/log")"
}

#= docs/requirements/13-guest-image.md#guest-kind
#= type=test
#/ When the guest has a default route once the wait for the network
#/ has ended, the Guest Image's kind image SHALL write
#/ `/etc/kind/kind-config.yaml` with a containerd mirror of `docker.io` over
#/ plain HTTP at port 5000 of the default route's gateway.
mirror "default via 10.220.0.1 dev eth1 proto dhcp src 10.220.0.7 metric 1024"
config=$(cat "$work/root/etc/kind/kind-config.yaml" 2>/dev/null)
hosts=$(cat "$work/root/etc/kind/certs.d/docker.io/hosts.toml" 2>/dev/null)
if grep -q 'config_path = "/etc/containerd/certs.d"' <<<"$config" &&
	grep -q 'containerPath: /etc/containerd/certs.d' <<<"$config" &&
	grep -q 'hostPath: /etc/kind/certs.d' <<<"$config"; then
	ok "with a default route, kind's containerd takes its mirrors from /etc/kind/certs.d"
else
	fail "with a default route, kind-config.yaml is: $config"
fi
if grep -qx '\[host."http://10.220.0.1:5000"\]' <<<"$hosts" && grep -q 'capabilities = \["pull", "resolve"\]' <<<"$hosts"; then
	ok "with a default route via 10.220.0.1, docker.io's mirror is http://10.220.0.1:5000"
else
	fail "with a default route via 10.220.0.1, docker.io/hosts.toml is: $hosts"
fi
if grep -q '"registry-mirrors": \["http://10.220.0.1:5000"\]' "$work/root/etc/docker/daemon.json" 2>/dev/null; then
	ok "the same boot unit points Docker Engine at the mirror"
else
	fail "Docker Engine's daemon.json has no mirror: $(cat "$work/root/etc/docker/daemon.json" 2>&1)"
fi

#= docs/requirements/13-guest-image.md#guest-kind
#= type=test
#/ If the guest has no default route once the wait for the network has
#/ ended, then the Guest Image's kind image SHALL write
#/ `/etc/kind/kind-config.yaml` with no registry mirror.
mirror ""
config=$(cat "$work/root/etc/kind/kind-config.yaml" 2>/dev/null)
if grep -q '^kind: Cluster$' <<<"$config" && ! grep -qE 'containerdConfigPatches|certs.d' <<<"$config" &&
	[ ! -e "$work/root/etc/kind/certs.d" ]; then
	ok "with no default route, kind-config.yaml has no mirror"
else
	fail "with no default route, kind-config.yaml is: $config"
fi
if grep -q '^kind: Cluster$' /etc/kind/kind-config.yaml && ! grep -q containerdConfigPatches /etc/kind/kind-config.yaml; then
	ok "the image's own kind-config.yaml, until the first boot, has no mirror"
else
	fail "the image's kind-config.yaml is: $(cat /etc/kind/kind-config.yaml 2>&1)"
fi

echo "== a fast kind cluster"

#= docs/requirements/13-guest-image.md#guest-kind-fast
#= type=test
#/ The Guest Image's kind image SHALL write `/etc/kind/kind-config.yaml`
#/ with one control-plane node and no other, and with etcd's data
#/ directory on the node's `/tmp`, which kind mounts as a tmpfs.
for route in "default via 10.220.0.1 dev eth1 proto dhcp src 10.220.0.7 metric 1024" ""; do
	mirror "$route"
	config=$(cat "$work/root/etc/kind/kind-config.yaml" 2>/dev/null)
	what="with${route:+ a}${route:- no} default route"
	if [ "$(grep -c -- '- role:' <<<"$config")" = 1 ] && grep -q -- '- role: control-plane$' <<<"$config"; then
		ok "$what, kind-config.yaml has one node, the control plane"
	else
		fail "$what, kind-config.yaml does not have one control-plane node: $config"
	fi
	if grep -q '^kubeadmConfigPatches:$' <<<"$config" && grep -q '^    kind: ClusterConfiguration$' <<<"$config" &&
		grep -q '^        dataDir: /tmp/etcd$' <<<"$config"; then
		ok "$what, etcd keeps its data on the node's tmpfs /tmp"
	else
		fail "$what, kind-config.yaml does not put etcd's data on /tmp: $config"
	fi
done

#= docs/requirements/13-guest-image.md#guest-kind-fast
#= type=test
#/ The Guest Image's kind image SHALL contain the node image that
#/ `KIND_NODE_IMAGE` names, for the image's architecture, in Docker
#/ Engine's image store under that reference, so that a kind cluster
#/ starts without a pull or a load of the node image.
#
# Docker Engine is not running here, so this reads its store as Docker
# Engine would: the reference, the configuration, and each layer of the
# configuration's diff ids by its chain id.
store=/var/lib/docker/image/overlay2
repos=$(cat "$store/repositories.json" 2>/dev/null)
node_id=${KIND_NODE_IMAGE_ID:-}
name=${node%%:*}
digest=${node#*@}
if [[ $node_id =~ ^sha256:[0-9a-f]{64}$ ]] && grep -qF "\"$name@$digest\":\"$node_id\"" <<<"$repos" &&
	grep -qF "\"${node%@*}\":\"$node_id\"" <<<"$repos"; then
	ok "Docker Engine's store has $name@$digest and ${node%@*} as $node_id"
else
	fail "Docker Engine's store does not have $node as '$node_id': $repos"
fi
config=$store/imagedb/content/sha256/${node_id#sha256:}
if [ -f "$config" ] && [ "sha256:$(sha256sum <"$config" | cut -d' ' -f1)" = "$node_id" ]; then
	ok "the node image's configuration is $node_id"
else
	fail "no configuration $node_id in Docker Engine's store"
fi
if grep -q "\"architecture\":\"$arch\"" "$config" 2>/dev/null; then ok "the node image is for $arch"; else fail "the node image is not for $arch"; fi
chain='' layers=0
while read -r diff; do
	if [ -z "$chain" ]; then chain=$diff; else chain=sha256:$(printf '%s %s' "$chain" "$diff" | sha256sum | cut -d' ' -f1); fi
	meta=$store/layerdb/sha256/${chain#sha256:}
	cache=$(cat "$meta/cache-id" 2>/dev/null)
	if [ "$(cat "$meta/diff" 2>/dev/null)" = "$diff" ] && [ -n "$cache" ] &&
		[ -n "$(ls -A "/var/lib/docker/overlay2/$cache/diff" 2>/dev/null)" ] &&
		[ -s "/var/lib/docker/overlay2/$cache/link" ] && [ -d "/var/lib/docker/overlay2/l/$(cat "/var/lib/docker/overlay2/$cache/link")" ]; then
		layers=$((layers + 1))
	else
		fail "layer $chain of the node image is not in Docker Engine's store"
	fi
done < <(grep -o '"diff_ids":\[[^]]*\]' "$config" 2>/dev/null | grep -o 'sha256:[0-9a-f]\{64\}')
if [ "$layers" -gt 0 ]; then ok "the node image's $layers layers are unpacked in Docker Engine's overlay2 store"; else fail "the node image has no layers in Docker Engine's store"; fi

# kind_cluster CLUSTERS [VOLUMES [MOUNTED [CONTAINERS]]] runs
# flr-kind-cluster under $work/root, with stand-ins for docker, kind,
# kubectl, systemctl, mount and mountpoint: `kind get clusters` prints
# CLUSTERS, `docker ps -aq` prints CONTAINERS, and `mountpoint` answers
# yes unless MOUNTED is no. A systemctl call made while the flag of
# FLR_KIND_VOLUMES=disk exists ends with "(flag)". FLR_KIND_VOLUMES is
# VOLUMES, unset when VOLUMES is empty. It prints the commands it ran, and
# fails the check when flr-kind-cluster fails unless KIND_EXPECT_FAIL is
# set; $work/rc has the exit code.
kind_cluster() {
	rm -rf "${work:?}/bin" "${work:?}/home" "${work:?}/calls" "${work:?}/root"
	mkdir -p "$work/bin" "$work/home" "$work/root"
	for tool in docker kind kubectl mount mountpoint; do
		printf '#!/bin/sh\necho "%s $*" >>"%s/calls"\n' "$tool" "$work" >"$work/bin/$tool"
	done
	# shellcheck disable=SC2016 # the stand-ins expand $1, $2 and $*
	{
		printf '#!/bin/sh\nif [ -e "%s" ]; then echo "systemctl $* (flag)"; else echo "systemctl $*"; fi >>"%s/calls"\n' \
			"$work/root/run/flr-guest/volumes-on-disk" "$work" >"$work/bin/systemctl"
		printf 'case "$1 $2" in "get clusters") printf "%%s\\n" "%s" ;; esac\n' "$1" >>"$work/bin/kind"
		printf 'case "$1 $2" in "ps -aq") printf "%%s\\n" "%s" ;; esac\n' "${4:-}" >>"$work/bin/docker"
		printf '[ "%s" != no ]\n' "${3:-yes}" >>"$work/bin/mountpoint"
	}
	chmod +x "$work/bin"/*
	local rc=0
	env -i PATH="$work/bin:$PATH" HOME="$work/home" FLR_ROOT="$work/root" ${2:+FLR_KIND_VOLUMES="$2"} \
		/usr/local/bin/flr-kind-cluster >"$work/log" 2>&1 || rc=$?
	echo "$rc" >"$work/rc"
	if [ "$rc" != 0 ] && [ -z "${KIND_EXPECT_FAIL:-}" ]; then fail "flr-kind-cluster failed: $(cat "$work/log")"; fi
	cat "$work/calls" 2>/dev/null
}

#= docs/requirements/13-guest-image.md#guest-kind-fast
#= type=test
#/ The Guest Image's kind image SHALL contain `flr-kind-cluster`, a
#/ command that makes the kind cluster of `/etc/default/kind-bootstrap` when
#/ there is none, writes the existing cluster's kubeconfig to
#/ `KUBECONFIG_PATH` when there is one, and ends once every node of the
#/ cluster is Ready.
kubeconfig=$work/home/.kube/kind-flr.yaml
calls=$(kind_cluster "")
if grep -qxF "kind create cluster --name flr --image $node --config /etc/kind/kind-config.yaml --kubeconfig $kubeconfig --wait 5m" <<<"$calls"; then
	ok "with no cluster, flr-kind-cluster makes one from KIND_NODE_IMAGE"
else
	fail "with no cluster, flr-kind-cluster ran: $calls"
fi
if grep -qxF "kubectl --kubeconfig $kubeconfig wait --for=condition=Ready nodes --all --timeout=5m" <<<"$calls"; then
	ok "flr-kind-cluster waits for every node to be Ready"
else
	fail "flr-kind-cluster does not wait for the nodes: $calls"
fi
calls=$(kind_cluster "flr")
if grep -qxF "kind export kubeconfig --name flr --kubeconfig $kubeconfig" <<<"$calls" && ! grep -q '^kind create' <<<"$calls"; then
	ok "with the cluster running, flr-kind-cluster writes its kubeconfig and makes none"
else
	fail "with the cluster running, flr-kind-cluster ran: $calls"
fi
if grep -qxF "kubectl --kubeconfig $kubeconfig wait --for=condition=Ready nodes --all --timeout=5m" <<<"$calls"; then
	ok "with the cluster running, flr-kind-cluster still waits for every node to be Ready"
else
	fail "with the cluster running, flr-kind-cluster does not wait for the nodes: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-fast
#= type=test
#/ The Guest Image's kind image SHALL mount a tmpfs of 25 percent
#/ of the MicroVM's memory, with mode 0700 and owned by root, at
#/ `/var/lib/docker/volumes` before Docker Engine starts, so that the `/var`
#/ volume of a kind node is in memory.
#
# systemd is not running here, so this reads the unit and its links as
# systemd would at boot.
mount_unit=var-lib-docker-volumes.mount
unit=/etc/systemd/system/$mount_unit
value() { sed -n "s/^$1=//p" "$unit" 2>/dev/null | tail -1; }
if [ -f "$unit" ]; then ok "$unit is here"; else fail "$unit is missing"; fi
same "the mount's Where" "$(value Where)" /var/lib/docker/volumes
same "the mount's What" "$(value What)" tmpfs
same "the mount's Type" "$(value Type)" tmpfs
options=",$(value Options),"
for want in size=25% mode=0700 uid=0 gid=0; do
	if [[ $options == *",$want,"* ]]; then ok "the tmpfs has $want"; else fail "the tmpfs options are '$(value Options)', without $want"; fi
done
if [ "$(systemctl is-enabled "$mount_unit" 2>/dev/null)" = enabled ]; then
	ok "$mount_unit is enabled"
else
	fail "$mount_unit is not enabled"
fi
if [ "$(readlink -f "/etc/systemd/system/docker.service.requires/$mount_unit")" = "$unit" ]; then
	ok "docker.service requires $mount_unit"
else
	fail "docker.service does not require $mount_unit"
fi
# FLR_KIND_VOLUMES=disk writes the flag, and the unit is skipped (#157).
same "the mount's condition" "$(value ConditionPathExists)" "!/run/flr-guest/volumes-on-disk"
if [[ " $(value Before | tr ',' ' ') " == *" docker.service "* ]]; then
	ok "$mount_unit is ordered before docker.service"
else
	fail "$mount_unit is not ordered before docker.service: Before=$(value Before)"
fi
if [ -d /var/lib/docker/volumes ] && [ -z "$(ls -A /var/lib/docker/volumes)" ]; then
	ok "the image has nothing under /var/lib/docker/volumes for the tmpfs to hide"
else
	fail "/var/lib/docker/volumes is missing or not empty: $(ls -A /var/lib/docker/volumes 2>&1)"
fi

echo "== kind and Cluster API"

#= docs/requirements/13-guest-image.md#guest-kind-capi
#= type=test
#/ The Guest Image's kind image SHALL write `/etc/kind/kind-config.yaml`
#/ with the guest's Docker socket, `/var/run/docker.sock`, mounted at the
#/ same path in the control-plane node, with a registry mirror and
#/ without one.
#
# The node is the only one (GI-066), so its extraMounts are the file's.
# The mount is writable: a client needs write access to connect to the
# socket.
for route in "default via 10.220.0.1 dev eth1 proto dhcp src 10.220.0.7 metric 1024" ""; do
	mirror "$route"
	config=$(cat "$work/root/etc/kind/kind-config.yaml" 2>/dev/null)
	what="with${route:+ a}${route:- no} default route"
	mount=$(sed -n '/^  - role: control-plane$/,$p' <<<"$config" | grep -A2 -x '      - hostPath: /var/run/docker.sock')
	if grep -qx '    extraMounts:' <<<"$config" &&
		grep -qx '        containerPath: /var/run/docker.sock' <<<"$mount" && ! grep -q 'readOnly: true' <<<"$mount"; then
		ok "$what, the node has the guest's Docker socket at /var/run/docker.sock"
	else
		fail "$what, kind-config.yaml does not mount the Docker socket into the node: $config"
	fi
done
if grep -qx '      - hostPath: /var/run/docker.sock' /etc/kind/kind-config.yaml; then
	ok "the image's own kind-config.yaml, until the first boot, mounts the Docker socket"
else
	fail "the image's kind-config.yaml does not mount the Docker socket: $(cat /etc/kind/kind-config.yaml 2>&1)"
fi

echo "== where Docker's volumes go"

# in_order CALLS LINE... succeeds when each LINE is a line of CALLS, after
# the one before it.
in_order() {
	local calls=$1 at=0 n
	shift
	for line in "$@"; do
		n=$(grep -nxF -- "$line" <<<"$calls" | cut -d: -f1 | awk -v at="$at" '$1 > at { print; exit }')
		[ -n "$n" ] || return 1
		at=$n
	done
}
create="kind create cluster --name flr --image $node --config /etc/kind/kind-config.yaml --kubeconfig $kubeconfig --wait 5m"
moves=$'systemctl stop docker.socket docker.service\nsystemctl stop var-lib-docker-volumes.mount (flag)\nsystemctl start docker.socket docker.service (flag)'

calls=$(kind_cluster "")
if grep -qxF "$create" <<<"$calls" && ! grep -qE '^(systemctl|mount) ' <<<"$calls"; then
	ok "with no FLR_KIND_VOLUMES, flr-kind-cluster leaves the tmpfs as the boot mounted it"
else
	fail "with no FLR_KIND_VOLUMES, flr-kind-cluster ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-volumes
#= type=test
#/ When `flr-kind-cluster` makes the cluster and `FLR_KIND_VOLUMES`
#/ is `disk`, `flr-kind-cluster` SHALL stop Docker Engine, unmount
#/ the tmpfs at `/var/lib/docker/volumes` and start Docker Engine
#/ again before it makes the cluster, so that Docker Engine's
#/ volumes are on the root disk.
calls=$(kind_cluster "" disk)
mapfile -t want <<<"$moves"
if in_order "$calls" "${want[@]}" "$create"; then
	ok "with FLR_KIND_VOLUMES=disk, flr-kind-cluster stops Docker Engine, writes the flag, unmounts the tmpfs and starts Docker Engine before it makes the cluster"
else
	fail "with FLR_KIND_VOLUMES=disk, flr-kind-cluster ran: $calls"
fi
calls=$(kind_cluster "" disk no)
if grep -qxF "$create" <<<"$calls" && ! grep -q '^systemctl ' <<<"$calls"; then
	ok "with FLR_KIND_VOLUMES=disk and no tmpfs, flr-kind-cluster restarts nothing"
else
	fail "with FLR_KIND_VOLUMES=disk and no tmpfs, flr-kind-cluster ran: $calls"
fi
calls=$(kind_cluster flr disk)
if grep -q '^kind export kubeconfig' <<<"$calls" && ! grep -qE '^(systemctl |kind create)' <<<"$calls"; then
	ok "with FLR_KIND_VOLUMES=disk and the cluster running, flr-kind-cluster leaves the volumes where they are"
else
	fail "with FLR_KIND_VOLUMES=disk and the cluster running, flr-kind-cluster ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-volumes
#= type=test
#/ When `flr-kind-cluster` makes the cluster and `FLR_KIND_VOLUMES`
#/ is `tmpfs:SIZE`, `flr-kind-cluster` SHALL stop Docker Engine,
#/ resize the tmpfs at `/var/lib/docker/volumes` to SIZE and start
#/ Docker Engine again before it makes the cluster.
for size in 50% 4G; do
	calls=$(kind_cluster "" "tmpfs:$size")
	if in_order "$calls" "systemctl stop docker.socket docker.service" "mount -o remount,size=$size /var/lib/docker/volumes" \
		"systemctl start docker.socket docker.service" "$create" && ! grep -q '(flag)$' <<<"$calls"; then
		ok "with FLR_KIND_VOLUMES=tmpfs:$size, flr-kind-cluster stops Docker Engine, resizes the tmpfs to $size and starts Docker Engine before it makes the cluster"
	else
		fail "with FLR_KIND_VOLUMES=tmpfs:$size, flr-kind-cluster ran: $calls"
	fi
done
calls=$(KIND_EXPECT_FAIL=1 kind_cluster "" tmpfs:50% no)
if [ "$(cat "$work/rc")" != 0 ] && ! grep -qE '^(systemctl |mount |kind create)' <<<"$calls"; then
	ok "with FLR_KIND_VOLUMES=tmpfs:50% and no tmpfs, flr-kind-cluster fails and makes no cluster"
else
	fail "with FLR_KIND_VOLUMES=tmpfs:50% and no tmpfs, flr-kind-cluster exited $(cat "$work/rc") and ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-volumes
#= type=test
#/ If `FLR_KIND_VOLUMES` is `disk` or `tmpfs:SIZE` and Docker Engine
#/ has a container or a volume, then `flr-kind-cluster` SHALL fail
#/ without making the cluster and SHALL leave Docker Engine
#/ running.
for v in disk tmpfs:50%; do
	calls=$(KIND_EXPECT_FAIL=1 kind_cluster "" "$v" yes 0123456789ab)
	if [ "$(cat "$work/rc")" != 0 ] && ! grep -qE '^(systemctl |mount |kind create)' <<<"$calls"; then
		ok "with FLR_KIND_VOLUMES=$v and a container, flr-kind-cluster fails, makes no cluster and leaves Docker Engine running"
	else
		fail "with FLR_KIND_VOLUMES=$v and a container, flr-kind-cluster exited $(cat "$work/rc") and ran: $calls"
	fi
done

#= docs/requirements/13-guest-image.md#guest-kind-volumes
#= type=test
#/ If `FLR_KIND_VOLUMES` is set to a value other than `tmpfs`,
#/ `tmpfs:SIZE` or `disk`, then `flr-kind-cluster` SHALL fail without
#/ making the cluster.
for bad in ram tmpfs: tmpfs:lots tmpfs:0 "disk "; do
	calls=$(KIND_EXPECT_FAIL=1 kind_cluster "" "$bad")
	if [ "$(cat "$work/rc")" != 0 ] && ! grep -qE '^(systemctl |mount |kind create)' <<<"$calls"; then
		ok "with FLR_KIND_VOLUMES='$bad', flr-kind-cluster fails and makes no cluster"
	else
		fail "with FLR_KIND_VOLUMES='$bad', flr-kind-cluster exited $(cat "$work/rc") and ran: $calls"
	fi
done

echo "== a project's kind cluster in the Pool"

# ELF machine of a file: 3e00 for amd64, b700 for arm64.
elf_arch() {
	case $(od -An -tx1 -j18 -N2 "$1" 2>/dev/null | tr -d ' \n') in
	3e00) echo amd64 ;;
	b700) echo arm64 ;;
	*) echo unknown ;;
	esac
}

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ The Guest Image's kind image SHALL contain `helm`, at the version pinned
#/ in its sources.
same "helm" "$(helm version --template '{{.Version}}' 2>/dev/null)" "v${HELM_VERSION:-}"
same "helm's architecture" "$(elf_arch /usr/local/bin/helm)" "$arch"

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ The Guest Image's kind image SHALL contain `flr-kind`, built for the
#/ image's architecture from the Guest Image's sources.
same "flr-kind's architecture" "$(elf_arch /usr/local/bin/flr-kind)" "$arch"
mkdir -p "$work/digest/.flr" "$work/digest/deploy"
printf 'apiVersion: flintlock-runner.dev/v1alpha1\nkind: KindCluster\nname: e2e\nmanifests:\n  - path: ../deploy/a.yaml\n' >"$work/digest/.flr/kind-cluster.yaml"
printf 'kind: Namespace\n' >"$work/digest/deploy/a.yaml"
same "flr-kind digest" "$(flr-kind digest "$work/digest/.flr/kind-cluster.yaml" 2>&1)" \
	"sha256:$(cd "$work/digest" && sha256sum .flr/kind-cluster.yaml deploy/a.yaml | sha256sum | cut -d' ' -f1)"

# spec_root writes the guest's files under $work/root as a project's guest
# image has them: the kind image's kind configuration with a registry
# mirror, the baked spec at /etc/flr/kind-cluster.yaml with the digest
# beside it, two image archives of 3 MiB and 1 MiB, two manifests and a
# chart with a values file. Its spec makes two nodes, with volumes: disk.
baked_digest=sha256:$(printf 'a project spec' | sha256sum | cut -d' ' -f1)
spec_root() {
	rm -rf "${work:?}/root"
	mkdir -p "$work/root/etc/flr" "$work/root/var/lib/flr/images" "$work/root/var/lib/flr/manifests" "$work/root/var/lib/flr/charts"
	FLR_ROOT=$work/root FLR_REGISTRY_MIRROR=http://10.220.0.1:5000 /usr/libexec/flr-guest/registry-mirror.d/20-kind >/dev/null
	truncate -s 3M "$work/root/var/lib/flr/images/a.tar"
	truncate -s 1M "$work/root/var/lib/flr/images/b.tar"
	printf 'kind: Namespace\n' >"$work/root/var/lib/flr/manifests/0.yaml"
	printf 'kind: Namespace\n' >"$work/root/var/lib/flr/manifests/2.yaml"
	printf 'a chart' >"$work/root/var/lib/flr/charts/1.tgz"
	printf 'a: b\n' >"$work/root/var/lib/flr/charts/1-values-0.yaml"
	printf '%s\n' "$baked_digest" >"$work/root/etc/flr/kind-cluster.digest"
	cat >"$work/root/etc/flr/kind-cluster.yaml" <<-EOF
		apiVersion: flintlock-runner.dev/v1alpha1
		kind: KindCluster
		name: e2e
		volumes: ${1:-disk}
		kindConfig: |
		  kind: Cluster
		  apiVersion: kind.x-k8s.io/v1alpha4
		  nodes:
		    - role: control-plane
		    - role: worker
		manifests:
		  - path: ../../var/lib/flr/manifests/0.yaml
		    wait:
		      - deployment/cert-manager-webhook -n cert-manager
		      - crd/certificates.cert-manager.io
		  - helm:
		      chart: ../../var/lib/flr/charts/1.tgz
		      version: 1.4.2
		      digest: sha256:$(printf '1%.0s' $(seq 64))
		      release: thing
		      namespace: thing
		      values: [../../var/lib/flr/charts/1-values-0.yaml]
		    wait:
		      - deployment/thing -n thing
		  - path: ../../var/lib/flr/manifests/2.yaml
		    wait:
		      - pod/thing-0 -n thing
		${2:-}
	EOF
}

# project_spec writes a project's checkout at $work/repo, with its spec at
# .flr/kind-cluster.yaml: no kindConfig, a kustomize directory, and the
# extra entries in $1. It prints the spec's path.
project_spec() {
	rm -rf "${work:?}/repo"
	mkdir -p "$work/repo/.flr" "$work/repo/deploy/k"
	printf 'kind: Namespace\n' >"$work/repo/deploy/k/ns.yaml"
	printf 'resources: [ns.yaml]\n' >"$work/repo/deploy/k/kustomization.yaml"
	printf 'apiVersion: flintlock-runner.dev/v1alpha1\nkind: KindCluster\nname: e2e\nmanifests:\n  - kustomize: ../deploy/k\n%s' "${1:-}" \
		>"$work/repo/.flr/kind-cluster.yaml"
	echo "$work/repo/.flr/kind-cluster.yaml"
}

# kind_spec SPEC [CLUSTERS [HAVE [FREE]]] runs flr-kind-cluster --spec SPEC
# under $work/root, with the stand-ins of kind_cluster and stand-ins for
# helm, df and curl: `kind get clusters` prints CLUSTERS,
# /run/flr-guest/kind-cluster.digest holds HAVE (missing when HAVE is
# empty), and df says FREE bytes are free (100 GB by default). kubectl
# fails on `apply` when KIND_FAIL_APPLY is set, and curl writes CURL_BODY.
# Each entry of spec_env goes into the environment. It prints the commands
# it ran, and fails the check when flr-kind-cluster fails unless
# KIND_EXPECT_FAIL is set; $work/rc has the exit code and $work/log the log.
spec_env=()
kind_spec() {
	rm -rf "${work:?}/bin" "${work:?}/home" "${work:?}/calls" "${work:?}/root/run"
	mkdir -p "$work/bin" "$work/home" "$work/root/run/flr-guest"
	[ -z "${3:-}" ] || printf '%s\n' "$3" >"$work/root/run/flr-guest/kind-cluster.digest"
	for tool in docker kind kubectl helm mount mountpoint curl; do
		printf '#!/bin/sh\necho "%s $*" >>"%s/calls"\n' "$tool" "$work" >"$work/bin/$tool"
	done
	# shellcheck disable=SC2016 # the stand-ins expand $1, $2 and $*
	{
		printf '#!/bin/sh\nif [ -e "%s" ]; then echo "systemctl $* (flag)"; else echo "systemctl $*"; fi >>"%s/calls"\n' \
			"$work/root/run/flr-guest/volumes-on-disk" "$work" >"$work/bin/systemctl"
		printf 'case "$1 $2" in "get clusters") printf "%%s\\n" "%s" ;; esac\n' "${2:-}" >>"$work/bin/kind"
		printf 'echo "df $*" >>"%s/calls"\necho Avail\necho %s\n' "$work" "${4:-100000000000}" >"$work/bin/df"
		printf 'case " $* " in *" apply "*) [ -z "%s" ] || exit 1 ;; esac\n' "${KIND_FAIL_APPLY:-}" >>"$work/bin/kubectl"
		printf 'while [ $# -gt 0 ]; do [ "$1" = -o ] && printf "%%s" "%s" >"$2"; shift; done\n' "${CURL_BODY:-}" >>"$work/bin/curl"
	}
	chmod +x "$work/bin"/*
	local rc=0
	env -i PATH="$work/bin:$PATH" HOME="$work/home" FLR_ROOT="$work/root" "${spec_env[@]}" \
		/usr/local/bin/flr-kind-cluster --spec "$1" >"$work/log" 2>&1 || rc=$?
	echo "$rc" >"$work/rc"
	if [ "$rc" != 0 ] && [ -z "${KIND_EXPECT_FAIL:-}" ]; then fail "flr-kind-cluster --spec failed: $(cat "$work/log")"; fi
	cat "$work/calls" 2>/dev/null
}

# nodes_of FILE prints a line for each node of a kind configuration as
# flr-kind writes it: its role, "sock" when it mounts the Docker socket,
# "certs" when it mounts the mirror's hosts directory, and its image.
nodes_of() {
	awk '/^nodes:/ { on = 1; next } on && /^[^ ]/ { on = 0 }
		on && /^  - / { n++ }
		on && /role: / { role[n] = $NF }
		on && /containerPath: \/var\/run\/docker.sock$/ { sock[n] = 1 }
		on && /containerPath: \/etc\/containerd\/certs.d$/ { certs[n] = 1 }
		on && /image: / { img[n] = $NF }
		END { for (i = 1; i <= n; i++) print role[i], (sock[i] ? "sock" : "-"), (certs[i] ? "certs" : "-"), img[i] }' "$1" 2>/dev/null
}

baked=$work/root/etc/flr/kind-cluster.yaml
merged=$work/root/run/flr-guest/kind-cluster-config.yaml
kc=$work/home/.kube/config
lib=$work/root/var/lib/flr
screate="kind create cluster --name e2e --config $merged --kubeconfig $kc --wait 5m"
sready="kubectl --kubeconfig $kc wait --for=condition=Ready nodes --all --timeout=5m"
load_a="kind load image-archive $lib/images/a.tar --name e2e"
load_b="kind load image-archive $lib/images/b.tar --name e2e"
apply0="kubectl --kubeconfig $kc apply -f $lib/manifests/0.yaml"
wait0a="kubectl --kubeconfig $kc rollout status deployment/cert-manager-webhook -n cert-manager --timeout=5m"
wait0b="kubectl --kubeconfig $kc wait --for=condition=Established crd/certificates.cert-manager.io --timeout=5m"
helm1="helm install thing $lib/charts/1.tgz --kubeconfig $kc --namespace thing --create-namespace -f $lib/charts/1-values-0.yaml"
wait1="kubectl --kubeconfig $kc rollout status deployment/thing -n thing --timeout=5m"
apply2="kubectl --kubeconfig $kc apply -f $lib/manifests/2.yaml"
wait2="kubectl --kubeconfig $kc wait --for=condition=Ready pod/thing-0 -n thing --timeout=5m"

spec_root
calls=$(kind_spec "$baked")
hook_calls=$calls

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` makes the cluster, it SHALL make it with
#/ the spec's `name`, from the spec's `kindConfig`, or from
#/ `/etc/kind/kind-config.yaml` when the spec has no `kindConfig`.
if grep -qxF "$screate" <<<"$calls" && [ "$(grep -c '^kind create' <<<"$calls")" = 1 ] &&
	[ "$(nodes_of "$merged" | awk '{print $1}' | tr '\n' ' ')" = "control-plane worker " ]; then
	ok "with --spec, flr-kind-cluster makes the cluster e2e from the spec's kindConfig, with its two nodes"
else
	fail "with --spec, flr-kind-cluster ran: $calls; nodes: $(nodes_of "$merged")"
fi
nokind=$(project_spec)
calls=$(kind_spec "$nokind")
if grep -qxF "$screate" <<<"$calls" && grep -q '^        dataDir: /tmp/etcd$' "$merged" &&
	[ "$(nodes_of "$merged" | awk '{print $1}' | tr '\n' ' ')" = "control-plane " ]; then
	ok "with --spec and no kindConfig, flr-kind-cluster makes the cluster from the kind image's own kind configuration"
else
	fail "with --spec and no kindConfig, flr-kind-cluster ran: $calls; config: $(cat "$merged" 2>&1)"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` makes the cluster from a spec with a
#/ `kindConfig`, `flr-kind-cluster` SHALL add to it the
#/ `containerdConfigPatches` of `/etc/kind/kind-config.yaml`, and add to
#/ every node each mount of the control-plane node of
#/ `/etc/kind/kind-config.yaml` other than the Docker socket.
spec_root
kind_spec "$baked" >/dev/null
if grep -q 'config_path = "/etc/containerd/certs.d"' "$merged" &&
	[ "$(nodes_of "$merged" | awk '{print $3}' | tr '\n' ' ')" = "certs certs " ]; then
	ok "the spec's kind configuration gets the registry mirror's containerd patch, and its hosts directory on every node"
else
	fail "the spec's kind configuration lacks the mirror: $(cat "$merged" 2>&1)"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` makes the cluster from a spec with a
#/ `kindConfig`, `flr-kind-cluster` SHALL mount the guest's Docker socket,
#/ `/var/run/docker.sock`, at the same path in each control-plane node and
#/ in no other node.
if [ "$(nodes_of "$merged" | awk '{print $1, $2}' | tr '\n' ' ')" = "control-plane sock worker - " ] &&
	grep -qE '^ +(- )?hostPath: /var/run/docker.sock$' "$merged"; then
	ok "the spec's control-plane node, and no other, gets the guest's Docker socket"
else
	fail "the Docker socket is not on the control plane only: $(nodes_of "$merged")"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` makes the cluster, `flr-kind-cluster`
#/ SHALL give each node that names no `image` the node image of
#/ `KIND_NODE_IMAGE`.
if [ "$(nodes_of "$merged" | awk '{print $4}' | sort -u)" = "$node" ]; then
	ok "each node of the spec's cluster has the node image $node"
else
	fail "the spec's nodes do not have $node: $(nodes_of "$merged")"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` makes the cluster, it SHALL put Docker
#/ Engine's volumes where the spec's `volumes` says, as GI-081 to
#/ GI-084 say for the same value of `FLR_KIND_VOLUMES`.
mapfile -t want <<<"$moves"
if in_order "$hook_calls" "${want[@]}" "$screate"; then
	ok "with volumes: disk, flr-kind-cluster --spec moves Docker Engine's volumes to the root disk before it makes the cluster"
else
	fail "with volumes: disk, flr-kind-cluster --spec ran: $hook_calls"
fi
spec_root tmpfs:4G
calls=$(spec_env=(FLR_KIND_VOLUMES=disk); kind_spec "$baked")
if in_order "$calls" "systemctl stop docker.socket docker.service" "mount -o remount,size=4G /var/lib/docker/volumes" \
	"systemctl start docker.socket docker.service" "$screate" && ! grep -q '(flag)$' <<<"$calls"; then
	ok "with volumes: tmpfs:4G, flr-kind-cluster --spec resizes the tmpfs to 4G before it makes the cluster, whatever FLR_KIND_VOLUMES says"
else
	fail "with volumes: tmpfs:4G, flr-kind-cluster --spec ran: $calls"
fi
spec_root tmpfs
calls=$(kind_spec "$baked")
if grep -qxF "$screate" <<<"$calls" && ! grep -qE '^(systemctl|mount) ' <<<"$calls"; then
	ok "with volumes: tmpfs, flr-kind-cluster --spec leaves the tmpfs as the boot mounted it"
else
	fail "with volumes: tmpfs, flr-kind-cluster --spec ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` has made the cluster and every node is
#/ Ready, `flr-kind-cluster` SHALL load each archive under
#/ `/var/lib/flr/images` into every node with `kind load image-archive`.
if in_order "$hook_calls" "$screate" "$sready" "$load_a" && in_order "$hook_calls" "$sready" "$load_b" &&
	! grep -q -- '--nodes' <<<"$hook_calls"; then
	ok "once every node is Ready, flr-kind-cluster --spec loads each archive into every node"
else
	fail "flr-kind-cluster --spec did not load each archive into every node: $hook_calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ If the space free at `/var/lib/docker/volumes` is less than twice the
#/ size of the archives under `/var/lib/flr/images` times the number of
#/ nodes, then `flr-kind-cluster --spec` SHALL fail before it loads an
#/ archive, with a message that names `volumes: disk` and
#/ `volumes: tmpfs:SIZE`.
#
# 4 MiB of archives, two nodes: 16 MiB.
spec_root
calls=$(KIND_EXPECT_FAIL=1 kind_spec "$baked" "" "" $((16 * 1048576 - 1)))
if [ "$(cat "$work/rc")" != 0 ] && grep -qxF "df -B1 --output=avail /var/lib/docker/volumes" <<<"$calls" &&
	! grep -qE '^(kind load|kubectl .* apply|helm )' <<<"$calls" &&
	grep -qF 'volumes: disk' "$work/log" && grep -qF 'volumes: tmpfs:SIZE' "$work/log"; then
	ok "with 1 byte less than 16 MiB free for two nodes and 4 MiB of archives, flr-kind-cluster --spec fails before it loads them, naming volumes: disk and volumes: tmpfs:SIZE"
else
	fail "with too little space, flr-kind-cluster --spec exited $(cat "$work/rc") and ran: $calls; log: $(cat "$work/log")"
fi
calls=$(kind_spec "$baked" "" "" $((16 * 1048576)))
if grep -qxF "$load_a" <<<"$calls"; then
	ok "with 16 MiB free for two nodes and 4 MiB of archives, flr-kind-cluster --spec loads them"
else
	fail "with just enough space, flr-kind-cluster --spec ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` has loaded the archives, it SHALL
#/ apply each entry under `manifests` in the spec's order: a `path` with
#/ `kubectl apply -f`, a `kustomize` directory with `kubectl apply -k`,
#/ and a `helm` chart with `helm install`, with its values files, and
#/ with its namespace and `--create-namespace` when it has one.
if in_order "$hook_calls" "$load_a" "$load_b" "$apply0" "$helm1" "$apply2"; then
	ok "flr-kind-cluster --spec applies the manifest, installs the chart with its values in its namespace, and applies the next manifest, in that order, after the loads"
else
	fail "flr-kind-cluster --spec did not apply the entries in order: $hook_calls"
fi
calls=$(kind_spec "$nokind")
if grep -qxF "kubectl --kubeconfig $kc apply -k $work/repo/deploy/k" <<<"$calls"; then
	ok "flr-kind-cluster --spec applies a kustomize directory with kubectl apply -k"
else
	fail "flr-kind-cluster --spec did not apply the kustomize directory: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ After `flr-kind-cluster --spec` applies an entry under `manifests`,
#/ it SHALL wait, for up to `KIND_WAIT` each, for each item of the
#/ entry's `wait` list before it applies the next entry: with
#/ `kubectl rollout status` for a Deployment, DaemonSet or StatefulSet,
#/ for the condition Established for a CustomResourceDefinition, and for
#/ the condition Ready for anything else.
if in_order "$hook_calls" "$apply0" "$wait0a" "$wait0b" "$helm1" "$wait1" "$apply2" "$wait2"; then
	ok "flr-kind-cluster --spec waits for each entry's rollouts, CRDs and pods before the next entry"
else
	fail "flr-kind-cluster --spec did not wait for each entry: $hook_calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` has applied every entry under
#/ `manifests` and waited for each item of their `wait` lists, it SHALL
#/ write the spec's digest to `/run/flr-guest/kind-cluster.digest`.
#
#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ `flr-kind-cluster --spec FILE` SHALL take as the spec's digest the
#/ content of the file beside FILE with the same name and the extension
#/ `.digest`, when there is one, and the digest that `flr-kind digest`
#/ prints for FILE when there is none.
spec_root
kind_spec "$baked" >/dev/null
same "the digest that flr-kind-cluster --spec writes for the baked spec" \
	"$(cat "$work/root/run/flr-guest/kind-cluster.digest" 2>/dev/null)" "$baked_digest"
kind_spec "$nokind" >/dev/null
same "the digest that flr-kind-cluster --spec writes for a project's spec" \
	"$(cat "$work/root/run/flr-guest/kind-cluster.digest" 2>/dev/null)" "$(flr-kind digest "$nokind")"

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ If a step of `flr-kind-cluster --spec` fails, then `flr-kind-cluster`
#/ SHALL exit with a non-zero status and SHALL NOT write
#/ `/run/flr-guest/kind-cluster.digest`.
calls=$(KIND_EXPECT_FAIL=1 KIND_FAIL_APPLY=1 kind_spec "$baked")
if [ "$(cat "$work/rc")" != 0 ] && [ ! -e "$work/root/run/flr-guest/kind-cluster.digest" ] && ! grep -q '^helm ' <<<"$calls"; then
	ok "when kubectl apply fails, flr-kind-cluster --spec fails, stops and writes no digest"
else
	fail "when kubectl apply fails, flr-kind-cluster --spec exited $(cat "$work/rc") and ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ If `flr-kind-cluster --spec FILE` takes the spec's digest from a
#/ `.digest` file and the spec has an entry under `manifests` with a
#/ `url`, or a `helm` entry whose `chart` is not a file in the guest,
#/ then `flr-kind-cluster` SHALL fail before it makes the cluster.
sum=$(printf 'a%.0s' $(seq 64))
spec_root disk "  - url: https://example.com/a.yaml
    sha256: $sum"
calls=$(KIND_EXPECT_FAIL=1 kind_spec "$baked")
if [ "$(cat "$work/rc")" != 0 ] && ! grep -qE '^(systemctl |mount |kind create|curl )' <<<"$calls"; then
	ok "with a url in the baked spec, flr-kind-cluster --spec fails before it makes the cluster"
else
	fail "with a url in the baked spec, flr-kind-cluster --spec exited $(cat "$work/rc") and ran: $calls"
fi
spec_root disk "  - helm:
      chart: oci://ghcr.io/example/thing
      version: 1.0.0
      digest: sha256:$sum
      release: other"
calls=$(KIND_EXPECT_FAIL=1 kind_spec "$baked")
if [ "$(cat "$work/rc")" != 0 ] && ! grep -qE '^(systemctl |mount |kind create|helm )' <<<"$calls"; then
	ok "with a chart that is not in the guest, flr-kind-cluster --spec fails before it makes the cluster"
else
	fail "with a chart that is not in the guest, flr-kind-cluster --spec exited $(cat "$work/rc") and ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When `flr-kind-cluster --spec` applies an entry with a `url`, it SHALL
#/ download the file, and SHALL fail without applying it unless its sha256
#/ is the entry's `sha256`.
spec_root
body=$'kind: Namespace\n'
urlspec=$(project_spec "  - url: https://example.com/ns.yaml
    sha256: $(printf '%s' "$body" | sha256sum | cut -d' ' -f1)
")
calls=$(CURL_BODY=$body kind_spec "$urlspec")
if grep -qE '^curl -fsSL -[o] [^ ]+ https://example\.com/ns\.yaml$' <<<"$calls" &&
	[ "$(grep -c "^kubectl --kubeconfig $kc apply -f " <<<"$calls")" = 1 ]; then
	ok "in a job, flr-kind-cluster --spec downloads a url entry and applies it when its sha256 matches"
else
	fail "in a job, flr-kind-cluster --spec did not apply the url entry: $calls"
fi
calls=$(KIND_EXPECT_FAIL=1 CURL_BODY="something else" kind_spec "$urlspec")
if [ "$(cat "$work/rc")" != 0 ] && ! grep -q "^kubectl --kubeconfig $kc apply -f " <<<"$calls"; then
	ok "in a job, flr-kind-cluster --spec fails without applying a url entry whose sha256 does not match"
else
	fail "with a wrong sha256, flr-kind-cluster --spec exited $(cat "$work/rc") and ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When the cluster of the spec's `name` is running and
#/ `/run/flr-guest/kind-cluster.digest` holds the spec's digest,
#/ `flr-kind-cluster --spec` SHALL write the cluster's kubeconfig and
#/ make no cluster.
job=$(project_spec)
jdigest=$(flr-kind digest "$job")
calls=$(kind_spec "$job" e2e "$jdigest")
if grep -qxF "kind export kubeconfig --name e2e --kubeconfig $kc" <<<"$calls" && grep -qxF "$sready" <<<"$calls" &&
	! grep -qE '^(kind (create|delete|load)|kubectl .* apply|helm |systemctl )' <<<"$calls"; then
	ok "in a job whose spec has the hook's digest, flr-kind-cluster --spec writes the kubeconfig and changes nothing"
else
	fail "in a job whose spec has the hook's digest, flr-kind-cluster --spec ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ When a kind cluster is running and `/run/flr-guest/kind-cluster.digest`
#/ is missing or does not hold the spec's digest, `flr-kind-cluster --spec`
#/ SHALL log both digests, delete every kind cluster and make the cluster
#/ from the spec.
calls=$(kind_spec "$job" e2e "$baked_digest")
if in_order "$calls" "kind delete cluster --name e2e" "$screate" "kubectl --kubeconfig $kc apply -k $work/repo/deploy/k" &&
	grep "^flr-kind-cluster: WARNING: " "$work/log" | grep -F "$baked_digest" | grep -qF "$jdigest" &&
	[ "$(cat "$work/root/run/flr-guest/kind-cluster.digest" 2>/dev/null)" = "$jdigest" ]; then
	ok "in a job whose spec has another digest, flr-kind-cluster --spec logs both, deletes the cluster and makes the job's"
else
	fail "in a job whose spec has another digest, flr-kind-cluster --spec ran: $calls; log: $(cat "$work/log")"
fi
calls=$(kind_spec "$job" flr)
if in_order "$calls" "kind delete cluster --name flr" "$screate"; then
	ok "with a cluster running and no digest, flr-kind-cluster --spec deletes it and makes the spec's"
else
	fail "with a cluster running and no digest, flr-kind-cluster --spec ran: $calls"
fi

#= docs/requirements/13-guest-image.md#guest-kind-spec-hook
#= type=test
#/ `flr-kind-cluster --spec` SHALL write the cluster's kubeconfig to
#/ `FLR_KIND_KUBECONFIG` when it is set, and to `.kube/config` in the
#/ home directory when it is not.
if grep -qxF "$screate" <<<"$hook_calls"; then
	ok "flr-kind-cluster --spec writes the kubeconfig to ~/.kube/config"
else
	fail "flr-kind-cluster --spec does not write the kubeconfig to ~/.kube/config: $hook_calls"
fi
calls=$(spec_env=(FLR_KIND_KUBECONFIG="$work/kc.yaml"); kind_spec "$job" e2e "$jdigest")
if grep -qxF "kind export kubeconfig --name e2e --kubeconfig $work/kc.yaml" <<<"$calls"; then
	ok "flr-kind-cluster --spec writes the kubeconfig to FLR_KIND_KUBECONFIG when it is set"
else
	fail "flr-kind-cluster --spec ignores FLR_KIND_KUBECONFIG: $calls"
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "every check passed"
