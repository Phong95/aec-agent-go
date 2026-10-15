package skill_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// corpusCase is one case of a parser corpus: the package given to the parser and
// its recorded outcome (the golden), kept as raw bytes.
type corpusCase struct {
	Name   string          `json:"name"`
	Pkg    skill.Package   `json:"pkg"`
	Result json.RawMessage `json:"result"`
}

// parseOk and parseFailed are the two outcome shapes
// wiregen records: { ok: true, ...parsed } and { ok: false, code, skillName,
// message }.
type parseOk struct {
	OK            bool              `json:"ok"`
	Frontmatter   skill.Frontmatter `json:"frontmatter"`
	Body          string            `json:"body"`
	Files         []skill.File      `json:"files"`
	RequiresTools []string          `json:"requiresTools"`
}

type parseFailed struct {
	OK        bool   `json:"ok"`
	Code      string `json:"code"`
	SkillName string `json:"skillName"`
	Message   string `json:"message"`
}

// outcomeBytes parses pkg with the Go parser and encodes the outcome the way
// wiregen encodes the recorded one.
func outcomeBytes(t *testing.T, pkg skill.Package) []byte {
	t.Helper()
	parsed, err := skill.ParsePackage(pkg)
	var v any
	if err != nil {
		var se *kernel.SkillError
		if !errors.As(err, &se) {
			t.Fatalf("ParsePackage returned %T, want *kernel.SkillError: %v", err, err)
		}
		v = parseFailed{OK: false, Code: string(se.Code), SkillName: se.SkillName, Message: se.Message}
	} else {
		v = parseOk{OK: true, Frontmatter: parsed.Frontmatter, Body: parsed.Body, Files: parsed.Files, RequiresTools: parsed.RequiresTools}
	}
	b, encErr := kernel.EncodeJSON(v)
	if encErr != nil {
		t.Fatalf("encode outcome: %v", encErr)
	}
	return b
}

// loadCorpus reads a corpus fixture strictly.
func loadCorpus(t testing.TB, name string) []corpusCase {
	t.Helper()
	var cases []corpusCase
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, name), &cases); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return cases
}

// runParseCorpus requires the Go parser to reproduce every golden outcome byte for
// byte, and reports the name of each case that differs.
func runParseCorpus(t *testing.T, file string, minCases int) {
	t.Helper()
	cases := loadCorpus(t, file)
	if len(cases) < minCases {
		t.Fatalf("%s has %d cases, want at least %d", file, len(cases), minCases)
	}
	var sawOK, sawErr bool
	for _, c := range cases {
		got := outcomeBytes(t, c.Pkg)
		if strings.HasPrefix(string(c.Result), `{"ok":true`) {
			sawOK = true
		} else {
			sawErr = true
		}
		if string(got) != string(c.Result) {
			t.Errorf("case %q differs:\n got %s\nwant %s", c.Name, got, c.Result)
		}
	}
	if !sawOK || !sawErr {
		t.Errorf("%s must hold at least one success and one failure (ok=%v err=%v)", file, sawOK, sawErr)
	}
}

func TestParseCorpus(t *testing.T) {
	runParseCorpus(t, "skill-parse-corpus.json", 20)
}

// TestParseReturnsSkillError pins the error contract that does not depend on a
// corpus: the error is a *kernel.SkillError, code invalid, never retryable, and
// carries the skill name once the frontmatter named one.
func TestParseReturnsSkillError(t *testing.T) {
	_, err := skill.ParsePackage(skill.Package{Content: "no frontmatter"})
	var se *kernel.SkillError
	if !errors.As(err, &se) || se.Code != kernel.SkillCodeInvalid || se.SkillName != "" || se.Retryable {
		t.Fatalf("no frontmatter: %#v", err)
	}
	if !errors.Is(err, &kernel.SkillError{Code: kernel.SkillCodeInvalid}) {
		t.Error("errors.Is must match the invalid code")
	}
	_, err = skill.ParsePackage(skill.Package{Content: "---\nname: Bad Name\ndescription: d\n---\nbody"})
	if !errors.As(err, &se) || se.SkillName != "Bad Name" {
		t.Fatalf("bad name: %#v", err)
	}
	if !strings.HasPrefix(se.Message, "Invalid skill \"Bad Name\":\n- ") {
		t.Errorf("message = %q", se.Message)
	}
}

