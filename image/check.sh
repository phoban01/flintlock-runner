#!/usr/bin/bash
# The Host Image's check stage. It runs inside the built container image, as
# /usr/libexec/flr/check, from the Containerfile's check stage and from
# `make image-check`, and needs neither systemd running, nor KVM, nor a
# block device: it inspects the image and runs the boot scripts against
# temporary directories. Every failure is reported before it exits non-zero.
#
#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image build SHALL run a check stage inside the built
#/ container image that fails unless every component of HI-003 reports its
#/ pinned version and every unit this document requires is enabled.
# The patterns below are literal on purpose.
# shellcheck disable=SC2016
set -uo pipefail
export PATH=/usr/sbin:/usr/bin

VERSIONS=${FLR_VERSIONS:-/usr/share/flr/versions.env}
UNITS=/usr/lib/systemd/system
LIBEXEC=/usr/libexec/flr
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}
skip() { printf 'skip  %s\n' "$*"; }
# expect DESCRIPTION COMMAND...: the command has to succeed.
expect() {
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else fail "$what"; fi
}
# refute DESCRIPTION COMMAND...: the command has to fail.
refute() {
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then fail "$what"; else ok "$what"; fi
}
has() { grep -qE -- "$2" "$1"; }
# code_has PATTERN FILE...: the pattern occurs outside a comment.
code_has() {
  local pattern=$1
  shift
  cat "$@" 2>/dev/null | grep -v "^[[:space:]]*#" | grep -qE -- "$pattern"
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=image/versions.env
. "$VERSIONS" || {
  echo "FAIL  $VERSIONS is missing"
  exit 1
}

# ---------------------------------------------------------------------------
echo "== build"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL be defined by one Containerfile whose base
#/ is a bootc base image pinned by digest.
# The running image is a bootc image: bootc and its ostree layout are here,
# and `bootc container lint` passed in the build. That the base is pinned by
# digest is checked on the Containerfile by `make image-lint` in CI.
expect "bootc is installed" command -v bootc
expect "the image has the ostree layout of a bootc image" test -d /sysroot/ostree

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL be built for the `x86_64` architecture.
for b in containerd runc firecracker jailer cloud-hypervisor-static flintlockd kubelet kubeadm; do
  if file -L "/usr/bin/$b" 2>/dev/null | grep -q 'x86-64'; then
    ok "$b is an x86-64 binary"
  elif ! command -v file >/dev/null 2>&1 && [ "$(od -An -tx1 -j18 -N2 "/usr/bin/$b" | tr -d ' ')" = 3e00 ]; then
    ok "$b is an x86-64 binary (ELF machine 0x3e)"
  else
    fail "$b is not an x86-64 binary"
  fi
done

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL contain containerd, Firecracker with its
#/ jailer, Cloud Hypervisor, `flintlockd`, the kubelet, `kubeadm` and
#/ cloud-init at the versions pinned in one versions file that the
#/ Containerfile reads.
# reports NAME PINNED COMMAND...: the first version-looking token of the
# command's output has to equal the pinned version, "v" prefix or not.
reports() {
  local name=$1 want=${2#v} out got
  shift 2
  out=$("$@" 2>&1 | head -n 5)
  got=$(printf '%s\n' "$out" | grep -Eo 'v?[0-9]+\.[0-9]+(\.[0-9]+)?[0-9A-Za-z.+-]*' | head -n1)
  got=${got#v}
  # cloud-hypervisor reports one more component than its tag: v41.0 is 41.0.0.
  if [ -n "$got" ] && { [ "$got" = "$want" ] || [ "$got" = "$want.0" ]; }; then
    ok "$name reports $got, pinned ${want}"
  elif printf '%s' "$out" | grep -q 'Function not implemented' && [ -n "${FLR_CHECK_BINARY:-}" ] &&
    grep -aqF -- "$want" "$FLR_CHECK_BINARY"; then
    # User-mode emulation lacks system calls some binaries make before they
    # print anything (the jailer calls close_range). The binary could not
    # speak, so its embedded version string is read instead. This says less
    # than running it, and says so; on an x86_64 kernel the branch is not
    # reached.
    ok "$name embeds $want (could not run here: $(printf '%s' "$out" | grep -m1 -o 'Failed to call.*' | cut -c1-80))"
  else
    fail "$name reports '${got:-nothing}', pinned $want: $out"
  fi
}
reports containerd "$CONTAINERD_VERSION" sh -c "containerd --version | awk '{print \$3}'"
# runc is asked under another name. An emulation layer that runs this x86_64
# image on another architecture may answer for a binary called "runc" with
# the machine's own native runc (OrbStack does), and the check has to hear
# from the bytes that were pinned, on every machine.
cp /usr/bin/runc "$work/runc-under-test"
reports runc "$RUNC_VERSION" "$work/runc-under-test" --version
reports firecracker "$FIRECRACKER_VERSION" firecracker --version
FLR_CHECK_BINARY=/usr/bin/jailer reports jailer "$FIRECRACKER_VERSION" jailer --version
reports cloud-hypervisor "$CLOUD_HYPERVISOR_VERSION" cloud-hypervisor-static --version
reports flintlockd "$FLINTLOCK_VERSION" flintlockd version --short
reports kubelet "$KUBERNETES_VERSION" kubelet --version
reports kubeadm "$KUBERNETES_VERSION" kubeadm version -o short
reports cloud-init "$CLOUD_INIT_VERSION" cloud-init --version
expect "containerd has the devmapper snapshotter built in" \
  sh -c "containerd config default | grep -q 'io.containerd.snapshotter.v1.devmapper'"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image build SHALL verify the checksum of every binary
#/ it downloads against a checksum recorded in the versions file and SHALL
#/ fail when one does not match.
# The fetch stage has already refused any mismatch, or this image would not
# exist. What is checked here is that no download can escape it: every
# pinned download has a well-formed checksum beside its version.
for k in CONTAINERD RUNC FIRECRACKER CLOUD_HYPERVISOR FLINTLOCK; do
  sum=${k}_SHA256
  if [[ ${!sum:-} =~ ^[0-9a-f]{64}$ ]]; then ok "$sum is a sha256"; else fail "$sum is not a sha256"; fi
done
if [[ ${KUBERNETES_REPO_KEY_SHA256:-} =~ ^[0-9a-f]{64}$ ]] &&
  echo "$KUBERNETES_REPO_KEY_SHA256  /etc/pki/rpm-gpg/RPM-GPG-KEY-kubernetes" | sha256sum -c - >/dev/null 2>&1; then
  ok "the Kubernetes repository key matches its pinned checksum"
else
  fail "the Kubernetes repository key does not match its pinned checksum"
fi
expect "the Kubernetes repository verifies packages" has /etc/yum.repos.d/kubernetes.repo '^gpgcheck=1$'

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL record the pinned version of each component
#/ of HI-003 as an OCI image label and in a versions file on the image's
#/ `/usr` tree.
# The versions file half. The labels are outside the filesystem; `make
# image-check` compares them with this file after the build.
for k in CONTAINERD_VERSION RUNC_VERSION FIRECRACKER_VERSION CLOUD_HYPERVISOR_VERSION FLINTLOCK_VERSION KUBERNETES_VERSION CLOUD_INIT_VERSION; do
  if [ -n "${!k:-}" ]; then ok "$VERSIONS pins $k=${!k}"; else fail "$VERSIONS pins no $k"; fi
done

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL NOT contain any credential, private key or
#/ token.
leaks=$(grep -rIl -e 'PRIVATE KE[Y]-----' /etc /root /var /home /usr/share/flr /usr/libexec/flr "$UNITS" 2>/dev/null | head -n 20)
if [ -z "$leaks" ]; then ok "no private key under /etc, /var, /root, /home or the image's own files"; else fail "private keys: $leaks"; fi
for p in /etc/ssh/ssh_host_*_key /etc/kubernetes/*.conf /etc/kubernetes/pki /var/lib/kubelet/pki \
  /var/lib/cloud/instance /root/.ssh /root/.aws /root/.kube /root/.docker /etc/flintlock-runner/host.conf; do
  if [ -e "$p" ]; then fail "$p exists in the image"; else ok "no $p"; fi
done
if [ -s /etc/machine-id ] && [ "$(cat /etc/machine-id)" != uninitialized ]; then
  fail "/etc/machine-id is set"
else
  ok "/etc/machine-id is unset"
fi
refute "flintlockd is given no token and no key" grep -Eq -- '--(basic-auth-token|tls-key)' "$UNITS/flintlockd.service"
refute "root has no password" sh -c "awk -F: '\$1 == \"root\" && \$2 !~ /^[!*]/ && \$2 != \"\" { found = 1 } END { exit !found }' /etc/shadow"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image build SHALL complete on a machine that has
#/ neither `/dev/kvm` nor access to AWS.
# This stage is part of the build, and CI runs it on a hosted runner with
# neither. It records what it found rather than requiring their absence.
if [ -e /dev/kvm ]; then skip "/dev/kvm exists here; the build did not use it"; else ok "built and checked without /dev/kvm"; fi
refute "no AWS command line or credentials are needed or present" sh -c 'command -v aws || test -e /root/.aws'

# ---------------------------------------------------------------------------
echo "== units"
enabled() {
  local state
  state=$(systemctl is-enabled "$1" 2>/dev/null)
  case "$state" in
  enabled | enabled-runtime | static | alias | indirect) ok "$1 is $state" ;;
  *) fail "$1 is ${state:-missing}, want enabled" ;;
  esac
}
for u in flr-host-config flr-kvm flr-thin-pool flr-cache flr-network flr-dnsmasq flr-kubelet-config \
  containerd flintlockd kubelet cloud-init-local cloud-config cloud-final; do
  enabled "$u.service"
done
if [ -e "$UNITS/cloud-init-network.service" ]; then enabled cloud-init-network.service; else enabled cloud-init.service; fi
refute "the distribution's dnsmasq.service is not enabled" systemctl is-enabled dnsmasq.service
for s in host-config kvm-gate thin-pool cache-volume network kubelet-config; do
  expect "$LIBEXEC/$s is executable and parses" sh -c "test -x $LIBEXEC/$s && bash -n $LIBEXEC/$s"
done
if command -v systemd-analyze >/dev/null 2>&1; then
  out=$(systemd-analyze verify --man=no "$UNITS"/flr-*.service "$UNITS/flintlockd.service" "$UNITS/containerd.service" 2>&1 |
    grep -E 'flr-|flintlockd|containerd|20-flr' || true)
  if [ -z "$out" ]; then ok "systemd-analyze verify has nothing to say about the image's units"; else fail "systemd-analyze verify: $out"; fi
else
  skip "systemd-analyze is not installed"
fi

# ---------------------------------------------------------------------------
echo "== kernel and KVM"

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL load the `kvm`, `vhost_vsock`,
#/ `dm_thin_pool`, `tun` and `bridge` kernel modules at boot.
kernel=$(find /usr/lib/modules -mindepth 1 -maxdepth 1 -type d | head -n1)
for m in kvm vhost_vsock dm_thin_pool tun bridge; do
  expect "modules-load.d loads $m" has /usr/lib/modules-load.d/flr.conf "^$m\$"
  file_name=${m//_/[_-]}
  if find "$kernel" -name "$file_name.ko*" 2>/dev/null | grep -q . ||
    grep -qE "/${file_name}\.ko" "$kernel/modules.builtin" 2>/dev/null; then
    ok "the image's kernel ships $m"
  else
    fail "the image's kernel $kernel has no $m module"
  fi
done

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ If `/dev/kvm` is absent or unusable at boot, then the Host Image
#/ SHALL NOT start `flintlockd` and SHALL report the Host as not ready with
#/ the reason that KVM is unavailable.
export FLR_RUN=$work/run FLR_NOT_READY_DIR=$work/run/not-ready.d
mkdir -p "$FLR_NOT_READY_DIR"
refute "the KVM gate fails without a KVM device" env FLR_KVM_DEVICE="$work/no-kvm" "$LIBEXEC/kvm-gate"
expect "the KVM gate records that KVM is unavailable" has "$FLR_NOT_READY_DIR/flr-kvm" '^KVM is unavailable: .* does not exist$'
: >"$work/not-a-device"
refute "the KVM gate fails on a KVM node that is no device" env FLR_KVM_DEVICE="$work/not-a-device" "$LIBEXEC/kvm-gate"
expect "the KVM gate says why" has "$FLR_NOT_READY_DIR/flr-kvm" 'is not a character device$'
expect "the KVM gate passes a usable device and clears the reason" \
  sh -c "FLR_KVM_DEVICE=/dev/null $LIBEXEC/kvm-gate && ! test -e $FLR_NOT_READY_DIR/flr-kvm"
expect "flintlockd requires the KVM gate" has "$UNITS/flintlockd.service" '^Requires=.*flr-kvm\.service'
expect "flintlockd starts after the KVM gate" has "$UNITS/flintlockd.service" '^After=.*flr-kvm\.service'

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL NOT disable SELinux globally.
expect "SELinux is configured enforcing" has /etc/selinux/config '^SELINUX=enforcing$'
refute "no kernel argument turns SELinux off" \
  sh -c "grep -rqsE '(selinux=0|enforcing=0)' /usr/lib/bootc/kargs.d /etc/default/grub /usr/lib/bootupd 2>/dev/null"
refute "nothing in the image calls setenforce" code_has setenforce "$LIBEXEC"/[!c]* "$LIBEXEC/cache-volume" "$UNITS"/flr-*.service /etc/cloud/cloud.cfg.d/90-flr.cfg

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ Where a component cannot run under the base image's SELinux
#/ policy, the Host Image SHALL ship a policy module that relaxes confinement
#/ for that component's domain only.
if semodule -l 2>/dev/null | grep -qx flr; then ok "the flr policy module is installed"; else fail "the flr policy module is not installed"; fi
refute "no domain is made permissive by the image" sh -c "semodule -l 2>/dev/null | grep -q '^permissive_'"
if command -v sesearch >/dev/null 2>&1; then
  expect "dnsmasq_t may read flr_run_t" sh -c "sesearch -A -s dnsmasq_t -t flr_run_t -c file -p read | grep -q allow"
else
  skip "sesearch is not installed; the module's rules are not inspected"
fi

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL label the Host environment file and the
#/ not ready reason directory it writes under `/run/flr` so that the Host
#/ Agent's containers can read them, and the Host Service cache directory so
#/ that they can write it, under the base image's SELinux policy and without
#/ changing the domain of any container or the label of any other path.
# The label each path takes from the policy the module was installed into,
# and that the boot scripts label what they write before it takes its name.
# That container_t may read container_ro_file_t and write container_file_t
# is container-selinux's, and is inspected where sesearch is installed.
if "$LIBEXEC/check-selinux-contexts-cases" "$work"; then
  ok "SELinux context cases (labels of the Host Agent's host paths, labelled before rename)"
else
  fail "SELinux context cases"
fi
expect "container-selinux is installed" rpm -q container-selinux

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL configure the container runtime interface
#/ of containerd to run every container of a Kubernetes pod under SELinux
#/ confinement, in the container domain the base image's policy assigns,
#/ rather than unconfined.
# The effective value, as the pinned containerd reads its configuration:
# `config dump` merges the file over the defaults without starting the
# daemon. Where it cannot run, the file itself is read.
# cri_selinux prints enable_selinux of the CRI plugin's own table.
cri_selinux() {
  awk '/^\[/ { t = $0 } t ~ /^\[plugins\."io\.containerd\.grpc\.v1\.cri"\]$/ && $1 == "enable_selinux" { print $3 }' "$1"
}
if containerd --config /etc/containerd/config.toml config dump >"$work/containerd-dump.toml" 2>"$work/containerd-dump.err"; then
  sed -i 's/^[[:space:]]*//' "$work/containerd-dump.toml"
  got=$(cri_selinux "$work/containerd-dump.toml")
  if [ "$got" = true ]; then ok "containerd's effective CRI configuration has enable_selinux = true"; else fail "containerd's effective CRI enable_selinux is '$got'"; fi
else
  skip "containerd config dump cannot run here: $(head -n1 "$work/containerd-dump.err")"
  sed 's/^[[:space:]]*//' /etc/containerd/config.toml >"$work/containerd-file.toml"
  got=$(cri_selinux "$work/containerd-file.toml")
  if [ "$got" = true ]; then ok "/etc/containerd/config.toml sets enable_selinux = true for the CRI plugin"; else fail "/etc/containerd/config.toml sets CRI enable_selinux to '$got'"; fi
fi
# The container domain comes from the base policy's container contexts,
# which containerd reads through go-selinux; nothing in the image overrides
# them.
expect "the base policy assigns containers container_t" has /etc/selinux/targeted/contexts/lxc_contexts '^process = "system_u:system_r:container_t:s0"$'
if command -v sesearch >/dev/null 2>&1; then
  expect "container_t may read container_ro_file_t" sh -c "sesearch -A -s container_t -t container_ro_file_t -c file -p read | grep -q allow"
  expect "container_t may write container_file_t" sh -c "sesearch -A -s container_t -t container_file_t -c file -p write | grep -q allow"
  refute "container_t may not write container_ro_file_t" sh -c "sesearch -A -s container_t -t container_ro_file_t -c file -p write | grep -q allow"
fi

# ---------------------------------------------------------------------------
echo "== Host configuration"
export FLR_HOST_ENV=$work/run/host.env
host_config() { FLR_HOST_CONF=$1 "$LIBEXEC/host-config"; }

#= docs/requirements/11-host-image.md#host-configuration
#= type=test
#/ If the Host configuration file is absent, then the Host Image
#/ SHALL boot with its defaults for every setting.
expect "host-config succeeds with no Host configuration file" host_config "$work/absent.conf"
for kv in FLR_GUEST_SUBNET=172.31.0.0/16 FLR_GATEWAY=172.31.0.1 FLR_PREFIX=16 FLR_NETMASK=255.255.0.0 \
  FLR_DHCP_START=172.31.0.10 FLR_DHCP_END=172.31.255.254 FLR_THIN_POOL_DEVICE= FLR_PROTECTED_CIDRS= \
  FLR_HOST_RESERVE_VCPU=2 FLR_HOST_RESERVE_MEMORY_MB=4096 FLR_HOST_SERVICE_PORTS=1234,3000,5000,3128 \
  FLR_HOST_SERVICE_UIDS=101,1000,10001,10002,100000-165535; do
  expect "default $kv" grep -qxF -- "$kv" "$FLR_HOST_ENV"
done

#= docs/requirements/11-host-image.md#host-configuration
#= type=test
#/ The Host Image SHALL read per-Host settings from one Host
#/ configuration file that cloud-init writes at first boot, containing the
#/ guest subnet, the thin pool device, the protected CIDRs and the Host
#/ reserve.
cat >"$work/host.conf" <<'EOF'
# written by cloud-init
GUEST_SUBNET = "10.200.4.0/22"
THIN_POOL_DEVICE=/dev/nvme1n1
PROTECTED_CIDRS=10.0.0.0/16, 192.168.0.0/16
HOST_RESERVE_VCPU=4
HOST_RESERVE_MEMORY_MB=8192
JOIN_TOKEN=abcdef.0123456789abcdef
EOF
expect "host-config reads a Host configuration file" host_config "$work/host.conf"
for kv in FLR_GUEST_SUBNET=10.200.4.0/22 FLR_GATEWAY=10.200.4.1 FLR_NETMASK=255.255.252.0 \
  FLR_DHCP_START=10.200.4.10 FLR_DHCP_END=10.200.7.254 FLR_THIN_POOL_DEVICE=/dev/nvme1n1 \
  FLR_PROTECTED_CIDRS=10.0.0.0/16,192.168.0.0/16 FLR_HOST_RESERVE_VCPU=4 FLR_HOST_RESERVE_MEMORY_MB=8192; do
  expect "configured $kv" grep -qxF -- "$kv" "$FLR_HOST_ENV"
done
expect "cloud-init is told where the file goes" has /etc/cloud/cloud.cfg.d/90-flr.cfg '/etc/flintlock-runner/host.conf'
expect "host-config waits for cloud-init's write_files stage" has "$UNITS/flr-host-config.service" '^After=.*cloud-init-network\.service'

#= docs/requirements/11-host-image.md#host-configuration
#= type=test
#/ The Host Image SHALL NOT read any secret from the Host
#/ configuration file or from instance user-data.
refute "a key the image does not know never reaches the other units" grep -rq -e JOIN_TOKEN -e abcdef "$work/run"
refute "nothing in the image reads instance user-data" \
  code_has 'user-data|user_data|169\.254\.169\.254/' "$LIBEXEC"/[!c]* "$LIBEXEC/cache-volume" "$UNITS"/flr-*.service
printf 'GUEST_SUBNET=not-a-subnet\n' >"$work/bad.conf"
refute "host-config refuses an invalid guest subnet" host_config "$work/bad.conf"
expect "and records why" has "$FLR_NOT_READY_DIR/flr-host-config" '^host configuration invalid: GUEST_SUBNET'
printf 'PROTECTED_CIDRS=10.0.0.0/16;reboot\n' >"$work/bad.conf"
refute "host-config refuses a value that is not a CIDR list" host_config "$work/bad.conf"
host_config "$work/host.conf" >/dev/null 2>&1
refute "a valid configuration clears the reason" test -e "$FLR_NOT_READY_DIR/flr-host-config"

# ---------------------------------------------------------------------------
echo "== storage"

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ If no device is named and none is detected, then the Host Image
#/ SHALL NOT start `flintlockd` and SHALL report the Host as not ready with
#/ the reason that no thin pool device is available.
# The build container has no instance-store disk, which is the condition.
host_config "$work/absent.conf" >/dev/null 2>&1
if lsblk -dno MODEL 2>/dev/null | grep -q 'Instance Storage'; then
  skip "this machine has an instance-store disk; the no-device case cannot be run"
else
  refute "thin-pool fails when no device is named and none is detected" "$LIBEXEC/thin-pool"
  expect "thin-pool records that no thin pool device is available" \
    has "$FLR_NOT_READY_DIR/flr-thin-pool" '^no thin pool device is available: none is named'
fi
expect "flintlockd requires the thin pool unit" has "$UNITS/flintlockd.service" '^Requires=.*flr-thin-pool\.service'

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ The Host Image SHALL create the containerd devicemapper thin
#/ pool at boot on the block device named in the Host configuration file, or,
#/ when none is named, on the unused instance-store device it detects.
printf 'THIN_POOL_DEVICE=%s\n' "$work/no-such-device" >"$work/named.conf"
refute "host-config refuses a thin pool device outside /dev" host_config "$work/named.conf"
printf 'THIN_POOL_DEVICE=/dev/flr-check-no-such-device\n' >"$work/named.conf"
host_config "$work/named.conf" >/dev/null 2>&1
refute "thin-pool fails when the named device is no block device" \
  env FLR_DEVICE_WAIT=1 "$LIBEXEC/thin-pool"
expect "and names the device in the reason" has "$FLR_NOT_READY_DIR/flr-thin-pool" '/dev/flr-check-no-such-device, named in the Host configuration'
expect "containerd's devmapper snapshotter uses the pool the unit creates" \
  has /etc/containerd/config.toml '^ *pool_name = "flintlock-thinpool"$'
expect "thin-pool detects instance-store disks by model" has "$LIBEXEC/thin-pool" "INSTANCE_STORE_MODEL='Instance Storage'"
expect "the thin pool is ready before containerd starts" has "$UNITS/flr-thin-pool.service" '^Before=containerd\.service'

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ If the thin pool already exists at boot, then the Host Image
#/ SHALL NOT recreate it or wipe its device.
#
#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ The Host Image SHALL NOT select a device that holds a mounted
#/ filesystem or the root volume, and SHALL treat a device that already backs
#/ the thin pool as the thin pool device rather than as in use.
# Both are run against stand-ins for the LVM and block device tools, because
# a build container has no block device to make a pool on. The stand-ins
# describe a Host after its first boot: nvme0n1 is the root disk, nvme1n1 is
# an instance-store disk that carries the pool and the mounted cache volume.
if "$LIBEXEC/check-thin-pool-cases" "$work"; then
  ok "thin-pool cases (existing pool, root disk, mounted disk, pool member)"
else
  fail "thin-pool cases"
fi

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ The Host Image SHALL create the Host Service cache directory on
#/ instance-store storage and SHALL keep all `flintlockd` and containerd
#/ state under `/var`.
expect "the cache volume is a volume of the thin pool's group" has "$LIBEXEC/cache-volume" '^vg=\$FLR_VG$'
expect "the cache directory is /var/lib/flintlock-runner/cache" has "$LIBEXEC/lib.sh" '^FLR_CACHE_DIR=/var/lib/flintlock-runner/cache$'
expect "flintlockd keeps its state under /var" has "$UNITS/flintlockd.service" '--state-dir /var/lib/flintlock '
expect "containerd keeps its state under /var" has /etc/containerd/config.toml '^root = "/var/lib/containerd"$'
expect "the devmapper snapshotter keeps its state under /var" has /etc/containerd/config.toml 'root_path = "/var/lib/containerd/'
expect "tmpfiles.d creates the state directories" has /usr/lib/tmpfiles.d/flr.conf '^d /var/lib/flintlock '

# ---------------------------------------------------------------------------
echo "== networking"
host_config "$work/host.conf" >/dev/null 2>&1
expect "network renders the firewall and the dnsmasq configuration" "$LIBEXEC/network" render "$work/net"
nftf=$work/net/guest-firewall.nft
dnsf=$work/net/dnsmasq.conf

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL create a Linux bridge at boot with the
#/ guest subnet from the Host configuration file and SHALL configure
#/ `flintlockd` to attach TAP interfaces to it.
expect "network creates the bridge" has "$LIBEXEC/network" 'ip link add "\$bridge" type bridge'
expect "the bridge takes the gateway address of the configured subnet" has "$LIBEXEC/network" 'ip addr add "\$FLR_GATEWAY/\$FLR_PREFIX" dev "\$bridge"'
expect "flintlockd attaches TAP interfaces to flbr0" has "$UNITS/flintlockd.service" '--bridge-name flbr0 '
expect "the bridge is named flbr0" has "$LIBEXEC/lib.sh" '^FLR_BRIDGE=flbr0$'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL run a DHCP and DNS service bound to the
#/ bridge so that guests obtain an address, gateway and resolver without
#/ static configuration.
expect "dnsmasq serves the bridge only" has "$dnsf" '^interface=flbr0$'
expect "dnsmasq hands out the subnet" has "$dnsf" '^dhcp-range=10\.200\.4\.10,10\.200\.7\.254,255\.255\.252\.0,12h$'
expect "dnsmasq hands out the gateway" has "$dnsf" '^dhcp-option=option:router,10\.200\.4\.1$'
expect "dnsmasq hands out the resolver" has "$dnsf" '^dhcp-option=option:dns-server,10\.200\.4\.1$'
mkdir -p /run/systemd/resolve /var/lib/dnsmasq
[ -e /run/systemd/resolve/resolv.conf ] || : >/run/systemd/resolve/resolv.conf
expect "dnsmasq accepts the configuration" dnsmasq --test --conf-file="$dnsf"

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL enable IPv4 forwarding and SHALL configure
#/ source NAT from the guest subnet to the Host's primary interface.
expect "sysctl.d enables IPv4 forwarding" has /usr/lib/sysctl.d/90-flr.conf '^net\.ipv4\.ip_forward = 1$'
expect "guests are masqueraded out of the primary interface" has "$nftf" 'ip saddr 10\.200\.4\.0/22 oifname "eth0" masquerade'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to the
#/ EC2 instance metadata service address.
expect "the metadata service is dropped" has "$nftf" 'iifname "flbr0" ip daddr 169\.254\.169\.254 drop'
expect "the IPv6 metadata service is dropped" has "$nftf" 'iifname "flbr0" ip6 daddr fd00:ec2::254 drop'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to the
#/ Host's own kubelet, Pod Provider and metrics ports.
expect "the kubelet, Pod Provider, flintlockd and metrics ports are dropped" \
  has "$nftf" 'iifname "flbr0" tcp dport \{ 9090, 8090, 10248, 10250, 10255, 10256, 10260, 9252, 1338 \} drop'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to every
#/ protected CIDR listed in the Host configuration file.
expect "the protected CIDRs are a set" has "$nftf" 'elements = \{ 10\.0\.0\.0/16, 192\.168\.0\.0/16 \}'
expect "traffic to the protected set is dropped" has "$nftf" 'iifname "flbr0" ip daddr @protected drop'
drop_line=$(grep -n '@protected drop' "$nftf" | cut -d: -f1)
accept_line=$(grep -n 'oifname "eth0" accept' "$nftf" | cut -d: -f1)
expect "the drops come before guests are let out" test "${drop_line:-9999}" -lt "${accept_line:-0}"

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL allow traffic from the guest subnet to the
#/ bridge gateway address only on the Host Service ports and the DHCP and DNS
#/ ports, and SHALL keep every other port on the gateway closed to guests.
expect "DHCP is allowed" has "$nftf" 'iifname "flbr0" udp dport 67 accept'
expect "DNS is allowed on the gateway" has "$nftf" 'iifname "flbr0" ip daddr 10\.200\.4\.1 udp dport 53 accept'
expect "the Host Service ports are allowed on the gateway" \
  has "$nftf" 'iifname "flbr0" ip daddr 10\.200\.4\.1 tcp dport \{ 1234, 3000, 5000, 3128 \} accept'
expect "everything else from the bridge is dropped" has "$nftf" '^[[:space:]]*iifname "flbr0" drop$'
last_input=$(awk '/chain input/ { on = 1 } on && /^\t}/ { exit } on && /(accept|drop)$/ { l = $0 } END { print l }' "$nftf")
expect "the drop is the input chain's last rule" test "$(echo "$last_input" | xargs)" = 'iifname flbr0 drop'
# nft -c needs a netlink socket, which a build container may not grant.
nft_out=$(nft -c -f "$nftf" 2>&1)
case "$?:$nft_out" in
0:*) ok "nft accepts the ruleset" ;;
*"Operation not permitted"* | *"netlink"* | *"Protocol not supported"*) skip "nft cannot open netlink here: $(echo "$nft_out" | head -n1)" ;;
*) fail "nft rejects the ruleset: $nft_out" ;;
esac

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the user ids that run the
#/ Host Services, which it reads from the Host configuration file with
#/ defaults when none are set, to the instance metadata service address and
#/ to the Host's own kubelet, Pod Provider, `flintlockd` and metrics ports.
# The rules, rendered from the installed scripts for the default ids, a
# configured list and values that are no list of ids. That the kernel
# enforces them cannot be shown here, as for HI-063.
if "$LIBEXEC/check-host-service-egress-cases" "$work"; then
  ok "Host Service egress cases (default ids, configured ids, refused values)"
