package skill_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxSeedBytes keeps the very large size-limit cases of the corpus out of the fuzz
// seed corpus: they only exercise the byte counters, and megabyte seeds slow the
// fuzzing engine down.
const maxSeedBytes = 8 << 10

// FuzzParsePackage feeds arbitrary model-authored input to the parser. It must never
// panic; every error must be a *kernel.SkillError with code invalid; every success
// must respect the format limits and hold a valid name. The seeds are every input of the
// three golden corpora, so the fuzzer starts from the shapes the parser is known to
// handle and mutates around them.
func FuzzParsePackage(f *testing.F) {
	for _, file := range []string{"skill-parse-corpus.json", "skill-mutation-corpus.json"} {
		for _, c := range loadCorpus(f, file) {
			addSeed(f, c.Pkg)
		}
	}
	for _, c := range loadYAMLSources(f) {
		f.Add("---\n"+c+"\n---\nbody", false, "", "")
	}
	f.Add("", false, "", "")
	f.Add("---\n---\n", false, "", "")
	f.Add("---\nname: a\ndescription: b\n---\nc", true, "./x/../y", "z")

	f.Fuzz(func(t *testing.T, content string, hasFile bool, path, fileContent string) {
		pkg := skill.Package{Content: content}
		if hasFile {
			pkg.Files = []skill.File{{Path: path, Content: fileContent}}
		}
		parsed, err := skill.ParsePackage(pkg)
		if err != nil {
			var se *kernel.SkillError
			if !errors.As(err, &se) {
				t.Fatalf("error is %T, want *kernel.SkillError: %v", err, err)
			}
			if se.Code != kernel.SkillCodeInvalid {
				t.Fatalf("code = %q, want invalid", se.Code)
			}
			if !strings.HasPrefix(se.Message, "Invalid skill") {
				t.Fatalf("message = %q", se.Message)
			}
			return
		}

		name := parsed.Frontmatter.Name
		if !skill.NamePattern.MatchString(name) || kernel.UTF16Len(name) > skill.MaxNameLength {
			t.Fatalf("accepted an invalid name %q", name)
		}
		if d := parsed.Frontmatter.Description; d == "" || kernel.UTF16Len(d) > skill.MaxDescriptionLength {
			t.Fatalf("accepted a description of %d units", kernel.UTF16Len(d))
		}
		if kernel.UTF16Len(parsed.Frontmatter.Compatibility) > skill.MaxCompatibilityLength {
			t.Fatal("accepted an over-long compatibility")
		}
		if parsed.Body == "" {
			t.Fatal("accepted an empty body")
		}
		if len(strings.ReplaceAll(content, "\r\n", "\n")) > skill.MaxContentBytes {
			t.Fatal("accepted SKILL.md over the byte limit")
		}
		if parsed.Files == nil || parsed.RequiresTools == nil {
			t.Fatal("Files and RequiresTools must not be nil")
		}
		total := 0
		for _, file := range parsed.Files {
			// Normalising is not idempotent ("./ 0" gives " 0", which trims
			// to "0"), so check the safety properties of the accepted path instead.
			if file.Path == "" || kernel.UTF16Len(file.Path) > skill.MaxPathLength ||
				strings.HasPrefix(file.Path, "/") || strings.ContainsAny(file.Path, "\\\x00") {
				t.Fatalf("accepted unsafe file path %q", file.Path)
			}
			for _, segment := range strings.Split(file.Path, "/") {
				if segment == "" || segment == "." || segment == ".." {
					t.Fatalf("accepted file path %q with segment %q", file.Path, segment)
				}
			}
			if len(file.Content) > skill.MaxFileBytes {
				t.Fatalf("accepted a file of %d bytes", len(file.Content))
			}
			total += len(file.Content)
		}
		if total > skill.MaxTotalBytes {
			t.Fatalf("accepted files totalling %d bytes", total)
		}
	})
}

// addSeed adds a corpus package as a seed: its content and its first file.
func addSeed(f *testing.F, pkg skill.Package) {
	size := len(pkg.Content)
	for _, file := range pkg.Files {
		size += len(file.Path) + len(file.Content)
	}
	if size > maxSeedBytes {
		return
	}
	if len(pkg.Files) > 0 {
		f.Add(pkg.Content, true, pkg.Files[0].Path, pkg.Files[0].Content)
		return
	}
	f.Add(pkg.Content, false, "", "")
}

// loadYAMLSources returns the source of every case of the YAML-subset corpus.
func loadYAMLSources(tb testing.TB) []string {
	tb.Helper()
	var cases []struct {
		Source string          `json:"source"`
		Name   string          `json:"name"`
		Result json.RawMessage `json:"result"`
	}
	if err := jsonx.UnmarshalStrict(wiretest.Load(tb, "skill-yaml-corpus.json"), &cases); err != nil {
		tb.Fatalf("decode skill-yaml-corpus.json: %v", err)
	}
	out := make([]string, 0, len(cases))
	for _, c := range cases {
		if len(c.Source) <= maxSeedBytes {
			out = append(out, c.Source)
		}
	}
	return out
}
