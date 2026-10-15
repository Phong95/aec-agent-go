package skillhub_test

import (
	"context"
	"encoding/json"
	"os"
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

// skillWireFixture is the part of skill-wire.json the hub test reads: the package
// registered as an app skill and the two Skills the hub returned. They are
// kept raw so the comparison is on the exact bytes.
type skillWireFixture struct {
	Package     skill.Package   `json:"package"`
	SkillApp    json.RawMessage `json:"skillApp"`
	SkillCustom json.RawMessage `json:"skillCustom"`
}

// wireClockStart is the createdAt of skillCustom in skill-wire.json
// (2023-11-14T22:13:20.000Z): the wiregen case starts the patched clock at
// 1700000000000 and create reads it once.
const wireClockStart = 1700000000000

func encodeSkill(t *testing.T, sk skill.Skill) []byte {
	t.Helper()
	b, err := kernel.EncodeJSON(sk)
	if err != nil {
		t.Fatalf("encode skill: %v", err)
	}
	return b
}

// TestSkillWireReproduced drives the Go hub through the steps of the wiregen case
// skill-wire, and requires the same bytes as the recorded fixture.
func TestSkillWireReproduced(t *testing.T) {
	var fixture skillWireFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "skill-wire.json"), &fixture); err != nil {
		t.Fatalf("decode skill-wire.json: %v", err)
	}
	store := persistence.NewMemorySkillStore()
	hub := skillhub.NewSkillHub(func() persistence.SkillStore { return store }, skillhub.WithClock(wiretest.Clock(wireClockStart, 1)))

	app, err := hub.Register(fixture.Package, skill.SourceApp)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := encodeSkill(t, app); string(got) != string(fixture.SkillApp) {
		t.Errorf("skillApp differs\n got: %s\nwant: %s", got, fixture.SkillApp)
	}

	minimal := skill.Package{Content: "---\nname: clash-review\ndescription: Review a clash report.\n---\nOpen the report and list the clashes.\n"}
	custom, err := hub.Create(context.Background(), minimal, skillhub.CreateOptions{CreatedBy: "agent-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := encodeSkill(t, custom); string(got) != string(fixture.SkillCustom) {
		t.Errorf("skillCustom differs\n got: %s\nwant: %s", got, fixture.SkillCustom)
	}

	record, err := store.Load(context.Background(), "clash-review")
	if err != nil || record == nil {
		t.Fatalf("the custom skill was not saved: %v", err)
	}
	if record.CreatedBy != "agent-1" || record.Version != 1 || record.CreatedAt != "2023-11-14T22:13:20.000Z" {
		t.Errorf("stored record = %+v", record)
	}
}

// skillhubFixtures is every golden fixture owned by package skillhub.
var skillhubFixtures = []string{
	"essential-skills.json",
	"skillhub-errors.json",
}

// TestSkillhubFixturesCovered fails when a fixture with the prefix essential- or
// skillhub- has no entry above, and when an entry has no file, so a fixture added
// to wiregen cannot go unchecked.
func TestSkillhubFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		owned := strings.HasPrefix(e.Name(), "essential-") || strings.HasPrefix(e.Name(), "skillhub-")
		if owned && !slices.Contains(skillhubFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in skillhubFixtures", e.Name())
		}
	}
	for _, f := range skillhubFixtures {
		if !present[f] {
			t.Errorf("skillhubFixtures entry %s has no fixture file", f)
		}
	}
}
