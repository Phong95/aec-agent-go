package preview

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The non-ASCII characters of the tool texts are built from code points, so this source
// stays ASCII-only.
var (
	emDash = string(rune(0x2014)) // U+2014 EM DASH
	midDot = string(rune(0x00b7)) // U+00B7 MIDDLE DOT
)

const (
	// appsDir is the folder of the agent's workspace that holds apps, one per sub-folder.
	appsDir = "apps"
	// appManifest is the manifest file of an app: apps/<app>/app.json.
	appManifest = "app.json"
	// indexFile is what a folder preview opens.
	indexFile = "index.html"
	// manifestMaxBytes bounds the read of an app.json.
	manifestMaxBytes = 64 * 1024
)

// hints are the advice appended to a SandboxError, by code (the workspace tools' set).
var hints = map[kernel.SandboxErrorCode]string{
	kernel.SandboxCodePathEscape:             "Only paths inside your workspace are allowed.",
	kernel.SandboxCodeDisabled:               "This agent has no sandbox. Ask the user to enable one in the agent's Sandbox settings.",
	kernel.SandboxCodeNotConfigured:          "No sandbox is configured on this server.",
	kernel.SandboxCodeEnvironmentUnavailable: "The sandbox is not available right now (e.g. Docker is not running).",
}

// withSandbox runs fn against the agent's sandbox and maps every failure to an output
// the model can act on, like the workspace tools: no sandbox, a SandboxError (with its
// code and retryable flag) or another error.
func withSandbox(ctx context.Context, tc toolhub.Context, name string, fn func(env sandboxhub.View) (toolhub.Output, error)) toolhub.Output {
	env, err := toolkit.SandboxFor(ctx, tc)
	if err == nil {
		var out toolhub.Output
		if out, err = fn(env); err == nil {
			return out
		}
	}
	if errors.Is(err, toolkit.ErrNoSandbox) {
		return toolkit.Fail(name + ": this agent has no workspace " + emDash + " no sandbox is available")
	}
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		content := name + " failed (" + string(se.Code) + "): " + se.Message
		if hint, ok := hints[se.Code]; ok {
			content += "\n" + hint
		}
		meta := ordered.NewObject()
		meta.Set("code", string(se.Code))
		return toolkit.Failure(content, se.Retryable, meta)
	}
	return toolhub.Error(name + " failed: " + err.Error())
}

// argError is a problem the model can fix: "<tool>: <message>", not retryable.
func argError(name, message string) toolhub.Output {
	return toolkit.Fail(name + ": " + message)
}

// workspaceRelative is the workspace path of logical relative to the working directory
// ("" = the working directory itself).
func workspaceRelative(env sandboxhub.View, logical string) string {
	wd := env.WorkingDir()
	if logical == wd {
		return ""
	}
	if rest, ok := strings.CutPrefix(logical, strings.TrimSuffix(wd, "/")+"/"); ok {
		return rest
	}
	return strings.TrimLeft(logical, "/")
}

// encodeURIComponent is JavaScript's encodeURIComponent: every byte but
// A-Z a-z 0-9 - _ . ! ~ * ' ( ) is percent-encoded (UTF-8, upper-case hex).
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// encodePath percent-encodes each segment of a relative path (keeps the slashes).
func encodePath(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = encodeURIComponent(p)
	}
	return strings.Join(parts, "/")
}

// imageExtensions are the files the chat shows inline in a reply as an image (A-133).
var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".avif": true, ".svg": true, ".bmp": true, ".ico": true,
}

// isImageFile reports whether a workspace path is an image the chat can show inside a
// reply (A-133), by extension.
func isImageFile(rel string) bool {
	return imageExtensions[strings.ToLower(path.Ext(rel))]
}

// embedImageNote is appended to the preview of an image instead of CiteLinkNote (A-133):
// it asks the model to show the image inside its reply as markdown
// (![name](preview://...)), which the chat renders as the image itself; a click still
// opens it in the Preview panel.
func embedImageNote(link kernel.ToolResultLink) string {
	return "\nIt is an image: to show it inside your reply, write ![" + link.Name + "](" + link.URI + ") on its own line."
}

// previewLink is the preview link of a workspace-relative path, with an optional query.
// A-133: an image file (no query) is type "image", so the chat can show a thumbnail of it
// on the tool call; anything else is "preview".
func previewLink(rel, name, query string) *kernel.ToolResultLink {
	uri := "preview://" + encodePath(rel)
	linkType := "preview"
	if query != "" {
		uri += "?" + query
	} else if isImageFile(rel) {
		linkType = "image"
	}
	return &kernel.ToolResultLink{URI: uri, Name: name, Type: linkType}
}

var appNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validAppName: one path segment, no dot-files, no "..".
func validAppName(app string) bool {
	return appNamePattern.MatchString(app) && !strings.Contains(app, "..")
}

// appManifestData is apps/<app>/app.json after the checks of readManifest.
type appManifestData struct {
	name        string
	description string
	entry       string
	opens       []string
	openURL     string
	// service (A-223) is the recipe of a service app - set when app.json has a "service"
	// block. Such an app is started with StartApp (the service tool's start with "app", or
	// the host's start-app route) instead of served as files.
	service *AppServiceRecipe
}

