package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	pmfake "github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

// fakeEC2Endpoint answers the EC2 query API's DescribeInstances from the
// instances the test sets, and records the action of every request. It
// stands in for EC2 at AWS_ENDPOINT_URL_EC2, so the Runner's real SDK
// client is exercised without an AWS account.
type fakeEC2Endpoint struct {
	srv *httptest.Server

	mu        sync.Mutex
	instances []fakeEC2Instance
	actions   []string
	filters   []url.Values
}

// fakeEC2Instance is one running instance carrying the discovery tag.
type fakeEC2Instance struct {
	ID, IP string
}

func newFakeEC2Endpoint(t *testing.T) *fakeEC2Endpoint {
	t.Helper()
	f := &fakeEC2Endpoint{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEC2Endpoint) set(insts ...fakeEC2Instance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances = slices.Clone(insts)
}

func (f *fakeEC2Endpoint) calls() ([]string, []url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.actions), slices.Clone(f.filters)
}

func (f *fakeEC2Endpoint) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	f.actions = append(f.actions, form.Get("Action"))
	f.filters = append(f.filters, form)
	insts := slices.Clone(f.instances)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	if form.Get("Action") != "DescribeInstances" {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>not allowed: %s</Message></Error></Errors><RequestID>x</RequestID></Response>`, form.Get("Action"))
		return
	}
	var items strings.Builder
	for _, in := range insts {
		fmt.Fprintf(&items, `<item><instanceId>%s</instanceId><instanceType>c7g.metal</instanceType><architecture>arm64</architecture>`+
			`<privateIpAddress>%s</privateIpAddress><instanceState><code>16</code><name>running</name></instanceState>`+
			`<tagSet><item><key>flintlock-runner</key><value>ci</value></item></tagSet>`+
			`<cpuOptions><coreCount>64</coreCount><threadsPerCore>1</threadsPerCore></cpuOptions></item>`, in.ID, in.IP)
	}
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>req</requestId>
<reservationSet><item><reservationId>r-1</reservationId><instancesSet>%s</instancesSet></item></reservationSet>
</DescribeInstancesResponse>`, items.String())
}

// startFakeHost serves one fake flintlock Host until the test ends.
func startFakeHost(t *testing.T, name, root string) *hostfake.Host {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := hostfake.New(flintlock.FakeHostConfig{
		Name: name, Listen: "127.0.0.1:0", SandboxRoot: filepath.Join(root, name), Version: "v0.9.0", ExecEnabled: true,
	})
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-h.Ready():
	case err := <-done:
		t.Fatalf("fake host %s: %v", name, err)
	}
	return h
}

//= docs/requirements/06-fleet.md#launch-template-mode
//= type=test
//# Where launch template mode is selected, the Runner SHALL refresh
//# its Inventory from tag discovery at the configured interval so that
//# self-provisioned Hosts join without a restart.