else
  fail "Host Service egress cases"
fi
expect "the Host Services' ids are dropped to the metadata service" \
  has "$nftf" '^[[:space:]]*meta skuid \{ 101, 1000, 10001, 10002, 100000-165535 \} ip daddr 169\.254\.169\.254 counter drop$'
# The ids belong to the Host Services' containers alone: no account of the
# Host itself has one, or the rules would cut a Host daemon off too.
# shellcheck disable=SC2016
clash=$(awk -F: '($3 == 101 || $3 == 1000 || $3 == 10001 || $3 == 10002 || ($3 >= 100000 && $3 <= 165535)) { print $1 "=" $3 }' /etc/passwd /usr/lib/passwd 2>/dev/null | xargs)
if [ -z "$clash" ]; then ok "no account of the Host has a Host Service's user id"; else fail "accounts of the Host with a Host Service's user id: $clash"; fi

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL use a default guest subnet that the Host
#/ configuration file can override, so that it can be kept clear of the
#/ cluster's node, pod and service ranges.
expect "the image has a default guest subnet" has /usr/share/flr/host.conf.defaults '^GUEST_SUBNET=172\.31\.0\.0/16$'
expect "the Host configuration file overrode it above" grep -qxF FLR_GUEST_SUBNET=10.200.4.0/22 "$FLR_HOST_ENV"

