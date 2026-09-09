package config

import (
	"fmt"
	"strings"
)

// ImageRef is the parsed form of an OCI image reference as far as this
// package needs it: whether the reference pins a digest, a tag, or neither.
type ImageRef struct {
	// Repository is the reference without tag and digest.
	Repository string
	Tag        string
	Digest     string
}

// Pinned reports whether the reference names a digest or a tag (CF-026).
func (r ImageRef) Pinned() bool { return r.Digest != "" || r.Tag != "" }

// ParseImageRef splits ref into repository, tag and digest. It accepts the
// docker reference grammar loosely: an optional "@<algo>:<hex>" digest suffix
// and an optional ":<tag>" after the last path separator, so that registry
// ports ("localhost:5000/img") are not mistaken for tags.
func ParseImageRef(ref string) (ImageRef, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ImageRef{}, fmt.Errorf("empty image reference")
	}
	out := ImageRef{Repository: ref}
	if at := strings.LastIndex(out.Repository, "@"); at >= 0 {
		out.Digest = out.Repository[at+1:]
		out.Repository = out.Repository[:at]
		algo, hex, ok := strings.Cut(out.Digest, ":")
		if !ok || algo == "" || hex == "" {
			return ImageRef{}, fmt.Errorf("image reference %q has a malformed digest", ref)
		}
	}
	lastSlash := strings.LastIndex(out.Repository, "/")
	if colon := strings.LastIndex(out.Repository, ":"); colon > lastSlash {
		out.Tag = out.Repository[colon+1:]
		out.Repository = out.Repository[:colon]
		if out.Tag == "" {
			return ImageRef{}, fmt.Errorf("image reference %q has an empty tag", ref)
		}
	}
	if out.Repository == "" {
		return ImageRef{}, fmt.Errorf("image reference %q has no repository", ref)
	}
	return out, nil
}

// profileImage is one image reference of a Profile with the field path it
// came from, for validation (CF-026) and warnings (CF-027).
type profileImage struct {
	field string
	ref   string
}

// images lists every image reference a Profile declares.
func (p *Profile) images(prefix string) []profileImage {
	out := []profileImage{
		{prefix + ".kernel.image", p.Kernel.Image},
		{prefix + ".rootfs", p.RootFS},
	}
	if p.Initrd != nil {
		out = append(out, profileImage{prefix + ".initrd.image", p.Initrd.Image})
	}
	for i, v := range p.AdditionalVolumes {
		out = append(out, profileImage{fmt.Sprintf("%s.additional_volumes[%d].image", prefix, i), v.Image})
	}
	return out
}

// ImageWarning is a Profile image specified by tag rather than digest
// (CF-027).
type ImageWarning struct {
	Profile string
	Field   string
	Image   string
}

// String renders the warning the way Load logs it.
func (w ImageWarning) String() string {
	return fmt.Sprintf("profile %q: image %s %q is specified by tag rather than digest; pin a digest for reproducible MicroVMs", w.Profile, w.Field, w.Image)
}

//= docs/requirements/07-configuration.md#profiles-section
//# Where a Profile image is specified by tag rather than by digest,
//# the Runner SHALL log a warning at startup naming the Profile and the image.

// Warnings returns one ImageWarning per Profile image that carries a tag but
// no digest. Load logs each at warn level; callers that build a Config
// without Load (the Fleet Controller) may log them the same way. The Config
// is expected to have passed Validate, so unparsable references are skipped.
func Warnings(c *Config) []ImageWarning {
	var out []ImageWarning
	for i := range c.Profiles {
		p := &c.Profiles[i]
		for _, img := range p.images(fmt.Sprintf("profiles[%d]", i)) {
			ref, err := ParseImageRef(img.ref)
			if err != nil || ref.Digest != "" || ref.Tag == "" {
				continue
			}
			out = append(out, ImageWarning{Profile: p.Name, Field: img.field, Image: img.ref})
		}
	}
	return out
}
