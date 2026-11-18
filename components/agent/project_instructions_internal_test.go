package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// fakeInstructionsWorkspace is a session workspace over one in-memory view: folders are
// the session's folders (none = no folders), err fails GetSessionSandbox (an offline app).
type fakeInstructionsWorkspace struct {
	view    sandboxhub.View
	folders []string
	err     error
}

func (w *fakeInstructionsWorkspace) WorkspaceFolders(string) ([]string, bool) {
	return w.folders, len(w.folders) > 0
}

func (w *fakeInstructionsWorkspace) GetSessionSandbox(context.Context, string) (sandboxhub.View, error) {
	if w.err != nil {
		return nil, w.err
	}
	return w.view, nil
}

// instructionsRig is a ProjectInstructions over an in-memory sandbox whose working
// directory is /workspace/agents/a, with a clock the test moves.
type instructionsRig struct {
	t     *testing.T
	p     *ProjectInstructions
	ws    *fakeInstructionsWorkspace
	view  sandboxhub.View
	cfg   profile.LoopConfig
	clock time.Time
	ms    atomic.Int64
	bus   events.Bus
}

const instrRoot = "/workspace/agents/a"

func newInstructionsRig(t *testing.T) *instructionsRig {
	t.Helper()
	r := &instructionsRig{t: t, clock: time.Unix(1_000, 0)}
	r.ms.Store(1_000)
	env, err := aecmock.NewInMemoryProvider(aecmock.WithClock(func() int64 { return r.ms.Add(1) })).Create(t.Context(),
		sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "mem", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}
	if r.view, err = env.ForAgent(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	r.ws = &fakeInstructionsWorkspace{view: r.view, folders: []string{instrRoot}}
	r.bus = events.New()
	r.p = newProjectInstructions("a", r.ws, func() profile.ResolvedLoopConfig { return profile.ResolveLoopConfig(&r.cfg) }, r.bus)
	r.p.now = func() time.Time { return r.clock }
	return r
}

func (r *instructionsRig) write(p, content string) {
	r.t.Helper()
	if _, err := r.view.WriteFile(r.t.Context(), p, content, sandboxhub.WriteOptions{}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *instructionsRig) index(msgs ...kernel.Message) string {
	r.t.Helper()
	block, _ := r.p.Index(r.t.Context(), "s1", msgs)
	return block
}

func (r *instructionsRig) afterTool(tool, p string) string {
	return r.p.AfterTool(r.t.Context(), "s1", tool, kernel.Arguments(`{"path":"`+p+`"}`))
}

// later moves the clock past the recheck window.
func (r *instructionsRig) later() { r.clock = r.clock.Add(instructionsRecheck + time.Second) }

func TestProjectInstructions_RootFile(t *testing.T) {
	r := newInstructionsRig(t)
	if block := r.index(); block != "" {
		t.Fatalf("no AGENTS.md: %q", block)
	}
	r.write("AGENTS.md", "Use tabs.\n")
	r.later()
	block := r.index()
	for _, want := range []string{"<project_instructions>", `<file path="` + instrRoot + `/AGENTS.md">`, "Use tabs.", "wins where they disagree", "What the user asks in the chat wins", "</project_instructions>"} {
		if !strings.Contains(block, want) {
			t.Fatalf("block misses %q:\n%s", want, block)
		}
	}
	if again := r.index(); again != block {
		t.Fatal("the block must be byte-identical while nothing changed")
	}
}

func TestProjectInstructions_OnlyAgentsMd(t *testing.T) {
	r := newInstructionsRig(t)
	r.write("CLAUDE.md", "claude rules")
	r.write("GEMINI.md", "gemini rules")
	r.write("AGENTS.override.md", "override rules")
	if block := r.index(); block != "" {
		t.Fatalf("only AGENTS.md is read: %q", block)
	}
}

func TestProjectInstructions_NoFoldersOrOff(t *testing.T) {
	r := newInstructionsRig(t)
	r.write("AGENTS.md", "rules")
	off := false
	r.cfg.ProjectInstructions = &off
	if block := r.index(); block != "" {
		t.Fatalf("off: %q", block)
	}
	if note := r.afterTool("read_file", "x/y.txt"); note != "" {
		t.Fatalf("off: note %q", note)
	}
	r.cfg.ProjectInstructions = nil
	r.ws.folders = nil
	if block := r.index(); block != "" {
		t.Fatalf("a session without folders: %q", block)
	}
}

func TestProjectInstructions_SeveralFoldersInOrder(t *testing.T) {
	r := newInstructionsRig(t)
	r.write("web/AGENTS.md", "web rules")
	r.write("api/AGENTS.md", "api rules")
	r.ws.folders = []string{instrRoot + "/api", instrRoot + "/web"}
	block := r.index()
	api, web := strings.Index(block, "api rules"), strings.Index(block, "web rules")
	if api < 0 || web < 0 || api > web {
		t.Fatalf("both folders, the working one first:\n%s", block)
	}
}

func TestProjectInstructions_ByteLimit(t *testing.T) {
	r := newInstructionsRig(t)
	r.write("one/AGENTS.md", strings.Repeat("a", 30))
	r.write("two/AGENTS.md", strings.Repeat("b", 30))
	r.write("three/AGENTS.md", "c")
	r.ws.folders = []string{instrRoot + "/one", instrRoot + "/two", instrRoot + "/three"}
	limit := 40
	r.cfg.ProjectInstructionsMaxBytes = &limit
	block := r.index()
	if !strings.Contains(block, strings.Repeat("a", 30)) || !strings.Contains(block, strings.Repeat("b", 10)+"\n[cut at the instructions limit") || strings.Contains(block, strings.Repeat("b", 11)) {
		t.Fatalf("the second file is cut at the limit:\n%s", block)
	}
	if !strings.Contains(block, "Left out over the 40-byte instructions limit") || !strings.Contains(block, instrRoot+"/three/AGENTS.md") {
		t.Fatalf("the third file is named as left out:\n%s", block)
	}
	state := r.p.State(t.Context(), "s1")
	statuses := []string{}
	for _, f := range state.Files {
		statuses = append(statuses, f.Status)
	}
	if strings.Join(statuses, ",") != "loaded,cut,left_out" || state.MaxBytes != 40 {
		t.Fatalf("state = %+v", state)
	}
}

func TestProjectInstructions_Recheck(t *testing.T) {
	r := newInstructionsRig(t)
	r.write("AGENTS.md", "v1")
	if !strings.Contains(r.index(), "v1") {
		t.Fatal("v1")
	}
	r.write("AGENTS.md", "v2 longer")
	if !strings.Contains(r.index(), "v1") {
		t.Fatal("within the recheck window the cached file stays")
	}
	r.later()
	if block := r.index(); !strings.Contains(block, "v2 longer") || strings.Contains(block, "v1") {
		t.Fatalf("after the window the change shows: %s", block)
	}
	// The agent writing the folder's AGENTS.md is picked up at once.
	r.write("AGENTS.md", "v3")
	if note := r.afterTool("write_file", "AGENTS.md"); note != "" {
		t.Fatalf("a write brings no note: %q", note)
	}
	if !strings.Contains(r.index(), "v3") {
		t.Fatal("the written file must be read again at the next call")
	}
}

func TestProjectInstructions_Unreadable(t *testing.T) {
	r := newInstructionsRig(t)
	r.ws.err = errors.New("offline")
	block := r.index()
	if !strings.Contains(block, "Could not be read") || !strings.Contains(block, instrRoot+"/AGENTS.md") {
		t.Fatalf("offline with nothing cached: %s", block)
	}
	// Once read, an offline app keeps the cached file.
	r.ws.err = nil
	r.write("AGENTS.md", "cached rules")
	r.later()
	if !strings.Contains(r.index(), "cached rules") {
		t.Fatal("online")
	}
	r.ws.err = errors.New("offline")
	r.later()
	if block := r.index(); !strings.Contains(block, "cached rules") || strings.Contains(block, "Could not be read") {
		t.Fatalf("offline keeps the cache: %s", block)
	}
}

func TestProjectInstructions_SubFolderOnDemand(t *testing.T) {
	r := newInstructionsRig(t)
	r.write("AGENTS.md", "root rules")
	r.write("packages/api/AGENTS.md", "api rules")
	r.write("packages/web/AGENTS.md", "web rules")
	r.write("packages/api/src/x.ts", "x")
	r.write("packages/web/y.ts", "y")
	var announced [][]string
	events.Subscribe(r.bus, events.SessionInstructionsChanged, func(_ kernel.Event, p events.SessionInstructionsChangedPayload) {
		announced = append(announced, p.Paths)
	})

	if block := r.index(); strings.Contains(block, "api rules") {
		t.Fatal("sub-folder files are not loaded up front")
	}
	note := r.afterTool("read_file", "packages/api/src/x.ts")
	if !strings.Contains(note, `<project_instructions path="`+instrRoot+`/packages/api/AGENTS.md">`) || !strings.Contains(note, "api rules") || strings.Contains(note, "web rules") {
		t.Fatalf("the first read brings the api file only: %q", note)
	}
	if again := r.afterTool("read_file", "packages/api/src/x.ts"); again != "" {
		t.Fatalf("a second read adds nothing: %q", again)
	}
	if other := r.afterTool("list_files", "packages/web"); !strings.Contains(other, "web rules") || strings.Contains(other, "api rules") {
		t.Fatalf("the web folder brings its own: %q", other)
	}
	if outside := r.afterTool("terminal", "packages/api/src/x.ts"); outside != "" {
		t.Fatalf("only the file tools: %q", outside)
	}

	// While the notes are in the conversation, the system block leaves them out; once
	// they dropped out (compaction), it carries them.
	history := []kernel.Message{{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{Content: note}}}}
	if block := r.index(history...); strings.Contains(block, "api rules") || !strings.Contains(block, "web rules") {
		t.Fatalf("api's note is in the history, web's is not: %s", block)
	}

	// A changed sub-folder file is shown again after the recheck window.
	r.write("packages/api/AGENTS.md", "api rules v2")
	r.later()
	if note := r.afterTool("read_file", "packages/api/src/x.ts"); !strings.Contains(note, "api rules v2") || !strings.Contains(note, "changed") {
		t.Fatalf("changed: %q", note)
	}

	state := r.p.State(t.Context(), "s1")
	if len(state.Files) != 3 || state.Files[0].SubFolder || !state.Files[1].SubFolder || state.Files[1].Path != instrRoot+"/packages/api/AGENTS.md" {
		t.Fatalf("state = %+v", state)
	}
	if len(announced) == 0 || strings.Join(announced[len(announced)-1], ",") != instrRoot+"/AGENTS.md,"+instrRoot+"/packages/api/AGENTS.md,"+instrRoot+"/packages/web/AGENTS.md" {
		t.Fatalf("announced = %v", announced)
	}
}

func TestProjectInstructions_SearchFilesFolder(t *testing.T) {
	r := newInstructionsRig(t)
	r.write("lib/AGENTS.md", "lib rules")
	r.write("lib/deep/AGENTS.md", "deep rules")
	r.write("lib/deep/z.go", "z")
	note := r.afterTool("search_files", "lib")
	if !strings.Contains(note, "lib rules") || strings.Contains(note, "deep rules") {
		t.Fatalf("a folder search loads that folder's file, not deeper ones: %q", note)
	}
}

func TestDirsBetween(t *testing.T) {
	if got := strings.Join(dirsBetween("/w", "/w/a/b"), ","); got != "/w/a,/w/a/b" {
		t.Fatalf("got %q", got)
	}
	if got := dirsBetween("/w", "/w"); len(got) != 0 {
		t.Fatalf("the folder itself: %v", got)
	}
	if f, ok := containingFolder([]string{"/w/a", "/w/ab"}, "/w/ab/c"); !ok || f != "/w/ab" {
		t.Fatalf("containing = %q", f)
	}
}
