package aec_test

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// fakeKernel records lifecycle calls. It used to satisfy the root Kernel interface; that
// interface is gone (AgentCore is the API), so the fake keeps only the lifecycle methods
// the kernel plugin helpers call.
type fakeKernel struct {
	calls []string
	err   error
}

func (k *fakeKernel) Initialize(context.Context) error {
	k.calls = append(k.calls, "initialize")
	return k.err
}

func (k *fakeKernel) Dispose(context.Context) error {
	k.calls = append(k.calls, "dispose")
	return k.err
}

func TestKernelLifecycleThroughPluginHelpers(t *testing.T) {
	ctx := context.Background()
	k := &fakeKernel{}

	if err := kernel.Initialize(ctx, k); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := kernel.Dispose(ctx, k); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if len(k.calls) != 2 || k.calls[0] != "initialize" || k.calls[1] != "dispose" {
		t.Fatalf("lifecycle calls = %v, want [initialize dispose]", k.calls)
	}

	sentinel := errors.New("boom")
	failing := &fakeKernel{err: sentinel}
	if err := kernel.Initialize(ctx, failing); !errors.Is(err, sentinel) {
		t.Fatalf("Initialize error = %v, want %v", err, sentinel)
	}
}

// AgentCore carries the Kernel lifecycle, so the kernel plugin helpers drive it.
var (
	_ kernel.Initializer = (*aec.AgentCore)(nil)
	_ kernel.Disposer    = (*aec.AgentCore)(nil)
)

// executableThrough reports a path through t (slice, array, map, pointer and
// channel elements, exported struct fields, function results and interface-method
// results) to a type that has an Execute method, or "" when there is none. An
// unexported struct field is not walked: a caller cannot read it, so it hands
// nothing out (an *agent.Agent, the result of SpawnAgent, holds the tool hub in
// private fields, and the hub keeps the handlers private on purpose). A
// function type also hands values out through its parameters: an iterator
// (iter.Seq[toolhub.Handler] is func(yield func(toolhub.Handler) bool)) or a callback
// returns nothing, it passes each value to the callback the caller supplies. The
// AgentCore method's own parameters are never passed in (only its results are), so
// walking the parameters of a function type reached through a result cannot flag
// a method that merely accepts a handler.
func executableThrough(t reflect.Type, seen map[reflect.Type]bool) string {
	if seen[t] {
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
		return executableThrough(t.Elem(), seen)
	case reflect.Map:
		if p := executableThrough(t.Key(), seen); p != "" {
			return p
		}
		return executableThrough(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if !t.Field(i).IsExported() {
				continue
			}
			if p := executableThrough(t.Field(i).Type, seen); p != "" {
				return t.Field(i).Name + " -> " + p
			}
		}
	case reflect.Func:
		for i := range t.NumOut() {
			if p := executableThrough(t.Out(i), seen); p != "" {
				return p
			}
		}
		for i := range t.NumIn() { // callbacks and iterator yield funcs hand values to the caller
			if p := executableThrough(t.In(i), seen); p != "" {
				return p
			}
		}
	case reflect.Interface:
		// A method of a returned interface is a callable the caller holds: its
		// results are handed out, and so are the parameters of a callback it takes
		// (func-typed parameters), but a plain parameter is an input and is not.
		for i := range t.NumMethod() {
			m := t.Method(i).Type
			for j := range m.NumOut() {
				if p := executableThrough(m.Out(j), seen); p != "" {
					return t.Method(i).Name + " -> " + p
				}
			}
			for j := range m.NumIn() {
				if m.In(j).Kind() != reflect.Func {
					continue
				}
				if p := executableThrough(m.In(j), seen); p != "" {
					return t.Method(i).Name + " -> " + p
				}
			}
		}
	default:
	}
	return ""
}

// kernelMethods are the AgentCore methods that stand for the Kernel interface (the
// root Kernel interface that listed them is gone; AgentCore is the API). The hub
// accessors (ToolHub, SkillHub, ...) are not in the list: they hand out the hub itself,
// whose own API is the policy gate.
var kernelMethods = []string{
	"Initialize", "Dispose", "UseLLMProvider", "UseTool",
	"UsePersistence", "UseSkill", "UseSandboxProvider", "SpawnAgent",
}

