package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-223: service apps - apps/<app>/app.json with a "service" recipe that runs a project of
// the workspace as a service (any stack), started again by StartApp (the service tool's
// start with "app", or a host's start-app route). The cases of core/ts
// tests/unit/service-apps.test.ts.

// appServer is a tiny HTTP server on $PORT that prints its GREETING. Works in sh and cmd.
const appServer = `node -e "console.log('up');require('http').createServer((q,s)=>s.end(String(process.env.GREETING))).listen(process.env.PORT,'127.0.0.1')"`

// appSetup counts its runs in setup.log.
const appSetup = `node -e "require('fs').appendFileSync('setup.log','x')"`

func appManifestJSON(t *testing.T, service map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": "Hello API", "description": "says hi", "service": service})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func setupRunCount(t *testing.T, env sandboxhub.View, cwd string) int {
	t.Helper()
	st, err := env.Stat(context.Background(), cwd+"/setup.log")
	if err != nil || st == nil {
		return 0
	}
	return st.Size
}

func startErr(t *testing.T, err error) *AppStartError {
	t.Helper()
	var ae *AppStartError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want an *AppStartError", err)
	}
	return ae
}

func TestReadRecipeDefaults(t *testing.T) {
	r := setup(t)
	r.write(t, "apps/Hello_API/app.json", appManifestJSON(t, map[string]any{"command": "run", "setup": "npm ci", "setupInputs": []string{"./package-lock.json"}}))
	m, problem, err := readManifest(context.Background(), r.env, "Hello_API")
	if err != nil || problem != "" || m == nil || m.service == nil {
		t.Fatalf("manifest = %+v, problem %q, err %v", m, problem, err)
	}
	want := AppServiceRecipe{Name: "hello-api", Cwd: "apps/Hello_API", Setup: "npm ci", SetupInputs: []string{"package-lock.json"}, Command: "run", Env: map[string]string{}}
	if !reflect.DeepEqual(*m.service, want) {
		t.Fatalf("recipe = %+v, want %+v", *m.service, want)
	}
	if got := DefaultServiceName("My.App__2"); got != "my-app-2" {
		t.Fatalf("DefaultServiceName = %q", got)
	}
}

func TestReadRecipeFields(t *testing.T) {
	r := setup(t)
	r.write(t, "apps/api/app.json", appManifestJSON(t, map[string]any{
		"name": "api", "cwd": "./projects/api/", "command": "go run .", "path": "/docs", "port": 8080, "setupInputs": []string{"go.sum"}, "env": map[string]string{"A": "1"},
	}))
	m, _, _ := readManifest(context.Background(), r.env, "api")
	port := 8080
	want := AppServiceRecipe{Name: "api", Cwd: "projects/api", Command: "go run .", Path: "docs", Port: &port, Env: map[string]string{"A": "1"}}
	if m == nil || !reflect.DeepEqual(*m.service, want) {
		t.Fatalf("recipe = %+v", m)
	}
}

func TestReadRecipeRefusals(t *testing.T) {
	r := setup(t)
	cases := []struct {
		service any
		problem string
	}{
		{"x", `"service" must be an object`},
		{map[string]any{}, `"service" needs a "command"`},
		{map[string]any{"command": "x", "name": "Bad Name"}, `"service.name"`},
		{map[string]any{"command": "x", "cwd": "../other"}, `"service.cwd" must stay inside the workspace`},
		{map[string]any{"command": "x", "port": 70000}, `"service.port"`},
		{map[string]any{"command": "x", "setup": "y", "setupInputs": "a"}, `"service.setupInputs"`},
		{map[string]any{"command": "x", "env": map[string]string{"PORT": "1"}}, `cannot set "PORT"`},
		{map[string]any{"command": "x", "env": map[string]any{"A": 1}}, `"service.env.A" must be a string`},
		{map[string]any{"command": "x", "path": "../up"}, `"service.path"`},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(map[string]any{"service": c.service})
		r.write(t, "apps/b/app.json", string(raw))
		_, problem, err := readManifest(context.Background(), r.env, "b")
		if err != nil || !strings.Contains(problem, c.problem) {
			t.Errorf("%v: problem %q, want %q (err %v)", c.service, problem, c.problem, err)
		}
	}
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "cmd/server/main.go", true},
		{"*.go", "cmd/main.go", false},
		{"src/**", "src/a/b.ts", true},
		{"go.sum", "go.sum", true},
		{"a?.txt", "ab.txt", true},
	}
	for _, c := range cases {
		if got := globToRegexp(c.glob).MatchString(c.path); got != c.want {
			t.Errorf("%s ~ %s = %v", c.glob, c.path, got)
		}
	}
}