// AppServiceRecipe is the "service" block of app.json (A-223) - the recipe that runs a
// project as a service, so the user can start it again without asking the agent. The
// only contract is ServiceSpec's: the command listens on 127.0.0.1:$PORT (or on Port).
type AppServiceRecipe struct {
	// Name is the service's name (default: the app folder's name, lower-cased, other
	// characters as dashes).
	Name string `json:"name"`
	// Cwd is the workspace folder to run in, relative to the working directory (default:
	// the app folder).
	Cwd string `json:"cwd"`
	// Setup runs before Command when SetupInputs changed (or on every start without
	// SetupInputs), e.g. "npm ci". "" = none.
	Setup string `json:"setup,omitempty"`
	// SetupInputs are files (globs relative to Cwd) whose change makes Setup run again,
	// e.g. ["package-lock.json"].
	SetupInputs []string `json:"setupInputs,omitempty"`
	// Command starts the service, e.g. "npx vite --host 127.0.0.1 --port $PORT".
	Command string `json:"command"`
	// Path is the page the app opens, e.g. "docs" ("" = the root page).
	Path string `json:"path"`
	// Port, only when the server cannot take $PORT, is the fixed port it listens on.
	Port *int `json:"port,omitempty"`
	// Env holds environment variables; values may hold {{var:NAME}} and {{secret:NAME}}.
	Env map[string]string `json:"env"`
}

var (
	// serviceNamePattern is the sandbox's rule for service names.
	serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	envNamePattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	notServiceChars    = regexp.MustCompile(`[^a-z0-9-]+`)
	leadingDashes      = regexp.MustCompile(`^-+`)
	trailingDashes     = regexp.MustCompile(`-+$`)
	trailingSlashes    = regexp.MustCompile(`/+$`)
	leadingSlashes     = regexp.MustCompile(`^/+`)
	badPageChars       = regexp.MustCompile(`[\s#]`)
)

// DefaultServiceName is the service name an app gets by default (A-223): lower-cased,
// other characters as dashes, at most 40.
func DefaultServiceName(app string) string {
	name := leadingDashes.ReplaceAllString(notServiceChars.ReplaceAllString(strings.ToLower(app), "-"), "")
	if len(name) > 40 {
		name = name[:40]
	}
	return trailingDashes.ReplaceAllString(name, "")
}

// readRecipe checks the "service" block of app.json (A-223); a problem is what is wrong
// with it.
func readRecipe(raw any, app, p string) (*AppServiceRecipe, string) {
	s, ok := raw.(map[string]any)
	if !ok {
		return nil, p + `: "service" must be an object`
	}
	str := func(key string) string {
		v, _ := s[key].(string)
		return jsstr.Trim(v)
	}
	command := str("command")
	if command == "" {
		return nil, p + `: "service" needs a "command"`
	}
	name := str("name")
	if name == "" {
		name = DefaultServiceName(app)
	}
	if !serviceNamePattern.MatchString(name) {
		return nil, p + `: "service.name" must be lower-case letters, digits and dashes (at most 40)`
	}
	cwd := trailingSlashes.ReplaceAllString(leadingDotSlash.ReplaceAllString(str("cwd"), ""), "")
	if cwd == "" {
		cwd = appsDir + "/" + app
	}
	if containsDotDot(cwd) {
		return nil, p + `: "service.cwd" must stay inside the workspace`
	}
	page := leadingSlashes.ReplaceAllString(str("path"), "")
	if badPageChars.MatchString(page) || pageHasDotDot(page) {
		return nil, p + `: "service.path" is not a page of the service`
	}
	var port *int
	if v, present := s["port"]; present && v != nil && v != float64(0) {
		n, isNum := v.(float64)
		if !isNum || n != math.Trunc(n) || n < 1 || n > 65535 {
			return nil, p + `: "service.port" must be a port number`
		}
		pn := int(n)
		port = &pn
	}
	var inputs []string
	if v, present := s["setupInputs"]; present {
		list, isList := v.([]any)
		if !isList {
			return nil, p + `: "service.setupInputs" must be a list of file paths or globs`
		}
		for _, x := range list {
			if _, isStr := x.(string); !isStr {
				return nil, p + `: "service.setupInputs" must be a list of file paths or globs`
			}
		}
		for _, x := range list {
			if in := leadingDotSlash.ReplaceAllString(jsstr.Trim(x.(string)), ""); in != "" {
				inputs = append(inputs, in)
			}
		}
		for _, in := range inputs {
			if containsDotDot(in) {
				return nil, p + `: "service.setupInputs" must stay inside "cwd"`
			}
		}
	}
	env := map[string]string{}
	if v, present := s["env"]; present {
		obj, isObj := v.(map[string]any)
		if !isObj {
			return nil, p + `: "service.env" must be an object`
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !envNamePattern.MatchString(k) || k == "PORT" {
				return nil, p + `: "service.env" cannot set "` + k + `"`
			}
			value, isStr := obj[k].(string)
			if !isStr {
				return nil, p + `: "service.env.` + k + `" must be a string`
			}
			env[k] = value
		}
	}
	recipe := &AppServiceRecipe{Name: name, Cwd: cwd, Setup: str("setup"), Command: command, Path: page, Port: port, Env: env}
	if recipe.Setup != "" && len(inputs) > 0 {
		recipe.SetupInputs = inputs
	}
	return recipe, ""
}

