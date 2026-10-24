package preview

import (
	"context"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ToolNames returns the names of the preview tools, for an agent's tool allowlist.
// Update (A-124 phase 2): and the service tools (dev servers).
func ToolNames() []string {
	return []string{
		"preview", "list_apps", "open_in_app",
		"start_service", "stop_service", "list_services", "service_logs",
	}
}

// NewTools returns the three preview tools in a fixed order.
// Update (A-124 phase 2): followed by the four service tools - seven in all.
func NewTools() []toolhub.Handler {
	return []toolhub.Handler{
		NewPreviewTool(), NewListAppsTool(), NewOpenInAppTool(),
		NewStartServiceTool(), NewStopServiceTool(), NewListServicesTool(), NewServiceLogsTool(),
	}
}

// ---------------------------------------------------------------------------
// preview
// ---------------------------------------------------------------------------

// PreviewTool is preview: show the user a web page from the workspace, live - an HTML
// file, or a folder with an index.html.
type PreviewTool struct{}

var _ toolhub.Handler = (*PreviewTool)(nil)

// NewPreviewTool returns the preview tool.
func NewPreviewTool() *PreviewTool { return &PreviewTool{} }

// Name returns the plugin name.
func (*PreviewTool) Name() string { return "tool:preview" }

// Version returns the plugin version.
func (*PreviewTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*PreviewTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "preview",
		Description: "Show the user a web page from your workspace in a live preview next to the chat: an HTML file, " +
			"or a folder that has an index.html (a website or browser game with its CSS, scripts, images and other " +
			"files " + emDash + " keep them in the same folder and use relative links). Use it after you build or change a page. " +
			"Images, PDFs and other files the browser can show work too. For an image, the result also tells you how " +
			"to show it inside your reply.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: `The HTML file or folder to preview, e.g. "sites/landing" or "game/index.html"`}},
			),
			Required: []string{"path"},
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
	}
}

// Execute links the page.
func (*PreviewTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	raw, _ := a.String("path")
	p := jsstr.Trim(raw)
	if p == "" {
		return argError("preview", `"path" is required `+emDash+` the HTML file or folder to show`), nil
	}
	return withSandbox(ctx, tc, "preview", func(env sandboxhub.View) (toolhub.Output, error) {
		st, err := env.Stat(ctx, p)
		if err != nil {
			return toolhub.Output{}, err
		}
		if st == nil {
			return argError("preview", `"`+p+`" does not exist `+emDash+` write the page first`), nil
		}
		target := st.Path
		switch st.Type {
		case sandboxhub.EntryDirectory:
			index, err := env.Stat(ctx, path.Join(st.Path, indexFile))
			if err != nil {
				return toolhub.Output{}, err
			}
			if index == nil || index.Type != sandboxhub.EntryFile {
				return argError("preview", `"`+p+`" is a folder without an `+indexFile+` `+emDash+` pass the HTML file to show`), nil
			}
			target = index.Path
		case sandboxhub.EntryFile:
		default:
			return argError("preview", `"`+p+`" is a `+string(st.Type)+`, not a file or folder`), nil
		}
		rel := workspaceRelative(env, target)
		shown := rel
		if st.Type == sandboxhub.EntryDirectory {
			shown = workspaceRelative(env, st.Path)
			if shown == "" {
				shown = "workspace"
			}
		}
		link := previewLink(rel, path.Base(shown), "")
		meta := ordered.NewObject()
		meta.Set("path", target)
		meta.Set("uri", link.URI)
		// A-133: an image can be shown inside the reply itself.
		note := toolhub.CiteLinkNote(*link)
		if isImageFile(rel) {
			note = embedImageNote(*link)
		}
		out := toolkit.Result("The preview of "+shown+" is ready "+emDash+" the user sees it live next to the chat."+note, meta)
		out.Link = link
		return out, nil
	}), nil
}

// ---------------------------------------------------------------------------
// list_apps
// ---------------------------------------------------------------------------

// ListAppsTool is list_apps: the apps installed in the agent's workspace.
type ListAppsTool struct{}

var _ toolhub.Handler = (*ListAppsTool)(nil)

// NewListAppsTool returns the list_apps tool.
func NewListAppsTool() *ListAppsTool { return &ListAppsTool{} }

