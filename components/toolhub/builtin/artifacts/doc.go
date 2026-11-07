// Package artifacts holds the artifact tools (A-62): save_artifact,
// update_artifact, read_artifact and list_artifacts. An artifact tracks one file in the
// agent's workspace: the file is the deliverable, and the artifact adds a name, a type,
// versions and provenance, so later turns, other agents and the user's Artifacts panel
// can refer to it by name instead of re-pasting its content into the conversation.
//
// Update (A-209): the four tools are one tool, "artifact" (Tool, built by NewTool), with a
// required action: save, update or list. read is dropped: the model reads the tracked
// file with read_file, and list shows each file's live status (changed / missing). Its
// risk and sandbox operation follow the action (RiskFor, SandboxOperationFor), and the
// guidance moved into the essential "artifacts" skill. Profiles and subagent types that
// still list the old names get "artifact" (toolhub.RenamedToolNames; RetiredToolNames
// lists them).
//
// The tools see two things only: the agent's artifacts, through toolhub.Context.GetArtifacts
// (a view that is already scoped to the agent and to the session), and the agent's
// sandbox, through toolhub.Context.GetSandbox. They never take an agent id from the model's
// arguments (T-06-24, T-11-13), never read another agent's artifacts, and never reach
// the host: the package imports no os, os/exec, path/filepath, syscall or network
// package, and the architecture test guards it with no exception (T-05-08, T-11-07).
//
// NewTools builds the four tools with one Options value, in a fixed order, and
// ToolNames returns their names for an agent's tool allowlist. Each tool has its own
// constructor too (NewSaveTool and so on), for a host that wants a subset.
//
// save_artifact and update_artifact emit artifact:saved and artifact:updated through
// toolhub.Context.Emit. Every output, error text, hint and metadata key is pinned by the
// recorded corpus of the wire harness (tests/testdata/golden/builtin-artifact-tools.json).
// (Update (A-209): the save and update actions emit them.)
//
// Like every tool, the handlers of this package never return an error from Execute: a
// problem the model can act on is an error output, so the harness shows it to the model.
package artifacts
