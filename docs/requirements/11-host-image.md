# Host Image {#host-image}

This document specified the Host Image: the bootable container image, built
with bootc, from which every Host of a cluster fleet boots. The Host Image
is now battery-operator's reference Host Image
([battery-operator ADR 0007](https://github.com/phoban01/battery-operator/blob/main/docs/adr/0007-reference-host-image-and-cluster-api.md)),
in its `hostimage/`, with its requirements in its
[`docs/requirements/11-host-image.md`](https://github.com/phoban01/battery-operator/blob/main/docs/requirements/11-host-image.md).
flintlock-runner runs as pods, and its Host Services run in the Host Agent
DaemonSet, so it needs no image of its own. #107 removed `image/`.

Every requirement below is withdrawn. Each keeps its number and names what
replaces it. "battery-operator HI-0nn" is the requirement of that number in
battery-operator's `11-host-image.md`, which kept the numbers of this
document. The KF requirements are in `12-cluster-fleet.md`.

Three parts of this image were flintlock-runner's own, and they now live
with the Fleet Manifests:

- The Host Service cache. `flr-cache` made a cache volume on the
  instance-store disk, and the Host Agent mounted it as a hostPath. The
  Host Agent now keeps the cache in an `emptyDir` volume of its own pod
  (KF-197).
- The Host Service ports. The image opened them on the bridge gateway by
  default. battery-operator's image opens the gateway service ports that
  the Host configuration file names (battery-operator HI-078), and every
  Host pool of a cluster fleet sets them (KF-198).
- The Host Service user ids. The image dropped their traffic to the
  metadata service and the control ports by default. battery-operator's
  image drops the traffic of the gateway service user ids that the Host
  configuration file names (battery-operator HI-080). Every Host pool sets
  them (KF-198), and the Host Agent starts no Host Service on a Host that
  leaves one out (KF-199).

The Host label changed too. battery-operator's image and its Cluster API
templates register every Host with `battery.liquidmetal-x.dev/host=true`
(battery-operator HI-074, HP-020), and the Fleet Manifests select that
label. Nothing selects `gitlab-runner.flintlock.dev/host` any more, so no
Host carries it.

## Build {#image-build}

- **HI-001** (withdrawn) Replaced by battery-operator HI-001.
- **HI-002** (withdrawn) Replaced by battery-operator HI-002.
- **HI-003** (withdrawn) Replaced by battery-operator HI-003.
- **HI-004** (withdrawn) Replaced by battery-operator HI-004.
- **HI-005** (withdrawn) Replaced by battery-operator HI-005.
- **HI-006** (withdrawn) Replaced by battery-operator HI-006.
- **HI-007** (withdrawn) Replaced by battery-operator HI-007.
- **HI-008** (withdrawn) Replaced by battery-operator HI-008.
- **HI-009** (withdrawn) Replaced by battery-operator HI-009, which
  converts the published Host Image to an AMI.

## Kernel and KVM {#kernel-and-kvm}

- **HI-010** (withdrawn) Replaced by battery-operator HI-010.
- **HI-011** (withdrawn) Replaced by battery-operator HI-011.
- **HI-012** (withdrawn) Replaced by battery-operator HI-012.
- **HI-013** (withdrawn) Replaced by battery-operator HI-013.
- **HI-065** (withdrawn) Replaced by battery-operator HI-065 for the Host
  environment file and the not ready reason directory, now under
  `/run/battery`. The Host Service cache is no longer a path of the Host:
  it is the Host Agent's `emptyDir` volume, which the kubelet labels for
  the pod (KF-197).
- **HI-066** (withdrawn) Replaced by battery-operator HI-066.
- **HI-072** (withdrawn) Replaced by battery-operator HI-072.

## Storage {#image-storage}

- **HI-020** (withdrawn) Replaced by battery-operator HI-020.
- **HI-021** (withdrawn) Replaced by battery-operator HI-021.
- **HI-022** (withdrawn) Replaced by battery-operator HI-022.
- **HI-023** (withdrawn) Replaced by battery-operator HI-023.
- **HI-024** (withdrawn) Replaced by battery-operator HI-024 for the state
  of `flintlockd` and containerd under `/var`, and by KF-197 for the Host
  Service cache.

## Networking {#image-networking}

- **HI-030** (withdrawn) Replaced by battery-operator HI-030.
- **HI-031** (withdrawn) Replaced by battery-operator HI-031.
- **HI-032** (withdrawn) Replaced by battery-operator HI-032.
- **HI-033** (withdrawn) Replaced by battery-operator HI-033.
- **HI-034** (withdrawn) Replaced by battery-operator HI-034, which adds
  `flintlockd`'s port to the ports a guest cannot reach.
- **HI-035** (withdrawn) Replaced by battery-operator HI-035.
- **HI-036** (withdrawn) Replaced by battery-operator HI-036 and HI-078:
  the gateway opens the gateway service ports of the Host configuration
  file, which are the Host Service ports when a Host pool sets them as
  KF-198 says.
- **HI-037** (withdrawn) Replaced by battery-operator HI-037.
- **HI-064** (withdrawn) Replaced by battery-operator HI-080, for the
  gateway service user ids of the Host configuration file. KF-198 makes
  them the Host Services' user ids, and KF-199 keeps the Host Services off
  a Host that leaves one out.
- **HI-075** (withdrawn) Replaced by battery-operator HI-075.
- **HI-076** (withdrawn) Replaced by battery-operator HI-076.
- **HI-077** (withdrawn) Replaced by battery-operator HI-077.

## flintlockd {#image-flintlockd}

- **HI-040** (withdrawn) Replaced by battery-operator HI-040.
- **HI-041** (withdrawn) Replaced by battery-operator HI-041.
- **HI-042** (withdrawn) Replaced by HI-067 and HI-068: `flintlockd` no
  longer listens on loopback for a Pod Provider on the same Host, because
  battery reaches it over the network (battery-operator ADR 0002).
- **HI-043** (withdrawn) Replaced by battery-operator HI-043.
- **HI-044** (withdrawn) Replaced by HI-067 to HI-070: `flintlockd` is now
  reachable from outside the Host, by battery in the Operator's pod, with
  mutual TLS and behind the Host firewall.
- **HI-063** (withdrawn) Replaced by HI-070: the one user id admitted over
  loopback is the Exec Agent's, and the network path is HI-069's.
- **HI-067** (withdrawn) Replaced by battery-operator HI-067.
- **HI-068** (withdrawn) Replaced by battery-operator HI-068.
- **HI-069** (withdrawn) Replaced by battery-operator HI-069.
- **HI-070** (withdrawn) Replaced by battery-operator HI-070.
- **HI-071** (withdrawn) Replaced by battery-operator HI-071.
- **HI-073** (withdrawn) Replaced by battery-operator HI-073.

HI-042, HI-044 and HI-063 were withdrawn before the Host Image moved. Their
replacements, HI-067 to HI-070, are now battery-operator's.

## Host configuration {#host-configuration}

- **HI-050** (withdrawn) Replaced by battery-operator HI-050. The Host
  configuration file is now `/etc/battery/host.conf`, and the Host
  environment file `/run/battery/host.env`, with `BATTERY_` names.
- **HI-051** (withdrawn) Replaced by battery-operator HI-051.
- **HI-052** (withdrawn) Replaced by battery-operator HI-052.

## Kubernetes node {#kubernetes-node}

- **HI-060** (withdrawn) Replaced by battery-operator HI-060, for the
  image label and the hypervisor version labels, now under
  `battery.liquidmetal-x.dev/`. The label `gitlab-runner.flintlock.dev/host`
  is gone: the Fleet Manifests select battery-operator's Host label
  (battery-operator HI-074, HP-020).
- **HI-061** (withdrawn) Replaced by battery-operator HI-061.
- **HI-062** (withdrawn) Replaced by battery-operator HI-062.
- **HI-074** (withdrawn) Replaced by battery-operator HI-074.
