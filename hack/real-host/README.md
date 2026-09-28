# A Job on a real Host

`run.sh` runs one CI Job through the Runner on a real Host with KVM, next
to battery-operator. The Runner takes the Job from a fake GitLab, claims a
MicroVM with a `MicroVMClaim`, and runs the Job's script in the MicroVM
through battery-operator's Exec Agent. The MicroVM boots this repository's
Guest Image under Firecracker.

This is a manual harness. CI has no KVM, so CI runs the same path on fakes
only (`cmd/flr/run_claim_e2e_test.go`).

## What it needs

- A cluster with battery-operator deployed, and a Host that meets
  [deploy/README.md](../../deploy/README.md): steps 1 and 2 of its order
  of application are done. battery-operator's real hosts trial
  (`hack/real-hosts` in battery-operator, from `main` at #163 or later) is
  such a cluster: one Lima VM, `bo-host-1`, that is the Node, the k3s
  server and the only Host.
- On the Host:
  - k3s. `run.sh` imports the flr image into k3s's containerd with
    `k3s ctr`. On another distribution, change `import_to_kubelet`.
  - `ctr` for `flintlockd`'s containerd, and a registry on
    `127.0.0.1:5000` (`REGISTRY`) that `flintlockd` pulls from.
  - Guest networking on the bridge `flintlockd` uses: DHCP, DNS and NAT to
    the internet. The trial has it from battery-operator#163.
- An arm64 Host. The Job asks for `image: flr-arm64`, the arm64 Profile in
  `runner-config.yaml`.
- Where `run.sh` runs: `docker` with buildx, Go, `kubectl`, and
  `KUBECONFIG` set to the cluster's kubeconfig. For the trial that is its
  `hack/real-hosts/.state/kubeconfig`.
- A way to reach the Host. `HOST_DRIVER=lima` (the default) reaches the
  Lima VM `NODE_NAME` (`bo-host-1`) with `limactl shell`, through `mac` on
  an OrbStack machine. `HOST_DRIVER=ssh HOST_SSH=user@host` reaches any
  other Host; its user needs sudo.

Nothing is pushed to a public registry.

## Steps

```sh
export KUBECONFIG=<battery-operator>/hack/real-hosts/.state/kubeconfig
hack/real-host/run.sh          # images, deploy, job
hack/real-host/run.sh job      # another Job on the same Runner
hack/real-host/run.sh clean    # the Runner, its Pool and the fake GitLab go
```

| Step | What it does |
|---|---|
| `images` | Builds `flr` and `fake-gitlab` for the Host's architecture, each into an image of one binary (`hack/release/flr.Containerfile`, `fake-gitlab/Containerfile`). Imports both into k3s's containerd. Builds the Guest Image with `guest/build.sh --load`, and pushes the kernel and the root filesystem to the Host's registry. Writes the references to `.state/images.env`, the Guest Image by digest. |
| `deploy` | Applies `deploy/namespace.yaml` and the Secret with the fake's runner token. Deploys the fake GitLab (`fake-gitlab.yaml`). Deploys the Runner from `deploy/runner` through an overlay in `.state/overlay`: the flr image from `images`, and `runner-config.yaml` as its configuration, with the Guest Image references in place. The Runner keeps the probes of `deploy/runner`: its pod is Ready once its token is verified and one of its Pools is Ready (OB-031). Waits until the Runner's Pool `lima-arm64` is Ready. |
| `job` | Queues `job.json` on the fake GitLab. While the Job runs, it records `kubectl get microvmclaims -A` once the claim is Bound. When the Job ends, it waits for the claim to go. It prints the Job's status and log, and the claims during and after, and writes them to `.state/run-<time>.log`. |
| `clean` | Deletes the Runner, the fake GitLab, the claims and Pools in `flintlock-system`, and the namespace. battery-operator stays. The images stay on the Host. |

Each step can run again. `images` tags each image by its content, so a new
build rolls the pods and the same build does not. `deploy` applies. `job`
queues a new Job each time.

`runner-config.yaml` differs from `deploy/runner/config.yaml` in four ways,
each said in the file: the fake GitLab over plain HTTP, one arm64 Profile
as the default, the Guest Image from the Host's registry, and no Host
Services. The trial runs no Host Agent, because the Host Agent needs the
Host Image's `/run/flr/host.env`.

## The fake GitLab

`fake-gitlab/` serves `internal/testing/fakegitlab` on port 8080, in a pod
in `flintlock-system`. The Runner reaches it at
`http://fake-gitlab.flintlock-system:8080`. `run.sh` reaches it through
the API server's proxy to its Service:

| Request | What |
|---|---|
| `POST /fake/jobs` | Queues the Job in the body, in GitLab's JSON form, with a new id. Answers `{"id": N}`. |
| `GET /fake/jobs` | Every Job handed out: status, states, exit code and log. |
| `GET /fake/jobs/N/status`, `GET /fake/jobs/N/trace` | One Job's status (`pending` while queued) or log, as plain text. |

It also logs each Job's status and log when the Job ends:
`kubectl -n flintlock-system logs deploy/fake-gitlab`.

## The Job

`job.json` asks for `image: flr-arm64`, sets `GIT_STRATEGY: none` because
the fake GitLab serves no repository, and runs:

```sh
uname -a
cat /etc/os-release
git clone --depth 1 https://github.com/liquidmetal-dev/flintlock flintlock
git -C flintlock log -1 --format='cloned liquidmetal-dev/flintlock at %H'
```

## What a run proves

A `job` that passes shows, on a real Host:

- The Runner starts from `deploy/runner` with the claim backend, as its own
  ServiceAccount with `deploy/runner/rbac.yaml`, and declares its Pool.
  battery fills the Pool with MicroVMs of the Guest Image.
- The Runner takes the Job, and claims a MicroVM for it. The claim is
  `Bound` on the Host's Node while the Job runs.
- Each Stage reaches the MicroVM through the Exec Agent of the claim's
  Host, with TLS verified against battery-operator's serving CA and a
  claim token of the Holder.
- The Guest Image boots and runs the Job's shell: `uname -a` shows the
  guest's kernel, and `/etc/os-release` shows Ubuntu 24.04.
- The guest reaches the internet: it gets an address by DHCP, resolves
  `github.com`, and clones over HTTPS through the Host's NAT.
- The Job succeeds in GitLab, and the Runner deletes the claim when the
  Job ends.

It does not show the Host Services, the Host Image, more than one Host, or
a real GitLab.

## What the first run found

On the trial's Lima Host `bo-host-1` (an M4 Mac), with battery-operator at
`08d8273`, on 28 September 2026:

- `flr` serves no `/healthz` or `/readyz` (OB-030 to OB-032, #94).
  `deploy/runner`'s probes get a 404, so the Runner's pod never becomes
  Ready, and the kubelet restarts it about every two minutes. `flr` now
  serves both. Under the claim backend, `/readyz` passes once the token is
  verified and one of the Runner's Pools is Ready (OB-031). The overlay
  keeps the probes of `deploy/runner`. The next run checks them on a real
  cluster.
- The Pool of one MicroVM is Ready within seconds. The Job gets its
  MicroVM in about 1 s, the guest agent answers in about 60 ms, and the
  clone from GitHub takes 3 to 5 s.
- `kubectl get microvmclaims` shows `<invalid>` under `EXPIRES` for a
  Bound claim. kubectl prints that for a time in the future.
