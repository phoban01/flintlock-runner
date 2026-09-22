//go:build e2e

package harness

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// scenario is one TD-051 case. The same table runs on every stack
// (TestFakeTier, TestKubernetesStack, TestHardwareTier), so that the
// battery and the Kubernetes backends meet the same assertions; a scenario
// that has no meaning on a backend names it in notOn with the reason.
type scenario struct {
	name string
	// notOn maps a backend to the reason the scenario is skipped there.
	notOn map[Backend]string
	// opts adjusts the Stack's options.
	opts func(*Options)
	// run drives the scenario on a Stack whose Runner is polling for Jobs.
	run func(t *testing.T, s *Stack)
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=todo
//= tracking-issue=9
//# The harness SHALL cover a successful Job, a script failure with
//# its exit code, cancellation with `after_script`, a Job timeout, a wait on
//# an exhausted Pool, a Host becoming unhealthy during a Job, a Lease
//# expiring during a Job, an unknown Job Image, artifact upload and
//# dependency download, cache restore and save, and Host Service environment
//# injection.

// scenarios are the TD-051 cases. Every one runs except cache restore and
// save, which needs an S3 distributed cache no fake provides (cacheReason);
// that gap is why TD-051 is still tracked in issue #9.
var scenarios = []scenario{
	{
		name: "successful job",
		run: func(t *testing.T, s *Stack) {
			rec := runJob(t, s, Job{
				Name: "hello",
				Script: []string{
					`echo "hello from job $CI_JOB_ID ($CI_JOB_NAME)"`,
					`echo "stage cwd: $(pwd)"`,
					`for i in 1 2 3; do echo "step $i"; done`,
				},
			})
			wantStatus(t, rec, fakegitlab.StatusSuccess, "")
			if last := rec.States[len(rec.States)-1]; last != fakegitlab.StatusSuccess {
				t.Errorf("final state reported %q, want success", last)
			}
			wantTrace(t, rec, "hello from job", "step 3", "Job succeeded")
			// The Stage ran in the Profile's builds directory under the
			// root, never in /builds.
			wantTrace(t, rec, "stage cwd: "+filepath.Join(s.Root, "builds"))
		},
	},
	{
		name: "script failure with its exit code",
		run: func(t *testing.T, s *Stack) {
			rec := runJob(t, s, Job{
				Name:   "fails",
				Script: []string{`echo "about to fail"`, `exit 3`, `echo "not reached"`},
			})
			wantStatus(t, rec, fakegitlab.StatusFailed, "script_failure")
			if rec.ExitCode != 3 {
				t.Errorf("exit code %d, want 3", rec.ExitCode)
			}
			if !strings.Contains(rec.Trace, "about to fail") || strings.Contains(rec.Trace, "not reached") {
				t.Errorf("trace does not stop at the failing line:\n%s", rec.Trace)
			}
			wantTrace(t, rec, "exit status 3")
		},
	},
	{
		// GitLab cancels a running Job: the Runner stops the script, still
		// runs after_script in the same MicroVM, and reports the Job final.
		name: "cancellation with after_script",
		run: func(t *testing.T, s *Stack) {
			id, err := s.Enqueue(Job{
				Name:        "cancelled",
				Script:      []string{`echo "script started"`, `sleep 120`, `echo "script finished"`},
				AfterScript: []string{`echo "after_script ran"`},
			})
			if err != nil {
				t.Fatal(err)
			}
			waitTrace(t, s, id, "script started")
			if err := s.GitLab.Cancel(id); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			rec := wait(t, s, id)
			if rec.Status != fakegitlab.StatusCanceled {
				t.Errorf("status %s (reason %q), want canceled", rec.Status, rec.FailureReason)
			}
			wantTrace(t, rec, "after_script ran")
			if strings.Contains(rec.Trace, "script finished") {
				t.Errorf("the script ran to its end after the cancellation:\n%s", rec.Trace)
			}
			if d := time.Since(started); d > time.Minute {
				t.Errorf("the job took %s to end after it was cancelled", d)
			}
		},
	},
	{
		name: "job timeout",
		run: func(t *testing.T, s *Stack) {
			started := time.Now()
			rec := runJob(t, s, Job{
				Name:        "slow",
				Script:      []string{`echo "sleeping past the timeout"`, `sleep 120`, `echo "woke up"`},
				AfterScript: []string{`echo "after_script ran"`},
				Timeout:     5 * time.Second,
			})
			// GL-043 wants job_execution_timeout. Now and then the Runner
			// reports runner_system_failure instead, a race that
			// TestKnownBugJobTimeoutReportedAsSystemFailure demonstrates;
			// until it is fixed either reason passes here.
			if rec.Status != fakegitlab.StatusFailed ||
				(rec.FailureReason != "job_execution_timeout" && rec.FailureReason != "runner_system_failure") {
				t.Errorf("job ended %s (reason %q), want failed with job_execution_timeout\n%s", rec.Status, rec.FailureReason, rec.Trace)
			}
			if rec.FailureReason == "runner_system_failure" {
				t.Logf("the job timeout was reported as runner_system_failure (the GL-043 race)")
			}
			wantTrace(t, rec, "sleeping past the timeout")
			if strings.Contains(rec.Trace, "woke up") {
				t.Errorf("the script ran past the job timeout:\n%s", rec.Trace)
			}
			if d := time.Since(started); d > time.Minute {
				t.Errorf("a job with a 5s timeout took %s", d)
			}
		},
	},
	{
		// Two Jobs at once and a Pool of one: the second waits for the
		// Pool to be replenished instead of failing, and runs on a MicroVM
		// of its own.
		name: "wait on an exhausted pool",
		opts: func(o *Options) {
			o.PoolSize = 1
			o.BootDelay = 2 * time.Second
			o.Configure = func(c *config.Config) { c.GitLab.Concurrent = 2 }
		},
		run: func(t *testing.T, s *Stack) {
			gate := s.NewGate("first")
			first, err := s.Enqueue(Job{Name: "first", Script: []string{`echo "first started"`, gate.Wait()}})
			if err != nil {
				t.Fatal(err)
			}
			waitTrace(t, s, first, "first started")
			second, err := s.Enqueue(Job{Name: "second", Script: []string{`echo "second started"`}})
			if err != nil {
				t.Fatal(err)
			}
			// The only warm MicroVM is the first Job's; the second's has
			// to boot, and the second Job waits for it.
			rec := wait(t, s, second)
			if err := gate.Open(); err != nil {
				t.Fatal(err)
			}
			wantStatus(t, rec, fakegitlab.StatusSuccess, "")
			wantTrace(t, rec, "second started")
			firstRec := wait(t, s, first)
			wantStatus(t, firstRec, fakegitlab.StatusSuccess, "")
			vm1 := allocation(t, firstRec)
			vm2 := allocation(t, rec)
			if vm1 == vm2 {
				t.Errorf("both jobs ran on microvm %s", vm1)
			}
		},
	},
	{
		// The Host running the Job stops answering. The battery stack's
		// Runner probes its Hosts (SC-041, SC-043) and fails the Job as a
		// system failure well before its own timeout. A cluster fleet's
		// Runner reaches no Host (KF-063): its exec goes through the API
		// server to the Pod Provider, which reports the Virtual Node not
		// ready (KF-015), but nothing fails the Job, which runs out its
		// own timeout; TestKnownBugKubernetesUnresponsiveHost holds the
		// stricter assertion. On both stacks the Job fails and nothing is
		// left behind once the Host answers again.
		name: "host becomes unhealthy during a job",
		run: func(t *testing.T, s *Stack) {
			timeout := jobTimeout
			if s.Backend() == BackendKubernetes {
				timeout = 20 * time.Second
			}
			gate := s.NewGate("host")
			id, err := s.Enqueue(Job{
				Name: "stranded", Timeout: timeout,
				Script: []string{`echo "job started"`, gate.Wait(), `echo "job finished"`},
			})
			if err != nil {
				t.Fatal(err)
			}
			waitTrace(t, s, id, "job started")
			host := leaseHost(t, s)
			started := time.Now()
			host.SetFaults(flintlock.HostFaults{Unresponsive: true})
			defer host.SetFaults(flintlock.HostFaults{})
			defer func() { _ = gate.Open() }()
			if s.Backend() == BackendKubernetes {
				hosts, err := s.LeaseHosts(context.Background())
				if err != nil || len(hosts) != 1 {
					t.Fatalf("lease hosts %v (%v), want one", hosts, err)
				}
				vnode := s.KubeHost(hosts[0]).VirtualNode
				eventually(t, "the unresponsive host's virtual node to be not ready", func() bool {
					n, err := s.Cluster().Admin().CoreV1().Nodes().Get(context.Background(), vnode, metav1.GetOptions{})
					return err == nil && !nodeReady(n)
				})
			}
			rec := wait(t, s, id)
			if rec.Status != fakegitlab.StatusFailed {
				t.Errorf("status %s, want failed", rec.Status)
			}
			if s.Backend() != BackendKubernetes {
				wantStatus(t, rec, fakegitlab.StatusFailed, "runner_system_failure")
			}
			if strings.Contains(rec.Trace, "job finished") {
				t.Errorf("the job finished on an unresponsive host:\n%s", rec.Trace)
			}
			if d := time.Since(started); d > 90*time.Second {
				t.Errorf("the job took %s to fail after its host stopped answering", d)
			}
		},
	},
	{
		// The Job's Lease expires while it runs. The battery stack refuses
		// every heartbeat, as battery does for an expired Lease; on the
		// Kubernetes stack the Runner heartbeats less often than the Pod
		// Provider's lease duration, so the provider deletes the claimed
		// pod as expired (KF-032) and the next heartbeat finds it gone
		// (KF-047). Either way the Scheduler fails the Job (SC-061).
		name: "lease expires during a job",
		opts: func(o *Options) {
			if o.Backend != BackendKubernetes {
				return
			}
			o.ProviderLeaseDuration = 3 * time.Second
			o.Configure = func(c *config.Config) {
				c.Profiles[0].Pool.HeartbeatInterval = 6 * time.Second
				c.Profiles[0].Pool.HeartbeatExpiry = 30 * time.Second
			}
		},
		run: func(t *testing.T, s *Stack) {
			gate := s.NewGate("lease")
			id, err := s.Enqueue(Job{Name: "expiring", Script: []string{`echo "job started"`, gate.Wait(), `echo "job finished"`}})
			if err != nil {
				t.Fatal(err)
			}
			waitTrace(t, s, id, "job started")
			if s.PoolManager != nil {
				s.PoolManager.SetFaults(poolmgr.Faults{RefuseHeartbeats: true})
			}
			rec := wait(t, s, id)
			_ = gate.Open()
			if s.PoolManager != nil {
				// The Runner does not release a Lease the Pool Manager has
				// disowned; the fake refused its heartbeats without
				// forgetting it, and expires it at the threshold as battery
				// does, deleting its MicroVM.
				eventually(t, "the refused lease to expire", func() bool { return len(s.PoolManager.Leases()) == 0 })
				s.PoolManager.SetFaults(poolmgr.Faults{})
			}
			wantStatus(t, rec, fakegitlab.StatusFailed, "runner_system_failure")
			if strings.Contains(rec.Trace, "job finished") {
				t.Errorf("the job finished after its lease expired:\n%s", rec.Trace)
			}
		},
	},
	{
		// A Job Image no Profile matches is refused, with no fallback to
		// the Default Profile unless the configuration allows it (SC-012),
		// and no MicroVM is claimed for it.
		name: "unknown job image",
		run: func(t *testing.T, s *Stack) {
			rec := runJob(t, s, Job{
				Name:   "unknown-image",
				Image:  "registry.example.invalid/nobody/unknown:1.0",
				Script: []string{`echo "should not run"`},
			})
			if rec.Status != fakegitlab.StatusFailed {
				t.Errorf("status %s, want failed", rec.Status)
			}
			if strings.Contains(rec.Trace, "should not run") || allocatedRE.MatchString(rec.Trace) {
				t.Errorf("a job with an unknown image ran:\n%s", rec.Trace)
			}
			wantTrace(t, rec, "registry.example.invalid/nobody/unknown:1.0")
		},
	},
	{
		// The Job downloads the archive of a Job it depends on before its
		// script and uploads its own after, both through
		// gitlab-runner-helper in the guest, which here is the harness's
		// stand-in at the Profile's helper path (EX-018).
		name: "artifact upload and dependency download",
		opts: func(o *Options) { o.HelperBinary = helperBinary },
		run: func(t *testing.T, s *Stack) {
			const depID, depToken = 900, "glcbt-harness-dependency"
			dep := zipOf(t, map[string]string{"dep/input.txt": "from the dependency\n"})
			s.GitLab.SeedArtifact(depID, depToken, "artifacts.zip", dep)
			rec := runJob(t, s, Job{
				Name: "artifacts",
				Script: []string{
					`cat dep/input.txt`,
					`mkdir -p out && echo "built by job $CI_JOB_ID" > out/result.txt`,
				},
				Dependencies: []spec.Dependency{{
					ID: depID, Token: depToken, Name: "build",
					ArtifactsFile: spec.DependencyArtifactsFile{Filename: "artifacts.zip", Size: int64(len(dep))},
				}},
				Artifacts: []spec.Artifact{{
					Name: "result", Paths: spec.ArtifactPaths{"out/"}, When: spec.ArtifactWhenOnSuccess,
					Format: spec.ArtifactFormatZip, Type: "archive",
				}},
			})
			wantStatus(t, rec, fakegitlab.StatusSuccess, "")
			wantTrace(t, rec, "from the dependency", "Uploading artifacts")
			if len(rec.Uploads) != 1 {
				t.Fatalf("%d artifact uploads, want 1", len(rec.Uploads))
			}
			files := unzip(t, rec.Uploads[0].Content)
			if want := fmt.Sprintf("built by job %d\n", rec.ID); files["out/result.txt"] != want {
				t.Errorf("the uploaded archive holds %q, want out/result.txt = %q", files, want)
			}
		},
	},
	{
		name:  "cache restore and save",
		notOn: map[Backend]string{BackendBattery: cacheReason, BackendKubernetes: cacheReason},
	},
	{
		// The Job is told where its Host's Host Services are: from the
		// Inventory on the battery stack (EX-060), from the annotations
		// the Pod Provider publishes on the Virtual Node on the Kubernetes
		// stack (KF-017, KF-062).
		name: "host service environment injection",
		opts: func(o *Options) { o.HostServices = true },
		run: func(t *testing.T, s *Stack) {
			rec := runJob(t, s, Job{
				Name:   "services",
				Script: []string{`echo "BUILDKIT_HOST=$BUILDKIT_HOST"`, `echo "GOPROXY=$GOPROXY"`, `echo "GOFLAGS=$GOFLAGS"`},
			})
			wantStatus(t, rec, fakegitlab.StatusSuccess, "")
			wantTrace(t, rec,
				"BUILDKIT_HOST=tcp://"+s.ServiceAddr(kubelabels.HostServiceBuildkit),
				"GOPROXY=http://"+s.ServiceAddr(kubelabels.HostServiceGoProxy),
				"GOFLAGS=-modcacherw")
		},
	},
}

// cacheReason is why cache restore and save runs on no stack: the Runner
// supports `cache:` only through the S3 distributed cache (CF-080, CF-083),
// for which there is no fake, and the stand-in helper has no cache
// subcommands.
const cacheReason = "cache: needs the S3 distributed cache (CF-080, CF-083), which no fake provides"

// zipOf is a zip archive of files, by path.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// unzip reads a zip archive's files, by path.
func unzip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("the artifact is not a zip archive: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(content)
	}
	return out
}

// leaseHost is the fake Host of the one Lease held.
func leaseHost(t *testing.T, s *Stack) flintlock.HostFaultInjector {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hosts, err := s.LeaseHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("%d leases held (%v), want 1", len(hosts), hosts)
	}
	h := s.Host(hosts[0])
	if h == nil {
		t.Fatalf("the lease is on %q, which is not a fake host of the stack", hosts[0])
	}
	return h
}
