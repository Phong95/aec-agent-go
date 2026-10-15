package e2e_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// skillMD is the SKILL.md of a test skill named name.
func skillMD(name string) string {
	return "---\nname: " + name + "\ndescription: Does " + name + ".\n---\n\nStep 1.\n"
}

// skillOpTool is a test tool that runs fn against the agent's own skill handle
// (toolhub.Context.Skills, which is the agent's SkillManager). It is how a test makes
// "the agent creates or deletes a skill" happen through the real loop, the way the
// skill_manage tool of Phase 11 will.
type skillOpTool struct {
	name string
	fn   func(ctx context.Context, skills skill.AgentSkills) error
}

func (t *skillOpTool) Name() string    { return "tool:" + t.name }
func (t *skillOpTool) Version() string { return "1.0.0" }
func (t *skillOpTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        t.name,
		Description: "Runs a skill operation for the test",
		Parameters:  toolhub.JSONSchema{Type: "object"},
		Risk:        toolhub.RiskDraft,
	}
}

func (t *skillOpTool) Execute(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	if tc.Skills == nil {
		return toolhub.Output{}, errors.New("no skills in the tool context")
	}
	if err := t.fn(ctx, tc.Skills); err != nil {
		return toolhub.Output{}, err
	}
	return toolhub.Output{Content: "ok"}, nil
}

// callThenEnd is a model that calls the named tool on its first turn and ends on its
// second (it sees a tool result in the input).
func callThenEnd(t *testing.T, toolName string) *aecmock.MockProvider {
	t.Helper()
	args, err := kernel.NewArguments([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return aecmock.NewMockProvider("mock", nil, aecmock.WithResponse(func(msgs []kernel.Message) kernel.ModelResponse {
		usage := kernel.TokenUsage{InputTokens: 1, OutputTokens: 1}
		if slices.ContainsFunc(msgs, func(m kernel.Message) bool { return len(m.ToolResults) > 0 }) {
			return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "done"}, Usage: usage, StopReason: kernel.StopEndTurn}
		}
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: toolName, Arguments: args}}},
			Usage:      usage,
			StopReason: kernel.StopToolUse,
		}
	}))
}

// newSkillCore builds a core whose only provider runs callThenEnd(toolName) and that
// holds the skill-operation tool.
func newSkillCore(t *testing.T, op *skillOpTool) *aec.AgentCore {
	t.Helper()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatalf("NewAgentCore: %v", err)
	}
	if err := core.UseLLMProvider(callThenEnd(t, op.name)); err != nil {
		t.Fatal(err)
	}
	if err := core.UseTool(op); err != nil {
		t.Fatal(err)
	}
	return core
}

// chatOnce starts a session and runs one turn.
func chatOnce(ctx context.Context, t *testing.T, a *agent.Agent) agentloop.RunResult {
	t.Helper()
	if _, err := a.StartSession(ctx); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	res, err := a.Chat(ctx, "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return res
}

// A skill that a live agent creates changes its allowlist and saves the profile
// through the core's CURRENT persistence hub, not the hub the agent captured at spawn
// (the hub is read at call time there; the conversation keeps its snapshot).
func TestAgentCore_SkillCreateSavesToLivePersistence(t *testing.T) {
	ctx := t.Context()
	op := &skillOpTool{name: "create_skill", fn: func(ctx context.Context, skills skill.AgentSkills) error {
		_, err := skills.Create(ctx, skill.Package{Content: skillMD("site-diary")})
		return err
	}}
	core := newSkillCore(t, op)

	p := testProfile("a1")
	p.Tools = []string{"create_skill"}
	a, err := core.SpawnAgent(ctx, p)
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	// The hub captured at spawn, then a swap to a new hub: the agent saves into the new one.
	spawnHub := core.PersistenceHub()
	live := persistence.NewPersistenceHub()
	if err := core.UsePersistence(live); err != nil {
		t.Fatal(err)
	}

	res := chatOnce(ctx, t, a)
	if res.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2 (create_skill, then the answer)", res.Iterations)
	}

	// The profile with the new skill is in the live hub.
	saved, err := live.Agents.Load(ctx, "a1")
	if err != nil || saved == nil {
		t.Fatalf("live Agents.Load(a1) = %v, %v, want the saved profile", saved, err)
	}
	if !slices.Equal(saved.Skills, []string{"site-diary"}) {
		t.Errorf("saved profile skills = %v, want [site-diary]", saved.Skills)
	}
	// ... and not in the hub captured at spawn, which still holds the spawn-time profile.
	old, err := spawnHub.Agents.Load(ctx, "a1")
	if err != nil || old == nil {
		t.Fatalf("spawn hub Agents.Load(a1) = %v, %v, want the spawn-time profile", old, err)
	}
	if len(old.Skills) != 0 {
		t.Errorf("the hub captured at spawn holds skills %v, want none: the save must go to the live hub", old.Skills)
	}
	// The agent's own profile has the skill, and the skill sits in the live hub's skill store.
	if got := a.Profile().Skills; !slices.Equal(got, []string{"site-diary"}) {
		t.Errorf("Profile().Skills = %v, want [site-diary]", got)
	}
	rec, err := live.Skills.Load(ctx, "site-diary")
	if err != nil || rec == nil || rec.CreatedBy != "a1" {
		t.Fatalf("live Skills.Load(site-diary) = %+v, %v, want a skill created by a1", rec, err)
	}
}

