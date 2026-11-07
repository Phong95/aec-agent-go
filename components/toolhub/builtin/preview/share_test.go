package preview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/kernel"
)

// share_preview against a fake sharer - the same cases as core/ts
// tests/unit/share-preview-tool.test.ts (the service case is covered there).
// Update (A-210): preview with public: true - the "preview with public" cases of core/ts
// tests/unit/preview-tools.test.ts (the service case is covered there).

var shareExpires = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC).UnixMilli()

type fakeSharer struct {
	requests []SharePreviewRequest
	err      error
}

func (f *fakeSharer) Share(_ context.Context, req SharePreviewRequest) (SharedPreview, error) {
	if f.err != nil {
		return SharedPreview{}, f.err
	}
	f.requests = append(f.requests, req)
	return SharedPreview{URL: "https://preview.example.com/p/TOKEN/x", ExpiresAt: shareExpires, Notes: []string{"It stops working when the server restarts."}}, nil
}

func shareRig(t *testing.T, f *fakeSharer) (rig, *PreviewTool) {
	t.Helper()
	r := setup(t)
	r.tc.AgentID = "a1"
	var sharer PreviewSharer
	if f != nil {
		sharer = f
	}
	return r, NewPreviewTool(Options{Sharer: sharer})
}

func share(t *testing.T, r rig, tool *PreviewTool, args string) kernelOutput {
	t.Helper()
	o, err := tool.Execute(context.Background(), kernel.Arguments(args), r.tc)
	if err != nil {
		t.Fatal(err)
	}
	return kernelOutput{content: o.Content, isError: o.IsError != nil && *o.IsError, retryable: o.Retryable}
}

func TestPublicSharesAFolder(t *testing.T) {
	f := &fakeSharer{}
	r, tool := shareRig(t, f)
	r.write(t, "sites/landing/index.html", "<h1>hi</h1>")
	o, err := tool.Execute(context.Background(), kernel.Arguments(`{"path":"sites/landing","public":true,"hours":2}`), r.tc)
	if err != nil || isErr(o) {
		t.Fatal(err, o.Content)
	}
	want := SharePreviewRequest{URI: "preview://sites/landing/index.html", AgentID: "a1", SessionID: "s1", Folder: "sites/landing", TTL: 2 * time.Hour}
	if f.requests[0] != want {
		t.Fatalf("request = %+v", f.requests[0])
	}
	if o.Link == nil || o.Link.URI != "preview://sites/landing/index.html" {
		t.Fatalf("link = %+v", o.Link)
	}
	for _, part := range []string{
		"Public URL of sites/landing/index.html:\nhttps://preview.example.com/p/TOKEN/x",
		"until 2026-11-08T12:00:00.000Z",
		"only the files under sites/landing/",
		"- It stops working when the server restarts.",
	} {
		if !strings.Contains(o.Content, part) {
			t.Fatalf("content = %q, want %q", o.Content, part)
		}
	}
	if v, _ := o.Metadata.Get("folder"); v != "sites/landing" {
		t.Fatalf("metadata folder = %v", v)
	}
}

func TestPublicCoversTheAppFile(t *testing.T) {
	f := &fakeSharer{}
	r, tool := shareRig(t, f)
	r.installViewer(t)
	r.write(t, "files/model.ifc", "ISO-10303-21;")
	if out := share(t, r, tool, `{"app":"ifc-viewer","file":"files/model.ifc","public":true}`); out.isError {
		t.Fatal(out.content)
	}
	if got := f.requests[0]; got.Folder != "" || got.TTL != 24*time.Hour {
		t.Fatalf("request = %+v", got)
	}
	r.write(t, "apps/ifc-viewer/samples/a.ifc", "x")
	share(t, r, tool, `{"app":"ifc-viewer","file":"apps/ifc-viewer/samples/a.ifc","public":true}`)
	if f.requests[1].Folder != "apps/ifc-viewer" {
		t.Fatalf("folder = %q", f.requests[1].Folder)
	}
}

func TestPublicTakesAWiderFolder(t *testing.T) {
	f := &fakeSharer{}
	r, tool := shareRig(t, f)
	r.write(t, "site/pages/index.html", "<p></p>")
	r.write(t, "other/x.txt", "x")
	share(t, r, tool, `{"path":"site/pages","public":true,"folder":"site"}`)
	if f.requests[0].Folder != "site" {
		t.Fatalf("folder = %q", f.requests[0].Folder)
	}
	if out := share(t, r, tool, `{"path":"site/pages","public":true,"folder":"other"}`); !out.isError || !strings.Contains(out.content, "does not hold site/pages/index.html") {
		t.Fatalf("out = %+v", out)
	}
}

func TestPublicRefusesBadArguments(t *testing.T) {
	f := &fakeSharer{}
	r, tool := shareRig(t, f)
	for args, want := range map[string]string{
		`{}`:                                     `"path" is required`,
		`{"path":"nope","public":true}`:          `"nope" does not exist`,
		`{"path":"x","public":true,"hours":500}`: `"hours" must be`,
		`{"service":"Not A Name"}`:               "is not a service name",
		`{"app":"v","service":"web"}`:            `give "app" or "service", not both`,
	} {
		if out := share(t, r, tool, args); !out.isError || !strings.Contains(out.content, want) {
			t.Fatalf("%s: %+v", args, out)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("shared %d times", len(f.requests))
	}
}

func TestPublicKeepsThePreviewWithoutAURL(t *testing.T) {
	r, tool := shareRig(t, &fakeSharer{err: errors.New("live previews are off")})
	r.write(t, "p/index.html", "<p></p>")
	o, err := tool.Execute(context.Background(), kernel.Arguments(`{"path":"p","public":true}`), r.tc)
	if err != nil || isErr(o) || o.Link == nil || !strings.Contains(o.Content, "No public URL: live previews are off.") {
		t.Fatalf("out = %+v, %v", o, err)
	}
	r2, plain := shareRig(t, nil)
	r2.write(t, "p/index.html", "<p></p>")
	if out := share(t, r2, plain, `{"path":"p","public":true}`); !strings.Contains(out.content, "No public URL: this server cannot make links that work outside the chat.") {
		t.Fatalf("out = %+v", out)
	}
}

func TestCommonFolder(t *testing.T) {
	for _, c := range [][3]string{
		{"apps/viewer/web", "files", ""},
		{"apps/viewer/web", "apps/viewer/samples", "apps/viewer"},
		{"", "x", ""},
	} {
		if got := CommonFolder(c[0], c[1]); got != c[2] {
			t.Fatalf("CommonFolder(%q, %q) = %q", c[0], c[1], got)
		}
	}
}

// Strict-schema models (OpenAI) fill every field: "" / 0 / [] mean not given.
func TestPreviewWithEveryFieldFilled(t *testing.T) {
	f := &fakeSharer{}
	r, tool := shareRig(t, f)
	r.write(t, "games/snake/index.html", "<canvas></canvas>")
	filled := `"path":"games/snake","app":"","file":"","service":"","folder":"","hours":0`
	if out := share(t, r, tool, `{`+filled+`,"public":false}`); out.isError {
		t.Fatalf("plain: %+v", out)
	}
	if out := share(t, r, tool, `{`+filled+`,"public":true}`); out.isError || len(f.requests) != 1 || f.requests[0].Folder != "games/snake" || f.requests[0].TTL != 24*time.Hour {
		t.Fatalf("public: %+v, requests %+v", out, f.requests)
	}
}
