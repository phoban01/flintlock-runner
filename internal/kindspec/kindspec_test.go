package kindspec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	hex64  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	header = "apiVersion: flintlock-runner.dev/v1alpha1\nkind: KindCluster\nname: my-e2e\n"
	// specPath is where the tests put the spec, relative to the root.
	specPath = ".flr/kind-cluster.yaml"
)

// fullSpec uses every field and every source.
var fullSpec = header + `volumes: disk
kindConfig: ../test/e2e/kind-config.yaml
images:
  - ghcr.io/liquidmetal-dev/poolmgrd:0.6.0@sha256:` + hex64 + `
manifests:
  - url: https://example.com/cert-manager.yaml
    sha256: ` + hex64 + `
    wait:
      - deployment/cert-manager -n cert-manager
      - deployment/cert-manager-webhook --namespace cert-manager
  - path: ../deploy/thing.yaml
  - kustomize: ../deps
  - helm:
      chart: oci://ghcr.io/example/charts/thing
      version: 1.4.2
      digest: sha256:` + hex64 + `
      release: thing
      namespace: thing
      values: [../values.yaml]
    wait:
      - deployment/thing --namespace=thing
`

// fullFiles are the files that fullSpec names.
func fullFiles() map[string]string {
	return map[string]string{
		specPath:                    fullSpec,
		"test/e2e/kind-config.yaml": "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n  - role: control-plane\n    image: kindest/node:v1.36.0@sha256:" + hex64 + "\n  - role: worker\n",
		"deploy/thing.yaml":         "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: thing\n",
		"deps/kustomization.yaml":   "resources: [a.yaml, sub/b.yaml]\n",
		"deps/a.yaml":               "a: 1\n",
		"deps/sub/b.yaml":           "b: 2\n",
		"values.yaml":               "replicas: 1\n",
	}
}

// writeTree writes files under a new root and returns the root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// load writes the tree and loads its spec.
func load(t *testing.T, files map[string]string) (*Spec, error) {
	t.Helper()
	return Load(filepath.Join(writeTree(t, files), specPath))
}

// faultFields returns the fields of the faults in err, or the whole error
// when it has no FieldError in it.
func faultFields(err error) []string {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return []string{err.Error()}
	}
	var fields []string
	for _, e := range joined.Unwrap() {
		var fe *FieldError
		if errors.As(e, &fe) {
			fields = append(fields, fe.Field)
		} else {
			fields = append(fields, e.Error())
		}
	}
	return fields
}

type refusal struct {
	name string
	// spec replaces the full spec.
	spec string
	// files are written on top of fullFiles.
	files map[string]string
	// want is the field of the one fault, or a part of the error when the
	// spec does not parse.
	want string
	// msg, when set, is a part of the error that the fault has to have.
	msg string
}

// testRefusals loads each case's spec and checks that Load refuses it with
// one fault, in the field the case names.
func testRefusals(t *testing.T, cases []refusal) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := fullFiles()
			files[specPath] = tc.spec
			for k, v := range tc.files {
				files[k] = v
			}
			_, err := load(t, files)
			if err == nil {
				t.Fatalf("Load accepted the spec; want a fault in %s", tc.want)
			}
			got := faultFields(err)
			if len(got) != 1 || !strings.Contains(got[0], tc.want) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("faults %q (%v); want one in %s", got, err, tc.want)
			}
		})
	}
}

