package preview

import (
	"context"
	"encoding/json"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-181: screenshot - render a page of the workspace (or a running service) in the host's
// browser and hand the images to the model, so it can look at what it built and fix it
// (the visual-self-check skill, A-182). Offered only to a model that can see images
// (Definition.NeedsVision, A-180). The host supplies the renderer:
// NewScreenshotTool(renderer). The same texts and limits as core/ts.
// ---------------------------------------------------------------------------

// Viewport is a viewport size, in CSS pixels.
type Viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// ScreenshotRequest is what to capture, and how.
type ScreenshotRequest struct {
	// URI is the page: a "preview://<workspace path>" or "service://<name>/<path>" link (A-124).
	URI     string
	AgentID string
	// SessionID is the session whose workspace the page is in (A-160).
	SessionID string
	// Viewports gets one capture set each, in order.
	Viewports []Viewport
	// Selector captures this element only (a CSS selector) instead of the viewport.
	Selector string
	// FullPage captures the whole page height, not only the viewport (ignored with a selector).
	FullPage bool
	// Dark renders with prefers-color-scheme: dark.
	Dark bool
	// At are seconds of an animation to capture, each after running Seek with %t replaced
	// by the time. Without them, one capture per viewport.
	At   []float64
	Seek string
	// WaitMs is an extra wait after the page loaded (fonts and images are always waited for).
	WaitMs int
}

// Screenshot is one captured image.
type Screenshot struct {
	// Label says what it shows, e.g. "375x812" or "1440x900 at 2.5s".
	Label string
	// MIMEType is "image/png" or "image/jpeg".
	MIMEType string
	// Data is base64, no "data:" prefix.
	Data string
	// Width and Height are the pixel size of the image.
	Width, Height int
}

// ScreenshotResult is the captures of one request, and what the page reported while it ran.
type ScreenshotResult struct {
	Shots []Screenshot
	// PageErrors are console errors and uncaught exceptions of the page, first ones only.
	PageErrors []string
	// FailedRequests are requests of the page that failed (a missing image, a 404 script).
	FailedRequests []string
}

// ScreenshotRenderer is the host's browser. Capture returns an error whose message the
// model reads when the page cannot be rendered (no browser, the link cannot be resolved,
// a timeout).
type ScreenshotRenderer interface {
	Capture(ctx context.Context, req ScreenshotRequest) (ScreenshotResult, error)
}

// ContactSheeter is the optional side of a ScreenshotRenderer that makes one labelled grid
// image of several shots, so a call with many captures costs one image. Without it, the
// first shots are returned as they are.
type ContactSheeter interface {
	ContactSheet(ctx context.Context, shots []Screenshot) (Screenshot, error)
}

const (
	// maxViewports, maxTimes and maxCaptures bound one call.
	maxViewports = 4
	maxTimes     = 8
	maxCaptures  = 12
	// minSide and maxSide bound a viewport side, in CSS pixels.
	minSide = 200
	maxSide = 3840
	// maxWaitMs is the longest wait after load the model may ask for.
	maxWaitMs = 10_000
	// maxListed is how many page errors and failed requests the result lists.
	maxListed = 10
)

var (
	defaultViewport = Viewport{Width: 1280, Height: 800}
	viewportPattern = regexp.MustCompile(`(?i)^\s*(\d{2,5})\s*[x\x{00d7}]\s*(\d{2,5})\s*$`)
	servicePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	times           = "×"
)

// ParseViewport parses "375x812"; ok is false when it is not a size in bounds.
func ParseViewport(text string) (Viewport, bool) {
	m := viewportPattern.FindStringSubmatch(text)
	if m == nil {
		return Viewport{}, false
	}
	w, _ := strconv.Atoi(m[1])
	h, _ := strconv.Atoi(m[2])
	if w < minSide || h < minSide || w > maxSide || h > maxSide {
		return Viewport{}, false
	}
	return Viewport{Width: w, Height: h}, true
}

// ScreenshotTool is screenshot: capture a page the agent built and look at it.
type ScreenshotTool struct {
	renderer ScreenshotRenderer
}

var _ toolhub.Handler = (*ScreenshotTool)(nil)

// NewScreenshotTool returns the screenshot tool over the host's renderer.
func NewScreenshotTool(renderer ScreenshotRenderer) *ScreenshotTool {
	return &ScreenshotTool{renderer: renderer}
}

// Name returns the plugin name.
func (*ScreenshotTool) Name() string { return "tool:screenshot" }

// Version returns the plugin version.
func (*ScreenshotTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*ScreenshotTool) Definition() toolhub.Definition {
	str := func(desc string) toolhub.JSONSchema { return toolhub.JSONSchema{Type: "string", Description: desc} }
	return toolhub.Definition{
		Name: "screenshot",
		Description: "Take screenshots of a web page you built and look at them: an HTML file or folder of your workspace (as for " +
			"preview), or a page of a running service. Use it after building or changing anything people see, to check " +
			"layout, text, colours and assets with your own eyes before you say it works. Capture each size that matters " +
			`(e.g. viewports ["375x812", "1440x900"]), the dark theme, one element, the whole page, or exact moments of an ` +
			"animation. The result also lists the page's console errors and failed requests.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "path", Schema: str(`The HTML file or folder to capture, e.g. "sites/landing"; with service, the page of the service (default "/")`)},
				toolhub.Property{Name: "service", Schema: str("Capture a running service (start_service) by its name instead of a file")},
				toolhub.Property{Name: "viewports", Schema: toolhub.JSONSchema{
					Type: "array", Items: &toolhub.JSONSchema{Type: "string"},
					Description: `Viewport sizes as "WIDTHxHEIGHT" in CSS pixels, at most ` + strconv.Itoa(maxViewports) + ` (default "1280x800")`,
				}},
				toolhub.Property{Name: "selector", Schema: str("Capture only the element matching this CSS selector")},
				toolhub.Property{Name: "full_page", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Capture the whole page height, not only the viewport"}},
				toolhub.Property{Name: "dark", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Render with the dark colour scheme (prefers-color-scheme: dark)"}},
				toolhub.Property{Name: "at", Schema: toolhub.JSONSchema{
					Type: "array", Items: &toolhub.JSONSchema{Type: "number"},
					Description: "Seconds of an animation to capture, at most " + strconv.Itoa(maxTimes) + "; needs seek",
				}},
				toolhub.Property{Name: "seek", Schema: str(`JavaScript run before each "at" capture, %t is the time, e.g. "window.__timelines.main.seek(%t)"`)},
				toolhub.Property{Name: "wait_ms", Schema: toolhub.JSONSchema{
					Type:        "number",
					Description: "Extra wait after the page loaded, at most " + strconv.Itoa(maxWaitMs) + " ms (fonts and images are always waited for)",
				}},
			),
			Required: []string{},
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
		// A-160: the page is in the session's workspace (a runner folder when one is attached).
		SessionWorkspace: true,
		// A-180: the result is images, so only a model that can see them gets the tool.
		NeedsVision: true,
	}
}

// Execute captures the page.
func (t *ScreenshotTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	const tool = "screenshot"
	a := toolkit.DecodeArgs(args)
	rawPath, _ := a.String("path")
	p := jsstr.Trim(rawPath)
	service, hasService := a.String("service")
	service = jsstr.Trim(service)
	if hasService && !servicePattern.MatchString(service) {
		return argError(tool, `"`+service+`" is not a service name`), nil
	}
	if !hasService && p == "" {
		return argError(tool, `"path" is required `+emDash+` the HTML file or folder to capture (or give "service")`), nil
	}

	viewports, problem := viewportsArg(a)
	if problem != "" {
		return argError(tool, problem), nil
	}
	at, seek, problem := timesArg(a)
	if problem != "" {
		return argError(tool, problem), nil
	}
	n := len(at)
	if n == 0 {
		n = 1
	}
	if len(viewports)*n > maxCaptures {
		return argError(tool, "at most "+strconv.Itoa(maxCaptures)+" captures per call (viewports "+times+" times): split it into several calls"), nil
	}
	waitMs := 0
	if w, ok := a.Number("wait_ms"); ok {
		if w < 0 || w > maxWaitMs {
			return argError(tool, `"wait_ms" must be 0 to `+strconv.Itoa(maxWaitMs)), nil
		}
		waitMs = int(w)
	}
	selector, _ := a.String("selector")
	fullPage, _ := a.Bool("full_page")
	dark, _ := a.Bool("dark")

	return withSandbox(ctx, tc, tool, func(env sandboxhub.View) (toolhub.Output, error) {
		var uri, shown string
		if hasService {
			svc, ok := sandboxhub.AsServicesView(env)
			if !ok {
				return argError(tool, "this agent's sandbox cannot run services "+emDash+" capture a file with path"), nil
			}
			list, err := svc.ListServices(ctx)
			if err != nil {
				return toolhub.Output{}, err
			}
			var found *sandboxhub.ServiceInfo
			for i := range list {
				if list[i].Name == service {
					found = &list[i]
				}
			}
			if found == nil {
				return argError(tool, `there is no service named "`+service+`" `+emDash+` start it with start_service first`), nil
			}
			if found.Status == sandboxhub.ServiceExited {
				return argError(tool, `the service "`+service+`" has stopped `+emDash+` start it again first`), nil
			}
			page := strings.TrimLeft(p, "/")
			if strings.ContainsAny(page, " \t\n\r#") || pageHasDotDot(page) {
				return argError(tool, `"`+p+`" is not a page of the service`), nil
			}
			uri = "service://" + service + "/" + page
			shown = "the service " + service
			if page != "" {
				shown += " (/" + page + ")"
			}
		} else {
			st, err := env.Stat(ctx, p)
			if err != nil {
				return toolhub.Output{}, err
			}
			if st == nil {
				return argError(tool, `"`+p+`" does not exist `+emDash+` write the page first`), nil
			}
			target := st.Path
			switch st.Type {
			case sandboxhub.EntryDirectory:
				index, err := env.Stat(ctx, path.Join(st.Path, indexFile))
				if err != nil {
					return toolhub.Output{}, err
				}
				if index == nil || index.Type != sandboxhub.EntryFile {
					return argError(tool, `"`+p+`" is a folder without an `+indexFile+` `+emDash+` pass the HTML file to capture`), nil
				}
				target = index.Path
			case sandboxhub.EntryFile:
			default:
				return argError(tool, `"`+p+`" is a `+string(st.Type)+`, not a file or folder`), nil
			}
			rel := workspaceRelative(env, target)
			uri = previewLink(rel, path.Base(rel), "").URI
			shown = rel
		}

		result, err := t.renderer.Capture(ctx, ScreenshotRequest{
			URI: uri, AgentID: tc.AgentID, SessionID: tc.SessionID, Viewports: viewports,
			Selector: jsstr.Trim(selector), FullPage: fullPage, Dark: dark, At: at, Seek: seek, WaitMs: waitMs,
		})
		if err != nil {
			return toolkit.Fail("screenshot failed: " + err.Error()), nil //nolint:nilerr // a render failure is the model's to read, as core/ts
		}
		if len(result.Shots) == 0 {
			return toolkit.Fail("screenshot failed: nothing was captured of " + shown), nil
		}
		return t.output(ctx, shown, result)
	}), nil
}

// output is the result the model reads: the captures as images (a contact sheet when there
// are more than one result may carry), the page's errors and failed requests.
func (t *ScreenshotTool) output(ctx context.Context, shown string, result ScreenshotResult) (toolhub.Output, error) {
	images := result.Shots
	sheetNote := ""
	if len(result.Shots) > kernel.MaxImagesPerMessage {
		if sheeter, ok := t.renderer.(ContactSheeter); ok {
			sheet, err := sheeter.ContactSheet(ctx, result.Shots)
			if err != nil {
				return toolkit.Fail("screenshot failed: " + err.Error()), nil //nolint:nilerr // a render failure is the model's to read, as core/ts
			}
			images = []Screenshot{sheet}
			sheetNote = "\nThe " + strconv.Itoa(len(result.Shots)) + " captures are combined into one labelled contact sheet; call again with fewer viewports or times to see one at full size."
		} else {
			images = result.Shots[:kernel.MaxImagesPerMessage]
			sheetNote = "\nOnly the first " + strconv.Itoa(kernel.MaxImagesPerMessage) + " of " + strconv.Itoa(len(result.Shots)) + " captures are shown; call again for the rest."
		}
	}
	labels := make([]string, 0, len(result.Shots))
	for _, s := range result.Shots {
		labels = append(labels, s.Label+" ("+strconv.Itoa(s.Width)+times+strconv.Itoa(s.Height)+" px)")
	}
	content := "Screenshots of " + shown + ": " + strings.Join(labels, ", ") + "." + sheetNote + "\n" +
		"Page errors:" + listed(result.PageErrors) + "\n" +
		"Failed requests:" + listed(result.FailedRequests) + "\n" +
		"Look at every image against what the page should show before you call it done."
	out := toolhub.Output{Content: content}
	for _, s := range images {
		ext := "jpg"
		if s.MIMEType == "image/png" {
			ext = "png"
		}
		out.Images = append(out.Images, kernel.MessageAttachment{Type: kernel.AttachmentTypeImage, MIMEType: s.MIMEType, Data: s.Data, Name: s.Label + "." + ext})
	}
	return out, nil
}

// listed is " none", or the items one per line, the first maxListed of them.
func listed(items []string) string {
	if len(items) == 0 {
		return " none"
	}
	var b strings.Builder
	for i, item := range items {
		if i == maxListed {
			b.WriteString("\n- " + ellipsis + "and " + strconv.Itoa(len(items)-maxListed) + " more")
			break
		}
		b.WriteString("\n- " + item)
	}
	return b.String()
}

// viewportsArg reads "viewports" (default 1280x800); problem is what is wrong with it.
func viewportsArg(a toolkit.Args) (viewports []Viewport, problem string) {
	raw, ok := a.Raw("viewports")
	if !ok {
		return []Viewport{defaultViewport}, ""
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 || len(items) > maxViewports {
		return nil, `"viewports" must be a list of 1 to ` + strconv.Itoa(maxViewports) + ` sizes like "375x812"`
	}
	for _, item := range items {
		var text string
		if json.Unmarshal(item, &text) != nil {
			text = string(item)
		}
		v, ok := ParseViewport(text)
		if !ok {
			return nil, `"` + text + `" is not a viewport size: write WIDTHxHEIGHT, each side ` + strconv.Itoa(minSide) + " to " + strconv.Itoa(maxSide)
		}
		viewports = append(viewports, v)
	}
	return viewports, ""
}

// timesArg reads "at" and "seek"; problem is what is wrong with them.
func timesArg(a toolkit.Args) (at []float64, seek, problem string) {
	seek, _ = a.String("seek")
	seek = jsstr.Trim(seek)
	raw, ok := a.Raw("at")
	if !ok {
		return nil, seek, ""
	}
	bad := `"at" must be a list of 1 to ` + strconv.Itoa(maxTimes) + " times in seconds"
	var items []any
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 || len(items) > maxTimes {
		return nil, "", bad
	}
	for _, item := range items {
		f, isNumber := item.(float64)
		if !isNumber || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, "", bad
		}
		at = append(at, f)
	}
	if seek == "" {
		return nil, "", `"at" needs "seek": the JavaScript that moves the animation to %t seconds`
	}
	return at, seek, ""
}

// pageHasDotDot reports whether a page path (with an optional query) has a ".." segment.
func pageHasDotDot(page string) bool {
	for _, part := range strings.FieldsFunc(page, func(r rune) bool { return r == '/' || r == '?' }) {
		if part == ".." {
			return true
		}
	}
	return false
}
