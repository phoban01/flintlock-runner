package kindbake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Paths in Docker Engine's image store, relative to /var/lib/docker, as
// docker-store.sh writes them.
const (
	repositoriesJSON = "image/overlay2/repositories.json"
	imageContent     = "image/overlay2/imagedb/content/sha256"
	imageMetadata    = "image/overlay2/imagedb/metadata/sha256"
	layerDB          = "image/overlay2/layerdb/sha256"
	overlayDir       = "overlay2"
	overlayLinks     = "overlay2/l"
)

// kindBootstrap is the kind image's file that names its node image
// (GI-062).
const kindBootstrap = "etc/default/kind-bootstrap"

// repositories is Docker Engine's repositories.json: for each repository,
// each reference and the image ID it names.
type repositories struct {
	Repositories map[string]map[string]string `json:"Repositories"`
}

// nodeImages returns the image of each node in the kind configuration,
// each once, in the order of the nodes.
func nodeImages(kindConfig []byte) ([]string, error) {
	var cfg struct {
		Nodes []struct {
			Image string `yaml:"image"`
		} `yaml:"nodes"`
	}
	if err := yaml.Unmarshal(kindConfig, &cfg); err != nil {
		return nil, fmt.Errorf("kindConfig is not YAML: %w", err)
	}
	var images []string
	for _, n := range cfg.Nodes {
		if n.Image != "" && !contains(images, n.Image) {
			images = append(images, n.Image)
		}
	}
	return images, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var kindNodeImageLine = regexp.MustCompile(`^\s*(?:export\s+)?KIND_NODE_IMAGE=["']?([^"'\s]+)["']?\s*$`)

// baseNodeImage reads KIND_NODE_IMAGE from the kind image's
// /etc/default/kind-bootstrap under base.
func baseNodeImage(base string) (imageRef, error) {
	path := filepath.Join(base, kindBootstrap)
	f, err := os.Open(path)
	if err != nil {
		return imageRef{}, fmt.Errorf("the kind image's node image: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := kindNodeImageLine.FindStringSubmatch(sc.Text()); m != nil {
			return parseRef(m[1])
		}
	}
	if err := sc.Err(); err != nil {
		return imageRef{}, err
	}
	return imageRef{}, fmt.Errorf("%s does not set KIND_NODE_IMAGE", path)
}

// readRepositories reads a repositories.json; a file that does not exist
// is an empty one.
func readRepositories(path string) (repositories, error) {
	r := repositories{Repositories: map[string]map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	if r.Repositories == nil {
		r.Repositories = map[string]map[string]string{}
	}
	return r, nil
}

func (r repositories) merge(o repositories) {
	for repo, refs := range o.Repositories {
		if r.Repositories[repo] == nil {
			r.Repositories[repo] = map[string]string{}
		}
		for ref, id := range refs {
			r.Repositories[repo][ref] = id
		}
	}
}

// bakeNodeImages writes each node image of the kind configuration that the
// kind image does not have into Docker Engine's image store under
// OUT/var/lib/docker, and returns the references it wrote.
func (b *baker) bakeNodeImages(ctx context.Context) ([]string, error) {
	data, err := b.spec.KindConfigData()
	if err != nil || data == nil {
		return nil, err
	}
	images, err := nodeImages(data)
	if err != nil || len(images) == 0 {
		return nil, err
	}

	//= docs/requirements/13-guest-image.md#guest-kind-bake
	//# If a node image that `flr-kind bake` has to write into Docker Engine's
	//# image store has no tag, or `flr-kind bake` has no kind image root to
	//# read, then `flr-kind bake` SHALL fail.
	if b.opts.Base == "" {
		return nil, fmt.Errorf("kindConfig names node images (%s), and flr-kind bake needs the kind image's root filesystem (-base) to see which it has", strings.Join(images, ", "))
	}
	have, err := baseNodeImage(b.opts.Base)
	if err != nil {
		return nil, err
	}
	var todo []imageRef
	for _, img := range images {
		r, err := parseRef(img)
		if err != nil {
			return nil, fmt.Errorf("kindConfig: %w", err)
		}
		//= docs/requirements/13-guest-image.md#guest-kind-bake
		//# When a node of the kind configuration names an image other than the
		//# one that the kind image's `KIND_NODE_IMAGE` names, `flr-kind bake`
		//# SHALL write that image, for the target architecture, into Docker
		//# Engine's image store under `OUT/var/lib/docker` with
		//# `docker-store.sh`.
		if r.Name == have.Name && r.Digest == have.Digest {
			fmt.Fprintf(b.opts.Log, "flr-kind bake: the kind image has the node image %s already\n", img)
			continue
		}
		if r.Tag == "" {
			return nil, fmt.Errorf("kindConfig: the node image %s has no tag; docker-store.sh writes a node image under NAME:TAG@sha256:DIGEST", img)
		}
		todo = append(todo, r)
	}
	if len(todo) == 0 {
		return nil, nil
	}

	baseRoot := filepath.Join(b.opts.Base, DockerRoot)
	repos, err := readRepositories(filepath.Join(baseRoot, repositoriesJSON))
	if err != nil {
		return nil, fmt.Errorf("the kind image's image store: %w", err)
	}
	dest := filepath.Join(b.out, DockerRoot)
	for _, dir := range []string{imageContent, imageMetadata, layerDB, overlayLinks} {
		if err := os.MkdirAll(filepath.Join(dest, dir), 0o755); err != nil {
			return nil, err
		}
	}
	// The modes docker-store.sh gives, which COPY puts on the kind
	// image's directories too.
	for _, dir := range []string{dest, filepath.Join(dest, overlayDir)} {
		if err := os.Chmod(dir, 0o710); err != nil {
			return nil, err
		}
	}

	var wrote []string
	for i, r := range todo {
		ref := r.Name + ":" + r.Tag + "@" + r.Digest
		fmt.Fprintf(b.opts.Log, "flr-kind bake: writing the node image %s into Docker Engine's image store\n", ref)
		archive := filepath.Join(b.tmp, fmt.Sprintf("node-%d.tar", i))
		if err := b.tools.Registry.Archive(ctx, r.pullRef(), b.opts.Arch, archive, r.Name+":"+r.Tag); err != nil {
			return nil, fmt.Errorf("the node image %s: %w", r.Raw, err)
		}
		root := filepath.Join(b.tmp, fmt.Sprintf("node-%d", i))
		if err := b.tools.DockerStore.Store(ctx, archive, ref, root); err != nil {
			return nil, fmt.Errorf("the node image %s: %w", r.Raw, err)
		}
		if err := os.Remove(archive); err != nil {
			return nil, err
		}
		got, err := readRepositories(filepath.Join(root, repositoriesJSON))
		if err != nil {
			return nil, err
		}
		if len(got.Repositories) == 0 {
			return nil, fmt.Errorf("the node image %s: docker-store.sh wrote no repositories.json", r.Raw)
		}
		//= docs/requirements/13-guest-image.md#guest-kind-bake
		//# When `flr-kind bake` writes an image into Docker Engine's image store,
		//# `flr-kind bake` SHALL write a `repositories.json` that holds every
		//# reference of the kind image's `repositories.json` and every reference
		//# that it wrote.
		repos.merge(got)
		if err := mergeStore(root, dest, baseRoot); err != nil {
			return nil, fmt.Errorf("the node image %s: %w", r.Raw, err)
		}
		wrote = append(wrote, ref)
	}

	data, err = json.Marshal(repos)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dest, repositoriesJSON), data, 0o600); err != nil {
		return nil, err
	}
	return wrote, nil
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//# `flr-kind bake` SHALL leave out of `OUT/var/lib/docker` each layer and
//# image configuration that the kind image's image store holds already.

// mergeStore moves what docker-store.sh wrote under src into dest, leaving
// out each image configuration, layer and link that dest or the kind
// image's store under base has already. docker-store.sh names each of them
// after the chain of layer digests, so a name that exists holds the same
// files. The moves keep each file's owner, mode and extended attributes.
func mergeStore(src, dest, base string) error {
	for _, dir := range []string{imageContent, layerDB, overlayLinks, overlayDir} {
		entries, err := os.ReadDir(filepath.Join(src, dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if dir == overlayDir && name == "l" {
				continue
			}
			rel := filepath.Join(dir, name)
			if exists(filepath.Join(dest, rel)) || exists(filepath.Join(base, rel)) {
				continue
			}
			if err := os.Rename(filepath.Join(src, rel), filepath.Join(dest, rel)); err != nil {
				return err
			}
		}
	}
	return nil
}

// exists says whether path exists, without following a symbolic link.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
