package sandboxhub_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub/host"
	"github.com/Phong95/aec-agent-go/internal/servicecheck"
)

// A-124 phase 2: the "services" suite on a host sandbox in a temp folder (real
// processes, node required); the docker package runs it against a real container
// (opt-in, AEC_DOCKER_TESTS=1). The traced wrapper passes the methods through.

func TestServicesHost(t *testing.T) {
	servicecheck.Run(t, func(t *testing.T) sandboxhub.View {
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
		v, err := hub.Resolve(context.Background(), "a1", sandboxhub.Selection{})
		if err != nil {
			t.Fatal(err)
		}
		// Dispose ends the services too (registered first, so it runs after the suite's stop).
		t.Cleanup(func() { _ = hub.Dispose(context.Background()) })
		// Through the traced wrapper, as agents and servers see it.
		return sandboxhub.WithTracing(v, func(string, map[string]any) sandboxhub.SpanEnder { return nil }, sandboxhub.TraceScope{})
	})
}

// A view without "services" is not a ServicesView, also through the traced wrapper.
func TestServicesAbsentWithoutCapability(t *testing.T) {
	hub, _ := inMemoryHub(t)
	v, err := hub.Resolve(context.Background(), "a1", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sandboxhub.AsServicesView(v); ok {
		t.Fatal("the in-memory view has services")
	}
	traced := sandboxhub.WithTracing(v, func(string, map[string]any) sandboxhub.SpanEnder { return nil }, sandboxhub.TraceScope{})
	if _, ok := sandboxhub.AsServicesView(traced); ok {
		t.Fatal("the traced view claims services its inner view lacks")
	}
	if _, err := traced.(sandboxhub.ServicesView).ListServices(context.Background()); err == nil {
		t.Fatal("ListServices through the traced view of a view without services did not fail")
	}
}

// exitCode is on the wire exactly when the service exited, as a number or null.
func TestServiceInfoJSON(t *testing.T) {
	three := 3
	cases := []struct {
		info sandboxhub.ServiceInfo
		want string
	}{
		{
			info: sandboxhub.ServiceInfo{Name: "w", Command: "c", Cwd: "/workspace", Port: 1, Status: sandboxhub.ServiceListening, ExitCode: &three, StartedAt: 5},
			want: `{"name":"w","command":"c","cwd":"/workspace","port":1,"status":"listening","startedAt":5}`,
		},
		{
			info: sandboxhub.ServiceInfo{Name: "w", Command: "c", Cwd: "/workspace", Port: 1, Status: sandboxhub.ServiceExited, StartedAt: 5},
			want: `{"name":"w","command":"c","cwd":"/workspace","port":1,"status":"exited","exitCode":null,"startedAt":5}`,
		},
		{
			info: sandboxhub.ServiceInfo{Name: "w", Command: "c", Cwd: "/workspace", Port: 1, Status: sandboxhub.ServiceExited, ExitCode: &three, StartedAt: 5},
			want: `{"name":"w","command":"c","cwd":"/workspace","port":1,"status":"exited","exitCode":3,"startedAt":5}`,
		},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.info)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("json = %s, want %s", got, c.want)
		}
	}
}
