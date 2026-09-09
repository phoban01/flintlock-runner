package scheduler

import "path"

//= docs/requirements/03-scheduler.md#profile-resolution
//# The Scheduler SHALL resolve a Profile for a Job by matching the
//# Job Image name against Profile image names exactly, then against Profile
//# image glob patterns in configuration order, then by falling back to the
//# Default Profile.

//= docs/requirements/03-scheduler.md#profile-resolution
//# If a Job has no Job Image and no Default Profile is configured,
//# then the Scheduler SHALL report that no Profile can be resolved.

//= docs/requirements/03-scheduler.md#profile-resolution
//# If a Job Image matches no Profile and the configuration does not
//# allow falling back to the Default Profile for unknown images, then the
//# Scheduler SHALL report that no Profile can be resolved.

//= docs/requirements/03-scheduler.md#profile-resolution
//# The Scheduler SHALL resolve the Profile before claiming any
//# MicroVM so that a Job with an unknown image fails without consuming a warm
//# MicroVM.

// ResolveProfile implements ProfileResolver. It is a pure function of the
// Job and the configured Profiles: it makes no call to the Pool Manager and
// none to a Host, and Allocate takes the resolved *Profile as an argument, so
// a Job whose image resolves to nothing fails here, before any MicroVM has
// been claimed and with no warm MicroVM consumed.
func (s *impl) ResolveProfile(job JobInfo) (*Profile, error) {
	profiles := s.profileSnapshot()

	if job.Image != "" {
		for _, p := range profiles {
			for _, name := range p.Images {
				if name == job.Image {
					return p, nil
				}
			}
		}
		for _, p := range profiles {
			for _, pattern := range p.ImageGlobs {
				if ok, err := path.Match(pattern, job.Image); err == nil && ok {
					return p, nil
				}
			}
		}
		if !s.set.Scheduler.AllowDefaultForUnknownImages {
			return nil, &ProfileError{Image: job.Image}
		}
	}

	if def := defaultProfile(profiles); def != nil {
		return def, nil
	}
	return nil, &ProfileError{Image: job.Image}
}

// defaultProfile returns the Default Profile, or nil when none is marked
// (CF-022).
func defaultProfile(profiles []*Profile) *Profile {
	for _, p := range profiles {
		if p.Default {
			return p
		}
	}
	return nil
}
