// Package variable is the contract of variables (A-215): named configuration values that
// are not secret (a base URL, a region, a project id), like GitHub Actions' vars next to
// its secrets - the variable-hub/types.ts, rules.ts and variable-manager/types.ts of
// core/ts, and the VariableError of its kernel.
//
// A tool puts a value in wherever the model wrote {{var:NAME}} (http_request, MCP server
// headers / env, job prompts) or, for terminal / run_code, as an environment variable the
// call names in variables. Unlike a secret the model sees the value (in
// <available_variables>); it is stored as it is and never redacted.
//
// It holds the data types (Definition, Info, Config, AgentInfo, Patch), the Variables
// interface tools reach an agent's variables through (toolhub.Context.Variables), the
// Error, and the rules shared by the VariableHub, each agent's variable manager and the
// tools. It imports no component.
package variable
