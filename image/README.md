# Host Image

The bootc container image every Host of a cluster fleet boots from,
specified in `docs/requirements/11-host-image.md`. It carries what
`internal/fleet/scripts` installs on a running instance over SSH or Systems
Manager: containerd with the devicemapper snapshotter, Firecracker and its
jailer, Cloud Hypervisor, `flintlockd`, the kubelet and `kubeadm`,
cloud-init, and the units that make the thin pool, the guest bridge and the
guest firewall at boot. Nothing is pushed to a Host after it boots.

| Path | What it is |
|------|------------|
| `Containerfile` | The one definition of the image; base pinned by digest |
| `versions.env` | The one versions file: every version and every checksum |
| `build/` | The build steps the Containerfile runs |
| `rootfs/` | Copied to `/`: units, `/usr/libexec/flr` scripts, configuration |
| `selinux/` | The policy module |
| `check.sh`, `check-thin-pool.sh`, `check-flintlockd-access.sh` | The check stage, run inside the built image; the last two run in `make image-lint` too |
| `check-labels.sh`, `lint.sh` | Checks that run outside the image |
| `publish-ami.sh` | AMI publishing, on request only |

## Building

```sh
make image          # build for x86_64; the check stage is part of the build
make image-check    # run the checks again in the built image, compare its labels
make image-lint     # bash -n, shellcheck, digest pin, thin-pool and flintlockd access cases; no build
```

`make image` uses podman when it is installed and docker otherwise
(`CONTAINER_ENGINE=`), tags `localhost/flintlock-runner-host:dev`
(`HOST_IMAGE=`) and passes every `*_VERSION` of `versions.env` as a build
argument so that the Containerfile can make OCI labels of them. A build
without those arguments, or with one that disagrees with the file, fails.

The build needs network access to quay.io, github.com, pkgs.k8s.io and the
Fedora mirrors. It needs neither `/dev/kvm` nor AWS. On a machine that is
not x86_64 it runs under emulation, except for the `fetch` stage, which only
downloads and therefore runs natively.

The published image depends on a file that only the check stage produces, so
no builder can skip that stage: an image that fails its checks does not
exist.

### Changing a version

Edit `versions.env`, and only there. Every download has a `*_SHA256` beside
its version; take it from the checksum file the project publishes with the
release, or compute it from the downloaded artifact, and never from anywhere
else. A version bumped without its checksum fails the build. To move the
base image, resolve the new digest from the registry
(`skopeo inspect --raw docker://quay.io/fedora/fedora-bootc:44 | sha256sum`,
or the tag listing of quay.io) and replace it in both `FROM` lines of the
Containerfile.

The kubelet and `kubeadm` are RPMs from pkgs.k8s.io, verified by dnf against
the repository key, and the key is the pinned download. cloud-init is a
Fedora RPM, verified by signature; the build asks for exactly
`CLOUD_INIT_VERSION` and fails when the Fedora repositories no longer carry
it, which is the moment to bump it.

Kubernetes is pinned to the v1.35 line because it is the last that supports
containerd 1.x, and containerd is pinned where the push-provisioned fleet
has it.

## What happens at boot

| Unit | Does | Requirement |
|------|------|-------------|
| `systemd-modules-load` (`modules-load.d/flr.conf`) | loads `kvm`, `vhost_vsock`, `dm_thin_pool`, `tun`, `bridge` | HI-010 |
| `flr-host-config` | validates the Host configuration file, writes `/run/flr/host.env` | HI-050 to HI-052 |
| `flr-kvm` | refuses unless `/dev/kvm` opens for reading and writing | HI-011 |
| `flr-thin-pool` | creates the thin pool once; leaves an existing one alone | HI-020 to HI-023 |
| `flr-cache` | mounts the Host Service cache volume | HI-024 |
| `flr-network` | bridge `flbr0`, forwarding, NAT, guest firewall, `flintlockd` for the Pod Provider's user id only | HI-030, HI-032 to HI-037, HI-063 |
| `flr-dnsmasq` | DHCP and DNS on the bridge | HI-031 |
| `containerd` | one containerd for the kubelet and for `flintlockd` | HI-040 |
| `flintlockd` | `Requires=` the KVM gate, the thin pool, the bridge and containerd | HI-040 to HI-044 |
| `flr-kubelet-config` | writes the kubelet's labels and reservation | HI-060, HI-061 |
| `kubelet` | started by `kubeadm join` from the bootstrap configuration | |

