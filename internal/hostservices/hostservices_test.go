package hostservices_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/phoban01/flintlock-runner/internal/hostservices"
)

// writeConfig writes a configuration file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host-services.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadConfig loads a configuration as render.sh makes it, with the
// Host's Node given by the override, and refuses the mistakes that would
// publish something the Executor never reads.
func TestLoadConfig(t *testing.T) {
	t.Parallel()
	good := "bridge_gateway: 10.200.0.1\nhost_services:\n  buildkit:\n    enabled: true\n    port: 1234\n  go_proxy:\n    enabled: false\n    port: 3000\n"
	cfg, err := hostservices.LoadConfig(writeConfig(t, good), func(c *hostservices.Config) { c.HostNode = "host-a" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostNode != "host-a" || cfg.ResyncInterval != hostservices.DefaultResyncInterval {
		t.Errorf("config = %+v", cfg)
	}
	if got := strings.Join(cfg.EnabledHostServices(), ","); got != "buildkit" {
		t.Errorf("enabled = %s, want buildkit", got)
	}

	for name, tc := range map[string]struct{ body, field string }{
		"no host node":      {body: good, field: "host_node"},
		"an unknown key":    {body: good + "flintlockd: 127.0.0.1:9090\n", field: "flintlockd"},
		"an unknown name":   {body: "bridge_gateway: 10.200.0.1\nhost_services:\n  buildkitd: {enabled: true, port: 1}\n", field: "host_services.buildkitd"},
		"a bad port":        {body: "bridge_gateway: 10.200.0.1\nhost_services:\n  buildkit: {enabled: true, port: 0}\n", field: "host_services.buildkit.port"},
		"no gateway":        {body: "host_services:\n  buildkit: {enabled: true, port: 1234}\n", field: "bridge_gateway"},
		"a negative resync": {body: good + "resync_interval: -1s\n", field: "resync_interval"},
	} {
		override := func(c *hostservices.Config) { c.HostNode = "host-a" }
		if name == "no host node" {
			override = nil
		}
		_, err := hostservices.LoadConfig(writeConfig(t, tc.body), override)
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: LoadConfig = %v, want an error naming %s", name, err, tc.field)
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-host-agent
//= type=test
//# The Host Agent SHALL publish the address and port of each
//# enabled Host Service on the guest bridge gateway, under the names
//# `buildkit`, `go_proxy`, `registry_mirror` and `http_cache`, as the
//# annotations `host-service.gitlab-runner.flintlock.dev/<name>` on its Host's
//# Node.

// TestRunPublishesTheHostServices runs the publisher against a Node that
// carries an old annotation of a Host Service now switched off, and one of
// something else. The enabled services are published at their address on
// the bridge gateway, the switched-off one is removed, the other annotation
// is kept, and the annotations are written again after something removes
// them. A write that fails is tried again.
func TestRunPublishesTheHostServices(t *testing.T) {
	t.Parallel()
	kube := kubefake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "host-a", Annotations: map[string]string{
		"host-service.gitlab-runner.flintlock.dev/http_cache": "10.200.0.1:8080",
		"example.com/unrelated":                               "kept",
	}}})
	var patches, failures atomic.Int32
	kube.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		if patches.Add(1) == 1 {
			failures.Add(1)
			return true, nil, errors.New("the API server is away")
		}
		return false, nil, nil
	})
	cfg := &hostservices.Config{
		HostNode:      "host-a",
		BridgeGateway: "10.200.0.1",
		HostServices: map[string]hostservices.HostService{
			"buildkit":        {Enabled: true, Port: 1234},
			"go_proxy":        {Enabled: true, Port: 3000},
			"registry_mirror": {Enabled: true, Port: 5000},
			"http_cache":      {Enabled: false, Port: 8080},
		},
		ResyncInterval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hostservices.Run(ctx, cfg, kube, nil) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	}()

	want := map[string]string{
		"host-service.gitlab-runner.flintlock.dev/buildkit":        "10.200.0.1:1234",
		"host-service.gitlab-runner.flintlock.dev/go_proxy":        "10.200.0.1:3000",
		"host-service.gitlab-runner.flintlock.dev/registry_mirror": "10.200.0.1:5000",
		"example.com/unrelated":                                    "kept",
	}
	annotations := func() map[string]string {
		node, err := kube.CoreV1().Nodes().Get(context.Background(), "host-a", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return node.Annotations
	}
	matches := func() bool {
		got := annotations()
		if len(got) != len(want) {
			return false
		}
		for k, v := range want {
			if got[k] != v {
				return false
			}
		}
		return true
	}
	eventually(t, "the Host Services published", matches)
	if failures.Load() != 1 {
		t.Errorf("%d failed writes, want the first one", failures.Load())
	}

	// Something else removes an annotation; the next resync writes it back.
	node, err := kube.CoreV1().Nodes().Get(context.Background(), "host-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delete(node.Annotations, "host-service.gitlab-runner.flintlock.dev/buildkit")
	if _, err := kube.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the removed annotation written back", matches)
}

// eventually waits up to a few seconds for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
