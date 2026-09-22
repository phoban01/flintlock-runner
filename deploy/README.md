# Fleet Manifests

The Kubernetes manifests of a cluster fleet, specified in
`docs/requirements/12-cluster-fleet.md`. Two kustomize roots:

| Root | Applied to | What |
|------|------------|------|
| `deploy/` | the workload cluster | namespace `flintlock-system`, the Runner, the Host Agent |
| `deploy/capi/` | the management cluster | one MachineDeployment, KubeadmConfigTemplate, AWSMachineTemplate and MachineHealthCheck per Host pool |

```sh
make manifests                 # render both
make manifests-check           # render, kubeconform, deploy/tests/checks.yaml
```

Nothing here names a real account, cluster or AMI. Neither root applies
until the operator has replaced what says `REPLACE` and supplied the Secrets
below.

## Layout

| Path | What |
|------|------|
| `kustomization.yaml` | the workload cluster root; lists the Host Service Components |
| `runner/` | Deployment, ServiceAccount and bindings, `role.yaml`, the configuration file `config.yaml` |
| `runner/job-timeout.yaml` | the one place the job timeout is set (KF-081) |
| `host-agent/` | the DaemonSet with the Pod Provider, `rbac.yaml` and `admission-policy.yaml` |
| `host-agent/pod-provider-uid.yaml` | the one place the Pod Provider's user id is set (KF-135) |
| `host-agent/config/` | `render.sh`, `prepare-cache.sh`, `prewarm.sh`, the Pod Provider's configuration template |
| `host-agent/services/<service>/` | one kustomize Component per Host Service: containers, configuration templates, settings |
| `capi/host-pool/` | the objects of one Host pool, with placeholders |
| `capi/pools/<pool>/host-pool.yaml` | the settings of one Host pool |
| `tests/` | the checks of `make manifests-check` and the overlays they build |

## What the operator supplies

In `flintlock-system` of the workload cluster:

- **`flintlock-runner-gitlab-token`**, key `token`: the runner
  authentication token (`glrt-...`). The Runner does not start without it.
- **`flintlock-pod-provider-serving`**, type `kubernetes.io/tls`: the Pod
  Provider's serving certificate, below. The Host Agent does not start
  without it.
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

- `runner/config.yaml`: the GitLab URL and the Profiles.
- `capi/pools/default/host-pool.yaml`: the cluster, the AMI, the instance
  type, and the node and pod CIDRs in `host.conf`.
- `capi/kustomization.yaml`: the namespace of the workload cluster's Cluster.

## The Pod Provider's serving certificate

The Pod Provider serves the kubelet API (KF-030) on port 10260 of its Host,
with TLS, and accepts only clients with a certificate from the cluster's
kubelet client certificate authority (KF-031). The two halves come from
different places:

- **Client CA.** The ConfigMap `kube-root-ca.crt`, which every namespace
  has. On a kubeadm cluster that is the cluster CA, which also signs the
  API server's kubelet client certificate (`apiserver-kubelet-client`). If
  your API server's `--kubelet-client-certificate` is issued by another CA,
  patch the `kubelet-client-ca` volume of the DaemonSet to a ConfigMap that
  holds that CA as `ca.crt`.
- **Serving certificate and key.** The Secret
  `flintlock-pod-provider-serving`, which the operator creates. The API
  server of a kubeadm cluster does not verify kubelet serving certificates
  (it runs without `--kubelet-certificate-authority`), so one certificate
  serves every Host. Make it however you make certificates, for example:

  ```sh
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -days 365 -subj /CN=flintlock-pod-provider -keyout tls.key -out tls.crt
  kubectl -n flintlock-system create secret tls flintlock-pod-provider-serving \
    --cert=tls.crt --key=tls.key
  ```

  or a cert-manager Certificate whose `secretName` is this Secret. Rotate
  it by replacing the Secret and restarting the DaemonSet.

  Where the API server does verify kubelet serving certificates, every
  Host needs its own certificate for its InternalIP, signed by that CA.
  These manifests cannot express that; it needs the Pod Provider to request
  its certificate through the `kubernetes.io/kubelet-serving` signer.

## Requirements on the workload cluster

- **The API server prefers InternalIP for kubelets.** It reaches the Pod
  Provider through the Virtual Node's addresses, and the Virtual Node's
  Hostname address (`<host>-microvms`) resolves nowhere, so an API server
  whose `--kubelet-preferred-address-types` puts Hostname first cannot exec
  into Jobs. kubeadm's default, `InternalIP,ExternalIP,Hostname`, works.
- **Kubernetes 1.32 or later**, for the node name in bound ServiceAccount
  tokens that the admission policy relies on (KF-134), and the version of
  the Host Image's kubelet (`image/versions.env`) for the Hosts.
- **An AWS cloud controller manager**, as for any CAPA cluster: Hosts join
  with `cloud-provider: external`.

## Host Services

Each Host Service is a Component listed in `kustomization.yaml`. Removing
its line removes its containers, its configuration and its entry in the Pod
Provider's configuration, so that it is neither run nor published (KF-075).
Set `enabled: false` for it in `runner/config.yaml` as well.

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

If a pool's `host.conf` sets `POD_PROVIDER_UID`, set the same value in
`host-agent/pod-provider-uid.yaml`.

## Known gaps

- **SELinux.** The Host Image enforces SELinux and labels the three host
  paths the Host Agent mounts for `container_t` (HI-065): `host.env` and
  `not-ready.d` read-only, the cache directory writable, all at `s0`. The
  Host Agent's pod keeps `container_t` with a fixed MCS level,
  `s0:c311,c827`, so that its caches survive a restart of the pod; see
  "SELinux" in `image/README.md`. The Host Image's containerd does not set
  `enable_selinux`, and until it does the runtime does not apply the
  container domain at all. None of this has run on a booted Host.
- **Builds and the metadata service.** Rootless buildkitd runs in the
  Host's network namespace, as push provisioning runs it. The Host Image
  drops traffic from the Host Services' user ids, and from buildkit's
  subordinate ids that build steps run as, to the metadata service and the
  Host's control ports (HI-064); the ids are `HOST_SERVICE_UIDS` of the Host
  configuration file, and `make manifests-check` checks that they are the
  ones these manifests run the Host Services as. A pool whose `host.conf`
  sets `HOST_SERVICE_UIDS`, or a change to a Host Service's `runAsUser`,
  has to change the other too.
- **One DaemonSet for every pool.** `max_microvms` and the Host Service
  settings are the same on every Host; size `max_microvms` for the smallest
  instance type.
