// Package claimtest is the test environment of the claim pool backend: a
// real kube-apiserver and etcd from controller-runtime's envtest, serving
// battery-operator's Pool and MicroVMClaim resources from its own CRDs, and
// Battery, a fake that stands in for battery-operator and battery and binds
// claims (docs/requirements/12-cluster-fleet.md#claim-test-doubles). The
// package is imported by tests only.
//
// The CRDs under crds/ are battery-operator v0.1.0's config/crd/bases, as
// they are in the module go.mod requires. Copy them again when that
// version changes.
package claimtest

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
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

//go:embed crds/*.yaml
var crdFiles embed.FS

// Env is one running API server with battery-operator's CRDs installed.
type Env struct {
	// Config is the administrator's client configuration.
	Config *rest.Config
	// Client is the administrator's client, with a scheme that knows
	// battery-operator's v1alpha1.
	Client client.WithWatch

	env *envtest.Environment
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

// CRDs are battery-operator's CustomResourceDefinitions.
func CRDs() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	var out []*apiextensionsv1.CustomResourceDefinition
	err := fs.WalkDir(crdFiles, "crds", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := crdFiles.ReadFile(path)
		if err != nil {
			return err
		}
		dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := dec.Decode(crd); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if crd.Name != "" {
				out = append(out, crd)
			}
		}
	})
	return out, err
}

// Start starts an API server and installs the CRDs. It returns a nil Env and
// the reason when the binaries are missing, for the caller to skip on; with
// RequireEnv set that is an error instead.
func Start() (*Env, string, error) {
	dir, hint := Assets()
	if dir == "" {
		if os.Getenv(RequireEnv) == "1" {
			return nil, "", fmt.Errorf("%s is set and %s", RequireEnv, hint)
		}
		return nil, hint, nil
	}
	crds, err := CRDs()
	if err != nil {
		return nil, "", fmt.Errorf("reading the CRDs: %w", err)
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: dir,
		CRDInstallOptions:     envtest.CRDInstallOptions{CRDs: crds},
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, "", fmt.Errorf("starting the test api server from %s: %w", dir, err)
	}
	scheme, err := claim.Scheme()
	if err != nil {
		_ = env.Stop()
		return nil, "", err
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		return nil, "", err
	}
	return &Env{Config: cfg, Client: c, env: env}, "", nil
}

// Stop stops the API server.
func (e *Env) Stop() error { return e.env.Stop() }
