// Package kindbake writes what a project's guest image adds to the kind
// image for its kind cluster spec
// (docs/requirements/13-guest-image.md#guest-kind-bake): the spec made to
// stand alone, its digest, the node images in Docker Engine's image store,
// the other images as archives, the manifests, the charts and the lock file
// of the images found in them.
//
// [Bake] writes them under a directory OUT, at the paths they have in the
// guest, so that a Containerfile copies OUT to / on top of the kind image:
//
//	etc/flr/kind-cluster.yaml       the spec, which names only files under OUT
//	etc/flr/kind-cluster.digest     the digest of the project's spec
//	etc/flr/images.lock             each image found in the manifests, and its digest
//	var/lib/docker/...              each node image the kind image lacks
//	var/lib/flr/images/N.tar        each image, as a docker-archive
//	var/lib/flr/manifests/N.yaml    each manifest entry N by url, path or kustomize
//	var/lib/flr/charts/N.tgz        each chart entry N, and N-values-M.yaml
//
// The programs and the network sit behind [Tools], so that tests run with
// fakes.
package kindbake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/phoban01/flintlock-runner/internal/kindspec"
)

// Where a bake writes, relative to OUT, which are the paths in the guest
// relative to /.
const (
	SpecFile     = "etc/flr/kind-cluster.yaml"
	DigestFile   = "etc/flr/kind-cluster.digest"
	LockFile     = "etc/flr/images.lock"
	ImagesDir    = "var/lib/flr/images"
	ManifestsDir = "var/lib/flr/manifests"
	ChartsDir    = "var/lib/flr/charts"
	DockerRoot   = "var/lib/docker"
)

// SpecRoot is the root that the baked spec's paths stay inside: /, since
// they lead from /etc/flr to /var/lib/flr. The create hook loads the baked
// spec with kindspec.LoadFromRoot(SpecFile, SpecRoot).
const SpecRoot = "/"

// specDir is the directory of the baked spec, against which its paths are
// read.
var specDir = path.Dir(SpecFile)

// Options are a bake's settings.
type Options struct {
	// Arch is the target architecture, amd64 or arm64; empty means the
	// architecture flr-kind runs on.
	Arch string
	// Base is a directory that holds the kind image's root filesystem, such
	// as a mount of the image. A bake reads the node image it has, and its
	// repositories.json, from there. It is needed only when the kind
	// configuration names a node image.
	Base string
	// Log takes a line for each step; nil means none.
	Log io.Writer
}

// Result says what a bake wrote.
type Result struct {
	// Bytes is the total size of the files written, and Files their
	// number.
	Bytes int64
	Files int
	// NodeImages are the node images written into Docker Engine's image
	// store, Images the archives under ImagesDir in order, and Lock the
	// lines of the lock file.
	NodeImages []string
	Images     []string
	Lock       []string
}

type baker struct {
	spec  *kindspec.Spec
	out   string
	tmp   string
	opts  Options
	tools Tools

	// baked is the spec that goes to SpecFile.
	baked kindspec.Spec
	// rendered holds the YAML of each manifest, kustomize directory and
	// chart, for the image search.
	rendered [][]byte
}

// Bake writes under out what a guest image needs for spec, as the package
// comment lists it.
func Bake(ctx context.Context, spec *kindspec.Spec, out string, tools Tools, opts Options) (*Result, error) {
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if err := emptyDir(out); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(out, ".flr-bake-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	b := &baker{spec: spec, out: out, tmp: tmp, opts: opts, tools: tools, baked: *spec}
	res := &Result{}

	digest, err := spec.Digest()
	if err != nil {
		return nil, err
	}
	if res.NodeImages, err = b.bakeNodeImages(ctx); err != nil {
		return nil, err
	}
	if err := b.inlineKindConfig(); err != nil {
		return nil, err
	}
	b.baked.Manifests = slices.Clone(spec.Manifests)
	for i := range b.baked.Manifests {
		if err := b.manifest(ctx, i, &b.baked.Manifests[i]); err != nil {
			return nil, fmt.Errorf("manifests[%d]: %w", i, err)
		}
	}
	if res.Images, res.Lock, err = b.images(ctx); err != nil {
		return nil, err
	}
	if err := b.writeSpec(); err != nil {
		return nil, err
	}

	//= docs/requirements/13-guest-image.md#guest-kind-bake
	//# `flr-kind bake` SHALL write the digest of the spec, as `flr-kind
	//# digest` prints it, to `OUT/etc/flr/kind-cluster.digest`.
	if err := b.write(DigestFile, []byte(digest+"\n")); err != nil {
		return nil, err
	}

	if err := os.RemoveAll(tmp); err != nil {
		return nil, err
	}
	res.Bytes, res.Files, err = size(out)
	return res, err
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//# If OUT holds a file, then `flr-kind bake` SHALL fail without writing to
//# it.

// emptyDir makes dir, or checks that it is an empty directory.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o755)
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("%s is not empty; flr-kind bake writes into an empty directory", dir)
	}
	return nil
}

