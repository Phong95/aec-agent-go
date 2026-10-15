package agentloop_test

import (
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-132: the link to what a tool call produced (e.g. the artifact it saved) travels on
// its ToolResult - in the tool_result chunk and in the saved conversation - so the chat
// can open it (also after a reload). An error result never carries one.

var resultLink = kernel.ToolResultLink{URI: "artifact://art-1", Name: "landing.html", Type: "text/html", Version: 1}

// streamOneCall runs one make_page call returning out and gives the tool_result chunk's
// result and the stored result.
func streamOneCall(t *testing.T, out toolhub.Output) (streamedResult, storedResult kernel.ToolResult) {
	t.Helper()
	page := constTool("make_page", out)
	r := newRig(t, rigOptions{prof: loopProfile(zeroInt(), zeroInt()), handlers: []toolhub.Handler{page}},
		toolTurn(callOf("tc-1", "make_page", "{}")),
		aecmock.ScriptedTurn{Content: "Your page [landing.html](artifact://art-1) is ready."},
	)
	chunks, err := r.stream(t, "Build me a landing page")
	if err != nil {
		t.Fatal(err)
	}
	var streamed []kernel.ToolResult
	for _, c := range chunks {
		if c.Type == kernel.ChunkToolResult {
			streamed = append(streamed, *c.ToolResult)
		}
	}
	if len(streamed) != 1 {
		t.Fatalf("%d tool_result chunks, want 1", len(streamed))
	}
	var stored []kernel.ToolResult
	for _, m := range r.conv.Messages() {
		stored = append(stored, m.ToolResults...)
	}
	if len(stored) != 1 {
		t.Fatalf("%d stored results, want 1", len(stored))
	}
	return streamed[0], stored[0]
}

func TestStream_ToolResultCarriesLink(t *testing.T) {
	l := resultLink
	streamed, stored := streamOneCall(t, toolhub.Output{Content: "Saved landing.html", Link: &l})
	if streamed.Link == nil || !reflect.DeepEqual(*streamed.Link, resultLink) {
		t.Fatalf("streamed link = %+v, want %+v", streamed.Link, resultLink)
	}
	if stored.Link == nil || !reflect.DeepEqual(*stored.Link, resultLink) {
		t.Fatalf("stored link = %+v, want %+v", stored.Link, resultLink)
	}
	if streamed.Link == &l {
		t.Fatal("the result shares the tool's Link pointer, want a copy")
	}
}

func TestStream_ErrorResultDropsLink(t *testing.T) {
	l := resultLink
	streamed, stored := streamOneCall(t, toolhub.Output{Content: "failed", IsError: boolRef(true), Retryable: boolRef(false), Link: &l})
	if streamed.IsError == nil || !*streamed.IsError {
		t.Fatalf("streamed = %+v, want an error result", streamed)
	}
	if streamed.Link != nil || stored.Link != nil {
		t.Fatalf("link = %+v / %+v, want none on an error result", streamed.Link, stored.Link)
	}
}

func TestStream_ResultWithoutLinkOmitsIt(t *testing.T) {
	streamed, _ := streamOneCall(t, toolhub.Output{Content: "plain"})
	if streamed.Link != nil {
		t.Fatalf("link = %+v, want none", streamed.Link)
	}
	if got := mustEncode(t, streamed); got != `{"toolCallId":"tc-1","content":"plain"}` {
		t.Fatalf("encoded = %s, want no link key", got)
	}
}