// TestParseResultShape pins the success result without a corpus: required
// collections are never nil and the optional frontmatter fields are omitted when
// empty.
func TestParseResultShape(t *testing.T) {
	parsed, err := skill.ParsePackage(skill.Package{Content: "---\nname: demo\ndescription: d\n---\nbody"})
	if err != nil {
		t.Fatalf("ParsePackage: %v", err)
	}
	if parsed.Files == nil || parsed.RequiresTools == nil {
		t.Error("Files and RequiresTools must not be nil")
	}
	if parsed.Frontmatter.License != "" || parsed.Frontmatter.Compatibility != "" || parsed.Frontmatter.AllowedTools != nil {
		t.Errorf("optional fields set: %+v", parsed.Frontmatter)
	}
	if parsed.Frontmatter.Metadata.Len() != 0 || parsed.Body != "body" {
		t.Errorf("metadata or body: %+v %q", parsed.Frontmatter.Metadata, parsed.Body)
	}
}

func TestNormalizeSkillPath(t *testing.T) {
	bs := string(rune(92))
	good := map[string]string{
		"./references/a.md": "references/a.md",
		"scripts/run.py":    "scripts/run.py",
		"./a/b":             "a/b",
		"a/ b":              "a/ b",
		"  a.md  ":          "a.md",
	}
	for in, want := range good {
		if got, ok := skill.NormalizeSkillPath(in); !ok || got != want {
			t.Errorf("NormalizeSkillPath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{
		"", "/etc/passwd", "../x", "a/../b", "a//b", "a" + bs + "b", "C:/x", "././a", "./", ".", "a/./b", "a/b/",
		"C:", "a:b", "." + bs + "a", " a /", "a\x00b", strings.Repeat("a", skill.MaxPathLength+1),
	} {
		if got, ok := skill.NormalizeSkillPath(bad); ok {
			t.Errorf("NormalizeSkillPath(%q) = %q, want a rejection", bad, got)
		}
	}
	if _, ok := skill.NormalizeSkillPath(strings.Repeat("a", skill.MaxPathLength)); !ok {
		t.Error("a path of exactly MaxPathLength is accepted")
	}
}

func TestMetadataKeepsParsedOrder(t *testing.T) {
	parsed, err := skill.ParsePackage(skill.Package{Content: "---\nname: demo\ndescription: d\nmetadata:\n  zeta: 1\n  alpha: 2\n  10: ten\n  2: two\n---\nbody"})
	if err != nil {
		t.Fatalf("ParsePackage: %v", err)
	}
	// JS object order: integer-like keys first, ascending, then insertion order.
	if got := parsed.Frontmatter.Metadata.Keys(); !slices.Equal(got, []string{"2", "10", "zeta", "alpha"}) {
		t.Errorf("metadata keys = %v", got)
	}
}

func TestMutationCorpus(t *testing.T) {
	runParseCorpus(t, "skill-mutation-corpus.json", 300)
	if n := len(loadCorpus(t, "skill-mutation-corpus.json")); n > 500 {
		t.Errorf("mutation corpus has %d cases, want at most 500", n)
	}
}

// TestSizeLimitsAtBoundaries covers the byte and count limits that are too large to
// keep a success case of in a fixture: the golden corpus pins the failure text, this
// pins that the limit itself is inclusive.
func TestSizeLimitsAtBoundaries(t *testing.T) {
	sized := func(target int) string {
		head, tail := "---\nname: demo\ndescription: d\n---\n\n", "\n"
		return head + strings.Repeat("x", target-len(head)-len(tail)) + tail
	}
	if _, err := skill.ParsePackage(skill.Package{Content: sized(skill.MaxContentBytes)}); err != nil {
		t.Errorf("content of exactly %d bytes: %v", skill.MaxContentBytes, err)
	}
	if _, err := skill.ParsePackage(skill.Package{Content: sized(skill.MaxContentBytes + 1)}); err == nil {
		t.Errorf("content of %d bytes must fail", skill.MaxContentBytes+1)
	}
	// The same size in a multi-byte alphabet: the limit counts bytes, not characters,
	// so 33334 three-byte characters (33334 runes, 100002 bytes) must fail.
	euro := strings.Repeat("\U000020ac", skill.MaxContentBytes/3+1)
	if _, err := skill.ParsePackage(skill.Package{Content: "---\nname: demo\ndescription: d\n---\n\n" + euro + "\n"}); err == nil {
		t.Error("more than MaxContentBytes bytes of 3-byte characters must fail")
	}

	five := make([]skill.File, 0, 6)
	for i := range 5 {
		five = append(five, skill.File{Path: "references/part-" + string(rune('a'+i)) + ".md", Content: strings.Repeat("x", skill.MaxFileBytes)})
	}
	pkg := skill.Package{Content: sized(80), Files: five}
	if _, err := skill.ParsePackage(pkg); err != nil {
		t.Errorf("files totalling exactly %d bytes: %v", skill.MaxTotalBytes, err)
	}
	pkg.Files = append(five, skill.File{Path: "references/one.md", Content: "y"})
	_, err := skill.ParsePackage(pkg)
	var se *kernel.SkillError
	if !errors.As(err, &se) || !strings.Contains(se.Message, "- the skill's files total 1000001 bytes; the limit is 1000000") {
		t.Errorf("total over the limit: %v", err)
	}
}

// --- the skill-format tests -------------------------------------------------------
// The essential-skills test is at the end of this file (plan 06-05). parseYamlSubset is unexported, so its three tests
// live in yaml_test.go (package skill).

func skillMdText(frontmatter string, body string) string {
	return "---\n" + frontmatter + "\n---\n\n" + body + "\n"
}

const defaultBody = "# Title\n\nDo the thing."

// problemsOf returns the message of the invalid SkillError ParsePackage returns.
func problemsOf(t *testing.T, pkg skill.Package) string {
	t.Helper()
	_, err := skill.ParsePackage(pkg)
	var se *kernel.SkillError
	if !errors.As(err, &se) {
		t.Fatalf("expected a SkillError, got %v", err)
	}
	if se.Code != kernel.SkillCodeInvalid {
		t.Fatalf("code = %q, want invalid", se.Code)
	}
	return se.Message
}

func TestSkillFormatParsesAValidSkillWithMetadataAllowedToolsAndFiles(t *testing.T) {
	parsed, err := skill.ParsePackage(skill.Package{
		Content: skillMdText(strings.Join([]string{
			"name: quantity-takeoff",
			"description: >",
			"  Compute quantities from a model export.",
			"  Use when the user asks for a takeoff.",
			"license: MIT",
			"allowed-tools: read_file terminal",
			"metadata:",
			"  requires-tools: read_file, terminal",
			"  author: team",
		}, "\n"), defaultBody),
		Files: []skill.File{{Path: "./scripts/takeoff.py", Content: "print(1)"}},
	})
	if err != nil {
		t.Fatalf("ParsePackage: %v", err)
	}
	if got := encode(t, parsed.Frontmatter); got != `{"name":"quantity-takeoff","description":"Compute quantities from a model export. Use when the user asks for a takeoff.","license":"MIT","allowedTools":["read_file","terminal"],"metadata":{"requires-tools":"read_file, terminal","author":"team"}}` {
		t.Errorf("frontmatter = %s", got)
	}
	if parsed.Body != "# Title\n\nDo the thing." {
		t.Errorf("body = %q", parsed.Body)
	}
	if got := encode(t, parsed.Files); got != `[{"path":"scripts/takeoff.py","content":"print(1)"}]` {
		t.Errorf("files = %s", got)
	}
	if !slices.Equal(parsed.RequiresTools, []string{"read_file", "terminal"}) {
		t.Errorf("requiresTools = %v", parsed.RequiresTools)
	}
}

func TestSkillFormatAcceptsHermesStyleNestedRequirements(t *testing.T) {
	parsed, err := skill.ParsePackage(skill.Package{
		Content: skillMdText("name: arxiv\ndescription: Search papers.\nmetadata:\n  hermes:\n    requires_tools: [web_search, web_extract]", defaultBody),
	})
	if err != nil {
		t.Fatalf("ParsePackage: %v", err)
	}
	if !slices.Equal(parsed.RequiresTools, []string{"web_search", "web_extract"}) {
		t.Errorf("requiresTools = %v", parsed.RequiresTools)
	}
	if v, _ := parsed.Frontmatter.Metadata.Get("hermes.requires_tools"); v != "web_search, web_extract" {
		t.Errorf("hermes.requires_tools = %q", v)
	}
}

func TestSkillFormatRequiresFrontmatter(t *testing.T) {
	if msg := problemsOf(t, skill.Package{Content: "# no frontmatter"}); !strings.Contains(msg, "must start with YAML frontmatter") {
		t.Errorf("message = %q", msg)
	}
}

func TestSkillFormatListsEveryProblemAtOnce(t *testing.T) {
	msg := problemsOf(t, skill.Package{
		Content: skillMdText("name: Bad_Name\ndescription: \"\"", ""),
		Files: []skill.File{
			{Path: "../escape.sh", Content: "x"},
			{Path: "/abs.md", Content: "x"},
			{Path: "SKILL.md", Content: "x"},
			{Path: "a.md", Content: "x"},
			{Path: "a.md", Content: "y"},
		},
	})
	for _, want := range []string{
		`"name" must be lowercase letters`,
		`"description" is required`,
		"no instructions after the frontmatter",
		`"../escape.sh" must be a relative path`,
		`"/abs.md" must be a relative path`,
		`"SKILL.md" is the skill content itself`,
		`"a.md" is listed twice`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

func TestSkillFormatEnforcesTheSpecsLengthLimits(t *testing.T) {
	cases := []struct{ name, frontmatter, want string }{
		{"long name", "name: " + strings.Repeat("a", 65) + "\ndescription: x", "longer than 64"},
		{"long description", "name: ok\ndescription: " + strings.Repeat("d", 1025), "limit is 1024"},
		{"hyphens", "name: -bad-\ndescription: x", "single hyphens"},
	}
	for _, tc := range cases {
		if msg := problemsOf(t, skill.Package{Content: skillMdText(tc.frontmatter, defaultBody)}); !strings.Contains(msg, tc.want) {
			t.Errorf("%s: message lacks %q:\n%s", tc.name, tc.want, msg)
		}
	}
}

func TestSkillFormatRejectsCredentialsInTheContentOrFiles(t *testing.T) {
	msg := problemsOf(t, skill.Package{Content: skillMdText("name: leak\ndescription: x", "Use key AKIAABCDEFGHIJKLMNOP")})
	if !strings.Contains(msg, "AWS access key") {
		t.Errorf("content secret: %s", msg)
	}
	msg = problemsOf(t, skill.Package{
		Content: skillMdText("name: leak\ndescription: x", defaultBody),
		Files:   []skill.File{{Path: "scripts/a.sh", Content: "export KEY=sk-ant-abcdefghijklmnopqrstuvwxyz"}},
	})
	if !strings.Contains(msg, "scripts/a.sh appears to contain an API key") {
		t.Errorf("file secret: %s", msg)
	}
}

func TestSkillFormatNormalizeSkillPathAcceptsRelativePathsAndRejectsEscapes(t *testing.T) {
	if got, ok := skill.NormalizeSkillPath("./references/a.md"); !ok || got != "references/a.md" {
		t.Errorf("./references/a.md -> %q %v", got, ok)
	}
	if got, ok := skill.NormalizeSkillPath("scripts/run.py"); !ok || got != "scripts/run.py" {
		t.Errorf("scripts/run.py -> %q %v", got, ok)
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../b", "a//b", "a\\b", "C:/x"} {
		if got, ok := skill.NormalizeSkillPath(bad); ok {
			t.Errorf("NormalizeSkillPath(%q) = %q, want a rejection", bad, got)
		}
	}
}

// TestSkillFormatTheEssentialSkillsAreValid checks that the essential skills are
// valid. The package under test cannot import skillhub (which owns the embedded
// copies), so it decodes the packages of the recorded essential skills from the
// wiregen dump, which skillhub proves byte-identical to its embedded files.
func TestSkillFormatTheEssentialSkillsAreValid(t *testing.T) {
	var dump struct {
		Skills []skill.Package `json:"skills"`
		Names  []string        `json:"names"`
	}
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "essential-skills.json"), &dump); err != nil {
		t.Fatalf("decode essential-skills.json: %v", err)
	}
	// Update (A-70): watch-and-react is the fourth essential skill.
	if len(dump.Skills) != 4 {
		t.Fatalf("the dump holds %d essential skills, want 4", len(dump.Skills))
	}
	var names []string
	parsedAll := make([]skill.Parsed, 0, len(dump.Skills))
	for _, pkg := range dump.Skills {
		parsed, err := skill.ParsePackage(pkg)
		if err != nil {
			t.Fatalf("an essential skill does not parse: %v", err)
		}
		parsedAll = append(parsedAll, parsed)
		names = append(names, parsed.Frontmatter.Name)
	}
	if !slices.Equal(names, dump.Names) {
		t.Errorf("parsed names = %v, ESSENTIAL_SKILL_NAMES = %v", names, dump.Names)
	}
	creator := parsedAll[0]
	if !slices.Equal(creator.RequiresTools, []string{"skill_manage"}) {
		t.Errorf("skill-creator requiresTools = %v", creator.RequiresTools)
	}
	if len(creator.Files) != 1 || creator.Files[0].Path != "references/format.md" {
		t.Errorf("skill-creator files = %+v", creator.Files)
	}
	if !strings.Contains(creator.Body, "skill_manage(action=\"create\"") {
		t.Error("the skill-creator body does not mention skill_manage(action=\"create\"")
	}
}
