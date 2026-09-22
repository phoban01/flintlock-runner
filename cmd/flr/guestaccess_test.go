package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/executor"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// apiRecorder is an HTTP server standing in for the API server: it records
// the path and credentials of every request and answers a read of a Node
// with a Virtual Node carrying a buildkit annotation.
type apiRecorder struct {
	mu       sync.Mutex
	requests []string
}

func (a *apiRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	a.mu.Unlock()
	if name, ok := strings.CutPrefix(r.URL.Path, "/api/v1/nodes/"); ok {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&corev1.Node{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
				kubelabels.HostServiceAnnotation("buildkit"): "10.200.0.1:1234",
			}},
		})
		return
	}
	http.Error(w, "not here", http.StatusNotFound)
}

func (a *apiRecorder) seen(prefix string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.requests {
		if strings.HasPrefix(r, prefix) {
			return r
		}
	}
	return ""
}

func (a *apiRecorder) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

//= docs/requirements/12-cluster-fleet.md#kube-allocation
//= type=test
//# Where the Kubernetes pool backend is configured, the Executor
//# SHALL use the `kube-exec` Guest Transport for every Profile

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//= type=test
//# Where the `kube-exec` Guest Transport is configured, the Runner
//# SHALL NOT open any connection to a Host.

// TestGuestAccessOfTheKubernetesBackend checks what `run` wires for a
// cluster fleet. The Guest Transport factory builds kube-exec, whose
// sessions go to the API server of the pool backend's kubeconfig, with its
// credentials, in the pool backend's namespace; the Host Service lookup
// reads the Virtual Node from the same API server; the Executor is told to
// use kube-exec for every Profile; and the Host Registry has no endpoints
// and a dialer that refuses. Battery keeps the Inventory, the dialer and
// the per-Profile transports it had.
func TestGuestAccessOfTheKubernetesBackend(t *testing.T) {
	t.Parallel()
	api := &apiRecorder{}
	server := httptest.NewTLSServer(api)
	t.Cleanup(server.Close)
	kubeconfig := strings.ReplaceAll(testKubeconfig, "server: https://127.0.0.1:1",
		"server: "+server.URL+"\n      insecure-skip-tls-verify: true")
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		GitLab: config.GitLab{Name: "runner-a"},
		PoolManager: config.PoolManager{
			Backend:    config.PoolBackendKubernetes,
			Kubernetes: &config.KubernetesPools{Kubeconfig: path, Context: "fleet", Namespace: "runners"},
		},
		Scheduler: config.Scheduler{Namespace: "ci"},
	}
	client, err := newPoolClient(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	access := newGuestAccess(cfg, client, newInventoryView(nil), log)

	if len(access.endpoints) != 0 {
		t.Errorf("host registry endpoints = %v, want none", access.endpoints)
	}
	if _, err := access.dialer.Dial(context.Background(), flintlock.Endpoint{Name: "host-a", Address: "10.0.1.10:9090"}); err == nil {
		t.Error("the cluster fleet's host dialer dialled a host")
	}
	if len(access.options) != 1 {
		t.Errorf("executor options = %d, want the one that makes every profile use kube-exec", len(access.options))
	}

	tr, err := access.transports.New(context.Background(), transport.Target{Kind: transport.KindKubeExec, VMUID: "pool-default-abcde"})
	if err != nil {
		t.Fatalf("the factory does not build kube-exec: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tr.Ready(ctx); !errors.Is(err, transport.ErrNotReady) {
		t.Errorf("Ready against a server that refuses = %v, want ErrNotReady", err)
	}
	if got := api.seen("GET /api/v1/namespaces/runners/pods/pool-default-abcde/exec Bearer not-a-real-token"); got == "" {
		t.Errorf("no exec session reached the pool backend's API server with its credentials; requests: %v", api.all())
	}

	entry, ok := access.inventory.Host("host-7-microvms")
	if !ok || entry.Services.Buildkit != "tcp://10.200.0.1:1234" {
		t.Errorf("the virtual node lookup = %+v, %t; want buildkit from the annotation", entry, ok)
	}
	if got := api.seen("GET /api/v1/nodes/host-7-microvms Bearer not-a-real-token"); got == "" {
		t.Errorf("the virtual node was not read from the pool backend's API server; requests: %v", api.all())
	}

	battery := &config.Config{
		PoolManager: config.PoolManager{Endpoint: "127.0.0.1:1", TLS: config.ClientTLS{Insecure: true}},
		Inventory: config.Inventory{Hosts: []config.HostEntry{{
			Name: "host-a", Endpoint: "10.0.1.10:9090", TLS: config.ClientTLS{Insecure: true},
		}}},
	}
	bc, err := newPoolClient(battery, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bc.Close() }()
	inv := newInventoryView(battery.Inventory.Hosts)
	access = newGuestAccess(battery, bc, inv, log)
	if _, refusing := access.dialer.(noHostDialer); refusing || len(access.endpoints) != 1 || len(access.options) != 0 {
		t.Errorf("battery access = dialer %T, %d endpoints, %d options; want the gRPC dialer over the inventory and no override",
			access.dialer, len(access.endpoints), len(access.options))
	}
	if access.inventory != executor.InventoryLookup(inv) {
		t.Error("battery's host services do not come from the inventory")
	}
}
