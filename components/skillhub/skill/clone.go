package skill

import "slices"

// Clone returns a deep copy of the skill: AllowedTools, Metadata, Files and
// RequiresTools are copied (a nil slice stays nil), so a caller and a hub never
// share a mutable part of a skill. Add a line here when a pointer, slice or map
// field is added to Skill or File; the clonecheck test fails until you do.
func (s Skill) Clone() Skill {
	s.AllowedTools = slices.Clone(s.AllowedTools)
	s.Metadata = s.Metadata.Clone()
	s.Files = slices.Clone(s.Files)
	s.RequiresTools = slices.Clone(s.RequiresTools)
	return s
}