# ---------------------------------------------------------------------------
echo "== flintlockd"
fl=$UNITS/flintlockd.service
# The unit without its comments, for checks on what it actually passes.
flx=$work/flintlockd.service
grep -v "^[[:space:]]*#" "$fl" >"$flx"

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL run `flintlockd`, its containerd and every
#/ hypervisor process they start as systemd services outside the cgroup of
#/ any Kubernetes pod.
expect "flintlockd is a systemd service of the image" test -f "$fl"
expect "containerd is a systemd service of the image" test -f "$UNITS/containerd.service"
refute "flintlockd is put in no slice of the kubelet's" grep -Eq '^Slice=.*kubepods' "$fl"
expect "a flintlockd restart leaves its hypervisor processes alone" has "$fl" '^KillMode=process$'
refute "no static pod or manifest runs flintlockd" grep -rqs flintlockd /etc/kubernetes/manifests

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL start `flintlockd` only after the thin
#/ pool and the bridge are present.
for dep in flr-thin-pool flr-network containerd; do
  expect "flintlockd requires $dep" has "$fl" "^Requires=.*$dep\\.service"
  expect "flintlockd starts after $dep" has "$fl" "^After=.*$dep\\.service"
done

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL configure `flintlockd` to listen only on a
#/ local endpoint, a unix socket or a loopback address, that the Pod Provider
#/ of `12-cluster-fleet.md` can reach and a guest cannot.
expect "flintlockd listens on loopback" has "$fl" '--grpc-endpoint 127\.0\.0\.1:9090 '
expect "guests are refused the flintlockd port" has "$nftf" 'tcp dport \{ 9090,'

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL NOT expose `flintlockd` on any address
#/ reachable from outside the Host.
endpoints=$(grep -Eo -- '--(grpc|http|debug)-endpoint [^ ]+' "$fl" | awk '{print $2}')
outside=$(printf '%s\n' "$endpoints" | grep -Ev '^(127\.[0-9.]+|localhost|\[::1\]):[0-9]+$' || true)
if [ -n "$endpoints" ] && [ -z "$outside" ]; then ok "every flintlockd endpoint is loopback: $(echo "$endpoints" | xargs)"; else fail "flintlockd endpoints outside loopback: $outside"; fi
refute "the HTTP gateway stays off" grep -q -- "--enable-http" "$flx"
refute "no flintlockd configuration file overrides the unit" test -e /etc/opt/flintlockd/config.yaml

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL admit connections to the local
#/ `flintlockd` endpoint only from the Pod Provider's user id, which it reads
#/ from the Host configuration file with a default when none is set, and
#/ SHALL refuse them from every other process on the Host.
# The rule the firewall loads, rendered from the installed scripts for the
# default user id, a configured one and values that are no user id. That
# the kernel enforces it cannot be shown here: a build container has no
# netlink for nft to load it with, let alone a second user to connect as.
if "$LIBEXEC/check-flintlockd-access-cases" "$work"; then
  ok "flintlockd access cases (default user id, configured user id, refused values)"
