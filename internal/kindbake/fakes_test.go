package kindbake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// fakeRegistry serves manifests from a map and writes each archive as a
// line that says what was asked for.
type fakeRegistry struct {
	mu        sync.Mutex
	manifests map[string][]byte
	archives  []string
}

func (f *fakeRegistry) Manifest(_ context.Context, ref string) ([]byte, error) {
	m, ok := f.manifests[ref]
	if !ok {
		return nil, fmt.Errorf("fake registry: no manifest %s", ref)
	}
	return m, nil
}

func (f *fakeRegistry) Archive(_ context.Context, ref, arch, path, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := fmt.Sprintf("%s %s %s", ref, arch, tag)
	f.archives = append(f.archives, line)
	return os.WriteFile(path, []byte(line+"\n"), 0o644)
}

type fakeFetcher map[string][]byte

func (f fakeFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	data, ok := f[url]
	if !ok {
		return nil, fmt.Errorf("fake fetcher: no %s", url)
	}
	return data, nil
}

// fakeKustomize renders a directory as the content of its
// rendered.yaml.
type fakeKustomize struct{}

func (fakeKustomize) Build(_ context.Context, dir string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, "rendered.yaml"))
}

// fakeHelm pulls the archive it holds and renders a fixed YAML, with the
// values files' content appended as a comment so tests see they were
// passed.
type fakeHelm struct {
	archive  []byte
	rendered string
	pulls    [][]string
	values   [][]string
}

func (f *fakeHelm) Pull(_ context.Context, args []string, version, dir string) (string, error) {
	f.pulls = append(f.pulls, append(append([]string{}, args...), version))
	p := filepath.Join(dir, "chart-"+version+".tgz")
	return p, os.WriteFile(p, f.archive, 0o644)
}

func (f *fakeHelm) Template(_ context.Context, archive, _, _ string, values []string) ([]byte, error) {
	if _, err := os.Stat(archive); err != nil {
		return nil, err
	}
	var contents []string
	for _, v := range values {
		data, err := os.ReadFile(v)
		if err != nil {
			return nil, err
		}
		contents = append(contents, string(data))
	}
	f.values = append(f.values, contents)
	return []byte(f.rendered), nil
}

// fakeDockerStore writes the tree that docker-store.sh writes, with the
// layers that layers names for each reference: a layer is its diff id, and
// the cache ids and links come from the chain ids, as in docker-store.sh.
type fakeDockerStore struct {
	layers map[string][]string
	calls  []string
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (f *fakeDockerStore) Store(_ context.Context, archive, ref, root string) error {
	f.calls = append(f.calls, ref)
	if _, err := os.Stat(archive); err != nil {
		return err
	}
	return writeStore(root, ref, f.layers[ref])
}

// writeStore writes an image store under root with one image, ref, and
// its layers.
func writeStore(root, ref string, layers []string) error {
	nameTag, digest, _ := strings.Cut(ref, "@")
	i := strings.LastIndex(nameTag, ":")
	name := nameTag[:i]
	config := sum("config " + ref)
	for _, dir := range []string{imageContent, imageMetadata, layerDB, overlayLinks} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, imageContent, config), []byte("config"), 0o600); err != nil {
		return err
	}
	chain := ""
	for _, l := range layers {
		diff := "sha256:" + sum(l)
		if chain == "" {
			chain = diff
		} else {
			chain = "sha256:" + sum(chain+" "+diff)
		}
		cache := sum("cache " + chain)
		link := strings.ToUpper(sum(cache)[:26])
		if err := os.MkdirAll(filepath.Join(root, overlayDir, cache, "diff"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, overlayDir, cache, "diff", l), []byte(l), 0o644); err != nil {
			return err
		}
		if err := os.Symlink("../"+cache+"/diff", filepath.Join(root, overlayLinks, link)); err != nil {
			return err
		}
		meta := filepath.Join(root, layerDB, strings.TrimPrefix(chain, "sha256:"))
		if err := os.MkdirAll(meta, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(meta, "cache-id"), []byte(cache), 0o644); err != nil {
			return err
		}
	}
	repos := fmt.Sprintf(`{"Repositories":{%q:{%q:"sha256:%s",%q:"sha256:%s"}}}`, name, nameTag, config, name+"@"+digest, config)
	return os.WriteFile(filepath.Join(root, repositoriesJSON), []byte(repos), 0o600)
}

// cacheID is the cache id of the layer chain of layers, as writeStore
// names it.
func cacheID(layers ...string) string {
	chain := ""
	for _, l := range layers {
		diff := "sha256:" + sum(l)
		if chain == "" {
			chain = diff
		} else {
			chain = "sha256:" + sum(chain+" "+diff)
		}
	}
	return sum("cache " + chain)
}
