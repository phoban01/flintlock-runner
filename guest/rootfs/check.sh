#!/usr/bin/env bash
# The check stage of the Guest Image's root filesystem
# (guest/rootfs/Containerfile). It runs inside the built root filesystem,
# under emulation on a platform other than the build machine's, and needs
# neither systemd running, nor KVM, nor a network: it inspects the files and
# runs the tools a Job uses. Every failure is reported before it exits
# non-zero.
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
# expect DESCRIPTION COMMAND...: the command has to succeed.
expect() {
	local what=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$what"; else fail "$what"; fi
}
# unit_lines UNIT KEY: every value of KEY in UNIT's [Unit] section, from the
# unit file and its drop-ins, the way systemd would read them.
unit_lines() {
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

# The architecture the image was built for, as dpkg names it: amd64 or arm64.
arch=$(dpkg --print-architecture)

echo "== root filesystem"

#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Image SHALL consist of a kernel image and a root
#/ filesystem image, each built for `linux/amd64` and `linux/arm64`.
case $arch in
amd64 | arm64) ok "the root filesystem is built for linux/$arch" ;;
*) fail "the root filesystem is built for $arch, not amd64 or arm64" ;;
esac

#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Image build SHALL complete on a machine that has
#/ neither `/dev/kvm` nor a Host.
# This stage is part of the build, and CI runs it on a hosted runner with
# neither. It records what it found rather than requiring their absence.
if [ -e /dev/kvm ]; then ok "/dev/kvm exists here; the build did not use it"; else ok "built and checked without /dev/kvm"; fi
if command -v flintlockd >/dev/null 2>&1 || command -v firecracker >/dev/null 2>&1; then
	fail "the image carries a Host's flintlockd or firecracker"
else
	ok "built and checked with no Host's flintlockd or firecracker"
fi

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Image's root filesystem SHALL be Ubuntu 24.04, with
#/ systemd as its init.
# shellcheck source=/dev/null
os=$(. /etc/os-release && echo "$ID $VERSION_ID")
if [ "$os" = "ubuntu 24.04" ]; then ok "the root filesystem is Ubuntu 24.04"; else fail "the root filesystem is $os, not Ubuntu 24.04"; fi
if [ "$(readlink -f /sbin/init)" = "$(readlink -f /lib/systemd/systemd)" ] && [ -x /lib/systemd/systemd ]; then
	ok "/sbin/init is systemd"
else
	fail "/sbin/init is $(readlink -f /sbin/init), not systemd"
fi

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Image SHALL contain flintlock's guest agent at the
#/ version pinned in its sources and SHALL start it at boot.
got=$(dpkg-query -W -f '${Version}' guest-agent 2>/dev/null)
if [ -n "$got" ] && [ "$got" = "${GUEST_AGENT_VERSION:-}" ]; then
	ok "the guest agent is $got"
else
	fail "the guest agent is '$got', want '${GUEST_AGENT_VERSION:-}'"
fi
expect "the guest agent's binary is installed" test -x "$(sed -n 's/^ExecStart=//p' /usr/lib/systemd/system/guest-agent.service | awk '{print $1}')"
if [ "$(systemctl is-enabled guest-agent.service 2>/dev/null)" = enabled ]; then
	ok "the guest agent starts at boot"
else
	fail "the guest agent is not enabled"
fi

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Image SHALL contain bash at `/bin/bash`, git, curl
#/ and the certificate authority certificates of the Ubuntu archive.
if [ "$(/bin/bash -c 'echo "$BASH_VERSION"' 2>/dev/null)" != "" ]; then ok "/bin/bash runs"; else fail "/bin/bash does not run"; fi
expect "git runs" git --version
expect "git can speak HTTPS" test -x /usr/lib/git-core/git-remote-https
expect "curl runs" curl --version
expect "curl speaks HTTPS" sh -c 'curl --version | grep -qw https'
expect "the CA certificates package is installed" dpkg-query -W ca-certificates
if [ "$(grep -c 'BEGIN CERTIFICATE' /etc/ssl/certs/ca-certificates.crt 2>/dev/null)" -gt 100 ]; then
	ok "the CA bundle holds the archive's certificates"
else
	fail "the CA bundle /etc/ssl/certs/ca-certificates.crt is missing or short"
fi
# A generated Stage script makes the builds and cache directories and
# uses these tools before any Job command runs.
for tool in mkdir rm cat tar gzip sed grep find; do
	expect "$tool is on PATH" command -v "$tool"
done

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Image SHALL contain `gitlab-runner-helper` at
#/ `/usr/local/bin/gitlab-runner-helper`, built for the image's architecture
#/ from the gitlab-runner module at the version that `go.mod` pins.
helper=/usr/local/bin/gitlab-runner-helper
if [ -x "$helper" ]; then
	ok "gitlab-runner-helper is at $helper"
	out=$("$helper" --version 2>&1)
	version=$(sed -n 's/^Version:[[:space:]]*//p' <<<"$out")
	if [ -n "$version" ] && [ "$version" = "${GITLAB_RUNNER_VERSION:-}" ]; then
		ok "gitlab-runner-helper is $version"
	else
		fail "gitlab-runner-helper reports version '$version', want '${GITLAB_RUNNER_VERSION:-}'"
	fi
	if grep -qx "OS/Arch:[[:space:]]*linux/$arch" <<<"$out"; then
		ok "gitlab-runner-helper is built for linux/$arch"
	else
		fail "gitlab-runner-helper is not built for linux/$arch: $(grep '^OS/Arch' <<<"$out")"
	fi
	# The commands the artifact and cache scripts call.
	for cmd in artifacts-uploader artifacts-downloader cache-archiver cache-extractor; do
		expect "gitlab-runner-helper has $cmd" "$helper" "$cmd" --help
	done