else
  fail "flintlockd access cases"
fi
expect "the rendered firewall admits only the Pod Provider's user id to flintlockd" \
  has "$nftf" '^[[:space:]]*oifname "lo" tcp dport 9090 meta skuid != 10250 counter reject with tcp reset$'
expect "flr-network loads the rule before flintlockd starts" has "$UNITS/flr-network.service" '^Before=flintlockd\.service'

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL enable the `flintlockd` exec API.
expect "the exec API is enabled" has "$fl" '--enable-exec-api '
# Every flag the unit passes has to exist in the pinned flintlockd.
help=$(flintlockd run --help 2>&1)
# shellcheck disable=SC2013
for flag in $(grep -Eo -- "--[a-z][a-z-]+" "$flx" | sort -u); do
  if printf '%s\n' "$help" | grep -q -- "$flag"; then ok "flintlockd $FLINTLOCK_VERSION knows $flag"; else fail "flintlockd $FLINTLOCK_VERSION has no $flag"; fi
done

# ---------------------------------------------------------------------------
echo "== Kubernetes node"

#= docs/requirements/11-host-image.md#kubernetes-node
#= type=test
#/ The Host Image SHALL register its kubelet with the labels
#/ `gitlab-runner.flintlock.dev/host` set to `true`,
#/ `gitlab-runner.flintlock.dev/image` set to a value derived from the image
#/ digest and one label per hypervisor carrying its pinned version.
digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
expect "kubelet-config runs" env FLR_IMAGE_DIGEST=$digest FLR_CPUS=96 FLR_MEM_KIB=394264576 "$LIBEXEC/kubelet-config"
kenv=$work/run/kubelet.env
p=gitlab-runner\\.flintlock\\.dev
expect "label host=true" has "$kenv" "--node-labels=[^ ]*$p/host=true"
expect "label image from the digest" has "$kenv" "--node-labels=[^ ]*$p/image=0123456789abcdef0123456789abcdef(,| )"
expect "label firecracker version" has "$kenv" "--node-labels=[^ ]*$p/firecracker=${FIRECRACKER_VERSION//./\\.}(,| )"
expect "label cloud-hypervisor version" has "$kenv" "--node-labels=[^ ]*$p/cloud-hypervisor=${CLOUD_HYPERVISOR_VERSION//./\\.}(,| )"
expect "the kubelet is started with the labels" has "$UNITS/kubelet.service.d/20-flr.conf" '^ExecStart=/usr/bin/kubelet .*\$FLR_KUBELET_ARGS$'
expect "kubeadm's drop-in is the one 20-flr.conf extends" test -f "$UNITS/kubelet.service.d/10-kubeadm.conf"
expect "kubeadm's drop-in still starts the kubelet the way 20-flr.conf repeats" \
  has "$UNITS/kubelet.service.d/10-kubeadm.conf" '^ExecStart=/usr/bin/kubelet \$KUBELET_KUBECONFIG_ARGS \$KUBELET_CONFIG_ARGS \$KUBELET_KUBEADM_ARGS \$KUBELET_EXTRA_ARGS$'

