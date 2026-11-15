package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const goodSpec = `apiVersion: flintlock-runner.dev/v1alpha1
kind: KindCluster
name: my-e2e
manifests:
  - path: ../deploy/thing.yaml
`

const thing = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: thing\n"

// repo writes a repository with a spec and the one manifest it names, and
// returns the spec's path.
func repo(t *testing.T, spec string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{
		".flr/kind-cluster.yaml": spec,
		"deploy/thing.yaml":      thing,
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, ".flr", "kind-cluster.yaml")
}

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = command(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCheck(t *testing.T) {
	if code, out, errOut := run("check", repo(t, goodSpec)); code != 0 || out != "" || errOut != "" {
		t.Fatalf("check on a good spec: %d, %q, %q", code, out, errOut)
	}

	bad := strings.Replace(goodSpec, "  - path: ../deploy/thing.yaml\n", "  - helm:\n      chart: c\n      version: v\n      release: r\nimages: [nginx:1.27]\n", 1)
	code, out, errOut := run("check", repo(t, bad))
	if code != 1 || out != "" {
		t.Fatalf("check on a bad spec: %d, %q; want 1 and no output", code, out)
	}
	for _, want := range []string{"is not a valid kind cluster spec", "\n  manifests[0].helm.digest: ", "\n  images[0]: "} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q does not have %q", errOut, want)
		}
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bake"}, {"check"}, {"digest", "a", "b"}, {"check", "-x", "a"}} {
		if code, _, errOut := run(args...); code != 2 || errOut == "" {
			t.Errorf("flr-kind %q: %d, %q; want 2 and a message", args, code, errOut)
		}
	}
	if code, _, errOut := run("check", filepath.Join(t.TempDir(), "nope.yaml")); code != 1 || !strings.Contains(errOut, "nope.yaml") {
		t.Errorf("check on a missing file: %d, %q; want 1 and the file's name", code, errOut)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//= type=test
//# `flr-kind digest` SHALL print `sha256:` and the sha256 of a list with one
//# line for the spec and for each file that the spec names by path, the
//# files under its kustomize directories included, where each line holds
//# the file's sha256 and its path relative to the repository root, and the
//# lines are in the byte order of the paths.

func TestDigest(t *testing.T) {
	line := func(content, path string) string {
		sum := sha256.Sum256([]byte(content))
		return fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), path)
	}
	list := line(goodSpec, ".flr/kind-cluster.yaml") + line(thing, "deploy/thing.yaml")
	sum := sha256.Sum256([]byte(list))
	want := "sha256:" + hex.EncodeToString(sum[:]) + "\n"

	spec := repo(t, goodSpec)
	code, out, errOut := run("digest", spec)
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("digest: %d, %q, %q; want 0 and %q", code, out, errOut, want)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}\n$`).MatchString(out) {
		t.Fatalf("digest printed %q", out)
	}

	// -root names the root that the default gives, so the digest is the
	// same; the spec's directory as the root refuses the manifest's path.
	root := filepath.Dir(filepath.Dir(spec))
	if code, out, _ := run("digest", "-root", root, spec); code != 0 || out != want {
		t.Fatalf("digest -root: %d, %q; want %q", code, out, want)
	}
	if code, _, errOut := run("digest", "-root", filepath.Dir(spec), spec); code != 1 || !strings.Contains(errOut, "manifests[0].path") {
		t.Fatalf("digest -root with the spec's directory: %d, %q; want 1 and manifests[0].path", code, errOut)
	}

	if code, out, _ := run("digest", repo(t, "name: x\n")); code != 1 || out != "" {
		t.Fatalf("digest on a bad spec: %d, %q; want 1 and no digest", code, out)
	}
}