// normalizeExtension: ".IFC" / "ifc" -> ".ifc".
func normalizeExtension(ext string) string {
	e := strings.ToLower(jsstr.Trim(ext))
	if strings.HasPrefix(e, ".") {
		return e
	}
	return "." + e
}

// extensionOf is the extension of a file name, lower-case with its dot ("" when none).
func extensionOf(name string) string {
	base := path.Base(name)
	dot := strings.LastIndex(base, ".")
	if dot > 0 {
		return strings.ToLower(base[dot:])
	}
	return ""
}

var leadingDotSlash = regexp.MustCompile(`^\.?/+`)

// readManifest reads and checks apps/<app>/app.json. A missing manifest is nil with an
// empty problem; a broken one is nil with the problem the tools report back.
func readManifest(ctx context.Context, env sandboxhub.View, app string) (*appManifestData, string, error) {
	p := appsDir + "/" + app + "/" + appManifest
	st, err := env.Stat(ctx, p)
	if err != nil {
		return nil, "", err
	}
	if st == nil {
		return nil, "", nil
	}
	if st.Type != sandboxhub.EntryFile {
		return nil, p + " is not a file", nil
	}
	limit := manifestMaxBytes
	file, err := env.ReadFile(ctx, p, sandboxhub.ReadOptions{MaxBytes: &limit})
	if err != nil {
		return nil, "", err
	}
	var raw any
	if err := json.Unmarshal([]byte(file.Content), &raw); err != nil {
		return nil, p + " is not valid JSON: " + err.Error(), nil //nolint:nilerr // a broken manifest is a problem the tool reports back, not a failure
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, p + " must be a JSON object", nil
	}
	str := func(key string) string {
		s, _ := m[key].(string)
		return jsstr.Trim(s)
	}
	entry := indexFile
	if e := str("entry"); e != "" {
		entry = leadingDotSlash.ReplaceAllString(e, "")
	}
	if containsDotDot(entry) {
		return nil, p + `: "entry" must stay inside the app folder`, nil
	}
	var opens []string
	if list, ok := m["opens"].([]any); ok {
		for _, x := range list {
			if s, ok := x.(string); ok && jsstr.Trim(s) != "" {
				opens = append(opens, normalizeExtension(s))
			}
		}
	}
	// {path} goes in the query ("viewer.html?model={path}"); anything else falls back to the default.
	given := leadingDotSlash.ReplaceAllString(str("openUrl"), "")
	openURL := entry + "?file={path}"
	if q := strings.Index(given, "?"); q > 0 && strings.Index(given, "{path}") > q {
		openURL = given
	}
	if containsDotDot(strings.SplitN(openURL, "?", 2)[0]) {
		return nil, p + `: "openUrl" must stay inside the app folder`, nil
	}
	// A-223: a service app.
	var service *AppServiceRecipe
	if raw, present := m["service"]; present && raw != nil {
		recipe, problem := readRecipe(raw, app, p)
		if problem != "" {
			return nil, problem, nil
		}
		service = recipe
	}
	name := str("name")
	if name == "" {
		name = app
	}
	return &appManifestData{name: name, description: str("description"), entry: entry, opens: opens, openURL: openURL, service: service}, "", nil
}

func containsDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// relativeURL is the URL of fileRel relative to the folder fromDir (both
// workspace-relative), percent-encoded per segment.
func relativeURL(fromDir, fileRel string) string {
	from := strings.Split(strings.Trim(fromDir, "/"), "/")
	to := strings.Split(strings.Trim(fileRel, "/"), "/")
	i := 0
	for i < len(from) && i < len(to)-1 && from[i] == to[i] {
		i++
	}
	parts := make([]string, 0, len(from)-i+len(to)-i)
	for range from[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, to[i:]...)
	return encodePath(strings.Join(parts, "/"))
}

// listAppNames is the folder names of the apps in apps/ that have a readable app.json, in
// listing order (A-210) - what preview names when it is asked for an app that does not
// exist.
func listAppNames(ctx context.Context, env sandboxhub.View) ([]string, error) {
	dir, err := env.Stat(ctx, appsDir)
	if err != nil {
		return nil, err
	}
	if dir == nil || dir.Type != sandboxhub.EntryDirectory {
		return nil, nil
	}
	listing, err := env.List(ctx, appsDir, sandboxhub.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range listing.Entries {
		if entry.Type != sandboxhub.EntryDirectory || !validAppName(entry.Name) {
			continue
		}
		if m, problem, err := readManifest(ctx, env, entry.Name); err == nil && problem == "" && m != nil {
			names = append(names, entry.Name)
		}
	}
	return names, nil
}
