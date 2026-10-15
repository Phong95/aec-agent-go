package workspace_test

import (
	"fmt"

	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/workspace"
)

// No built-in tool is registered automatically: pass the ones the agent should
// have. The workspace tools reach files and commands only through the agent's
// sandbox.
func ExampleNewTools() {
	for _, t := range workspace.NewTools(workspace.Options{}) {
		fmt.Println(t.Definition().Name)
	}
	// Output:
	// read_file
	// write_file
	// edit_file
	// list_files
	// search_files
	// terminal
	// run_code
}
