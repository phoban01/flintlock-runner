package kindbake

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/kindspec"
)

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hexC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	// The kind image's node image, and two others.
	baseNode  = "kindest/node:v1.37.0@sha256:" + hexA
	otherNode = "kindest/node:v1.36.0@sha256:" + hexB
	thirdNode = "kindest/node:v1.35.0@sha256:" + hexC

	poolmgrd = "ghcr.io/liquidmetal-dev/poolmgrd:0.6.0@sha256:" + hexA
	certURL  = "https://example.com/cert-manager.yaml"
	chartOCI = "oci://ghcr.io/example/charts/thing"
)

// certManager is the manifest at certURL: a Deployment with a container
// and an init container.
const certManager = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: cert-manager
spec:
  template:
    spec:
      initContainers:
        - name: init
          image: busybox:1.37
      containers:
        - name: controller
          image: quay.io/jetstack/cert-manager-controller:v1.20.2
`

const thing = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: thing\n"

// kustomized is what fakeKustomize renders for deps/: a CronJob, whose Pod
// spec is two levels down, and poolmgrd by the tag that images names.
const kustomized = `apiVersion: batch/v1
kind: CronJob
metadata:
  name: tick
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: tick
              image: ghcr.io/liquidmetal-dev/poolmgrd:0.6.0
`

// charted is what fakeHelm renders: a Pod with an ephemeral container, and
// busybox again under its full name.
const charted = `apiVersion: v1
kind: Pod
metadata:
  name: thing
spec:
  containers:
    - name: thing
      image: ghcr.io/example/thing:1.4.2@sha256:` + hexC + `
  ephemeralContainers:
    - name: debug
      image: docker.io/library/busybox:1.37
`

var chartArchive = []byte("a chart's archive")

func specText(chartDigest string) string {
	return `apiVersion: flintlock-runner.dev/v1alpha1
kind: KindCluster
name: my-e2e
volumes: disk
kindConfig: ../test/e2e/kind-config.yaml
images:
  - ` + poolmgrd + `
manifests:
  - url: ` + certURL + `
    sha256: ` + sum(certManager) + `
    wait:
      - deployment/cert-manager -n cert-manager
  - path: ../deploy/thing.yaml
  - kustomize: ../deps
  - helm:
      chart: ` + chartOCI + `
      version: 1.4.2
      digest: ` + chartDigest + `
      release: thing
      namespace: thing
      values: [../values.yaml, ../more-values.yaml]
    wait:
      - deployment/thing -n thing
`
}

const kindConfig = "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n  - role: control-plane\n    image: " + baseNode + "\n  - role: worker\n    image: " + otherNode + "\n  - role: worker\n    image: " + thirdNode + "\n  - role: worker\n    image: " + otherNode + "\n"

// fixture is a repository with a spec, a kind image root and fakes.
type fixture struct {
	repo, base, out string
	spec            *kindspec.Spec
	registry        *fakeRegistry
	helm            *fakeHelm
	store           *fakeDockerStore
	tools           Tools
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newFixture(t *testing.T, spec string) *fixture {
	t.Helper()
	f := &fixture{repo: t.TempDir(), base: t.TempDir()}
	f.out = filepath.Join(t.TempDir(), "out")
	writeFiles(t, f.repo, map[string]string{
		".flr/kind-cluster.yaml":    spec,
		"test/e2e/kind-config.yaml": kindConfig,
		"deploy/thing.yaml":         thing,
		"deps/kustomization.yaml":   "resources: [rendered.yaml]\n",
		"deps/rendered.yaml":        kustomized,
		"values.yaml":               "replicas: 1\n",
		"more-values.yaml":          "replicas: 2\n",
	})
	// The kind image: its bootstrap file, and its node image in Docker
	// Engine's image store with one layer, "ubuntu".
	writeFiles(t, f.base, map[string]string{
		kindBootstrap: "# comment\nexport KIND_CLUSTER_NAME=flr\nexport KIND_NODE_IMAGE=" + baseNode + "\n",
	})
	if err := writeStore(filepath.Join(f.base, DockerRoot), "docker.io/"+baseNode, []string{"ubuntu"}); err != nil {
		t.Fatal(err)
	}
	s, err := kindspec.Load(filepath.Join(f.repo, ".flr", "kind-cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f.spec = s
	f.registry = &fakeRegistry{manifests: map[string][]byte{
		"docker.io/library/busybox:1.37":                   []byte("busybox index"),
		"quay.io/jetstack/cert-manager-controller:v1.20.2": []byte("controller index"),
		"ghcr.io/example/charts/thing:1.4.2":               ociManifest("sha256:" + sum(string(chartArchive))),
	}}
	f.helm = &fakeHelm{archive: chartArchive, rendered: charted}
	f.store = &fakeDockerStore{layers: map[string][]string{
		"docker.io/" + otherNode: {"ubuntu", "k8s-1.36"},
		"docker.io/" + thirdNode: {"ubuntu", "k8s-1.36", "patch"},
	}}
	f.tools = Tools{
		Registry:    f.registry,
		Fetcher:     fakeFetcher{certURL: []byte(certManager)},
		Kustomize:   fakeKustomize{},
		Helm:        f.helm,
		DockerStore: f.store,
	}
	return f
}

func ociManifest(layer string) []byte {
	return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:` + hexA + `","size":1},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"` + layer + `","size":17}]}`)
}