func TestListApps(t *testing.T) {
	r := setup(t)
	r.write(t, "apps/viewer/app.json", `{"name":"Viewer","opens":[".ifc"]}`)
	r.write(t, "apps/api/app.json", appManifestJSON(t, map[string]any{"command": "x", "path": "docs"}))
	r.write(t, "apps/bad/app.json", "{")
	r.write(t, "apps/empty/readme.md", "no manifest")
	apps, err := ListApps(context.Background(), r.env)
	if err != nil {
		t.Fatal(err)
	}
	byApp := map[string]AppListing{}
	for _, a := range apps {
		byApp[a.App] = a
	}
	if len(apps) != 3 {
		t.Fatalf("apps = %+v", apps)
	}
	if got := byApp["viewer"]; got != (AppListing{App: "viewer", Name: "Viewer", Kind: "static", Page: "index.html"}) {
		t.Errorf("viewer = %+v", got)
	}
	if got := byApp["api"]; got != (AppListing{App: "api", Name: "Hello API", Description: "says hi", Kind: "service", Service: "api", Status: "stopped", Page: "docs"}) {
		t.Errorf("api = %+v", got)
	}
	if !strings.Contains(byApp["bad"].Problem, "not valid JSON") {
		t.Errorf("bad = %+v", byApp["bad"])
	}
}

func TestPreviewServiceAppNotRunning(t *testing.T) {
	r := setup(t)
	r.write(t, "apps/api/app.json", appManifestJSON(t, map[string]any{"command": "x"}))
	out := r.run(t, "preview", `{"app":"api"}`)
	if !isErr(out) || !strings.Contains(out.Content, `start it with service (action start, app "api")`) {
		t.Fatalf("out = %+v", out)
	}
	if out := r.run(t, "preview", `{"app":"api","file":"files/a.ifc"}`); !strings.Contains(out.Content, "does not open files") {
		t.Fatalf("file = %s", out.Content)
	}
}

func TestStartAppRefusals(t *testing.T) {
	r := setup(t)
	r.write(t, "apps/viewer/app.json", `{"name":"Viewer"}`)
	r.write(t, "apps/api/app.json", appManifestJSON(t, map[string]any{"command": "x"}))
	cases := map[string]struct {
		want string
		code AppStartCode
	}{
		"nope":   {`no app "nope"`, AppCodeNotFound},
		"viewer": {"is not a service app", AppCodeInvalid},
		// The in-memory sandbox runs no services.
		"api":  {"cannot run services", AppCodeUnsupported},
		"../x": {"is not an app folder name", AppCodeInvalid},
	}
	for app, c := range cases {
		_, err := StartApp(context.Background(), r.env, app, StartAppOptions{})
		ae := startErr(t, err)
		if ae.Step != AppStepManifest || ae.Code != c.code || !strings.Contains(ae.Message, c.want) {
			t.Errorf("%s: %+v, want %q (%s)", app, ae, c.want, c.code)
		}
	}
}