`bootc-fetch-apply-updates.timer` and its service are masked (HI-062).

`flintlockd` v0.15.1 can listen on TCP only, so its local endpoint is
`127.0.0.1:9090`, without TLS or a token, with the exec API on and the HTTP
gateway off. The Pod Provider reaches it there because the Host Agent runs in
the Host's network namespace. A guest cannot: the guest firewall drops
everything that arrives on the bridge except DHCP, DNS and the Host Service
ports on the gateway. Of the Host's own processes, only the Pod Provider's
user id can: see [The Pod Provider's user id](#the-pod-providers-user-id).

## Host configuration file

`/etc/flintlock-runner/host.conf`, written by cloud-init at first boot from
the bootstrap configuration of the MachineDeployment (KF-003). `KEY=value`
lines, `#` comments, optional quotes. The file is parsed, never sourced. A
key that is absent, or the whole file when it is absent, takes the default
from `/usr/share/flr/host.conf.defaults`. Unknown keys are skipped and their
values are not kept. It holds settings and no secrets: nothing on the Host
reads a credential from it or from user-data.

| Key | Default | Meaning |
|-----|---------|---------|
| `GUEST_SUBNET` | `172.31.0.0/16` | The bridge's IPv4 subnet, /29 or larger. The first address is the gateway. Keep it clear of the cluster's node, pod and service ranges |
| `THIN_POOL_DEVICE` | empty | Block device for the thin pool. Empty means detect the unused instance-store disk |
| `PROTECTED_CIDRS` | empty | Comma-separated IPv4 CIDRs no guest may reach: the node and pod CIDRs of the cluster, and anything else |
| `HOST_RESERVE_VCPU` | `2` | CPUs the Host's own Node offers to pods |
| `HOST_RESERVE_MEMORY_MB` | `4096` | Memory the Host's own Node offers to pods |
| `HOST_SERVICE_PORTS` | `1234,3000,5000,3128` | TCP ports guests may reach on the gateway |
| `HOST_CONTROL_PORTS` | `9090,8090,10248,10250,10255,10256,10260,9252,1338` | The Host's own ports, dropped for guests by name as well as by the final drop |
| `CACHE_VOLUME_PERCENT` | `15` | Share of the volume group for the cache volume, when first created |
| `POD_PROVIDER_UID` | `10250` | The one user id that may connect to `flintlockd`; 1 to 4294967294, never 0. See below |

```yaml
# in the KubeadmConfigTemplate
files:
  - path: /etc/flintlock-runner/host.conf
    permissions: "0644"
    content: |
      GUEST_SUBNET=10.200.0.0/16
      PROTECTED_CIDRS=10.0.0.0/16,192.168.0.0/16
      HOST_RESERVE_VCPU=2
      HOST_RESERVE_MEMORY_MB=4096
```

An invalid file fails `flr-host-config`, and with it every unit that needs
the settings, `flintlockd` included; the reason is reported as below.

### The thin pool device

With `THIN_POOL_DEVICE` empty, the device is a disk that already belongs to
the `flintlock` volume group, or else the first whole disk whose model says
`Instance Storage`, that holds no mounted filesystem, is not under the root
filesystem and is blank. EBS volumes are never detected; name one to use it.
A named device that turns out to be the root disk, which Nitro's NVMe
enumeration makes possible, is replaced by the other unused NVMe disk when
there is exactly one and refused otherwise. A device with any filesystem or
partition signature is refused, never wiped. Once `flintlock/thinpool`
exists the unit does nothing but activate it, so the pool, the images in it
and the cache volume survive a reboot and an in-place upgrade.

The volume group is laid out as the pool (80%), its metadata (1%), the cache
volume (`CACHE_VOLUME_PERCENT`, ext4, mounted at
`/var/lib/flintlock-runner/cache`) and 4% left free for the pool's
autoextension.

### The Pod Provider's user id

`flintlockd` has no authentication of its own, and a loopback port is open
to every process in the Host's network namespace, which includes every pod
with host networking. So the guest firewall (`flr-network`, table
`inet flr`) has an output chain with one rule:

```
oifname "lo" tcp dport 9090 meta skuid != <POD_PROVIDER_UID> counter reject with tcp reset
```

Every connection to port 9090 over loopback, to any address, from a socket
owned by any other user id is reset before it is made: root's, a Host
Service's, a DaemonSet's. `flintlockd`'s replies come from port 9090 and
pass. The rule is loaded with the rest of the firewall before `flintlockd`
starts (HI-063).

This is the contract the Fleet Manifests follow (KF-135):

- The Pod Provider's container runs with `runAsUser` equal to
  `POD_PROVIDER_UID`, `10250` unless the Host configuration file says
  otherwise, and with `runAsNonRoot: true`. A value changed in the
  bootstrap configuration has to be changed in the manifests too; nothing
  checks that the two agree except the Virtual Node, which stays not ready
  because `flintlockd` refuses the provider.
- No other container of the Host Agent, and nothing else scheduled onto a
  Host, runs as that user id. The id is chosen to be outside the ranges base
  images default to (`65532` for distroless, `65534` for nobody) and
  outside systemd's dynamic users; keep it that way.
- The Host Agent runs in the Host's network namespace (KF-071), which
  rules out a user namespace for the pod, so the id in the pod is the id
  the Host's kernel sees. A pod in a user namespace would present a
  different id and be refused.
- The container needs no capability to connect: port 9090 is not privileged
  and the rule matches the socket's owner only. Files it reads, the kubelet
  API's certificates and its ServiceAccount token, have to be readable by
  that user id (`fsGroup` or `defaultMode`).

An operator who needs to talk to `flintlockd` on a Host by hand does it as
that user id, for example with `setpriv --reuid=10250 --regid=10250
--clear-groups`. The checks render the rule for the default and for a
configured user id and refuse `0` and anything that is not a user id; where
unprivileged user and network namespaces are available (not in the image
build) they also load it and show a connection from the configured id let
through and one from root or any other id refused. On a booted Host,
`nft list chain inet flr output` shows the rule and its counter.

A unix socket with file permissions would be the better boundary, and is the
one to move to when `flintlockd` can listen on one.

### The kubelet

`flr-kubelet-config` writes `/run/flr/kubelet.env`, and
`kubelet.service.d/20-flr.conf` appends it after kubeadm's arguments:

- `--node-labels=gitlab-runner.flintlock.dev/host=true,.../image=<id>,.../firecracker=<version>,.../cloud-hypervisor=<version>`,
  where `<id>` is the first 32 hexadecimal digits of the booted image's
  digest from `bootc status`, or `unknown` when bootc reports none.
- `--system-reserved=cpu=…,memory=…`: the machine's capacity minus the Host
  reserve, so that the Node's allocatable is the Host reserve (less the
  kubelet's own eviction threshold).

Because the flag comes last it replaces a `--node-labels` given through
`kubeletExtraArgs` in the kubeadm join configuration. Put other labels on
the Node through the API instead. The Host taint of KF-002 belongs in the
join configuration and is untouched.

## Not-ready reasons

A unit that refuses to let `flintlockd` start says why in a file:

```
/run/flr/not-ready.d/<unit>      one line, the reason, newline-terminated
```

- The file's name is the unit's name without `.service`.
- The file exists exactly while the condition holds. The unit writes it
  atomically before it fails and removes it when it next succeeds. The
  directory is on `/run`, so a reboot starts clean.
- An empty or absent directory means no unit has anything to report. It does
  not by itself mean `flintlockd` is healthy.
- The Pod Provider reports every file as the Virtual Node's not ready
  message (KF-016), for example joined as `<unit>: <reason>`. It should
  read the directory on each status update rather than watch it once.

| File | First words of the reason | When |
|------|---------------------------|------|
| `flr-kvm` | `KVM is unavailable: ` | `/dev/kvm` is missing, not a character device, or cannot be opened (HI-011) |
| `flr-thin-pool` | `no thin pool device is available: ` | none named and none detected, or the named one is missing, mounted, the root disk, or not blank (HI-022, HI-023) |
| `flr-host-config` | `host configuration invalid: ` | the Host configuration file does not parse or validate |

The units are oneshots and do not retry. After fixing the cause,
`systemctl restart flr-thin-pool flintlockd` (or a reboot) clears it.

## SELinux

The image keeps the base image's policy enforcing (HI-012): it does not
touch `/etc/selinux/config`, adds no `selinux=0` or `enforcing=0` kernel
argument, never calls `setenforce` and makes no domain permissive.

- **containerd and the kubelet** are labelled by the base image's
  `container-selinux` (`container_runtime_exec_t`, `kubelet_exec_t`) and run
  in the domains that package gives them.
- **`flintlockd`, Firecracker, Cloud Hypervisor and the `/usr/libexec/flr`
  scripts** are `bin_t` under `/usr/bin` and `/usr/libexec`. Started by
  systemd they run as `unconfined_service_t`, which is how the targeted
  policy runs any service it has no module for. That is a property of the
  base policy, not a relaxation by this image, and it is per service: every
  confined domain stays confined. Writing a confining policy for the
  hypervisor processes is future work and needs a booted Host to develop
  against.
- **dnsmasq** is the one component the base policy stops (HI-013). It runs
  confined as `dnsmasq_t`, which may read its configuration only under
  `/etc`. The image generates that configuration at every boot and keeps it
  in `/run/flr`. The module `selinux/flr.te` gives `/run/flr` its own type,
  `flr_run_t`, and allows `dnsmasq_t` to search the directory and read the
  files in it. Nothing else changes for any domain.

The module is compiled in the build against the base image's policy and
installed with `semodule -n`. It has not been exercised on a booted Host:
the first boot on real hardware should be followed by
`ausearch -m avc -ts boot`, and anything it shows is a bug in this section.

## Networking notes

- `br_netfilter` is loaded because kubeadm's preflight wants it. With it,
  bridged guest-to-guest frames traverse the forward chain, whose last rule
  drops traffic into the bridge that is not a reply, so guests cannot reach
  each other. That is intended.
- kube-proxy may set `route_localnet`, so the loopback endpoint of
  `flintlockd` is protected by the input chain's drop rather than by the
  kernel's martian check alone.
- The default guest subnet is this repository's
  `config.DefaultGuestSubnet`, which is also the CIDR of an AWS default VPC.
  Override it wherever the cluster lives in that range.

## Publishing an AMI

```sh
make image-ami IMAGE_AMI_ARGS="--bucket my-import-bucket --region eu-west-1"
```

Runs `bootc-image-builder --type ami`, which uploads the disk image to the S3
bucket, imports it as a snapshot through the `vmimport` service role and
registers the AMI, then tags the AMI with
`gitlab-runner.flintlock.dev/image-digest` and
`gitlab-runner.flintlock.dev/kubernetes-version`. The operator creates the
bucket and the role once. The image has to be in root's podman storage with
a digest, which means pushed to a registry and pulled, or `IMAGE_DIGEST` set.
`--dry-run` prints the commands instead of running them. Nothing else in
this directory needs AWS, and nothing runs this unless asked.

This script has not been run: the project has no AWS account yet.

## Upgrading

Until in-place upgrade is specified, an upgrade is a new AMI and a
MachineDeployment rollout. The image is nevertheless built for the in-place
path: automatic updates are masked, all state is under `/var`, which
`bootc upgrade` does not touch, and an existing thin pool and cache volume
are kept, so a drained Host that runs `bootc upgrade` and reboots comes back
with its pulled images and caches. The `image` label changes with the
digest, which is how a rollout can be observed.
