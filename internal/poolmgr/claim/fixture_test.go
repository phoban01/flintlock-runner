package claim_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim/claimtest"
)

const (
	runnerName      = "runner-a"
	runnerNamespace = "ci"
	holder          = "runner"
	// waitFor bounds every eventually; tick is how often it looks.
	waitFor = 30 * time.Second
	tick    = 20 * time.Millisecond
)

// The API server is shared by the tests of the package, each of which works
// in a namespace of its own. envNote says why there is none.
var (
	env     *claimtest.Env
	envNote string
)

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//= type=test
//# The claim backend SHALL be tested against a fake battery that
//# serves the `Pool` and `MicroVMClaim` resources

// TestMain starts the API server test environment, a real kube-apiserver
// and etcd serving battery-operator's CRDs. Each test runs claimtest's fake
// battery in its own namespace. Where the binaries are missing the tests
// skip with the way to get them, unless FLINTLOCK_RUNNER_REQUIRE_ENVTEST is
// set, as it is in CI, in which case the run fails.
func TestMain(m *testing.M) {
	var err error
	env, envNote, err = claimtest.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if env != nil {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stopping the test api server:", err)
		}
	}
	os.Exit(code)
}

// fixture is one test's namespace, with the fake battery running in it.
type fixture struct {
	t         *testing.T
	ctx       context.Context
	kube      client.WithWatch
	namespace string
	battery   *claimtest.Battery
	logs      *logBuffer
	profile   config.Profile
}

var namespaces atomic.Int32

// hosts are the fake Hosts the fake battery places MicroVMs on.
var hosts = []claimtest.Host{
	{NodeName: "host-a", AgentAddress: "10.0.0.1:9443"},
	{NodeName: "host-b", AgentAddress: "10.0.0.2:9443"},
}

// newFixture makes a namespace with the Holder and the serving CA in it, and
// runs the fake battery there.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	if env == nil {
		t.Skip(envNote)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{
		t:         t,
		ctx:       ctx,
		kube:      env.Client,
		namespace: fmt.Sprintf("claim-pool-%d", namespaces.Add(1)),
		logs:      &logBuffer{},
		profile: config.Profile{
			Name:     "default",
			Arch:     config.ArchARM64,
			VCPU:     2,
			MemoryMB: 4096,
			Kernel: config.Kernel{
				Image:   "ghcr.io/example/kernel:6.1",
				Cmdline: map[string]string{"console": "ttyS0"},
			},
			RootFS:       "ghcr.io/example/ubuntu-ci:24.04",
			Provider:     "firecracker",
			HostSelector: map[string]string{"disk": "nvme", "example.com/zone": "a"},
			Pool: config.PoolSettings{
				Size:              1,
				HeartbeatInterval: 200 * time.Millisecond,
				HeartbeatExpiry:   5 * time.Second,
			},
		},
	}
	f.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.namespace}})
	f.create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: f.namespace, Name: holder}})
	f.create(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.namespace, Name: "flintlockd-ca"},
		Data:       map[string]string{"serving-ca.crt": caPEM(t)},
	})

	f.battery = claimtest.NewBattery(env.Client, f.namespace, hosts...)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); f.battery.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return f
}

func (f *fixture) create(obj client.Object) {
	f.t.Helper()
	if err := f.kube.Create(f.ctx, obj); err != nil {
		f.t.Fatal(err)
	}
}

// options are the backend's options in the fixture's namespace.
func (f *fixture) options() claim.Options {
	return claim.Options{
		Config:          env.Config,
		Namespace:       f.namespace,
		RunnerName:      runnerName,
		RunnerNamespace: runnerNamespace,
		Holder:          holder,
		ServingCA:       config.ServingCAConfigMap{Namespace: f.namespace, Name: "flintlockd-ca", Key: "serving-ca.crt"},
		Profiles:        []config.Profile{f.profile},
		Deadline:        5 * time.Second,
		Log:             slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// backend builds a backend in the fixture's namespace. mutate, when given,
// adjusts the options.
func (f *fixture) backend(mutate ...func(*claim.Options)) *claim.Backend {
	f.t.Helper()
	opts := f.options()
	for _, m := range mutate {
		m(&opts)
	}
	b, err := claim.New(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = b.Close() })
	return b
}

// spec derives the Pool's spec from the fixture's Profile exactly as the
// Scheduler does, with the real SpecBuilder.
func (f *fixture) spec() poolmgr.PoolSpec {
	f.t.Helper()
	spec, err := poolmgr.NewSpecBuilder().Build(f.profile, poolmgr.SpecInput{RunnerName: runnerName, Namespace: runnerNamespace})
	if err != nil {
		f.t.Fatal(err)
	}
	return spec
}

// declare declares the fixture's Pool through the real Declarer and waits
// for the fake battery to fill it.
func (f *fixture) declare(b *claim.Backend) poolmgr.PoolRef {
	f.t.Helper()
	spec := f.spec()
	if _, err := poolmgr.NewDeclarer(b).Declare(f.ctx, spec); err != nil {
		f.t.Fatal(err)
	}
	f.waitAvailable(b, spec.Ref, spec.Size)
	return spec.Ref
}

// waitAvailable waits until GetPool reports n available MicroVMs.
func (f *fixture) waitAvailable(b *claim.Backend, ref poolmgr.PoolRef, n int32) {
	f.t.Helper()
	f.eventually(fmt.Sprintf("%d available in %s", n, ref), func() bool {
		pool, err := b.GetPool(f.ctx, ref)
		return err == nil && pool.Status.Available == n
	})
}

// claims lists the namespace's claims.
func (f *fixture) claims() []batteryv1alpha1.MicroVMClaim {
	f.t.Helper()
	list := &batteryv1alpha1.MicroVMClaimList{}
	if err := f.kube.List(f.ctx, list, client.InNamespace(f.namespace)); err != nil {
		f.t.Fatal(err)
	}
	return list.Items
}

// claim reads one claim, or reports that it is gone.
func (f *fixture) claim(name string) (*batteryv1alpha1.MicroVMClaim, bool) {
	f.t.Helper()
	obj := &batteryv1alpha1.MicroVMClaim{}
	err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: f.namespace, Name: name}, obj)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			f.t.Fatal(err)
		}
		return nil, false
	}
	return obj, true
}

func (f *fixture) eventually(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s\nlogs:\n%s", what, f.logs.String())
		}
		time.Sleep(tick)
	}
}

// logBuffer is a log sink safe for the concurrent writes of the backend.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// caPEM is a self-signed CA certificate, for the serving CA ConfigMap.
func caPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test serving ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
