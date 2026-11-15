package kindspec

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldError is one fault that [Spec.Check] finds, with the field it is in,
// for example `manifests[2].helm.digest`. Err is the error behind the fault,
// such as [ErrNotInRoot] or a [fs.PathError], when there is one.
type FieldError struct {
	Field   string
	Message string
	Err     error
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

// Unwrap returns Err.
func (e *FieldError) Unwrap() error { return e.Err }

var (
	// kindName is what kind takes as a cluster name, and kindNameMax its
	// longest: kind adds "-control-plane" to it for a host name of at most
	// 64 characters.
	kindName    = regexp.MustCompile(`^[a-z0-9.-]+$`)
	kindNameMax = 50
	// tmpfsSize is what flr-kind-cluster takes as SIZE in tmpfs:SIZE
	// (guest/rootfs-kind/kind-cluster.sh).
	tmpfsSize = regexp.MustCompile(`^[1-9][0-9]*[kKmMgG%]?$`)
	// byDigest is an image reference that ends in a sha256 digest.
	byDigest  = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// chartDigest is a chart's digest.
	chartDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// releaseName is what helm takes as a release name, at most 53
	// characters.
	releaseName    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	releaseNameMax = 53
	// dnsLabel is a Kubernetes namespace name, at most 63 characters.
	dnsLabel    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsLabelMax = 63
)

// Check checks the spec and returns every fault it finds, each a
// *[FieldError], joined with [errors.Join]. It reads the files that the
// spec names, so it needs the spec from [Load] or [LoadFromRoot].
func (s *Spec) Check() error {
	c := &checker{s: s}
	c.header()
	c.volumes()
	c.kindConfig()
	c.images()
	for i := range s.Manifests {
		c.manifest(i, &s.Manifests[i])
	}
	return errors.Join(c.errs...)
}

type checker struct {
	s    *Spec
	errs []error
}

func (c *checker) fail(field, format string, args ...any) {
	c.errs = append(c.errs, &FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (c *checker) failErr(field string, err error) {
	c.errs = append(c.errs, &FieldError{Field: field, Message: err.Error(), Err: err})
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# If a kind cluster spec has a field that `flr-kind` does not know, an
//# `apiVersion` other than `flintlock-runner.dev/v1alpha1`, a `kind` other
//# than `KindCluster` or no valid kind cluster name in `name`, then
//# `flr-kind` SHALL refuse the spec.

func (c *checker) header() {
	s := c.s
	if s.APIVersion != APIVersion {
		c.fail("apiVersion", "is %q, not %s", s.APIVersion, APIVersion)
	}
	if s.Kind != Kind {
		c.fail("kind", "is %q, not %s", s.Kind, Kind)
	}
	switch {
	case s.Name == "":
		c.fail("name", "is required")
	case len(s.Name) > kindNameMax:
		c.fail("name", "has %d characters; a kind cluster name has at most %d", len(s.Name), kindNameMax)
	case !kindName.MatchString(s.Name):
		c.fail("name", "%q is not a kind cluster name, which matches %s", s.Name, kindName)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# If a kind cluster spec sets `volumes` to a value other than `tmpfs`,
//# `tmpfs:SIZE` or `disk`, then `flr-kind` SHALL refuse the spec.

func (c *checker) volumes() {
	v := c.s.Volumes
	switch {
	case v == "tmpfs" || v == "disk":
	case strings.HasPrefix(v, "tmpfs:"):
		if !tmpfsSize.MatchString(strings.TrimPrefix(v, "tmpfs:")) {
			c.fail("volumes", "%q: SIZE is not a tmpfs size such as 50%% or 4G", v)
		}
	default:
		c.fail("volumes", "%q is not tmpfs, tmpfs:SIZE or disk", v)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# `flr-kind` SHALL take a `kindConfig` that holds a line break as the kind
//# configuration itself, and any other `kindConfig` as the path of a file
//# that holds it.

func (c *checker) kindConfig() {
	s := c.s
	if s.KindConfig == "" {
		return
	}
	if !s.KindConfigInline() && !c.file("kindConfig", s.KindConfig) {
		return
	}
	data, err := s.KindConfigData()
	if err != nil {
		c.failErr("kindConfig", err)
		return
	}
	var cfg struct {
		Nodes []struct {
			Image string `yaml:"image"`
		} `yaml:"nodes"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		c.fail("kindConfig", "is not YAML: %v", err)
		return
	}
	for i, n := range cfg.Nodes {
		if n.Image != "" {
			c.image(fmt.Sprintf("kindConfig.nodes[%d].image", i), n.Image)
		}
	}
}

func (c *checker) images() {
	for i, img := range c.s.Images {
		c.image(fmt.Sprintf("images[%d]", i), img)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# If an image that a kind cluster spec names under `images`, or as a
//# node's `image` in its kind configuration, is not named by a `sha256`
//# digest, then `flr-kind` SHALL refuse the spec.

func (c *checker) image(field, ref string) {
	if !byDigest.MatchString(ref) {
		c.fail(field, "%q is not named by digest (NAME@sha256:<64 hexadecimal digits>)", ref)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# If an entry under `manifests` has no source, or more than one of `url`,
//# `path`, `kustomize` and `helm`, then `flr-kind` SHALL refuse the spec.

func (c *checker) manifest(i int, m *Manifest) {
	field := fmt.Sprintf("manifests[%d]", i)
	var sources []string
	for _, src := range []struct {
		name string
		set  bool
	}{
		{"url", m.URL != ""},
		{"path", m.Path != ""},
		{"kustomize", m.Kustomize != ""},
		{"helm", m.Helm != nil},
	} {
		if src.set {
			sources = append(sources, src.name)
		}
	}
	switch len(sources) {
	case 0:
		c.fail(field, "has no source; give one of url, path, kustomize or helm")
	case 1:
	default:
		c.fail(field, "has %d sources (%s); give only one", len(sources), strings.Join(sources, ", "))
	}

	c.url(field, m)
	if m.Path != "" {
		c.file(field+".path", m.Path)
	}
	if m.Kustomize != "" {
		if abs, ok := c.dir(field+".kustomize", m.Kustomize); ok {
			c.kustomization(field+".kustomize", abs)
		}
	}
	if m.Helm != nil {
		c.helm(field+".helm", m.Helm)
	}
	for j, w := range m.Wait {
		if _, err := ParseWait(w); err != nil {
			c.failErr(fmt.Sprintf("%s.wait[%d]", field, j), err)
		}
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# If an entry under `manifests` has a `url` and no `sha256` of 64
//# hexadecimal digits, then `flr-kind` SHALL refuse the spec.

func (c *checker) url(field string, m *Manifest) {
	if m.URL == "" {
		if m.SHA256 != "" {
			c.fail(field+".sha256", "is set, but only a url takes a sha256")
		}
		return
	}
	if u, err := url.Parse(m.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		c.fail(field+".url", "%q is not an http or https URL", m.URL)
	}
	switch {
	case m.SHA256 == "":
		c.fail(field+".sha256", "is required with a url")
	case !sha256Hex.MatchString(m.SHA256):
		c.fail(field+".sha256", "%q is not 64 lowercase hexadecimal digits", m.SHA256)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# If a `helm` entry under `manifests` has no `chart`, `version` or
//# `release`, or no `digest` of the form `sha256:` and 64 hexadecimal
//# digits, then `flr-kind` SHALL refuse the spec.

func (c *checker) helm(field string, h *Helm) {
	if h.Chart == "" {
		c.fail(field+".chart", "is required")
	}
	if h.Version == "" {
		c.fail(field+".version", "is required")
	}
	switch {
	case h.Digest == "":
		c.fail(field+".digest", "is required")
	case !chartDigest.MatchString(h.Digest):
		c.fail(field+".digest", "%q is not sha256:<64 lowercase hexadecimal digits>", h.Digest)
	}
	switch {
	case h.Release == "":
		c.fail(field+".release", "is required")
	case len(h.Release) > releaseNameMax || !releaseName.MatchString(h.Release):
		c.fail(field+".release", "%q is not a Helm release name", h.Release)
	}
	if h.Namespace != "" && (len(h.Namespace) > dnsLabelMax || !dnsLabel.MatchString(h.Namespace)) {
		c.fail(field+".namespace", "%q is not a namespace name", h.Namespace)
	}
	for j, v := range h.Values {
		c.file(fmt.Sprintf("%s.values[%d]", field, j), v)
	}
}

// file checks that p names a regular file and says whether it does.
func (c *checker) file(field, p string) bool {
	abs, _, err := c.s.resolve(p)
	if err != nil {
		c.failErr(field, err)
		return false
	}
	fi, err := os.Stat(abs)
	if err != nil {
		c.failErr(field, err)
		return false
	}
	if !fi.Mode().IsRegular() {
		c.fail(field, "%s is not a regular file", p)
		return false
	}
	return true
}

// dir checks that p names a directory and returns its absolute path.
func (c *checker) dir(field, p string) (string, bool) {
	abs, _, err := c.s.resolve(p)
	if err != nil {
		c.failErr(field, err)
		return "", false
	}
	fi, err := os.Stat(abs)
	if err != nil {
		c.failErr(field, err)
		return "", false
	}
	if !fi.IsDir() {
		c.fail(field, "%s is not a directory", p)
		return "", false
	}
	return abs, true
}

// kustomization checks that a kustomize directory has a kustomization file
// and that its files can go into the digest.
func (c *checker) kustomization(field, abs string) {
	found := false
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if fi, err := os.Stat(filepath.Join(abs, name)); err == nil && fi.Mode().IsRegular() {
			found = true
			break
		}
	}
	if !found {
		c.fail(field, "has no kustomization.yaml, kustomization.yml or Kustomization")
	}
	if _, err := c.s.walk(abs); err != nil {
		c.failErr(field, err)
	}
}

// ErrNotInRoot is the error for a path that is absolute or that leaves the
// repository root.
var ErrNotInRoot = errors.New("leads out of the repository root")

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# `flr-kind` SHALL read each path that a kind cluster spec names relative
//# to the directory that holds the spec.

// resolve turns a path from the spec into an absolute path and a path
// relative to the root, with forward slashes. It refuses a path that leaves
// the root, either as written or through a symbolic link, and a path whose
// name would break the digest's lines.
func (s *Spec) resolve(p string) (abs, rel string, err error) {
	if p == "" {
		return "", "", errors.New("is empty")
	}
	if strings.ContainsAny(p, "\n\\") {
		return "", "", fmt.Errorf("%q has a line break or a backslash", p)
	}

	//= docs/requirements/13-guest-image.md#guest-kind-spec
	//# If a path that a kind cluster spec names is absolute, or leads out of
	//# the repository root, then `flr-kind` SHALL refuse the spec.
	if filepath.IsAbs(p) {
		return "", "", fmt.Errorf("%s is absolute; give a path relative to the spec's directory", p)
	}
	abs = filepath.Join(s.Dir(), p)
	if rel, err = relInside(s.root, abs); err != nil {
		return "", "", fmt.Errorf("%s %w %s", p, ErrNotInRoot, s.root)
	}
	if err := s.inRootResolved(abs); err != nil {
		return "", "", fmt.Errorf("%s: %w", p, err)
	}
	return abs, rel, nil
}

// inRootResolved checks that abs, with every symbolic link followed, is
// still inside the root. A path that does not exist is left for the caller's
// stat to report.
func (s *Spec) inRootResolved(abs string) error {
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return err
	}
	if _, err := relInside(realRoot, real); err != nil {
		return fmt.Errorf("a symbolic link %w %s", ErrNotInRoot, s.root)
	}
	return nil
}

// relInside returns target relative to root, with forward slashes, or an
// error when target is not inside root.
func relInside(root, target string) (string, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", ErrNotInRoot
	}
	return filepath.ToSlash(rel), nil
}

// Wait is one entry of a manifest's wait list.
type Wait struct {
	// Resource is KIND/NAME, as kubectl takes it.
	Resource string
	// Namespace is empty for the default namespace.
	Namespace string
}

// ParseWait reads a wait entry: `KIND/NAME`, then optionally `-n NAMESPACE`,
// `--namespace NAMESPACE` or `--namespace=NAMESPACE`.
func ParseWait(s string) (Wait, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return Wait{}, errors.New("is empty")
	}
	var w Wait
	kind, name, ok := strings.Cut(f[0], "/")
	if !ok || kind == "" || name == "" || strings.Contains(name, "/") {
		return Wait{}, fmt.Errorf("%q does not start with KIND/NAME", s)
	}
	w.Resource = f[0]
	switch rest := f[1:]; {
	case len(rest) == 0:
	case len(rest) == 2 && (rest[0] == "-n" || rest[0] == "--namespace"):
		w.Namespace = rest[1]
	case len(rest) == 1 && strings.HasPrefix(rest[0], "--namespace="):
		w.Namespace = strings.TrimPrefix(rest[0], "--namespace=")
	default:
		return Wait{}, fmt.Errorf("%q is not KIND/NAME [-n NAMESPACE]", s)
	}
	if len(f) > 1 && (len(w.Namespace) > dnsLabelMax || !dnsLabel.MatchString(w.Namespace)) {
		return Wait{}, fmt.Errorf("%q: %q is not a namespace name", s, w.Namespace)
	}
	return w, nil
}
