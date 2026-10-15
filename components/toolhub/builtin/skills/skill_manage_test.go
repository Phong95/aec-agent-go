package skills_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/skills"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel"
)

// skillMD is the SKILL.md text of a small skill.
func skillMD(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
}

// libraryOf is a skill hub over an in-memory store, and the SkillManager of one agent over
// it with the given allowlist (the manager applies an allowlist change itself).
func libraryOf(agentID string, allowlist ...string) (*skillhub.SkillHub, *agent.SkillManager) {
	store := persistence.NewMemorySkillStore()
	hub := skillhub.NewSkillHub(func() persistence.SkillStore { return store })
	return hub, agent.NewSkillManager(hub, profile.AgentProfile{ID: agentID, Skills: allowlist})
}

func manageArgs(t *testing.T, raw string) kernel.Arguments {
	t.Helper()
	args, err := kernel.NewArguments([]byte(raw))
	if err != nil {
		t.Fatalf("arguments %s: %v", raw, err)
	}
	return args
}

func TestSkillManageDefinition(t *testing.T) {
	h := skills.NewManageTool()
	if h.Name() != "tool:skill-manage" || h.Version() != "1.0.0" {
		t.Errorf("name %q version %q", h.Name(), h.Version())
	}
	d := h.Definition()
	if d.Name != "skill_manage" {
		t.Errorf("definition name = %q", d.Name)
	}
	// A skill changes how agents behave from then on, so every call needs the user's approval.
	if d.Risk != toolhub.RiskConsequential {
		t.Errorf("risk = %q, want consequential", d.Risk)
	}
	if d.SandboxOperation != "" {
		t.Errorf("sandbox operation = %q, want none: the tool reaches the sandbox only for the workspace copy", d.SandboxOperation)
	}
	if !slices.Equal(d.Parameters.Required, []string{"action"}) {
		t.Errorf("required = %v, want [action]", d.Parameters.Required)
	}
	action, ok := d.Parameters.Properties.Get("action")
	if !ok || !slices.Equal(action.Enum, []string{"create", "rewrite", "patch", "write_file", "remove_file", "delete"}) {
		t.Errorf("action = %+v, want the six actions in the TS order", action)
	}
	// The definition is a fresh value: a change to one cannot reach the next.
	d.Parameters.Required[0] = "changed"
	if again := h.Definition(); again.Parameters.Required[0] != "action" {
		t.Errorf("the definition is shared between calls: %v", again.Parameters.Required)
	}
}

// The skills a call works on are the agent's own, handed over by the context: an agent id
// in the arguments reaches nothing (T-06-24, T-11-14).
func TestSkillManageUsesOnlyTheContextSkills(t *testing.T) {
	hub, theirs := libraryOf("a2")
	if _, err := theirs.Create(t.Context(), skill.Package{Content: skillMD("secret-skill", "Not yours.", "Body.")}); err != nil {
		t.Fatal(err)
	}
	mine := agent.NewSkillManager(hub, profile.AgentProfile{ID: "a1"})

	h := skills.NewManageTool()
	for _, raw := range []string{
		`{"action":"delete","name":"secret-skill","agentId":"a2","agent_id":"a2"}`,
		`{"action":"rewrite","name":"secret-skill","content":"x","agent":"a2"}`,
		`{"action":"write_file","name":"secret-skill","path":"a.md","content":"x","agentId":"a2"}`,
	} {
		out, err := h.Execute(t.Context(), manageArgs(t, raw), toolhub.Context{AgentID: "a1", Skills: mine})
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if out.IsError == nil || !*out.IsError || !strings.Contains(out.Content, `you have no skill named "secret-skill"`) {
			t.Errorf("%s: output = %+v, want the agent to have no such skill", raw, out)
		}
	}
	if _, found, err := hub.Get(t.Context(), "secret-skill"); err != nil || !found {
		t.Errorf("the other agent's skill is gone: found %v, %v", found, err)
	}
}

