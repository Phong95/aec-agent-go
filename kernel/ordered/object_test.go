package ordered_test

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

func decodeObject(t *testing.T, in string) ordered.Object {
	t.Helper()
	var o ordered.Object
	if err := o.UnmarshalJSON([]byte(in)); err != nil {
		t.Fatalf("UnmarshalJSON(%q): %v", in, err)
	}
	return o
}

func encodeObject(t *testing.T, o ordered.Object) string {
	t.Helper()
	b, err := o.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	return string(b)
}

func TestObjectInsertionOrderAndOverwrite(t *testing.T) {
	var o ordered.Object // the zero value is usable
	o.Set("b", 1)
	o.Set("a", 2)
	o.Set("c", 3)
	o.Set("a", 20) // an existing key keeps its position
	if got := o.Keys(); !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Fatalf("keys = %v, want [b a c]", got)
	}
	if v, ok := o.Get("a"); !ok || v != 20 {
		t.Fatalf("Get(a) = %v, %v", v, ok)
	}
	if o.Has("zzz") || !o.Has("b") || o.Len() != 3 {
		t.Fatalf("Has/Len wrong: %v %v %d", o.Has("zzz"), o.Has("b"), o.Len())
	}
	o.Delete("b")
	o.Delete("missing") // a no-op
	o.Set("b", 4)       // a re-added key goes to the end
	if got := o.Keys(); !slices.Equal(got, []string{"a", "c", "b"}) {
		t.Fatalf("keys after delete and re-add = %v", got)
	}
	keys := o.Keys()
	keys[0] = "mutated"
	if got := o.Keys(); got[0] != "a" {
		t.Fatal("Keys exposed internal state")
	}
	var seen []string
	for k := range o.All() {
		seen = append(seen, k)
		break
	}
	if !slices.Equal(seen, []string{"a"}) {
		t.Fatalf("All did not iterate in order and stop on break: %v", seen)
	}
}

// Pitfall 1: a pointer-receiver MarshalJSON on a by-value field encodes {}.
func TestObjectByValueInStruct(t *testing.T) {
	type holder struct {
		O ordered.Object `json:"o"`
		Z ordered.Object `json:"z,omitzero"`
		E ordered.Object `json:"e,omitzero"`
	}
	var h holder
	h.O.Set("b", 1)
	h.O.Set("a", "x")
	h.E = ordered.NewObject()
	got, err := jsonx.Marshal(h) // by value, not addressable
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"o":{"b":1,"a":"x"},"e":{}}`; string(got) != want {
		t.Fatalf("by value = %s, want %s (an absent object is dropped, an explicit {} kept)", got, want)
	}

	// Inside []any and map[string]any the entries must not become {}.
	var inner ordered.Object
	inner.Set("z", true)
	inner.Set("y", nil)
	got, err = jsonx.Marshal([]any{inner, map[string]any{"k": inner}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"z":true,"y":null},{"k":{"z":true,"y":null}}]`; string(got) != want {
		t.Fatalf("inside any = %s, want %s", got, want)
	}
}

