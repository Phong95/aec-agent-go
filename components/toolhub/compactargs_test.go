package toolhub_test

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-129: same cases as core/ts tests/unit/context-token-optimisation.test.ts.

func TestWithContentReferenceReplacesLargeContent(t *testing.T) {
	args := kernel.Arguments(`{"path":"site/index.html","content":"` + strings.Repeat("x", toolhub.CompactContentMinChars) + `"}`)
	got := toolhub.WithContentReference(args, toolhub.ContentWritten{Size: "1 KB", Path: "site/index.html", ReadWith: "read_file"})
	want := `{"path":"site/index.html","content":"[1 KB written to site/index.html — read it with read_file if needed]"}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestWithContentReferenceLeavesSmallOrMissingContent(t *testing.T) {
	written := toolhub.ContentWritten{Size: "1 B", Path: "a.txt", ReadWith: "read_file"}
	for _, args := range []string{`{"path":"a.txt","content":"hi"}`, `{"from_path":"a.txt"}`} {
		if got := toolhub.WithContentReference(kernel.Arguments(args), written); got != nil {
			t.Errorf("%s: got %s, want nil", args, got)
		}
	}
}
