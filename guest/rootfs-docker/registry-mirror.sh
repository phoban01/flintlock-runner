#!/usr/bin/env bash
# flr-registry-mirror.service, installed as /usr/libexec/flr-guest/registry-mirror
# in the Guest Image's Docker image (guest/rootfs-docker): once the guest
# has its DHCP lease, point the guest's container tools at the Host's
# registry mirror.
#
# The Host Services listen on the guest bridge's gateway, which is the
# guest's default gateway (docs/host-services.md), and the registry mirror
# on REGISTRY_MIRROR_PORT there, 5000 unless /etc/default/flr-registry-mirror
# says otherwise. The mirror speaks plain HTTP: traffic to the gateway never
# leaves the Host.
#
# It sets FLR_REGISTRY_MIRROR to http://<gateway>:<port>, or to nothing when
# the guest has no default route, and runs each hook in
# /usr/libexec/flr-guest/registry-mirror.d in order. Each hook writes one
# tool's configuration: 10-docker Docker Engine's, and in the kind image
# 20-kind the kind cluster's. Docker Engine starts after this unit.
#
# FLR_ROOT, when set, is a directory the hooks write under instead of /,
# and FLR_HOOKS the directory of hooks; the check stages use both.
set -euo pipefail

hooks=${FLR_HOOKS:-/usr/libexec/flr-guest/registry-mirror.d}
port=${REGISTRY_MIRROR_PORT:-5000}

#= docs/requirements/13-guest-image.md#guest-docker
#/ When the guest has a default route once the wait for the network
#/ has ended, the Guest Image's Docker image SHALL configure Docker Engine,
#/ before it starts, with a registry mirror over plain HTTP at port 5000 of
#/ the default route's gateway.
#
#= docs/requirements/13-guest-image.md#guest-docker
#/ If the guest has no default route once the wait for the network has
#/ ended, then the Guest Image's Docker image SHALL start Docker Engine with
#/ no registry mirror.
gateway=$(ip -4 route show default 2>/dev/null | awk '$1 == "default" && $2 == "via" { print $3; exit }' || true)
if [[ $gateway =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] && [[ $port =~ ^[0-9]+$ ]]; then
	export FLR_REGISTRY_MIRROR="http://$gateway:$port"
	echo "registry-mirror: the default gateway is $gateway; the registry mirror is $FLR_REGISTRY_MIRROR"
else
	export FLR_REGISTRY_MIRROR=''
	echo "registry-mirror: no default IPv4 gateway; no registry mirror"
fi
export FLR_ROOT=${FLR_ROOT:-}

status=0
for hook in "$hooks"/*; do
	[ -x "$hook" ] || continue
	"$hook" || { echo "registry-mirror: $hook failed" >&2; status=1; }
done
exit "$status"
