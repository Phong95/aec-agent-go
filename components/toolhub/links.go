package toolhub

import "github.com/Phong95/aec-agent-go/kernel"

// CiteLinkNote (A-132) is appended to a tool's result content: it asks the model to
// name what the call produced as a markdown link with the link's URI in its reply
// ([name](artifact://id)), which the chat turns into a link that opens it. Any tool
// that sets Output.Link uses it, so every kind of link is cited the same way.
func CiteLinkNote(link kernel.ToolResultLink) string {
	return "\nWhen you tell the user about it, write its name as the link [" + link.Name + "](" + link.URI + ") " +
		"— the chat opens it from that link."
}