func TestStartAppSetupOnceWhileInputsStay(t *testing.T) {
	r := hostSetup(t)
	ctx := context.Background()
	r.write(t, "projects/hello/package-lock.json", "{}")
	r.write(t, "apps/hello/app.json", appManifestJSON(t, map[string]any{
		"cwd": "projects/hello", "setup": appSetup, "setupInputs": []string{"package-lock.json"}, "command": appServer, "path": "docs", "env": map[string]string{"GREETING": "hi"},
	}))
	svc, _ := sandboxhub.AsServicesView(r.env)

	first, err := StartApp(ctx, r.env, "hello", StartAppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Setup != SetupRan || first.AlreadyRunning || first.Service.Name != "hello" || first.Service.Status != sandboxhub.ServiceListening {
		t.Fatalf("first = %+v", first)
	}
	if want := (kernel.ToolResultLink{URI: "service://hello/docs", Name: "Hello API", Type: "service"}); first.Link != want {
		t.Fatalf("link = %+v", first.Link)
	}
	if n := setupRunCount(t, r.env, "projects/hello"); n != 1 {
		t.Fatalf("setup ran %d times", n)
	}
	if st, _ := r.env.Stat(ctx, "apps/hello/"+SetupStateFile); st == nil {
		t.Fatal("no setup state file")
	}

	again, err := StartApp(ctx, r.env, "hello", StartAppOptions{})
	if err != nil || !again.AlreadyRunning || again.Setup != SetupNone {
		t.Fatalf("again = %+v, %v", again, err)
	}
	if apps, _ := ListApps(ctx, r.env); len(apps) != 1 || apps[0].Status != "listening" {
		t.Fatalf("apps = %+v", apps)
	}

	if _, err := svc.StopService(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	skipped, err := StartApp(ctx, r.env, "hello", StartAppOptions{})
	if err != nil || skipped.Setup != SetupSkipped || skipped.AlreadyRunning {
		t.Fatalf("skipped = %+v, %v", skipped, err)
	}

	// A changed input runs setup again.
	if _, err := svc.StopService(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	r.write(t, "projects/hello/package-lock.json", `{"lockfileVersion": 3, "changed": true}`)
	ran, err := StartApp(ctx, r.env, "hello", StartAppOptions{})
	if err != nil || ran.Setup != SetupRan {
		t.Fatalf("ran = %+v, %v", ran, err)
	}
	if n := setupRunCount(t, r.env, "projects/hello"); n != 2 {
		t.Fatalf("setup ran %d times", n)
	}
}

func TestStartAppSetupEveryStartWithoutInputs(t *testing.T) {
	r := hostSetup(t)
	ctx := context.Background()
	r.write(t, "apps/hello/keep.txt", "")
	r.write(t, "apps/hello/app.json", appManifestJSON(t, map[string]any{"setup": appSetup, "command": appServer}))
	svc, _ := sandboxhub.AsServicesView(r.env)
	for i := range 2 {
		started, err := StartApp(ctx, r.env, "hello", StartAppOptions{})
		if err != nil || started.Setup != SetupRan {
			t.Fatalf("start %d = %+v, %v", i, started, err)
		}
		if _, err := svc.StopService(ctx, "hello"); err != nil {
			t.Fatal(err)
		}
	}
	if n := setupRunCount(t, r.env, "apps/hello"); n != 2 {
		t.Fatalf("setup ran %d times", n)
	}
}

func TestStartAppFailures(t *testing.T) {
	r := hostSetup(t)
	ctx := context.Background()
	r.write(t, "apps/bad/app.json", appManifestJSON(t, map[string]any{"setup": `node -e "console.error('no toolchain');process.exit(3)"`, "command": appServer}))
	_, err := StartApp(ctx, r.env, "bad", StartAppOptions{})
	ae := startErr(t, err)
	if ae.Step != AppStepSetup || ae.Code != AppCodeSetupFailed || !strings.Contains(ae.Message, "failed with code 3") || !strings.Contains(ae.Output, "no toolchain") {
		t.Fatalf("setup = %+v", ae)
	}

	r.write(t, "apps/bad/app.json", appManifestJSON(t, map[string]any{"command": `node -e "console.error('port taken');process.exit(1)"`}))
	_, err = StartApp(ctx, r.env, "bad", StartAppOptions{})
	ae = startErr(t, err)
	if ae.Step != AppStepCommand || ae.Code != AppCodeExited || !strings.Contains(ae.Message, "exited with code 1") || !strings.Contains(ae.Output, "port taken") {
		t.Fatalf("command = %+v", ae)
	}
}

// fakeVariables and fakeSecrets resolve every name to one value.
type fakeVariables struct {
	variable.Variables
	value string
}

func (f fakeVariables) Resolve(_ context.Context, names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range names {
		out[n] = f.value
	}
	return out, nil
}

type fakeSecrets struct {
	secret.Secrets
	value string
	uses  *[]secret.Use
}

func (f fakeSecrets) Resolve(_ context.Context, names []string, use secret.Use, _ secret.ResolveContext) (map[string]string, error) {
	*f.uses = append(*f.uses, use)
	out := map[string]string{}
	for _, n := range names {
		out[n] = f.value
	}
	return out, nil
}

func TestStartAppFillsEnv(t *testing.T) {
	r := hostSetup(t)
	ctx := context.Background()
	r.write(t, "apps/hello/app.json", appManifestJSON(t, map[string]any{"command": appServer, "env": map[string]string{"GREETING": "{{var:WHO}}-{{secret:KEY}}"}}))
	_, err := StartApp(ctx, r.env, "hello", StartAppOptions{})
	if ae := startErr(t, err); ae.Step != AppStepManifest || !strings.Contains(ae.Message, "{{var:WHO}}") {
		t.Fatalf("missing = %+v", ae)
	}

	var uses []secret.Use
	vars := fakeVariables{value: "world"}
	started, err := StartApp(ctx, r.env, "hello", StartAppOptions{Variables: vars, Secrets: fakeSecrets{value: "s3", uses: &uses}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uses, []secret.Use{{Tool: "service"}}) {
		t.Fatalf("uses = %+v", uses)
	}
	svc, _ := sandboxhub.AsServicesView(r.env)
	conn, err := svc.Connect(ctx, started.Service.Port)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	_, _ = io.Copy(&body, conn)
	if !strings.Contains(body.String(), "world-s3") {
		t.Fatalf("body = %q", body.String())
	}
}

func TestServiceStartWithApp(t *testing.T) {
	r := hostSetup(t)
	r.write(t, "apps/hello/app.json", appManifestJSON(t, map[string]any{"setup": appSetup, "command": appServer, "path": "docs"}))
	out := r.run(t, "service", `{"action":"start","app":"apps/hello","name":"","command":""}`)
	if isErr(out) {
		t.Fatalf("start failed: %s", out.Content)
	}
	want := kernel.ToolResultLink{URI: "service://hello/docs", Name: "Hello API", Type: "service"}
	if out.Link == nil || *out.Link != want {
		t.Fatalf("link = %+v", out.Link)
	}
	if !strings.Contains(out.Content, "Hello API is running on port") || !strings.Contains(out.Content, "Setup ran") {
		t.Fatalf("content = %s", out.Content)
	}
	if v, _ := out.Metadata.Get("setup"); v != SetupRan {
		t.Fatalf("metadata = %+v", out.Metadata)
	}

	shown := r.run(t, "preview", `{"app":"hello"}`)
	if shown.Link == nil || *shown.Link != want || !strings.Contains(shown.Content, "Hello API is live next to the chat.") {
		t.Fatalf("preview = %+v", shown)
	}
	if again := r.run(t, "service", `{"action":"start","app":"hello"}`); !strings.Contains(again.Content, "already running") {
		t.Fatalf("again = %s", again.Content)
	}
	bad := r.run(t, "service", `{"action":"start","app":"nope"}`)
	if !isErr(bad) || !strings.Contains(bad.Content, `service start: no app "nope"`) {
		t.Fatalf("bad = %+v", bad)
	}
	if step, _ := bad.Metadata.Get("step"); step != "manifest" {
		t.Fatalf("bad metadata = %+v", bad.Metadata)
	}
}
