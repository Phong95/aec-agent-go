package persistence

import (
	"context"
	"slices"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// ---------------------------------------------------------------------------
// Skill store - persistence for custom skills (created at runtime by agents or
// users). Built-in and app skills are registered in code and never stored.
// The SkillHub validates before saving; stores keep records as given.
// ---------------------------------------------------------------------------

// SkillRecord is a stored custom skill. The fields are in wire order.
type SkillRecord struct {
	// Name is the unique skill name - the SKILL.md frontmatter name.
	Name string `json:"name"`
	// Content is the full SKILL.md text.
	Content string `json:"content"`
	// Files are the skill's other files. Required on the wire: nil encodes as [].
	Files []skill.File `json:"files"`
	// CreatedBy is the agent that created it, when an agent did. An empty value is
	// omitted.
	CreatedBy string `json:"createdBy,omitempty"`
	Version   int    `json:"version"`
	// CreatedAt and UpdatedAt are ISO timestamps.
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// MarshalJSON writes a nil Files as [] because the field is required. It
// takes a value receiver so a SkillRecord held by value encodes correctly, and it
// never writes the normalised slice back into the caller's value.
func (r SkillRecord) MarshalJSON() ([]byte, error) {
	type plain SkillRecord // drops methods, avoids recursion
	if r.Files == nil {
		r.Files = []skill.File{}
	}
	return jsonx.Marshal(plain(r))
}

// Clone returns a deep copy: the Files slice is copied (nil stays nil), so a
// caller and a store never share a mutable part of a skill record. Add a line
// here when a pointer, slice or map field is added to SkillRecord or skill.File;
// the clonecheck test fails until you do.
func (r SkillRecord) Clone() SkillRecord {
	r.Files = slices.Clone(r.Files)
	return r
}

// SkillStore persists custom skills.
type SkillStore interface {
	// Save inserts or replaces by name.
	Save(ctx context.Context, r SkillRecord) error
	// Load returns the record, or a nil pointer and a nil error when the name is
	// unknown.
	Load(ctx context.Context, name string) (*SkillRecord, error)
	List(ctx context.Context) ([]SkillRecord, error)
	Delete(ctx context.Context, name string) error
}
