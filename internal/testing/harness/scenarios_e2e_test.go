//go:build e2e

package harness

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// scenario is one TD-051 case. The same table runs on every stack
// (TestFakeTier, TestClaimStack, TestHardwareTier), so that the battery and
// the claim backends meet the same assertions. Where a backend differs, the
// scenario says why beside the check.
type scenario struct {
	name string
	// fakeHostsOnly skips the scenario on the hardware tier, because it
	// injects a fault into a fake Host.
	fakeHostsOnly bool
	// opts adjusts the Stack's options.
	opts func(*Options)
	// run drives the scenario on a Stack whose Runner is polling for Jobs.
	run func(t *testing.T, s *Stack)
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# The harness SHALL cover a successful Job, a script failure with
//# its exit code, cancellation with `after_script`, a Job timeout, a wait on
//# an exhausted Pool, a Host becoming unhealthy during a Job, a Lease
//# expiring during a Job, an unknown Job Image, artifact upload and
//# dependency download, cache restore and save, and Host Service environment
//# injection.

// scenarios are the TD-051 cases.
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
			if s.Claim != nil {
				wantAgentExecs(t, s)
			}
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
			wantTrace(t, rec, "about to fail", "exit status 3")
			wantNotInTrace(t, rec, "not reached")
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
			wantNotInTrace(t, rec, "script finished")
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
			wantStatus(t, rec, fakegitlab.StatusFailed, "job_execution_timeout")
			wantTrace(t, rec, "sleeping past the timeout")
			wantNotInTrace(t, rec, "woke up")
			if d := time.Since(started); d > time.Minute {
				t.Errorf("a job with a 5s timeout took %s", d)
			}
		},
	},
	{
		// Two Jobs at once and a Pool of one. The second waits for a
		// MicroVM instead of failing, and runs on one of its own. The fake
		// Pool Manager replenishes the Pool after the boot delay, so there
		// the second Job can run while the first still holds its MicroVM.
		// The fake battery replaces a claimed MicroVM only when its claim
		// goes, so on the claim stack the second Job cannot start until the
		// first ends.
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
			if s.Claim != nil {
				// The Pool has no warm MicroVM while the first Job holds
				// its only one, so the Runner does not take the second Job.
				for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(pollInterval) {
					if rec := s.GitLab.Record(second); rec != nil {
						t.Fatalf("the second job was taken while the pool was exhausted: %s\n%s", rec.Status, rec.Trace)
					}
				}
			} else {
				time.Sleep(3 * time.Second)
			}
			if rec := s.GitLab.Record(second); rec != nil && rec.Status == fakegitlab.StatusFailed {
				t.Fatalf("the second job failed instead of waiting for the pool:\n%s", rec.Trace)
			}
			if err := gate.Open(); err != nil {
				t.Fatal(err)
			}
			rec := wait(t, s, second)
			wantStatus(t, rec, fakegitlab.StatusSuccess, "")
			wantTrace(t, rec, "second started")
			firstRec := wait(t, s, first)
			wantStatus(t, firstRec, fakegitlab.StatusSuccess, "")
			if vm1, vm2 := allocation(t, firstRec), allocation(t, rec); vm1 == vm2 {
				t.Errorf("both jobs ran on microvm %s", vm1)
			}
		},
	},
	{
		// The flintlockd of the Host running the Job stops answering. The
		// battery stack's Runner probes its Hosts (SC-041, SC-043). The
		// claim stack's Runner has no Inventory to probe; it probes the
		// Exec Agent of the Job's claim instead, which relays GetMicroVM to
		// the stalled flintlockd (KF-200, KF-201). Either way the Job fails
		// as a system failure well before its own timeout, does not finish,
		// and leaves nothing behind once the Host answers again.
		//
		// The Guest Transport's own watch (EX-051) also fails a Stage whose
		// Host stops answering, within the transport deadline, and the Job
		// then fails once its cleanup Stage has waited out the deadline as
		// well. Here the probes are short and the transport deadline long,
		// so KF-202's bound ends before the watch first asks after the Host,
		// at half the deadline. On the claim stack the claim's probe is what
		// fails the Job: its failure names the Exec Agent, and it comes
		// within the bound, plus the graceful kill timeout the Executor may
		// wait for the stalled Stage (EX-024) and a few seconds to report it.
		name:          "host becomes unhealthy during a job",
		fakeHostsOnly: true,
		opts: func(o *Options) {
			o.Configure = func(cfg *config.Config) {
				cfg.Scheduler.HostHealthInterval = time.Second
				cfg.Scheduler.HostCallDeadline = 2 * time.Second
				cfg.Executor.TransportDeadline = 30 * time.Second
			}
		},
		run: func(t *testing.T, s *Stack) {
			gate := s.NewGate("host")
			id, err := s.Enqueue(Job{
				Name: "stranded", Timeout: jobTimeout,
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
			rec := wait(t, s, id)
			took := time.Since(started)
			wantStatus(t, rec, fakegitlab.StatusFailed, "runner_system_failure")
			wantNotInTrace(t, rec, "job finished")
			limit := 90 * time.Second
			if s.Claim != nil {
				bound := agentProbeBound(s.Config)
				if bound >= s.Config.Executor.TransportDeadline/2 {
					t.Fatalf("KF-202's bound %s does not end before the transport watch's first probe, at half of %s", bound, s.Config.Executor.TransportDeadline)
				}
				wantTrace(t, rec, "Job failed (system failure): scheduler: host of microvm became unhealthy: the exec agent of claim")
				limit = bound + s.Config.Executor.GracefulKillTimeout + reportSlack
			}
			if took > limit {
				t.Errorf("the job took %s to fail after its host stopped answering, want within %s", took, limit)
			}
		},
	},
	{
		// The Job's Lease expires while it runs. The fake Pool Manager
		// refuses every heartbeat, as battery does for an expired Lease;
		// the fake battery marks the Job's claim Expired, as
		// battery-operator does when a Lease lapses (KF-154). Either way the
		// Scheduler fails the Job (SC-061). On both stacks the MicroVM
		// outlives the Lease for a while, so the Runner learns of the loss
		// from the Lease, not from a command killed with its MicroVM. The
		// next scenario deletes the MicroVM at once.
		name:          "lease expires during a job",
		fakeHostsOnly: true,
		run: func(t *testing.T, s *Stack) {
			gate := s.NewGate("lease")
			id, err := s.Enqueue(Job{Name: "expiring", Script: []string{`echo "job started"`, gate.Wait(), `echo "job finished"`}})
			if err != nil {
				t.Fatal(err)
			}
			waitTrace(t, s, id, "job started")
			defer func() { _ = gate.Open() }()
			if s.Claim != nil {
				claims, err := s.BoundClaims(context.Background())
				if err != nil || len(claims) != 1 {
					t.Fatalf("bound claims %v (%v), want one", claims, err)
				}
				s.Claim.Battery.ExpireKeepingMicroVM(claims[0])
			} else {
				s.PoolManager.SetFaults(poolmgr.Faults{RefuseHeartbeats: true})
			}
			rec := wait(t, s, id)
			if s.PoolManager != nil {
				// The Runner does not release a Lease the Pool Manager has
				// disowned; the fake refused its heartbeats without
				// forgetting it, and expires it at the threshold as battery
				// does, deleting its MicroVM.
				eventually(t, "the refused lease to expire", func() bool { return len(s.PoolManager.Leases()) == 0 })
				s.PoolManager.SetFaults(poolmgr.Faults{})
			}
			wantStatus(t, rec, fakegitlab.StatusFailed, "runner_system_failure")
			wantNotInTrace(t, rec, "job finished")
		},
	},
	{
		// The Job's Lease expires while it runs, and its MicroVM goes at
		// once. The fake battery's plain Expire deletes the MicroVM with the
		// Lease, as battery does. On the battery stack the fake Pool Manager
		// refuses every heartbeat and the test deletes the MicroVM on its
		// Host. The Stage ends with the exit status -1 before the next
		// heartbeat, so the Executor asks for the Lease at that moment and
		// the Job is a runner_system_failure, not a script_failure (SC-061).
		name:          "lease expires during a job and its microvm goes at once",
		fakeHostsOnly: true,
		run: func(t *testing.T, s *Stack) {
			gate := s.NewGate("lease-vm")
			id, err := s.Enqueue(Job{Name: "expiring-vm", Script: []string{`echo "job started"`, gate.Wait(), `echo "job finished"`}})
			if err != nil {
				t.Fatal(err)
			}
			waitTrace(t, s, id, "job started")
			defer func() { _ = gate.Open() }()
			if s.Claim != nil {
				claims, err := s.BoundClaims(context.Background())
				if err != nil || len(claims) != 1 {
					t.Fatalf("bound claims %v (%v), want one", claims, err)
				}
				s.Claim.Battery.Expire(claims[0])
			} else {
				s.PoolManager.SetFaults(poolmgr.Faults{RefuseHeartbeats: true})
				deleteLeasedMicroVM(t, s)
			}
			rec := wait(t, s, id)
			if s.PoolManager != nil {
				eventually(t, "the refused lease to expire", func() bool { return len(s.PoolManager.Leases()) == 0 })
				s.PoolManager.SetFaults(poolmgr.Faults{})
			}
			wantStatus(t, rec, fakegitlab.StatusFailed, "runner_system_failure")
			wantNotInTrace(t, rec, "job finished")
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
			if s.Claim != nil {
				for _, a := range s.Claim.Agents {
					if calls := a.Calls(); len(calls) > 0 {
						t.Errorf("the exec agent of %s saw %d call(s) for a job that should claim nothing", a.Node(), len(calls))
					}
				}
			}
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
		// One Job saves a cache and the next restores it, through the
		// distributed cache on the fake object store (CF-081). The Runner
		// signs the URLs with its instance credentials, and the guest gets
		// the URLs and nothing else (CF-082).
		name:          "cache restore and save",
		fakeHostsOnly: true,
		opts: func(o *Options) {
			o.HelperBinary = helperBinary
			o.Cache = true
		},
		run: func(t *testing.T, s *Stack) {
			cache := []spec.Cache{{
				Key: "harness-deps", Paths: spec.ArtifactPaths{"vendor/"},
				Policy: spec.CachePolicyPullPush, When: spec.CacheWhenOnSuccess,
			}}
			// The fake Hosts run the helper on this machine, so it trusts
			// the object store's certificate through SSL_CERT_FILE.
			vars := map[string]string{"SSL_CERT_FILE": s.CacheCA()}
			save := runJob(t, s, Job{
				Name:      "save",
				Script:    []string{`mkdir -p vendor && echo "cached by job $CI_JOB_ID" > vendor/cached.txt`},
				Cache:     cache,
				Variables: vars,
			})
			wantStatus(t, save, fakegitlab.StatusSuccess, "")
			wantTrace(t, save, "Uploading cache.zip to the distributed cache")
			if len(s.CacheObjects()) == 0 {
				t.Fatalf("the object store holds nothing after the job saved its cache:\n%s", save.Trace)
			}
			// Every Job of the harness shares one project directory on the
			// fake Hosts; empty it, so that only a restore can bring the
			// file back.
			if err := os.RemoveAll(s.buildsDir()); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(s.buildsDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			restore := runJob(t, s, Job{
				Name:      "restore",
				Script:    []string{`cat vendor/cached.txt`},
				Cache:     cache,
				Variables: vars,
			})
			wantStatus(t, restore, fakegitlab.StatusSuccess, "")
			wantTrace(t, restore, "Successfully extracted cache", fmt.Sprintf("cached by job %d", save.ID))
			for _, rec := range []*fakegitlab.JobRecord{save, restore} {
				wantNotInTrace(t, rec, CacheSecretKey, cacheAccessKey)
			}
		},
	},
	{
		// The Job is told where its Host's Host Services are: from the
		// Inventory on the battery stack (EX-060), and from the annotations
		// on the Host's Node on the claim stack (KF-189, KF-194).
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

// wantAgentExecs checks that the Stages reached the fake Hosts through
// their Exec Agents (KF-185), each call with a claim token the agent
// admitted.
func wantAgentExecs(t *testing.T, s *Stack) {
	t.Helper()
	execs := 0
	for _, a := range s.Claim.Agents {
		for _, c := range a.Calls() {
			if !c.Admitted {
				t.Errorf("the exec agent of %s refused %s on %q", a.Node(), c.Method, c.VMUID)
			}
			if c.Method == "ExecCommand" {
				execs++
			}
		}
	}
	if execs == 0 {
		t.Error("no Stage reached an exec agent")
	}
}

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
// deleteLeasedMicroVM deletes the MicroVM of the one Lease the fake Pool
// Manager holds on its fake Host, as battery does when a Lease expires. A
// command running in it is killed.
func deleteLeasedMicroVM(t *testing.T, s *Stack) {
	t.Helper()
	leases := s.PoolManager.Leases()
	if len(leases) != 1 {
		t.Fatalf("%d leases held, want 1", len(leases))
	}
	var hostName string
	for _, vm := range s.PoolManager.VMs() {
		if vm.UID == leases[0].VMUID {
			hostName = vm.Host
		}
	}
	h := s.Host(hostName)
	if h == nil {
		t.Fatalf("microvm %s is on %q, which is not a fake host of the stack", leases[0].VMUID, hostName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Client().DeleteMicroVM(ctx, leases[0].VMUID); err != nil {
		t.Fatalf("deleting microvm %s on %s: %v", leases[0].VMUID, hostName, err)
	}
}

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

// reportSlack is how long the Runner may take to report a Job it has
// already failed: the run loop's cleanup and the trace and status updates
// to the fake GitLab.
const reportSlack = 5 * time.Second

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# The Scheduler SHALL abort a Job under KF-201 no later than the
//# Host unhealthy threshold times the sum of the Host health interval and
//# the Host call deadline after the claim's Exec Agent stops answering.

// agentProbeBound is KF-202's bound for the Runner's configuration: the
// Host unhealthy threshold times the sum of the Host health interval and
// the Host call deadline.
func agentProbeBound(cfg *config.Config) time.Duration {
	sc := cfg.Scheduler
	return time.Duration(sc.HostUnhealthyThreshold) * (sc.HostHealthInterval + sc.HostCallDeadline)
}
