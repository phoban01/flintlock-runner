# Fleet Manifests

The Kubernetes manifests of a cluster fleet, specified in
`docs/requirements/12-cluster-fleet.md`, for the design of battery's claim
resources: battery schedules MicroVMs and owns the Pools, the Runner claims
a MicroVM with a `MicroVMClaim`, and the Exec Agent on each Host relays
every Stage to that Host's `flintlockd`. The resources and the Exec Agent
come from [battery-operator v0.1.0](https://github.com/phoban01/battery-operator/releases/tag/v0.1.0);
see [The Exec Agent](#the-exec-agent).

The Hosts come from battery-operator too. They boot its
[reference Host Image](https://github.com/phoban01/battery-operator/tree/main/hostimage),
and its [Cluster API templates](https://github.com/phoban01/battery-operator/tree/main/config/capi)
make them (its ADR 0007). This repository has no Host Image and no Cluster
API objects of its own. See [Host pools](#host-pools) for what a pool of
this fleet sets.

One kustomize root here:

| Root | Applied to | What |
|------|------------|------|
| `deploy/` | the workload cluster | namespace `flintlock-system`, the Host Agent with the Host Services, and the Runner on the claim backend with its Holder and permissions (`deploy/runner/`) |

```sh
make manifests                 # render deploy/
make manifests-check           # render deploy/ and deploy/runner, kubeconform, deploy/tests/checks.yaml
```

Nothing here names a real account or cluster. `deploy/` does not apply
until the operator has replaced what says `REPLACE` and supplied what is
listed below. A release's `flintlock-runner-fleet.yaml` has this project's
images by digest already, the Guest Image in the Runner's Profiles
included (`docs/RELEASING.md`).

## Order of application

battery-operator's Manifests go first, then these. Each step needs what
the one before it made.

1. **cert-manager**, which battery-operator's Manifests need for the
   Operator's own certificates.
2. **battery-operator v0.1.0**, `config/default` and `config/exec-agent`,
   with the images of that release
   (`ghcr.io/phoban01/battery-operator:v0.1.0` and
   `ghcr.io/phoban01/battery-operator/exec-agent:v0.1.0`). They make the
   namespace `battery-operator-system` and serve the `Pool` and
   `MicroVMClaim` resources (`battery.liquidmetal-x.dev/v1alpha1`). They
   run the Operator with battery as its sidecar, the Inventory Controller,
   and the Exec Agent on every Node labelled
   `battery.liquidmetal-x.dev/host=true`. The Operator writes its serving
   CA to the ConfigMap `flintlockd-ca` in its namespace.
3. **`deploy/`**, or a release's `flintlock-runner-fleet.yaml`, to the
   same cluster. It makes the namespace `flintlock-system`, the Host Agent
   with the Host Services on every Host, and the Runner. The Runner's Role
   to read `flintlockd-ca` goes into `battery-operator-system`, which has
   to exist, and the Runner's claims and Pools need battery-operator's
   resources.
4. **The GitLab runner token**, as the Secret
   `flintlock-runner-gitlab-token` in `flintlock-system`
   ([The Runner](#the-runner)). The Runner's pod does not start without it.
5. **The Host pools**, from battery-operator's `config/capi`, to the
   management cluster, with the settings of [Host pools](#host-pools).

What each side provides:

| From | What |
|------|------|
| battery-operator | the `Pool` and `MicroVMClaim` resources, battery, the Exec Agent on every Host, the Inventory Controller that gives battery its Hosts, the serving CA, the Host Image and the Cluster API templates of the Host pools |
| `deploy/` | the Host Agent and its Host Services, and their addresses on each Host's Node (KF-194); the Runner, its Holder and its permissions; the settings every Host pool adds for the Host Services (`host-agent/config/host-pool.conf`, KF-198) |

## Layout

| Path | What |
|------|------|
| `kustomization.yaml` | the workload cluster root; lists the Host Service Components |
| `host-agent/` | the DaemonSet and ServiceAccount of the Host Agent, with its `host-services` container |
| `host-agent/host-services-rbac.yaml`, `host-agent/host-services-admission-policy.yaml` | what the `host-services` container may do: annotate its own Host's Node with the Host Services. internal/hostservices' tests load these files unchanged |
| `host-agent/config/` | `render.sh`, `prepare-cache.sh`, `prewarm.sh`, and `host-services.yaml.tmpl`, the configuration of `flr host-services` |
| `host-agent/config/host-pool.conf` | the two lines every Host pool adds to its `host.conf` (KF-198) |
| `host-agent/services/<service>/` | one kustomize Component per Host Service: containers, configuration templates, settings |
| `runner/` | the Runner's Deployment, ServiceAccount and configuration file `config.yaml` |
| `runner/rbac.yaml` | what the Runner may do (KF-196). cmd/flr's end-to-end test of the claim backend applies it unchanged |
| `runner/holder.yaml` | the Holder, the ServiceAccount every claim names; bound to nothing |
| `runner/job-timeout.yaml` | the one place the job timeout is set (KF-081) |
| `tests/` | the checks of `make manifests-check` and the overlays they build |

## What the operator supplies

In the workload cluster:

- **battery-operator v0.1.0**: its Manifests, `config/default` and
  `config/exec-agent`, which run the Operator, battery, the Exec Agent on
  every Host and the Inventory Controller, and serve the `Pool` and
  `MicroVMClaim` resources. See [Order of application](#order-of-application).
- **The GitLab runner token**, in the Secret
  `flintlock-runner-gitlab-token`, key `token`.
- The Host Service credentials, if an upstream needs them. The Components
  generate both Secrets empty; replace them from an overlay:

  ```yaml
  # overlay/kustomization.yaml
  resources: [../deploy]
  secretGenerator:
    - name: flintlock-registry-mirror-credentials
      namespace: flintlock-system
      behavior: replace
      files: [credentials.json]   # {"registry-1.docker.io": {"username": "...", "password": "..."}}
    - name: flintlock-go-proxy-credential
      namespace: flintlock-system
      behavior: replace
      files: [netrc]              # machine gitlab.example.com login oauth2 password <read-only token>
  ```

  Each is mounted into the one container that uses it, the registry mirror
  and the Go module proxy (KF-073). The Go module proxy credential has to be
  read-only and scoped to the private module patterns (SE-055); push
  provisioning checks that against GitLab, and nothing here does.

In the manifests:

- `runner/config.yaml`: the GitLab URL and the Profiles. Applied from
  source, the Profiles' Guest Image says `:REPLACE`; pin it by the digests
  of a release's `images.txt`. A release's fleet asset has them already.

In the management cluster: the Host pools, from battery-operator's
templates, with the settings of [Host pools](#host-pools).

## The Exec Agent

The Exec Agent comes from battery-operator's Manifests, not from
`deploy/`. `flr agent`, this repository's Exec Agent, is gone (KF-170 to
KF-182 are withdrawn). Apply battery-operator v0.1.0's Manifests,
`config/default` and `config/exec-agent`, to the same cluster before
`deploy/` ([Order of application](#order-of-application)).
battery-operator's documentation says what they need: cert-manager for the
Operator's own certificates, and the Host prerequisites of its Exec Agent.

Its Exec Agent runs on every Node labelled
`battery.liquidmetal-x.dev/host=true`, the Host label. battery-operator's
Host Image and its Cluster API templates both put that label on each
Host's Node (its HI-074, HP-020). The Host Agent selects the same label.

`deploy/` and battery-operator's Manifests can be applied to the same
cluster. Nothing of one clashes with the other:

- **Port 10270.** Only battery-operator's Exec Agent listens on it, on the
  Host's internal address. The Host Agent listens on no port of its own;
  its Host Services listen on the guest bridge gateway only (KF-071).
- **Node annotations.** battery-operator's Exec Agent writes the Node
  report under `battery.liquidmetal-x.dev/` (its EA-034). The Host Agent
  writes only the Host Service annotations,
  `host-service.gitlab-runner.flintlock.dev/<name>` (KF-194).
- **Admission policies and identities.** Each policy matches its own
  ServiceAccount: battery-operator's matches
  `battery-operator-system/battery-operator-exec-agent`, and
  `host-agent/host-services-admission-policy.yaml` matches
  `flintlock-system/flintlock-host-agent`. Each lets its identity change
  only its own annotations of its own Host's Node.
- **Namespaces.** battery-operator runs in `battery-operator-system`, this
  project in `flintlock-system`.

The Runner reaches each Exec Agent on port 10270 of the Host's internal
address, over TLS verified against the Operator's serving CA, with a claim
token of the Job's claim (KF-186). battery-operator's Client Library
requests the token and dials the connection.

## The Host Services on the Host's Node

The Runner reads where a Host's Host Services listen from annotations of
the Host's Node (KF-189). battery-operator's Exec Agent does not publish
them, so the Host Agent does: its `host-services` container, `flr
host-services`, writes `host-service.gitlab-runner.flintlock.dev/<name>`
set to `<bridge gateway>:<port>` for each enabled Host Service, and removes
the annotation of one that is not enabled (KF-194). It writes them again
every minute. The names are the ones `flr agent` used, so the Runner reads
them as before.

The container authenticates to the API server with the Host Agent pod's
bound ServiceAccount token, projected into its container only.
`host-agent/host-services-rbac.yaml` lets it patch Nodes, and
`host-agent/host-services-admission-policy.yaml` narrows that to the Host
Service annotations of its own Host's Node. The API server tells one Host
from another by the node name in the token's user information.

The Virtual Node design and its Pod Provider (`flr kubelet`) are gone
(#101). Nothing here runs a pod on a Virtual Node, and nothing grants the
Runner pod exec or ReplicaSets. battery-operator's Exec Agent and its
admission policy take the Pod Provider's place on each Host.

## The Runner

`deploy/runner` holds the Runner, and `deploy/kustomization.yaml` lists
it. The Runner runs as a Deployment of one pod in `flintlock-system`, off
the Hosts (KF-082): it needs a Node without the Host label, and it does
not tolerate the Host taint. It reads its configuration from a ConfigMap
and the GitLab runner token from the Secret `flintlock-runner-gitlab-token`,
key `token` (KF-080), which the operator creates:

```sh
kubectl -n flintlock-system create secret generic \
  flintlock-runner-gitlab-token --from-literal=token=glrt-...
```

Its pool backend is the claim backend (`12-cluster-fleet.md#battery-claims`),
with the pod's own identity:

- **Claims.** For each Job the Runner creates a `MicroVMClaim` in
  `flintlock-system` for the Pool of the Job's Profile, and waits until it
  is `Bound`. It renews the claim while the Job runs and deletes it when
  the Job ends. It declares one `Pool` per Profile from
  `runner/config.yaml`.
- **The Holder.** Every claim names the ServiceAccount
  `flintlock-runner-holder` (`runner/holder.yaml`). battery-operator's
  Client Library requests a claim token of the Holder for each claim,
  bound to the claim's Secret `<claim name>-exec`. No pod runs as the
  Holder, and it has no permission of its own.
- **Stages.** The Executor runs every Stage over `agent-exec`: it dials the
  Exec Agent at the address in the claim's status, verifies it against the
  serving CA in `battery-operator-system/flintlockd-ca`, and sends the claim
  token (KF-185, KF-186). A Profile that names `ssh` is refused, because
  the Exec Agent relays exec only (KF-195).
- **Host Services.** The Executor reads them from the annotations of the
  Node of the claim's Host (KF-189).

The Runner does not select Hosts by a label of its own. battery places each
MicroVM, and the claim names its Host. So no Host needs the old label
`gitlab-runner.flintlock.dev/host`, and nothing here sets or reads it.

`runner/rbac.yaml` grants exactly that and no more (KF-196): in
`flintlock-system`, `microvmclaims` (create, get, list, watch, patch,
delete), `pools` (create, get, list, watch, update, patch), `secrets`
(create) and `serviceaccounts/token` (create) on the Holder alone; `get`
on Nodes; and `get` on the ConfigMap `flintlockd-ca` in
`battery-operator-system`. The grace period comes from
`runner/job-timeout.yaml`, which also sets `gitlab.shutdown_timeout`
(KF-081).

## Requirements on the workload cluster

- **Kubernetes 1.32 or later**, for the node name in bound ServiceAccount
  tokens that the admission policies rely on. The Hosts run the kubelet of
  battery-operator's Host Image (`KUBERNETES_VERSION` in its
  `hostimage/versions.env`).
- **A route from the Runner to port 10270 of every Host's internal
  address**, where the Exec Agents listen.
- **An AWS cloud controller manager**, as for any CAPA cluster: Hosts join
  with `cloud-provider: external`.
- **CNI and kube-proxy DaemonSets that tolerate the Host taint**,
  `battery.liquidmetal-x.dev/host=true:NoSchedule`. Most tolerate every
  taint.

## Host Services

Each Host Service is a Component listed in `kustomization.yaml`. Removing
its line removes its containers, its configuration and its entry in the
`host-services` configuration, so that it is neither run nor published on
the Host's Node (KF-075, KF-194). Remove it from `runner/config.yaml` as
well.

Every Host Service binds only the guest bridge gateway address (KF-071).
That address is the first address of `GUEST_SUBNET` in the Host
configuration file, which can differ per pool, so it is not in the
manifests. The Host Agent's `render` init container reads it as
`BATTERY_GATEWAY` from `/run/battery/host.env`, which battery-operator's
Host Image writes at every boot (its `battery-host-config` unit), and
writes it into each service's configuration. A Host without that file does
not start the Host Agent.

The same container reads `BATTERY_GATEWAY_SERVICE_PORTS` and
`BATTERY_GATEWAY_SERVICE_UIDS` from that file. If the Host leaves out a
port or a user id of `host-agent/config/host-pool.conf`, it fails, and no
Host Service starts on that Host (KF-199). Its log says which value is
missing. See [Host pools](#host-pools).

The Host Agent does not read `/run/battery/not-ready.d`. battery-operator's
Exec Agent reports those reasons on the Node.

Settings per service, in `host-agent/services/<service>/`:

| Service | Software | Settings |
|---------|----------|----------|
| `buildkit` | rootless buildkitd | `buildkitd.toml.tmpl` |
| `go_proxy` | Athens behind nginx | `go-proxy.env` (upstream, private patterns), `prewarm.go-proxy.modules` |
| `registry_mirror` | zot | `registry-mirror.upstreams`, `prewarm.registry-mirror.images` |
| `http_cache` | nginx | `http-cache.upstreams` |

The software is what push provisioning runs (`internal/fleet/scripts`), at
the same versions, and each configuration is ported from there. The
registry mirror is zot rather than distribution because one zot is a
pull-through cache of every upstream, where a distribution registry proxies
one.

### The Host Service cache

All Host Service storage is in the volume `cache` of the Host Agent's pod,
mounted at `/var/lib/flintlock-runner/cache` (KF-197). It is an
`emptyDir` on the Node's disk. The `prepare-cache` init container creates
one directory per service in it and gives it to that service's user id;
each service mounts only its own.

The cache used to be a volume that the flintlock-runner Host Image made on
the instance-store disk, mounted as a hostPath. battery-operator's Host
Image makes no such volume, so the cache had to move into the manifests.
There were two choices: a hostPath under `/var`, or an ephemeral volume.
It is an `emptyDir` for these reasons:

- **SELinux.** battery-operator's Host Image enforces SELinux, and runs
  the Host Agent's containers as `container_t` (its HI-066). That domain
  may write only files labelled for containers. The kubelet labels an
  `emptyDir` for the pod that mounts it. It labels no hostPath, and
  battery-operator's image labels no directory for this project, so a
  hostPath under `/var` would be `var_lib_t` and every write to it would be
  refused.
- **Nothing left on the Host.** An `emptyDir` goes with its pod. A
  hostPath would stay on the Host after the Host Agent is removed.
- **No generic ephemeral volume.** A generic ephemeral volume needs a
  StorageClass with a local provisioner on every Host, which the fleet does
  not have.

What it costs:

- **The cache starts cold with each new pod.** It survives a restart of a
  container, but not a rollout of the DaemonSet or an eviction, and so not
  the replacement of a Host.
- **It is on the root disk, not the instance-store disk.** The thin pool
  takes the whole instance-store disk. battery-operator's templates give
  every Host a root volume of 100 GiB, and a pool's settings do not change
  it. The cache shares that volume with the operating system and the
  kubelet, and counts against the Node's ephemeral storage, so a full disk
  makes the kubelet evict pods. buildkitd keeps its
  use under its garbage collection limit (`buildkitd.toml.tmpl`), and the
  HTTP cache under the size of each upstream. zot and Athens have no size
  limit.

The pod's SELinux level is fixed, `s0:c311,c827`. The kubelet labels the
volume with that level. buildkitd names its own SELinux type (KF-139), so
it repeats the level; without it, containerd would give it random
categories, and it could not use the volume.

## Host pools

The Host pools are battery-operator's Cluster API templates, in its
[`config/capi/`](https://github.com/phoban01/battery-operator/tree/main/config/capi),
specified in its
[`docs/requirements/12-host-pool.md`](https://github.com/phoban01/battery-operator/blob/main/docs/requirements/12-host-pool.md)
(HP-001 to HP-031). Its
[README](https://github.com/phoban01/battery-operator/blob/main/config/capi/README.md)
says how to fill in a pool, and how to make the AMI from the Host Image
(its HI-009). This section says what a pool of this fleet sets on top of
that, in the pool's `host-pool.yaml`.

In `host.conf`:

```sh
# The Host Services: copy both lines from deploy/host-agent/config/host-pool.conf.
GATEWAY_SERVICE_PORTS=1234,3000,5000,3128
GATEWAY_SERVICE_UIDS=101,1000,10001,10002,100000-165535
# The cluster: every pool sets these (battery-operator HP-011).
FLINTLOCKD_CLIENT_CIDRS=<the Operator's pod network>
PROTECTED_CIDRS=<the node, pod and Service ranges>
```

- **`GATEWAY_SERVICE_PORTS`**: the ports the Host Services serve on the
  guest bridge gateway: buildkitd 1234, the Go module proxy 3000, the
  registry mirror 5000 and the HTTP cache 3128. The Host Image opens them
  to guests and to nothing else (battery-operator HI-078, HI-079).
- **`GATEWAY_SERVICE_UIDS`**: the user ids the Host Services run as:
  nginx 101, buildkitd 1000, Athens 10001 and zot 10002, and
  100000-165535, the subordinate ids of the buildkit image's user, which
  build steps run as. The Host Image drops their traffic to the instance
  metadata service, to the Host's control ports and to `flintlockd`'s port
  (battery-operator HI-080).
- **`FLINTLOCKD_CLIENT_CIDRS`**: the Operator's pod network, where battery
  runs. Only these addresses reach `flintlockd` from off the Host
  (battery-operator HI-069). On a CNI that masquerades pod traffic between
  Nodes, add the node range. While it is empty, the default, battery cannot
  reach a Host and no Stage reaches a MicroVM there.
- **`PROTECTED_CIDRS`**: the cluster's node, pod and Service ranges, so
  that no MicroVM reaches them (battery-operator HI-035, HI-076).

`make manifests-check` checks that `host-pool.conf` lists exactly the
ports and user ids of the manifests (KF-198). A change to a Host Service's
`runAsUser` or port changes that file too, and then every pool's
`host.conf`. A pool that does not set them runs no Host Service (KF-199).

If a pool's `host.conf` sets `HOST_CONTROL_PORTS`, keep 10270, the Exec
Agent's port, in the list. None of the gateway service ports may be in it.

In `additionalSecurityGroups`: at least one security group that admits TCP
port 9090, `flintlockd`'s port, from the ranges of
`FLINTLOCKD_CLIENT_CIDRS`, and from the node range on a CNI that
masquerades pod traffic between Nodes. CAPA's default node security group
does not open that port (battery-operator HP-006).

The Host label and the Host taint need nothing from the pool. The
templates join every Host with the label
`battery.liquidmetal-x.dev/host=true` and the taint
`battery.liquidmetal-x.dev/host=true:NoSchedule` (battery-operator HP-020,
HP-021). The Host Agent selects that label and tolerates that taint, and
the Runner avoids both.

A Host has no instance profile (battery-operator HP-030), and the Fleet
Manifests grant no AWS permission (KF-112).

## Known gaps

- **Nothing has run on a booted Host.** The Host Agent has not run on
  battery-operator's Host Image yet. In particular, whether the kubelet
  labels the cache volume as described, and whether the Host Services run
  without an SELinux denial, only an enforcing Host can show: run one and
  read `ausearch -m avc -ts boot`.
- **buildkitd in `container_engine_t`.** Every build step of rootless
  buildkitd mounts a fresh `devpts`, which `container_t` may not, so the
  buildkitd container alone names the base policy's `container_engine_t`
  (KF-139), with the pod's level repeated beside it: the kubelet takes a
  container's `seLinuxOptions` whole, and containerd gives a container that
  names a type but no level random categories. What the base policy was
  checked to allow, with `sesearch`: containerd (`container_runtime_t`, an
  unconfined domain) may transition to it; it may read
  `container_ro_file_t`, read and write `container_file_t`, mount any
  filesystem type (`devpts` included), create user namespaces and hold
  `sys_admin`, `setuid` and `setgid` inside them. It is an
  `mcs_constrained_type` like `container_t`, so at the fixed level it uses
  the `s0` host paths and its own cache and nothing of another pod's; the
  build steps, which buildkit starts with no label of their own (no
  `--oci-worker-selinux`), stay in `container_engine_t` at that same level.
  It is not a `svirt_sandbox_domain`, so what that attribute grants
  `container_t` it does not get.
- **A cold cache after each rollout.** See
  [The Host Service cache](#the-host-service-cache).
- **One DaemonSet for every pool.** The Host Service settings are the same
  on every Host.
- **A Host that cannot run MicroVMs stays in its pool.** battery-operator's
  Exec Agent reports a Host not ready (its EA-030 to EA-034) in an
  annotation of its Node, not as a Node condition, so the
  MachineHealthCheck of the pool (battery-operator HP-005) does not see it.
