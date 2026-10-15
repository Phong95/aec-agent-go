package skill_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// skillWire mirrors the object skill-wire.json holds, fields in fixture order.
// package is a SkillPackage; skillApp is the Skill SkillHub.register returned
// for source app; skillCustom is the Skill the hub's create path returned.
type skillWire struct {
	Package     skill.Package `json:"package"`
	SkillApp    skill.Skill   `json:"skillApp"`
	SkillCustom skill.Skill   `json:"skillCustom"`
}

func TestSkillWireFixture(t *testing.T) {
	var w skillWire
	wiretest.AssertRoundTrip(t, "skill-wire.json", &w)

	if w.SkillApp.Source != skill.SourceApp || w.SkillCustom.Source != skill.SourceCustom {
		t.Errorf("sources = %q, %q", w.SkillApp.Source, w.SkillCustom.Source)
	}
	if !slices.Equal(w.SkillApp.AllowedTools, []string{"read_file", "run_command"}) {
		t.Errorf("allowedTools = %v", w.SkillApp.AllowedTools)
	}
	if v, _ := w.SkillApp.Metadata.Get("requires-tools"); v != "read_file run_command" {
		t.Errorf("metadata requires-tools = %q", v)
	}
	if v, _ := w.SkillApp.Metadata.Get("version"); v != "1.0" {
		t.Errorf("metadata version = %q", v)
	}
	// Metadata keeps the parsed key order (an ordered.Map), not sorted order.
	if got := w.SkillApp.Metadata.Keys(); !slices.Equal(got, []string{"author", "requires-tools", "version"}) {
		t.Errorf("metadata keys = %v", got)
	}
	if !strings.Contains(w.SkillApp.Body, "<schedule> & sum") {
		t.Errorf("body lost its raw <>&: %q", w.SkillApp.Body)
	}
	if w.SkillApp.CreatedAt != "" || w.SkillApp.CreatedBy != "" {
		t.Error("a registered skill has no createdAt or createdBy")
	}
	if w.SkillCustom.CreatedBy != "agent-1" || w.SkillCustom.CreatedAt == "" || w.SkillCustom.UpdatedAt == "" {
		t.Errorf("custom skill provenance lost: %+v", w.SkillCustom)
	}
	if w.SkillCustom.Files == nil || len(w.SkillCustom.Files) != 0 || w.SkillCustom.Metadata.Len() != 0 {
		t.Error("explicit empty files and metadata must decode as empty, non-nil files")
	}
	if got := w.SkillApp.Frontmatter(); got.Name != "quantity-takeoff" || got.License != "MIT" || len(got.AllowedTools) != 2 {
		t.Errorf("Frontmatter() = %+v", got)
	}
}

// skillFixtures is every golden fixture owned by package skill.
var skillFixtures = []string{
	"skill-wire.json",
	"skill-parse-corpus.json",
	"skill-yaml-corpus.json",
	"skill-mutation-corpus.json",
}

// TestSkillFixturesCovered fails when a fixture with the skill- prefix has no
// entry above, and when an entry has no file, so a fixture added to wiregen
// cannot go unchecked.
func TestSkillFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "skill-") && !slices.Contains(skillFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in skillFixtures", e.Name())
		}
	}
	for _, f := range skillFixtures {
		if !present[f] {
			t.Errorf("skillFixtures entry %s has no fixture file", f)
		}
	}
}

func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := jsonx.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return string(b)
}

func TestSkillNilCollections(t *testing.T) {
	got := encode(t, skill.Skill{Name: "n", Description: "d", Source: skill.SourceBuiltIn, Version: 1})
	want := `{"name":"n","description":"d","metadata":{},"body":"","content":"","files":[],"requiresTools":[],"source":"built-in","version":1}`
	if got != want {
		t.Errorf("nil collections in Skill:\n got %s\nwant %s", got, want)
	}

	if got := encode(t, skill.Frontmatter{Name: "n", Description: "d"}); got != `{"name":"n","description":"d","metadata":{}}` {
		t.Errorf("nil metadata in Frontmatter: %s", got)
	}
	// A pointer encodes the same way as a value.
	if got := encode(t, &skill.Frontmatter{Name: "n", Description: "d"}); got != `{"name":"n","description":"d","metadata":{}}` {
		t.Errorf("pointer to Frontmatter: %s", got)
	}

	// Optional frontmatter keys are omitted when unset and kept when set.
	set := skill.Frontmatter{
		Name: "n", Description: "d", License: "MIT", Compatibility: "c",
		AllowedTools: []string{"a"},
	}
	set.Metadata.Set("k", "v")
	if got := encode(t, set); got != `{"name":"n","description":"d","license":"MIT","compatibility":"c","allowedTools":["a"],"metadata":{"k":"v"}}` {
		t.Errorf("set frontmatter: %s", got)
	}

	// A package without files omits the key; an explicit empty list keeps it.
	if got := encode(t, skill.Package{Content: "c"}); got != `{"content":"c"}` {
		t.Errorf("package without files: %s", got)
	}
	if got := encode(t, skill.Package{Content: "c", Files: []skill.File{}}); got != `{"content":"c","files":[]}` {
		t.Errorf("package with empty files: %s", got)
	}

	// Encoding does not write the normalised values back into the caller's value.
	s := skill.Skill{Name: "n", Description: "d"}
	_ = encode(t, s)
	if s.Metadata.Len() != 0 || s.Files != nil || s.RequiresTools != nil {
		t.Error("encoding must not change the caller's value")
	}
}

func TestSkillDecodesStrictly(t *testing.T) {
	var s skill.Skill
	err := jsonx.UnmarshalStrict([]byte(`{"name":"n","description":"d","metadata":{},"body":"","content":"","files":[],"requiresTools":[],"source":"app","version":1,"extra":1}`), &s)
	if err == nil {
		t.Error("an unknown key must be rejected")
	}
}
