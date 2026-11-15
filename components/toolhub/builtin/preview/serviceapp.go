package preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-223: service apps. An app whose app.json has a "service" block is a saved recipe that
// runs a project of the workspace as a service - a Vite app, a Go API, a Python server -
// so the user can start it again without asking the agent. StartApp is the one code path:
// the service tool (action start with "app") and the hosts' start-app route both call it.
//
// Setup (npm ci, go build, pip install ...) runs before the command only when the files of
// SetupInputs changed since its last success: their sizes and times are hashed with the
// recipe into apps/<app>/.setup-state.json. Without SetupInputs it runs on every start.
// Same as core/ts (components/tool-hub/built-in-tools/preview/service-app.ts).
// ---------------------------------------------------------------------------

// SetupStateFile is where StartApp remembers the inputs of the last setup that succeeded.
const SetupStateFile = ".setup-state.json"

const (
	// appStartWait is how long StartApp waits for the port by default.
	appStartWait = 30 * time.Second
	// appSetupTimeout is how long setup may run by default (the sandbox's exec limit may
	// cap it).
	appSetupTimeout = 10 * time.Minute
	appSetupOutput  = 256 * 1024
	// outputTailChars is how much of a failed step's output an AppStartError keeps.
	outputTailChars = 4000
	// maxInputFiles: past this many matched SetupInputs files nothing is hashed; setup runs.
	maxInputFiles = 2000
)

// AppStartStep is the step of StartApp that failed.
type AppStartStep string

// The steps of StartApp.
const (
	AppStepManifest AppStartStep = "manifest"
	AppStepSetup    AppStartStep = "setup"
	AppStepCommand  AppStartStep = "command"
)

// AppStartCode is why StartApp failed, for hosts to answer with.
type AppStartCode string

// The codes of an AppStartError.
const (
	// AppCodeNotFound: there is no such app.
	AppCodeNotFound AppStartCode = "not_found"
	// AppCodeUnsupported: the sandbox runs no services.
	AppCodeUnsupported AppStartCode = "capability_unsupported"
	// AppCodeInvalid: a broken or non-service app, or an env that cannot be filled.
	AppCodeInvalid AppStartCode = "invalid_app"
	// AppCodeSetupFailed: the setup failed.
	AppCodeSetupFailed AppStartCode = "setup_failed"
	// AppCodeExited: the command ended before it listened.
	AppCodeExited AppStartCode = "exited"
)

// AppStartError is why an app did not start (A-223) - something the person (or the model)
// can fix, with the step and the end of its output. Sandbox failures are returned as a
// *kernel.SandboxError instead.
type AppStartError struct {
	kernel.SDKError
	Step AppStartStep
	// Output is the end of the failed step's output ("" when none).
	Output string
	// Code is invalid_app, setup_failed or exited by step, unless set otherwise.
	Code AppStartCode
}

func appStartError(message string, step AppStartStep, output string) *AppStartError {
	code := AppCodeInvalid
	switch step {
	case AppStepSetup:
		code = AppCodeSetupFailed
	case AppStepCommand:
		code = AppCodeExited
	case AppStepManifest:
	}
	return &AppStartError{SDKError: kernel.SDKError{Message: message}, Step: step, Output: output, Code: code}
}

// StartAppOptions are the optional inputs of StartApp.
type StartAppOptions struct {
	// Secrets fills {{secret:NAME}} in the recipe's env (each secret must allow the tool
	// "service"). nil = none.
	Secrets secret.Secrets
	// Variables fills {{var:NAME}} in the recipe's env. nil = none.
	Variables variable.Variables
	// SessionID is the session the secrets are used for.
	SessionID string
	// Wait is how long to wait for the port (0 = 30 s). Past it the app is returned as
	// "starting".
	Wait time.Duration
	// SetupTimeout is how long setup may run (0 = 10 minutes).
	SetupTimeout time.Duration
}

// Setup outcomes of StartApp.
const (
	SetupRan     = "ran"
	SetupSkipped = "skipped"
	SetupNone    = "none"
)

