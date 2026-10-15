// Package workspace holds the workspace tools (A-61): read_file,
// write_file, edit_file, list_files, search_files, terminal and run_code. Files and
// commands are reached ONLY through the agent's sandboxhub.View (toolhub.Context.GetSandbox),
// never through the host: the package imports no os, os/exec, path/filepath, syscall or
// network package, and the architecture test guards it with no exception (T-05-08,
// T-11-07). The sandbox enforces the paths, the limits and the isolation; the Options
// of this package only bound what is shown to the model.
//
// NewTools builds all seven tools with one Options value, in a fixed order, and
// ToolNames returns their names for an agent's tool allowlist. Each tool has its own
// constructor too (NewReadFileTool and so on), for a host that wants a subset.
//
// Every output, error text, hint and metadata key is pinned by the recorded corpus of
// the wire harness (tests/testdata/golden/builtin-workspace-*.json),
// including the number formatting (JS toFixed and Math.round, from internal/jsstr) and
// the UTF-8 byte cuts of the command output.
//
// Like every tool, the handlers of this package never return an error from Execute: a
// problem the model can act on is an error output, so the harness shows it to the model.
package workspace
