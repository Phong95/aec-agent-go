package artifact

import "slices"

// Clone returns a deep copy: the Tags slice is copied (nil stays nil), so a
// caller and a store never share a mutable part of an artifact head record. Add a
// line here when a pointer, slice or map field is added to Artifact; the
// clonecheck test fails until you do.
func (a Artifact) Clone() Artifact {
	a.Tags = slices.Clone(a.Tags)
	return a
}

// Clone returns a copy of the version. Version and Origin hold only values today,
// so the copy shares nothing; add a line here when a pointer, slice or map field
// is added to Version or Origin, and the clonecheck test fails until you do.
func (v Version) Clone() Version {
	return v
}
