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

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "every check passed"
