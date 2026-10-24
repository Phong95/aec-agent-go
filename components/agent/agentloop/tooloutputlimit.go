package agentloop

import (
	"context"
	"regexp"
	"strconv"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-129 item 4: cap tool output at the source. A tool result longer than
// LoopConfig.MaxToolResultChars keeps its head and tail; the middle is replaced by a
// note, which names the file holding the full output when the loop could save it
// (tool-output-limit.ts of core/ts). Lengths are UTF-16 code units, as in core/ts.
// ---------------------------------------------------------------------------

// toolOutputDir is where a cut tool output is saved, in an isolated sandbox.
const toolOutputDir = ".tool-output"

// toolOutputHeadShare is the share of the limit kept from the start of the output; the
// rest comes from the end.
const toolOutputHeadShare = 0.6

var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// toolOutputPath is the file a cut output of the call toolCallID is saved to (ids are
// made path-safe).
func toolOutputPath(toolCallID string) string {
	name := unsafePathChars.ReplaceAllString(toolCallID, "_")
	if name == "" {
		name = "output"
	}
	return toolOutputDir + "/" + name + ".txt"
}

// cutToolOutput returns content cut to about limit UTF-16 code units: its head and tail
// with a note in between that says how much was left out and, when savedPath is not
// empty, where the full output is. Content within the limit (or a limit of 0) is
// returned as it is.
func cutToolOutput(content string, limit int, savedPath string) string {
	length := kernel.UTF16Len(content)
	if limit <= 0 || length <= limit {
		return content
	}
	headLength := int(float64(limit) * toolOutputHeadShare)
	tailLength := limit - headLength
	head := jsstr.TruncateUTF16(content, headLength)
	tail := lastUTF16(content, tailLength)
	omitted := length - kernel.UTF16Len(head) - kernel.UTF16Len(tail)
	where := "Run the tool again with a narrower request if you need them."
	if savedPath != "" {
		where = "The full output (" + strconv.Itoa(length) + " characters) is saved in " + savedPath +
			" — read the part you need with read_file (start_line/end_line) or search_files."
	}
	return head + "\n\n[… " + strconv.Itoa(omitted) + " characters cut here to save context space. " + where + "]\n\n" + tail
}

// lastUTF16 is the longest suffix of s of at most n UTF-16 code units that starts on a
// character boundary (a surrogate pair that would be split is dropped whole, as in
// jsstr.TruncateUTF16).
func lastUTF16(s string, n int) string {
	if n <= 0 {
		return ""
	}
	units := 0
	runes := []rune(s)
	start := len(runes)
	for start > 0 {
		w := 1
		if runes[start-1] >= 0x10000 {
			w = 2
		}
		if units+w > n {
			break
		}
		units += w
		start--
	}
	return string(runes[start:])
}

// limitOutput (A-129 item 4) caps a result the model is given: a result longer than
// LoopConfig.MaxToolResultChars keeps its head and tail. In an isolated sandbox the full
// output is saved to .tool-output/<call id>.txt first and the note names it; on a host
// folder (or with no sandbox) nothing is written there unasked, and the note says to
// narrow the request instead.
func (l *Loop) limitOutput(ctx context.Context, run *toolRun, call kernel.ToolCall, result kernel.ToolResult) kernel.ToolResult {
	limit := profile.ResolveLoopConfig(l.deps.Profile().LoopConfig).MaxToolResultChars
	if limit <= 0 || kernel.UTF16Len(result.Content) <= limit {
		return result
	}
	savedPath := ""
	if run.tc.GetSandbox != nil {
		if view, err := run.tc.GetSandbox(ctx); err == nil && !nilcheck.IsNil(view) && view.Traits().Isolated {
			if written, err := view.WriteFile(ctx, toolOutputPath(call.ID), result.Content, sandboxhub.WriteOptions{}); err == nil {
				savedPath = written.Path
			}
		}
	}
	result.Content = cutToolOutput(result.Content, limit, savedPath)
	return result
}
