package preview

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/kernel"
)

// A-181: the screenshot tool against a fake renderer - the same cases as core/ts
// tests/unit/screenshot-tool.test.ts.

const dot = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// fakeRenderer records its requests and returns one 1x1 PNG per capture.
type fakeRenderer struct {
	requests []ScreenshotRequest
	sheets   int
	err      error
}

func (f *fakeRenderer) Capture(_ context.Context, req ScreenshotRequest) (ScreenshotResult, error) {
	if f.err != nil {
		return ScreenshotResult{}, f.err
	}
	f.requests = append(f.requests, req)
	var shots []Screenshot
	for _, v := range req.Viewports {
		label := strconv.Itoa(v.Width) + "x" + strconv.Itoa(v.Height)
		if len(req.At) == 0 {
			shots = append(shots, Screenshot{Label: label, MIMEType: "image/png", Data: dot, Width: v.Width, Height: v.Height})
		}
		for _, t := range req.At {
			shots = append(shots, Screenshot{Label: label + " at " + strconv.FormatFloat(t, 'f', -1, 64) + "s", MIMEType: "image/png", Data: dot, Width: v.Width, Height: v.Height})
		}
	}
	return ScreenshotResult{Shots: shots, PageErrors: []string{"TypeError: x is undefined"}}, nil
}

func (f *fakeRenderer) ContactSheet(_ context.Context, shots []Screenshot) (Screenshot, error) {
	f.sheets++
	return Screenshot{Label: "contact sheet of " + strconv.Itoa(len(shots)), MIMEType: "image/png", Data: dot, Width: 10, Height: 10}, nil
}

func screenshotRig(t *testing.T, f *fakeRenderer) (rig, *ScreenshotTool) {
	t.Helper()
	r := setup(t)
	r.tc.AgentID = "a1"
	return r, NewScreenshotTool(f)
}

func shoot(t *testing.T, r rig, tool *ScreenshotTool, args string) (out kernelOutput) {
	t.Helper()
	o, err := tool.Execute(context.Background(), kernel.Arguments(args), r.tc)
	if err != nil {
		t.Fatal(err)
	}
	return kernelOutput{content: o.Content, isError: o.IsError != nil && *o.IsError, retryable: o.Retryable, images: o.Images}
}

type kernelOutput struct {
	content   string
	isError   bool
	retryable *bool
	images    []kernel.MessageAttachment
}

func TestScreenshotDefinition(t *testing.T) {
	d := NewScreenshotTool(&fakeRenderer{}).Definition()
	if d.Name != "screenshot" || d.Risk != "read-only" || !d.SessionWorkspace || !d.NeedsVision {
		t.Fatalf("definition = %+v", d)
	}
}

func TestScreenshotCapturesEachViewport(t *testing.T) {
	f := &fakeRenderer{}
	r, tool := screenshotRig(t, f)
	r.write(t, "sites/landing/index.html", "<h1>hi</h1>")
	out := shoot(t, r, tool, `{"path":"sites/landing","viewports":["375x812","1440x900"],"dark":true}`)
	if out.isError {
		t.Fatal(out.content)
	}
	want := ScreenshotRequest{
		URI: "preview://sites/landing/index.html", AgentID: "a1", SessionID: "s1",
		Viewports: []Viewport{{375, 812}, {1440, 900}}, Dark: true,
	}
	if !reflect.DeepEqual(f.requests[0], want) {
		t.Fatalf("request = %+v", f.requests[0])
	}
	if len(out.images) != 2 || out.images[0].Name != "375x812.png" || out.images[1].Data != dot {
		t.Fatalf("images = %+v", out.images)
	}
	for _, part := range []string{
		"Screenshots of sites/landing/index.html: 375x812 (375×812 px), 1440x900 (1440×900 px).",
		"Page errors:\n- TypeError: x is undefined",
		"Failed requests: none",
	} {
		if !strings.Contains(out.content, part) {
			t.Fatalf("content = %q, want %q", out.content, part)
		}
	}
}

func TestScreenshotMakesAContactSheet(t *testing.T) {
	f := &fakeRenderer{}
	r, tool := screenshotRig(t, f)
	r.write(t, "v/index.html", "<div></div>")
	out := shoot(t, r, tool, `{"path":"v/index.html","at":[0.5,1,2,3,4],"seek":"seek(%t)"}`)
	if len(out.images) != 1 || f.sheets != 1 || !strings.Contains(out.content, "The 5 captures are combined into one labelled contact sheet") {
		t.Fatalf("out = %+v", out)
	}
}

func TestScreenshotRefusesBadArguments(t *testing.T) {
	f := &fakeRenderer{}
	r, tool := screenshotRig(t, f)
	r.write(t, "p.html", "<p></p>")
	for _, args := range []string{
		`{}`,
		`{"path":"missing.html"}`,
		`{"path":"p.html","viewports":["big"]}`,
		`{"path":"p.html","viewports":["100x100"]}`,
		`{"path":"p.html","at":[1]}`,
		`{"path":"p.html","viewports":["400x400","500x500"],"at":[1,2,3,4,5,6,7],"seek":"s(%t)"}`,
		`{"service":"Not A Name"}`,
	} {
		if out := shoot(t, r, tool, args); !out.isError {
			t.Fatalf("%s: not an error: %q", args, out.content)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("rendered %d times", len(f.requests))
	}
}

func TestScreenshotReportsARendererFailure(t *testing.T) {
	r, tool := screenshotRig(t, &fakeRenderer{err: errors.New("no browser on this server")})
	r.write(t, "p.html", "<p></p>")
	out := shoot(t, r, tool, `{"path":"p.html"}`)
	if !out.isError || out.content != "screenshot failed: no browser on this server" || out.retryable == nil || *out.retryable {
		t.Fatalf("out = %+v", out)
	}
}

func TestParseViewport(t *testing.T) {
	if v, ok := ParseViewport("375x812"); !ok || v != (Viewport{375, 812}) {
		t.Fatal(v, ok)
	}
	if v, ok := ParseViewport(" 1440 × 900 "); !ok || v != (Viewport{1440, 900}) {
		t.Fatal(v, ok)
	}
	for _, bad := range []string{"10x10", "wide"} {
		if _, ok := ParseViewport(bad); ok {
			t.Fatal(bad)
		}
	}
}
