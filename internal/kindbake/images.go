package kindbake

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// imageRef is an image reference, split into its parts.
type imageRef struct {
	// Raw is the reference as it was written.
	Raw string
	// Name is the repository with its registry, as Docker Engine and
	// containerd name it: docker.io/library/nginx for nginx.
	Name string
	// Tag and Digest are empty when the reference has none.
	Tag    string
	Digest string
}

var (
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	pathRe   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
)

// parseRef reads an image reference: [REGISTRY/]PATH[:TAG][@sha256:DIGEST].
// A reference with no registry is on docker.io, and one with a single path
// component on docker.io is under library/.
func parseRef(s string) (imageRef, error) {
	r := imageRef{Raw: s}
	rest := s
	if name, digest, ok := strings.Cut(rest, "@"); ok {
		if !digestRe.MatchString(digest) {
			return r, fmt.Errorf("image %q: %q is not sha256:<64 hexadecimal digits>", s, digest)
		}
		r.Digest, rest = digest, name
	}
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		r.Tag, rest = rest[i+1:], rest[:i]
		if !tagRe.MatchString(r.Tag) {
			return r, fmt.Errorf("image %q: %q is not a tag", s, r.Tag)
		}
	}
	domain, path := "docker.io", rest
	if first, after, ok := strings.Cut(rest, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		domain, path = first, after
	}
	if domain == "index.docker.io" {
		domain = "docker.io"
	}
	if domain == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if !pathRe.MatchString(path) {
		return r, fmt.Errorf("image %q is not an image reference", s)
	}
	r.Name = domain + "/" + path
	return r, nil
}

// pullRef is the reference to copy the image from: by digest when there
// is one, since skopeo takes no reference with both a tag and a digest.
func (r imageRef) pullRef() string {
	switch {
	case r.Digest != "":
		return r.Name + "@" + r.Digest
	case r.Tag != "":
		return r.Name + ":" + r.Tag
	}
	return r.Name + ":latest"
}

// archiveTag is the name a docker-archive gives the image, NAME:TAG, or
// empty for a reference by digest alone. A reference with neither is
// NAME:latest, as a pull takes it.
func (r imageRef) archiveTag() string {
	switch {
	case r.Tag != "":
		return r.Name + ":" + r.Tag
	case r.Digest != "":
		return ""
	}
	return r.Name + ":latest"
}

// covers says whether the archive of r serves a Pod that names o: the
// same name, and the same tag or the same digest.
func (r imageRef) covers(o imageRef) bool {
	if r.Name != o.Name {
		return false
	}
	if o.Digest != "" && r.Digest == o.Digest {
		return true
	}
	otag := o.Tag
	if otag == "" && o.Digest == "" {
		otag = "latest"
	}
	return otag != "" && r.Tag == otag
}

// containerKeys are the fields of a Pod spec that list containers.
var containerKeys = []string{"containers", "initContainers", "ephemeralContainers"}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//# `flr-kind bake` SHALL find the image of each container, init container
//# and ephemeral container in each manifest, in the output of `kustomize
//# build` for each kustomize directory, and in the output of `helm template`
//# for each chart with its values.

// findImages returns the image of every container, init container and
// ephemeral container in the YAML documents of data, each once, in byte
// order. It looks in every map, so that it finds the Pod spec of any kind
// of object: a Pod, a Deployment's template, a CronJob's job template, or
// a custom resource that embeds one.
func findImages(data []byte) ([]string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var found []string
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the YAML: %w", err)
		}
		walk(doc, &found)
	}
	slices.Sort(found)
	return slices.Compact(found), nil
}

func walk(v any, found *[]string) {
	switch v := v.(type) {
	case map[string]any:
		for _, key := range containerKeys {
			list, ok := v[key].([]any)
			if !ok {
				continue
			}
			for _, c := range list {
				if m, ok := c.(map[string]any); ok {
					if img, ok := m["image"].(string); ok && strings.TrimSpace(img) != "" {
						*found = append(*found, strings.TrimSpace(img))
					}
				}
			}
		}
		for _, child := range v {
			walk(child, found)
		}
	case []any:
		for _, child := range v {
			walk(child, found)
		}
	}
}
