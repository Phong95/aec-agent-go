package conformance_test

import (
	"context"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel"
)

func TestToolPolicyDefault(t *testing.T) {
	conformance.ToolPolicy(t,
		func(*testing.T) toolhub.Policy { return toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{}) },
		func(*testing.T) toolhub.Policy {
			return toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{
				AuthorizationHook: func(context.Context, string, kernel.Arguments, toolhub.Context) (toolhub.Decision, error) {
					return toolhub.Deny("Denied by test hook"), nil
				},
			})
		},
	)
}

func TestToolHandlerMockTools(t *testing.T) {
	cases := []struct {
		name string
		new  conformance.ToolHandlerFactory
		args string
	}{
		{"schedule_lookup", func(*testing.T) toolhub.Handler { return &aecmock.ScheduleLookupTool{} }, `{"projectId":"test-project","scope":"foundation"}`},
		{"budget_query", func(*testing.T) toolhub.Handler { return &aecmock.BudgetQueryTool{} }, `{"projectId":"test-project"}`},
		{"document_search", func(*testing.T) toolhub.Handler { return &aecmock.DocumentSearchTool{} }, `{"query":"rebar spacing"}`},
		{
			"send_rfi", func(*testing.T) toolhub.Handler { return &aecmock.SendRfiTool{} },
			`{"projectId":"test-project","recipient":"consultant@example.com","subject":"Rebar spacing","question":"Is 150 mm acceptable?"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, err := kernel.NewArguments([]byte(c.args))
			if err != nil {
				t.Fatal(err)
			}
			conformance.ToolHandler(t, c.new, args)
		})
	}
}
