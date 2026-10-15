package skill

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// This file is in package skill (not skill_test) because parseYAMLSubset and
// splitFrontmatter are unexported.

type yamlCase struct {
	Name   string          `json:"name"`
	Source string          `json:"source"`
	Result json.RawMessage `json:"result"`
}

type yamlOk struct {
	OK    bool           `json:"ok"`
	Value ordered.Object `json:"value"`
}

type yamlFailed struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func yamlOutcomeBytes(t *testing.T, source string) []byte {
	t.Helper()
	value, err := parseYAMLSubset(source)
	var v any
	if err != nil {
		v = yamlFailed{OK: false, Message: err.Error()}
	} else {
		v = yamlOk{OK: true, Value: value}
	}
	b, encErr := kernel.EncodeJSON(v)
	if encErr != nil {
		t.Fatalf("encode outcome: %v", encErr)
	}
	return b
}

// TestYAMLCorpus requires the Go YAML-subset parser to return the same structure
// (JS key order included) or the same error text as the recorded golden,
// for every case of the golden corpus.
func TestYAMLCorpus(t *testing.T) {
	var cases []yamlCase
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "skill-yaml-corpus.json"), &cases); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(cases) < 40 {
		t.Fatalf("corpus has %d cases, want at least 40", len(cases))
	}
	var sawOK, sawErr bool
	for _, c := range cases {
		if strings.HasPrefix(string(c.Result), `{"ok":true`) {
			sawOK = true
		} else {
			sawErr = true
		}
		if got := yamlOutcomeBytes(t, c.Source); string(got) != string(c.Result) {
			t.Errorf("case %q differs:\n got %s\nwant %s", c.Name, got, c.Result)
		}
	}
	if !sawOK || !sawErr {
		t.Errorf("corpus must hold a success and a failure (ok=%v err=%v)", sawOK, sawErr)
	}
}

// nested builds a map nested depth levels deep: depth maps, the deepest with one
// scalar entry.
func nested(depth int) string {
	var b strings.Builder
	for i := range depth - 1 {
		b.WriteString(strings.Repeat(" ", i) + "k:\n")
	}
	b.WriteString(strings.Repeat(" ", depth-1) + "leaf: end")
	return b.String()
}