// StartedApp is a service app StartApp started (or found running).
type StartedApp struct {
	App string
	// Name is the app's display name.
	Name   string
	Recipe AppServiceRecipe
	// Service is the service as it is now: listening, or starting when the wait ran out.
	Service sandboxhub.ServiceInfo
	// Link is service://<name>/<path> - the page the app opens.
	Link kernel.ToolResultLink
	// Setup is SetupRan, SetupSkipped (inputs unchanged) or SetupNone (no setup).
	Setup string
	// AlreadyRunning is true when the service was already running (nothing was run).
	AlreadyRunning bool
}

// AppListing is one app of apps/ as a host lists it (A-223).
type AppListing struct {
	// App is the folder name in apps/.
	App         string `json:"app"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Kind is "static" (files served as they are) or "service" (a recipe that runs as a
	// service).
	Kind string `json:"kind"`
	// Service is the service's name (service apps).
	Service string `json:"service,omitempty"`
	// Status is the service's state, "stopped" when it is not running (service apps).
	Status string `json:"status,omitempty"`
	// Page is a static app's entry page, or the page a service app opens.
	Page string `json:"page"`
	// Problem is what is wrong with a broken app.json (then only App and Name are set).
	Problem string `json:"problem,omitempty"`
}

// AppServiceLink is the link of a service app: its service and page.
func AppServiceLink(recipe AppServiceRecipe, name string) kernel.ToolResultLink {
	return kernel.ToolResultLink{URI: "service://" + recipe.Name + "/" + recipe.Path, Name: name, Type: "service"}
}

// tailOf is the end of text, trimmed at the end and cut to outputTailChars.
func tailOf(text string) string {
	t := strings.TrimRight(text, " \t\r\n")
	if r := []rune(t); len(r) > outputTailChars {
		return ellipsis + string(r[len(r)-outputTailChars:])
	}
	return t
}

// globToRegexp turns a glob relative to a folder (*, ?, ** for any folders) into an
// anchored regular expression.
func globToRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			// "**/" = any folders (or none); a trailing "**" = anything.
			if i+2 < len(glob) && glob[i+2] == '/' {
				b.WriteString("(?:[^/]+/)*")
				i += 2
			} else {
				b.WriteString(".*")
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return regexp.MustCompile("^" + b.String() + "$")
}

func hasWildcard(s string) bool { return strings.ContainsAny(s, "*?") }

// setupHash is the hash of the recipe's setup and the sizes and times of the files its
// SetupInputs match; ok is false when they cannot be hashed cheaply (too many files, a
// listing cut short).
func setupHash(ctx context.Context, env sandboxhub.View, recipe AppServiceRecipe) (string, bool, error) {
	var lines []string
	files := 0
	for _, pattern := range recipe.SetupInputs {
		if !hasWildcard(pattern) {
			st, err := env.Stat(ctx, path.Join(recipe.Cwd, pattern))
			if err != nil {
				return "", false, err
			}
			if st == nil {
				lines = append(lines, pattern+"\tmissing")
			} else {
				lines = append(lines, pattern+"\t"+string(st.Type)+"\t"+strconv.Itoa(st.Size)+"\t"+strconv.FormatInt(st.ModifiedAt, 10))
			}
			continue
		}
		// List from the folder before the first wildcard.
		var fixed []string
		for _, part := range strings.Split(pattern, "/") {
			if hasWildcard(part) {
				break
			}
			fixed = append(fixed, part)
		}
		base := path.Join(append([]string{recipe.Cwd}, fixed...)...)
		baseStat, err := env.Stat(ctx, base)
		if err != nil {
			return "", false, err
		}
		if baseStat == nil || baseStat.Type != sandboxhub.EntryDirectory {
			lines = append(lines, pattern+"\tnone")
			continue
		}
		yes := true
		listing, err := env.List(ctx, base, sandboxhub.ListOptions{Recursive: &yes, IncludeHidden: &yes})
		if err != nil {
			return "", false, err
		}
		if listing.Truncated {
			return "", false, nil
		}
		re := globToRegexp(pattern)
		prefix := ""
		if len(fixed) > 0 {
			prefix = strings.Join(fixed, "/") + "/"
		}
		baseDir := strings.TrimRight(baseStat.Path, "/")
		type match struct{ path, rel string }
		var matched []match
		for _, e := range listing.Entries {
			if e.Type != sandboxhub.EntryFile {
				continue
			}
			rel := prefix + strings.TrimPrefix(strings.TrimPrefix(e.Path, baseDir), "/")
			if re.MatchString(rel) {
				matched = append(matched, match{e.Path, rel})
			}
		}
		sort.Slice(matched, func(i, j int) bool { return matched[i].rel < matched[j].rel })
		files += len(matched)
		if files > maxInputFiles {
			return "", false, nil
		}
		for _, m := range matched {
			st, err := env.Stat(ctx, m.path)
			if err != nil {
				return "", false, err
			}
			if st == nil {
				lines = append(lines, m.rel+"\tmissing")
			} else {
				lines = append(lines, m.rel+"\t"+strconv.Itoa(st.Size)+"\t"+strconv.FormatInt(st.ModifiedAt, 10))
			}
		}
	}
	raw, err := jsonx.Marshal(struct {
		Cwd    string            `json:"cwd"`
		Setup  string            `json:"setup"`
		Env    map[string]string `json:"env"`
		Inputs []string          `json:"inputs"`
	}{recipe.Cwd, recipe.Setup, recipe.Env, lines})
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true, nil
}

func readSetupState(ctx context.Context, env sandboxhub.View, app string) (string, error) {
	p := appsDir + "/" + app + "/" + SetupStateFile
	st, err := env.Stat(ctx, p)
	if err != nil || st == nil || st.Type != sandboxhub.EntryFile {
		return "", err
	}
	limit := 4096
	file, err := env.ReadFile(ctx, p, sandboxhub.ReadOptions{MaxBytes: &limit})
	if err != nil {
		return "", err
	}
	var state struct {
		Hash string `json:"hash"`
	}
	if json.Unmarshal([]byte(file.Content), &state) != nil {
		return "", nil //nolint:nilerr // a broken state file only means setup runs again
	}
	return state.Hash, nil
}

// fillEnv is the recipe's env with {{var:NAME}} then {{secret:NAME}} filled in.
func fillEnv(ctx context.Context, recipe AppServiceRecipe, opts StartAppOptions) (map[string]string, error) {
	keys := make([]string, 0, len(recipe.Env))
	for k := range recipe.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make(map[string]string, len(recipe.Env))
	for k, v := range recipe.Env {
		env[k] = v
	}
	unique := func(names func(string) []string) []string {
		var out []string
		seen := map[string]bool{}
		for _, k := range keys {
			for _, n := range names(env[k]) {
				if !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
		}
		return out
	}
	if names := unique(variable.PlaceholderNames); len(names) > 0 {
		if opts.Variables == nil {
			return nil, appStartError("the app's env uses {{var:"+names[0]+"}}, but this agent has no variables here", AppStepManifest, "")
		}
		values, err := opts.Variables.Resolve(ctx, names)
		if err != nil {
			return nil, appStartError("the app's env: "+err.Error(), AppStepManifest, "")
		}
		for k := range env {
			env[k] = variable.FillPlaceholders(env[k], values)
		}
	}
	if names := unique(secret.PlaceholderNames); len(names) > 0 {
		if opts.Secrets == nil {
			return nil, appStartError("the app's env uses {{secret:"+names[0]+"}}, but this agent has no secrets here", AppStepManifest, "")
		}
		values, err := opts.Secrets.Resolve(ctx, names, secret.Use{Tool: "service"}, secret.ResolveContext{SessionID: opts.SessionID})
		if err != nil {
			return nil, appStartError("the app's env: "+err.Error(), AppStepManifest, "")
		}
		for k := range env {
			env[k] = secret.FillPlaceholders(env[k], values)
		}
	}
	return env, nil
}

// StartApp starts a service app of apps/<app>/ (A-223): it runs the app's setup when its
// inputs changed, starts its command as a service and waits for the port. An app already
// running is returned as it is. It returns an *AppStartError for what the person can fix
// (no such app, a broken recipe, a failed setup, a command that exited) and the sandbox's
// error when the sandbox fails.
func StartApp(ctx context.Context, env sandboxhub.View, app string, opts StartAppOptions) (*StartedApp, error) {
	if !validAppName(app) {
		return nil, appStartError(`"`+app+`" is not an app folder name`, AppStepManifest, "")
	}
	m, problem, err := readManifest(ctx, env, app)
	if err != nil {
		return nil, err
	}
	if problem != "" {
		return nil, appStartError(`the app "`+app+`" is broken: `+problem, AppStepManifest, "")
	}
	if m == nil {
		e := appStartError(`no app "`+app+`" `+emDash+` there is no `+appsDir+"/"+app+"/"+appManifest, AppStepManifest, "")
		e.Code = AppCodeNotFound
		return nil, e
	}
	if m.service == nil {
		return nil, appStartError(m.name+` is not a service app `+emDash+` it has no "service" in its `+appManifest+"; show it with preview", AppStepManifest, "")
	}
	svc, ok := sandboxhub.AsServicesView(env)
	if !ok {
		e := appStartError("this agent's sandbox cannot run services", AppStepManifest, "")
		e.Code = AppCodeUnsupported
		return nil, e
	}
	recipe := *m.service
	started := &StartedApp{App: app, Name: m.name, Recipe: recipe, Link: AppServiceLink(recipe, m.name), Setup: SetupNone}

	services, err := svc.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range services {
		if s.Name == recipe.Name && s.Status != sandboxhub.ServiceExited {
			started.Service, started.AlreadyRunning = s, true
			return started, nil
		}
	}

	cwd, err := env.Stat(ctx, recipe.Cwd)
	if err != nil {
		return nil, err
	}
	if cwd == nil || cwd.Type != sandboxhub.EntryDirectory {
		return nil, appStartError(`the app's folder "`+recipe.Cwd+`" does not exist`, AppStepManifest, "")
	}
	vars, err := fillEnv(ctx, recipe, opts)
	if err != nil {
		return nil, err
	}

	if recipe.Setup != "" {
		if err := runSetup(ctx, env, app, m.name, recipe, vars, opts, started); err != nil {
			return nil, err
		}
	}

	spec := sandboxhub.ServiceSpec{Name: recipe.Name, Command: recipe.Command, Cwd: recipe.Cwd, Port: recipe.Port, Env: vars}
	info, err := svc.StartService(ctx, spec)
	if err != nil {
		return nil, err
	}
	wait := opts.Wait
	if wait <= 0 {
		wait = appStartWait
	}
	deadline := time.Now().Add(wait)
	for info.Status == sandboxhub.ServiceStarting && time.Now().Before(deadline) && ctx.Err() == nil {
		sleepCtx(ctx, startPoll)
		list, err := svc.ListServices(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			if s.Name == recipe.Name {
				info = s
			}
		}
	}
	if info.Status == sandboxhub.ServiceExited {
		code := ""
		if info.ExitCode != nil {
			code = " with code " + strconv.Itoa(*info.ExitCode)
		}
		output := ""
		if logs, err := svc.ServiceLogs(ctx, recipe.Name, outputTailChars); err == nil && strings.TrimSpace(logs.Text) != "" {
			output = strings.TrimRight(logs.Text, " \t\r\n")
			if logs.Truncated {
				output = ellipsis + output
			}
		}
		return nil, appStartError(m.name+" exited"+code+" before it listened on port "+strconv.Itoa(info.Port), AppStepCommand, output)
	}
	started.Service = info
	return started, nil
}

