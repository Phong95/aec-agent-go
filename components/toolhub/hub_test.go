package toolhub_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// hubErrors are the expected error texts, decoded from the recorded fixture; the
// tests never type them by hand.
type hubErrors struct {
	AlreadyRegistered string `json:"alreadyRegistered"`
	NotAvailable      string `json:"notAvailable"`
}

func loadHubErrors(t *testing.T) hubErrors {
	t.Helper()
	var e hubErrors
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "tool-hub-errors.json"), &e); err != nil {
		t.Fatalf("decode tool-hub-errors.json: %v", err)
	}
	return e
}

// stubTool is a test Handler. exec is optional.
type stubTool struct {
	name  string
	calls atomic.Int32
	exec  func(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error)
}

func (s *stubTool) Name() string    { return "tool:" + s.name }
func (s *stubTool) Version() string { return "1.0.0" }

func (s *stubTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        s.name,
		Description: "stub",
		Parameters:  toolhub.JSONSchema{Type: "object"},
	}
}

func (s *stubTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	s.calls.Add(1)
	if s.exec != nil {
		return s.exec(ctx, args, tc)
	}
	return toolhub.Output{Content: "ok"}, nil
}

// spyPolicy is a test Policy with a fixed decision that counts its calls.
type spyPolicy struct {
	name     string
	decision toolhub.Decision
	panicMsg string
	calls    atomic.Int32
}

func (s *spyPolicy) Name() string    { return s.name }
func (s *spyPolicy) Version() string { return "1.0.0" }

func (s *spyPolicy) Check(context.Context, string, kernel.Arguments, toolhub.Context, toolhub.Definition, []string) toolhub.Decision {
	s.calls.Add(1)
	if s.panicMsg != "" {
		panic(s.panicMsg)
	}
	return s.decision
}

