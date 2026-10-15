package secret

import (
	"maps"
	"slices"
)

// Clone returns a deep copy (the lists are copied).
// Add a line here when a pointer, slice or map field is added to Info.
func (i Info) Clone() Info {
	i.AllowedHosts = slices.Clone(i.AllowedHosts)
	i.AllowedTools = slices.Clone(i.AllowedTools)
	return i
}

// Clone returns a deep copy (the lists are copied).
func (s Stored) Clone() Stored {
	s.Info = s.Info.Clone()
	return s
}

// Clone returns a deep copy (the lists are copied).
func (a AgentInfo) Clone() AgentInfo {
	a.Info = a.Info.Clone()
	return a
}

// Clone returns a deep copy (the description and the lists are copied).
func (d Definition) Clone() Definition {
	if d.Description != nil {
		v := *d.Description
		d.Description = &v
	}
	d.AllowedHosts = slices.Clone(d.AllowedHosts)
	d.AllowedTools = slices.Clone(d.AllowedTools)
	return d
}

// Clone returns a deep copy (the map is copied).
func (s Status) Clone() Status {
	s.ByKeyID = maps.Clone(s.ByKeyID)
	return s
}

// Clone returns a deep copy (the Use slice is copied); a nil config clones to nil.
// Add a line here when a pointer, slice or map field is added to Config.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Use = slices.Clone(c.Use)
	return &out
}