#= docs/requirements/11-host-image.md#kubernetes-node
#= type=test
#/ The Host Image SHALL configure the kubelet to reserve all CPU
#/ and memory beyond the configured Host reserve, so that the Host's Node
#/ offers only the Host reserve to pods and the Virtual Node of KF-012 offers
#/ the rest to MicroVMs.
# 96 CPUs and 376 GiB with a Host reserve of 4 CPUs and 8192 MiB.
expect "all but the Host reserve is reserved" has "$kenv" -- "--system-reserved=cpu=92000m,memory=$((394264576 - 8192 * 1024))Ki\$"
expect "a machine smaller than the reserve reserves nothing" \
  sh -c "FLR_CPUS=2 FLR_MEM_KIB=1048576 $LIBEXEC/kubelet-config && grep -q -- '--system-reserved=cpu=0m,memory=0Ki' $kenv"

#= docs/requirements/11-host-image.md#kubernetes-node
#= type=test
#/ The Host Image SHALL disable automatic bootc updates, so that
#/ an operating system update is applied only to a drained Host.
for u in bootc-fetch-apply-updates.timer bootc-fetch-apply-updates.service; do
  if [ "$(systemctl is-enabled "$u" 2>/dev/null)" = masked ]; then ok "$u is masked"; else fail "$u is not masked"; fi
done

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "every check passed"
