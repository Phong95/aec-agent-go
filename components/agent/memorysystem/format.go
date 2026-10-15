package memorysystem

import (
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
)

// The recall block that is appended to a turn's system prompt: a heading, one line
// of guidance that tells the model the memories can be outdated, and one line per
// memory. TestPromptsMatchGolden pins the strings against the recorded golden
// prompts.
const (
	recallHeading  = "## Recalled memories"
	recallGuidance = "Long-term memories from earlier conversations that may be relevant to this message. They can be outdated " + emDash + " if the user says otherwise now, trust the user."
)

// formatRecalledMemories renders the recalled records as the system-prompt section:
// "- [kind] subject - key: value", or "- [kind] key: value" when the subject is empty.
func formatRecalledMemories(results []memory.RetrievalResult) string {
	lines := make([]string, 0, len(results)+2)
	lines = append(lines, recallHeading, recallGuidance)
	for _, r := range results {
		label := r.Record.Key
		if r.Record.Subject != "" {
			label = r.Record.Subject + " " + emDash + " " + r.Record.Key
		}
		lines = append(lines, "- ["+string(r.Record.Kind)+"] "+label+": "+r.Record.Value)
	}
	return strings.Join(lines, "\n")
}
