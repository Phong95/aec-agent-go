package agent

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/variablehub"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
)

// A-215: Fill (job prompts) keeps the names the agent can't use; the index cuts long values.
func TestVariableManagerFillAndIndex(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryVariableStore()
	hub := variablehub.NewVariableHub(func() persistence.VariableStore { return store })
	for _, in := range []variablehub.CreateInput{
		{Definition: variable.Definition{Name: "REGION", Value: "eu"}},
		{Definition: variable.Definition{Name: "LONG", Value: strings.Repeat("x", 400)}, OwnerAgentID: "a1"},
	} {
		if _, err := hub.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	p := profile.AgentProfile{ID: "a1", Variables: &variable.Config{Use: []string{"REGION"}}}
	m := newVariableManager(variableManagerDeps{agentID: "a1", profile: func() profile.AgentProfile { return p }, hub: hub, canManage: func() bool { return false }})
	got, err := m.Fill(ctx, "Deploy to {{var:REGION}} and {{var:NOPE}}")
	if err != nil || got != "Deploy to eu and {{var:NOPE}}" {
		t.Fatalf("fill = %q, %v", got, err)
	}
	index, ok := m.buildIndex(ctx, false)
	if !ok || !strings.Contains(index, `REGION = "eu" (global)`) || !strings.Contains(index, "… (400 characters)") {
		t.Fatalf("index = %q", index)
	}
	none := newVariableManager(variableManagerDeps{agentID: "a2", profile: func() profile.AgentProfile { return profile.AgentProfile{ID: "a2"} }, hub: hub, canManage: func() bool { return false }})
	if _, ok := none.buildIndex(ctx, false); ok {
		t.Fatal("an agent with no variables and no tool got an index")
	}
}
