// Package preview holds the preview tools (A-124): preview, list_apps and open_in_app.
// They show the user a web page or app from the agent's workspace, live next to the
// chat: each returns a ToolResult link (A-132) with a "preview://<workspace path>[?query]"
// URI, and the host turns it into a signed URL on its preview origin when the user
// opens it - so a link in a saved conversation never expires.
//
// An app is a folder apps/<app>/ in the agent's workspace with an app.json:
// { "name", "description", "entry": "index.html", "opens": [".ifc"],
// "openUrl": "index.html?file={path}" }. open_in_app fills {path} with the file's URL
// relative to the app folder, so the app fetches the file from the same preview origin.
//
// The tools are read-only and reach files only through the agent's sandbox
// (toolhub.Context.GetSandbox); the package imports no host package and the
// architecture test guards it with no exception. Like core/ts
// (components/tool-hub/built-in-tools/preview), the texts are the same in both SDKs.
//
// Update (A-124 phase 2): the service tools start_service, stop_service, list_services
// and service_logs run dev servers in the sandbox (sandboxhub.ServicesView, capability
// "services"); start_service returns a "service://<name>/" link that the host turns
// into a URL on an origin of the service's own. start_service and stop_service are
// risk draft (sandbox operation exec), the other two read-only.
package preview
