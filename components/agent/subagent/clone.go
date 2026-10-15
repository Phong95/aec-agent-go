package subagent

import "slices"

// Clone returns a deep copy (the Allow slice is copied); a nil config clones to nil.
// (Update (A-105 revision): the Types slice is copied too.)
// Add a line here when a pointer, slice or map field is added to Config.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Allow = slices.Clone(c.Allow)
	out.Types = slices.Clone(c.Types)
	return &out
}

// Clone returns a deep copy of the request (its context object is copied).
func (r Request) Clone() Request {
	r.Context = r.Context.Clone()
	return r
}