func TestHubRegisterDuplicateAndNil(t *testing.T) {
	want := loadHubErrors(t)
	hub := toolhub.NewToolHub()
	if err := hub.Register(&stubTool{name: "schedule_lookup"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := hub.Register(&stubTool{name: "schedule_lookup"})
	if err == nil {
		t.Fatal("a duplicate register must fail")
	}
	if err.Error() != want.AlreadyRegistered {
		t.Fatalf("error text = %q, want the TS text %q", err.Error(), want.AlreadyRegistered)
	}
	if !errors.Is(err, toolhub.ErrAlreadyRegistered) {
		t.Fatalf("error does not match ErrAlreadyRegistered: %v", err)
	}
	if err := hub.Register(nil); !errors.Is(err, toolhub.ErrNilHandler) {
		t.Fatalf("Register(nil) = %v, want ErrNilHandler", err)
	}
}

func TestHubGetGetByNamesAndList(t *testing.T) {
	hub := toolhub.NewToolHub()
	for _, n := range []string{"c", "a", "b"} {
		if err := hub.Register(&stubTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	if d, ok := hub.Get("a"); !ok || d.Name != "a" {
		t.Fatalf("Get(a) = %v, %v", d, ok)
	}
	if _, ok := hub.Get("zzz"); ok {
		t.Fatal("Get of an unknown name must report false")
	}

	names := func(ds []toolhub.Definition) []string {
		out := make([]string, 0, len(ds))
		for _, d := range ds {
			out = append(out, d.Name)
		}
		return out
	}
	if got := names(hub.GetByNames([]string{"b", "zzz", "c", "b"})); !slices.Equal(got, []string{"b", "c", "b"}) {
		t.Fatalf("GetByNames = %v, want unknown names skipped and the given order kept", got)
	}
	if got := names(hub.List()); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("List = %v, want insertion order", got)
	}
	list := hub.List()
	list[0] = toolhub.Definition{}
	if hub.List()[0].Name == "" {
		t.Fatal("List must return a fresh slice")
	}
}

// TestLookupReturnsIndependentDefinitions pins that a caller cannot change what
// the hub checks by mutating a definition it got from Get or List, nor by
// mutating the schema of the handler after registration (IN-01).
func TestLookupReturnsIndependentDefinitions(t *testing.T) {
	props := toolhub.NewProperties(toolhub.Property{Name: "a", Schema: toolhub.JSONSchema{Type: "string"}})
	handler := &schemaTool{def: toolhub.Definition{
		Name:         "t",
		Parameters:   toolhub.JSONSchema{Type: "object", Properties: props, Required: []string{"a"}},
		AllowedModes: []string{"plan"},
	}}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}

	// The handler's own schema changes after registration: the hub keeps its copy.
	props.Set("b", toolhub.JSONSchema{Type: "string"})
	got, _ := hub.Get("t")
	if got.Parameters.Properties.Len() != 1 {
		t.Fatalf("registered schema shares the handler's Properties: %v", got.Parameters.Properties.Names())
	}

	// A caller mutating a looked-up definition does not reach the hub.
	got.Parameters.Properties.Set("c", toolhub.JSONSchema{Type: "string"})
	got.Parameters.Required[0] = "zzz"
	got.AllowedModes[0] = "zzz"
	again, _ := hub.Get("t")
	if again.Parameters.Properties.Len() != 1 || again.Parameters.Required[0] != "a" || again.AllowedModes[0] != "plan" {
		t.Fatalf("a mutated lookup result leaked into the hub: %+v", again)
	}
	if hub.List()[0].Parameters.Properties.Len() != 1 || hub.GetByNames([]string{"t"})[0].Parameters.Properties.Len() != 1 {
		t.Fatal("List or GetByNames shares state with a previous result")
	}
}

// schemaTool is a Handler with a fixed definition.
type schemaTool struct{ def toolhub.Definition }

func (s *schemaTool) Name() string                   { return "tool:" + s.def.Name }
func (s *schemaTool) Version() string                { return "1.0.0" }
func (s *schemaTool) Definition() toolhub.Definition { return s.def }
func (s *schemaTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{Content: "ok"}, nil
}

// panickyTool panics in Definition.
type panickyTool struct{ schemaTool }

func (*panickyTool) Definition() toolhub.Definition { panic("no definition") }

func TestRegisterRejectsATypedNilAndAPanickingDefinition(t *testing.T) {
	hub := toolhub.NewToolHub()
	var typedNil *stubTool
	if err := hub.Register(typedNil); !errors.Is(err, toolhub.ErrNilHandler) {
		t.Fatalf("Register(typed nil) = %v, want ErrNilHandler", err)
	}
	err := hub.Register(&panickyTool{})
	if err == nil || !strings.Contains(err.Error(), "no definition") {
		t.Fatalf("Register of a panicking Definition = %v, want an error", err)
	}
	if len(hub.List()) != 0 {
		t.Fatal("a failed Register must leave the hub empty")
	}
}

func TestPropertiesSetOnNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Set on a nil *Properties must panic: it has nothing to store into")
		}
	}()
	var p *toolhub.Properties
	p.Set("a", toolhub.JSONSchema{})
}

func TestAuthorizeUnknownToolNeverConsultsPolicy(t *testing.T) {
	want := loadHubErrors(t)
	hub := toolhub.NewToolHub()
	spy := &spyPolicy{name: "policy:spy", decision: toolhub.Allow()}
	hub.SetPolicy(spy)

	authorized, decision, err := hub.Authorize(t.Context(), "schedule_lookup", kernel.Arguments(`{}`), toolhub.Context{}, nil)
	if err == nil {
		t.Fatal("an unknown tool must return an error")
	}
	if err.Error() != want.NotAvailable {
		t.Fatalf("error text = %q, want the TS text %q", err.Error(), want.NotAvailable)
	}
	if !errors.Is(err, toolhub.ErrNotFound) {
		t.Fatalf("error does not match ErrNotFound: %v", err)
	}
	if decision.Allowed {
		t.Fatal("an unknown tool must not be allowed")
	}
	if spy.calls.Load() != 0 {
		t.Fatalf("the policy was consulted %d times for an unknown tool", spy.calls.Load())
	}
	if _, execErr := authorized.Execute(t.Context()); !errors.Is(execErr, toolhub.ErrNotAuthorized) {
		t.Fatalf("Execute on the zero Authorized = %v", execErr)
	}
}

