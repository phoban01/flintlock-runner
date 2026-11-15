// Package kindspec reads a project's kind cluster spec, checks it and finds
// its digest (docs/requirements/13-guest-image.md#guest-kind-spec).
//
// A spec is a YAML file, by convention `.flr/kind-cluster.yaml` in the
// project's repository. It describes the kind cluster that the project's
// e2e suite needs: the kind configuration, the images to load into the
// nodes and the manifests to apply (docs/proposals/kind-cluster-spec.md).
// flr-bake and flr-kind-cluster --spec both read it through this package,
// so that they agree on what a spec means and on its digest.
//
// Paths in a spec are relative to the directory that holds the spec. They
// stay inside the repository root. [Load] takes the root to be the parent
// of the spec's directory, which is the repository for
// `.flr/kind-cluster.yaml`; [LoadFromRoot] takes the root as an argument.
package kindspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion and Kind are the only apiVersion and kind a spec can have.
const (
	APIVersion = "flintlock-runner.dev/v1alpha1"
	Kind       = "KindCluster"
)

// DefaultVolumes is the volumes setting of a spec that does not set one:
// Docker Engine's volumes stay on the tmpfs (GI-069).
const DefaultVolumes = "tmpfs"

// Spec is a project's kind cluster spec, as [Load] reads it.
type Spec struct {
	// APIVersion is always [APIVersion].
	APIVersion string `yaml:"apiVersion"`
	// Kind is always [Kind].
	Kind string `yaml:"kind"`
	// Name is the kind cluster's name.
	Name string `yaml:"name"`
	// Volumes says where Docker Engine keeps its volumes: tmpfs,
	// tmpfs:SIZE or disk, as FLR_KIND_VOLUMES takes them (GI-081 to
	// GI-084). Load sets it to [DefaultVolumes] when the file has none.
	Volumes string `yaml:"volumes,omitempty"`
	// KindConfig is the kind configuration. A value with a line break is
	// the configuration itself; any other value is a path to it, relative
	// to the spec's directory. Empty means the kind image's own
	// configuration. [Spec.KindConfigInline] and [Spec.KindConfigData]
	// tell the two apart.
	KindConfig string `yaml:"kindConfig,omitempty"`
	// Images are the images to load into every node, each by digest.
	Images []string `yaml:"images,omitempty"`
	// Manifests are applied in order once every node is Ready.
	Manifests []Manifest `yaml:"manifests,omitempty"`

	// file is the spec's absolute path, and root the absolute path of the
	// directory that the spec's paths cannot leave.
	file string
	root string
}

// Manifest is one entry under manifests. It has exactly one source: URL,
// Path, Kustomize or Helm.
type Manifest struct {
	// URL is a manifest to download, and SHA256 the sha256 of its content
	// as 64 hexadecimal digits.
	URL    string `yaml:"url,omitempty"`
	SHA256 string `yaml:"sha256,omitempty"`
	// Path is a manifest file, relative to the spec's directory.
	Path string `yaml:"path,omitempty"`
	// Kustomize is a kustomize directory, relative to the spec's
	// directory.
	Kustomize string `yaml:"kustomize,omitempty"`
	// Helm is a chart to install.
	Helm *Helm `yaml:"helm,omitempty"`
	// Wait lists what to wait for after the entry is applied, each as
	// `KIND/NAME [-n NAMESPACE]`; [ParseWait] reads one.
	Wait []string `yaml:"wait,omitempty"`
}

// Source names the entry's source: "url", "path", "kustomize" or "helm".
// It is empty for an entry with no source, and for one with more than one
// it names the first in that order; [Spec.Check] refuses both.
func (m *Manifest) Source() string {
	switch {
	case m.URL != "":
		return "url"
	case m.Path != "":
		return "path"
	case m.Kustomize != "":
		return "kustomize"
	case m.Helm != nil:
		return "helm"
	}
	return ""
}

// Helm is a chart that the create hook installs with helm install.
type Helm struct {
	// Chart is an OCI reference (oci://...) or a chart repository's
	// chart, as helm pull takes it.
	Chart string `yaml:"chart"`
	// Version is the chart's version.
	Version string `yaml:"version"`
	// Digest is sha256:<64 hexadecimal digits>. For a chart from a chart
	// repository it is the sha256 of the chart's archive.
	Digest string `yaml:"digest"`
	// Release is the release's name.
	Release string `yaml:"release"`
	// Namespace is the release's namespace; empty means helm's default.
	Namespace string `yaml:"namespace,omitempty"`
	// Values are values files, each relative to the spec's directory, in
	// the order helm takes them.
	Values []string `yaml:"values,omitempty"`
}

// Load reads the spec at path and checks it with [Spec.Check]. The
// repository root is the parent of the spec's directory, so that a spec at
// `.flr/kind-cluster.yaml` can name any file in the repository.
func Load(path string) (*Spec, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return LoadFromRoot(abs, filepath.Dir(filepath.Dir(abs)))
}

// LoadFromRoot is [Load] with the repository root given. The spec itself
// has to be inside root.
func LoadFromRoot(path, root string) (*Spec, error) {
	file, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	if _, err := relInside(root, file); err != nil {
		return nil, fmt.Errorf("%s: the spec is not inside the root %s", path, root)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	s, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.file, s.root = file, root
	if err := s.Check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# If a kind cluster spec has a field that `flr-kind` does not know, an
//# `apiVersion` other than `flintlock-runner.dev/v1alpha1`, a `kind` other
//# than `KindCluster` or no valid kind cluster name in `name`, then
//# `flr-kind` SHALL refuse the spec.

// parse decodes one YAML document, refusing fields the Spec does not have
// and a second document after the first.
func parse(data []byte) (*Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Spec
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the spec is empty")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("the spec has more than one YAML document")
	}
	if s.Volumes == "" {
		s.Volumes = DefaultVolumes
	}
	return &s, nil
}

// File is the spec's absolute path.
func (s *Spec) File() string { return s.file }

// Dir is the absolute path of the spec's directory, against which its
// paths are read.
func (s *Spec) Dir() string { return filepath.Dir(s.file) }

// Root is the absolute path of the repository root, which no path in the
// spec can leave.
func (s *Spec) Root() string { return s.root }

// Abs turns a path from the spec into an absolute path, relative to the
// spec's directory. It refuses an absolute path and one that leaves the
// root; [Spec.Check] has refused both already for the spec's own paths.
func (s *Spec) Abs(p string) (string, error) {
	abs, _, err := s.resolve(p)
	return abs, err
}

// KindConfigInline says whether KindConfig is the kind configuration
// itself, which it is when it holds a line break.
func (s *Spec) KindConfigInline() bool { return strings.Contains(s.KindConfig, "\n") }

// KindConfigData returns the kind configuration: KindConfig itself when it
// is inline, the content of the file it names otherwise, and nil when the
// spec has none.
func (s *Spec) KindConfigData() ([]byte, error) {
	switch {
	case s.KindConfig == "":
		return nil, nil
	case s.KindConfigInline():
		return []byte(s.KindConfig), nil
	}
	abs, err := s.Abs(s.KindConfig)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}