func (f *fixture) bake(t *testing.T) (*Result, error) {
	t.Helper()
	return Bake(context.Background(), f.spec, f.out, f.tools, Options{Arch: "arm64", Base: f.base})
}

func (f *fixture) mustBake(t *testing.T) *Result {
	t.Helper()
	res, err := f.bake(t)
	if err != nil {
		t.Fatalf("Bake: %v", err)
	}
	return res
}

func (f *fixture) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.out, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func goodSpec() string { return specText("sha256:" + sum(string(chartArchive))) }

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake SPEC OUT` SHALL write to `OUT/etc/flr/kind-cluster.yaml` a
//# kind cluster spec that has its kind configuration inline and that names,
//# by a path relative to `/etc/flr`, the file under `OUT/var/lib/flr` that
//# it wrote for each manifest, chart and values file.

func TestBakedSpecStandsAlone(t *testing.T) {
	f := newFixture(t, goodSpec())
	f.mustBake(t)

	// OUT stands in for /: the baked spec loads with OUT as its root, and
	// each of its paths leads to a file the bake wrote.
	baked, err := kindspec.LoadFromRoot(filepath.Join(f.out, SpecFile), f.out)
	if err != nil {
		t.Fatalf("the baked spec does not load: %v\n%s", err, f.read(t, SpecFile))
	}
	if !baked.KindConfigInline() || baked.KindConfig != kindConfig {
		t.Errorf("kindConfig is %q, want the configuration inline", baked.KindConfig)
	}
	if baked.Name != "my-e2e" || baked.Volumes != "disk" || !slices.Equal(baked.Images, []string{poolmgrd}) {
		t.Errorf("name, volumes or images changed: %q %q %q", baked.Name, baked.Volumes, baked.Images)
	}
	want := []struct{ path, content string }{
		{"../../var/lib/flr/manifests/0.yaml", certManager},
		{"../../var/lib/flr/manifests/1.yaml", thing},
		{"../../var/lib/flr/manifests/2.yaml", kustomized},
	}
	if len(baked.Manifests) != 4 {
		t.Fatalf("the baked spec has %d manifests, want 4", len(baked.Manifests))
	}
	for i, w := range want {
		m := baked.Manifests[i]
		if m.Source() != "path" || m.Path != w.path || m.URL != "" || m.SHA256 != "" || m.Kustomize != "" {
			t.Errorf("manifests[%d] is %+v, want only path %s", i, m, w.path)
			continue
		}
		abs, err := baked.Abs(m.Path)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(abs); string(got) != w.content {
			t.Errorf("manifests[%d]: %s holds %q, want %q", i, abs, got, w.content)
		}
	}
	if w := baked.Manifests[0].Wait; !slices.Equal(w, []string{"deployment/cert-manager -n cert-manager"}) {
		t.Errorf("manifests[0].wait is %q", w)
	}
	h := baked.Manifests[3].Helm
	if h == nil || h.Chart != "../../var/lib/flr/charts/3.tgz" || h.Release != "thing" || h.Namespace != "thing" || h.Version != "1.4.2" {
		t.Fatalf("manifests[3].helm is %+v", h)
	}
	if abs, _ := baked.Abs(h.Chart); !bytes.Equal(must(os.ReadFile(abs)), chartArchive) {
		t.Errorf("the chart at %s is not the pulled archive", abs)
	}
	if !slices.Equal(h.Values, []string{"../../var/lib/flr/charts/3-values-0.yaml", "../../var/lib/flr/charts/3-values-1.yaml"}) {
		t.Errorf("values are %q", h.Values)
	}
	for j, w := range []string{"replicas: 1\n", "replicas: 2\n"} {
		if abs, _ := baked.Abs(h.Values[j]); string(must(os.ReadFile(abs))) != w {
			t.Errorf("values[%d] at %s is not the spec's file", j, abs)
		}
	}
	// With / as the root, as in the guest, the paths lead to /var/lib/flr.
	if p := filepath.Join("/", specDir, h.Chart); p != "/var/lib/flr/charts/3.tgz" {
		t.Errorf("in the guest the chart is at %s", p)
	}
}

func must(data []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return data
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake` SHALL write the digest of the spec, as `flr-kind
//# digest` prints it, to `OUT/etc/flr/kind-cluster.digest`.

