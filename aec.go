// Package aec is the entry point of the AEC Agent Go SDK.
//
// It holds the two ways to start: CreateAgent builds one ready agent from an
// AgentConfig, and AgentCore wires every hub together (LLM providers, tools,
// skills, sandboxes, artifacts, persistence, events and trace) for an
// application that runs many agents. Everything else lives in the package that
// owns it, and you import that package directly: the agent and its chat options
// in components/agent, tool authoring in components/toolhub, the
// built-in tools under components/toolhub/builtin/..., shared types and errors in
// kernel. The root does not re-export them.
//
// AgentCore is the API; there is no Kernel interface. Its lifecycle (Initialize,
// Dispose), Use* (UseLLMProvider, UseTool, UsePersistence, UseSkill,
// UseSandboxProvider, UseCommand) and agent methods (SpawnAgent, Agent, ListAgents,
// DisposeAgent, RemoveAgent) cover what an application needs. Lookups go through
// the hub accessors: LLM().Get and Primary, ToolHub().Get and List (definitions,
// never handlers, so a tool runs only through toolhub.ToolHub.Authorize and
// Authorized.Execute), SkillHub().Get and List.
//
// Importing the root links no vendor SDK and no host or Docker sandbox: the LLM
// providers (llm/providers/anthropic, openaicompat, bedrock, openaisub, or
// providers/all for the four) register themselves when imported, the sandbox
// backends are sandbox/host and sandbox/docker, and the test doubles are in
// aecmock. tests/testdata/api/aec.txt is the snapshot of the exported API
// (TestAPISnapshot).
package aec

// Version is the SDK version of the Go module.
const Version = "0.1.0"
