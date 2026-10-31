package skillhub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// essentialFixture mirrors essential-skills.json: the recorded essential skills and
// their names.
type essentialFixture struct {
	Skills []skill.Package `json:"skills"`
	Names  []string        `json:"names"`
}

func loadEssentialFixture(t *testing.T) (essentialFixture, []byte) {
	t.Helper()
	raw := wiretest.Load(t, "essential-skills.json")
	var fixture essentialFixture
	if err := jsonx.UnmarshalStrict(raw, &fixture); err != nil {
		t.Fatalf("decode essential-skills.json: %v", err)
	}
	return fixture, raw
}

func TestEssentialSkillsMatchGolden(t *testing.T) {
	fixture, raw := loadEssentialFixture(t)
	got := skillhub.EssentialSkills()
	if len(got) != len(fixture.Skills) || len(got) != 5 { // Update (A-70): 4 with watch-and-react. Update (A-182): 5 with visual-self-check
		t.Fatalf("EssentialSkills() has %d packages, the TS dump has %d", len(got), len(fixture.Skills))
	}
	for i, want := range fixture.Skills {
		if got[i].Content != want.Content {
			t.Errorf("skill %d: SKILL.md differs from the TS dump (%d bytes, want %d)", i, len(got[i].Content), len(want.Content))
		}
		if strings.Contains(got[i].Content, "\r") {
			t.Errorf("skill %d: SKILL.md contains a carriage return", i)
		}
		if len(got[i].Files) != len(want.Files) {
			t.Errorf("skill %d: %d files, the TS dump has %d", i, len(got[i].Files), len(want.Files))
			continue
		}
		for j, wantFile := range want.Files {
			gotFile := got[i].Files[j]
			if gotFile.Path != wantFile.Path || gotFile.Content != wantFile.Content {
				t.Errorf("skill %d: file %d (%q) differs from the TS dump (%q)", i, j, gotFile.Path, wantFile.Path)
			}
			if strings.Contains(gotFile.Content, "\r") {
				t.Errorf("skill %d: file %q contains a carriage return", i, gotFile.Path)
			}
		}
	}

	// The packages encode to the very bytes of the dump's skills array.
	var dump struct {
		Skills json.RawMessage `json:"skills"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatalf("split the fixture: %v", err)
	}
	encoded, err := kernel.EncodeJSON(got)
	if err != nil {
		t.Fatalf("encode the essential skills: %v", err)
	}
	if !bytes.Equal(encoded, dump.Skills) {
		t.Errorf("the encoded essential skills differ from the TS dump\n got: %.200s\nwant: %.200s", encoded, dump.Skills)
	}
}

func TestEssentialSkillNames(t *testing.T) {
	fixture, _ := loadEssentialFixture(t)
	got := skillhub.EssentialSkillNames()
	if !slices.Equal(got, fixture.Names) {
		t.Errorf("EssentialSkillNames() = %v, the TS dump has %v", got, fixture.Names)
	}
	// Update (A-182): visual-self-check is the fifth.
	if !slices.Equal(got, []string{"skill-creator", "plan", "web-research", "watch-and-notify", "visual-self-check"}) {
		t.Errorf("EssentialSkillNames() = %v", got)
	}
}

func TestEssentialSkillsAreFreshCopies(t *testing.T) {
	first := skillhub.EssentialSkills()
	wantContent := first[0].Content
	wantFile := first[0].Files[0].Content
	first[0].Content = "changed"
	first[0].Files[0].Content = "changed"
	first[0].Files = append(first[0].Files, skill.File{Path: "extra.md", Content: "x"})
	first[1], first[2] = first[2], first[1]

	again := skillhub.EssentialSkills()
	if len(again) != 5 || again[0].Content != wantContent || again[0].Files[0].Content != wantFile || len(again[0].Files) != 1 {
		t.Error("mutating a returned package changed the next EssentialSkills() call")
	}
	if again[1].Content == again[2].Content {
		t.Error("the packages are not distinct")
	}

	names := skillhub.EssentialSkillNames()
	names[0] = "changed"
	if skillhub.EssentialSkillNames()[0] != "skill-creator" {
		t.Error("mutating the returned names changed the next EssentialSkillNames() call")
	}
}

func TestEssentialSkillsLoadAsBuiltIn(t *testing.T) {
	ctx := context.Background()
	store := persistence.NewMemorySkillStore()
	hub := skillhub.NewSkillHub(func() persistence.SkillStore { return store })
	for _, pkg := range skillhub.EssentialSkills() {
		sk, err := hub.Register(pkg, skill.SourceBuiltIn)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if sk.Source != skill.SourceBuiltIn || sk.Version != 1 {
			t.Errorf("%s: source %q version %d", sk.Name, sk.Source, sk.Version)
		}
	}

	list, err := hub.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, sk := range list {
		names = append(names, sk.Name)
		if sk.Source != skill.SourceBuiltIn || sk.Version != 1 {
			t.Errorf("%s: source %q version %d", sk.Name, sk.Source, sk.Version)
		}
	}
	if want := []string{"plan", "skill-creator", "visual-self-check", "watch-and-notify", "web-research"}; !slices.Equal(names, want) {
		t.Fatalf("List() names = %v, want %v", names, want)
	}

	creator, ok, err := hub.Get(ctx, "skill-creator")
	if err != nil || !ok {
		t.Fatalf("Get(skill-creator) = %v, %v", ok, err)
	}
	if !slices.Equal(creator.RequiresTools, []string{"skill_manage"}) {
		t.Errorf("skill-creator requiresTools = %v", creator.RequiresTools)
	}
	if len(creator.Files) != 1 || creator.Files[0].Path != "references/format.md" {
		t.Errorf("skill-creator files = %+v", creator.Files)
	}

	_, err = hub.Register(skillhub.EssentialSkills()[1], skill.SourceBuiltIn)
	var se *kernel.SkillError
	if !errors.As(err, &se) || se.Code != kernel.SkillCodeAlreadyExists || se.SkillName != "plan" {
		t.Errorf("registering plan twice = %v", err)
	}
}
