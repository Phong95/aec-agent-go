package toolhub_test

import (
	"context"
	"fmt"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A typed tool: the arguments are decoded into a struct, the schema is built with
// the schema helpers and the result is a Text output. The hub runs it only
// through the policy, which checks the arguments against the schema first.
func ExampleNewTypedTool() {
	type greetArgs struct {
		Name  string  `json:"name"`
		Times float64 `json:"times"`
	}
	greet := toolhub.NewTypedTool(toolhub.Definition{
		Name:        "greet",
		Description: "Greet someone by name.",
		Parameters: toolhub.Object(
			toolhub.Prop("name", toolhub.String("Who to greet")),
			toolhub.Prop("times", toolhub.Number("How many times (optional)")),
		).Require("name"),
		Risk: toolhub.RiskReadOnly,
	}, func(_ context.Context, a greetArgs, _ toolhub.Context) (toolhub.Output, error) {
		if a.Times < 1 {
			a.Times = 1
		}
		return toolhub.Text(fmt.Sprintf("Hello, %s! (x%v)", a.Name, a.Times)), nil
	})

	hub := toolhub.NewToolHub()
	if err := hub.Register(greet); err != nil {
		fmt.Println(err)
		return
	}
	ctx := context.Background()
	call, decision, err := hub.Authorize(ctx, "greet", kernel.Arguments(`{"name":"Ada","times":2}`), toolhub.Context{}, nil)
	if err != nil || !decision.Allowed {
		fmt.Println("denied:", err, decision.Reason)
		return
	}
	out, err := call.Execute(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(greet.Name())
	fmt.Println(out.Content)
	// Output:
	// tool:greet
	// Hello, Ada! (x2)
}
