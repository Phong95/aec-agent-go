package preview

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-124: the preview tools against the in-memory sandbox - the same cases as core/ts
// tests/unit/preview-tools.test.ts.

type rig struct {
	env   sandboxhub.View
	tc    toolhub.Context
	tools map[string]toolhub.Handler
}

func setup(t *testing.T) rig {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	if err := hub.Register(aecmock.NewInMemoryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	env, err := hub.Resolve(context.Background(), "a1", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]toolhub.Handler{}
	for _, h := range NewTools() {
		tools[h.Definition().Name] = h
	}
	return rig{
		env:   env,
		tc:    toolhub.Context{SessionID: "s1", GetSandbox: func(context.Context) (sandboxhub.View, error) { return env, nil }},
		tools: tools,
	}
}

func (r rig) write(t *testing.T, p, content string) {
	t.Helper()
	if _, err := r.env.WriteFile(context.Background(), p, content, sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (r rig) run(t *testing.T, name, args string) toolhub.Output {
	t.Helper()
	out, err := r.tools[name].Execute(context.Background(), kernel.Arguments(args), r.tc)
	if err != nil {
		t.Fatalf("%s returned an error: %v", name, err)
	}
	return out
}

func isErr(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

const viewerManifest = `{"name":"IFC viewer","description":"Views IFC building models","entry":"index.html","opens":[".IFC"],"openUrl":"index.html?model={path}"}`

func (r rig) installViewer(t *testing.T) {
	t.Helper()
	r.write(t, "apps/ifc-viewer/app.json", viewerManifest)
	r.write(t, "apps/ifc-viewer/index.html", "<!doctype html><title>viewer</title>")
}

func TestDefinitions(t *testing.T) {
	var names []string
	for _, h := range NewTools() {
		d := h.Definition()
		names = append(names, d.Name)
		wantRisk, wantOp := toolhub.RiskReadOnly, sandboxhub.OperationFSRead
		// A-124 phase 2: starting and stopping a service runs a command.
		if d.Name == "start_service" || d.Name == "stop_service" {
			wantRisk, wantOp = toolhub.RiskDraft, sandboxhub.OperationExec
		}
		if d.Risk != wantRisk || d.SandboxOperation != wantOp {
			t.Errorf("%s: risk %q, operation %q", d.Name, d.Risk, d.SandboxOperation)
		}
	}
	if strings.Join(names, ",") != strings.Join(ToolNames(), ",") {
		t.Fatalf("names = %v, want %v", names, ToolNames())
	}
}

func TestPreviewFile(t *testing.T) {
	r := setup(t)
	r.write(t, "sites/landing/index.html", "<h1>hi</h1>")
	out := r.run(t, "preview", `{"path":"sites/landing/index.html"}`)
	if isErr(out) || out.Link == nil {
		t.Fatalf("out = %+v", out)
	}
	want := kernel.ToolResultLink{URI: "preview://sites/landing/index.html", Name: "index.html", Type: "preview"}
	if *out.Link != want {
		t.Fatalf("link = %+v, want %+v", *out.Link, want)
	}
	if !strings.Contains(out.Content, "[index.html](preview://sites/landing/index.html)") {
		t.Fatalf("content does not cite the link: %s", out.Content)
	}
}

// A-133: the preview of an image asks the model to show it inside the reply as markdown.
func TestPreviewImageAsksToEmbedIt(t *testing.T) {
	r := setup(t)
	r.write(t, "out/thumb grid.PNG", "png")
	out := r.run(t, "preview", `{"path":"out/thumb grid.PNG"}`)
	// A-133: typed as an image, for the thumbnail on the tool call.
	if isErr(out) || out.Link == nil || out.Link.URI != "preview://out/thumb%20grid.PNG" || out.Link.Type != "image" {
		t.Fatalf("out = %+v", out)
	}
	if !strings.Contains(out.Content, "![thumb grid.PNG](preview://out/thumb%20grid.PNG) on its own line") {
		t.Fatalf("content does not ask to embed the image: %s", out.Content)
	}
	if strings.Contains(out.Content, "write its name as the link") {
		t.Fatalf("an image gets the embed note instead of the link note: %s", out.Content)
	}
}

func TestPreviewFolder(t *testing.T) {
	r := setup(t)
	r.write(t, "my game/index.html", "<canvas></canvas>")
	out := r.run(t, "preview", `{"path":"my game"}`)
	if out.Link == nil || out.Link.URI != "preview://my%20game/index.html" || out.Link.Name != "my game" {
		t.Fatalf("link = %+v", out.Link)
	}
}

func TestPreviewRefusals(t *testing.T) {
	r := setup(t)
	if o := r.run(t, "preview", `{"path":"nope"}`); !isErr(o) || o.Link != nil {
		t.Fatalf("missing: %+v", o)
	}
	r.write(t, "assets/a.css", "body{}")
	if o := r.run(t, "preview", `{"path":"assets"}`); !isErr(o) || !strings.Contains(o.Content, "index.html") {
		t.Fatalf("folder without index: %+v", o)
	}
	if o := r.run(t, "preview", `{"path":"../other/index.html"}`); !isErr(o) || o.Link != nil {
		t.Fatalf("escape: %+v", o)
	}
}

func TestListApps(t *testing.T) {
	r := setup(t)
	if o := r.run(t, "list_apps", `{}`); !strings.Contains(o.Content, "No apps installed") {
		t.Fatalf("empty: %s", o.Content)
	}
	r.installViewer(t)
	r.write(t, "apps/broken/app.json", "{ not json")
	r.write(t, "apps/notes/readme.md", "no manifest: not an app")
	o := r.run(t, "list_apps", `{}`)
	if !strings.Contains(o.Content, "ifc-viewer (IFC viewer) "+midDot+" opens .ifc "+midDot+" entry index.html") {
		t.Fatalf("content = %s", o.Content)
	}
	if !strings.Contains(o.Content, "broken "+midDot+" broken manifest") || strings.Contains(o.Content, "notes") {
		t.Fatalf("content = %s", o.Content)
	}
	if !strings.HasPrefix(o.Content, "1 app:") {
		t.Fatalf("count: %s", o.Content)
	}
}

func TestOpenInApp(t *testing.T) {
	r := setup(t)
	r.installViewer(t)
	r.write(t, "files/tower a.ifc", "ISO-10303-21;")
	o := r.run(t, "open_in_app", `{"app":"ifc-viewer","file":"files/tower a.ifc"}`)
	if isErr(o) || o.Link == nil {
		t.Fatalf("out = %+v", o)
	}
	want := kernel.ToolResultLink{
		URI:  "preview://apps/ifc-viewer/index.html?model=..%2F..%2Ffiles%2Ftower%2520a.ifc",
		Name: "tower a.ifc in IFC viewer", Type: "preview",
	}
	if *o.Link != want {
		t.Fatalf("link = %+v, want %+v", *o.Link, want)
	}
	q, _ := url.ParseQuery(strings.SplitN(o.Link.URI, "?", 2)[1])
	if q.Get("model") != "../../files/tower%20a.ifc" {
		t.Fatalf("model = %q", q.Get("model"))
	}

	start := r.run(t, "open_in_app", `{"app":"apps/ifc-viewer/"}`)
	if start.Link == nil || start.Link.URI != "preview://apps/ifc-viewer/index.html" || start.Link.Name != "IFC viewer" {
		t.Fatalf("start = %+v", start.Link)
	}
}

func TestOpenInAppRefusals(t *testing.T) {
	r := setup(t)
	r.installViewer(t)
	r.write(t, "files/plan.pdf", "%PDF")
	if o := r.run(t, "open_in_app", `{"app":"cad","file":"files/plan.pdf"}`); !isErr(o) || !strings.Contains(o.Content, "list_apps") {
		t.Fatalf("unknown app: %+v", o)
	}
	if o := r.run(t, "open_in_app", `{"app":"ifc-viewer","file":"files/nope.ifc"}`); !isErr(o) {
		t.Fatalf("missing file: %+v", o)
	}
	if o := r.run(t, "open_in_app", `{"app":"ifc-viewer","file":"files/plan.pdf"}`); !isErr(o) || !strings.Contains(o.Content, "opens .ifc files") {
		t.Fatalf("wrong type: %+v", o)
	}
	if o := r.run(t, "open_in_app", `{"app":"../x"}`); !isErr(o) {
		t.Fatalf("escape: %+v", o)
	}
}

func TestOpenInAppDefaultOpenURL(t *testing.T) {
	r := setup(t)
	r.write(t, "apps/v/app.json", `{"entry":"view.html","opens":["ifc"],"openUrl":"view.html#{path}"}`)
	r.write(t, "apps/v/view.html", "<p>v</p>")
	r.write(t, "m.ifc", "x")
	o := r.run(t, "open_in_app", `{"app":"v","file":"m.ifc"}`)
	if o.Link == nil || o.Link.URI != "preview://apps/v/view.html?file=..%2F..%2Fm.ifc" {
		t.Fatalf("link = %+v", o.Link)
	}
}
