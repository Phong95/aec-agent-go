package agent_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The eight subtests of TestSkillManager carry the titles of the shared skill
// manager conformance cases. The SkillManager resolves skills lazily
// from the skill hub, so every test builds a real hub over an in-memory skill store.

// skillMD is the SKILL.md of a skill named name.
func skillMD(name string) string { return skillMDWith(name, "") }

// skillMDWith is skillMD with extra frontmatter lines after the description.
func skillMDWith(name, extra string) string {
	return "---\nname: " + name + "\ndescription: Does " + name + ".\n" + extra + "---\n\nStep 1.\n"
}

// skillProfile is agent-1 with the given allowlist and no tools.
func skillProfile(skills ...string) profile.AgentProfile {
	p := testProfile("agent-1")
	p.Name = "test"
	p.SystemPrompt = ""
	p.Skills = skills
	return p
}

func newSkillHub() *skillhub.SkillHub {
	store := persistence.NewMemorySkillStore()
	return skillhub.NewSkillHub(func() persistence.SkillStore { return store })
}

func mustRegisterSkill(t *testing.T, hub *skillhub.SkillHub, content string) {
	t.Helper()
	if _, err := hub.Register(skill.Package{Content: content}, skill.SourceApp); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func skillNames(skills []skill.Skill) []string {
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	return names
}

// mustList lists the manager's skills and fails the test on an error.
func mustList(t *testing.T, m *agent.SkillManager) []skill.Skill {
	t.Helper()
	list, err := m.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return list
}

// mustGet reports whether the manager's Get finds the skill, failing the test on an error.
func mustGet(t *testing.T, m *agent.SkillManager, name string) bool {
	t.Helper()
	_, ok, err := m.Get(t.Context(), name)
	if err != nil {
		t.Fatalf("Get(%s): %v", name, err)
	}
	return ok
}

// skillCode returns the SkillError code of err, or "" when err is not a SkillError.
func skillCode(err error) kernel.SkillErrorCode {
	var se *kernel.SkillError
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

// failingSkillStore is a skill store whose List fails (a "db down" store).
type failingSkillStore struct{}

func (failingSkillStore) Save(context.Context, persistence.SkillRecord) error { return nil }
func (failingSkillStore) Load(context.Context, string) (*persistence.SkillRecord, error) {
	return nil, nil //nolint:nilnil // the store contract: a nil record and a nil error mean unknown
}

func (failingSkillStore) List(context.Context) ([]persistence.SkillRecord, error) {
	return nil, errors.New("db down")
}
func (failingSkillStore) Delete(context.Context, string) error { return nil }

func TestSkillManager(t *testing.T) {
	// --- SkillManager - lazy resolution ---
	t.Run("should resolve skills registered AFTER SkillManager construction", func(t *testing.T) {
		hub := newSkillHub()
		// Create the manager before any skills are registered (simulates setup ordering).
		manager := agent.NewSkillManager(hub, skillProfile("generate-report"))

		// Skills not yet registered: the list is empty.
		if n := len(mustList(t, manager)); n != 0 {
			t.Fatalf("List() = %d skills before registration, want 0", n)
		}
		if mustGet(t, manager, "generate-report") {
			t.Fatal("Get(generate-report) found a skill before registration")
		}

		// Register the skill after the manager was built.
		mustRegisterSkill(t, hub, skillMD("generate-report"))

		if n := len(mustList(t, manager)); n != 1 {
			t.Fatalf("List() = %d skills after registration, want 1", n)
		}
		if !mustGet(t, manager, "generate-report") {
			t.Fatal("the newly registered skill is not visible to Get")
		}
	})

	t.Run("should only return skills in the allowlist", func(t *testing.T) {
		hub := newSkillHub()
		mustRegisterSkill(t, hub, skillMD("generate-report"))

		manager := agent.NewSkillManager(hub, skillProfile())
		if n := len(mustList(t, manager)); n != 0 {
			t.Fatalf("List() = %d skills, want 0", n)
		}
		if mustGet(t, manager, "generate-report") {
			t.Fatal("Get found a skill that is not in the allowlist")
		}
	})

	t.Run("should reflect allowlist changes via setAllowedSkills()", func(t *testing.T) {
		hub := newSkillHub()
		mustRegisterSkill(t, hub, skillMD("generate-report"))

		manager := agent.NewSkillManager(hub, skillProfile())
		if n := len(mustList(t, manager)); n != 0 {
			t.Fatalf("List() = %d skills, want 0", n)
		}

		// Simulate PATCH /agents/:id { skills: [...] }
		manager.SetAllowedSkills([]string{"generate-report"})
		if n := len(mustList(t, manager)); n != 1 {
			t.Fatalf("List() = %d skills after SetAllowedSkills, want 1", n)
		}
		if !mustGet(t, manager, "generate-report") {
			t.Fatal("Get does not find the skill after SetAllowedSkills")
		}
	})

	// --- SkillManager - what the model sees ---
	t.Run("hides skills whose required tools the agent lacks", func(t *testing.T) {
		hub := newSkillHub()
		mustRegisterSkill(t, hub, skillMDWith("research", "metadata:\n  requires-tools: web_search web_extract\n"))
		mustRegisterSkill(t, hub, skillMD("plan"))
		var mu sync.Mutex
		tools := map[string]bool{"web_search": true}
		manager := agent.NewSkillManager(hub, skillProfile("research", "plan"), agent.WithHasTool(func(name string) bool {
			mu.Lock()
			defer mu.Unlock()
			return tools[name]
		}))

		if got := skillNames(mustList(t, manager)); !slices.Equal(got, []string{"plan"}) {
			t.Fatalf("List() = %v, want [plan]", got)
		}
		// Still enabled: Get finds it, so it can be edited or viewed on purpose.
		if !mustGet(t, manager, "research") {
			t.Fatal("Get(research) must find a skill that is enabled but lacks a tool")
		}

		mu.Lock()
		tools["web_extract"] = true
		mu.Unlock()
		if got := skillNames(mustList(t, manager)); !slices.Equal(got, []string{"research", "plan"}) {
			t.Fatalf("List() = %v, want [research plan]", got)
		}
	})

	t.Run("builds the <available_skills> index and adds skill_view only when there are skills", func(t *testing.T) {
		hub := newSkillHub()
		mustRegisterSkill(t, hub, skillMD("rfi-triage"))
		manager := agent.NewSkillManager(hub, skillProfile())

		if index, ok := manager.BuildIndex(t.Context()); ok || index != "" {
			t.Fatalf("BuildIndex() = %q, %v, want no index", index, ok)
		}
		if defs, err := manager.ToolDefinitions(t.Context()); err != nil || len(defs) != 0 {
			t.Fatalf("ToolDefinitions() = %v, %v, want none", defs, err)
		}

		manager.SetAllowedSkills([]string{"rfi-triage"})
		index, ok := manager.BuildIndex(t.Context())
		if !ok {
			t.Fatal("BuildIndex() reports no index for an enabled skill")
		}
		for _, want := range []string{
			"<available_skills>",
			"<name>rfi-triage</name>",
			"<description>Does rfi-triage.</description>",
		} {
			if !strings.Contains(index, want) {
				t.Errorf("index lacks %q:\n%s", want, index)
			}
		}
		defs, err := manager.ToolDefinitions(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, d := range defs {
			names = append(names, d.Name)
		}
		if !slices.Equal(names, []string{"skill_view"}) {
			t.Fatalf("ToolDefinitions() names = %v, want [skill_view]", names)
		}
		if _, ok := manager.Tool("skill_view"); !ok {
			t.Error("Tool(skill_view) is not defined")
		}
		if _, ok := manager.Tool("rfi-triage"); ok {
			t.Error("Tool(rfi-triage) must not be defined: a skill is not a tool")
		}
	})

	t.Run("runs without skills when the store fails", func(t *testing.T) {
		hub := skillhub.NewSkillHub(func() persistence.SkillStore { return failingSkillStore{} })
		var logs bytes.Buffer
		manager := agent.NewSkillManager(hub, skillProfile("anything"),
			agent.WithSkillLogger(slog.New(slog.NewTextHandler(&logs, nil))))

		if index, ok := manager.BuildIndex(t.Context()); ok || index != "" {
			t.Fatalf("BuildIndex() = %q, %v, want no index when the store fails", index, ok)
		}
		if defs, err := manager.ToolDefinitions(t.Context()); err != nil || len(defs) != 0 {
			t.Fatalf("ToolDefinitions() = %v, %v, want none and no error when the store fails", defs, err)
		}
		// The failure is logged with the agent id, not swallowed silently.
		if out := logs.String(); !strings.Contains(out, "agent-1") || !strings.Contains(out, "db down") {
			t.Errorf("the log must name the agent and the store error, got: %s", out)
		}
	})

	// --- SkillManager - AgentSkills (ToolContext.skills) ---
	t.Run("create() saves to the library and enables the skill for the creating agent", func(t *testing.T) {
		hub := newSkillHub()
		var (
			manager *agent.SkillManager
			calls   [][]string
		)
		manager = agent.NewSkillManager(hub, skillProfile(), agent.WithOnAllowlistChange(func(_ context.Context, names []string) error {
			calls = append(calls, slices.Clone(names))
			manager.SetAllowedSkills(names)
			return nil
		}))

		created, err := manager.Create(t.Context(), skill.Package{Content: skillMD("site-diary")})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if created.Name != "site-diary" || created.Source != skill.SourceCustom || created.CreatedBy != "agent-1" {
			t.Fatalf("Create() = name %q source %q createdBy %q, want site-diary custom agent-1", created.Name, created.Source, created.CreatedBy)
		}
		if len(calls) != 1 || !slices.Equal(calls[0], []string{"site-diary"}) {
			t.Fatalf("onAllowlistChange calls = %v, want one call with [site-diary]", calls)
		}
		if got := skillNames(mustList(t, manager)); !slices.Equal(got, []string{"site-diary"}) {
			t.Fatalf("List() = %v, want [site-diary]", got)
		}
	})

	t.Run("update()/delete() need the skill to be enabled for the agent; delete() disables it", func(t *testing.T) {
		hub := newSkillHub()
		if _, err := hub.Create(t.Context(), skill.Package{Content: skillMD("shared-skill")}, skillhub.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		manager := agent.NewSkillManager(hub, skillProfile())

		_, err := manager.Update(t.Context(), "shared-skill", skill.Package{Content: skillMD("shared-skill")})
		if got := skillCode(err); got != kernel.SkillCodeNotAllowed {
			t.Fatalf("Update of a skill that is not enabled: code %q (%v), want not_allowed", got, err)
		}
		if want := `Skill "shared-skill" is not enabled for this agent, so it cannot change it`; err == nil || err.Error() != want {
			t.Fatalf("not_allowed text = %v, want %q", err, want)
		}
		err = manager.Delete(t.Context(), "missing")
		if got := skillCode(err); got != kernel.SkillCodeNotFound {
			t.Fatalf("Delete of an unknown skill: code %q (%v), want not_found", got, err)
		}
		if want := `Skill "missing" not found`; err.Error() != want {
			t.Fatalf("not_found text = %q, want %q", err.Error(), want)
		}

		manager.SetAllowedSkills([]string{"shared-skill"})
		updated, err := manager.Update(t.Context(), "shared-skill", skill.Package{Content: strings.Replace(skillMD("shared-skill"), "Step 1.", "Step 2.", 1)})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.Version != 2 {
			t.Fatalf("Update() version = %d, want 2", updated.Version)
		}

		if err := manager.Delete(t.Context(), "shared-skill"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, ok, err := hub.Get(t.Context(), "shared-skill"); err != nil || ok {
			t.Fatalf("hub.Get after Delete = %v, %v, want no skill", ok, err)
		}
		if mustGet(t, manager, "shared-skill") {
			t.Fatal("Get still finds a deleted skill: Delete must disable it")
		}
		if n := len(mustList(t, manager)); n != 0 {
			t.Fatalf("List() = %d skills after Delete, want 0", n)
		}
	})
}

// A failing onAllowlistChange hook (the profile save failed) fails Create and Delete
// with the hook's error, but the library change stays: Create hands the skill back
// together with the error.
func TestSkillManager_HookErrorIsReturned(t *testing.T) {
	hub := newSkillHub()
	boom := errors.New("save failed")
	manager := agent.NewSkillManager(hub, skillProfile(), agent.WithOnAllowlistChange(func(context.Context, []string) error { return boom }))

	created, err := manager.Create(t.Context(), skill.Package{Content: skillMD("site-diary")})
	if !errors.Is(err, boom) {
		t.Fatalf("Create error = %v, want the hook's error", err)
	}
	if created.Name != "site-diary" {
		t.Fatalf("Create() = %q with the error, want the created skill handed back", created.Name)
	}
	if _, ok, _ := hub.Get(t.Context(), "site-diary"); !ok {
		t.Fatal("the skill must stay in the library when only the allowlist save failed")
	}
}

// The allowlist handed to NewSkillManager and to SetAllowedSkills is copied, and the
// one the hook receives is the manager's own copy.
func TestSkillManager_AllowlistIsCopied(t *testing.T) {
	hub := newSkillHub()
	mustRegisterSkill(t, hub, skillMD("a"))
	mustRegisterSkill(t, hub, skillMD("b"))

	names := []string{"a"}
	manager := agent.NewSkillManager(hub, skillProfile(names...))
	names[0] = "b"
	if got := skillNames(mustList(t, manager)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("List() = %v after the caller changed its slice, want [a]", got)
	}

	set := []string{"b"}
	manager.SetAllowedSkills(set)
	set[0] = "a"
	if got := skillNames(mustList(t, manager)); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("List() = %v after the caller changed the SetAllowedSkills slice, want [b]", got)
	}
}

// Two creates at once both end up in the allowlist (the manager serializes the
// read-modify-write so neither is lost).
func TestSkillManager_ConcurrentCreatesKeepBoth(t *testing.T) {
	hub := newSkillHub()
	var manager *agent.SkillManager
	manager = agent.NewSkillManager(hub, skillProfile(), agent.WithOnAllowlistChange(func(_ context.Context, names []string) error {
		manager.SetAllowedSkills(names)
		return nil
	}))

	var wg sync.WaitGroup
	for _, name := range []string{"one", "two", "three", "four"} {
		wg.Go(func() {
			if _, err := manager.Create(t.Context(), skill.Package{Content: skillMD(name)}); err != nil {
				t.Errorf("Create(%s): %v", name, err)
			}
		})
	}
	wg.Wait()

	got := skillNames(mustList(t, manager))
	slices.Sort(got)
	if want := []string{"four", "one", "three", "two"}; !slices.Equal(got, want) {
		t.Fatalf("List() = %v, want every created skill enabled: %v", got, want)
	}
}
