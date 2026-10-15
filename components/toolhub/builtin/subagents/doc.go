// Package subagents holds spawn_subagent (A-105): the tool a parent agent hands a
// bounded task to one of its subagents with (another agent profile on its allowlist,
// profile.AgentProfile.Subagents), waiting for the child's final report.
//
// The tool sees the agent's subagents only through toolhub.Context.Subagents, which is
// already scoped to the agent. The child starts in a fresh session of its own: it sees
// only the brief, never the parent's conversation, and its report comes back as
// untrusted data for the parent to check and use in its own reply.
//
// The tool texts are those of the core/ts tool of the same name.
package subagents
