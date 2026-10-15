package agent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// skillIndexRow is one row of the recorded fixture agent-skill-index.json: the skills
// it was given, the maxChars when the row set one, and the block it returned (null when
// there are no skills).
type skillIndexRow struct {
	Name     string `json:"name"`
	MaxChars *int   `json:"maxChars"`
	Skills   []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"skills"`
	Output *string `json:"output"`
}

// TestFormatSkillIndexMatchesGolden replays agent-skill-index.json: FormatSkillIndex
// equals the recorded fixture byte for byte on every recorded case (the guidance
// text, XML escaping, the 12000 unit budget, descriptions cut to 160 units, the
// "N more skill(s)" comment and UTF-16 lengths).
func TestFormatSkillIndexMatchesGolden(t *testing.T) {
	var fixture struct {
		DefaultMaxChars int             `json:"defaultMaxChars"`
		Rows            []skillIndexRow `json:"rows"`
	}
	if err := json.Unmarshal(wiretest.Load(t, "agent-skill-index.json"), &fixture); err != nil {
		t.Fatalf("decode agent-skill-index: %v", err)
	}
	if fixture.DefaultMaxChars != agent.SkillIndexMaxChars {
		t.Fatalf("SkillIndexMaxChars = %d, TS SKILL_INDEX_MAX_CHARS = %d", agent.SkillIndexMaxChars, fixture.DefaultMaxChars)
	}
	if len(fixture.Rows) < 20 {
		t.Fatalf("the corpus holds %d rows, want at least 20", len(fixture.Rows))
	}
	for _, r := range fixture.Rows {
		t.Run(r.Name, func(t *testing.T) {
			skills := make([]skill.Skill, len(r.Skills))
			for i, s := range r.Skills {
				skills[i] = skill.Skill{Name: s.Name, Description: s.Description}
			}
			maxChars := agent.SkillIndexMaxChars
			if r.MaxChars != nil {
				maxChars = *r.MaxChars
			}
			got, ok := agent.FormatSkillIndex(skills, maxChars)
			if r.Output == nil {
				if ok || got != "" {
					t.Fatalf("FormatSkillIndex = %q, %v, want no index (TS null)", got, ok)
				}
				return
			}
			if !ok {
				t.Fatal("FormatSkillIndex reports no index, TS returned a block")
			}
			if got != *r.Output {
				t.Errorf("block differs from TS\n got: %q\nwant: %q", got, *r.Output)
			}
		})
	}
}

// The rows of the corpus that exercise the budget really do: a row that was meant to
// shorten, drop or keep entries must show it, so a recorder that stopped covering
// the branch cannot go unnoticed.
func TestFormatSkillIndex_CorpusCoversEveryBranch(t *testing.T) {
	var fixture struct {
		Rows []skillIndexRow `json:"rows"`
	}
	if err := json.Unmarshal(wiretest.Load(t, "agent-skill-index.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	var shortened, dropped, escaped, unchanged int
	ellipsisRune := string(rune(0x2026))
	for _, r := range fixture.Rows {
		if r.Output == nil {
			continue
		}
		out := *r.Output
		if strings.Contains(out, ellipsisRune+"</description>") {
			shortened++
		}
		if strings.Contains(out, " more skill(s) not listed: ") {
			dropped++
		}
		if strings.Contains(out, "&amp;") && strings.Contains(out, "&lt;") && strings.Contains(out, "&gt;") {
			escaped++
		}
		if r.Name == "budget-equal-keeps-full" && !strings.Contains(out, ellipsisRune) && !strings.Contains(out, " more skill(s)") {
			unchanged++
		}
	}
	if shortened == 0 || dropped == 0 || escaped == 0 || unchanged != 1 {
		t.Errorf("corpus branches: shortened %d, dropped %d, escaped %d, unchanged %d; want each above 0 and unchanged 1", shortened, dropped, escaped, unchanged)
	}
}

// A description whose cut at 159 UTF-16 units would split a surrogate pair loses the
// whole pair: a cut inside the pair would leave a lone surrogate, which a string cannot
// hold (the recorder keeps such a cut out of the corpus).
func TestFormatSkillIndex_SurrogateCutDropsWholePair(t *testing.T) {
	emoji := string(rune(0x1f600))
	// 159 units are 79 emoji and one unit of the 80th: the cut is inside a pair.
	desc := strings.Repeat(emoji, 100)
	got, ok := agent.FormatSkillIndex([]skill.Skill{{Name: "x", Description: desc}}, 500)
	if !ok {
		t.Fatal("no index")
	}
	want := "<description>" + strings.Repeat(emoji, 79) + string(rune(0x2026)) + "</description>"
	if !strings.Contains(got, want) {
		t.Errorf("index does not hold 79 whole emoji and the ellipsis:\n%s", got)
	}
}

// The budget is counted in UTF-16 units, so a block with astral characters is over a
// maxChars that its code point count is under.
func TestFormatSkillIndex_BudgetCountsUTF16Units(t *testing.T) {
	skills := []skill.Skill{{Name: "x", Description: "a" + strings.Repeat(string(rune(0x1f600)), 100)}}
	full, _ := agent.FormatSkillIndex(skills, 1_000_000)
	units := kernel.UTF16Len(full)
	if runes := len([]rune(full)); runes >= units {
		t.Fatalf("test setup: runes %d must be fewer than units %d", runes, units)
	}
	// maxChars between the rune count and the unit count: still over budget in UTF-16 units.
	between := units - 1
	got, _ := agent.FormatSkillIndex(skills, between)
	if got == full {
		t.Error("a block of more UTF-16 units than maxChars must be shortened, but it was kept in full")
	}
	if kept, _ := agent.FormatSkillIndex(skills, units); kept != full {
		t.Error("a block of exactly maxChars UTF-16 units must be kept in full")
	}
}
