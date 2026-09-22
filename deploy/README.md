# Fleet Manifests

The Kubernetes manifests of a cluster fleet, specified in
`docs/requirements/12-cluster-fleet.md`, for the design of battery's claim
resources: battery schedules MicroVMs and owns the Pools, the Runner claims
a MicroVM with a `MicroVMClaim`, and the Exec Agent on each Host relays
every Stage to that Host's `flintlockd`. Two kustomize roots, and the
Runner on its own:

| Root | Applied to | What |
|------|------------|------|
| `deploy/` | the workload cluster | namespace `flintlock-system`, the Host Agent with the Exec Agent and the Host Services |
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
is not built: it waits for battery to settle the shapes of its `Pool` and
`MicroVMClaim` resources. Until it exists, these manifests bring up Hosts
with their Host Services and an Exec Agent that answers no exec request,
and no Runner.

## Layout

| Path | What |
|------|------|
| `kustomization.yaml` | the workload cluster root; lists the Host Service Components |
| `agent/` | the Exec Agent: its container in the Host Agent (`host-agent-patch.yaml`), `rbac.yaml` and `admission-policy.yaml`, as one Component. internal/agent's tests load these files unchanged |
| `host-agent/` | the DaemonSet and ServiceAccount of the Host Agent; includes `agent/` |
| `host-agent/config/` | `render.sh`, `prepare-cache.sh`, `prewarm.sh`, the Exec Agent's configuration template `agent.yaml.tmpl` |
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

- **cert-manager and its csi-driver** (`csi.cert-manager.io`), and an
  **Issuer `flintlock-exec-agent`** in `flintlock-system`, for the Exec
  Agent's serving certificates; see below. The Host Agent does not start
  without the driver.
- **battery's claim resource.** The Exec Agent authorizes every exec
  request against a `MicroVMClaim` (KF-174). Its group and resource are
  PROVISIONAL: `claims.test.flintlock-runner.dev/v1alpha1`, `microvmclaims`,
  this project's test definition
  (`internal/agent/testdata/crds/microvmclaims.yaml`), named in
  `host-agent/config/agent.yaml.tmpl` and `agent/rbac.yaml`. No cluster
  serves it, and nothing here installs it, so the agent refuses every exec
  request (KF-175) until battery publishes its resource and both files name
  it.
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

The Exec Agent (`flr agent`, KF-170 to KF-181) is a container of the Host
Agent, so it runs on every Host, in the Host's network namespace. It
listens on port 10270 of the Host's internal address (KF-172), which the
Runner has to be able to reach; the port is one of the Host Image's
`HOST_CONTROL_PORTS`, closed to guests (HI-034) and to the Host Services'
user ids (HI-064). It reaches `flintlockd` on the Host's loopback endpoint
(HI-042) and runs as user id 10250, the one the Host Image admits there
(HI-063); no other container of the Host Agent runs as that id. The id is
set once, in `agent/host-agent-patch.yaml`. The `render` init container
writes the Host's own `POD_PROVIDER_UID`, from `/run/flr/host.env`, into
the agent's configuration, and the agent refuses to start as any other id.
A pool whose `host.conf` sets `POD_PROVIDER_UID` therefore stops the agent
until the patch says the same; `make manifests-check` compares the patch
with the Host Image's default and with every pool's `host.conf`.

It authenticates to the API server with the Host Agent pod's bound
ServiceAccount token, projected into its container only. `agent/rbac.yaml`
and `agent/admission-policy.yaml` confine it to its own Host's Node
annotations and drain guard (KF-180). They replace the Pod Provider's
`host-agent/rbac.yaml` and `host-agent/admission-policy.yaml`, which match
the same ServiceAccount and refuse the change to a Host's own Node that the
Exec Agent has to make, so a cluster runs one pair or the other, never both.

### The Exec Agent's serving certificate

**Unverified: nothing here has run on a cluster.** The certificate has to
name the Host's internal address (KF-172). The manifests follow
`deploy/agent`'s proposal: a `csi.cert-manager.io` volume, so that
cert-manager's csi-driver issues each pod its own certificate and key, with
the pod's IP as its IP SAN (`${POD_IP}`); in the Host's network namespace
that is the Host's internal address. The Issuer is `flintlock-exec-agent`,
kind `Issuer`, in `flintlock-system`, which the operator provides, for
example a CA Issuer whose certificate authority the Runner's `agent-exec`
Guest Transport is then configured to trust (KF-186). The files are group
10250 (`fs-group`) so the agent can read the key. What has not been checked:
that the installed csi-driver version expands `${POD_IP}` for a pod with
host networking, that the certificate is renewed without restarting the
agent, and that the Runner verifies it.

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
  tokens that the admission policy relies on (KF-180), and the version of
  the Host Image's kubelet (`image/versions.env`) for the Hosts.
- **A route from the Runner to port 10270 of every Host's internal
  address**, where the Exec Agents listen.
- **An AWS cloud controller manager**, as for any CAPA cluster: Hosts join
  with `cloud-provider: external`.

## Host Services

Each Host Service is a Component listed in `kustomization.yaml`. Removing
its line removes its containers, its configuration and its entry in the Exec
Agent's configuration, so that it is neither run, nor probed, nor published
on the Host's Node (KF-075, KF-179). Remove it from `runner/config.yaml` as
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

If a pool's `host.conf` sets `POD_PROVIDER_UID`, set the same value as the
Exec Agent's `runAsUser`, `runAsGroup` and `fs-group` in
`agent/host-agent-patch.yaml`; the name of the key is the Virtual Node
design's, and the Exec Agent is what it admits now. If it sets
`HOST_CONTROL_PORTS`, keep 10270, the Exec Agent's port, in the list.

## Known gaps

- **SELinux.** The Host Image enforces SELinux and labels the three host
  paths the Host Agent mounts for `container_t` (HI-065): `host.env` and
  `not-ready.d` read-only, the cache directory writable, all at `s0`. The
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
- **One DaemonSet for every pool.** The Host Service settings and the Exec
  Agent's user id are the same on every Host.
- **A Host that cannot run MicroVMs stays in its pool.** The Exec Agent
  reports a Host not ready (KF-178) in an annotation of its Node, not as a
  Node condition, so the MachineHealthCheck (KF-005) does not see it.
