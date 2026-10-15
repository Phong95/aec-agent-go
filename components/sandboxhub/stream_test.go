package sandboxhub_test

import (
	"context"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub/host"
	"github.com/Phong95/aec-agent-go/internal/streamcheck"
)

// A-124: the "fs.stream" suite on the in-memory sandbox (opt-in capability) and on a
// host sandbox in a temp folder; the docker package runs it against a real container
// (opt-in, AEC_DOCKER_TESTS=1). The traced wrapper passes the methods through.

func TestStreamInMemory(t *testing.T) {
	streamcheck.Run(t, func(t *testing.T) sandboxhub.View {
		t.Helper()
		hub := sandboxhub.NewSandboxHub()
		provider := aecmock.NewInMemoryProvider(aecmock.WithCapabilities(
			sandboxhub.CapabilityFSRead, sandboxhub.CapabilityFSWrite, sandboxhub.CapabilityFSSearch,
			sandboxhub.CapabilityExec, sandboxhub.CapabilityFSStream,
		))
		if err := hub.Register(provider); err != nil {
			t.Fatal(err)
		}
		if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
			t.Fatal(err)
		}
		v, err := hub.Resolve(context.Background(), "a1", sandboxhub.Selection{})
		if err != nil {
			t.Fatal(err)
		}
		// Through the traced wrapper, as agents and servers see it.
		return sandboxhub.WithTracing(v, func(string, map[string]any) sandboxhub.SpanEnder { return nil }, sandboxhub.TraceScope{})
	})
}

func TestStreamHost(t *testing.T) {
	streamcheck.Run(t, func(t *testing.T) sandboxhub.View {
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
		t.Cleanup(func() { _ = hub.Dispose(context.Background()) })
		return v
	})
}

// A view without "fs.stream" is not a StreamView, also through the traced wrapper.
func TestStreamAbsentWithoutCapability(t *testing.T) {
	hub, _ := inMemoryHub(t)
	v, err := hub.Resolve(context.Background(), "a1", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sandboxhub.AsStreamView(v); ok {
		t.Fatal("the default in-memory view has fs.stream")
	}
	traced := sandboxhub.WithTracing(v, func(string, map[string]any) sandboxhub.SpanEnder { return nil }, sandboxhub.TraceScope{})
	if _, ok := sandboxhub.AsStreamView(traced); ok {
		t.Fatal("the traced view claims fs.stream its inner view lacks")
	}
}