func TestDenySpyNeverReachesHandler(t *testing.T) {
	hub := toolhub.NewToolHub()
	handler := &stubTool{name: "t"}
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	spy := &spyPolicy{name: "policy:spy", decision: toolhub.Deny("no way")}
	hub.SetPolicy(spy)

	authorized, decision, err := hub.Authorize(t.Context(), "t", kernel.Arguments(`{}`), toolhub.Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed || decision.Reason == nil || *decision.Reason != "no way" {
		t.Fatalf("decision = %+v, want the spy's denial", decision)
	}
	if spy.calls.Load() != 1 {
		t.Fatalf("policy calls = %d", spy.calls.Load())
	}
	if _, err := authorized.Execute(t.Context()); !errors.Is(err, toolhub.ErrNotAuthorized) {
		t.Fatalf("Execute after a denial = %v, want ErrNotAuthorized", err)
	}
	if handler.calls.Load() != 0 {
		t.Fatalf("handler ran %d times after a denial", handler.calls.Load())
	}
}

func TestSetPolicyAppliesToTheNextCall(t *testing.T) {
	hub := toolhub.NewToolHub()
	handler := &stubTool{name: "t"}
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	args := kernel.Arguments(`{}`)

	if hub.Policy().Name() != "policy:default" {
		t.Fatalf("initial policy = %s", hub.Policy().Name())
	}
	if _, d, err := hub.Authorize(t.Context(), "t", args, toolhub.Context{}, nil); err != nil || !d.Allowed {
		t.Fatalf("default policy must allow: %+v %v", d, err)
	}

	spy := &spyPolicy{name: "policy:spy", decision: toolhub.Deny("spy says no")}
	hub.SetPolicy(spy)
	if hub.Policy().Name() != "policy:spy" {
		t.Fatalf("policy after SetPolicy = %s", hub.Policy().Name())
	}
	if _, d, err := hub.Authorize(t.Context(), "t", args, toolhub.Context{}, nil); err != nil || d.Allowed {
		t.Fatalf("the swapped-in policy must be consulted: %+v %v", d, err)
	}
	if spy.calls.Load() != 1 {
		t.Fatalf("spy calls = %d, want 1", spy.calls.Load())
	}

	hub.SetPolicy(nil)
	if hub.Policy().Name() != "policy:default" {
		t.Fatalf("SetPolicy(nil) must restore the default, got %s", hub.Policy().Name())
	}
	if _, d, err := hub.Authorize(t.Context(), "t", args, toolhub.Context{}, nil); err != nil || !d.Allowed {
		t.Fatalf("restored default must allow: %+v %v", d, err)
	}
}

func TestAuthorizeTurnsAPolicyPanicIntoADenial(t *testing.T) {
	hub := toolhub.NewToolHub()
	handler := &stubTool{name: "t"}
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	hub.SetPolicy(&spyPolicy{name: "policy:panics", panicMsg: "kaboom"})

	authorized, decision, err := hub.Authorize(t.Context(), "t", kernel.Arguments(`{}`), toolhub.Context{}, nil)
	if err != nil {
		t.Fatalf("a policy panic must be a denial, not an error: %v", err)
	}
	if decision.Allowed || decision.Reason == nil || !strings.HasPrefix(*decision.Reason, "Policy check failed: ") || !strings.Contains(*decision.Reason, "kaboom") {
		t.Fatalf("decision = %+v", decision)
	}
	if _, execErr := authorized.Execute(t.Context()); !errors.Is(execErr, toolhub.ErrNotAuthorized) || handler.calls.Load() != 0 {
		t.Fatalf("a panicking policy must not authorize: err=%v calls=%d", execErr, handler.calls.Load())
	}
}

func TestAuthorizedHoldsAPrivateCopyOfTheArguments(t *testing.T) {
	var received kernel.Arguments
	handler := &stubTool{name: "t", exec: func(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
		received = bytes.Clone(args)
		for i := range args { // a misbehaving handler
			args[i] = 'X'
		}
		return toolhub.Output{Content: "ok"}, nil
	}}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}

	original := []byte(`{"a":1}`)
	callerArgs := kernel.Arguments(bytes.Clone(original))
	authorized, decision, err := hub.Authorize(t.Context(), "t", callerArgs, toolhub.Context{}, nil)
	if err != nil || !decision.Allowed {
		t.Fatalf("authorize: %+v %v", decision, err)
	}

	// The caller overwrites its slice after Authorize: the handler must still see the original.
	for i := range callerArgs {
		callerArgs[i] = 'Z'
	}
	if got := authorized.Arguments(); !bytes.Equal(got, original) {
		t.Fatalf("Authorized.Arguments = %s, want the original %s", got, original)
	}
	if _, err := authorized.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, original) {
		t.Fatalf("handler received %s, want %s", received, original)
	}
	if bytes.Contains(callerArgs, []byte("X")) {
		t.Fatalf("a handler overwriting its args changed the caller's slice: %s", callerArgs)
	}

	// The handler wrote into its own copy only: the authorized bytes are intact.
	if !bytes.Equal(authorized.Arguments(), original) {
		t.Fatalf("authorized bytes changed after Execute: %s", authorized.Arguments())
	}

	// Arguments returns a fresh clone each time.
	got := authorized.Arguments()
	got[0] = 'Q'
	if !bytes.Equal(authorized.Arguments(), original) {
		t.Fatal("Authorized.Arguments exposed internal state")
	}
}

