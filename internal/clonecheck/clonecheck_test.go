package clonecheck_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/clonecheck"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// recorder is a testing.TB that records failures instead of failing the test.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

type inner struct {
	N int `json:"n"`
}

type record struct {
	Name string              `json:"name"`
	P    *inner              `json:"p"`
	List []string            `json:"list"`
	M    map[string]int      `json:"m"`
	Obj  ordered.Object      `json:"obj"`
	Ord  ordered.Map[string] `json:"ord"`
}

func sample() record {
	obj := ordered.NewObject()
	obj.Set("a", json.Number("1"))
	obj.Set("nested", func() ordered.Object {
		n := ordered.NewObject()
		n.Set("k", "v")
		return n
	}())
	var ord ordered.Map[string]
	ord.Set("x", "y")
	return record{Name: "n", P: &inner{N: 1}, List: []string{"a", "b"}, M: map[string]int{"k": 1}, Obj: obj, Ord: ord}
}

func deepClone(r record) record {
	out := r
	p := *r.P
	out.P = &p
	out.List = append([]string(nil), r.List...)
	out.M = make(map[string]int, len(r.M))
	for k, v := range r.M {
		out.M[k] = v
	}
	out.Obj = r.Obj.Clone()
	out.Ord = r.Ord.Clone()
	return out
}

func TestAssertDeepPassesDeepClone(t *testing.T) {
	r := &recorder{TB: t}
	clonecheck.AssertDeep(r, sample(), deepClone)
	if len(r.failures) != 0 {
		t.Fatalf("a deep clone was reported: %v", r.failures)
	}
	clonecheck.AssertDeep(t, sample(), deepClone) // and with the real TB
}

func TestAssertDeepCatchesShallowClone(t *testing.T) {
	shallow := map[string]func(record) record{
		"everything shared": func(r record) record { return r },
		"pointer shared":    func(r record) record { c := deepClone(r); c.P = r.P; return c },
		"slice shared":      func(r record) record { c := deepClone(r); c.List = r.List; return c },
		"map shared":        func(r record) record { c := deepClone(r); c.M = r.M; return c },
		"object shared":     func(r record) record { c := deepClone(r); c.Obj = r.Obj; return c },
		"ordered map shared": func(r record) record {
			c := deepClone(r)
			c.Ord = r.Ord // Map.Clone is shallow in V, but the value map is copied; sharing the field shares it
			return c
		},
	}
	for name, clone := range shallow {
		r := &recorder{TB: t}
		clonecheck.AssertDeep(r, sample(), clone)
		if len(r.failures) == 0 {
			t.Errorf("%s: a shallow clone was not reported", name)
		}
	}
}

func TestAssertDeepCatchesZeroField(t *testing.T) {
	for name, mutate := range map[string]func(*record){
		"nil pointer":   func(r *record) { r.P = nil },
		"empty string":  func(r *record) { r.Name = "" },
		"empty slice":   func(r *record) { r.List = nil },
		"empty map":     func(r *record) { r.M = map[string]int{} },
		"zero object":   func(r *record) { r.Obj = ordered.Object{} },
		"empty ordered": func(r *record) { r.Ord = ordered.Map[string]{} },
		"zero nested":   func(r *record) { r.P = &inner{} },
	} {
		s := sample()
		mutate(&s)
		r := &recorder{TB: t}
		clonecheck.AssertDeep(r, s, deepClone)
		if len(r.failures) == 0 {
			t.Errorf("%s: a zero field was not reported", name)
			continue
		}
		if !strings.Contains(r.failures[0], "fill it in the sample") {
			t.Errorf("%s: the report does not tell the author what to do: %q", name, r.failures[0])
		}
	}
	// The report names the field path.
	s := sample()
	s.P = &inner{}
	r := &recorder{TB: t}
	clonecheck.AssertDeep(r, s, deepClone)
	if len(r.failures) != 1 || !strings.Contains(r.failures[0], "record.P.N") {
		t.Fatalf("path not named: %v", r.failures)
	}
}
