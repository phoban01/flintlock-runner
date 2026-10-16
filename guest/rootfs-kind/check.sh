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

# kind_cluster CLUSTERS runs flr-kind-cluster with stand-ins for docker,
# kind and kubectl: `kind get clusters` prints CLUSTERS. It prints the
# commands it ran.
kind_cluster() {
	rm -rf "${work:?}/bin" "${work:?}/home" "${work:?}/calls"
	mkdir -p "$work/bin" "$work/home"
	for tool in docker kind kubectl; do
		printf '#!/bin/sh\necho "%s $*" >>"%s/calls"\n' "$tool" "$work" >"$work/bin/$tool"
	done
	# shellcheck disable=SC2016 # the stand-in expands $1 and $2
	printf 'case "$1 $2" in "get clusters") printf "%%s\\n" "%s" ;; esac\n' "$1" >>"$work/bin/kind"
	chmod +x "$work/bin"/*
	env -i PATH="$work/bin:$PATH" HOME="$work/home" \
		/usr/local/bin/flr-kind-cluster >"$work/log" 2>&1 || fail "flr-kind-cluster failed: $(cat "$work/log")"
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

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "every check passed"
