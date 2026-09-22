// Package kubetest is the test environment of the Kubernetes pool backend: a
// real kube-apiserver and etcd from controller-runtime's envtest, a test
// reconciler that stands in for the ReplicaSet controller, and a stand-in for
// the kubelet that binds pods, reports them ready and finishes their deletion
// (docs/requirements/12-cluster-fleet.md#cluster-test-doubles). envtest runs
// no controller manager, no scheduler and no kubelet, so without these three
// a ReplicaSet would never have a pod. The package is imported by tests only.
package kubetest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	// AssetsEnv is the variable envtest reads the directory of the
	// kube-apiserver and etcd binaries from. `make envtest` prints it.
	AssetsEnv = "KUBEBUILDER_ASSETS"
	// RequireEnv is the variable that, set to 1, makes a test fail rather
	// than skip when the binaries are missing. CI sets it, so that CI never
	// skips.
	RequireEnv = "FLINTLOCK_RUNNER_REQUIRE_ENVTEST"
)

// Env is one running API server.
type Env struct {
	// Config is the administrator's client configuration.
	Config *rest.Config
	// Admin is the administrator's client, which the stand-ins and the
	// tests' own assertions use. A backend under test gets a client from
	// User instead, so that it runs with the Runner's permissions only.
	Admin kubernetes.Interface

	// users serialises AddUser, whose certificate authority is not safe for
	// the concurrent use parallel tests make of it.
	users sync.Mutex
	env   *envtest.Environment
}

// Assets finds the envtest binaries: the directory AssetsEnv names or,
// failing that, the newest version for this platform in setup-envtest's own
// store, which is where `make envtest` puts them. The second return value
// says what to do about it when there are none.
func Assets() (dir, hint string) {
	if dir := os.Getenv(AssetsEnv); dir != "" {
		return dir, ""
	}
	hint = "envtest binaries not found: run `make envtest`, or set " + AssetsEnv
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", hint
		}
		base = filepath.Join(home, ".local", "share")
	}
	pattern := filepath.Join(base, "kubebuilder-envtest", "k8s", fmt.Sprintf("*-%s-%s", runtime.GOOS, runtime.GOARCH))
	found, _ := filepath.Glob(pattern)
	sort.Strings(found)
	for i := len(found) - 1; i >= 0; i-- {
		if _, err := os.Stat(filepath.Join(found[i], "kube-apiserver")); err == nil {
			return found[i], ""
		}
	}
	return "", hint
}

// Start starts an API server. It returns a nil Env and the reason when the
// binaries are missing, for the caller to skip on; with RequireEnv set that
// is an error instead.
func Start() (*Env, string, error) {
	dir, hint := Assets()
	if dir == "" {
		if os.Getenv(RequireEnv) == "1" {
			return nil, "", fmt.Errorf("%s is set and %s", RequireEnv, hint)
		}
		return nil, hint, nil
	}
	env := &envtest.Environment{BinaryAssetsDirectory: dir}
	cfg, err := env.Start()
	if err != nil {
		return nil, "", fmt.Errorf("starting the test api server from %s: %w", dir, err)
	}
	admin, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		_ = env.Stop()
		return nil, "", err
	}
	return &Env{Config: cfg, Admin: admin, env: env}, "", nil
}

// Stop stops the API server.
func (e *Env) Stop() error { return e.env.Stop() }

// User returns the client configuration of a user of that name, who has no
// permission but what the test then binds to it. A test that wants to get
// between the backend and the API server wraps the configuration's
// transport before building a client from it.
func (e *Env) User(t testing.TB, name string) *rest.Config {
	t.Helper()
	e.users.Lock()
	user, err := e.env.AddUser(envtest.User{Name: name}, nil)
	e.users.Unlock()
	if err != nil {
		t.Fatalf("adding user %s: %v", name, err)
	}
	return user.Config()
}