else
	fail "no gitlab-runner-helper at $helper"
fi

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Image SHALL record the versions of the guest agent
#/ and of `gitlab-runner-helper` it carries in a versions file on the root
#/ filesystem's `/usr` tree.
for v in GUEST_AGENT_VERSION GITLAB_RUNNER_VERSION; do
	if [ -n "${!v:-}" ]; then ok "$VERSIONS records $v=${!v}"; else fail "$VERSIONS does not record $v"; fi
done

echo "== network"

#= docs/requirements/13-guest-image.md#guest-network
#= type=test
#/ The Guest Image SHALL configure every Ethernet interface
#/ except `eth0` by DHCP for IPv4, with the default route and the DNS servers
#/ of the lease.
#
# systemd-networkd takes the first .network file, in lexical order, whose
# [Match] section matches; match_file IFACE prints it.
match_file() {
	local iface=$1 name f pattern patterns
	while IFS= read -r name; do
		f=/etc/systemd/network/$name
		[ -f "$f" ] || f=/usr/lib/systemd/network/$name
		# read splits the Name= list without expanding its globs.
		read -ra patterns <<<"$(sed -n 's/^Name=//p' "$f" | tr '\n' ' ')"
		for pattern in "${patterns[@]}"; do
			# shellcheck disable=SC2254 # the pattern is a glob on purpose
			case $iface in $pattern) echo "$f"; return 0 ;; esac
		done
	done < <(find /etc/systemd/network /usr/lib/systemd/network -maxdepth 1 -name '*.network' -printf '%f\n' 2>/dev/null | sort -u)
	return 1
}
for iface in eth1 eth2; do
	f=$(match_file "$iface") || { fail "no .network file matches $iface"; continue; }
	if grep -qx 'DHCP=ipv4' "$f" && grep -qx 'Type=ether' "$f"; then
		ok "$iface is configured by DHCP for IPv4 ($(basename "$f"))"
	else
		fail "$iface matches $(basename "$f"), which does not configure it by DHCP"
	fi
	for opt in UseDNS UseRoutes; do
		if grep -qx "$opt=no" "$f"; then fail "$(basename "$f") turns off $opt"; else ok "$iface takes $opt from the lease"; fi
	done
done
for unit in systemd-networkd.service systemd-resolved.service; do
	if [ "$(systemctl is-enabled "$unit" 2>/dev/null)" = enabled ]; then ok "$unit is enabled"; else fail "$unit is not enabled"; fi
done
expect "/etc/resolv.conf becomes systemd-resolved's stub at boot" \
	grep -qx 'L+ /etc/resolv.conf - - - - ../run/systemd/resolve/stub-resolv.conf' /etc/tmpfiles.d/resolv.conf

#= docs/requirements/13-guest-image.md#guest-network
#= type=test
#/ The Guest Image SHALL leave `eth0` unconfigured.
f=$(match_file eth0) || f=''
if [ -n "$f" ] && grep -qx 'Unmanaged=yes' "$f"; then
	ok "eth0 is unmanaged ($(basename "$f"))"
else
	fail "eth0 is not left unmanaged${f:+ ($(basename "$f"))}"
fi

#= docs/requirements/13-guest-image.md#guest-network
#= type=test
#/ If no interface has an IPv4 address 10 seconds after the guest
#/ starts to wait for the network, then the Guest Image SHALL end the wait
#/ and continue to boot.
exec_start=$(unit_lines systemd-networkd-wait-online.service ExecStart | tail -1)
timeout=$(grep -oE -- '--timeout=[0-9]+' <<<"$exec_start" | cut -d= -f2)
if [ -n "$timeout" ] && [ "$timeout" -gt 0 ] && [ "$timeout" -le 10 ]; then
	ok "the wait for the network ends after ${timeout}s"
else
	fail "the wait for the network has no timeout of 10s or less: $exec_start"
fi
if grep -q -- '--any' <<<"$exec_start"; then ok "the wait ends when any interface is up"; else fail "the wait needs every interface: $exec_start"; fi

#= docs/requirements/13-guest-image.md#guest-network
#= type=test
#/ The Guest Image SHALL NOT start the guest agent after the wait
#/ for the network.
order=$(unit_lines guest-agent.service After; unit_lines guest-agent.service Wants; unit_lines guest-agent.service Requires)
if grep -qw 'network-online.target' <<<"$order"; then
	fail "the guest agent waits for network-online.target"
else
	ok "the guest agent does not wait for the network"
fi

#= docs/requirements/13-guest-image.md#guest-network
#= type=test
#/ The Guest Image SHALL contain an empty machine ID, so that each
#/ MicroVM makes its own at boot.
if [ -f /etc/machine-id ] && [ ! -s /etc/machine-id ]; then
	ok "/etc/machine-id is empty"
else
	fail "/etc/machine-id is missing or holds an ID"
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "every check passed"
