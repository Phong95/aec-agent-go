package sandboxhub

import "slices"

// Clone returns a deep copy: every pointer is copied, so the copy shares nothing
// with the original. Add a line here when a pointer field is added to
// LimitsOverride; the clonecheck test fails until you do.
func (l LimitsOverride) Clone() LimitsOverride {
	if l.Cpus != nil {
		v := *l.Cpus
		l.Cpus = &v
	}
	if l.MemoryMb != nil {
		v := *l.MemoryMb
		l.MemoryMb = &v
	}
	if l.Pids != nil {
		v := *l.Pids
		l.Pids = &v
	}
	return l
}

// Clone returns a deep copy: the Limits pointer and its fields are copied. Add a
// line here when a pointer or slice field is added to OwnConfig.
func (c OwnConfig) Clone() OwnConfig {
	if c.Limits != nil {
		l := c.Limits.Clone()
		c.Limits = &l
	}
	return c
}

// Clone returns a deep copy: the Own config pointer and everything behind it are
// copied (a nil Own stays nil), so a profile and a store never share a mutable
// part of a selection.
func (s Selection) Clone() Selection {
	if s.Own != nil {
		o := s.Own.Clone()
		s.Own = &o
	}
	return s
}

// Clone returns a deep copy: the Limits pointer and Options are copied (a nil
// Limits stays nil and an absent Options stays absent), so a caller and a hub
// never share a mutable part of a spec. Add a line here when a pointer, slice or
// map field is added to Spec; the clonecheck test fails until you do.
func (s Spec) Clone() Spec {
	if s.Limits != nil {
		l := s.Limits.Clone()
		s.Limits = &l
	}
	s.Options = s.Options.Clone()
	return s
}

// Clone returns a deep copy: the three slices are copied (a nil slice stays nil
// and an empty one stays empty), so a caller and a hub never share a mutable part
// of a policy. Add a line here when a pointer, slice or map field is added to
// HubPolicy; the clonecheck test fails until you do.
func (p HubPolicy) Clone() HubPolicy {
	p.AllowedImages = slices.Clone(p.AllowedImages)
	p.AllowedNetwork = slices.Clone(p.AllowedNetwork)
	p.OwnKinds = slices.Clone(p.OwnKinds)
	return p
}