// A skill the agent deletes disables itself when no listener did it first, and the
// profile is saved through the live hub.
func TestAgentCore_SkillDeleteSavesToLivePersistence(t *testing.T) {
	ctx := t.Context()
	op := &skillOpTool{name: "delete_skill", fn: func(ctx context.Context, skills skill.AgentSkills) error {
		return skills.Delete(ctx, "site-diary")
	}}
	core := newSkillCore(t, op)

	p := testProfile("a1")
	p.Tools = []string{"delete_skill"}
	p.Skills = []string{"site-diary"}
	a, err := core.SpawnAgent(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	live := persistence.NewPersistenceHub()
	if err := core.UsePersistence(live); err != nil {
		t.Fatal(err)
	}
	// The skill hub reads the live hub's skill store (it reloaded on UsePersistence),
	// so the custom skill goes there.
	if _, err := core.SkillHub().Create(ctx, skill.Package{Content: skillMD("site-diary")}, skillhub.CreateOptions{CreatedBy: "a1"}); err != nil {
		t.Fatal(err)
	}

	chatOnce(ctx, t, a)

	saved, err := live.Agents.Load(ctx, "a1")
	if err != nil || saved == nil {
		t.Fatalf("live Agents.Load(a1) = %v, %v, want the saved profile", saved, err)
	}
	if len(saved.Skills) != 0 {
		t.Errorf("saved profile skills = %v, want none after the delete", saved.Skills)
	}
	if got := a.Profile().Skills; len(got) != 0 {
		t.Errorf("Profile().Skills = %v, want none after the delete", got)
	}
}

// The skill hub's OnDeleted listener (the AgentCore's disable-for-every-agent fan-out
// in a later plan) calls Agent.UpdateSkills while the same agent is inside
// SkillManager.Delete. No lock of the manager or the agent profile may be held across
// the hub call, or this deadlocks. The turn must finish well inside the timeout.
func TestAgent_SkillDeleteListenerNoDeadlock(t *testing.T) {
	ctx := t.Context()
	op := &skillOpTool{name: "delete_skill", fn: func(ctx context.Context, skills skill.AgentSkills) error {
		return skills.Delete(ctx, "site-diary")
	}}
	core := newSkillCore(t, op)
	if _, err := core.SkillHub().Create(ctx, skill.Package{Content: skillMD("site-diary")}, skillhub.CreateOptions{CreatedBy: "a1"}); err != nil {
		t.Fatal(err)
	}

	p := testProfile("a1")
	p.Tools = []string{"delete_skill"}
	p.Skills = []string{"site-diary", "other"}
	a, err := core.SpawnAgent(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// The listener disables the skill on the agent that is deleting it.
	var listenerRan atomic.Bool
	unsubscribe := core.SkillHub().OnDeleted(func(name string) {
		listenerRan.Store(true)
		a.UpdateSkills(slices.DeleteFunc(a.Profile().Skills, func(n string) bool { return n == name }))
	})
	defer unsubscribe()

	if _, err := a.StartSession(ctx); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		res agentloop.RunResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := a.Chat(ctx, "go")
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("Chat: %v", o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not finish in 5 s: the delete listener deadlocked against the SkillManager or the agent profile lock")
	}

	if !listenerRan.Load() {
		t.Fatal("the OnDeleted listener never ran")
	}
	if got := a.Profile().Skills; !slices.Equal(got, []string{"other"}) {
		t.Errorf("Profile().Skills = %v, want [other]", got)
	}
}

// TestAgentCore_SkillHub holds the 3 AgentCore cases of the skill hub; the hub-level
// cases are tests of package skillhub.
func TestAgentCore_SkillHub(t *testing.T) {
	t.Run("AgentCore registers the essential skills as built-in", func(t *testing.T) {
		ctx := t.Context()
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		skills, err := core.SkillHub().List(ctx)
		if err != nil {
			t.Fatalf("Skills: %v", err)
		}
		var names []string
		for _, s := range skills {
			names = append(names, s.Name)
			if s.Source != skill.SourceBuiltIn {
				t.Errorf("skill %q has source %q, want built-in", s.Name, s.Source)
			}
		}
		want := skillhub.EssentialSkillNames()
		slices.Sort(want)
		if !slices.Equal(names, want) {
			t.Errorf("skills = %v, want the essential names sorted %v", names, want)
		}
		creator, ok, err := core.SkillHub().Get(ctx, "skill-creator")
		if err != nil || !ok {
			t.Fatalf("Skill(skill-creator) = %v, %v", ok, err)
		}
		if !slices.Equal(creator.RequiresTools, []string{"skill_manage"}) {
			t.Errorf("skill-creator requires %v, want [skill_manage]", creator.RequiresTools)
		}
	})

	t.Run("useSkill() registers app skills and rejects duplicates and invalid packages", func(t *testing.T) {
		ctx := t.Context()
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		if err := core.UseSkill(skill.Package{Content: skillMD("rfi-triage")}); err != nil {
			t.Fatalf("UseSkill: %v", err)
		}
		got, ok, err := core.SkillHub().Get(ctx, "rfi-triage")
		if err != nil || !ok || got.Source != skill.SourceApp {
			t.Fatalf("Skill(rfi-triage) = %+v, %v, %v, want an app skill", got, ok, err)
		}
		var se *kernel.SkillError
		if err := core.UseSkill(skill.Package{Content: skillMD("rfi-triage")}); !errors.As(err, &se) {
			t.Errorf("a duplicate UseSkill = %v, want a *kernel.SkillError", err)
		}
		if err := core.UseSkill(skill.Package{Content: "no frontmatter"}); err == nil || !strings.Contains(err.Error(), "frontmatter") {
			t.Errorf("an invalid UseSkill = %v, want an error that mentions the frontmatter", err)
		}
	})

	t.Run("usePersistence() switches the store custom skills come from", func(t *testing.T) {
		ctx := t.Context()
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := core.SkillHub().Create(ctx, skill.Package{Content: skillMD("first-store-skill")}, skillhub.CreateOptions{}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, ok, err := core.SkillHub().Get(ctx, "first-store-skill"); err != nil || !ok {
			t.Fatalf("Skill(first-store-skill) = %v, %v, want it found", ok, err)
		}

		other := persistence.NewMemorySkillStore()
		now := "2026-10-03T00:00:00.000Z"
		if err := other.Save(ctx, persistence.SkillRecord{
			Name: "second-store-skill", Content: skillMD("second-store-skill"), Files: []skill.File{},
			Version: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := core.UsePersistence(persistence.NewPersistenceHub(persistence.WithSkills(other))); err != nil {
			t.Fatal(err)
		}

		if _, ok, err := core.SkillHub().Get(ctx, "first-store-skill"); err != nil || ok {
			t.Errorf("Skill(first-store-skill) after UsePersistence = %v, %v, want it gone with the old store", ok, err)
		}
		if _, ok, err := core.SkillHub().Get(ctx, "second-store-skill"); err != nil || !ok {
			t.Errorf("Skill(second-store-skill) after UsePersistence = %v, %v, want it found", ok, err)
		}
	})
}
