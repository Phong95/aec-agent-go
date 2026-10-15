package ordered_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

func TestInsertionOrderAndOverwrite(t *testing.T) {
	m := ordered.New[int]()
	if m.Set("b", 1) {
		t.Fatal("first Set of b reported a replacement")
	}
	m.Set("a", 2)
	m.Set("c", 3)
	if !m.Set("a", 20) {
		t.Fatal("Set of an existing key must report a replacement")
	}
	if got := m.Keys(); !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Fatalf("keys = %v, want [b a c]: overwrite must keep the position", got)
	}
	if got := m.Values(); !slices.Equal(got, []int{1, 20, 3}) {
		t.Fatalf("values = %v", got)
	}
	if v, ok := m.Get("a"); !ok || v != 20 {
		t.Fatalf("Get(a) = %d, %v", v, ok)
	}
	if m.Has("zzz") || m.Len() != 3 {
		t.Fatalf("Has(zzz)=%v Len=%d", m.Has("zzz"), m.Len())
	}
}

func TestDeleteThenReAddAppends(t *testing.T) {
	m := ordered.New[int]()
	m.Set("a", 1)
	m.Set("b", 2)
	m.Set("c", 3)
	m.Delete("a")
	m.Delete("missing") // a no-op
	m.Set("a", 4)
	if got := m.Keys(); !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("keys = %v, want [b c a]", got)
	}
	if m.Len() != 3 {
		t.Fatalf("Len = %d", m.Len())
	}
}

func TestZeroValueIsUsable(t *testing.T) {
	var m ordered.Map[string]
	if m.Len() != 0 || m.Has("x") {
		t.Fatal("zero value must be empty")
	}
	if b, err := m.MarshalJSON(); err != nil || string(b) != "{}" {
		t.Fatalf("zero value marshals to %q, %v", b, err)
	}
	m.Set("x", "y")
	if v, _ := m.Get("x"); v != "y" {
		t.Fatalf("Get(x) = %q", v)
	}
}

func TestNilReceiverReads(t *testing.T) {
	var m *ordered.Map[int]
	if m.Len() != 0 || m.Has("a") || m.Keys() != nil || m.Values() != nil {
		t.Fatal("nil receiver reads must be empty")
	}
	m.Delete("a")
	for range m.All() {
		t.Fatal("nil receiver must yield nothing")
	}
	// MarshalJSON has a value receiver, so it cannot be called through a nil
	// pointer; encoding/json writes null for a nil *Map without calling it.
}

func TestKeysAndValuesAreCopies(t *testing.T) {
	m := ordered.New[int]()
	m.Set("a", 1)
	m.Set("b", 2)
	keys := m.Keys()
	keys[0] = "mutated"
	values := m.Values()
	values[0] = 99
	if got := m.Keys(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Keys exposed internal state: %v", got)
	}
	if v, _ := m.Get("a"); v != 1 {
		t.Fatalf("Values exposed internal state: %d", v)
	}
}

func TestAllIteratesInOrderAndStopsEarly(t *testing.T) {
	m := ordered.New[int]()
	for i, k := range []string{"z", "y", "x", "w"} {
		m.Set(k, i)
	}
	var seen []string
	for k := range m.All() {
		seen = append(seen, k)
	}
	if !slices.Equal(seen, []string{"z", "y", "x", "w"}) {
		t.Fatalf("order = %v", seen)
	}
	n := 0
	for range m.All() {
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("break was not honoured: %d", n)
	}
}

func TestMarshalKeepsOrderAndWritesRawCharacters(t *testing.T) {
	m := ordered.New[any]()
	m.Set("b", 1)
	m.Set("a", "<&>")
	m.Set("2", true)
	m.Set("1", nil)
	got, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"b":1,"a":"<&>","2":true,"1":null}`
	if string(got) != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}
}

func TestUnmarshalKeepsOrder(t *testing.T) {
	in := `{"b":1,"a":2,"2":3,"1":4}`
	var m ordered.Map[int]
	if err := m.UnmarshalJSON([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if got := m.Keys(); !slices.Equal(got, []string{"b", "a", "2", "1"}) {
		t.Fatalf("keys = %v", got)
	}
	out, err := m.MarshalJSON()
	if err != nil || string(out) != in {
		t.Fatalf("re-encode = %s, %v; want %s", out, err, in)
	}
}

func TestUnmarshalDuplicateKeyKeepsFirstPositionLastValue(t *testing.T) {
	var m ordered.Map[int]
	if err := m.UnmarshalJSON([]byte(`{"a":1,"b":2,"a":3}`)); err != nil {
		t.Fatal(err)
	}
	if got := m.Keys(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("keys = %v, want [a b]", got)
	}
	if v, _ := m.Get("a"); v != 3 {
		t.Fatalf("a = %d, want the last value 3", v)
	}
}

func TestUnmarshalRejectsNonObjects(t *testing.T) {
	for _, in := range []string{`[1]`, `"x"`, `1`, `true`, ``, `{`, `{"a":1} x`} {
		m := ordered.New[int]()
		m.Set("keep", 1)
		if err := m.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("UnmarshalJSON(%q) succeeded, want an error", in)
		}
		if m.Len() != 1 || !m.Has("keep") {
			t.Errorf("UnmarshalJSON(%q) changed the map on error: %v", in, m.Keys())
		}
	}
}

// JSON null is a no-op, as for Object: a record that carries "metadata": null
// must decode (WR-04). Before the fix Map rejected it while Object accepted it.
func TestUnmarshalNullLeavesTheMapUnchanged(t *testing.T) {
	m := ordered.New[int]()
	m.Set("keep", 1)
	for _, in := range []string{`null`, " null\n"} {
		if err := m.UnmarshalJSON([]byte(in)); err != nil {
			t.Fatalf("UnmarshalJSON(%q) = %v, want nil", in, err)
		}
		if m.Len() != 1 || !m.Has("keep") {
			t.Fatalf("UnmarshalJSON(%q) changed the map: %v", in, m.Keys())
		}
	}
	var holder struct {
		Metadata ordered.Map[string] `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(`{"metadata":null}`), &holder); err != nil {
		t.Fatalf("decode a struct with a null Map field: %v", err)
	}
	if holder.Metadata.Len() != 0 {
		t.Fatalf("a null Map field must stay empty, got %v", holder.Metadata.Keys())
	}
	if got, err := json.Marshal(holder); err != nil || string(got) != `{"metadata":{}}` {
		t.Fatalf("re-encode = %s, %v", got, err)
	}
}

func TestUnmarshalReplacesContents(t *testing.T) {
	m := ordered.New[int]()
	m.Set("old", 1)
	if err := m.UnmarshalJSON([]byte(`{"new":2}`)); err != nil {
		t.Fatal(err)
	}
	if m.Has("old") || m.Len() != 1 {
		t.Fatalf("old contents survived: %v", m.Keys())
	}
}

func TestUnmarshalDecodesNestedValuesStrictly(t *testing.T) {
	type item struct {
		A int `json:"a"`
	}
	var ok ordered.Map[item]
	if err := ok.UnmarshalJSON([]byte(`{"x":{"a":1}}`)); err != nil {
		t.Fatalf("a known field must decode: %v", err)
	}
	var bad ordered.Map[item]
	err := bad.UnmarshalJSON([]byte(`{"x":{"a":1,"extra":2}}`))
	if err == nil || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("an unknown field inside a value must fail, got %v", err)
	}
}