func TestLoadAcceptsGoodSpecs(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
	}{
		{"minimal", header},
		{"full", fullSpec},
		{"tmpfs size", header + "volumes: tmpfs:50%\n"},
		{"tmpfs size in G", header + "volumes: tmpfs:4G\n"},
		{"inline kind config", header + "kindConfig: |\n  kind: Cluster\n  apiVersion: kind.x-k8s.io/v1alpha4\n"},
		{"path manifest", header + "manifests:\n  - path: ../deploy/thing.yaml\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := fullFiles()
			files[specPath] = tc.spec
			if _, err := load(t, files); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

func TestLoadDefaultsVolumesToTmpfs(t *testing.T) {
	files := fullFiles()
	files[specPath] = header
	s, err := load(t, files)
	if err != nil {
		t.Fatal(err)
	}
	if s.Volumes != DefaultVolumes {
		t.Fatalf("Volumes = %q; want %q", s.Volumes, DefaultVolumes)
	}
}

func TestLoadReportsEveryFault(t *testing.T) {
	files := fullFiles()
	files[specPath] = "apiVersion: v1\nkind: KindCluster\nname: Bad_Name\nimages: [nginx:1.27]\nmanifests:\n  - helm:\n      chart: c\n      version: v\n      release: r\n"
	_, err := load(t, files)
	got := faultFields(err)
	want := []string{"apiVersion", "name", "images[0]", "manifests[0].helm.digest"}
	if !slices.Equal(got, want) {
		t.Fatalf("faults %q; want %q", got, want)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# If a kind cluster spec has a field that `flr-kind` does not know, an
//# `apiVersion` other than `flintlock-runner.dev/v1alpha1`, a `kind` other
//# than `KindCluster` or no valid kind cluster name in `name`, then
//# `flr-kind` SHALL refuse the spec.

func TestLoadRefusesBadHeader(t *testing.T) {
	testRefusals(t, []refusal{
		{name: "unknown field", spec: header + "volume: disk\n", want: "field volume not found"},
		{name: "unknown field in a manifest", spec: header + "manifests:\n  - path: ../deploy/thing.yaml\n    sha: x\n", want: "field sha not found"},
		{name: "unknown field in a chart", spec: header + "manifests:\n  - helm:\n      chart: c\n      version: v\n      digest: sha256:" + hex64 + "\n      release: r\n      valuesFiles: []\n", want: "field valuesFiles not found"},
		{name: "two documents", spec: header + "---\n" + header, want: "more than one YAML document"},
		{name: "empty", spec: "", want: "empty"},
		{name: "apiVersion", spec: "apiVersion: flintlock-runner.dev/v1beta1\nkind: KindCluster\nname: a\n", want: "apiVersion"},
		{name: "no apiVersion", spec: "kind: KindCluster\nname: a\n", want: "apiVersion"},
		{name: "kind", spec: "apiVersion: flintlock-runner.dev/v1alpha1\nkind: Cluster\nname: a\n", want: "kind"},
		{name: "no name", spec: "apiVersion: flintlock-runner.dev/v1alpha1\nkind: KindCluster\n", want: "name"},
		{name: "upper case name", spec: "apiVersion: flintlock-runner.dev/v1alpha1\nkind: KindCluster\nname: My-E2E\n", want: "name"},
		{name: "long name", spec: "apiVersion: flintlock-runner.dev/v1alpha1\nkind: KindCluster\nname: " + strings.Repeat("a", 51) + "\n", want: "name"},
	})
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# If an image that a kind cluster spec names under `images`, or as a
//# node's `image` in its kind configuration, is not named by a `sha256`
//# digest, then `flr-kind` SHALL refuse the spec.

func TestLoadRefusesImagesNotByDigest(t *testing.T) {
	testRefusals(t, []refusal{
		{name: "tag", spec: header + "images: [ghcr.io/x/y:1.0]\n", want: "images[0]"},
		{name: "short digest", spec: header + "images: [ghcr.io/x/y@sha256:abc]\n", want: "images[0]"},
		{name: "other algorithm", spec: header + "images: [ghcr.io/x/y@sha512:" + hex64 + hex64 + "]\n", want: "images[0]"},
		{name: "second image", spec: header + "images: [ghcr.io/x/y@sha256:" + hex64 + ", ghcr.io/x/z:latest]\n", want: "images[1]"},
		{
			name:  "node image in a kind config file",
			spec:  header + "kindConfig: ../test/e2e/kind-config.yaml\n",
			files: map[string]string{"test/e2e/kind-config.yaml": "kind: Cluster\nnodes:\n  - role: control-plane\n    image: kindest/node:v1.36.0@sha256:" + hex64 + "\n  - role: worker\n    image: kindest/node:v1.36.0\n"},
			want:  "kindConfig.nodes[1].image",
		},
		{
			name: "node image in an inline kind config",
			spec: header + "kindConfig: |\n  kind: Cluster\n  nodes:\n    - role: control-plane\n      image: kindest/node:v1.36.0\n",
			want: "kindConfig.nodes[0].image",
		},
	})
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# If an entry under `manifests` has a `url` and no `sha256` of 64
//# hexadecimal digits, then `flr-kind` SHALL refuse the spec.

func TestLoadRefusesURLWithoutSHA256(t *testing.T) {
	m := header + "manifests:\n  - path: ../deploy/thing.yaml\n  - url: https://example.com/m.yaml\n"
	testRefusals(t, []refusal{
		{name: "no sha256", spec: m, want: "manifests[1].sha256"},
		{name: "short sha256", spec: m + "    sha256: abc\n", want: "manifests[1].sha256"},
		{name: "upper case sha256", spec: m + "    sha256: " + strings.ToUpper(hex64) + "\n", want: "manifests[1].sha256"},
		{name: "prefixed sha256", spec: m + "    sha256: sha256:" + hex64 + "\n", want: "manifests[1].sha256"},
		{name: "not a URL", spec: header + "manifests:\n  - url: example.com/m.yaml\n    sha256: " + hex64 + "\n", want: "manifests[0].url"},
		{name: "sha256 without url", spec: header + "manifests:\n  - path: ../deploy/thing.yaml\n    sha256: " + hex64 + "\n", want: "manifests[0].sha256"},
	})
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# If a `helm` entry under `manifests` has no `chart`, `version` or
//# `release`, or no `digest` of the form `sha256:` and 64 hexadecimal
//# digits, then `flr-kind` SHALL refuse the spec.

func TestLoadRefusesIncompleteChart(t *testing.T) {
	chart := func(lines ...string) string {
		return header + "manifests:\n  - path: ../deploy/thing.yaml\n  - path: ../deploy/thing.yaml\n  - helm:\n" + "      " + strings.Join(lines, "\n      ") + "\n"
	}
	const (
		c = "chart: oci://ghcr.io/example/charts/thing"
		v = "version: 1.4.2"
		r = "release: thing"
	)
	d := "digest: sha256:" + hex64
	testRefusals(t, []refusal{
		{name: "no chart", spec: chart(v, d, r), want: "manifests[2].helm.chart"},
		{name: "no version", spec: chart(c, d, r), want: "manifests[2].helm.version"},
		{name: "no release", spec: chart(c, v, d), want: "manifests[2].helm.release"},
		{name: "no digest", spec: chart(c, v, r), want: "manifests[2].helm.digest"},
		{name: "bare digest", spec: chart(c, v, r, "digest: "+hex64), want: "manifests[2].helm.digest"},
		{name: "bad release", spec: chart(c, v, d, "release: Thing_1"), want: "manifests[2].helm.release"},
		{name: "bad namespace", spec: chart(c, v, d, r, "namespace: Thing"), want: "manifests[2].helm.namespace"},
		{name: "missing values file", spec: chart(c, v, d, r, "values: [../nope.yaml]"), want: "manifests[2].helm.values[0]"},
	})
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# If an entry under `manifests` has no source, or more than one of `url`,
//# `path`, `kustomize` and `helm`, then `flr-kind` SHALL refuse the spec.

func TestLoadRefusesManifestWithoutOneSource(t *testing.T) {
	testRefusals(t, []refusal{
		{name: "no source", spec: header + "manifests:\n  - wait: [deployment/a]\n", want: "manifests[0]"},
		{name: "path and kustomize", spec: header + "manifests:\n  - path: ../deploy/thing.yaml\n    kustomize: ../deps\n", want: "manifests[0]"},
		{name: "path and helm", spec: header + "manifests:\n  - path: ../deploy/thing.yaml\n  - path: ../deploy/thing.yaml\n    helm:\n      chart: c\n      version: v\n      digest: sha256:" + hex64 + "\n      release: r\n", want: "manifests[1]"},
		{name: "url and path", spec: header + "manifests:\n  - url: https://example.com/m.yaml\n    sha256: " + hex64 + "\n    path: ../deploy/thing.yaml\n", want: "manifests[0]"},
	})
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# If a kind cluster spec sets `volumes` to a value other than `tmpfs`,
//# `tmpfs:SIZE` or `disk`, then `flr-kind` SHALL refuse the spec.

func TestLoadRefusesBadVolumes(t *testing.T) {
	var cases []refusal
	for _, v := range []string{"ram", "Disk", "tmpfs:", "tmpfs:0", "tmpfs:04G", "tmpfs:4T", "tmpfs:4GB", "tmpfs:-1"} {
		cases = append(cases, refusal{name: v, spec: header + "volumes: '" + v + "'\n", want: "volumes"})
	}
	testRefusals(t, cases)
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# `flr-kind` SHALL read each path that a kind cluster spec names relative
//# to the directory that holds the spec.

func TestLoadReadsPathsFromTheSpecDirectory(t *testing.T) {
	files := fullFiles()
	files[".flr/local.yaml"] = "local: true\n"
	files[specPath] = header + "kindConfig: ../test/e2e/kind-config.yaml\nmanifests:\n  - path: local.yaml\n"
	s, err := load(t, files)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	data, err := s.KindConfigData()
	if err != nil || string(data) != files["test/e2e/kind-config.yaml"] {
		t.Fatalf("KindConfigData = %q, %v; want test/e2e/kind-config.yaml", data, err)
	}
	abs, err := s.Abs("local.yaml")
	if err != nil || abs != filepath.Join(s.Root(), ".flr", "local.yaml") {
		t.Fatalf("Abs(local.yaml) = %q, %v", abs, err)
	}

	// A path relative to the root, not to the spec's directory, is not
	// found.
	testRefusals(t, []refusal{
		{name: "path from the root", spec: header + "manifests:\n  - path: deploy/thing.yaml\n", want: "manifests[0].path"},
	})
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# If a path that a kind cluster spec names is absolute, or leads out of
//# the repository root, then `flr-kind` SHALL refuse the spec.

func TestLoadRefusesPathsOutOfTheRoot(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "m.yaml"), []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRefusals(t, []refusal{
		{name: "absolute", spec: header + "manifests:\n  - path: " + filepath.Join(outside, "m.yaml") + "\n", want: "manifests[0].path", msg: "is absolute"},
		{name: "out of the root", spec: header + "manifests:\n  - path: ../../m.yaml\n", want: "manifests[0].path", msg: "leads out of the repository root"},
		{name: "kind config out of the root", spec: header + "kindConfig: ../../kind.yaml\n", want: "kindConfig", msg: "leads out of the repository root"},
		{name: "kustomize out of the root", spec: header + "manifests:\n  - kustomize: ../..\n", want: "manifests[0].kustomize", msg: "leads out of the repository root"},
		{name: "values out of the root", spec: header + "manifests:\n  - helm:\n      chart: c\n      version: v\n      digest: sha256:" + hex64 + "\n      release: r\n      values: [../../v.yaml]\n", want: "manifests[0].helm.values[0]", msg: "leads out of the repository root"},
	})

	for _, tc := range []struct {
		name, link, spec, want string
	}{
		{"symbolic link out of the root", "deploy/out.yaml", header + "manifests:\n  - path: ../deploy/out.yaml\n", "manifests[0].path"},
		{"symbolic link out of the root in a kustomize directory", "deps/out.yaml", header + "manifests:\n  - kustomize: ../deps\n", "manifests[0].kustomize"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := fullFiles()
			files[specPath] = tc.spec
			root := writeTree(t, files)
			if err := os.Symlink(filepath.Join(outside, "m.yaml"), filepath.Join(root, tc.link)); err != nil {
				t.Fatal(err)
			}
			_, err := Load(filepath.Join(root, specPath))
			if got := faultFields(err); err == nil || len(got) != 1 || got[0] != tc.want || !strings.Contains(err.Error(), "a symbolic link leads out") {
				t.Fatalf("faults %q (%v); want one in %s", got, err, tc.want)
			}
		})
	}

	t.Run("spec out of the root", func(t *testing.T) {
		root := writeTree(t, fullFiles())
		if _, err := LoadFromRoot(filepath.Join(root, specPath), filepath.Join(root, "deps")); err == nil {
			t.Fatal("LoadFromRoot accepted a spec outside its root")
		}
	})
	t.Run("a narrower root", func(t *testing.T) {
		root := writeTree(t, fullFiles())
		_, err := LoadFromRoot(filepath.Join(root, specPath), filepath.Join(root, ".flr"))
		if !errors.Is(err, ErrNotInRoot) {
			t.Fatalf("LoadFromRoot with the spec's directory as root: %v; want ErrNotInRoot", err)
		}
	})
}

func TestLoadRefusesKustomizeProblems(t *testing.T) {
	testRefusals(t, []refusal{
		{name: "no kustomization", spec: header + "manifests:\n  - kustomize: ../deploy\n", want: "manifests[0].kustomize"},
		{name: "a file", spec: header + "manifests:\n  - kustomize: ../deploy/thing.yaml\n", want: "manifests[0].kustomize"},
		{name: "path is a directory", spec: header + "manifests:\n  - path: ../deploy\n", want: "manifests[0].path"},
	})
	t.Run("symbolic link to a directory", func(t *testing.T) {
		files := fullFiles()
		root := writeTree(t, files)
		if err := os.Symlink(filepath.Join(root, "deploy"), filepath.Join(root, "deps", "linked")); err != nil {
			t.Fatal(err)
		}
		_, err := Load(filepath.Join(root, specPath))
		if got := faultFields(err); err == nil || len(got) != 1 || got[0] != "manifests[2].kustomize" {
			t.Fatalf("faults %q (%v); want one in manifests[2].kustomize", got, err)
		}
	})
}

func TestLoadRefusesBadWait(t *testing.T) {
	var cases []refusal
	for _, w := range []string{"cert-manager", "/a", "deployment/", "deployment/a -n", "deployment/a -n X", "deployment/a --namespace=", "deployment/a b"} {
		cases = append(cases, refusal{name: w, spec: header + "manifests:\n  - path: ../deploy/thing.yaml\n    wait: ['" + w + "']\n", want: "manifests[0].wait[0]"})
	}
	testRefusals(t, cases)
}

func TestParseWait(t *testing.T) {
	for in, want := range map[string]Wait{
		"deployment/a":                     {Resource: "deployment/a"},
		"deployment/a -n ns":               {Resource: "deployment/a", Namespace: "ns"},
		"deployment.apps/a --namespace ns": {Resource: "deployment.apps/a", Namespace: "ns"},
		"  pod/a   --namespace=ns ":        {Resource: "pod/a", Namespace: "ns"},
	} {
		got, err := ParseWait(in)
		if err != nil || got != want {
			t.Errorf("ParseWait(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# `flr-kind` SHALL take a `kindConfig` that holds a line break as the kind
//# configuration itself, and any other `kindConfig` as the path of a file
//# that holds it.

func TestKindConfigInlineOrPath(t *testing.T) {
	inline := "kind: Cluster\nnodes:\n  - role: control-plane\n"
	files := fullFiles()
	files[specPath] = header + "kindConfig: |\n  kind: Cluster\n  nodes:\n    - role: control-plane\n"
	s, err := load(t, files)
	if err != nil {
		t.Fatal(err)
	}
	if !s.KindConfigInline() {
		t.Fatal("KindConfigInline = false for a block scalar")
	}
	if data, err := s.KindConfigData(); err != nil || string(data) != inline {
		t.Fatalf("KindConfigData = %q, %v; want %q", data, err, inline)
	}
	digestFiles, err := s.Files()
	if err != nil || !slices.Equal(digestFiles, []string{specPath}) {
		t.Fatalf("Files = %q, %v; want the spec alone", digestFiles, err)
	}

	files[specPath] = header + "kindConfig: ../test/e2e/kind-config.yaml\n"
	if s, err = load(t, files); err != nil {
		t.Fatal(err)
	}
	if s.KindConfigInline() {
		t.Fatal("KindConfigInline = true for a path")
	}
	if data, err := s.KindConfigData(); err != nil || string(data) != files["test/e2e/kind-config.yaml"] {
		t.Fatalf("KindConfigData = %q, %v; want the file's content", data, err)
	}

	// A one-line value is a path, even when it looks like YAML.
	testRefusals(t, []refusal{
		{name: "one line", spec: header + "kindConfig: 'kind: Cluster'\n", want: "kindConfig"},
	})
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# `flr-kind digest` SHALL print `sha256:` and the sha256 of a list with one
//# line for the spec and for each file that the spec names by path, the
//# files under its kustomize directories included, where each line holds
//# the file's sha256 and its path relative to the repository root, and the
//# lines are in the byte order of the paths.

func TestDigest(t *testing.T) {
	digest := func(t *testing.T, files map[string]string) string {
		t.Helper()
		s, err := load(t, files)
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	base := digest(t, fullFiles())

	t.Run("format and content", func(t *testing.T) {
		files := fullFiles()
		paths := []string{
			".flr/kind-cluster.yaml",
			"deploy/thing.yaml",
			"deps/a.yaml",
			"deps/kustomization.yaml",
			"deps/sub/b.yaml",
			"test/e2e/kind-config.yaml",
			"values.yaml",
		}
		var list strings.Builder
		for _, p := range paths {
			sum := sha256.Sum256([]byte(files[p]))
			fmt.Fprintf(&list, "%s  %s\n", hex.EncodeToString(sum[:]), p)
		}
		sum := sha256.Sum256([]byte(list.String()))
		if want := "sha256:" + hex.EncodeToString(sum[:]); base != want {
			t.Fatalf("Digest = %s; want %s, the sha256 of\n%s", base, want, list.String())
		}
	})

	t.Run("the same in another directory", func(t *testing.T) {
		if d := digest(t, fullFiles()); d != base {
			t.Fatalf("Digest = %s in a second directory; want %s", d, base)
		}
	})

	t.Run("the same with -root as the default", func(t *testing.T) {
		root := writeTree(t, fullFiles())
		s, err := LoadFromRoot(filepath.Join(root, specPath), root)
		if err != nil {
			t.Fatal(err)
		}
		if d, err := s.Digest(); err != nil || d != base {
			t.Fatalf("Digest = %s, %v; want %s", d, err, base)
		}
	})

	changes := map[string]func(map[string]string){
		"the spec":             func(f map[string]string) { f[specPath] += "# a comment\n" },
		"the kind config":      func(f map[string]string) { f["test/e2e/kind-config.yaml"] += "# a comment\n" },
		"a manifest path":      func(f map[string]string) { f["deploy/thing.yaml"] += "# a comment\n" },
		"the kustomization":    func(f map[string]string) { f["deps/kustomization.yaml"] += "# a comment\n" },
		"a kustomize file":     func(f map[string]string) { f["deps/sub/b.yaml"] = "b: 3\n" },
		"a new kustomize file": func(f map[string]string) { f["deps/README"] = "notes\n" },
		"a values file":        func(f map[string]string) { f["values.yaml"] = "replicas: 2\n" },
		"a kustomize file's name": func(f map[string]string) {
			f["deps/c.yaml"] = f["deps/a.yaml"]
			delete(f, "deps/a.yaml")
		},
		"the spec's name": func(f map[string]string) {
			f[".flr/other.yaml"] = f[specPath]
			delete(f, specPath)
		},
	}
	for name, change := range changes {
		t.Run("changes with "+name, func(t *testing.T) {
			files := fullFiles()
			change(files)
			root := writeTree(t, files)
			p := filepath.Join(root, specPath)
			if _, ok := files[specPath]; !ok {
				p = filepath.Join(root, ".flr", "other.yaml")
			}
			s, err := Load(p)
			if err != nil {
				t.Fatal(err)
			}
			d, err := s.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if d == base {
				t.Fatalf("Digest = %s, the same as before the change", d)
			}
		})
	}

	t.Run("does not change with a file the spec does not name", func(t *testing.T) {
		files := fullFiles()
		files["deploy/other.yaml"] = "x: 1\n"
		if d := digest(t, files); d != base {
			t.Fatalf("Digest = %s; want %s", d, base)
		}
	})
}
