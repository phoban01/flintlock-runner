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
| `check.sh`, `check-thin-pool.sh`, `check-flintlockd-access.sh`, `check-host-service-egress.sh`, `check-selinux-contexts.sh` | The check stage, run inside the built image; all but `check.sh` run in `make image-lint` too |
| `check-labels.sh`, `lint.sh` | Checks that run outside the image |
| `publish-ami.sh` | AMI publishing, on request only |

## Building

```sh
make image          # build for x86_64; the check stage is part of the build
make image-check    # run the checks again in the built image, compare its labels
make image-lint     # bash -n, shellcheck, digest pin, thin-pool, flintlockd access, Host Service egress and SELinux context cases; no build
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
| `flr-host-config` | validates the Host configuration file, writes `/run/flr/host.env`, labelled for the Host Agent | HI-050 to HI-052, HI-065 |
| `flr-kvm` | refuses unless `/dev/kvm` opens for reading and writing | HI-011 |
| `flr-thin-pool` | creates the thin pool once; leaves an existing one alone | HI-020 to HI-023 |
| `flr-cache` | mounts the Host Service cache volume and labels it for the Host Agent | HI-024, HI-065 |
| `flr-network` | bridge `flbr0`, forwarding, NAT, guest firewall, `flintlockd` for the Pod Provider's user id only, the Host Services' egress | HI-030, HI-032 to HI-037, HI-063, HI-064 |
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
| `HOST_SERVICE_UIDS` | `101,1000,10001,10002,100000-165535` | The user ids of the Host Services, ids and ranges `LOW-HIGH`, which may reach neither the metadata service nor the control ports; never 0 or the Pod Provider's, no overlaps. See below |

No port may be both a Host Service port and a control port.

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

### The Host Services' user ids

The Host Services run in the Host's network namespace, so the guest
firewall's forward chain never sees their traffic, and `buildkitd` runs the
`RUN` steps of every Job's image build. HI-064 gives them what SE-030 and
SE-031 gave the push-provisioned `buildkit` user: the output chain of
`inet flr` drops, for the socket owners in `HOST_SERVICE_UIDS`,

```
meta skuid { <ids> } ip daddr 169.254.169.254 counter drop
meta skuid { <ids> } ip6 daddr fd00:ec2::254 counter drop
meta skuid { <ids> } oifname "lo" tcp dport { <HOST_CONTROL_PORTS>, 9090 } counter drop
```

Every address of the Host, the bridge gateway and the primary address
included, is reached over `lo`, so the last rule closes the kubelet, the Pod
Provider, `flintlockd` and the metrics ports on all of them. Nothing else is
taken from the Host Services: they still reach the internet, each other on
the gateway's Host Service ports, and nginx still reaches Athens on
`127.0.0.1:3999`. `flintlockd`'s port is added even when
`HOST_CONTROL_PORTS` leaves it out, and the HI-063 rule refuses it to them
anyway.

The default ids are the ones the Fleet Manifests run the Host Services as:
nginx 101 (the Go module proxy's front and the HTTP cache), `buildkitd`
1000, Athens 10001 and zot 10002; `make manifests-check` compares them.
`100000-165535` is the subordinate id range of the rootless buildkit image's
user (`/etc/subuid` in `moby/buildkit:v0.23.2-rootless`): a build step that
runs as root in the build is 1000 on the Host, and one that runs as any other
user is an id in that range. No account of the Host Image has any of these
ids, which the check stage verifies. A hostNetwork pod of another workload
that happens to run as one of them is held to the same rules.

`image/check-host-service-egress.sh` renders the rules for the defaults, a
configured list and invalid values, and where it can make an unprivileged
user and network namespace with nftables (a developer machine, not the image
build), loads them and makes connections as the Host Services' ids: to the
metadata service, v4 and v6, and the kubelet, Pod Provider and metrics
ports, dropped; to `127.0.0.1:3999`, the gateway's registry mirror port and
an internet address, let through. On a booted Host,
`nft list chain inet flr output` shows the counters.

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
- **The Host Agent's host paths** (HI-065). The Host Agent's containers
  run as `container_t`, which may use only files labelled for containers.
  The module's file contexts (`selinux/flr.fc`) label the three paths it
  mounts with types the base image's `container-selinux` already grants
  `container_t`, and no rule in the module names a container domain:

  | Path | Label | `container_t` may |
  |------|-------|-------------------|
  | `/run/flr/host.env` | `container_ro_file_t:s0` | read |
  | `/run/flr/not-ready.d` and the reasons in it | `container_ro_file_t:s0` | read |
  | `/var/lib/flintlock-runner/cache` and below | `container_file_t:s0` | read and write |

  Everything else under `/run/flr` stays `flr_run_t`, and nothing else is
  relabelled.

### Why these labels

The read-only paths are `container_ro_file_t` rather than
`container_file_t`: the policy lets containers read that type and write
none of it, so a compromised Host Agent container still cannot forge the
bridge gateway address or clear a not ready reason even if its mount were
writable. The cache has to be written, so it is `container_file_t`.

The level is `s0`, with no categories, on all three. Every container runs at
`s0` plus two categories of its own, and the MCS constraint lets a process
use a file only when its level dominates the file's; `s0` is dominated by
every level, so each container of the Host Agent can use the paths whatever
categories it gets. The tradeoff is that `s0` is not private to the Host
Agent: any other container given these paths by a hostPath mount could read
them, and write the cache. Only the Host Agent mounts them, hostPath mounts
are for privileged workloads in a cluster that enforces Pod Security, and
the files hold nothing secret; per-pod categories would instead be lost on
every restart of the pod. Running the Host Agent as `spc_t` would work too,
and would remove SELinux from between the Host Services and the Host.

What the Host Services create in the cache carries the creating process's
level, not the directory's. So that a replaced Host Agent pod can read and
hand over what its predecessor wrote, the DaemonSet fixes its pod's level
to one pair of categories (`seLinuxOptions.level: s0:c311,c827` in
`deploy/host-agent/daemonset.yaml`) instead of taking random ones; the
domain is still `container_t`. A pod with random categories cannot read
what the Host Agent writes there.

### Keeping the labels

`/run` is a tmpfs, rebuilt at every boot, and a rename keeps the label of
the file renamed. So the labels come from the module's file contexts and
are applied where each path is made:

- systemd-tmpfiles labels `/run/flr/not-ready.d` when it creates it, and
  `z` lines in `tmpfiles.d/flr.conf` restore the three labels, without
  recursing, whenever tmpfiles runs and the paths exist.
- `flr-host-config` writes `host.env` to a temporary file in `/run/flr`,
  which would be `flr_run_t`, and runs `restorecon -F` on it before the
  rename; the file contexts give the temporary names `.host.env.*` the same
  label. A not ready reason is labelled the same way before it takes its
  name, and the directory when a unit has to make it.
- The cache directory is the root of the cache volume's ext4 filesystem,
  whose label lives on the volume. `flr-cache` runs `restorecon -F` on it
  after every mount, and when it finds it mounted. Below the root, files
  inherit the type from their directory.

`restorecon` runs only where SELinux is enabled, so the check stage and
`make image-lint` run the same scripts. `check-selinux-contexts.sh` checks
the ordering with a stand-in for `restorecon`, and in the check stage looks
every path up with `matchpathcon` in the policy the module was installed
into.

### What has been verified

The module compiles against the base image's policy and installs with
`semodule -n`, and `matchpathcon` in that policy gives each path above its
label and leaves the rest of `/run/flr` `flr_run_t`. `sesearch` on that
policy shows `container_t` (a `svirt_sandbox_domain` and an
`mcs_constrained_type`) may read `container_ro_file_t` and not write it, and
may read and write `container_file_t`. None of it has been enforced on a
booted Host: the first boot on real hardware should be followed by
`ausearch -m avc -ts boot`, and anything it shows is a bug in this section.

### Pods run confined (HI-066)

containerd labels a pod only when its CRI plugin is told to; without it
every container the kubelet starts is unlabelled and runs unconfined, and an
enforcing Host enforces nothing between its pods and itself.
`/etc/containerd/config.toml` sets `enable_selinux = true` in
`[plugins."io.containerd.grpc.v1.cri"]`, the `PluginConfig.EnableSelinux`
of containerd v1.7.22's CRI plugin (`pkg/cri/config/config.go`; false by
default, when the plugin calls `selinux.SetDisabled()`). With it, every
container of every pod the kubelet starts on a Host runs in the domain the
base policy's `lxc_contexts` names, `container_t`, at a level with
categories of its own, unless the pod's `seLinuxOptions` ask for another
level or type. That covers the Host Agent (at its fixed level), the CNI
and kube-proxy DaemonSets, and anything else scheduled onto a Host. The
one exception is containerd's own: a container with `privileged: true`
gets no label (`pkg/cri/sbserver/container_create.go`) and runs unconfined,
as a privileged container is meant to. kube-proxy and most CNI agents are
privileged; the Host Agent is not.

The MicroVMs are not affected. `flintlockd` v0.15.1 talks to containerd's
own API for its content store, images, snapshots and leases only; it never
creates a containerd container or task (`NewContainer` appears only in its
client interface and mock), and it starts Firecracker and Cloud Hypervisor
itself as child processes (`process.DetachedStart` in
`infrastructure/microvm/firecracker/create.go`, `exec.Command` in
`infrastructure/microvm/cloudhypervisor/create.go`). `enable_selinux` is
read by the CRI plugin alone, so the hypervisor processes keep the
`unconfined_service_t` of `flintlockd.service` described above.

The check stage runs `containerd config dump` in the image, which merges the
file over containerd's defaults without starting it, and requires the CRI
plugin's `enable_selinux` to be `true`; it also checks that the base
policy's container process context is `container_t`.

Not verified until a Host boots: that containerd starts with the setting on
an enforcing kernel, that the Host Agent's containers show
`system_u:system_r:container_t:s0:c311,c827` (`ps -eZ`), that other pods get
categories of their own, and that no AVC denials follow
(`ausearch -m avc -ts boot`). An unprivileged CNI or storage DaemonSet that
touches host paths is the most likely thing to need its own
`seLinuxOptions`. The Host Agent's `buildkitd` names `container_engine_t`
(KF-139), the base policy's domain for a container engine in a container;
see "Known gaps" in `deploy/README.md`.

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
