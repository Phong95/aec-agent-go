// Package secret is the contract of secrets (A-101): sensitive keys (API keys, tokens) an
// agent can use but never see - the secret-hub/types.ts, rules.ts and
// secret-manager/types.ts of core/ts, and the SecretError of its kernel.
//
// The model works with secret NAMES; a tool that supports secrets puts the value in at
// the moment it runs (http_request headers/body, terminal/run_code environment). Values
// are sealed with envelope encryption before they reach a store (package secrethub).
//
// It holds the data types (Definition, Info, Stored, Use, Status, Config, AgentInfo), the
// Secrets interface tools reach an agent's secrets through (toolhub.Context.Secrets), the
// Error, and the rules shared by the SecretHub, each agent's secret manager and the tools:
// names, hosts, tools, {{secret:NAME}} placeholders and redaction. It is shared by
// secrethub, persistence, profile, toolhub, agentloop, agent, the built-in tools and the
// root, so it imports no component.
package secret
