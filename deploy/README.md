# Fleet Manifests

The Kubernetes manifests of a cluster fleet, specified in
`docs/requirements/12-cluster-fleet.md`, for the design of battery's claim
resources: battery schedules MicroVMs and owns the Pools, the Runner claims
a MicroVM with a `MicroVMClaim`, and the Exec Agent on each Host relays
every Stage to that Host's `flintlockd`. The resources and the Exec Agent
come from [battery-operator v0.1.0](https://github.com/phoban01/battery-operator/releases/tag/v0.1.0);
see [The Exec Agent](#the-exec-agent). Two kustomize roots here:

| Root | Applied to | What |
|------|------------|------|
| `deploy/` | the workload cluster | namespace `flintlock-system`, the Host Agent with the Host Services, and the Runner on the claim backend with its Holder and permissions (`deploy/runner/`) |
| `deploy/capi/` | the management cluster | one MachineDeployment, KubeadmConfigTemplate, AWSMachineTemplate and MachineHealthCheck per Host pool |

```sh
make manifests                 # render deploy/ and deploy/capi
make manifests-check           # render deploy/, deploy/capi and deploy/runner, kubeconform, deploy/tests/checks.yaml
```

Nothing here names a real account, cluster or AMI. Neither root applies
until the operator has replaced what says `REPLACE` and supplied what is
listed below. A release's `flintlock-runner-fleet.yaml` has this project's
images by digest already, the Guest Image in the Runner's Profiles
included (`docs/RELEASING.md`).

The fleet's own Hosts reach battery once each pool's `host.conf` sets
`FLINTLOCKD_CLIENT_CIDRS` to the Operator's pod network. See
[Known gaps](#known-gaps).

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
5. **`deploy/capi/`**, or a release's `flintlock-runner-capi.yaml`, to the
   management cluster, for the Hosts.

What each side provides:

| From | What |
|------|------|
| battery-operator | the `Pool` and `MicroVMClaim` resources, battery, the Exec Agent on every Host, the Inventory Controller that gives battery its Hosts, and the serving CA |
| `deploy/` | the Host Agent and its Host Services, and their addresses on each Host's Node (KF-194); the Runner, its Holder and its permissions |
| `deploy/capi/` | the Hosts, as Cluster API Machines of the Host Image |

## Layout

| Path | What |
|------|------|
| `kustomization.yaml` | the workload cluster root; lists the Host Service Components |
| `host-agent/` | the DaemonSet and ServiceAccount of the Host Agent, with its `host-services` container |
| `host-agent/host-services-rbac.yaml`, `host-agent/host-services-admission-policy.yaml` | what the `host-services` container may do: annotate its own Host's Node with the Host Services. internal/hostservices' tests load these files unchanged |
| `host-agent/config/` | `render.sh`, `prepare-cache.sh`, `prewarm.sh`, and `host-services.yaml.tmpl`, the configuration of `flr host-services` |
| `host-agent/services/<service>/` | one kustomize Component per Host Service: containers, configuration templates, settings |
| `runner/` | the Runner's Deployment, ServiceAccount and configuration file `config.yaml` |
| `runner/rbac.yaml` | what the Runner may do (KF-196). cmd/flr's end-to-end test of the claim backend applies it unchanged |
| `runner/holder.yaml` | the Holder, the ServiceAccount every claim names; bound to nothing |
| `runner/job-timeout.yaml` | the one place the job timeout is set (KF-081) |
| `capi/host-pool/` | the objects of one Host pool, with placeholders |
| `capi/pools/<pool>/host-pool.yaml` | the settings of one Host pool |
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

- `capi/pools/default/host-pool.yaml`: the cluster, the AMI, the instance
  type, and in `host.conf` the node, pod and Service CIDRs and the
  Operator's pod CIDR.
- `capi/kustomization.yaml`: the namespace of the workload cluster's Cluster.
- `runner/config.yaml`: the GitLab URL and the Profiles. Applied from
  source, the Profiles' Guest Image says `:REPLACE`; pin it by the digests
  of a release's `images.txt`. A release's fleet asset has them already.

## The Exec Agent

The Exec Agent comes from battery-operator's Manifests, not from
`deploy/`. `flr agent`, this repository's Exec Agent, is gone (KF-170 to
KF-182 are withdrawn). Apply battery-operator v0.1.0's Manifests,
`config/default` and `config/exec-agent`, to the same cluster before
`deploy/` ([Order of application](#order-of-application)).
battery-operator's documentation says what they need: cert-manager for the
Operator's own certificates, and the Host prerequisites of its Exec Agent.

Its Exec Agent runs on every Node labelled
`battery.liquidmetal-x.dev/host=true`. The Host Image registers each Host's
Node with that label as well as the Host label of this project (HI-074).

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
the Hosts (KF-082). It reads its configuration from a ConfigMap and the
GitLab runner token from the Secret `flintlock-runner-gitlab-token`, key
`token` (KF-080), which the operator creates:

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
  tokens that the admission policies rely on, and the version of the Host
  Image's kubelet (`image/versions.env`) for the Hosts.
- **A route from the Runner to port 10270 of every Host's internal
  address**, where the Exec Agents listen.
- **An AWS cloud controller manager**, as for any CAPA cluster: Hosts join
  with `cloud-provider: external`.

## Host Services

Each Host Service is a Component listed in `kustomization.yaml`. Removing
its line removes its containers, its configuration and its entry in the
`host-services` configuration, so that it is neither run nor published on
the Host's Node (KF-075, KF-194). Remove it from `runner/config.yaml` as
well.

Every Host Service binds only the guest bridge gateway address (KF-071).
That address is the first address of `GUEST_SUBNET` in the Host
configuration file, which differs per pool, so it is not in the manifests.
The Host Agent's `render` init container reads it from `/run/flr/host.env`,
which the Host Image's `flr-host-config` unit writes at every boot, and
writes it into each service's configuration. A Host without that file does
not start the Host Agent.

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

All storage is under `/var/lib/flintlock-runner/cache`, the Host Image's
cache volume (KF-072). The `prepare-cache` init container creates one
directory per service and gives it to that service's user id; each service
mounts only its own.

## Host pools

`capi/pools/default` is one pool. To add one, copy the directory, give its
`host-pool.yaml` a new ConfigMap name and a new `name`, and list it in
`capi/kustomization.yaml`. The AMI is the one `make image-ami` published
from the Host Image (`ghcr.io/phoban01/flintlock-runner/host`), and the
pool's `kubernetesVersion` has to be the one that image carries.

A Host has no instance profile (KF-112). CAPA therefore passes the bootstrap
data in the instance's user data instead of Secrets Manager
(`insecureSkipSecretsManager`); it holds a short-lived join token and the
Host configuration file, which holds no secret.

If a pool's `host.conf` sets `HOST_CONTROL_PORTS`, keep 10270, the Exec
Agent's port, in the list.

## Known gaps

- **The Host Image and battery-operator's Exec Agent.** The Host Image
  serves `flintlockd` on the Host's address, port 9090, with mutual TLS,
  from the certificates the Exec Agent writes to `/etc/battery/flintlockd`.
  It labels the Node `battery.liquidmetal-x.dev/host=true` (HI-067 to
  HI-074). battery reaches `flintlockd` only from the addresses in the
  pool's `FLINTLOCKD_CLIENT_CIDRS`. Set it to the Operator's pod network,
  and add the node range on a CNI that masquerades pod traffic between
  Nodes. While it is empty, the default, battery cannot reach a Host and no
  Stage reaches a MicroVM there. None of this has run on a booted Host.
- **SELinux.** The Host Image enforces SELinux and labels the host paths
  the Host Agent mounts for `container_t` (HI-065): `host.env` read-only,
  the cache directory writable, both at `s0`. The
  Host Agent's pod keeps `container_t` with a fixed MCS level,
  `s0:c311,c827`, so that its caches survive a restart of the pod; see
  "SELinux" in `image/README.md`. The Host Image's containerd labels every
  unprivileged pod (HI-066), so the Host Agent's containers, and every other
  unprivileged pod on a Host, run confined as `container_t`; privileged
  containers stay unconfined. None of this has run on a booted Host.
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
  `container_t` it does not get. Whether a build then runs without a denial
  only an enforcing Host can show: run one and read
  `ausearch -m avc -ts boot`.
- **Builds and the metadata service.** Rootless buildkitd runs in the
  Host's network namespace, as push provisioning runs it. The Host Image
  drops traffic from the Host Services' user ids, and from buildkit's
  subordinate ids that build steps run as, to the metadata service and the
  Host's control ports (HI-064); the ids are `HOST_SERVICE_UIDS` of the Host
  configuration file, and `make manifests-check` checks that they are the
  ones these manifests run the Host Services as. A pool whose `host.conf`
  sets `HOST_SERVICE_UIDS`, or a change to a Host Service's `runAsUser`,
  has to change the other too.
- **One DaemonSet for every pool.** The Host Service settings are the same
  on every Host.
- **A Host that cannot run MicroVMs stays in its pool.** battery-operator's
  Exec Agent reports a Host not ready (its EA-030 to EA-034) in an
  annotation of its Node, not as a Node condition, so the
  MachineHealthCheck (KF-005) does not see it.