// Name returns the plugin name.
func (*ListAppsTool) Name() string { return "tool:list-apps" }

// Version returns the plugin version.
func (*ListAppsTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*ListAppsTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "list_apps",
		Description: "List the apps installed in your workspace " + emDash + " each is a folder " + appsDir + "/<app>/ with an " + appManifest + " " +
			`({ "name", "description", "entry": "index.html", "opens": [".ifc"], "openUrl": "index.html?file={path}" }). ` +
			"Use it to find an app that opens a file the user gave you, then call open_in_app.",
		Parameters:       toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties(), Required: []string{}},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
	}
}

// Execute lists the apps.
func (*ListAppsTool) Execute(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	return withSandbox(ctx, tc, "list_apps", func(env sandboxhub.View) (toolhub.Output, error) {
		none := func(content string) toolhub.Output {
			meta := ordered.NewObject()
			meta.Set("count", 0)
			return toolkit.Result(content, meta)
		}
		dir, err := env.Stat(ctx, appsDir)
		if err != nil {
			return toolhub.Output{}, err
		}
		if dir == nil || dir.Type != sandboxhub.EntryDirectory {
			return none("No apps installed " + emDash + " there is no " + appsDir + "/ folder. To install one, put a static web app in " +
				appsDir + "/<app>/ with an " + appManifest + " that says which files it opens."), nil
		}
		listing, err := env.List(ctx, appsDir, sandboxhub.ListOptions{})
		if err != nil {
			return toolhub.Output{}, err
		}
		var lines []string
		var apps []any
		for _, entry := range listing.Entries {
			if entry.Type != sandboxhub.EntryDirectory || !validAppName(entry.Name) {
				continue
			}
			m, problem, err := readManifest(ctx, env, entry.Name)
			if err != nil {
				return toolhub.Output{}, err
			}
			if problem != "" {
				lines = append(lines, entry.Name+" "+midDot+" broken manifest: "+problem)
				continue
			}
			if m == nil {
				continue
			}
			obj := ordered.NewObject()
			obj.Set("app", entry.Name)
			obj.Set("name", m.name)
			if m.description != "" {
				obj.Set("description", m.description)
			}
			obj.Set("entry", m.entry)
			opens := make([]any, len(m.opens))
			for i, o := range m.opens {
				opens[i] = o
			}
			obj.Set("opens", opens)
			obj.Set("openUrl", m.openURL)
			apps = append(apps, obj)

			title := entry.Name
			if m.name != entry.Name {
				title += " (" + m.name + ")"
			}
			opensText := "opens no files"
			if len(m.opens) > 0 {
				opensText = "opens " + strings.Join(m.opens, ", ")
			}
			parts := []string{title, opensText, "entry " + m.entry}
			if m.description != "" {
				parts = append(parts, m.description)
			}
			lines = append(lines, strings.Join(parts, " "+midDot+" "))
		}
		if len(lines) == 0 {
			return none("No apps installed " + emDash + " no folder in " + appsDir + "/ has an " + appManifest + "."), nil
		}
		noun := "apps"
		if len(apps) == 1 {
			noun = "app"
		}
		meta := ordered.NewObject()
		meta.Set("count", len(apps))
		meta.Set("apps", apps)
		return toolkit.Result(strconv.Itoa(len(apps))+" "+noun+":\n"+strings.Join(lines, "\n"), meta), nil
	}), nil
}

// ---------------------------------------------------------------------------
// open_in_app
// ---------------------------------------------------------------------------

// OpenInAppTool is open_in_app: open a workspace file in an installed app, or start it.
type OpenInAppTool struct{}

var _ toolhub.Handler = (*OpenInAppTool)(nil)

// NewOpenInAppTool returns the open_in_app tool.
func NewOpenInAppTool() *OpenInAppTool { return &OpenInAppTool{} }

// Name returns the plugin name.
func (*OpenInAppTool) Name() string { return "tool:open-in-app" }