func TestBakeWritesDigest(t *testing.T) {
	f := newFixture(t, goodSpec())
	f.mustBake(t)
	want, err := f.spec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, DigestFile); got != want+"\n" {
		t.Errorf("%s is %q, want %q", DigestFile, got, want+"\n")
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# If OUT holds a file, then `flr-kind bake` SHALL fail without writing to
//# it.

func TestBakeRefusesFullOut(t *testing.T) {
	f := newFixture(t, goodSpec())
	writeFiles(t, f.out, map[string]string{"stale": "x"})
	if _, err := f.bake(t); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Bake into a directory with a file: %v, want an error", err)
	}
	entries, _ := os.ReadDir(f.out)
	if len(entries) != 1 {
		t.Errorf("OUT has %d entries, want only the stale file", len(entries))
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# When a node of the kind configuration names an image other than the
//# one that the kind image's `KIND_NODE_IMAGE` names, `flr-kind bake`
//# SHALL write that image, for the target architecture, into Docker
//# Engine's image store under `OUT/var/lib/docker` with
//# `docker-store.sh`.

func TestBakeNodeImages(t *testing.T) {
	f := newFixture(t, goodSpec())
	res := f.mustBake(t)
	want := []string{"docker.io/" + otherNode, "docker.io/" + thirdNode}
	if !slices.Equal(f.store.calls, want) || !slices.Equal(res.NodeImages, want) {
		t.Errorf("docker-store.sh wrote %q (result %q), want %q: each node image but the kind image's, once", f.store.calls, res.NodeImages, want)
	}
	for _, want := range []string{
		"docker.io/kindest/node@sha256:" + hexB + " arm64 docker.io/kindest/node:v1.36.0",
		"docker.io/kindest/node@sha256:" + hexC + " arm64 docker.io/kindest/node:v1.35.0",
	} {
		if !slices.Contains(f.registry.archives, want) {
			t.Errorf("no archive %q in %q", want, f.registry.archives)
		}
	}
	// The layers of both node images are in OUT.
	for _, c := range []string{cacheID("ubuntu", "k8s-1.36"), cacheID("ubuntu", "k8s-1.36", "patch")} {
		if _, err := os.Stat(filepath.Join(f.out, DockerRoot, overlayDir, c, "diff")); err != nil {
			t.Errorf("a layer is missing: %v", err)
		}
	}
	// Nothing is left behind but the guest's paths.
	entries, _ := os.ReadDir(f.out)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"etc", "var"}) {
		t.Errorf("OUT holds %q, want etc and var", names)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# When `flr-kind bake` writes an image into Docker Engine's image store,
//# `flr-kind bake` SHALL write a `repositories.json` that holds every
//# reference of the kind image's `repositories.json` and every reference
//# that it wrote.

func TestBakeMergesRepositories(t *testing.T) {
	f := newFixture(t, goodSpec())
	f.mustBake(t)
	var got repositories
	if err := json.Unmarshal([]byte(f.read(t, DockerRoot+"/"+repositoriesJSON)), &got); err != nil {
		t.Fatal(err)
	}
	refs := got.Repositories["docker.io/kindest/node"]
	for _, ref := range []string{
		"docker.io/kindest/node:v1.37.0", "docker.io/kindest/node@sha256:" + hexA,
		"docker.io/kindest/node:v1.36.0", "docker.io/kindest/node@sha256:" + hexB,
		"docker.io/kindest/node:v1.35.0", "docker.io/kindest/node@sha256:" + hexC,
	} {
		if refs[ref] == "" {
			t.Errorf("repositories.json has no %s: %v", ref, got.Repositories)
		}
	}
	fi, err := os.Stat(filepath.Join(f.out, DockerRoot, repositoriesJSON))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("repositories.json: %v, mode %v; want 0600", err, fi.Mode())
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake` SHALL leave out of `OUT/var/lib/docker` each layer and
//# image configuration that the kind image's image store holds already.

func TestBakeLeavesOutBaseLayers(t *testing.T) {
	f := newFixture(t, goodSpec())
	f.mustBake(t)
	root := filepath.Join(f.out, DockerRoot)
	// "ubuntu" is the kind image's layer: neither its directory, its link
	// nor its layerdb entry is in OUT.
	if _, err := os.Lstat(filepath.Join(root, overlayDir, cacheID("ubuntu"))); err == nil {
		t.Error("OUT has the kind image's layer")
	}
	links, _ := os.ReadDir(filepath.Join(root, overlayLinks))
	layers, _ := os.ReadDir(filepath.Join(root, layerDB))
	dirs, _ := os.ReadDir(filepath.Join(root, overlayDir))
	// Two images share "k8s-1.36", which goes in once: two new layers in
	// all, plus overlay2/l.
	if len(links) != 2 || len(layers) != 2 || len(dirs) != 3 {
		t.Errorf("OUT has %d links, %d layerdb entries and %d overlay2 entries; want 2, 2 and 3", len(links), len(layers), len(dirs))
	}
	for _, d := range []string{root, filepath.Join(root, overlayDir)} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o710 {
			t.Errorf("%s: %v, want mode 0710 as docker-store.sh gives", d, err)
		}
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# If a node image that `flr-kind bake` has to write into Docker Engine's
//# image store has no tag, or `flr-kind bake` has no kind image root to
//# read, then `flr-kind bake` SHALL fail.

func TestBakeNodeImageNeedsTagAndBase(t *testing.T) {
	f := newFixture(t, goodSpec())
	if _, err := Bake(context.Background(), f.spec, f.out, f.tools, Options{Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "-base") {
		t.Errorf("Bake with node images and no -base: %v, want an error that names -base", err)
	}

	f = newFixture(t, goodSpec())
	writeFiles(t, f.repo, map[string]string{"test/e2e/kind-config.yaml": "nodes:\n  - image: kindest/node@sha256:" + hexB + "\n"})
	if _, err := f.bake(t); err == nil || !strings.Contains(err.Error(), "no tag") {
		t.Errorf("Bake with a node image with no tag: %v, want an error", err)
	}

	// Only the kind image's own node image: no -base is needed beyond
	// reading it, and nothing goes into the image store.
	f = newFixture(t, goodSpec())
	writeFiles(t, f.repo, map[string]string{"test/e2e/kind-config.yaml": "nodes:\n  - image: " + baseNode + "\n"})
	f.mustBake(t)
	if len(f.store.calls) != 0 {
		t.Errorf("docker-store.sh ran for %q", f.store.calls)
	}
	if _, err := os.Stat(filepath.Join(f.out, DockerRoot)); err == nil {
		t.Error("OUT has var/lib/docker with no node image to write")
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake` SHALL write each image under `images`, for the target
//# architecture, as a docker-archive in `OUT/var/lib/flr/images`.

func TestBakeSpecImages(t *testing.T) {
	f := newFixture(t, goodSpec())
	res := f.mustBake(t)
	if len(res.Images) == 0 || res.Images[0] != poolmgrd {
		t.Fatalf("archives %q, want %s first", res.Images, poolmgrd)
	}
	want := "ghcr.io/liquidmetal-dev/poolmgrd@sha256:" + hexA + " arm64 ghcr.io/liquidmetal-dev/poolmgrd:0.6.0\n"
	if got := f.read(t, ImagesDir+"/0.tar"); got != want {
		t.Errorf("images/0.tar is %q, want %q", got, want)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake` SHALL write each manifest by URL or by path, and the
//# output of `kustomize build` for each kustomize directory, to
//# `OUT/var/lib/flr/manifests/N.yaml`, where N is the entry's index under
//# `manifests`.

func TestBakeManifests(t *testing.T) {
	f := newFixture(t, goodSpec())
	f.mustBake(t)
	for rel, want := range map[string]string{
		ManifestsDir + "/0.yaml": certManager,
		ManifestsDir + "/1.yaml": thing,
		ManifestsDir + "/2.yaml": kustomized,
	} {
		if got := f.read(t, rel); got != want {
			t.Errorf("%s is %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.out, ManifestsDir, "3.yaml")); err == nil {
		t.Error("the chart's entry has a manifest file")
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# If the sha256 of a manifest that `flr-kind bake` downloads is not the
//# entry's `sha256`, then `flr-kind bake` SHALL fail.

func TestBakeChecksURLSHA256(t *testing.T) {
	f := newFixture(t, goodSpec())
	f.tools.Fetcher = fakeFetcher{certURL: []byte(certManager + "# changed\n")}
	_, err := f.bake(t)
	if err == nil || !strings.Contains(err.Error(), "manifests[0]") || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("Bake with a changed download: %v, want a sha256 error on manifests[0]", err)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake` SHALL write each chart's archive to
//# `OUT/var/lib/flr/charts/N.tgz`, and its values files to
//# `OUT/var/lib/flr/charts/N-values-M.yaml`, where N is the entry's index
//# under `manifests` and M the file's index under `values`.

func TestBakeCharts(t *testing.T) {
	f := newFixture(t, goodSpec())
	f.mustBake(t)
	if got := f.read(t, ChartsDir+"/3.tgz"); got != string(chartArchive) {
		t.Errorf("charts/3.tgz is %q", got)
	}
	for rel, want := range map[string]string{
		ChartsDir + "/3-values-0.yaml": "replicas: 1\n",
		ChartsDir + "/3-values-1.yaml": "replicas: 2\n",
	} {
		if got := f.read(t, rel); got != want {
			t.Errorf("%s is %q, want %q", rel, got, want)
		}
	}
	if len(f.helm.pulls) != 1 || !slices.Equal(f.helm.pulls[0], []string{chartOCI, "1.4.2"}) {
		t.Errorf("helm pull got %q", f.helm.pulls)
	}
	// helm template rendered the chart with both values files, in order.
	if len(f.helm.values) != 1 || !slices.Equal(f.helm.values[0], []string{"replicas: 1\n", "replicas: 2\n"}) {
		t.Errorf("helm template got the values %q", f.helm.values)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# If a chart's `digest` is neither the sha256 of the chart's archive nor,
//# for a chart from an OCI registry, the digest of an OCI manifest whose
//# chart layer is that archive, then `flr-kind bake` SHALL fail.

func TestChartDigest(t *testing.T) {
	archiveDigest := "sha256:" + sum(string(chartArchive))
	good := ociManifest(archiveDigest)
	other := ociManifest("sha256:" + hexB)
	for _, tc := range []struct {
		name, chart, digest string
		manifest            []byte
		ok                  bool
	}{
		{"oci by the archive's sha256", chartOCI, archiveDigest, good, true},
		{"oci by the manifest's digest", chartOCI, "sha256:" + sum(string(good)), good, true},
		{"oci by a manifest whose layer is another archive", chartOCI, "sha256:" + sum(string(other)), other, false},
		{"oci by neither", chartOCI, "sha256:" + hexC, good, false},
		{"repository by the archive's sha256", "https://charts.example.com/thing", archiveDigest, nil, true},
		{"repository by another digest", "https://charts.example.com/thing", "sha256:" + hexC, nil, false},
		{"repository by its manifest's digest, which only OCI has", "https://charts.example.com/thing", "sha256:" + sum(string(good)), good, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := strings.Replace(specText(tc.digest), chartOCI, tc.chart, 1)
			f := newFixture(t, spec)
			// The manifest is where the OCI check would look, so a chart from a
			// repository fails for want of the check, not of the manifest.
			f.registry.manifests[strings.TrimPrefix(tc.chart, "oci://")+":1.4.2"] = tc.manifest
			_, err := f.bake(t)
			if tc.ok && err != nil {
				t.Fatalf("Bake: %v", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "manifests[3]")) {
				t.Fatalf("Bake: %v, want a digest error on manifests[3]", err)
			}
			if tc.ok {
				// The baked spec names the archive by its own sha256.
				baked, err := kindspec.LoadFromRoot(filepath.Join(f.out, SpecFile), f.out)
				if err != nil {
					t.Fatal(err)
				}
				if d := baked.Manifests[3].Helm.Digest; d != archiveDigest {
					t.Errorf("the baked chart's digest is %s, want the archive's %s", d, archiveDigest)
				}
			}
		})
	}
}

func TestChartArgs(t *testing.T) {
	for chart, want := range map[string][]string{
		chartOCI:                                  {chartOCI},
		"https://example.com/charts/x-1.0.tgz":    {"https://example.com/charts/x-1.0.tgz"},
		"https://charts.jetstack.io/cert-manager": {"cert-manager", "--repo", "https://charts.jetstack.io"},
		"https://example.com/helm/stable/thing":   {"thing", "--repo", "https://example.com/helm/stable"},
		"thing":                                   nil,
		"jetstack/cert-manager":                   nil,
		"https://charts.jetstack.io/":             nil,
		"https://charts.jetstack.io":              nil,
	} {
		got, _, err := chartArgs(chart)
		if want == nil {
			if err == nil {
				t.Errorf("chartArgs(%q) = %q, want an error", chart, got)
			}
			continue
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("chartArgs(%q) = %q, %v; want %q", chart, got, err, want)
		}
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake` SHALL find the image of each container, init container
//# and ephemeral container in each manifest, in the output of `kustomize
//# build` for each kustomize directory, and in the output of `helm template`
//# for each chart with its values.

func TestFindImages(t *testing.T) {
	yaml := certManager + "---\n" + kustomized + "---\n" + charted + `---
apiVersion: v1
kind: List
items:
  - apiVersion: apps/v1
    kind: DaemonSet
    spec:
      template:
        spec:
          containers:
            - image: "registry.k8s.io/pause:3.10 "
---
# a CRD's schema has containers as a map, not a list: no image
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  versions:
    - schema:
        openAPIV3Schema:
          properties:
            containers:
              type: array
              items: {properties: {image: {type: string}}}
---
apiVersion: v1
kind: ConfigMap
data:
  image: not-a-container
`
	got, err := findImages([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"busybox:1.37",
		"docker.io/library/busybox:1.37",
		"ghcr.io/example/thing:1.4.2@sha256:" + hexC,
		"ghcr.io/liquidmetal-dev/poolmgrd:0.6.0",
		"quay.io/jetstack/cert-manager-controller:v1.20.2",
		"registry.k8s.io/pause:3.10",
	}
	if !slices.Equal(got, want) {
		t.Errorf("findImages = %q\nwant %q", got, want)
	}
	if _, err := findImages([]byte("a: [")); err == nil {
		t.Error("findImages on bad YAML: no error")
	}

	// A bake searches the URL manifest (busybox, the controller), the
	// kustomize output (poolmgrd) and the chart's template (thing).
	f := newFixture(t, goodSpec())
	res := f.mustBake(t)
	var images []string
	for _, l := range res.Lock {
		images = append(images, strings.Fields(l)[0])
	}
	if !slices.Equal(images, want[:5]) {
		t.Errorf("a bake found %q, want %q", images, want[:5])
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake` SHALL write each image that it finds, by the digest
//# that its reference resolves to and for the target architecture, as a
//# docker-archive in `OUT/var/lib/flr/images`, unless an image under
//# `images` has the same name and the same tag or digest.

func TestBakeFoundImages(t *testing.T) {
	f := newFixture(t, goodSpec())
	res := f.mustBake(t)
	// poolmgrd is under images, by the tag the kustomize output names, so
	// it has one archive. busybox, by two names, has one too.
	wantRefs := []string{
		poolmgrd,
		"busybox:1.37",
		"ghcr.io/example/thing:1.4.2@sha256:" + hexC,
		"quay.io/jetstack/cert-manager-controller:v1.20.2",
	}
	if !slices.Equal(res.Images, wantRefs) {
		t.Fatalf("archives %q\nwant %q", res.Images, wantRefs)
	}
	for rel, want := range map[string]string{
		ImagesDir + "/1.tar": "docker.io/library/busybox@sha256:" + sum("busybox index") + " arm64 docker.io/library/busybox:1.37\n",
		ImagesDir + "/2.tar": "ghcr.io/example/thing@sha256:" + hexC + " arm64 ghcr.io/example/thing:1.4.2\n",
		ImagesDir + "/3.tar": "quay.io/jetstack/cert-manager-controller@sha256:" + sum("controller index") + " arm64 quay.io/jetstack/cert-manager-controller:v1.20.2\n",
	} {
		if got := f.read(t, rel); got != want {
			t.Errorf("%s is %q\nwant %q", rel, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.out, ImagesDir, "4.tar")); err == nil {
		t.Error("there is a fifth archive")
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//= type=test
//# `flr-kind bake` SHALL write `OUT/etc/flr/images.lock` with one line for
//# each image that it finds, which holds the image as the manifest names
//# it and the digest of the image that it baked, in the byte order of the
//# images.

func TestBakeLockFile(t *testing.T) {
	f := newFixture(t, goodSpec())
	f.mustBake(t)
	want := "# The images that flr-kind bake found in the manifests, each with the\n# digest it baked: IMAGE DIGEST.\n" +
		"busybox:1.37 sha256:" + sum("busybox index") + "\n" +
		"docker.io/library/busybox:1.37 sha256:" + sum("busybox index") + "\n" +
		"ghcr.io/example/thing:1.4.2@sha256:" + hexC + " sha256:" + hexC + "\n" +
		"ghcr.io/liquidmetal-dev/poolmgrd:0.6.0 sha256:" + hexA + "\n" +
		"quay.io/jetstack/cert-manager-controller:v1.20.2 sha256:" + sum("controller index") + "\n"
	if got := f.read(t, LockFile); got != want {
		t.Errorf("images.lock is\n%s\nwant\n%s", got, want)
	}
}

func TestBakeMinimalSpec(t *testing.T) {
	f := newFixture(t, "apiVersion: flintlock-runner.dev/v1alpha1\nkind: KindCluster\nname: tiny\n")
	f.tools = Tools{} // a spec with nothing to fetch uses no tool
	res, err := Bake(context.Background(), f.spec, f.out, f.tools, Options{})
	if err != nil {
		t.Fatal(err)
	}
	baked, err := kindspec.LoadFromRoot(filepath.Join(f.out, SpecFile), f.out)
	if err != nil || baked.KindConfig != "" || len(baked.Manifests) != 0 {
		t.Fatalf("baked spec %+v, %v", baked, err)
	}
	if res.Files != 3 || res.Bytes == 0 {
		t.Errorf("wrote %d files, %d bytes; want the spec, digest and lock file", res.Files, res.Bytes)
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]imageRef{
		"nginx":                               {Name: "docker.io/library/nginx"},
		"nginx:1.27":                          {Name: "docker.io/library/nginx", Tag: "1.27"},
		"kindest/node:v1.37.0@sha256:" + hexA: {Name: "docker.io/kindest/node", Tag: "v1.37.0", Digest: "sha256:" + hexA},
		"localhost:5000/x/y:t":                {Name: "localhost:5000/x/y", Tag: "t"},
		"localhost/x":                         {Name: "localhost/x"},
		"quay.io/a/b@sha256:" + hexB:          {Name: "quay.io/a/b", Digest: "sha256:" + hexB},
		"index.docker.io/library/nginx:1":     {Name: "docker.io/library/nginx", Tag: "1"},
	} {
		got, err := parseRef(in)
		want.Raw = in
		if err != nil || got != want {
			t.Errorf("parseRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "UPPER/case", "nginx@sha256:short", "nginx:bad tag", "a//b"} {
		if _, err := parseRef(bad); err == nil {
			t.Errorf("parseRef(%q): no error", bad)
		}
	}
}
