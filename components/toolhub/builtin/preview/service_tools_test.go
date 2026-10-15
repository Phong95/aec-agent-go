package preview

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub/host"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-124 phase 2: the service tools on a host sandbox (real processes, node required) -
// the "service tools" cases of core/ts tests/unit/preview-tools.test.ts.

const testServer = `node -e "console.log('ready');require('http').createServer((q,s)=>s.end('ok')).listen(process.env.PORT,'127.0.0.1')"`

func hostSetup(t *testing.T) rig {
	t.Helper()
	provider, err := host.New(host.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	hub := sandboxhub.NewSandboxHub()
	if err := hub.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindHost}); err != nil {
		t.Fatal(err)
	}
	env, err := hub.Resolve(context.Background(), "svc", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	// Dispose ends every service the case left running.
	t.Cleanup(func() { _ = hub.Dispose(context.Background()) })
	tools := map[string]toolhub.Handler{}
	for _, h := range NewTools() {
		tools[h.Definition().Name] = h
	}
	return rig{
		env:   env,
		tc:    toolhub.Context{SessionID: "s1", AgentID: "svc", GetSandbox: func(context.Context) (sandboxhub.View, error) { return env, nil }},
		tools: tools,
	}
}

func TestServiceToolsStartListLogsStop(t *testing.T) {
	r := hostSetup(t)
	out := r.run(t, "start_service", `{"name":"web","command":`+jsonString(testServer)+`}`)
	if isErr(out) {
		t.Fatalf("start_service failed: %s", out.Content)
	}
	want := kernel.ToolResultLink{URI: "service://web/", Name: "web", Type: "service"}
	if out.Link == nil || *out.Link != want {
		t.Fatalf("link = %+v, want %+v", out.Link, want)
	}
	if !strings.Contains(out.Content, "is running on port") || !strings.Contains(out.Content, "[web](service://web/)") {
		t.Fatalf("content = %s", out.Content)
	}

	list := r.run(t, "list_services", `{}`)
	if !regexp.MustCompile(`web ` + midDot + ` listening ` + midDot + ` port \d+`).MatchString(list.Content) {
		t.Fatalf("list = %s", list.Content)
	}
	logs := r.run(t, "service_logs", `{"name":"web"}`)
	if !strings.Contains(logs.Content, "ready") {
		t.Fatalf("logs = %s", logs.Content)
	}

	if stopped := r.run(t, "stop_service", `{"name":"web"}`); stopped.Content != "Stopped web." {
		t.Fatalf("stop = %s", stopped.Content)
	}
	if missing := r.run(t, "stop_service", `{"name":"web2"}`); !isErr(missing) {
		t.Fatalf("stop of a missing service = %+v", missing)
	}
}

func TestServiceToolsReportAnExit(t *testing.T) {
	r := hostSetup(t)
	out := r.run(t, "start_service", `{"name":"bad","command":`+jsonString(`node -e "console.error('missing module');process.exit(1)"`)+`}`)
	if !isErr(out) || out.Retryable == nil || *out.Retryable {
		t.Fatalf("out = %+v", out)
	}
	if !strings.Contains(out.Content, "exited with code 1") || !strings.Contains(out.Content, "missing module") {
		t.Fatalf("content = %s", out.Content)
	}
	if out.Link != nil {
		t.Fatalf("link = %+v", out.Link)
	}
}

func TestServiceToolsWithoutServices(t *testing.T) {
	r := setup(t)
	out := r.run(t, "start_service", `{"name":"web","command":"x"}`)
	if !isErr(out) || out.Retryable == nil || *out.Retryable {
		t.Fatalf("out = %+v", out)
	}
	if !strings.Contains(out.Content, "cannot run services") {
		t.Fatalf("content = %s", out.Content)
	}
}

// jsonString quotes s as a JSON string (the commands hold quotes).
func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