// TestYAMLDepthCap checks the intentional nesting limit: 100 nested maps
// parse, 101 are an error with a line number, and hostile nesting returns an error
// instead of overflowing the stack.
func TestYAMLDepthCap(t *testing.T) {
	if _, err := parseYAMLSubset(nested(maxYAMLDepth)); err != nil {
		t.Fatalf("%d levels must parse: %v", maxYAMLDepth, err)
	}
	_, err := parseYAMLSubset(nested(maxYAMLDepth + 1))
	if err == nil || !strings.Contains(err.Error(), "nesting is deeper than 100 levels") {
		t.Fatalf("101 levels: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "line 101: ") {
		t.Errorf("the error must name the line that goes too deep: %v", err)
	}
	if _, err := parseYAMLSubset(nested(5000)); err == nil {
		t.Error("5000 levels must be rejected")
	}

	// Through ParsePackage the error is an invalid skill with the "frontmatter: " prefix.
	_, err = ParsePackage(Package{Content: "---\n" + nested(150) + "\n---\nbody"})
	var se *kernel.SkillError
	if !errors.As(err, &se) || !strings.Contains(se.Message, "frontmatter: line ") {
		t.Fatalf("ParsePackage deep: %v", err)
	}
	if se.Code != kernel.SkillCodeInvalid {
		t.Errorf("code = %q", se.Code)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name, in    string
		front, body string
		ok          bool
	}{
		{name: "plain", in: "---\na: b\n---\nbody", front: "a: b", body: "body", ok: true},
		{name: "closing at end", in: "---\na: b\n---", front: "a: b", body: "", ok: true},
		{name: "empty frontmatter needs a blank line", in: "---\n\n---\nbody", front: "", body: "body", ok: true},
		{name: "adjacent delimiters do not split", in: "---\n---\nbody", ok: false},
		{name: "trailing spaces and tabs", in: "--- \t\na\n---\t \nb", front: "a", body: "b", ok: true},
		{name: "bom is stripped once", in: "\U0000feff---\na\n---\nb", front: "a", body: "b", ok: true},
		{name: "two boms", in: "\U0000feff\U0000feff---\na\n---\nb", ok: false},
		{name: "text after closing dashes", in: "---\na\n---x\nb\n---\nc", front: "a\n---x\nb", body: "c", ok: true},
		{name: "four dashes do not close", in: "---\na\n----\nb\n---\n", front: "a\n----\nb", body: "", ok: true},
		{name: "no closing", in: "---\na\n", ok: false},
		{name: "no newline after opening", in: "---", ok: false},
		{name: "leading space", in: " ---\na\n---\n", ok: false},
		{name: "carriage return only", in: "---\ra\r---\r", ok: false},
	}
	for _, tc := range tests {
		front, body, ok := splitFrontmatter(tc.in)
		if ok != tc.ok || front != tc.front || body != tc.body {
			t.Errorf("%s: splitFrontmatter(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.name, tc.in, front, body, ok, tc.front, tc.body, tc.ok)
		}
	}
}

func TestJSKeyOrder(t *testing.T) {
	for _, k := range []string{"0", "1", "42", "4294967294"} {
		if !isArrayIndex(k) {
			t.Errorf("%q is an array index", k)
		}
	}
	for _, k := range []string{"", "01", "-1", "1.5", "4294967295", "99999999999", "a", "1e3", "\U0000ff11"} {
		if isArrayIndex(k) {
			t.Errorf("%q is not an array index", k)
		}
	}
	got := jsKeyOrder([]string{"b", "10", "a", "2", "0", "9"})
	want := []string{"0", "2", "9", "10", "b", "a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("jsKeyOrder = %v, want %v", got, want)
	}
}

// TestProtoIsHiddenFromTheResult covers the __proto__ key on the YAML level: a __proto__
// key never appears among the keys, whatever its value, and a later one replaces
// an earlier one.
func TestProtoIsHiddenFromTheResult(t *testing.T) {
	m, err := parseYAMLDoc("__proto__:\n  name: foo\na: b\n__proto__: text\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if keys := m.own.Keys(); len(keys) != 1 || keys[0] != "a" {
		t.Errorf("own keys = %v", keys)
	}
	if v, ok := m.lookup("name"); !ok || v != "foo" {
		t.Errorf("name must read through the prototype, got %v %v", v, ok)
	}
	if _, ok := m.lookup("missing"); ok {
		t.Error("a missing key stays missing")
	}
	m, err = parseYAMLDoc("__proto__:\n  name: foo\n__proto__:\n  - x\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := m.lookup("name"); ok {
		t.Error("a list prototype supplies nothing")
	}
}

// --- the parseYamlSubset tests ------------------------------------------------------

func mustEncode(t *testing.T, v any) string {
	t.Helper()
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

func TestSkillFormatParseYamlSubsetReadsPlainQuotedFoldedAndLiteralScalars(t *testing.T) {
	yaml := strings.Join([]string{
		"name: rfi-log",
		`quoted: "a: b # not a comment"`,
		"single: 'it''s'",
		"plain: value # trailing comment",
		"folded: >",
		"  first line",
		"  second line",
		"",
		"  new paragraph",
		"literal: |",
		"  line 1",
		"  line 2",
		"wrapped: starts here",
		"  and continues",
	}, "\n")
	got, err := parseYAMLSubset(yaml)
	if err != nil {
		t.Fatalf("parseYAMLSubset: %v", err)
	}
	want := `{"name":"rfi-log","quoted":"a: b # not a comment","single":"it's","plain":"value","folded":"first line second line\nnew paragraph","literal":"line 1\nline 2","wrapped":"starts here and continues"}`
	if enc := mustEncode(t, got); enc != want {
		t.Errorf("got  %s\nwant %s", enc, want)
	}
}

func TestSkillFormatParseYamlSubsetReadsFlowListsDashListsAndNestedMaps(t *testing.T) {
	yaml := strings.Join([]string{
		`tags: [a, 'b c', "d"]`,
		"tools:",
		"  - read_file",
		"  - terminal",
		"metadata:",
		"  requires-tools: web_search",
		"  hermes:",
		"    category: research",
	}, "\n")
	got, err := parseYAMLSubset(yaml)
	if err != nil {
		t.Fatalf("parseYAMLSubset: %v", err)
	}
	want := `{"tags":["a","b c","d"],"tools":["read_file","terminal"],"metadata":{"requires-tools":"web_search","hermes":{"category":"research"}}}`
	if enc := mustEncode(t, got); enc != want {
		t.Errorf("got  %s\nwant %s", enc, want)
	}
}

func TestSkillFormatParseYamlSubsetRejectsStructuresItCannotRead(t *testing.T) {
	for source, want := range map[string]string{
		"a: {x: 1}":       "inline maps",
		"just text":       `expected "key: value"`,
		"list:\n  - a: 1": "lists of maps",
	} {
		if _, err := parseYAMLSubset(source); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseYAMLSubset(%q) error = %v, want one containing %q", source, err, want)
		}
	}
}
