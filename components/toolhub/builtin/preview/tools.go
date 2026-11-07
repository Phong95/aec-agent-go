package preview

import (
	"context"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ToolNames returns the names of the preview tools, for an agent's tool allowlist.
// Update (A-124 phase 2): and the service tools (dev servers).
// Update (A-210): two tools - preview (pages, apps, services, public URLs) and service
// (start / stop / list / logs). The live-preview skill carries the guidance.
func ToolNames() []string {
	return []string{"preview", "service"}
}

// RetiredToolNames returns the preview tools before A-210 and the tool each became, as a
// fresh map. Profiles and subagent types that still name them get the new tool instead
// (toolhub.RenamedToolNames, which writes them out; a test checks the two agree).
// share_preview never shipped in a release but was committed.
func RetiredToolNames() map[string]string {
	return map[string]string{
		"list_apps":     "preview",
		"open_in_app":   "preview",
		"share_preview": "preview",
		"start_service": "service",
		"stop_service":  "service",
		"list_services": "service",
		"service_logs":  "service",
	}
}

// Options configures the preview tools.
type Options struct {
	// Sharer signs public URLs (preview public: true, A-210). Without one, a public call
	// says this host cannot make them.
	Sharer PreviewSharer
}

// NewTools returns the three preview tools in a fixed order.
// Update (A-124 phase 2): followed by the four service tools - seven in all.
// Update (A-210): preview and service, in that order; opts.Sharer lets preview make
// public URLs.
func NewTools(opts Options) []toolhub.Handler {
	return []toolhub.Handler{NewPreviewTool(opts), NewServiceTool()}
}

// ---------------------------------------------------------------------------
// preview
//
// Update (A-210): one tool for everything the user can look at - a page or folder (path),
// a file opened in one of the workspace's apps (app + file, was open_in_app; list_apps is
// now the live-preview skill's job), or a running service (service + path, its page).
// public: true also asks the host for a temporary URL that works outside the chat (was
// share_preview): it reads only the page's folder, and every such call asks the user.
//
// Risk follows the call: read-only, or consequential with public (RiskFor). The
// definition says read-only - not the worst case - so plan mode and read-only subagents
// keep showing previews; both judge the call's own risk, so a public call is refused there.
// ---------------------------------------------------------------------------

const (
	// defaultShareHours is how long a public URL works when the model gives no "hours".
	defaultShareHours = 24
	// maxShareHours is the longest "hours" the model may ask for; the host may cap it further.
	maxShareHours = 168
)

var (
	appsPrefix    = regexp.MustCompile(`^apps/`)
	trailingSlash = regexp.MustCompile(`/+$`)
)

// PreviewTool is preview: show the user a web page from the workspace, live - an HTML
// file, or a folder with an index.html.
// Update (A-210): or an app with a file, or a service, and on request a public URL of it.
type PreviewTool struct {
	sharer PreviewSharer
}

var (
	_ toolhub.Handler    = (*PreviewTool)(nil)
	_ toolhub.CallRisker = (*PreviewTool)(nil)
)

// NewPreviewTool returns the preview tool.
func NewPreviewTool(opts Options) *PreviewTool { return &PreviewTool{sharer: opts.Sharer} }

// Name returns the plugin name.
func (*PreviewTool) Name() string { return "tool:preview" }

// Version returns the plugin version.
func (*PreviewTool) Version() string { return "2.0.0" }

// Definition returns the tool definition.
func (*PreviewTool) Definition() toolhub.Definition {
	str := func(desc string) toolhub.JSONSchema { return toolhub.JSONSchema{Type: "string", Description: desc} }
	return toolhub.Definition{
		Name: "preview",
		Description: "Show the user something from your workspace live next to the chat: a page, or a folder with an index.html " +
			"(path); a file opened in one of your apps (app, and file); or a running service (service, and path for one of " +
			"its pages). Images, PDFs and other files a browser shows work too. public: true also makes a temporary URL " +
			"anyone can open outside the chat " + emDash + " only when the user asks for a link; it asks the user first. The " +
			"live-preview skill covers apps, dev servers and public links.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "path", Schema: str(`The HTML file or folder to show, e.g. "sites/landing" or "game/index.html"; with service, its page (default "/")`)},
				toolhub.Property{Name: "app", Schema: str("Instead of path: an app's folder name in " + appsDir + "/")},
				toolhub.Property{Name: "file", Schema: str(`With app: the workspace file to open in it, e.g. "files/tower-a.ifc"`)},
				toolhub.Property{Name: "service", Schema: str("Instead of path: a running service, by its name")},
				toolhub.Property{Name: "public", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Also make a URL that works outside the chat, for the user to open or send"}},
				toolhub.Property{Name: "hours", Schema: toolhub.JSONSchema{
					Type:        "number",
					Description: "With public: how long the URL works, at most " + strconv.Itoa(maxShareHours) + " (default " + strconv.Itoa(defaultShareHours) + ")",
				}},
				toolhub.Property{Name: "folder", Schema: str("With public, only when the page loads files from elsewhere: the folder the URL may read (it must hold the page)")},
			),
			Required: []string{},
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
		// A-160: in the session's runner folder when the user attached one (the link carries the session).
		SessionWorkspace: true,
	}
}

// WantsPublic reports whether the call asks for a public URL (true, or the string "true"
// some models send).
func WantsPublic(args kernel.Arguments) bool {
	a := toolkit.DecodeArgs(args)
	if b, ok := a.Bool("public"); ok {
		return b
	}
	s, ok := a.String("public")
	return ok && strings.EqualFold(jsstr.Trim(s), "true")
}

// RiskFor is the risk of one call: a public URL makes workspace files reachable outside
// the chat, so that call is consequential (it asks first); anything else is read-only.
func (*PreviewTool) RiskFor(args kernel.Arguments) toolhub.Risk {
	if WantsPublic(args) {
		return toolhub.RiskConsequential
	}
	return toolhub.RiskReadOnly
}

// shown is what a call shows: its chat link and what the result says, and (workspace
// pages) the folders a public URL needs.
type shown struct {
	link    *kernel.ToolResultLink
	content string
	// label is the page as the result names it.
	label string
	meta  ordered.Object
	// isService is true for a service page (a public URL reaches the whole service).
	isService bool
	// folder is the folder a public URL reads by default (workspace pages).
	folder string
	// needs are the workspace paths a public URL must reach (the page, the file an app opens).
	needs []string
}

// previewArgs are the arguments of one preview call.
type previewArgs struct {
	path, app, file, service, folder string
	hasApp, hasService, public       bool
	hours                            float64
}

// readPreviewArgs reads and checks the arguments; problem is what is wrong with them.
func readPreviewArgs(args kernel.Arguments) (p previewArgs, problem string) {
	a := toolkit.DecodeArgs(args)
	p.path = trimmedString(a, "path")
	// Strict-schema models (OpenAI) fill every field: an empty string, 0 or [] means not given.
	if raw, ok := a.String("app"); ok {
		p.app = trailingSlash.ReplaceAllString(appsPrefix.ReplaceAllString(jsstr.Trim(raw), ""), "")
		p.hasApp = p.app != ""
	}
	p.file = trimmedString(a, "file")
	if raw, ok := a.String("service"); ok {
		p.service = jsstr.Trim(raw)
		p.hasService = p.service != ""
	}
	p.folder = trimmedString(a, "folder")
	p.public = WantsPublic(args)
	p.hours = defaultShareHours
	if h, ok := a.Number("hours"); ok && h != 0 {
		p.hours = h
	}
	switch {
	case p.hasApp && p.hasService:
		return p, `give "app" or "service", not both`
	case p.hasApp && p.path != "":
		return p, `give "path" or "app", not both ` + emDash + ` "file" is what the app opens`
	case p.hasService && !servicePattern.MatchString(p.service):
		return p, `"` + p.service + `" is not a service name`
	case p.hasApp && !validAppName(p.app):
		return p, `"` + p.app + `" is not an app folder name`
	case !p.hasApp && !p.hasService && p.path == "":
		return p, `"path" is required ` + emDash + ` the HTML file or folder to show (or give "app" or "service")`
	case p.public && !(p.hours > 0 && p.hours <= maxShareHours):
		return p, `"hours" must be more than 0 and at most ` + strconv.Itoa(maxShareHours)
	}
	return p, ""
}

// Execute links the page.
// Update (A-210): and makes its public URL when asked.
func (t *PreviewTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	const tool = "preview"
	p, problem := readPreviewArgs(args)
	if problem != "" {
		return argError(tool, problem), nil
	}
	return withSandbox(ctx, tc, tool, func(env sandboxhub.View) (toolhub.Output, error) {
		var s *shown
		var fail string
		var err error
		switch {
		case p.hasService:
			s, fail, err = showService(ctx, env, p.service, p.path)
		case p.hasApp:
			s, fail, err = showApp(ctx, env, p.app, p.file)
		default:
			s, fail, err = showPath(ctx, env, p.path)
		}
		if err != nil {
			return toolhub.Output{}, err
		}
		if fail != "" {
			return argError(tool, fail), nil
		}
		note := toolhub.CiteLinkNote(*s.link)
		if s.link.Type == "image" {
			note = embedImageNote(*s.link)
		}
		if !p.public {
			out := toolkit.Result(s.content+note, s.meta)
			out.Link = s.link
			return out, nil
		}

		// public: the folder the URL may read.
		folder := s.folder
		if !s.isService && p.folder != "" {
			given, err := env.Stat(ctx, p.folder)
			if err != nil {
				return toolhub.Output{}, err
			}
			if given == nil || given.Type != sandboxhub.EntryDirectory {
				return argError(tool, `"`+p.folder+`" is not a folder of your workspace`), nil
			}
			folderRel := workspaceRelative(env, given.Path)
			for _, need := range s.needs {
				if !folderInside(folderRel, need) {
					return argError(tool, `"`+p.folder+`" does not hold `+need+` `+emDash+` give a folder that holds the page and its files`), nil
				}
			}
			folder = folderRel
		}
		text := t.share(ctx, s, folder, p.hours, tc)
		out := toolkit.Result(s.content+text+note, s.meta)
		out.Link = s.link
		return out, nil
	}), nil
}

// share is the public URL part of the result - or why there is none (the chat preview
// still works). It also records public, expiresAt and folder in the result's metadata;
// the URL itself stays out of it, as it carries a working token.
func (t *PreviewTool) share(ctx context.Context, s *shown, folder string, hours float64, tc toolhub.Context) string {
	if t.sharer == nil {
		s.meta.Set("public", false)
		return "\nNo public URL: this server cannot make links that work outside the chat."
	}
	req := SharePreviewRequest{URI: s.link.URI, AgentID: tc.AgentID, SessionID: tc.SessionID, TTL: time.Duration(hours * float64(time.Hour))}
	if !s.isService {
		req.Folder = folder
	}
	shared, err := t.sharer.Share(ctx, req)
	if err != nil {
		s.meta.Set("public", false)
		return "\nNo public URL: " + err.Error() + "."
	}
	reach := "only the files under " + folder + "/"
	switch {
	case s.isService:
		reach = "the whole service"
	case folder == "":
		reach = "every file of your workspace"
	}
	var notes strings.Builder
	for _, n := range shared.Notes {
		notes.WriteString("\n- " + n)
	}
	text := "\nPublic URL of " + s.label + ":\n" + shared.URL + "\n" +
		"Anyone who has it can open it, without signing in, until " + time.UnixMilli(shared.ExpiresAt).UTC().Format("2006-01-02T15:04:05.000Z") + ". It reaches " + reach + "."
	if notes.Len() > 0 {
		text += "\nTell the user:" + notes.String()
	}
	text += "\nGive the user this exact URL written out in full (it is not a chat link)."
	s.meta.Set("public", true)
	s.meta.Set("expiresAt", shared.ExpiresAt)
	if !s.isService {
		s.meta.Set("folder", folder)
	}
	return text
}

// showPath is a page or folder of the workspace; fail is a problem the model can fix.
func showPath(ctx context.Context, env sandboxhub.View, p string) (*shown, string, error) {
	st, err := env.Stat(ctx, p)
	if err != nil {
		return nil, "", err
	}
	if st == nil {
		return nil, `"` + p + `" does not exist ` + emDash + ` write the page first`, nil
	}
	target := st.Path
	switch st.Type {
	case sandboxhub.EntryDirectory:
		index, err := env.Stat(ctx, path.Join(st.Path, indexFile))
		if err != nil {
			return nil, "", err
		}
		if index == nil || index.Type != sandboxhub.EntryFile {
			return nil, `"` + p + `" is a folder without an ` + indexFile + ` ` + emDash + ` pass the HTML file to show`, nil
		}
		target = index.Path
	case sandboxhub.EntryFile:
	default:
		return nil, `"` + p + `" is a ` + string(st.Type) + `, not a file or folder`, nil
	}
	rel := workspaceRelative(env, target)
	name := rel
	folder := parentFolder(rel)
	if st.Type == sandboxhub.EntryDirectory {
		folder = workspaceRelative(env, st.Path)
		name = folder
		if name == "" {
			name = "workspace"
		}
	}
	link := previewLink(rel, path.Base(name), "")
	meta := ordered.NewObject()
	meta.Set("path", target)
	meta.Set("uri", link.URI)
	return &shown{
		link: link, content: "The preview of " + name + " is ready " + emDash + " the user sees it live next to the chat.",
		label: rel, meta: meta, folder: folder, needs: []string{rel},
	}, "", nil
}

// showApp is an app of apps/<app>/, with a file opened in it or on its own (was
// open_in_app); fail is a problem the model can fix.
func showApp(ctx context.Context, env sandboxhub.View, app, file string) (*shown, string, error) {
	m, problem, err := readManifest(ctx, env, app)
	if err != nil {
		return nil, "", err
	}
	if problem != "" {
		return nil, `the app "` + app + `" is broken: ` + problem, nil
	}
	if m == nil {
		apps, err := listAppNames(ctx, env)
		if err != nil {
			return nil, "", err
		}
		hint := "You have no apps; the live-preview skill says how to make one."
		if len(apps) > 0 {
			hint = "Your apps: " + strings.Join(apps, ", ") + "."
		}
		return nil, `no app "` + app + `" ` + emDash + ` there is no ` + appsDir + "/" + app + "/" + appManifest + ". " + hint, nil
	}
	appDir := appsDir + "/" + app
	entry, err := env.Stat(ctx, path.Join(appDir, m.entry))
	if err != nil {
		return nil, "", err
	}
	if entry == nil || entry.Type != sandboxhub.EntryFile {
		return nil, `the app "` + app + `" has no ` + m.entry + " (its entry page)", nil
	}

	if file == "" {
		link := previewLink(appDir+"/"+m.entry, m.name, "")
		meta := ordered.NewObject()
		meta.Set("app", app)
		meta.Set("uri", link.URI)
		return &shown{
			link: link, content: m.name + " is open " + emDash + " the user sees it live next to the chat.",
			label: appDir + "/" + m.entry, meta: meta, folder: appDir, needs: []string{appDir + "/" + m.entry},
		}, "", nil
	}

	st, err := env.Stat(ctx, file)
	if err != nil {
		return nil, "", err
	}
	if st == nil {
		return nil, `"` + file + `" does not exist`, nil
	}
	if st.Type != sandboxhub.EntryFile {
		return nil, `"` + file + `" is a ` + string(st.Type) + ", not a file", nil
	}
	ext := extensionOf(st.Path)
	if len(m.opens) == 0 {
		return nil, m.name + ` does not open files ` + emDash + ` leave out "file" to just start it`, nil
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
		return nil, m.name + " opens " + strings.Join(m.opens, ", ") + ` files, not "` + shownExt + `"`, nil
	}
	fileRel := workspaceRelative(env, st.Path)
	page, query, _ := strings.Cut(m.openURL, "?")
	filled := strings.ReplaceAll(query, "{path}", encodeURIComponent(relativeURL(appDir, fileRel)))
	base := path.Base(fileRel)
	link := previewLink(appDir+"/"+page, base+" in "+m.name, filled)
	meta := ordered.NewObject()
	meta.Set("app", app)
	meta.Set("file", st.Path)
	meta.Set("uri", link.URI)
	return &shown{
		link: link, content: base + " is open in " + m.name + " " + emDash + " the user sees it live next to the chat.",
		label: base + " in " + m.name, meta: meta,
		// A public URL reads the app and the file it opens.
		folder: CommonFolder(appDir, parentFolder(fileRel)),
		needs:  []string{appDir + "/" + page, fileRel},
	}, "", nil
}

// showService is a page of a running service; fail is a problem the model can fix.
func showService(ctx context.Context, env sandboxhub.View, name, p string) (*shown, string, error) {
	svc, ok := sandboxhub.AsServicesView(env)
	if !ok {
		return nil, "this agent's sandbox cannot run services " + emDash + " show a file with path", nil
	}
	list, err := svc.ListServices(ctx)
	if err != nil {
		return nil, "", err
	}
	var found *sandboxhub.ServiceInfo
	for i := range list {
		if list[i].Name == name {
			found = &list[i]
		}
	}
	if found == nil {
		return nil, `there is no service named "` + name + `" ` + emDash + ` start it with service (action start) first`, nil
	}
	if found.Status == sandboxhub.ServiceExited {
		return nil, `the service "` + name + `" has stopped ` + emDash + ` start it again first`, nil
	}
	page := strings.TrimLeft(p, "/")
	if strings.ContainsAny(page, " \t\n\r#") || pageHasDotDot(page) {
		return nil, `"` + p + `" is not a page of the service`, nil
	}
	label := name
	if page != "" {
		label += " (/" + page + ")"
	}
	link := &kernel.ToolResultLink{URI: "service://" + name + "/" + page, Name: name, Type: "service"}
	meta := ordered.NewObject()
	meta.Set("service", name)
	meta.Set("port", found.Port)
	meta.Set("uri", link.URI)
	return &shown{link: link, content: label + " is live next to the chat.", label: "the service " + label, meta: meta, isService: true}, "", nil
}