// TestRunRefreshesInventoryFromTagDiscovery starts the flintlock-runner
// binary's `run` in launch template mode with a short refresh interval,
// with EC2 answered by an in-process endpoint. The Runner starts with the
// one Host it is configured with; an instance carrying the tag then
// appears, as one that provisioned itself from the user-data would, and
// joins the Runner's Pool without a restart; when it goes, it leaves. The
// Runner asks EC2 for nothing but DescribeInstances by the tag (SE-041).
func TestRunRefreshesInventoryFromTagDiscovery(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	hostA := startFakeHost(t, "host-a", filepath.Join(dir, "sandboxes"))
	joiner := startFakeHost(t, "i-0joiner", filepath.Join(dir, "sandboxes"))
	_, joinerPort, err := net.SplitHostPort(joiner.Addr())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	hosts := pmfake.NewHosts()
	hosts.Add("host-a", hostA.Client(), hostA.Addr())
	hosts.Add("i-0joiner", joiner.Client(), joiner.Addr())
	pm := pmfake.New(poolmgr.FakeConfig{Listen: "127.0.0.1:0", Hosts: hosts, ReconcileInterval: 50 * time.Millisecond, ReadyTimeout: 10 * time.Second})
	pmErr := make(chan error, 1)
	go func() { pmErr <- pm.Serve(ctx) }()
	select {
	case <-pm.Ready():
	case err := <-pmErr:
		t.Fatalf("fake pool manager: %v", err)
	}
	gl := fakegitlab.New(fakegitlab.Options{RunnerToken: testRunnerToken, LongPollTimeout: 500 * time.Millisecond})
	if err := gl.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gl.Close()
		cancel()
		<-pmErr
	})

	ec2 := newFakeEC2Endpoint(t)
	// host-a is the configured Host; its instance carries the tag too.
	ec2.set(fakeEC2Instance{ID: "host-a", IP: "127.0.0.1"})

	cfg := fmt.Sprintf(`
gitlab:
  url: %s
  token: %s
  name: e2e-runner
  allow_insecure: true
  check_interval: 1s
  shutdown_timeout: 5s
pool_manager:
  endpoint: %s
  tls:
    insecure: true
inventory:
  hosts:
    - name: host-a
      endpoint: %s
      arch: arm64
      vcpu: 8
      memory_mb: 16384
      tls:
        insecure: true
profiles:
  - name: default
    arch: arm64
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    shell: %s
    pool:
      size: 1
fleet:
  region: eu-west-1
  discovery:
    tag_key: flintlock-runner
    tag_value: ci
  versions:
    flintlock: v0.9.0
    firecracker: v1.10.0
    containerd: v1.7.0
    pool_manager: v0.1.0
  thin_pool_device: /dev/nvme1n1
  flintlockd:
    port: %s
    insecure: true
  host_reserve:
    vcpu: 2
    memory_mb: 4096
  inventory_path: %s/inventory.yaml
  launch_template:
    parameters:
      host_token: /flintlock-runner/host-token
    inventory_refresh_interval: 300ms
observability:
  log_format: text
  listen_address: 127.0.0.1:0
state_dir: %s
`, gl.URL(), testRunnerToken, pm.Addr(), hostA.Addr(), bashPath(t), joinerPort, dir, filepath.Join(dir, "state"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &syncBuffer{}
	cmd := exec.Command(bin, "--config", path, "run")
	cmd.Stdout, cmd.Stderr = out, out
	// The SDK's default chain finds these static credentials and sends EC2
	// calls to the fake. Any request that would leave for AWS instead goes
	// to a proxy that is not there; loopback is never proxied.
	cmd.Env = append(os.Environ(),
		"AWS_ENDPOINT_URL_EC2="+ec2.srv.URL,
		"AWS_IGNORE_CONFIGURED_ENDPOINT_URLS=false",
		"AWS_ACCESS_KEY_ID=AKIDFAKE",
		"AWS_SECRET_ACCESS_KEY=fake-secret",
		"AWS_SESSION_TOKEN=",
		"AWS_PROFILE=",
		"AWS_CONFIG_FILE="+os.DevNull,
		"AWS_SHARED_CREDENTIALS_FILE="+os.DevNull,
		"AWS_EC2_METADATA_DISABLED=true",
		"HTTPS_PROXY=http://127.0.0.1:9",
		"HTTP_PROXY=http://127.0.0.1:9",
		"NO_PROXY=",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	client := pm.Client()
	poolHosts := func() []string {
		select {
		case err := <-exited:
			t.Fatalf("runner exited early: %v\n%s", err, out.String())
		default:
		}
		pools, err := client.ListPools(context.Background(), "")
		if err != nil || len(pools) != 1 {
			return nil
		}
		hosts := slices.Clone(pools[0].Spec.FlintlockHosts)
		slices.Sort(hosts)
		return hosts
	}
	waitFor(t, 30*time.Second, "the Pool on the configured Host", func() bool {
		return slices.Equal(poolHosts(), []string{"host-a"})
	})

	// An instance provisions itself and carries the tag: it joins.
	ec2.set(fakeEC2Instance{ID: "host-a", IP: "127.0.0.1"}, fakeEC2Instance{ID: "i-0joiner", IP: "127.0.0.1"})
	waitFor(t, 30*time.Second, "the self-provisioned Host to join the Pool", func() bool {
		return slices.Equal(poolHosts(), []string{"host-a", "i-0joiner"})
	})
	// It is terminated: it leaves.
	ec2.set(fakeEC2Instance{ID: "host-a", IP: "127.0.0.1"})
	waitFor(t, 30*time.Second, "the terminated Host to leave the Pool", func() bool {
		return slices.Equal(poolHosts(), []string{"host-a"})
	})

	actions, forms := ec2.calls()
	if len(actions) < 3 {
		t.Errorf("EC2 was asked %d times, want a refresh at every interval", len(actions))
	}
	for i, a := range actions {
		if a != "DescribeInstances" {
			t.Errorf("the Runner called EC2 %s; it may call only DescribeInstances (SE-041)", a)
		}
		if f := forms[i]; f.Get("Filter.1.Name") != "tag:flintlock-runner" || f.Get("Filter.1.Value.1") != "ci" {
			t.Errorf("DescribeInstances filters = %v, want the discovery tag", f)
		}
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("runner exited with %v\n%s", err, out.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("runner did not stop\n%s", out.String())
	}
}
