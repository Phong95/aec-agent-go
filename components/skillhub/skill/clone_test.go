package skill_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

func TestSkillCloneIsDeep(t *testing.T) {
	var meta ordered.Map[string]
	meta.Set("author", "aec")
	meta.Set("requires-tools", "read_file")
	sample := skill.Skill{
		Name:          "demo",
		Description:   "A demo skill.",
		License:       "MIT",
		Compatibility: "Needs read_file",
		AllowedTools:  []string{"read_file"},
		Metadata:      meta,
		Body:          "Do the thing.",
		Content:       "---\nname: demo\n---\nDo the thing.\n",
		Files:         []skill.File{{Path: "references/a.md", Content: "a"}},
		RequiresTools: []string{"read_file"},
		Source:        skill.SourceCustom,
		Version:       2,
		CreatedAt:     "2023-11-14T22:13:20.000Z",
		UpdatedAt:     "2023-11-14T22:13:21.000Z",
		CreatedBy:     "agent-1",
	}
	clonecheck.AssertDeep(t, sample, skill.Skill.Clone)
}

// Frontmatter() hands out a copy: changing its Metadata or AllowedTools must not
// reach the skill it came from (WR-05). Before the fix the copy shared the
// Metadata map and the AllowedTools backing array with the skill.
func TestFrontmatterSharesNothingWithTheSkill(t *testing.T) {
	var meta ordered.Map[string]
	meta.Set("author", "aec")
	sk := skill.Skill{
		Name:         "demo",
		Description:  "A demo skill.",
		AllowedTools: []string{"read_file"},
		Metadata:     meta,
	}
	fm := sk.Frontmatter()
	fm.Metadata.Set("zz", "1")
	fm.Metadata.Set("author", "someone-else")
	fm.AllowedTools[0] = "rm"

	if sk.Metadata.Len() != 1 || sk.Metadata.Has("zz") {
		t.Fatalf("a Set on the frontmatter reached the skill: keys %v, has zz %v", sk.Metadata.Keys(), sk.Metadata.Has("zz"))
	}
	if v, _ := sk.Metadata.Get("author"); v != "aec" {
		t.Fatalf("an overwrite on the frontmatter reached the skill: author = %q", v)
	}
	if sk.AllowedTools[0] != "read_file" {
		t.Fatalf("a write on the frontmatter AllowedTools reached the skill: %v", sk.AllowedTools)
	}

	// An empty skill stays empty: nil AllowedTools must stay nil (omitzero).
	empty := skill.Skill{Name: "e", Description: "d"}.Frontmatter()
	if empty.AllowedTools != nil {
		t.Fatalf("nil AllowedTools became %#v", empty.AllowedTools)
	}
}