func TestAuthorizedExecutePassesResultsThrough(t *testing.T) {
	wantErr := errors.New("handler failed")
	isErr := true
	handler := &stubTool{name: "t", exec: func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		return toolhub.Output{Content: "partial", IsError: &isErr}, wantErr
	}}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	authorized, _, err := hub.Authorize(t.Context(), "t", nil, toolhub.Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorized.Name() != "t" || authorized.Definition().Name != "t" {
		t.Fatalf("Authorized identity = %s / %s", authorized.Name(), authorized.Definition().Name)
	}
	out, err := authorized.Execute(t.Context())
	if !errors.Is(err, wantErr) || out.Content != "partial" || out.IsError == nil || !*out.IsError {
		t.Fatalf("Execute = %+v, %v; want the handler's output and error unchanged", out, err)
	}
}

func TestNilEmitIsReplacedWithANoOp(t *testing.T) {
	emitted := false
	handler := &stubTool{name: "t", exec: func(_ context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
		if tc.Emit == nil {
			t.Error("Emit must not be nil in the handler")
			return toolhub.Output{}, nil
		}
		tc.Emit("x", nil)
		emitted = true
		return toolhub.Output{Content: "ok"}, nil
	}}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	authorized, _, err := hub.Authorize(t.Context(), "t", nil, toolhub.Context{SessionID: "s"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorized.Execute(t.Context()); err != nil || !emitted {
		t.Fatalf("Execute = %v, emitted = %v", err, emitted)
	}
}

func TestZeroValueHubIsSafe(t *testing.T) {
	var hub toolhub.ToolHub
	if _, ok := hub.Get("x"); ok || len(hub.List()) != 0 {
		t.Fatal("a zero Hub must be empty")
	}
	if hub.Policy() == nil {
		t.Fatal("a zero Hub must still report a policy")
	}
	if _, _, err := hub.Authorize(t.Context(), "x", nil, toolhub.Context{}, nil); !errors.Is(err, toolhub.ErrNotFound) {
		t.Fatalf("Authorize on a zero Hub = %v", err)
	}
	if err := hub.Register(&stubTool{name: "x"}); err != nil {
		t.Fatalf("Register on a zero Hub: %v", err)
	}
}

func TestConcurrentAuthorizeAndSetPolicy(t *testing.T) {
	hub := toolhub.NewToolHub()
	handler := &stubTool{name: "t"}
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	allow := &spyPolicy{name: "policy:allow", decision: toolhub.Allow()}
	deny := &spyPolicy{name: "policy:deny", decision: toolhub.Deny("no")}

	const workers, iterations = 16, 200
	stop := make(chan struct{})
	var swapper sync.WaitGroup
	swapper.Add(1)
	go func() {
		defer swapper.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch i % 3 {
			case 0:
				hub.SetPolicy(allow)
			case 1:
				hub.SetPolicy(deny)
			default:
				hub.SetPolicy(nil)
			}
			_ = hub.Policy()
		}
	}()

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			args := kernel.Arguments(`{"a":1}`)
			for range iterations {
				authorized, decision, err := hub.Authorize(context.Background(), "t", args, toolhub.Context{}, nil)
				if err != nil {
					t.Errorf("authorize: %v", err)
					return
				}
				if decision.Allowed {
					// SetPolicy runs concurrently, so a stale authorization is a
					// legal outcome; anything else is a bug.
					if _, err := authorized.Execute(context.Background()); err != nil && !errors.Is(err, toolhub.ErrPolicyChanged) {
						t.Errorf("execute: %v", err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	swapper.Wait()
}

// executable reports a path through t (a type, the elements of a slice, array,
// map, pointer or channel, struct fields, and the results of interface methods
// and functions) that reaches a type with an Execute method, or "" when there is
// none. Types in allowed are the sanctioned exceptions and are not entered.
func executable(t reflect.Type, allowed map[reflect.Type]bool, seen map[reflect.Type]bool) string {
	if allowed[t] || seen[t] {
		return ""
	}
	seen[t] = true
	for _, ty := range []reflect.Type{t, reflect.PointerTo(t)} {
		if _, ok := ty.MethodByName("Execute"); ok {
			return ty.String() + " has an Execute method"
		}
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Pointer, reflect.Chan:
		return executable(t.Elem(), allowed, seen)
	case reflect.Map:
		if p := executable(t.Key(), allowed, seen); p != "" {
			return p
		}
		return executable(t.Elem(), allowed, seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if p := executable(t.Field(i).Type, allowed, seen); p != "" {
				return t.Field(i).Name + " -> " + p
			}
		}
	case reflect.Func:
		for i := range t.NumOut() {
			if p := executable(t.Out(i), allowed, seen); p != "" {
				return p
			}
		}
	case reflect.Interface:
		for i := range t.NumMethod() {
			if p := executable(t.Method(i).Type, allowed, seen); p != "" {
				return t.Method(i).Name + " -> " + p
			}
		}
	default:
	}
	return ""
}

// TestHubOffersNoDirectExecution proves by reflection that no exported method of
// *ToolHub returns, directly or nested in a slice, map, pointer or struct, a value
// with an Execute method, except the Authorized that Authorize mints. That is
// the choke point: a handler cannot be reached around the policy.
func TestHubOffersNoDirectExecution(t *testing.T) {
	hubType := reflect.TypeFor[*toolhub.ToolHub]()
	if hubType.NumMethod() == 0 {
		t.Fatal("reflection found no methods on *toolhub.ToolHub")
	}
	authorizedType := reflect.TypeFor[toolhub.Authorized]()
	allowed := map[reflect.Type]bool{authorizedType: true}
	for i := range hubType.NumMethod() {
		m := hubType.Method(i)
		for j := range m.Type.NumOut() {
			if p := executable(m.Type.Out(j), allowed, map[reflect.Type]bool{}); p != "" {
				t.Errorf("method %s returns something executable: %s", m.Name, p)
			}
			// Only Authorize may hand out the sanctioned Authorized.
			if m.Type.Out(j) == authorizedType && m.Name != "Authorize" {
				t.Errorf("method %s returns toolhub.Authorized, only Authorize may", m.Name)
			}
		}
	}
}

// TestExecutableDetectorIsNotVacuous proves the detector above flags the shapes
// the bypass of CR-01 had.
func TestExecutableDetectorIsNotVacuous(t *testing.T) {
	cases := map[string]reflect.Type{
		"handler":              reflect.TypeFor[toolhub.Handler](),
		"handler slice":        reflect.TypeFor[[]toolhub.Handler](),
		"lookup func":          reflect.TypeFor[func(string) (toolhub.Handler, bool)](),
		"map of handlers":      reflect.TypeFor[map[string]toolhub.Handler](),
		"struct with handler":  reflect.TypeFor[struct{ H []*stubTool }](),
		"interface returning":  reflect.TypeFor[interface{ Get() []toolhub.Handler }](),
		"concrete stub":        reflect.TypeFor[*stubTool](),
		"pointer to a handler": reflect.TypeFor[*toolhub.Handler](),
	}
	for name, ty := range cases {
		if executable(ty, nil, map[reflect.Type]bool{}) == "" {
			t.Errorf("%s: detector missed an executable type", name)
		}
	}
	for name, ty := range map[string]reflect.Type{
		"definition":       reflect.TypeFor[toolhub.Definition](),
		"definition slice": reflect.TypeFor[[]toolhub.Definition](),
		"output":           reflect.TypeFor[toolhub.Output](),
	} {
		if p := executable(ty, nil, map[reflect.Type]bool{}); p != "" {
			t.Errorf("%s: detector flagged a harmless type: %s", name, p)
		}
	}
}

// TestAuthorizedIsSingleUse pins WR-05: one authorization runs the handler once,
// on every copy of the value.
func TestAuthorizedIsSingleUse(t *testing.T) {
	handler := &stubTool{name: "t"}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	authorized, d, err := hub.Authorize(t.Context(), "t", kernel.Arguments(`{}`), toolhub.Context{}, nil)
	if err != nil || !d.Allowed {
		t.Fatalf("authorize: %+v %v", d, err)
	}
	copyOf := authorized
	if _, err := authorized.Execute(t.Context()); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	for name, a := range map[string]toolhub.Authorized{"same": authorized, "copy": copyOf} {
		_, err := a.Execute(t.Context())
		if !errors.Is(err, toolhub.ErrAlreadyExecuted) || !errors.Is(err, toolhub.ErrNotAuthorized) {
			t.Fatalf("%s: second Execute = %v, want ErrAlreadyExecuted (matching ErrNotAuthorized)", name, err)
		}
	}
	if handler.calls.Load() != 1 {
		t.Fatalf("handler ran %d times for one authorization", handler.calls.Load())
	}
}

// TestAuthorizedIsBoundToThePolicyGeneration pins WR-05: SetPolicy after
// Authorize makes the old authorization unusable, and a fresh one works.
func TestAuthorizedIsBoundToThePolicyGeneration(t *testing.T) {
	handler := &stubTool{name: "t"}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	authorized, d, err := hub.Authorize(t.Context(), "t", kernel.Arguments(`{}`), toolhub.Context{}, nil)
	if err != nil || !d.Allowed {
		t.Fatalf("authorize: %+v %v", d, err)
	}
	hub.SetPolicy(&spyPolicy{name: "policy:deny", decision: toolhub.Deny("no more")})
	_, err = authorized.Execute(t.Context())
	if !errors.Is(err, toolhub.ErrPolicyChanged) || !errors.Is(err, toolhub.ErrNotAuthorized) {
		t.Fatalf("Execute after SetPolicy = %v, want ErrPolicyChanged", err)
	}
	if handler.calls.Load() != 0 {
		t.Fatal("the handler ran under a replaced policy")
	}

	hub.SetPolicy(&spyPolicy{name: "policy:allow", decision: toolhub.Allow()})
	fresh, d, err := hub.Authorize(t.Context(), "t", kernel.Arguments(`{}`), toolhub.Context{}, nil)
	if err != nil || !d.Allowed {
		t.Fatalf("re-authorize: %+v %v", d, err)
	}
	if _, err := fresh.Execute(t.Context()); err != nil {
		t.Fatalf("a fresh authorization must run: %v", err)
	}
}

// TestPolicyAndExecuteSeeTheSameBytes pins WR-04: a caller that rewrites its
// slice while the policy is running changes neither the checked nor the executed
// bytes.
func TestPolicyAndExecuteSeeTheSameBytes(t *testing.T) {
	var executed kernel.Arguments
	handler := &stubTool{name: "t", exec: func(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
		executed = bytes.Clone(args)
		return toolhub.Output{Content: "ok"}, nil
	}}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	callerArgs := kernel.Arguments(`{"a":1}`)
	var checked kernel.Arguments
	hub.SetPolicy(&funcPolicy{check: func(args kernel.Arguments) toolhub.Decision {
		checked = bytes.Clone(args)
		for i := range callerArgs { // the caller scribbles while the policy runs
			callerArgs[i] = 'Z'
		}
		return toolhub.Allow()
	}})
	authorized, d, err := hub.Authorize(t.Context(), "t", callerArgs, toolhub.Context{}, nil)
	if err != nil || !d.Allowed {
		t.Fatalf("authorize: %+v %v", d, err)
	}
	if _, err := authorized.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(checked, executed) || string(executed) != `{"a":1}` {
		t.Fatalf("policy checked %s but the handler ran with %s", checked, executed)
	}
}

// funcPolicy is a test Policy that runs check on the arguments.
type funcPolicy struct {
	check func(args kernel.Arguments) toolhub.Decision
}

func (*funcPolicy) Name() string    { return "policy:func" }
func (*funcPolicy) Version() string { return "1.0.0" }

func (f *funcPolicy) Check(_ context.Context, _ string, args kernel.Arguments, _ toolhub.Context, _ toolhub.Definition, _ []string) toolhub.Decision {
	return f.check(args)
}