// TestKernelOffersNoDirectExecution pins that the public facade hands out no
// executable tool: no Kernel method of AgentCore returns, directly or nested, a type
// with an Execute method (the tool policy gate cannot be bypassed through AgentCore).
func TestKernelOffersNoDirectExecution(t *testing.T) {
	kt := reflect.TypeFor[*aec.AgentCore]()
	for _, name := range kernelMethods {
		m, ok := kt.MethodByName(name)
		if !ok {
			t.Errorf("AgentCore has no method %s", name)
			continue
		}
		// m.Type of a method of a concrete type has the receiver as its first parameter;
		// only the results are handed out.
		for j := range m.Type.NumOut() {
			if p := executableThrough(m.Type.Out(j), map[reflect.Type]bool{}); p != "" {
				t.Errorf("AgentCore.%s returns something executable: %s", m.Name, p)
			}
		}
	}
	if executableThrough(reflect.TypeFor[[]toolhub.Handler](), map[reflect.Type]bool{}) == "" {
		t.Fatal("the detector must flag a slice of toolhub.Handler")
	}
	// A function type hands a value out through its parameters too: an iterator, a
	// callback, and a func returned from a method.
	for name, typ := range map[string]reflect.Type{
		"iter.Seq[toolhub.Handler]":          reflect.TypeFor[iter.Seq[toolhub.Handler]](),
		"iter.Seq2[string, toolhub.Handler]": reflect.TypeFor[iter.Seq2[string, toolhub.Handler]](),
		"func(func(toolhub.Handler))":        reflect.TypeFor[func(func(toolhub.Handler))](),
		"func() iter.Seq[toolhub.Handler]":   reflect.TypeFor[func() iter.Seq[toolhub.Handler]](),
		"interface with an iterator method": reflect.TypeFor[interface {
			Each() iter.Seq[toolhub.Handler]
		}](),
		"interface with a callback parameter": reflect.TypeFor[interface{ Each(func(toolhub.Handler)) }](),
	} {
		if executableThrough(typ, map[reflect.Type]bool{}) == "" {
			t.Errorf("the detector must flag %s", name)
		}
	}
	// An exported field hands a value out, an unexported one does not.
	type exportedHandler struct{ Handler toolhub.Handler }
	type privateHandler struct{ handler toolhub.Handler }
	_ = privateHandler{handler: nil} // the field is only read through reflect
	if executableThrough(reflect.TypeFor[exportedHandler](), map[reflect.Type]bool{}) == "" {
		t.Error("the detector must flag a struct with an exported toolhub.Handler field")
	}
	if p := executableThrough(reflect.TypeFor[privateHandler](), map[reflect.Type]bool{}); p != "" {
		t.Errorf("the detector flagged an unexported field: %s", p)
	}
	// A plain parameter is an input, not a hand-out, and an iterator of data is fine.
	for name, typ := range map[string]reflect.Type{
		"interface that accepts a handler": reflect.TypeFor[interface{ Register(toolhub.Handler) }](),
		"iter.Seq[string]":                 reflect.TypeFor[iter.Seq[string]](),
		"iter.Seq[toolhub.Definition]":     reflect.TypeFor[iter.Seq[toolhub.Definition]](),
	} {
		if p := executableThrough(typ, map[reflect.Type]bool{}); p != "" {
			t.Errorf("the detector flagged %s: %s", name, p)
		}
	}
}

// TestHandleChannelWebhookNotFound: a webhook for a channel no running agent owns is a 404
// (A-100 phase 3); the full path is covered in components/agent channels_webhook_test.go.
func TestHandleChannelWebhookNotFound(t *testing.T) {
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Dispose(context.Background()) })
	res := core.HandleChannelWebhook(context.Background(), "missing", channel.WebhookRequest{Method: "POST", Body: "{}"})
	if res.Status != 404 {
		t.Fatalf("status = %d, want 404", res.Status)
	}
}
