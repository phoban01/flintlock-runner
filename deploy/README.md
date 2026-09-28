# Fleet Manifests

The Kubernetes manifests of a cluster fleet, specified in
`docs/requirements/12-cluster-fleet.md`, for the design of battery's claim
resources: battery schedules MicroVMs and owns the Pools, the Runner claims
a MicroVM with a `MicroVMClaim`, and the Exec Agent on each Host relays
every Stage to that Host's `flintlockd`. The resources and the Exec Agent
come from [battery-operator v0.1.0](https://github.com/phoban01/battery-operator/releases/tag/v0.1.0);
see [The Exec Agent](#the-exec-agent). Two kustomize roots here, and the
Runner on its own:

| Root | Applied to | What |
|------|------------|------|
| `deploy/` | the workload cluster | namespace `flintlock-system`, the Host Agent with the Host Services |
| `deploy/capi/` | the management cluster | one MachineDeployment, KubeadmConfigTemplate, AWSMachineTemplate and MachineHealthCheck per Host pool |
| `deploy/runner/` | not yet | the Runner; see [The Runner](#the-runner) |

```sh
make manifests                 # render deploy/ and deploy/capi
make manifests-check           # render all three, kubeconform, deploy/tests/checks.yaml
```

Nothing here names a real account, cluster or AMI. Neither root applies
until the operator has replaced what says `REPLACE` and supplied what is
listed below.

**The fleet cannot run a Job end to end yet.** The Runner's pool backend on
a cluster fleet, the claim backend (`12-cluster-fleet.md#battery-claims`),
is not wired into `flr` yet. Until it is, these manifests bring up Hosts
with their Host Services, and no Runner.

## Layout

| Path | What |
|------|------|
| `kustomization.yaml` | the workload cluster root; lists the Host Service Components |
| `host-agent/` | the DaemonSet and ServiceAccount of the Host Agent, with its `host-services` container |
| `host-agent/host-services-rbac.yaml`, `host-agent/host-services-admission-policy.yaml` | what the `host-services` container may do: annotate its own Host's Node with the Host Services. internal/hostservices' tests load these files unchanged |
| `host-agent/config/` | `render.sh`, `prepare-cache.sh`, `prewarm.sh`, and `host-services.yaml.tmpl`, the configuration of `flr host-services` |
| `host-agent/services/<service>/` | one kustomize Component per Host Service: containers, configuration templates, settings |
| `host-agent/rbac.yaml`, `host-agent/admission-policy.yaml` | the Pod Provider's, from the Virtual Node design; not deployed, kept for `internal/kubelet`'s tests until that design is withdrawn |
| `runner/` | Deployment, ServiceAccount and the configuration file `config.yaml`; not in `kustomization.yaml` yet |
| `runner/job-timeout.yaml` | the one place the job timeout is set (KF-081) |
| `runner/role.yaml` | the Virtual Node design's Runner Role; not deployed, kept for `internal/poolmgr/kube`'s tests |
| `capi/host-pool/` | the objects of one Host pool, with placeholders |
| `capi/pools/<pool>/host-pool.yaml` | the settings of one Host pool |
| `tests/` | the checks of `make manifests-check` and the overlays they build |

## What the operator supplies

In the workload cluster:

- **battery-operator v0.1.0**: its Manifests, `config/default` and
  `config/exec-agent`, which run the Operator, battery, the Exec Agent on
  every Host and the Inventory Controller, and serve the `Pool` and
  `MicroVMClaim` resources. See [The Exec Agent](#the-exec-agent).
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
  type, and the node and pod CIDRs in `host.conf`.
- `capi/kustomization.yaml`: the namespace of the workload cluster's Cluster.
- `runner/config.yaml`, once the Runner can be deployed: the GitLab URL and
  the Profiles.

## The Exec Agent

The Exec Agent comes from battery-operator's Manifests, not from
`deploy/`. `flr agent`, this repository's Exec Agent, is gone (KF-170 to
KF-182 are withdrawn). Apply battery-operator v0.1.0's Manifests,
`config/default` and `config/exec-agent`, to the same cluster as
`deploy/`, with the images of that release
(`ghcr.io/phoban01/battery-operator:v0.1.0` and
`ghcr.io/phoban01/battery-operator/exec-agent:v0.1.0`).
battery-operator's documentation says what they need: cert-manager for the
Operator's own certificates, and the Host prerequisites of its Exec Agent.

Its Exec Agent runs on every Node labelled
`battery.liquidmetal-x.dev/host=true`, so each Host's Node needs that label
as well as the Host label of this project.

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
from another by the node name in the token's user information. The policy
replaces the Pod Provider's `host-agent/admission-policy.yaml`, which
matches the same ServiceAccount and refuses any change to a Host's own
Node, so a cluster runs one or the other, never both.

## The Runner

`deploy/runner` holds the Runner's Deployment, ServiceAccount and
configuration, and `deploy/kustomization.yaml` leaves it out. It is built
and checked on its own (`kustomize build deploy/runner`), and it is not
meant to be applied yet: its configuration names no pool backend, because
the only backends `flr` has are battery's gRPC one of the push fleet and
the Virtual Node design's Kubernetes backend, and neither is this design's.
`flr config show` refuses the file for want of a backend and for nothing
else, which `make manifests-check` asserts, so a Runner started from it
exits at once. The Profiles carry no Guest Transport, because `agent-exec`
is not yet a transport a Profile can name.

The work package that builds the claim backend adds the backend to
`runner/config.yaml`, the Runner's permissions on the claim resource and
read access to Nodes (KF-189) to `runner/`, and `- runner` to the resources
of `deploy/kustomization.yaml`. What stays as it is: the configuration
from a ConfigMap and the token from the Secret
`flintlock-runner-gitlab-token`, key `token` (KF-080); the grace period
from `runner/job-timeout.yaml`, which also sets `gitlab.shutdown_timeout`
(KF-081); and the node affinity that keeps the Runner off Hosts (KF-082).

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

- **The Host Image and battery-operator's Exec Agent.** battery-operator's
  Exec Agent reaches `flintlockd` at the Host's address, port 9090, over
  mutual TLS, with certificates it writes to `/etc/battery/flintlockd`
  (its EA-001, EA-064). The Host Image serves `flintlockd` on the loopback
  port 9090 without TLS (HI-042, HI-063), and does not label the Node
  `battery.liquidmetal-x.dev/host=true`. The Host Image has to change
  before battery-operator's Exec Agent can run a Stage on it.
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
