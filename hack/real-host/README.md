# Jobs on a real Host

`run.sh` runs CI Jobs through the Runner on a real Host with KVM, next to
battery-operator. The Runner takes each Job from a fake GitLab, claims a
MicroVM with a `MicroVMClaim`, and runs the Job's script in the MicroVM
through battery-operator's Exec Agent. The Host Agent runs the Host
Services on the Host. The MicroVM boots this repository's Guest Image
under Firecracker.

This is a manual harness. CI has no KVM, so CI runs the same path on fakes
only (`cmd/flr/run_claim_e2e_test.go`).

## The cluster

The Host boots battery-operator's Host Image, with SELinux enforcing, and
joins a kubeadm cluster with Calico. battery-operator's
[`hack/kubeadm-host`](https://github.com/phoban01/battery-operator/tree/main/hack/kubeadm-host)
makes such a cluster on an Apple silicon Mac:

| Node | What runs there |
|---|---|
| `bo-kubeadm-cp`, a Lima VM | the kubeadm control plane, battery-operator and cert-manager; here the Runner and the fake GitLab, which keep off Hosts |
| `bo-kubeadm-host-1`, a vfkit VM | the Host: the Exec Agent, `flintlockd`, the MicroVMs; here the Host Agent and its Host Services |

`run.sh` no longer supports battery-operator's older real hosts trial
(`hack/real-hosts`, k3s on Ubuntu). That Host is not the Host Image: it
has no `/run/battery/host.env`, so the Host Agent cannot run there.

### The Host pool

The Host Services need two settings in the Host's `host.conf`, from
[`deploy/host-agent/config/host-pool.conf`](../../deploy/host-agent/config/host-pool.conf)
(KF-198). Without them the Host Agent's `render` container fails and no
Host Service starts (KF-199). `hack/kubeadm-host` renders `host.conf`
from its `pool/host-pool.yaml`, which leaves them out. Add both lines to
its `host.conf` before `up.sh` boots the Host, in a checkout of
battery-operator that you keep for the run:

```yaml
  host.conf: |
    GUEST_SUBNET=10.220.0.0/16
    # ... the lines that are there ...
    GATEWAY_SERVICE_PORTS=1234,3000,5000,3128
    GATEWAY_SERVICE_UIDS=101,1000,10001,10002,100000-165535
```

The Host reads `host.conf` once, at its first boot. To change it on a
Host that is up, run `down.sh` and `up.sh` again. battery-operator#224
asks for a way to add the lines without the edit. The `deploy` step
checks the Host's `host.env` and stops early when it lacks either value.

### Reaching the cluster from an OrbStack machine

On some Macs an OrbStack machine cannot reach the VMs' network
(battery-operator#205). Then:

- `HOST_SSH_VIA_MAC=1` for `up.sh`, so that its ssh to the Host goes
  through `mac nc`. Its `.state/ssh_config` keeps that, and `run.sh` uses
  the same file.
- A forwarder from `127.0.0.1:6443` to the control plane's port 6443
  through `mac nc`, and the kubeconfig pointed at it:

  ```yaml
  server: https://127.0.0.1:6443
  tls-server-name: <the control plane's address>
  ```

  The forwarder is not in either repository. Any TCP forwarder that
  starts `mac nc <address> 6443` for each connection works.

## What it needs

- The cluster above, after `hack/kubeadm-host/up.sh`: battery-operator is
  deployed and the Host is ready.
- An arm64 Host. The Jobs ask for `image: flr-arm64`, the arm64 Profile
  in `runner-config.yaml`.
- Where `run.sh` runs: `docker` with buildx, Go, `kubectl`, `curl`, and
  `mac` on an OrbStack machine (or the Mac itself), for `limactl`.

The flr image and the fake GitLab's image go straight into the kubelet's
containerd on each Node. Nothing is pushed to a public registry.

## Steps

```sh
export KUBEADM_HOST=<battery-operator>/hack/kubeadm-host
hack/real-host/run.sh                        # images deploy job services
hack/real-host/run.sh job                    # another Job on the same Runner
hack/real-host/run.sh fault-flintlockd       # stop flintlockd mid-Job
hack/real-host/run.sh fault-flintlockd-hang  # freeze flintlockd mid-Job
hack/real-host/run.sh fault-exec-agent-hang  # freeze the Exec Agent mid-Job
hack/real-host/run.sh fault-not-ready        # the Host not ready mid-Job
hack/real-host/run.sh clean                  # the Runner, the Host Agent, the Pool and the fake GitLab go
```

`KUBEADM_HOST` gives `run.sh` the cluster's kubeconfig and ssh
configuration from its `.state`. Without it, set `KUBECONFIG`, and
`HOST_SSH` and `SSH_OPTS` for the Host.

| Step | What it does |
|---|---|
| `images` | Builds `flr` and `fake-gitlab` for the Host's architecture, each into an image of one binary (`hack/release/flr.Containerfile`, `fake-gitlab/Containerfile`). Imports both into the kubelet's containerd on the Host and on the control plane, with `ctr`. Takes the Guest Image's digests from the release `RELEASE`'s `images.txt`, after checking that file against the release's `checksums.txt`. Writes the references to `.state/images.env`. |
| `deploy` | Checks that the Host is ready and that its `host.env` has the Host pool's settings. Applies the namespace and the Secret with the fake's runner token. Deploys the fake GitLab (`fake-gitlab.yaml`). Deploys the Fleet Manifests, `deploy/`, through an overlay in `.state/overlay`: the flr image from `images`, and `runner-config.yaml` as the Runner's configuration, with the Guest Image references in place. Waits for the Host Agent, the Runner and its Pool `real-host-arm64`, and prints the Host Service annotations of the Host's Node. |
| `job` | Queues `job.json` on the fake GitLab. While the Job runs, it records `kubectl get microvmclaims -A` once the claim is Bound. When the Job ends, it waits for the claim to go. It prints the Job's status and log, and the claims during and after, and writes them to `.state/run-job-<time>.log`. |
| `services` | As `job`, with `job-host-services.json`, which fetches a Go module through the Go module proxy and an image manifest through the registry mirror. Then it reports on the Host Agent: `getenforce`, the SELinux label of each Host Service's process, the cache `emptyDir` on the Host with its labels and sizes, and every AVC denial since boot, from the journal (the Host Image has no `ausearch`). It fails on a denial of the Host Agent's SELinux level, except buildkitd's probe of the Host's emulators (battery-operator#222). Writes the report to `.state/host-agent-<time>.log`. |
| `cache-demo`, `build-demo`, `kind`, `kind-ready` | The example Jobs of `docs/docker-and-kind.md` on the Docker and kind Profiles: `job-cache-demo.json` (a timed `docker pull` through the registry mirror and `go get` through `GOPROXY`), `job-build-demo.json` (`go build`, `docker buildx build` on the Host's buildkitd, `docker run`) and `job-kind.json` (a kind cluster with `flr-kind-cluster`, `kubectl cluster-info`, an nginx pod Ready, and the guest's boot timings). `kind-ready` runs `job-kind-ready.json`, the same Job on `flr/kind-ready`, whose Pool made the cluster in its create hook, and checks that `flr-kind-cluster` found it running. Each checks Docker Engine in the guest (overlay2, cgroup v2, the mirror, legacy iptables) and `io.max` of `containers.slice` (GI-072), and adds the Job's `TIMING` and `BOOT` lines to `.state/timings.txt`, then prints the AVC denials on the Host. They need `deploy` with `PROFILES=docker`, `PROFILES=kind` or `PROFILES=kind-ready`. |
| `io-burst` | `job-io-burst.json` on `flr/docker`: two write bursts in containers, each long enough to hold the MicroVM's disk queue full without the limits of `containers.slice`: 50,000 small files with `sync`, then `fio --rw=randwrite --bs=4k --iodepth=64 --direct=1` for 120 s. The Job prints the limits the guest applied, where the Stage, the daemons and a container run, and the write IOPS each burst achieved from `io.stat` (`RESULT` lines). It passes when the Job ends; a dropped exec stream fails it (#133, #138). Needs `deploy` with `PROFILES=docker`. |
| `fault-flintlockd` | Waits for an available MicroVM, and queues `job-long.json`, which prints a line a second for ten minutes. Once it runs, and battery has replaced the claimed MicroVM in the Pool, it stops `flintlockd` and `flintlockd.path` on the Host, and times how long the Job takes to fail. It checks that the Job failed as `runner_system_failure` and that the Runner deleted its claim. Then it starts `flintlockd` again and waits for the Host and the Pool to be ready and the claim to be gone. |
| `fault-flintlockd-hang` | As `fault-flintlockd`, but the fault is `kill -STOP` of `flintlockd`, and `kill -CONT` puts it back. |
| `fault-exec-agent-hang` | As `fault-flintlockd`, but the fault is `kill -STOP` of battery-operator's Exec Agent. Its Node still says the Host is ready, so only the Runner's probe sees the fault (KF-200 to KF-202). |
| `fault-not-ready` | As `fault-flintlockd`, but the fault is a not ready reason file, `/run/battery/not-ready.d/flr-real-host`, which the Exec Agent reports (KF-203 to KF-205). `flintlockd` keeps running. The step removes the file at the end. |
| `clean` | Deletes the Runner, the Host Agent, the fake GitLab, the claims and Pools in `flintlock-system`, the rest of the Fleet Manifests and the namespace. battery-operator stays. The images stay on the Nodes. |

Each step can run again. `images` tags each image by its content, so a new
build rolls the pods and the same build does not. `deploy` applies. The
job and fault steps queue a new Job each time.

Settings:

| Variable | Default | What |
|---|---|---|
| `GUEST` | `release` | `release`: the Guest Image of `RELEASE`, which `flintlockd` pulls from ghcr.io. `build`: build all four images from `guest/` (with a docker-container builder, `BUILDX_BUILDER`) and push them to the Host's registry on `REGISTRY` (`hack/kubeadm-host`'s `images` step starts one on `127.0.0.1:5000`) |
| `RELEASE` | `v1.0.0-rc.9` | the release whose `images.txt` names the Guest Image |
| `NODE_NAME` | `bo-kubeadm-host-1` | the Host's Node |
| `CP_DRIVER`, `CP_NAME`, `CP_SSH` | `lima`, `bo-kubeadm-cp` | how to reach the Node that runs the Runner, to import images there: a Lima VM through `mac limactl`, or `ssh` to `CP_SSH` |
| `JOB_WAIT`, `FAULT_WAIT` | `900`, `180` | how long a step waits for its Job, in seconds |
| `POOL_WAIT` | `180` | how long a job step waits for a MicroVM to claim, in seconds. A MicroVM that misses battery's 30 s wait for its guest agent is replaced only at the Pool's next reseed, minutes later |
| `PROFILES` | `arm64` | the Profiles of `runner-config.yaml` that `deploy` keeps: `arm64`, `docker`, `kind`, `kind-ready`. `deploy` deletes the Pools of the others. On a Mac, keep one: two guests that boot at once often miss battery's 30 s wait |

`runner-config.yaml` differs from `deploy/runner/config.yaml` in three
ways, each said in the file: the fake GitLab over plain HTTP, one arm64
Profile as the default, and the Guest Image by digest. The Host Services
are on.

## The fake GitLab

`fake-gitlab/` serves `internal/testing/fakegitlab` on port 8080, in a pod
in `flintlock-system`. The Runner reaches it at
`http://fake-gitlab.flintlock-system:8080`. `run.sh` reaches it through
the API server's proxy to its Service:

| Request | What |
|---|---|
| `POST /fake/jobs` | Queues the Job in the body, in GitLab's JSON form, with a new id. Answers `{"id": N}`. |
| `GET /fake/jobs` | Every Job handed out: status, states, failure reason, exit code and log. |
| `GET /fake/jobs/N/status`, `GET /fake/jobs/N/failure_reason`, `GET /fake/jobs/N/trace` | One Job's status (`pending` while queued), failure reason (empty unless it failed) or log, as plain text. |

By hand:

```sh
P=/api/v1/namespaces/flintlock-system/services/fake-gitlab:http/proxy/fake
kubectl create --raw $P/jobs -f hack/real-host/job.json   # {"id":N}
kubectl get --raw $P/jobs/N/status
kubectl get --raw $P/jobs/N/trace
```

It also logs each Job's status and log when the Job ends:
`kubectl -n flintlock-system logs deploy/fake-gitlab`.

## The Jobs

Each Job asks for `image: flr-arm64` and sets `GIT_STRATEGY: none`,
because the fake GitLab serves no repository.

- `job.json` runs `uname -a`, `cat /etc/os-release`, and a shallow clone
  of `liquidmetal-dev/flintlock` from GitHub.
- `job-host-services.json` prints the Host Service variables, fetches
  `github.com/google/uuid@v1.6.0` from `$GOPROXY`, and the manifest of
  `library/busybox:1.37.0` from `$CI_REGISTRY_MIRROR`.
- `job-long.json` prints `tick N` once a second for ten minutes, for the
  fault steps.

## What a run proves

- The Runner starts from `deploy/runner` with the claim backend, as its own
  ServiceAccount with `deploy/runner/rbac.yaml`, and declares its Pool.
  battery fills the Pool with MicroVMs of the released Guest Image.
- The Runner takes each Job and claims a MicroVM for it. The claim is
  `Bound` on the Host's Node while the Job runs, and gone after.
- Each Stage reaches the MicroVM through the Exec Agent of the claim's
  Host, with TLS verified against battery-operator's serving CA and a
  claim token of the Holder.
- The guest reaches the internet through the Host's NAT.
- The Host Agent runs from `deploy/host-agent` on the Host Image, under
  SELinux enforcing, with its cache in the pod's `emptyDir`. A Job reaches
  the Go module proxy and the registry mirror on the guest bridge gateway.
- A Job whose Host loses `flintlockd`, whose Exec Agent stops answering,
  or whose Host reports not ready, fails as `runner_system_failure` well
  before its timeout, and the Runner deletes its claim (KF-200 to
  KF-205). See [What the runs found](#what-the-runs-found) for which of
  these have run.

It does not show more than one Host, a real GitLab, or buildkit and the
HTTP cache in use by a Job.

## What the runs found

### On k3s, 28 September 2026

On battery-operator's real hosts trial (`bo-host-1`, k3s on Ubuntu, an
M4 Mac), with battery-operator at `08d8273`:

- `flr` served no `/healthz` or `/readyz` (#94), so the Runner's pod never
  became Ready. `flr` now serves both (OB-030 to OB-032).
- The Pool of one MicroVM was Ready within seconds. A Job got its MicroVM
  in about 1 s, and the clone from GitHub took 3 to 5 s.

### On a kubeadm Host, 15 October 2026

On an M4 Mac, with battery-operator's `hack/kubeadm-host` at `main`
40954f0 (Host Image with Kubernetes v1.36.3, containerd v2.2.9, Calico
v3.31.2, SELinux enforcing), the Runner at this branch, and the Guest
Image of v1.0.0-rc.9:

- `images`, `deploy`, `job` and `services` passed. The Host Agent's pod
  had all 8 containers running within a minute. The Pool was Ready
  within seconds of the Runner's start.
- Each Job got its MicroVM in about 1 s, and the guest agent answered in
  about 110 ms. `job` took 18 s from queued to success, the clone 4 s.
  The claim was Bound on `bo-kubeadm-host-1` while each Job ran, and gone
  when it ended.
- Every Host Service ran as `container_t` at `s0:c311,c827`, buildkitd as
  `container_engine_t` at the same level. The cache `emptyDir` and each
  service's directory in it were `container_file_t:s0:c311,c827`, owned
  by each service's user id. The Job's Go module landed in `goproxy`, the
  image manifest in `registry`.
- The only AVC denials from the Host Agent were nine `map` denials of
  buildkitd's probe of the Host's `qemu-*-static` emulators, which the
  Host Image registers with binfmt_misc (battery-operator#222). buildkitd
  ran without them. The other denials of the boot were `bootupctl` and
  `chcon`, as the kubeadm Host proof found before.
- Stopping `flintlockd` failed the Job at once: the Stage's exec stream
  to `flintlockd` broke, and the Executor reported `runner_system_failure`
  ("the exec stream to flintlockd failed before the command's exit
  status"). That is the Guest Transport's path, not KF-200 to KF-205.
- Freezing `flintlockd` failed the Job 19.8 s after the fault, as
  `runner_system_failure`. The Exec Agent reported the Host not ready
  (`FlintlockdNotReady`) about 10 s after the fault, and the next
  heartbeat aborted the Job with that reason (KF-203 to KF-205). The
  Runner's probe had failed once by then (KF-200). The Runner deleted
  the claim at once. battery-operator kept its finalizer until battery
  could release the lease, once `flintlockd` ran again.
- Both `flintlockd` faults leaked a MicroVM: the Pool's replacement for
  the claimed MicroVM was still booting, its create hook failed, and
  battery did not retry its delete. After the second, the Pool stayed
  "provisioning" for good, said Ready, and served no claim
  (battery-operator#223). The fault steps now wait for the replacement
  before the fault, and every step that queues a Job waits for an
  available MicroVM first.
- `fault-exec-agent-hang` and `fault-not-ready` did not run: the leaked
  MicroVMs took all of the Host's MicroVM memory first.

### The kind Jobs, 16 October 2026

On the same Mac and `hack/kubeadm-host` at `main` 40954f0, with Profiles
of 2 vCPUs and 2 GiB, one at a time (#132):

- A guest of 3 GiB missed battery's 30 s wait for its guest agent in each
  of five boots. Guests of 2 GiB answered in 17 to 25 s, unless another
  guest booted or a Job ran beside them.
- With the Guest Image of v1.0.0-rc.10, the kind Job pulled the node
  image through the mirror in 347 s, then kind failed after 217 s waiting
  for the node's systemd. The Job failed after 697 s.
- With this branch the node image was in Docker Engine already, and kind
  took 6 to 8 s to find it. kind still failed at the same step, and the
  Job failed after 273 s. The cause is the guest's disk: Docker Engine
  copies the node image's `/var`, 391 MB, into a new volume when kind
  starts the node, which took 101 s and 205 s. A `docker run` of the node
  image with no volume took 17 s. Cold reads of the guest's root disk run
  at a few MB/s on this Host; writes ran at 450 to 530 MB/s (#133).
- Docker Engine was ready 71 s after the guest's kernel started, against
  109 s on the same kernel with the old unit order, and about 79 s with
  v1.0.0-rc.10. containerd and Docker Engine spend most of that reading
  their own binaries from the cold disk.
- No exec stream timed out in eight Jobs of up to 12 minutes, most with
  kind running, on either Guest Image. With this branch the guest agent
  ran in `agent.slice`, at OOM score adjustment -999.

### Docker's volumes on a tmpfs, 17 October 2026

On the same Mac, with `hack/kubeadm-host` at battery-operator `main`
40954f0 and the Guest Image built from #136's branch (`GUEST=build`),
one Profile at a time (#136):

- `dd if=/dev/zero of=/var/tmp/big bs=1M count=1024 oflag=direct` on
  `flr/docker` (2 vCPUs, 2 GiB) wrote 1 GiB in 4.2 s, at 263 MB/s. With
  `count=6144` it wrote 6 GiB in 45 s, at 24 to 145 MB/s. Both Jobs
  succeeded. No exec stream dropped.
- A `flr/kind` guest of 4 GiB missed battery's 30 s wait for its guest
  agent in each of four boots. The kind runs used 2 vCPUs and 2 GiB.
- The `kind` step failed as before: kind gave up waiting for the node's
  systemd after 253 s.
- A Job that ran `kind create cluster --retain` and looked at the node
  showed why. The node's `/var` is an anonymous volume under
  `/var/lib/docker/volumes`, now on the tmpfs. Docker Engine still took
  4 minutes to copy 390 MB into it, about 1.6 MB/s: it reads the node
  image from the guest's cold root disk. The node started 75 s later,
  and its systemd was still starting containerd when kind gave up.
  The tmpfs then held 390 MB of its 498 MB, and the guest had 1.36 GB
  of memory available.
- No exec stream dropped in any Job, including the 11-minute Job above.
- `kind-ready` did not run: its create hook would fail at the same step.

### Block I/O limits on containers.slice, 18 October 2026

On the same Mac, with `hack/kubeadm-host` at battery-operator `main`
40954f0 and the Guest Image built from #138's branch (`GUEST=build`),
one Profile at a time, `flr/docker` and `flr/kind` at 2 vCPUs and 2 GiB:

- In the guest, `io.max` of `containers.slice` was
  `254:0 rbps=60000000 wbps=60000000 riops=1500 wiops=1500`, and the root
  cgroup's `subtree_control` had `io`. systemd resolved `/` to the root
  disk, 254:0. The Stage ran in `/agent.slice/guest-agent.service`,
  containerd and Docker Engine in `containers.slice`, and a container in
  `/containers.slice/docker-<id>.scope`.
- `io-burst` passed three times. 50,000 small files from a container took
  69 to 108 s: 56,000 write IOs at 513 to 808 IOPS, under the cap of 1500.
  fio's random 4k direct writes at a queue depth of 64 ran at 1487 to 1493
  IOPS, 5.9 MiB/s, for 120 s: the cap, with 60 percent of the writes
  waiting about 100 ms in the throttle. No exec stream dropped in the
  three Jobs of 8 to 13 minutes.
- The cost of the cap: `apt-get install fio` in a container took 384 to
  392 s. Package unpacking is small writes, at 1500 IOPS at most.
- `cache-demo` and `build-demo` passed with the cap: `docker pull` 47.8 s
  (41.5 and 49.6 s in #130), `go get` 26.9 s (12.6 and 25.5 s), `go build`
  301 s (304 and 343 s), `buildx build` 12.3 s (18.3 and 20.9 s). The
  pull's writes are Docker Engine's, in the slice; the Go build's are the
  Stage's, outside it.
- A replacement MicroVM that booted while `io-burst` ran missed battery's
  30 s wait for its guest agent three times, as a guest that boots beside
  a Job did in the earlier runs. The Pool filled at its next reseed after
  the Job ended. `POOL_WAIT=900` covers that.
- `kind` failed as before, with the cap in place in the kind image: kind
  gave up waiting for the node's systemd after 223 s. The cold read of
  the node image is still the Mac's limit. No exec stream dropped.
