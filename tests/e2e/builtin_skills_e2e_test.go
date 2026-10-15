package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/skills"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file holds the 9 end-to-end cases of skills in the agent loop (A-66). The <available_skills> index in the system prompt, skill_view added
// automatically, skill files copied to the sandbox, and skill_manage creating and changing
// skills behind the approval gate, all through a real AgentCore, a scripted model, the
// in-memory sandbox and aecmock.ScriptedInteraction.

// skillsAgentID is the id of the test agent.
const skillsAgentID = "skills-agent"

// skillsMD builds the SKILL.md of a test skill.
func skillsMD(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
}

// skillsRunOptions are the options of the test setup.
type skillsRunOptions struct {
	calls []loopCall
	// tools is the allowlist of tools of the agent: nil means skill_manage, an empty
	// slice means none.
	tools []string
	// skills is the allowlist of skills: nil means the essential skills.
	skills []string
	// handler is the interaction handler of the tool hub; nil means there is none.
	handler toolhub.InteractionHandler
	// prepare runs on the core before the agent is spawned.
	prepare func(t *testing.T, core *aec.AgentCore)
	// afterSpawn runs once the agent exists and has a session, before the chat (a case
	// that needs a second agent spawns it here).
	afterSpawn func(t *testing.T, core *aec.AgentCore)
}

// skillsRun is the result of the test setup after one chat: the core, the agent, and what the
// scripted model saw: the system prompt and the tool names at every model call, and the
// tool result after every call.
type skillsRun struct {
	wc            *workspaceCore
	agent         *agent.Agent
	systemPrompts []string
	toolNames     [][]string
	results       []kernel.ToolResult
}

