#!/usr/bin/env bash
# The check stage of the Guest Image's Docker image
# (guest/rootfs-docker/Containerfile), after the root filesystem image's own
# check.sh. It runs inside the built root filesystem, under emulation on a
# platform other than the build machine's, and needs neither systemd
# running, nor KVM, nor a network. Every failure is reported before it
# exits non-zero.
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
# same DESCRIPTION GOT WANT
same() {
	if [ -n "$2" ] && [ "$2" = "$3" ]; then ok "$1 is $2"; else fail "$1 is '$2', want '$3'"; fi
}
enabled() {
	if [ "$(systemctl is-enabled "$1" 2>/dev/null)" = enabled ]; then ok "$1 is enabled"; else fail "$1 is not enabled"; fi
}
# unit_values UNIT SECTION_KEY: every value of KEY for UNIT, from the unit
# file and its drop-ins.
unit_values() {
	local unit=$1 key=$2 f
	for f in "/usr/lib/systemd/system/$unit" "/etc/systemd/system/$unit" \
		/usr/lib/systemd/system/"$unit".d/*.conf /etc/systemd/system/"$unit".d/*.conf; do
		[ -f "$f" ] || continue
		sed -n "s/^$key=//p" "$f"
	done
}

# shellcheck source=/dev/null
. "$VERSIONS" 2>/dev/null || {
	echo "FAIL  $VERSIONS is missing"
	exit 1
}
arch=$(dpkg --print-architecture)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "== Docker image"

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ The Guest Image SHALL include a Docker image, a root filesystem
#/ image built on the Guest Image's root filesystem image for `linux/amd64`
#/ and `linux/arm64`, which carries everything this document requires of
#/ that image.
#
# The root filesystem image's check.sh has run in this stage first.
case $arch in
amd64 | arm64) ok "the Docker image is built for linux/$arch" ;;
*) fail "the Docker image is built for $arch, not amd64 or arm64" ;;
esac
for v in GUEST_AGENT_VERSION GITLAB_RUNNER_VERSION; do
	if [ -n "${!v:-}" ]; then ok "the root filesystem image's $v=${!v} is here"; else fail "$VERSIONS lost $v"; fi
done

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ The Guest Image's Docker image SHALL contain Docker Engine,
#/ containerd, runc and the Docker buildx plugin, each at the version pinned
#/ in its sources, and SHALL start Docker Engine at boot.
same "dockerd" "$(dockerd --version 2>/dev/null | sed -n 's/^Docker version \([^,]*\),.*/\1/p')" "${DOCKER_VERSION:-}"
same "the docker CLI" "$(docker version --format '{{.Client.Version}}' 2>/dev/null)" "${DOCKER_VERSION:-}"
same "containerd" "$(containerd --version 2>/dev/null | awk '{print $3}' | sed 's/^v//')" "${CONTAINERD_VERSION:-}"
same "runc" "$(runc --version 2>/dev/null | sed -n 's/^runc version //p')" "${RUNC_VERSION:-}"
same "docker buildx" "$(docker buildx version 2>/dev/null | awk '{print $2}' | sed 's/^v//')" "${BUILDX_VERSION:-}"
for unit in containerd.service docker.service docker.socket; do enabled "$unit"; done
# A fast start: containerd needs no migration of its configuration, and
# Docker Engine waits neither for containerd nor for the network.
if containerd --config /etc/containerd/config.toml config dump 2>&1 | grep -qi migrat; then
	fail "containerd migrates /etc/containerd/config.toml at every start"
else
	ok "containerd takes /etc/containerd/config.toml as it is"
fi
docker_unit=$(sed -n 's/^\(After\|Wants\)=//p' /etc/systemd/system/docker.service 2>/dev/null)
if [ -n "$docker_unit" ] && ! grep -qwE 'network-online\.target' <<<"$docker_unit" &&
	! sed -n 's/^After=//p' /etc/systemd/system/docker.service | grep -qw 'containerd\.service'; then
	ok "docker.service waits neither for network-online.target nor for containerd.service"
else
	fail "docker.service still waits for the network or containerd: $docker_unit"
fi

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ The Guest Image's Docker image SHALL set iptables and ip6tables to
#/ their legacy backend.
for tool in iptables ip6tables; do
	got=$("$tool" --version 2>/dev/null)
	if [[ $got == *"(legacy)"* ]]; then ok "$tool is $got"; else fail "$tool is '$got', not the legacy backend"; fi
done

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ The Guest Image's Docker image SHALL run Docker Engine without
#/ the iptables `raw` table, which the kernel does not have.
if unit_values docker.service Environment | tr ' ' '\n' | grep -qx 'DOCKER_INSECURE_NO_IPTABLES_RAW=1'; then
	ok "Docker Engine runs without the iptables raw table"
else
	fail "docker.service does not set DOCKER_INSECURE_NO_IPTABLES_RAW=1; docker run fails on a kernel with no raw table"
fi

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ The Guest Image's Docker image SHALL contain Go at the version
#/ pinned in its sources, on the `PATH`, with `GOTOOLCHAIN` set to `auto`.
same "go on the PATH" "$(command -v go)" /usr/local/bin/go
same "go" "$(go version 2>/dev/null | awk '{print $3}')" "go${GO_VERSION:-}"
same "go's GOTOOLCHAIN" "$(HOME=$work go env GOTOOLCHAIN 2>/dev/null)" auto
same "GOTOOLCHAIN in /etc/environment" "$(sed -n 's/^GOTOOLCHAIN=//p' /etc/environment)" auto
if [[ $(go version 2>/dev/null) == *"linux/$arch" ]]; then ok "go is built for linux/$arch"; else fail "go is not for linux/$arch: $(go version 2>&1)"; fi

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ The Guest Image's Docker image SHALL start Docker Engine only after
#/ the boot unit that writes its configuration has finished.
enabled flr-registry-mirror.service
if unit_values docker.service After | tr ' ' '\n' | grep -qx flr-registry-mirror.service; then
	ok "docker.service starts after flr-registry-mirror.service"
else
	fail "docker.service is not ordered after flr-registry-mirror.service"
fi
if unit_values docker.service Wants | tr ' ' '\n' | grep -qx flr-registry-mirror.service; then
	ok "docker.service pulls in flr-registry-mirror.service"
else
	fail "docker.service does not want flr-registry-mirror.service"
fi
if unit_values flr-registry-mirror.service After | tr ' ' '\n' | grep -qx network-online.target; then
	ok "flr-registry-mirror.service runs once the wait for the network has ended"
else
	fail "flr-registry-mirror.service does not wait for network-online.target"
fi
# The guest agent still waits for neither (GI-033).
if unit_values guest-agent.service After | tr ' ' '\n' | grep -qE '^(network-online.target|flr-registry-mirror.service|docker.service)$'; then
	fail "the guest agent waits for the network or Docker Engine"
else
	ok "the guest agent waits for neither the network nor Docker Engine"
fi

# mirror ROUTE runs the boot unit's script as systemd would, with ROUTE as
# the output of `ip -4 route show default`, writing under $work/ROOT, and
# prints the daemon.json it wrote.
mirror() {
	local route=$1 out=$work/root
	rm -rf "$out" "${work:?}/bin"
	mkdir -p "$out" "$work/bin"
	printf '#!/bin/sh\nprintf "%%s\\n" "%s"\n' "$route" >"$work/bin/ip"
	chmod +x "$work/bin/ip"
	PATH=$work/bin:$PATH FLR_ROOT=$out /usr/libexec/flr-guest/registry-mirror >"$work/log" 2>&1 ||
		fail "the boot unit's script failed: $(cat "$work/log")"
	cat "$out/etc/docker/daemon.json" 2>/dev/null
}
# valid FILE DESCRIPTION: dockerd accepts FILE.
valid() {
	if dockerd --validate --config-file "$1" >/dev/null 2>&1; then ok "dockerd accepts $2"; else fail "dockerd refuses $2: $(dockerd --validate --config-file "$1" 2>&1)"; fi
}

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ When the guest has a default route once the wait for the network
#/ has ended, the Guest Image's Docker image SHALL configure Docker Engine,
#/ before it starts, with a registry mirror over plain HTTP at port 5000 of
#/ the default route's gateway.
got=$(mirror "default via 10.220.0.1 dev eth1 proto dhcp src 10.220.0.7 metric 1024")
if grep -q '"registry-mirrors": \["http://10.220.0.1:5000"\]' <<<"$got"; then
	ok "with a default route via 10.220.0.1, Docker Engine's mirror is http://10.220.0.1:5000"
else
	fail "with a default route via 10.220.0.1, daemon.json is: $got"
fi
printf '%s\n' "$got" >"$work/with-mirror.json"
valid "$work/with-mirror.json" "the configuration with a mirror"

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ If the guest has no default route once the wait for the network has
#/ ended, then the Guest Image's Docker image SHALL start Docker Engine with
#/ no registry mirror.
got=$(mirror "")
if [ -n "$got" ] && ! grep -q 'registry-mirrors' <<<"$got"; then
	ok "with no default route, Docker Engine has no registry mirror"
else
	fail "with no default route, daemon.json is: $got"
fi
printf '%s\n' "$got" >"$work/no-mirror.json"
valid "$work/no-mirror.json" "the configuration with no mirror"

#= docs/requirements/13-guest-image.md#guest-docker
#= type=test
#/ The Guest Image's Docker image SHALL configure Docker Engine with
#/ the overlay2 storage driver.
for f in "$work/with-mirror.json" "$work/no-mirror.json"; do
	if grep -q '"storage-driver": "overlay2"' "$f" && grep -q '"containerd-snapshotter": false' "$f"; then
		ok "$(basename "$f") keeps the overlay2 storage driver"
	else
		fail "$(basename "$f") does not keep overlay2: $(cat "$f")"
	fi
done
if grep -q '"storage-driver": "overlay2"' /etc/docker/daemon.json && ! grep -q 'registry-mirrors' /etc/docker/daemon.json; then
	ok "the image's own daemon.json, until the first boot, has overlay2 and no mirror"
else
	fail "the image's daemon.json is: $(cat /etc/docker/daemon.json 2>&1)"
fi
valid /etc/docker/daemon.json "the image's own configuration"

#= docs/requirements/13-guest-image.md#guest-agent-protection
#= type=test
#/ The Guest Image's Docker image SHALL run the guest agent with an OOM
#/ score adjustment of -999, in a slice of its own with at least 128 MiB of
#/ memory protected and a CPU weight of 1000.
same "the guest agent's slice" "$(unit_values guest-agent.service Slice | tail -1)" agent.slice
same "the guest agent's OOMScoreAdjust" "$(unit_values guest-agent.service OOMScoreAdjust | tail -1)" -999
same "agent.slice's MemoryMin" "$(unit_values agent.slice MemoryMin | tail -1)" 128M
same "agent.slice's CPUWeight" "$(unit_values agent.slice CPUWeight | tail -1)" 1000

#= docs/requirements/13-guest-image.md#guest-agent-protection
#= type=test
#/ The Guest Image's Docker image SHALL run containerd, Docker Engine
#/ and every container in a slice whose memory limit is 90 percent of the
#/ MicroVM's memory and whose CPU weight is the default of 100.
for unit in containerd.service docker.service; do
	same "$unit's slice" "$(unit_values "$unit" Slice | tail -1)" containers.slice
done
same "containers.slice's MemoryMax" "$(unit_values containers.slice MemoryMax | tail -1)" 90%
same "containers.slice's CPUWeight" "$(unit_values containers.slice CPUWeight | tail -1)" 100
for f in "$work/with-mirror.json" "$work/no-mirror.json" /etc/docker/daemon.json; do
	if grep -q '"cgroup-parent": "containers.slice"' "$f"; then
		ok "$(basename "$f") puts containers in containers.slice"
	else
		fail "$(basename "$f") does not put containers in containers.slice: $(cat "$f")"
	fi
done

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "every check passed"
