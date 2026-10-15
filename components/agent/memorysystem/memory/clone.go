package memory

import "slices"

// Clone returns a deep copy: the Tags slice is copied (nil stays nil), so a
// caller and a store never share a mutable part of a record. Add a line here when
// a pointer, slice or map field is added to Record; the clonecheck test fails
// until you do.
func (r Record) Clone() Record {
	r.Tags = slices.Clone(r.Tags)
	return r
}

// Clone returns a deep copy: every pointer sub-config is copied, so the copy
// shares nothing with the original. Add a line here when a pointer or slice field
// is added to Config or its sub-configs; the clonecheck test fails until you do.
func (c Config) Clone() Config {
	if c.Consolidation != nil {
		t := *c.Consolidation
		if t.IntervalMinutes != nil {
			v := *t.IntervalMinutes
			t.IntervalMinutes = &v
		}
		c.Consolidation = &t
	}
	if c.Dreaming != nil {
		d := *c.Dreaming
		c.Dreaming = &d
	}
	if c.Recall != nil {
		r := *c.Recall
		if r.MaxResults != nil {
			v := *r.MaxResults
			r.MaxResults = &v
		}
		c.Recall = &r
	}
	return c
}

// Clone returns a deep copy: the content pointer, both slices and the two optional
// numbers are copied (nil stays nil), so a caller and the loop never share a
// mutable part of a recall result. Add a line here when a pointer or slice field is
// added to RecalledMemories; the clonecheck test fails until you do.
func (r RecalledMemories) Clone() RecalledMemories {
	if r.Content != nil {
		c := *r.Content
		r.Content = &c
	}
	r.RecordIDs = slices.Clone(r.RecordIDs)
	r.Memories = slices.Clone(r.Memories)
	if r.IndexRebuildMs != nil {
		v := *r.IndexRebuildMs
		r.IndexRebuildMs = &v
	}
	if r.IndexedCount != nil {
		v := *r.IndexedCount
		r.IndexedCount = &v
	}
	return r
}