func TestMapByValueInStruct(t *testing.T) {
	type holder struct {
		M ordered.Map[string] `json:"m"`
	}
	var h holder
	h.M.Set("b", "1")
	h.M.Set("a", "2")
	got, err := jsonx.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"m":{"b":"1","a":"2"}}`; string(got) != want {
		t.Fatalf("by value = %s, want %s", got, want)
	}
	got, err = jsonx.Marshal([]any{h.M})
	if err != nil || string(got) != `[{"b":"1","a":"2"}]` {
		t.Fatalf("inside []any = %s, %v", got, err)
	}
	got, err = jsonx.Marshal(&h.M) // through a pointer as before
	if err != nil || string(got) != `{"b":"1","a":"2"}` {
		t.Fatalf("through a pointer = %s, %v", got, err)
	}
}

func TestMapClone(t *testing.T) {
	m := ordered.New[int]()
	m.Set("b", 1)
	m.Set("a", 2)
	c := m.Clone()
	m.Set("c", 3)
	m.Set("b", 99)
	if got := c.Keys(); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("clone keys = %v", got)
	}
	if v, _ := c.Get("b"); v != 1 {
		t.Fatalf("clone shares entries with the original: %d", v)
	}
	var zero ordered.Map[int]
	if z := zero.Clone(); z.Len() != 0 {
		t.Fatal("clone of the zero map must be empty")
	}
}

func TestObjectDecodeKeepsNumberSpelling(t *testing.T) {
	in := `{"a":1.0,"b":100000000000000000001,"c":1e21,"d":-0,"e":1.50}`
	o := decodeObject(t, in)
	if got := encodeObject(t, o); got != in {
		t.Fatalf("re-encode = %s, want %s", got, in)
	}
	if v, _ := o.Get("b"); v != json.Number("100000000000000000001") {
		t.Fatalf("b = %#v, want a json.Number", v)
	}
}

func TestObjectDecodeKeepsNestedOrder(t *testing.T) {
	in := `{"zeta":1.5,"nested":{"b":[1,{"y":2,"x":null}],"a":"<&>"},"2":true,"1":false}`
	o := decodeObject(t, in)
	if got := encodeObject(t, o); got != in {
		t.Fatalf("re-encode = %s, want %s", got, in)
	}
	if got := o.Keys(); !slices.Equal(got, []string{"zeta", "nested", "2", "1"}) {
		t.Fatalf("keys = %v", got)
	}
	v, _ := o.Get("nested")
	n, ok := v.(ordered.Object)
	if !ok {
		t.Fatalf("nested is %T, want ordered.Object", v)
	}
	if got := n.Keys(); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("nested keys = %v", got)
	}
}

func TestObjectDuplicateKeyKeepsFirstPositionLastValue(t *testing.T) {
	o := decodeObject(t, `{"a":1,"b":2,"a":3}`)
	if got := o.Keys(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("keys = %v, want [a b]", got)
	}
	if v, _ := o.Get("a"); v != json.Number("3") {
		t.Fatalf("a = %#v, want the last value 3", v)
	}
}

func TestObjectDecodeRejectsBadInputAndKeepsReceiver(t *testing.T) {
	for _, in := range []string{`[1]`, `"x"`, `1`, `true`, ``, `{`, `{"a":1`, `{"a":}`, `{"a":1,}`, `{"a":1} x`, `{"a":1}{"b":2}`, `{"a":[1,}`, `{1:2}`, `{"a":tru}`} {
		o := ordered.NewObject()
		o.Set("keep", true)
		err := o.UnmarshalJSON([]byte(in))
		if err == nil {
			t.Errorf("UnmarshalJSON(%q) succeeded, want an error", in)
			continue
		}
		if !strings.HasPrefix(err.Error(), "ordered: ") {
			t.Errorf("UnmarshalJSON(%q) error %q lacks the ordered: prefix", in, err)
		}
		if o.Len() != 1 || !o.Has("keep") {
			t.Errorf("UnmarshalJSON(%q) changed the receiver on error: %v", in, o.Keys())
		}
	}
	o := ordered.NewObject()
	o.Set("keep", true)
	if err := o.UnmarshalJSON([]byte(` null `)); err != nil {
		t.Fatalf("null must not be an error: %v", err)
	}
	if !o.Has("keep") {
		t.Fatal("null changed the receiver")
	}
}

func TestObjectDecodeDepthCap(t *testing.T) {
	// The top-level object is depth 1; each array adds one.
	nest := func(depth int) string {
		return `{"a":` + strings.Repeat("[", depth-1) + `1` + strings.Repeat("]", depth-1) + `}`
	}
	var ok ordered.Object
	if err := ok.UnmarshalJSON([]byte(nest(ordered.MaxDepth))); err != nil {
		t.Fatalf("MaxDepth levels must decode: %v", err)
	}
	var bad ordered.Object
	if err := bad.UnmarshalJSON([]byte(nest(ordered.MaxDepth + 1))); err == nil {
		t.Fatal("MaxDepth+1 levels must fail")
	}
	mixed := strings.Repeat(`{"a":[`, (ordered.MaxDepth+1)/2+1) + strings.Repeat(`]}`, (ordered.MaxDepth+1)/2+1)
	var m ordered.Object
	if err := m.UnmarshalJSON([]byte(mixed)); err == nil {
		t.Fatal("mixed array and object nesting beyond MaxDepth must fail")
	}
	// Adversarial input far beyond any cap returns an error, never recurses unboundedly.
	var huge ordered.Object
	if err := huge.UnmarshalJSON([]byte(`{"a":` + strings.Repeat("[", 200000))); err == nil {
		t.Fatal("unterminated deep input must fail")
	}
}

func TestStrictEqual(t *testing.T) {
	obj := ordered.NewObject()
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"number across json.Number and int", json.Number("3"), 3, true},
		{"number across json.Number and float64", json.Number("3"), 3.0, true},
		{"number across int64", int64(3), json.Number("3.0"), true},
		{"number spellings", json.Number("1e2"), json.Number("100"), true},
		{"different numbers", json.Number("3"), 4, false},
		{"string and number", "3", 3, false},
		{"string and json.Number", "3", json.Number("3"), false},
		{"nil equals nil", nil, nil, true},
		{"nil and false", nil, false, false},
		{"nil and zero", nil, 0, false},
		{"bool", true, true, true},
		{"bool differs", true, false, false},
		{"string", "x", "x", true},
		{"string differs", "x", "y", false},
		{"two equal objects are different references", obj, ordered.NewObject(), false},
		{"an object equals nothing, not even itself", obj, obj, false},
		{"two arrays", []any{1}, []any{1}, false},
		{"a map and itself", map[string]any{}, map[string]any{}, false},
		{"NaN is not equal to itself", math.NaN(), math.NaN(), false},
		{"unparsable number", json.Number("abc"), json.Number("abc"), false},
	}
	for _, c := range cases {
		if got := ordered.StrictEqual(c.a, c.b); got != c.want {
			t.Errorf("%s: StrictEqual(%#v, %#v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

func TestObjectCloneIsDeep(t *testing.T) {
	inner := ordered.NewObject()
	inner.Set("k", json.Number("1"))
	arr := []any{"a", inner}
	m := map[string]any{"x": "y"}
	strs := []string{"p", "q"}
	var o ordered.Object
	o.Set("inner", inner)
	o.Set("arr", arr)
	o.Set("map", m)
	o.Set("strs", strs)
	before := encodeObject(t, o)
	c := o.Clone()

	// Mutate everything reachable from the original.
	inner.Set("added", true)
	arr[0] = "changed"
	if n, ok := arr[1].(ordered.Object); ok {
		n.Set("viaArr", true)
	}
	m["x"] = "changed"
	m["new"] = 1
	strs[0] = "changed"
	o.Set("top", true)

	if got := encodeObject(t, c); got != before {
		t.Fatalf("clone changed with the original:\n got: %s\nwant: %s", got, before)
	}
	if z := (ordered.Object{}).Clone(); !z.IsZero() {
		t.Fatal("clone of the zero Object must stay zero")
	}
}

func TestObjectZeroAndExplicitEmpty(t *testing.T) {
	var z ordered.Object
	if !z.IsZero() || encodeObject(t, z) != "{}" || z.Len() != 0 {
		t.Fatal("the zero Object must be IsZero, empty and encode as {}")
	}
	e := ordered.NewObject()
	if e.IsZero() || encodeObject(t, e) != "{}" {
		t.Fatal("NewObject must not be IsZero and must encode as {}")
	}
	if d := decodeObject(t, `{}`); d.IsZero() {
		t.Fatal("a decoded {} must not be IsZero")
	}
}

func TestObjectEncodesNonFiniteFloatsAsNull(t *testing.T) {
	var o ordered.Object
	o.Set("nan", math.NaN())
	o.Set("inf", math.Inf(1))
	if got := encodeObject(t, o); got != `{"nan":null,"inf":null}` {
		t.Fatalf("got %s", got)
	}
}

func fuzzSeeds(f *testing.F) {
	f.Helper()
	var fixture struct {
		Created struct {
			Metadata json.RawMessage `json:"metadata"`
		} `json:"created"`
	}
	if err := json.Unmarshal(wiretest.Load(f, "store-session.json"), &fixture); err != nil {
		f.Fatal(err)
	}
	f.Add(string(fixture.Created.Metadata))
	for _, s := range []string{
		``, `{}`, `null`, `[]`, `{"a":[[[[1]]]]}`, `{"a":1,"b":2,"a":3}`,
		`{"a":100000000000000000001,"b":1e21,"c":-0,"d":1.50}`,
		`{"a":" <&>","b":"\ud83d"}`, `{"":{"":[{},[],null]}}`,
		`{"a":{"b":{"c":{"d":[true,false,null,"x",1]}}}}`, `{"a":1} x`, `{"a":`,
	} {
		f.Add(s)
	}
}

// FuzzObjectDecode: arbitrary input never panics, and whatever decodes
// re-encodes to bytes that decode and re-encode to the same bytes.
func FuzzObjectDecode(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, in string) {
		var o ordered.Object
		if err := o.UnmarshalJSON([]byte(in)); err != nil {
			return
		}
		out, err := o.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON after a successful decode of %q: %v", in, err)
		}
		var again ordered.Object
		if err := again.UnmarshalJSON(out); err != nil {
			t.Fatalf("re-decode of %s: %v", out, err)
		}
		out2, err := again.MarshalJSON()
		if err != nil || !bytes.Equal(out, out2) {
			t.Fatalf("encoding is not stable:\n first: %s\nsecond: %s (%v)", out, out2, err)
		}
		c := o.Clone()
		if cb, err := c.MarshalJSON(); err != nil || !bytes.Equal(cb, out) {
			t.Fatalf("clone encodes differently: %s vs %s (%v)", cb, out, err)
		}
	})
}
