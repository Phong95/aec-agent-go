// Package secrets holds the secret tools (A-101): list_secrets (read-only, metadata only)
// and manage_secret (consequential: creates, changes and deletes the agent's own
// secrets). http_request, the tool that sends a secret to an API, is a web tool
// (web.HTTPRequestTool); terminal and run_code take secrets as environment variables.
//
// The tools see the agent's secrets only through toolhub.Context.Secrets, which is already
// scoped to the agent; a value comes from the user through the interaction handler's
// toolhub.SecretRequester (a masked field) and never through the model. The tool texts are
// those of the core/ts tools of the same names.
package secrets