// runSetup runs the recipe's setup unless its inputs are unchanged, and remembers them.
func runSetup(ctx context.Context, env sandboxhub.View, app, name string, recipe AppServiceRecipe, vars map[string]string, opts StartAppOptions, started *StartedApp) error {
	if len(recipe.SetupInputs) > 0 {
		hash, ok, err := setupHash(ctx, env, recipe)
		if err != nil {
			return err
		}
		if ok {
			last, err := readSetupState(ctx, env, app)
			if err != nil {
				return err
			}
			if hash == last {
				started.Setup = SetupSkipped
				return nil
			}
		}
	}
	timeout := opts.SetupTimeout
	if timeout <= 0 {
		timeout = appSetupTimeout
	}
	yes := true
	result, err := env.Exec(ctx, sandboxhub.ExecRequest{
		Command: []string{recipe.Setup}, Shell: &yes, Cwd: recipe.Cwd, Env: vars,
		TimeoutMs: int(timeout / time.Millisecond), MaxOutputBytes: appSetupOutput,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		return err
	}
	out := result.Stdout
	if result.Stderr != "" {
		out += "\n" + result.Stderr
	}
	output := tailOf(out)
	switch {
	case result.Cancelled:
		return appStartError("setup of "+name+" was stopped", AppStepSetup, output)
	case result.TimedOut:
		return appStartError("setup of "+name+` ("`+recipe.Setup+`") took too long`, AppStepSetup, output)
	case result.ExitCode == nil || *result.ExitCode != 0:
		code := "unknown"
		if result.ExitCode != nil {
			code = strconv.Itoa(*result.ExitCode)
		}
		return appStartError("setup of "+name+` ("`+recipe.Setup+`") failed with code `+code, AppStepSetup, output)
	}
	started.Setup = SetupRan
	// Remember the inputs it ran on (hashed again: setup may have changed them, e.g. a lockfile).
	if len(recipe.SetupInputs) > 0 {
		after, ok, err := setupHash(ctx, env, recipe)
		if err != nil {
			return err
		}
		if ok {
			state, _ := jsonx.Marshal(map[string]string{"hash": after, "ranAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z")})
			if _, err := env.WriteFile(ctx, appsDir+"/"+app+"/"+SetupStateFile, string(state)+"\n", sandboxhub.WriteOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ListApps lists the apps of apps/ (folders with an app.json), static and service, with
// each service app's state (A-223).
func ListApps(ctx context.Context, env sandboxhub.View) ([]AppListing, error) {
	dir, err := env.Stat(ctx, appsDir)
	if err != nil {
		return nil, err
	}
	if dir == nil || dir.Type != sandboxhub.EntryDirectory {
		return []AppListing{}, nil
	}
	var services []sandboxhub.ServiceInfo
	if svc, ok := sandboxhub.AsServicesView(env); ok {
		if services, err = svc.ListServices(ctx); err != nil {
			return nil, err
		}
	}
	listing, err := env.List(ctx, appsDir, sandboxhub.ListOptions{})
	if err != nil {
		return nil, err
	}
	apps := []AppListing{}
	for _, entry := range listing.Entries {
		if entry.Type != sandboxhub.EntryDirectory || !validAppName(entry.Name) {
			continue
		}
		m, problem, err := readManifest(ctx, env, entry.Name)
		if err != nil {
			return nil, err
		}
		if problem != "" {
			apps = append(apps, AppListing{App: entry.Name, Name: entry.Name, Kind: "static", Problem: problem})
			continue
		}
		if m == nil {
			continue
		}
		item := AppListing{App: entry.Name, Name: m.name, Description: m.description}
		if m.service != nil {
			item.Kind, item.Service, item.Page, item.Status = "service", m.service.Name, m.service.Path, "stopped"
			for _, s := range services {
				if s.Name == m.service.Name && s.Status != sandboxhub.ServiceExited {
					item.Status = string(s.Status)
				}
			}
		} else {
			item.Kind, item.Page = "static", m.entry
		}
		apps = append(apps, item)
	}
	return apps, nil
}