// write writes a file under OUT with mode 0644, making its directories.
func (b *baker) write(rel string, data []byte) error {
	p := filepath.Join(b.out, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// specPath is the path of a file under OUT as the baked spec names it:
// relative to /etc/flr.
func specPath(rel string) string {
	p, err := filepath.Rel(filepath.FromSlash(specDir), filepath.FromSlash(rel))
	if err != nil {
		panic(err) // both are relative, so Rel cannot fail
	}
	return filepath.ToSlash(p)
}

// inlineKindConfig puts the kind configuration in the baked spec itself.
func (b *baker) inlineKindConfig() error {
	data, err := b.spec.KindConfigData()
	if err != nil || data == nil {
		return err
	}
	cfg := string(data)
	if !strings.HasSuffix(cfg, "\n") {
		cfg += "\n"
	}
	b.baked.KindConfig = cfg
	return nil
}

// manifest bakes one entry under manifests and rewrites m, the baked
// spec's copy of it, to name the baked files.
func (b *baker) manifest(ctx context.Context, i int, m *kindspec.Manifest) error {
	var data []byte
	switch m.Source() {
	case "url":
		fmt.Fprintf(b.opts.Log, "flr-kind bake: downloading %s\n", m.URL)
		got, err := b.tools.Fetcher.Fetch(ctx, m.URL)
		if err != nil {
			return err
		}
		//= docs/requirements/13-guest-image.md#guest-kind-bake
		//# If the sha256 of a manifest that `flr-kind bake` downloads is not the
		//# entry's `sha256`, then `flr-kind bake` SHALL fail.
		if sum := sha256Hex(got); sum != m.SHA256 {
			return fmt.Errorf("%s has sha256 %s, and the spec says %s", m.URL, sum, m.SHA256)
		}
		data = got
	case "path":
		abs, err := b.spec.Abs(m.Path)
		if err != nil {
			return err
		}
		if data, err = os.ReadFile(abs); err != nil {
			return err
		}
	case "kustomize":
		abs, err := b.spec.Abs(m.Kustomize)
		if err != nil {
			return err
		}
		fmt.Fprintf(b.opts.Log, "flr-kind bake: rendering %s with kustomize build\n", m.Kustomize)
		if data, err = b.tools.Kustomize.Build(ctx, abs); err != nil {
			return err
		}
	case "helm":
		return b.chart(ctx, i, m)
	default:
		return errors.New("has no source")
	}

	//= docs/requirements/13-guest-image.md#guest-kind-bake
	//# `flr-kind bake` SHALL write each manifest by URL or by path, and the
	//# output of `kustomize build` for each kustomize directory, to
	//# `OUT/var/lib/flr/manifests/N.yaml`, where N is the entry's index under
	//# `manifests`.
	rel := fmt.Sprintf("%s/%d.yaml", ManifestsDir, i)
	if err := b.write(rel, data); err != nil {
		return err
	}
	b.rendered = append(b.rendered, data)
	*m = kindspec.Manifest{Path: specPath(rel), Wait: m.Wait}
	return nil
}

// chart pulls a chart, checks its digest, copies its values files and
// renders it for the image search.
func (b *baker) chart(ctx context.Context, i int, m *kindspec.Manifest) error {
	h := *m.Helm
	args, oci, err := chartArgs(h.Chart)
	if err != nil {
		return fmt.Errorf("helm.chart: %w", err)
	}
	dir, err := os.MkdirTemp(b.tmp, "chart-")
	if err != nil {
		return err
	}
	fmt.Fprintf(b.opts.Log, "flr-kind bake: pulling the chart %s %s\n", h.Chart, h.Version)
	pulled, err := b.tools.Helm.Pull(ctx, args, h.Version, dir)
	if err != nil {
		return err
	}
	archive, err := os.ReadFile(pulled)
	if err != nil {
		return err
	}
	sum := "sha256:" + sha256Hex(archive)
	if err := b.checkChart(ctx, h, oci, sum); err != nil {
		return err
	}

	//= docs/requirements/13-guest-image.md#guest-kind-bake
	//# `flr-kind bake` SHALL write each chart's archive to
	//# `OUT/var/lib/flr/charts/N.tgz`, and its values files to
	//# `OUT/var/lib/flr/charts/N-values-M.yaml`, where N is the entry's index
	//# under `manifests` and M the file's index under `values`.
	rel := fmt.Sprintf("%s/%d.tgz", ChartsDir, i)
	if err := b.write(rel, archive); err != nil {
		return err
	}
	var values, valuesAbs []string
	for j, v := range h.Values {
		abs, err := b.spec.Abs(v)
		if err != nil {
			return fmt.Errorf("helm.values[%d]: %w", j, err)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("helm.values[%d]: %w", j, err)
		}
		vrel := fmt.Sprintf("%s/%d-values-%d.yaml", ChartsDir, i, j)
		if err := b.write(vrel, data); err != nil {
			return err
		}
		values = append(values, specPath(vrel))
		valuesAbs = append(valuesAbs, filepath.Join(b.out, filepath.FromSlash(vrel)))
	}
	out, err := b.tools.Helm.Template(ctx, filepath.Join(b.out, filepath.FromSlash(rel)), h.Release, h.Namespace, valuesAbs)
	if err != nil {
		return err
	}
	b.rendered = append(b.rendered, out)

	h.Chart, h.Digest, h.Values = specPath(rel), sum, values
	*m = kindspec.Manifest{Helm: &h, Wait: m.Wait}
	return nil
}

// helmChartLayer is the media type of a chart's archive in an OCI
// manifest.
const helmChartLayer = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

//= docs/requirements/13-guest-image.md#guest-kind-bake
//# If a chart's `digest` is neither the sha256 of the chart's archive nor,
//# for a chart from an OCI registry, the digest of an OCI manifest whose
//# chart layer is that archive, then `flr-kind bake` SHALL fail.

// checkChart checks the chart's digest against sum, the sha256 of the
// archive that helm pulled. For a chart from an OCI registry the digest can
// also be the OCI manifest's, which helm push prints: then the manifest
// has to have that digest, and its chart layer has to be the archive.
func (b *baker) checkChart(ctx context.Context, h kindspec.Helm, oci bool, sum string) error {
	if h.Digest == sum {
		return nil
	}
	if !oci {
		return fmt.Errorf("the chart's archive has digest %s, and the spec says %s", sum, h.Digest)
	}
	// helm tags an OCI chart with its version, with + as _.
	ref := strings.TrimPrefix(h.Chart, "oci://") + ":" + strings.ReplaceAll(h.Version, "+", "_")
	raw, err := b.tools.Registry.Manifest(ctx, ref)
	if err != nil {
		return err
	}
	if got := "sha256:" + sha256Hex(raw); got != h.Digest {
		return fmt.Errorf("the chart's archive has digest %s and its OCI manifest %s, and the spec says %s", sum, got, h.Digest)
	}
	var manifest struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("the chart's OCI manifest: %w", err)
	}
	for _, l := range manifest.Layers {
		if l.MediaType == helmChartLayer && l.Digest == sum {
			return nil
		}
	}
	return fmt.Errorf("the chart's OCI manifest %s has no chart layer %s, the archive that helm pulled", h.Digest, sum)
}

// chartArgs turns a chart into helm pull's arguments: an OCI reference
// (oci://...) or the URL of a chart's archive (.tgz) as it is, and
// REPOSITORY_URL/CHART as CHART --repo REPOSITORY_URL. It says whether the
// chart is from an OCI registry.
func chartArgs(chart string) ([]string, bool, error) {
	switch {
	case strings.HasPrefix(chart, "oci://"):
		return []string{chart}, true, nil
	case strings.HasPrefix(chart, "https://") || strings.HasPrefix(chart, "http://"):
		if strings.HasSuffix(chart, ".tgz") {
			return []string{chart}, false, nil
		}
		u, err := url.Parse(chart)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, false, fmt.Errorf("%q is not REPOSITORY_URL/CHART", chart)
		}
		dir, name := path.Split(u.Path)
		if name == "" || dir == "" {
			return nil, false, fmt.Errorf("%q is not REPOSITORY_URL/CHART", chart)
		}
		u.Path = strings.TrimSuffix(dir, "/")
		return []string{name, "--repo", u.String()}, false, nil
	}
	return nil, false, fmt.Errorf("%q is not an oci:// reference, the URL of a chart's archive or REPOSITORY_URL/CHART", chart)
}