// Version returns the plugin version.
func (*OpenInAppTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*OpenInAppTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "open_in_app",
		Description: "Open a file from your workspace in one of your installed apps (see list_apps), shown live next to the chat " + emDash + " " +
			`e.g. an IFC model the user uploaded (in "files/") in an IFC viewer. Leave out "file" to just start the app.`,
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "app", Schema: toolhub.JSONSchema{Type: "string", Description: "The app's folder name in " + appsDir + "/"}},
				toolhub.Property{Name: "file", Schema: toolhub.JSONSchema{Type: "string", Description: `The workspace file to open, e.g. "files/tower-a.ifc"`}},
			),
			Required: []string{"app"},
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
		// A-137: it acts on the user's screen, which the parent decides.
		Subagent: toolhub.SubagentNever,
	}
}

var (
	appsPrefix     = regexp.MustCompile(`^apps/`)
	trailingSlash  = regexp.MustCompile(`/+$`)
	queryDelimiter = "?"
)

// Execute opens the file in the app (or starts the app).
func (*OpenInAppTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	rawApp, _ := a.String("app")
	app := trailingSlash.ReplaceAllString(appsPrefix.ReplaceAllString(jsstr.Trim(rawApp), ""), "")
	rawFile, _ := a.String("file")
	file := jsstr.Trim(rawFile)
	if app == "" {
		return argError("open_in_app", `"app" is required `+emDash+` see list_apps`), nil
	}
	if !validAppName(app) {
		return argError("open_in_app", `"`+app+`" is not an app folder name`), nil
	}
	return withSandbox(ctx, tc, "open_in_app", func(env sandboxhub.View) (toolhub.Output, error) {
		m, problem, err := readManifest(ctx, env, app)
		if err != nil {
			return toolhub.Output{}, err
		}
		if problem != "" {
			return argError("open_in_app", `The app "`+app+`" is broken: `+problem), nil
		}
		if m == nil {
			return argError("open_in_app", `No app "`+app+`" `+emDash+` there is no `+appsDir+"/"+app+"/"+appManifest+". Check list_apps."), nil
		}
		appDir := appsDir + "/" + app
		entry, err := env.Stat(ctx, path.Join(appDir, m.entry))
		if err != nil {
			return toolhub.Output{}, err
		}
		if entry == nil || entry.Type != sandboxhub.EntryFile {
			return argError("open_in_app", `The app "`+app+`" has no `+m.entry+" (its entry page)"), nil
		}

		if file == "" {
			link := previewLink(appDir+"/"+m.entry, m.name, "")
			meta := ordered.NewObject()
			meta.Set("app", app)
			meta.Set("uri", link.URI)
			out := toolkit.Result(m.name+" is open "+emDash+" the user sees it live next to the chat."+toolhub.CiteLinkNote(*link), meta)
			out.Link = link
			return out, nil
		}

		st, err := env.Stat(ctx, file)
		if err != nil {
			return toolhub.Output{}, err
		}
		if st == nil {
			return argError("open_in_app", `"`+file+`" does not exist`), nil
		}
		if st.Type != sandboxhub.EntryFile {
			return argError("open_in_app", `"`+file+`" is a `+string(st.Type)+", not a file"), nil
		}
		ext := extensionOf(st.Path)
		if len(m.opens) == 0 {
			return argError("open_in_app", m.name+` does not open files `+emDash+` leave out "file" to just start it`), nil
		}
		opensIt := false
		for _, o := range m.opens {
			if o == ext {
				opensIt = true
			}
		}
		if !opensIt {
			shownExt := ext
			if shownExt == "" {
				shownExt = "(no extension)"
			}
			return argError("open_in_app", m.name+" opens "+strings.Join(m.opens, ", ")+` files, not "`+shownExt+`"`), nil
		}
		fileRel := workspaceRelative(env, st.Path)
		page, query, _ := strings.Cut(m.openURL, queryDelimiter)
		filled := strings.ReplaceAll(query, "{path}", encodeURIComponent(relativeURL(appDir, fileRel)))
		base := path.Base(fileRel)
		link := previewLink(appDir+"/"+page, base+" in "+m.name, filled)
		meta := ordered.NewObject()
		meta.Set("app", app)
		meta.Set("file", st.Path)
		meta.Set("uri", link.URI)
		out := toolkit.Result(base+" is open in "+m.name+" "+emDash+" the user sees it live next to the chat."+toolhub.CiteLinkNote(*link), meta)
		out.Link = link
		return out, nil
	}), nil
}