// One out-of-range number (1e999, which JS reads as Infinity) drops that one argument and
// keeps the others (the toolkit's per-key decode).
func TestSkillManageHugeNumberKeepsTheOtherArguments(t *testing.T) {
	hub, mgr := libraryOf("a1")
	out, err := skills.NewManageTool().Execute(t.Context(),
		manageArgs(t, `{"n":1e999,"action":"create","content":`+quote(skillMD("rfi-triage", "Triage RFIs.", "Read."))+`}`), toolhub.Context{AgentID: "a1", Skills: mgr})
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError != nil && *out.IsError {
		t.Fatalf("error output: %s", out.Content)
	}
	if _, found, _ := hub.Get(t.Context(), "rfi-triage"); !found {
		t.Error("the skill was not created")
	}
}

func quote(s string) string {
	b, err := kernel.EncodeJSON(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// A failing library call that is not a SkillError is an error output with no retryable
// flag; a SkillError is a non-retryable one with its code.
func TestSkillManageMapsLibraryErrors(t *testing.T) {
	_, mgr := libraryOf("a1")
	tc := toolhub.Context{AgentID: "a1", Skills: mgr}
	h := skills.NewManageTool()

	out, err := h.Execute(t.Context(), manageArgs(t, `{"action":"create","content":"no frontmatter"}`), tc)
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError == nil || !*out.IsError || out.Retryable == nil || *out.Retryable {
		t.Fatalf("output = %+v, want a non-retryable error", out)
	}
	if code, _ := out.Metadata.Get("code"); code != "invalid" {
		t.Errorf("metadata code = %v, want invalid", code)
	}
	if !strings.HasPrefix(out.Content, "skill_manage create failed (invalid): ") {
		t.Errorf("content = %q", out.Content)
	}
}

// createFailsSkills is an AgentSkills whose every call fails with a plain error.
type createFailsSkills struct{ skill.AgentSkills }

func (createFailsSkills) Create(context.Context, skill.Package) (skill.Skill, error) {
	return skill.Skill{}, errPlain
}

type plainError struct{}

func (plainError) Error() string { return "the library is down" }

var errPlain error = plainError{}

func TestSkillManagePlainErrorIsRetryable(t *testing.T) {
	out, err := skills.NewManageTool().Execute(t.Context(),
		manageArgs(t, `{"action":"create","content":"x"}`), toolhub.Context{AgentID: "a1", Skills: createFailsSkills{}})
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError == nil || !*out.IsError || out.Retryable != nil {
		t.Errorf("output = %+v, want an error output with no retryable flag, so the harness may retry it", out)
	}
	if out.Content != "skill_manage create failed: the library is down" {
		t.Errorf("content = %q", out.Content)
	}
}

// withSkills wraps a handler so that every Execute gets a tool context with the agent's
// skills. The conformance suite builds its own bare toolhub.Context, so a factory cannot
// inject them; this decorator is how the suite also reaches the path of a tool that has
// skills.
type withSkills struct {
	toolhub.Handler
	skills skill.AgentSkills
}

func (w withSkills) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	tc.Skills = w.skills
	return w.Handler.Execute(ctx, args, tc)
}

// TestSkillManageConformance runs the tool-handler conformance suite on skill_manage,
// once with no skills in the context (the tool says they are not available) and once with
// the agent's skills.
func TestSkillManageConformance(t *testing.T) {
	args := manageArgs(t, `{"action":"create","content":`+quote(skillMD("rfi-triage", "Triage RFIs.", "Read the RFI."))+`}`)
	t.Run("with no skills", func(t *testing.T) {
		conformance.ToolHandler(t, func(*testing.T) toolhub.Handler { return skills.NewManageTool() }, args)
	})
	t.Run("with the agent's skills", func(t *testing.T) {
		conformance.ToolHandler(t, func(*testing.T) toolhub.Handler {
			_, mgr := libraryOf("a1")
			return withSkills{Handler: skills.NewManageTool(), skills: mgr}
		}, args)
	})
}

// TestSkillManageTrimQuirk pins the patch trimming rule (review WR-B03). Trimming patch
// `old_string` but not `new_string` would let an old_string that carried the file's own
// indentation and newline match the trimmed text while the untrimmed new_string added its
// whitespace on top (doubled indentation, an extra blank line). old_string and new_string
// are read untrimmed: the patch replaces exactly the text sent. The full create/rewrite
// content is still trimmed, because a SKILL.md must open with its frontmatter. `name` and
// `path` are still trimmed, and a whitespace-only old_string or content still counts as
// missing.
func TestSkillManageTrimQuirk(t *testing.T) {
	hub, mgr := libraryOf("a1")
	tc := toolhub.Context{AgentID: "a1", Skills: mgr}
	h := skills.NewManageTool()
	run := func(raw string) toolhub.Output {
		t.Helper()
		out, err := h.Execute(t.Context(), manageArgs(t, raw), tc)
		if err != nil {
			t.Fatal(err)
		}
		if out.IsError != nil && *out.IsError {
			t.Fatalf("error output: %s", out.Content)
		}
		return out
	}

	// create trims the full SKILL.md text: the trailing newline goes.
	run(`{"action":"create","name":"  trim-skill  ","content":` + quote(skillMD("trim-skill", "Trim.", "Read.\n  indented line\nMore.")) + `}`)
	got, found, err := hub.Get(t.Context(), "trim-skill")
	if err != nil || !found {
		t.Fatalf("get: %v %v", found, err)
	}
	if !strings.HasSuffix(got.Content, "More.") {
		t.Errorf("create must trim the SKILL.md text, got %q", got.Content)
	}

	// patch: old_string and new_string are both used as sent, so the indentation and the
	// newline are replaced, not doubled (a trimmed old_string would give
	// "Read.\n    changed line\n\nMore."). The name is still trimmed.
	run(`{"action":"patch","name":" trim-skill ","old_string":` + quote("  indented line\n") + `,"new_string":` + quote("  changed line\n") + `}`)
	got, _, _ = hub.Get(t.Context(), "trim-skill")
	if want := "Read.\n  changed line\nMore."; !strings.Contains(got.Content, want) {
		t.Errorf("patched content = %q, want it to contain %q (Q29 fixed in Go)", got.Content, want)
	}

	// rewrite trims its content too, and a whitespace-only content is "required".
	run(`{"action":"rewrite","name":"trim-skill","content":` + quote(skillMD("trim-skill", "Trim.", "  Lead.\nBody.")+"\n\n") + `}`)
	got, _, _ = hub.Get(t.Context(), "trim-skill")
	if !strings.HasSuffix(got.Content, "Body.") {
		t.Errorf("rewrite must trim the content, got %q", got.Content)
	}
	blank, err := h.Execute(t.Context(), manageArgs(t, `{"action":"rewrite","name":"trim-skill","content":" \n "}`), tc)
	if err != nil {
		t.Fatal(err)
	}
	if blank.IsError == nil || !*blank.IsError || !strings.Contains(blank.Content, `"content" (the full new SKILL.md) is required`) {
		t.Errorf("a whitespace-only content must be rejected: %+v", blank)
	}

	// an old_string that is only whitespace is "required".
	out, err := h.Execute(t.Context(), manageArgs(t, `{"action":"patch","name":"trim-skill","old_string":"  \n","new_string":"x"}`), tc)
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError == nil || !*out.IsError || !strings.Contains(out.Content, `"old_string" and "new_string" are required`) {
		t.Errorf("a whitespace-only old_string must be rejected: %+v", out)
	}

	// write_file content is NOT trimmed.
	run(`{"action":"write_file","name":"trim-skill","path":"notes.txt","content":` + quote("  keep me\n") + `}`)
	got, _, _ = hub.Get(t.Context(), "trim-skill")
	var file string
	for _, f := range got.Files {
		if f.Path == "notes.txt" {
			file = f.Content
		}
	}
	if file != "  keep me\n" {
		t.Errorf("write_file content = %q, want it untrimmed", file)
	}
}