// runSkills registers skill_manage on a core with the shared in-memory sandbox, spawns the
// agent and plays the scripted calls, one per model turn, in one chat. The core is
// disposed when the test ends.
func runSkills(t *testing.T, o skillsRunOptions) *skillsRun {
	t.Helper()
	ctx := t.Context()
	wc := newWorkspaceCore(t, workspaceCoreOptions{interaction: o.handler})
	t.Cleanup(func() {
		if err := wc.core.Dispose(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	})

	turns := make([]aecmock.ScriptedTurn, 0, len(o.calls)+1)
	for i, c := range o.calls {
		turns = append(turns, toolTurn(toolCallOf(fmt.Sprintf("tc-%d", i+1), c.name, c.args)))
	}
	turns = append(turns, aecmock.ScriptedTurn{Content: "done"})
	provider := aecmock.NewScriptedProvider("scripted", turns)
	if err := wc.core.UseLLMProvider(provider); err != nil {
		t.Fatalf("UseLLMProvider: %v", err)
	}
	if err := wc.core.UseTool(skills.NewManageTool()); err != nil {
		t.Fatalf("UseTool: %v", err)
	}
	if o.prepare != nil {
		o.prepare(t, wc.core)
	}

	tools := o.tools
	if tools == nil {
		tools = []string{"skill_manage"}
	}
	skillNames := o.skills
	if skillNames == nil {
		skillNames = skillhub.EssentialSkillNames()
	}
	retries, backoff := 1, 0
	a, err := wc.core.SpawnAgent(ctx, profile.AgentProfile{
		ID: skillsAgentID, Name: "skills", SystemPrompt: "You are a test agent.",
		Tools: tools, Skills: skillNames,
		Compactor: testCompactor(), Memory: loopMemoryOff(),
		LoopConfig: &profile.LoopConfig{MaxToolRetries: &retries, RetryBackoffMs: &backoff},
	})
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	if _, err := a.StartSession(ctx); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if o.afterSpawn != nil {
		o.afterSpawn(t, wc.core)
	}
	if _, err := a.Chat(ctx, "go"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	run := &skillsRun{wc: wc, agent: a}
	run.read(provider)
	return run
}

// read extracts what the scripted model saw: at every call
// the system prompt and the tool names, and at every call after the first the first result
// of the last tool message.
func (r *skillsRun) read(provider *aecmock.ScriptedProvider) {
	for i, call := range provider.Calls() {
		system := ""
		for _, m := range call.Messages {
			if m.Role == kernel.RoleSystem {
				system = m.Content
				break
			}
		}
		r.systemPrompts = append(r.systemPrompts, system)
		names := []string{}
		for _, d := range call.Options.Tools {
			names = append(names, d.Name)
		}
		r.toolNames = append(r.toolNames, names)
		if i == 0 {
			continue
		}
		for j := len(call.Messages) - 1; j >= 0; j-- {
			if len(call.Messages[j].ToolResults) > 0 {
				r.results = append(r.results, call.Messages[j].ToolResults[0])
				break
			}
		}
	}
}

// approvalsOf is an approval handler: it approves every call, or declines
// it with the reason "no thanks".
func approvalsOf(approve bool) *aecmock.ScriptedInteraction {
	return aecmock.NewScriptedInteraction(aecmock.WithApprovalFunc(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
		if approve {
			return toolhub.ApprovalDecision{Approved: true}, nil
		}
		reason := "no thanks"
		return toolhub.ApprovalDecision{Approved: false, Reason: &reason}, nil
	}))
}

// inSandbox reads a file of the agent's sandbox (the shared in-memory one).
func (r *skillsRun) inSandbox(t *testing.T, path string) (string, bool) {
	t.Helper()
	return fileIn(t, r.agent, path)
}

// filesUnder lists the files of the agent's workspace (the shared in-memory sandbox) whose
// path starts with the prefix, relative to the agent's working directory.
func (r *skillsRun) filesUnder(t *testing.T, prefix string) []string {
	t.Helper()
	view, err := r.agent.Sandbox(t.Context())
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	yes, limit := true, 1000
	listed, err := view.List(t.Context(), ".", sandboxhub.ListOptions{Recursive: &yes, IncludeHidden: &yes, MaxEntries: &limit})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []string
	for _, e := range listed.Entries {
		if e.Type == sandboxhub.EntryFile && strings.HasPrefix(e.Path, prefix) {
			out = append(out, e.Path)
		}
	}
	return out
}

func (r *skillsRun) requireResults(t *testing.T, n int) {
	t.Helper()
	if len(r.results) != n {
		t.Fatalf("the model saw %d tool results, want %d: %+v", len(r.results), n, r.results)
	}
}

// customSkill creates a custom skill in the core's library, the prepare step of three
// cases.
func customSkill(t *testing.T, core *aec.AgentCore, content string, files ...skill.File) {
	t.Helper()
	if _, err := core.SkillHub().Create(t.Context(), skill.Package{Content: content, Files: files}, skillhub.CreateOptions{}); err != nil {
		t.Fatalf("create the skill: %v", err)
	}
}

// manageCall is one scripted skill_manage call, with the argument object encoded.
func manageCall(t *testing.T, args map[string]any) loopCall {
	t.Helper()
	b, err := kernel.EncodeJSON(args)
	if err != nil {
		t.Fatal(err)
	}
	return loopCall{"skill_manage", string(b)}
}

// TestAgentSkillsE2E holds the 9 end-to-end skill cases, each under its own title.
func TestAgentSkillsE2E(t *testing.T) {
	t.Run("lists usable skills in the system prompt and offers skill_view", func(t *testing.T) {
		run := runSkills(t, skillsRunOptions{})

		system := run.systemPrompts[0]
		if !strings.HasPrefix(system, "You are a test agent.") {
			t.Errorf("the system prompt does not start with the agent prompt:\n%s", system)
		}
		for _, want := range []string{"<available_skills>", "<name>skill-creator</name>", "<name>plan</name>"} {
			if !strings.Contains(system, want) {
				t.Errorf("the system prompt does not contain %q:\n%s", want, system)
			}
		}
		// web-research needs web_search + web_extract, which this agent does not have
		if strings.Contains(system, "<name>web-research</name>") {
			t.Error("the system prompt lists web-research, which needs tools this agent does not have")
		}
		if want := []string{"skill_manage", "skill_view"}; !slices.Equal(run.toolNames[0], want) {
			t.Errorf("tool names = %v, want %v", run.toolNames[0], want)
		}
	})

	t.Run("omits the index and skill_view when the agent has no usable skills", func(t *testing.T) {
		// skill-creator needs skill_manage; without it the agent has no usable skill
		run := runSkills(t, skillsRunOptions{tools: []string{}, skills: []string{"skill-creator"}})
		if got := run.systemPrompts[0]; got != "You are a test agent." {
			t.Errorf("the system prompt = %q, want the agent prompt alone", got)
		}
		if len(run.toolNames[0]) != 0 {
			t.Errorf("tool names = %v, want none", run.toolNames[0])
		}
	})

	t.Run("skill_view loads a skill, reads its files, and is traced as a skill dispatch", func(t *testing.T) {
		run := runSkills(t, skillsRunOptions{calls: []loopCall{
			{"skill_view", `{"name":"skill-creator"}`},
			{"skill_view", `{"name":"skill-creator","path":"references/format.md"}`},
			{"skill_view", `{"name":"no-such-skill"}`},
		}})

		run.requireResults(t, 3)
		first := run.results[0].Content
		for _, want := range []string{
			`<skill_content name="skill-creator">`,
			"# Skill Creator",
			"<skill_resources>\nreferences/format.md\n</skill_resources>",
			".skills/skill-creator/",
		} {
			if !strings.Contains(first, want) {
				t.Errorf("result 0 does not contain %q:\n%s", want, first)
			}
		}
		if !strings.Contains(run.results[1].Content, "# Skill format reference") {
			t.Errorf("result 1 = %q, want the format reference", run.results[1].Content)
		}
		if !isErrorResult(run.results[2]) || !strings.Contains(run.results[2].Content, "Your skills: skill-creator, plan") {
			t.Errorf("result 2 = %+v, want an error that lists the skills", run.results[2])
		}

		// Files copied into the isolated sandbox
		if got, ok := run.inSandbox(t, ".skills/skill-creator/references/format.md"); !ok || !strings.Contains(got, "# Skill format reference") {
			t.Errorf("the copied format reference = %q (present %v)", got, ok)
		}
		if got, ok := run.inSandbox(t, ".skills/skill-creator/SKILL.md"); !ok || !strings.Contains(got, "# Skill Creator") {
			t.Errorf("the copied SKILL.md = %q (present %v)", got, ok)
		}

		var names []string
		for _, rec := range run.wc.spans.all() {
			if rec.phase == "end" && rec.span.Kind == trace.KindSkillDispatch {
				names = append(names, rec.span.Name)
			}
		}
		if want := []string{"skill-creator", "skill-creator", "no-such-skill"}; !slices.Equal(names, want) {
			t.Errorf("skill-dispatch spans = %v, want %v", names, want)
		}
		if n := run.wc.log.count(events.SkillDispatched.Name); n != 3 {
			t.Errorf("%d skill dispatch events, want 3", n)
		}
		if n := run.wc.log.count(events.ToolApprovalRequested.Name); n != 0 {
			t.Errorf("%d approval requests, want none: skill_view is exempt from the approval gate", n)
		}
	})

	t.Run("skill_manage creates a skill after approval, enables it and persists the profile", func(t *testing.T) {
		handler := approvalsOf(true)
		content := skillsMD("rfi-triage", "Triage incoming RFIs by trade and urgency. Use when the user shares RFIs.", "1. Read the RFI.\n2. Tag it.")
		call := manageCall(t, map[string]any{
			"action": "create", "content": content,
			"files": []map[string]string{{"path": "references/trades.md", "content": "Trades list"}},
		})
		run := runSkills(t, skillsRunOptions{handler: handler, calls: []loopCall{call}})
		core := run.wc.core

		requests := handler.Approvals()
		if len(requests) != 1 {
			t.Fatalf("%d approval requests, want 1", len(requests))
		}
		if requests[0].ToolName != "skill_manage" || requests[0].Risk != toolhub.RiskConsequential {
			t.Errorf("request = %+v, want skill_manage with the risk consequential", requests[0])
		}
		var shown map[string]any
		if err := json.Unmarshal(requests[0].Arguments, &shown); err != nil || shown["content"] != content {
			t.Errorf("the approval request does not show the full content being saved: %v %v", shown["content"], err)
		}

		run.requireResults(t, 1)
		for _, want := range []string{`Created skill "rfi-triage" with 1 extra file`, "written to your workspace at .skills/rfi-triage/"} {
			if !strings.Contains(run.results[0].Content, want) {
				t.Errorf("result = %q, want it to contain %q", run.results[0].Content, want)
			}
		}

		// The new skill's folder is in the isolated sandbox right away, SKILL.md included
		created, found, err := core.SkillHub().Get(t.Context(), "rfi-triage")
		if err != nil || !found {
			t.Fatalf("Skill(rfi-triage) = %v, %v", found, err)
		}
		dir := ".skills/rfi-triage"
		if got, ok := run.inSandbox(t, dir+"/SKILL.md"); !ok || got != created.Content {
			t.Errorf("the copied SKILL.md = %q (present %v), want the library's content", got, ok)
		}
		if got, ok := run.inSandbox(t, dir+"/references/trades.md"); !ok || got != "Trades list" {
			t.Errorf("the copied file = %q (present %v)", got, ok)
		}
		if created.Source != skill.SourceCustom || created.CreatedBy != skillsAgentID || created.Version != 1 {
			t.Errorf("skill = %+v, want custom, created by %s, version 1", created, skillsAgentID)
		}
		if !slices.Contains(run.agent.Profile().Skills, "rfi-triage") {
			t.Errorf("the agent's skills = %v, want rfi-triage enabled", run.agent.Profile().Skills)
		}
		saved, err := core.PersistenceHub().Agents.Load(t.Context(), skillsAgentID)
		if err != nil || saved == nil || !slices.Contains(saved.Skills, "rfi-triage") {
			t.Errorf("the saved profile = %+v, %v, want rfi-triage in its skills", saved, err)
		}

		// The next model call already lists the new skill
		if len(run.systemPrompts) < 2 || !strings.Contains(run.systemPrompts[1], "<name>rfi-triage</name>") {
			t.Errorf("the second model call's system prompt does not list the new skill:\n%v", run.systemPrompts)
		}
	})

	t.Run("does not create the skill when the user declines", func(t *testing.T) {
		call := manageCall(t, map[string]any{"action": "create", "content": skillsMD("nope", "x", "y")})
		run := runSkills(t, skillsRunOptions{handler: approvalsOf(false), calls: []loopCall{call}})

		run.requireResults(t, 1)
		if !strings.Contains(run.results[0].Content, "Approval denied") {
			t.Errorf("result = %q, want the denial", run.results[0].Content)
		}
		if _, found, err := run.wc.core.SkillHub().Get(t.Context(), "nope"); err != nil || found {
			t.Errorf("Skill(nope) = %v, %v, want no skill", found, err)
		}
		if slices.Contains(run.agent.Profile().Skills, "nope") {
			t.Errorf("the agent's skills = %v, want nope not enabled", run.agent.Profile().Skills)
		}
	})

	t.Run("fails closed without an interaction handler", func(t *testing.T) {
		call := manageCall(t, map[string]any{"action": "create", "content": skillsMD("nope", "x", "y")})
		run := runSkills(t, skillsRunOptions{calls: []loopCall{call}})

		run.requireResults(t, 1)
		if !strings.Contains(run.results[0].Content, "no interactive user") {
			t.Errorf("result = %q, want the no interactive user text", run.results[0].Content)
		}
		if _, found, err := run.wc.core.SkillHub().Get(t.Context(), "nope"); err != nil || found {
			t.Errorf("Skill(nope) = %v, %v, want no skill", found, err)
		}
	})

	t.Run("deleting a skill disables it for every agent that had it, not just the one that deleted it", func(t *testing.T) {
		var other *agent.Agent
		run := runSkills(t, skillsRunOptions{
			handler: approvalsOf(true),
			skills:  []string{"site-diary", "plan"},
			prepare: func(t *testing.T, core *aec.AgentCore) {
				customSkill(t, core, skillsMD("site-diary", "Write the site diary.", "1. List crews."))
			},
			afterSpawn: func(t *testing.T, core *aec.AgentCore) {
				var err error
				other, err = core.SpawnAgent(t.Context(), profile.AgentProfile{
					ID: "other-agent", Name: "other", SystemPrompt: "Other.", Tools: []string{}, Skills: []string{"site-diary", "plan"},
					Compactor: testCompactor(), Memory: loopMemoryOff(),
				})
				if err != nil {
					t.Fatalf("SpawnAgent(other-agent): %v", err)
				}
			},
			calls: []loopCall{{"skill_manage", `{"action":"delete","name":"site-diary"}`}},
		})
		core := run.wc.core

		if _, found, err := core.SkillHub().Get(t.Context(), "site-diary"); err != nil || found {
			t.Errorf("Skill(site-diary) = %v, %v, want it deleted", found, err)
		}
		if got := run.agent.Profile().Skills; !slices.Equal(got, []string{"plan"}) {
			t.Errorf("the deleting agent's skills = %v, want [plan]", got)
		}
		if got := other.Profile().Skills; !slices.Equal(got, []string{"plan"}) {
			t.Errorf("the other agent's skills = %v, want [plan]", got)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			saved, err := core.PersistenceHub().Agents.Load(t.Context(), "other-agent")
			if err == nil && saved != nil && slices.Equal(saved.Skills, []string{"plan"}) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the saved profile of the other agent = %+v, %v, want the skills [plan]", saved, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("skill_manage keeps the workspace copy in line: files a skill no longer has are removed", func(t *testing.T) {
		run := runSkills(t, skillsRunOptions{
			handler: approvalsOf(true),
			skills:  []string{"site-diary"},
			prepare: func(t *testing.T, core *aec.AgentCore) {
				customSkill(t, core, skillsMD("site-diary", "Write the site diary.", "1. List crews."), skill.File{Path: "assets/template.md", Content: "# Diary"})
			},
			calls: []loopCall{
				{"skill_view", `{"name":"site-diary"}`},
				{"skill_manage", `{"action":"remove_file","name":"site-diary","path":"assets/template.md"}`},
			},
		})

		run.requireResults(t, 2)
		if !strings.Contains(run.results[1].Content, "updated in your workspace at .skills/site-diary/") {
			t.Errorf("result 1 = %q", run.results[1].Content)
		}
		if got, ok := run.inSandbox(t, ".skills/site-diary/assets/template.md"); ok {
			t.Errorf("the removed file is still in the workspace copy: %q", got)
		}
		if got, ok := run.inSandbox(t, ".skills/site-diary/SKILL.md"); !ok || !strings.Contains(got, "1. List crews.") {
			t.Errorf("the copied SKILL.md = %q (present %v)", got, ok)
		}
	})

	t.Run("skill_manage patches, writes files, reports validation problems and deletes", func(t *testing.T) {
		run := runSkills(t, skillsRunOptions{
			handler: approvalsOf(true),
			skills:  []string{"site-diary", "plan"},
			prepare: func(t *testing.T, core *aec.AgentCore) {
				customSkill(t, core, skillsMD("site-diary", "Write the site diary.", "1. List crews.\n2. Note weather."))
			},
			calls: []loopCall{
				{"skill_manage", `{"action":"patch","name":"site-diary","old_string":"Note weather.","new_string":"Note weather and delays."}`},
				{"skill_manage", `{"action":"write_file","name":"site-diary","path":"assets/template.md","content":"# Diary"}`},
				{"skill_manage", `{"action":"patch","name":"site-diary","old_string":"not there","new_string":"x"}`},
				{"skill_manage", `{"action":"rewrite","name":"site-diary","content":"no frontmatter"}`},
				{"skill_manage", `{"action":"patch","name":"plan","old_string":"Plan","new_string":"Plan!"}`},
				{"skill_manage", `{"action":"remove_file","name":"site-diary","path":"assets/template.md"}`},
				{"skill_manage", `{"action":"delete","name":"site-diary"}`},
			},
		})
		core := run.wc.core

		run.requireResults(t, 7)
		contains := func(i int, want string) {
			t.Helper()
			if !strings.Contains(run.results[i].Content, want) {
				t.Errorf("result %d = %q, want it to contain %q", i, run.results[i].Content, want)
			}
		}
		contains(0, `Patched SKILL.md of skill "site-diary" (version 2`)
		contains(0, "updated in your workspace at .skills/site-diary/")
		contains(1, `Added assets/template.md in skill "site-diary" (version 3, 1 extra file)`)
		for i, want := range map[int]string{2: "old_string was not found", 3: "(invalid)", 4: "(read_only)"} {
			if !isErrorResult(run.results[i]) {
				t.Errorf("result %d = %+v, want an error", i, run.results[i])
			}
			contains(i, want)
		}
		contains(5, `Removed assets/template.md from skill "site-diary"`)
		if got, want := run.results[6].Content, `Deleted skill "site-diary". Its folder was removed from your workspace at .skills/site-diary/.`; got != want {
			t.Errorf("result 6 = %q, want %q", got, want)
		}
		if left := run.filesUnder(t, ".skills/site-diary"); len(left) != 0 {
			t.Errorf("files of the deleted skill are left in the workspace: %v", left)
		}

		if _, found, err := core.SkillHub().Get(t.Context(), "site-diary"); err != nil || found {
			t.Errorf("Skill(site-diary) = %v, %v, want it deleted", found, err)
		}
		if got := run.agent.Profile().Skills; !slices.Equal(got, []string{"plan"}) {
			t.Errorf("the agent's skills = %v, want [plan]", got)
		}
	})
}
