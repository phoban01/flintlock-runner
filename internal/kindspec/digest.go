package kindspec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Files returns the files that go into the spec's digest, each relative to
// the repository root with forward slashes, sorted by byte order and each
// once: the spec itself, the kindConfig file when kindConfig is a path,
// each manifest path, every file under each kustomize directory and each
// Helm values file.
func (s *Spec) Files() ([]string, error) {
	_, specRel, err := s.resolveFromRoot(s.file)
	if err != nil {
		return nil, fmt.Errorf("the spec: %w", err)
	}
	files := []string{specRel}
	add := func(field, p string) error {
		_, rel, err := s.resolve(p)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		files = append(files, rel)
		return nil
	}
	if s.KindConfig != "" && !s.KindConfigInline() {
		if err := add("kindConfig", s.KindConfig); err != nil {
			return nil, err
		}
	}
	for i := range s.Manifests {
		m := &s.Manifests[i]
		field := fmt.Sprintf("manifests[%d]", i)
		if m.Path != "" {
			if err := add(field+".path", m.Path); err != nil {
				return nil, err
			}
		}
		if m.Kustomize != "" {
			abs, _, err := s.resolve(m.Kustomize)
			if err != nil {
				return nil, fmt.Errorf("%s.kustomize: %w", field, err)
			}
			under, err := s.walk(abs)
			if err != nil {
				return nil, fmt.Errorf("%s.kustomize: %w", field, err)
			}
			files = append(files, under...)
		}
		if m.Helm != nil {
			for j, v := range m.Helm.Values {
				if err := add(fmt.Sprintf("%s.helm.values[%d]", field, j), v); err != nil {
					return nil, err
				}
			}
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

//= docs/requirements/13-guest-image.md#guest-kind-spec
//# `flr-kind digest` SHALL print `sha256:` and the sha256 of a list with one
//# line for the spec and for each file that the spec names by path, the
//# files under its kustomize directories included, where each line holds
//# the file's sha256 and its path relative to the repository root, and the
//# lines are in the byte order of the paths.

// Digest returns the spec's digest as sha256:<64 hexadecimal digits>. It is
// the sha256 of one line for each of [Spec.Files], in that order, as
// sha256sum prints it: the sha256 of the file's content in hexadecimal, two
// spaces, the path and a line feed. From the repository root,
//
//	sha256sum FILE... | sha256sum
//
// with the files in that order gives the same digest. The paths are
// relative to the root, so the digest does not depend on where the
// repository is, and a file that moves changes it.
func (s *Spec) Digest() (string, error) {
	files, err := s.Files()
	if err != nil {
		return "", err
	}
	list := sha256.New()
	for _, rel := range files {
		sum, err := fileSHA256(filepath.Join(s.root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(list, "%s  %s\n", sum, rel)
	}
	return "sha256:" + hex.EncodeToString(list.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolveFromRoot is resolve for an absolute path, such as the spec's own.
func (s *Spec) resolveFromRoot(abs string) (string, string, error) {
	rel, err := filepath.Rel(s.Dir(), abs)
	if err != nil {
		return "", "", err
	}
	return s.resolve(rel)
}

// walk returns every file under the directory dir, relative to the root
// with forward slashes. A symbolic link to a regular file inside the root
// counts as a file, under the link's own name. It refuses a symbolic link
// to a directory or out of the root, and anything that is neither a file
// nor a directory, so that what kustomize reads and what the digest covers
// stay the same.
func (s *Spec) walk(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := relInside(s.root, path)
		if err != nil {
			return fmt.Errorf("%s %w %s", path, err, s.root)
		}
		if strings.ContainsAny(rel, "\n\\") {
			return fmt.Errorf("%q has a line break or a backslash", rel)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if err := s.inRootResolved(path); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				return err
			}
			if !fi.Mode().IsRegular() {
				return fmt.Errorf("%s is a symbolic link to something other than a regular file", rel)
			}
		} else if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}
