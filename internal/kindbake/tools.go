package kindbake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Tools are the programs and the network that a bake uses. Each is a small
// interface, so that a test can give fakes and run with no network and none
// of the programs.
type Tools struct {
	Registry    Registry
	Fetcher     Fetcher
	Kustomize   Kustomize
	Helm        Helm
	DockerStore DockerStore
}

// Registry reads images from container registries.
type Registry interface {
	// Manifest returns the manifest or image index that ref names, as the
	// registry sends it. Its sha256 is the digest that ref resolves to.
	Manifest(ctx context.Context, ref string) ([]byte, error)
	// Archive writes the image that ref names, for linux and arch, to path
	// as a docker-archive. tag, NAME:TAG, is the name the archive gives the
	// image; empty means none.
	Archive(ctx context.Context, ref, arch, path, tag string) error
}

// Fetcher downloads a file.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Kustomize renders a kustomize directory.
type Kustomize interface {
	Build(ctx context.Context, dir string) ([]byte, error)
}

// Helm pulls and renders charts.
type Helm interface {
	// Pull downloads the chart's archive into the empty directory dir and
	// returns its path. args are the chart's arguments to helm pull: the
	// chart, and --repo URL for a chart from a chart repository.
	Pull(ctx context.Context, args []string, version, dir string) (string, error)
	// Template renders the chart's archive as helm install would install
	// it, with the values files in order.
	Template(ctx context.Context, archive, release, namespace string, values []string) ([]byte, error)
}

// DockerStore writes a docker-archive into Docker Engine's image store,
// as guest/rootfs-kind/docker-store.sh does: the image under ref,
// NAME:TAG@sha256:DIGEST, in the directory root that becomes
// /var/lib/docker.
type DockerStore interface {
	Store(ctx context.Context, archive, ref, root string) error
}

// DefaultDockerStore is where the guest-baker image has docker-store.sh.
const DefaultDockerStore = "/usr/local/libexec/flr/docker-store.sh"

// ExecTools returns the Tools that run skopeo, kustomize, helm and the
// docker-store.sh at script from PATH, and download over HTTP.
func ExecTools(script string) Tools {
	return Tools{
		Registry:    skopeo{},
		Fetcher:     httpFetcher{client: &http.Client{Timeout: 10 * time.Minute}},
		Kustomize:   kustomizeCmd{},
		Helm:        helmCmd{},
		DockerStore: dockerStoreCmd{script: script},
	}
}

// run runs a program and returns what it writes to stdout. Its stderr is
// in the error when it fails.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

type skopeo struct{}

// Manifest runs skopeo inspect --raw.
func (skopeo) Manifest(ctx context.Context, ref string) ([]byte, error) {
	return run(ctx, "skopeo", "inspect", "--raw", "docker://"+ref)
}

// Archive runs skopeo copy to a docker-archive.
func (skopeo) Archive(ctx context.Context, ref, arch, path, tag string) error {
	dest := "docker-archive:" + path
	if tag != "" {
		dest += ":" + tag
	}
	_, err := run(ctx, "skopeo", "copy", "--quiet", "--override-os", "linux", "--override-arch", arch, "docker://"+ref, dest)
	return err
}

type httpFetcher struct{ client *http.Client }

// Fetch downloads url with a GET, which has to answer 200.
func (f httpFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

type kustomizeCmd struct{}

// Build runs kustomize build.
func (kustomizeCmd) Build(ctx context.Context, dir string) ([]byte, error) {
	return run(ctx, "kustomize", "build", dir)
}

type helmCmd struct{}

// Pull runs helm pull.
func (helmCmd) Pull(ctx context.Context, args []string, version, dir string) (string, error) {
	a := append([]string{"pull"}, args...)
	a = append(a, "--version", version, "--destination", dir)
	if _, err := run(ctx, "helm", a...); err != nil {
		return "", err
	}
	found, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	if err != nil {
		return "", err
	}
	if len(found) != 1 {
		return "", fmt.Errorf("helm pull wrote %d chart archives, not one", len(found))
	}
	return found[0], nil
}

// Template runs helm template, without the chart's tests.
func (helmCmd) Template(ctx context.Context, archive, release, namespace string, values []string) ([]byte, error) {
	// A chart's test hooks run only with helm test, so their images are
	// left out.
	a := []string{"template", release, archive, "--skip-tests"}
	if namespace != "" {
		a = append(a, "--namespace", namespace)
	}
	for _, v := range values {
		a = append(a, "--values", v)
	}
	return run(ctx, "helm", a...)
}

type dockerStoreCmd struct{ script string }

// Store runs docker-store.sh with bash.
func (d dockerStoreCmd) Store(ctx context.Context, archive, ref, root string) error {
	if _, err := os.Stat(d.script); err != nil {
		return errors.Join(fmt.Errorf("docker-store.sh is not at %s; give its path with -docker-store", d.script), err)
	}
	_, err := run(ctx, "bash", d.script, archive, ref, root)
	return err
}
