package llmhub_test

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// hubErrors mirrors llm-hub-errors.json: the recorded texts and order of the
// provider hub, provider resolution and provider listing.
type hubErrors struct {
	NoProvider      string   `json:"noProvider"`
	NotRegistered   string   `json:"notRegistered"`
	UnknownProvider string   `json:"unknownProvider"`
	BuiltinOrder    []string `json:"builtinOrder"`
}

func loadHubErrors(t *testing.T) hubErrors {
	t.Helper()
	var e hubErrors
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "llm-hub-errors.json"), &e); err != nil {
		t.Fatalf("decode llm-hub-errors.json: %v", err)
	}
	return e
}

// fakeProvider is a test-local llmhub.Provider; the aecmock MockProvider arrives
// with the conformance plan. The tag tells two providers of one name apart.
type fakeProvider struct {
	name string
	tag  string
}

func (p *fakeProvider) Name() string    { return p.name }
func (p *fakeProvider) Version() string { return "1.0.0" }

func (p *fakeProvider) Chat(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
	return kernel.ModelResponse{}, nil
}

func (p *fakeProvider) Stream(context.Context, []kernel.Message, llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(func(kernel.StreamChunk, error) bool) {}
}

func (p *fakeProvider) CountTokens(context.Context, []kernel.Message) (int, error) { return 0, nil }

var _ llmhub.Provider = (*fakeProvider)(nil)

func names(ps []llmhub.Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHubFirstRegisteredIsPrimary(t *testing.T) {
	h := llmhub.NewLLMHub()
	a, b := &fakeProvider{name: "a"}, &fakeProvider{name: "b"}
	if err := h.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(b); err != nil {
		t.Fatal(err)
	}
	p, err := h.Primary()
	if err != nil || p != llmhub.Provider(a) {
		t.Fatalf("Primary = %v, %v; want the first registered provider", p, err)
	}
	if got := names(h.List()); !equalStrings(got, []string{"a", "b"}) {
		t.Fatalf("List = %v, want [a b]", got)
	}
}

func TestHubReRegisterKeepsPositionAndPrimary(t *testing.T) {
	h := llmhub.NewLLMHub()
	first := &fakeProvider{name: "a", tag: "first"}
	second := &fakeProvider{name: "b"}
	replacement := &fakeProvider{name: "a", tag: "replacement"}
	for _, p := range []llmhub.Provider{first, second, replacement} {
		if err := h.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := h.Get("a")
	if !ok || got != llmhub.Provider(replacement) {
		t.Fatalf("Get(a) = %v, %v; want the replacement", got, ok)
	}
	if list := names(h.List()); !equalStrings(list, []string{"a", "b"}) {
		t.Fatalf("List = %v, want [a b] (the position of a is kept)", list)
	}
	if p, err := h.Primary(); err != nil || p != llmhub.Provider(replacement) {
		t.Fatalf("Primary = %v, %v; want the replacement of the first provider", p, err)
	}
}

func TestHubSetPrimarySwitches(t *testing.T) {
	h := llmhub.NewLLMHub()
	b := &fakeProvider{name: "b"}
	for _, p := range []llmhub.Provider{&fakeProvider{name: "a"}, b} {
		if err := h.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.SetPrimary("b"); err != nil {
		t.Fatal(err)
	}
	if p, err := h.Primary(); err != nil || p != llmhub.Provider(b) {
		t.Fatalf("Primary = %v, %v; want b", p, err)
	}
	// Registering another provider must not move the primary.
	if err := h.Register(&fakeProvider{name: "c"}); err != nil {
		t.Fatal(err)
	}
	if p, err := h.Primary(); err != nil || p.Name() != "b" {
		t.Fatalf("Primary = %v, %v; want b to stay primary", p, err)
	}
}

func TestHubGetMissing(t *testing.T) {
	p, ok := llmhub.NewLLMHub().Get("missing")
	if ok || p != nil {
		t.Fatalf("Get(missing) = %v, %v; want nil, false", p, ok)
	}
}

func TestHubListIsAFreshSlice(t *testing.T) {
	h := llmhub.NewLLMHub()
	if err := h.Register(&fakeProvider{name: "a"}); err != nil {
		t.Fatal(err)
	}
	list := h.List()
	list[0] = nil
	if got := h.List(); got[0] == nil {
		t.Fatal("mutating the List result changed the hub")
	}
}

func TestHubPrimaryOnEmptyHub(t *testing.T) {
	want := loadHubErrors(t).NoProvider
	_, err := llmhub.NewLLMHub().Primary()
	if err == nil || err.Error() != want {
		t.Fatalf("Primary error = %v, want text %q", err, want)
	}
	if !errors.Is(err, llmhub.ErrNoProvider) {
		t.Fatalf("error %v does not match ErrNoProvider", err)
	}
}

func TestHubSetPrimaryUnknown(t *testing.T) {
	want := loadHubErrors(t).NotRegistered
	// The fixture was produced for the name "missing".
	err := llmhub.NewLLMHub().SetPrimary("missing")
	if err == nil || err.Error() != want {
		t.Fatalf("SetPrimary error = %v, want text %q", err, want)
	}
	if !errors.Is(err, llmhub.ErrProviderNotRegistered) {
		t.Fatalf("error %v does not match ErrProviderNotRegistered", err)
	}
}

func TestHubRegisterNil(t *testing.T) {
	err := llmhub.NewLLMHub().Register(nil)
	if !errors.Is(err, llmhub.ErrNilProvider) {
		t.Fatalf("Register(nil) = %v, want ErrNilProvider", err)
	}
}

func TestHubZeroValueIsUsable(t *testing.T) {
	var h llmhub.LLMHub
	if err := h.Register(&fakeProvider{name: "a"}); err != nil {
		t.Fatal(err)
	}
	if p, err := h.Primary(); err != nil || p.Name() != "a" {
		t.Fatalf("Primary = %v, %v", p, err)
	}
}

func TestHubConcurrentAccess(t *testing.T) {
	h := llmhub.NewLLMHub()
	if err := h.Register(&fakeProvider{name: "seed"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(4)
		name := string(rune('a' + i))
		go func() {
			defer wg.Done()
			_ = h.Register(&fakeProvider{name: name})
		}()
		go func() {
			defer wg.Done()
			_, _ = h.Get(name)
			_ = h.List()
		}()
		go func() {
			defer wg.Done()
			_, _ = h.Primary()
		}()
		go func() {
			defer wg.Done()
			_ = h.SetPrimary("seed")
		}()
	}
	wg.Wait()
	if _, err := h.Primary(); err != nil {
		t.Fatalf("Primary after the concurrent run: %v", err)
	}
}

// panickyNameProvider panics in Name.
type panickyNameProvider struct{ fakeProvider }

func (*panickyNameProvider) Name() string { panic("no name") }

// TestHubRegisterRejectsATypedNilAndAPanickingName pins IN-04 for the LLM hub.
func TestHubRegisterRejectsATypedNilAndAPanickingName(t *testing.T) {
	h := llmhub.NewLLMHub()
	var typedNil *fakeProvider
	if err := h.Register(typedNil); !errors.Is(err, llmhub.ErrNilProvider) {
		t.Fatalf("Register(typed nil) = %v, want ErrNilProvider", err)
	}
	if err := h.Register(&panickyNameProvider{}); err == nil {
		t.Fatal("Register of a panicking Name must return an error")
	}
	if len(h.List()) != 0 {
		t.Fatal("a failed Register must leave the hub empty")
	}
}