// images writes the archives: the spec's images in order, then each image
// found in the rendered YAML that none of them covers, in byte order. It
// returns the archives' references and the lock file's lines.
func (b *baker) images(ctx context.Context) ([]string, []string, error) {
	var found []string
	for _, data := range b.rendered {
		imgs, err := findImages(data)
		if err != nil {
			return nil, nil, err
		}
		found = append(found, imgs...)
	}
	slices.Sort(found)
	found = slices.Compact(found)

	var specRefs []imageRef
	for i, img := range b.spec.Images {
		r, err := parseRef(img)
		if err != nil {
			return nil, nil, fmt.Errorf("images[%d]: %w", i, err)
		}
		specRefs = append(specRefs, r)
	}

	type archive struct {
		ref imageRef
		// pull is what to copy, by digest.
		pull string
	}
	var archives []archive
	for _, r := range specRefs {
		archives = append(archives, archive{ref: r, pull: r.pullRef()})
	}

	var lock []string
	seen := map[string]bool{}
	for _, img := range found {
		r, err := parseRef(img)
		if err != nil {
			return nil, nil, fmt.Errorf("an image in the manifests: %w", err)
		}
		//= docs/requirements/13-guest-image.md#guest-kind-bake
		//# `flr-kind bake` SHALL write each image that it finds, by the digest
		//# that its reference resolves to and for the target architecture, as a
		//# docker-archive in `OUT/var/lib/flr/images`, unless an image under
		//# `images` has the same name and the same tag or digest.
		digest := r.Digest
		var covered bool
		for _, s := range specRefs {
			if s.covers(r) {
				covered, digest = true, s.Digest
				break
			}
		}
		if digest == "" {
			raw, err := b.tools.Registry.Manifest(ctx, r.pullRef())
			if err != nil {
				return nil, nil, fmt.Errorf("the image %s in the manifests: %w", img, err)
			}
			digest = "sha256:" + sha256Hex(raw)
		}
		lock = append(lock, img+" "+digest)
		// Two ways of writing one image, such as nginx:1.27 and
		// docker.io/library/nginx:1.27, give one archive.
		key := r.archiveTag() + "@" + digest
		if !covered && !seen[key] {
			seen[key] = true
			archives = append(archives, archive{ref: r, pull: r.Name + "@" + digest})
		}
	}

	var refs []string
	for n, a := range archives {
		//= docs/requirements/13-guest-image.md#guest-kind-bake
		//# `flr-kind bake` SHALL write each image under `images`, for the target
		//# architecture, as a docker-archive in `OUT/var/lib/flr/images`.
		rel := fmt.Sprintf("%s/%d.tar", ImagesDir, n)
		p := filepath.Join(b.out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, nil, err
		}
		tag := a.ref.archiveTag()
		if tag == "" {
			fmt.Fprintf(b.opts.Log, "flr-kind bake: warning: %s has no tag, so its archive names no image, and a Pod that names it by digest pulls it\n", a.ref.Raw)
		}
		fmt.Fprintf(b.opts.Log, "flr-kind bake: writing %s for %s to %s\n", a.ref.Raw, b.opts.Arch, rel)
		if err := b.tools.Registry.Archive(ctx, a.pull, b.opts.Arch, p, tag); err != nil {
			return nil, nil, fmt.Errorf("the image %s: %w", a.ref.Raw, err)
		}
		refs = append(refs, a.ref.Raw)
	}

	//= docs/requirements/13-guest-image.md#guest-kind-bake
	//# `flr-kind bake` SHALL write `OUT/etc/flr/images.lock` with one line for
	//# each image that it finds, which holds the image as the manifest names
	//# it and the digest of the image that it baked, in the byte order of the
	//# images.
	var buf bytes.Buffer
	buf.WriteString("# The images that flr-kind bake found in the manifests, each with the\n# digest it baked: IMAGE DIGEST.\n")
	for _, l := range lock {
		buf.WriteString(l + "\n")
	}
	if err := b.write(LockFile, buf.Bytes()); err != nil {
		return nil, nil, err
	}
	return refs, lock, nil
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//# `flr-kind bake SPEC OUT` SHALL write to `OUT/etc/flr/kind-cluster.yaml` a
//# kind cluster spec that has its kind configuration inline and that names,
//# by a path relative to `/etc/flr`, the file under `OUT/var/lib/flr` that
//# it wrote for each manifest, chart and values file.

// writeSpec writes the baked spec.
func (b *baker) writeSpec() error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Written by flr-kind bake. Every path is relative to /etc/flr and stays\n# inside /, and the kind configuration is inline. %s holds the\n# digest of the project's spec.\n", "/"+DigestFile)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&b.baked); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return b.write(SpecFile, buf.Bytes())
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// size returns the total size and the number of the regular files under
// dir.
func size(dir string) (int64, int, error) {
	var total int64
	var n int
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		n++
		return nil
	})
	return total, n, err
}
