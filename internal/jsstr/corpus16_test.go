package jsstr_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// ---------------------------------------------------------------------------
// The V8 tables of jsstr.
//
// Go's unicode package follows the Go toolchain (Go 1.25 knows Unicode 15, Go 1.27
// Unicode 17), Node 22.19 (the recorded oracle) Unicode 16. A jsstr that read
// Go's tables would therefore differ from V8 in one direction on one Go and in the
// other direction on the next: Go 1.25 lacks 13 punctuation code points (\p{P}) and
// 27 lowercase mappings (toLowerCase) that V8 has, Go 1.27 has a punctuation code
// point and five lowercase mappings that V8 lacks (gap G-12-1). So jsstr reads
// nothing from Go's unicode package: the recorder tools/wiregen/memory/jsstr.cases.mjs
// sweeps every code point in Node (\p{P}, toLowerCase, \p{Cased}, \p{Case_Ignorable}
// and three final-sigma probes) and writes tests/testdata/golden/jsstr-unicode16.json;
// unicode16.go holds the COMPLETE tables and is GENERATED from it (never typed by
// hand) with
//
//	go test ./internal/jsstr -run TestUnicode16Supplement -update-unicode16
//
// and this test is the drift check: it regenerates the tables in memory and
// compares them with the file on every Go, so a new recording or a hand edit shows
// up here; the generator never consults the running Go's unicode tables, so its
// output is the same on every toolchain. The per-code-point check against the V8
// sweep runs on every Go as well, and TestGeneratedTablesComparable bars the
// package from reading Go's unicode tables again.
// ---------------------------------------------------------------------------

var updateUnicode16 = flag.Bool("update-unicode16", false, "regenerate unicode16.go from tests/testdata/golden/jsstr-unicode16.json")

type unicode16Fixture struct {
	Node          string    `json:"node"`
	Unicode       string    `json:"unicode"`
	Punct         []rune    `json:"punct"`
	Lower         [][2]any  `json:"lower"`
	Cased         [][2]rune `json:"cased"`
	CaseIgnorable [][2]rune `json:"caseIgnorable"`
	SigmaAfter    [][2]rune `json:"sigmaAfter"`
	SigmaBetween  [][2]rune `json:"sigmaBetween"`
	SigmaBlocked  [][2]rune `json:"sigmaBlocked"`
}

type lowerRow struct {
	from rune
	to   string
}

func (f unicode16Fixture) lowerRows(t *testing.T) []lowerRow {
	t.Helper()
	rows := make([]lowerRow, len(f.Lower))
	for i, r := range f.Lower {
		cp, ok := r[0].(float64)
		to, ok2 := r[1].(string)
		if !ok || !ok2 {
			t.Fatalf("lower row %d is malformed: %v", i, r)
		}
		rows[i] = lowerRow{rune(cp), to}
	}
	return rows
}

// baselineLower is what a simple case mapping of Go's tables gives for a single code
// point: Go's simple mapping plus the one special casing of a lone code point (U+0130
// becomes i and a combining dot). A lone capital sigma lowercases to the ordinary
// sigma, as Go does. jsstr no longer reads Go's tables; this is the yardstick of
// TestUnicode16SupplementSize, which reports how far the running Go is from V8.
func baselineLower(r rune) string {
	if r == 0x130 {
		return "i" + string(rune(0x307))
	}
	return string(unicode.ToLower(r))
}

func loadUnicode16(t *testing.T) unicode16Fixture {
	t.Helper()
	var f unicode16Fixture
	if err := json.Unmarshal(wiretest.Load(t, "jsstr-unicode16.json"), &f); err != nil {
		t.Fatalf("decode jsstr-unicode16.json: %v", err)
	}
	return f
}

// collapse turns a strictly ascending list of code points into inclusive ranges.
func collapse(t *testing.T, name string, runes []rune) [][2]rune {
	t.Helper()
	var out [][2]rune
	for i, r := range runes {
		if i > 0 && r <= runes[i-1] {
			t.Fatalf("%s is not strictly ascending at U+%04X", name, r)
		}
		if n := len(out); n > 0 && out[n-1][1] == r-1 {
			out[n-1][1] = r
			continue
		}
		out = append(out, [2]rune{r, r})
	}
	return out
}

// checkRanges requires sorted, non-overlapping, non-empty inclusive ranges.
func checkRanges(t *testing.T, name string, ranges [][2]rune) {
	t.Helper()
	for i, r := range ranges {
		if r[0] > r[1] {
			t.Fatalf("%s range %d is empty: U+%04X-U+%04X", name, i, r[0], r[1])
		}
		if i > 0 && r[0] <= ranges[i-1][1] {
			t.Fatalf("%s range %d (U+%04X-U+%04X) is not after range %d", name, i, r[0], r[1], i-1)
		}
	}
}

// inRanges reports whether r lies in one of the sorted inclusive ranges.
func inRanges(ranges [][2]rune, r rune) bool {
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i][1] >= r })
	return i < len(ranges) && ranges[i][0] <= r
}

// rangeTable renders ranges as a runeRange table named name with the doc comment doc.
func rangeTable(b *strings.Builder, name, doc string, ranges [][2]rune) {
	fmt.Fprintf(b, "// %s\n", doc)
	fmt.Fprintf(b, "var %s = []runeRange{\n", name)
	for _, r := range ranges {
		fmt.Fprintf(b, "\t{0x%04X, 0x%04X},\n", r[0], r[1])
	}
	b.WriteString("}\n\n")
}

// generateUnicode16 renders unicode16.go from the recorded sweep: the complete V8
// tables. It never consults the unicode tables of the running Go, so the output is the
// same on every Go toolchain.
func generateUnicode16(t *testing.T, f unicode16Fixture) []byte {
	t.Helper()
	punct := collapse(t, "punct", f.Punct)
	for name, ranges := range map[string][][2]rune{"cased": f.Cased, "caseIgnorable": f.CaseIgnorable} {
		checkRanges(t, name, ranges)
	}

	var lower [][2]rune
	for _, row := range f.lowerRows(t) {
		runes := []rune(row.to)
		if row.from == 0x130 {
			if row.to != "i"+string(rune(0x307)) {
				t.Fatalf("U+0130 lowercases to %q, want i and a combining dot", row.to)
			}
			continue // ToLower special-cases U+0130 and never looks it up
		}
		if len(runes) != 1 {
			t.Fatalf("U+%04X lowercases to %q, a multi code point mapping the table cannot hold", row.from, row.to)
		}
		lower = append(lower, [2]rune{row.from, runes[0]})
	}
	slices.SortFunc(lower, func(a, b [2]rune) int { return int(a[0]) - int(b[0]) })

	var b strings.Builder
	b.WriteString("// Code generated by TestUnicode16Supplement (go test ./internal/jsstr -run TestUnicode16Supplement -update-unicode16)\n")
	b.WriteString("// from tests/testdata/golden/jsstr-unicode16.json; DO NOT EDIT.\n\n")
	b.WriteString("package jsstr\n\n")
	b.WriteString("// The complete V8 tables of Node " + f.Node + " (Unicode " + f.Unicode + "). The recorder\n")
	b.WriteString("// tools/wiregen/memory/jsstr.cases.mjs sweeps every code point, so nothing here is typed by\n")
	b.WriteString("// hand. jsstr reads nothing from Go's unicode package, so its results do not depend on the\n")
	b.WriteString("// Unicode version of the Go toolchain.\n")
	b.WriteString("const (\n")
	fmt.Fprintf(&b, "\tsupplementNode    = %q\n", f.Node)
	fmt.Fprintf(&b, "\tsupplementUnicode = %q\n", f.Unicode)
	b.WriteString(")\n\n")
	b.WriteString("// runeRange is an inclusive range of code points.\n")
	b.WriteString("type runeRange struct{ lo, hi rune }\n\n")
	rangeTable(&b, "punctRanges", fmt.Sprintf("punctRanges are the %d code points of \\p{P} as %d sorted ranges.", len(f.Punct), len(punct)), punct)
	b.WriteString("// lowerMapping is one single code point toLowerCase mapping.\n")
	b.WriteString("type lowerMapping struct{ from, to rune }\n\n")
	fmt.Fprintf(&b, "// lowerMappings are the %d single code point toLowerCase mappings (sorted by from); U+0130, which\n", len(lower))
	b.WriteString("// lowercases to two code points, is handled by ToLower itself.\n")
	b.WriteString("var lowerMappings = []lowerMapping{\n")
	for _, m := range lower {
		fmt.Fprintf(&b, "\t{0x%04X, 0x%04X},\n", m[0], m[1])
	}
	b.WriteString("}\n\n")
	rangeTable(&b, "casedRanges", fmt.Sprintf("casedRanges are the code points of \\p{Cased} as %d sorted ranges.", len(f.Cased)), f.Cased)
	rangeTable(&b, "caseIgnorableRanges", fmt.Sprintf("caseIgnorableRanges are the code points of \\p{Case_Ignorable} as %d sorted ranges.", len(f.CaseIgnorable)), f.CaseIgnorable)

	src, err := format.Source([]byte(strings.TrimRight(b.String(), "\n") + "\n"))
	if err != nil {
		t.Fatalf("format the generated source: %v", err)
	}
	return src
}

// generatedTablesComparable parses one Go source and returns the references that would
// tie jsstr to the Unicode version of the Go toolchain: an import of the unicode
// package or of regexp, a call through the strings or bytes package of EqualFold,
// ToUpper, ToTitle, Title, ToUpperSpecial, ToLowerSpecial or ToTitleSpecial, and
// bytes.ToLower. strings.ToLower stays allowed (its one call is ToLower's all-ASCII
// branch, which is byte arithmetic) and so does unicode/utf8.
//
// The name is historical. The committed tables used to be the Go 1.25 supplement and
// were compared byte for byte only on Unicode 15.0.0, until Go 1.27 brought Unicode 17
// (G-12-1). Now the tables are the complete V8 ones, the byte comparison runs on every
// Go, and this function is the guard that keeps it that way. The per-code-point
// comparison with the V8 sweep stays unconditional (WR-B05).
func generatedTablesComparable(src []byte) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return []string{"unparsable source: " + err.Error()}
	}
	var refs []string
	versioned := map[string]bool{}    // local package names of strings and bytes
	bannedImport := map[string]bool{} // local names of imports banned outright
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return []string{"unparsable import: " + imp.Path.Value}
		}
		local := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			local = path[i+1:]
		}
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch path {
		case "unicode", "regexp":
			refs = append(refs, "imports "+path)
			bannedImport[local] = true
		case "strings", "bytes":
			versioned[local] = true
		}
	}
	banned := map[string]bool{
		"EqualFold": true, "ToUpper": true, "ToTitle": true, "Title": true,
		"ToUpperSpecial": true, "ToLowerSpecial": true, "ToTitleSpecial": true,
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || !versioned[id.Name] {
			return true
		}
		name := sel.Sel.Name
		if banned[name] || (name == "ToLower" && id.Name == "bytes") {
			refs = append(refs, id.Name+"."+name)
		}
		return true
	})
	slices.Sort(refs)
	return slices.Compact(refs)
}

func TestGeneratedTablesComparable(t *testing.T) {
	negative := []string{
		"package x\n\nimport \"unicode\"\n\nvar _ = unicode.P\n",
		"package x\n\nimport u \"unicode\"\n\nvar _ = u.P\n",
		"package x\n\nimport \"regexp\"\n\nvar _ = regexp.MustCompile(\"a\")\n",
		"package x\n\nimport \"strings\"\n\nvar _ = strings.EqualFold\n",
		"package x\n\nimport \"strings\"\n\nvar _ = strings.ToUpper(\"a\")\n",
		"package x\n\nimport \"bytes\"\n\nvar _ = bytes.ToLower(nil)\n",
	}
	for _, src := range negative {
		if refs := generatedTablesComparable([]byte(src)); len(refs) == 0 {
			t.Errorf("a source that ties jsstr to the Go toolchain's Unicode tables was not caught:\n%s", src)
		}
	}
	positive := "package x\n\nimport (\n\t\"strings\"\n\t\"unicode/utf8\"\n)\n\nvar _ = strings.ToLower(\"a\")\nvar _ = utf8.RuneSelf\n"
	if refs := generatedTablesComparable([]byte(positive)); len(refs) != 0 {
		t.Errorf("a source using only unicode/utf8 and strings.ToLower reported %v", refs)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if refs := generatedTablesComparable(src); len(refs) != 0 {
			t.Errorf("%s reads the Go toolchain's Unicode tables (%v): its results would follow the Unicode version of the Go toolchain "+
				"instead of the pinned Node's; use the generated tables of unicode16.go", name, refs)
		}
	}
	if checked == 0 {
		t.Fatal("no non-test .go file found in the package directory")
	}
}

func TestUnicode16Supplement(t *testing.T) {
	f := loadUnicode16(t)

	got := generateUnicode16(t, f)
	if *updateUnicode16 {
		if err := os.WriteFile("unicode16.go", got, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Log("unicode16.go regenerated")
	}
	have, err := os.ReadFile("unicode16.go")
	if err != nil {
		t.Fatal(err)
	}

	// A fixture recorded under another Unicode version than the tables were generated
	// for is the one failure that deserves its own message.
	m := regexp.MustCompile(`supplementNode\s*=\s*"([^"]*)"\s*\n\s*supplementUnicode\s*=\s*"([^"]*)"`).FindSubmatch(have)
	if m == nil {
		t.Fatal("unicode16.go does not declare supplementNode and supplementUnicode")
	}
	if string(m[2]) != f.Unicode {
		t.Fatalf("the tables were generated for Node %s (Unicode %s) but the fixture was recorded under Node %s (Unicode %s): "+
			"regenerate them with go test ./internal/jsstr -run TestUnicode16Supplement -update-unicode16, and pin the same Node patch version in CI",
			m[1], m[2], f.Node, f.Unicode)
	}
	// The generator does not read the running Go's tables, so the comparison is exact on every Go.
	if !bytes.Equal(have, got) {
		t.Fatal("unicode16.go differs from the tables generated from jsstr-unicode16.json: " +
			"run go test ./internal/jsstr -run TestUnicode16Supplement -update-unicode16")
	}

	// The tables are what makes jsstr equal V8: every code point of the sweep agrees.
	punct := map[rune]bool{}
	for _, r := range f.Punct {
		punct[r] = true
	}
	lower := map[rune]string{}
	for _, row := range f.lowerRows(t) {
		lower[row.from] = row.to
	}
	const sigma, finalSigma = rune(0x3A3), rune(0x3C2)
	sigmaStr := string(sigma)
	var punctMismatch, lowerMismatch, afterMismatch, betweenMismatch, blockedMismatch []string
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // a lone surrogate is not a Go rune
		}
		if jsstr.IsPunct(r) != punct[r] && len(punctMismatch) < 5 {
			punctMismatch = append(punctMismatch, fmt.Sprintf("U+%04X got %v", r, jsstr.IsPunct(r)))
		}
		s := string(r)
		want, mapped := lower[r]
		if !mapped {
			want = s
		}
		if got := jsstr.ToLower(s); got != want && len(lowerMismatch) < 5 {
			lowerMismatch = append(lowerMismatch, fmt.Sprintf("U+%04X got %q want %q", r, got, want))
		}

		// The three final-sigma probes: a capital sigma next to r, as V8 lowercases it.
		after := []rune(jsstr.ToLower(s + sigmaStr))
		if got, want := after[len(after)-1] == finalSigma, inRanges(f.SigmaAfter, r); got != want && len(afterMismatch) < 5 {
			afterMismatch = append(afterMismatch, fmt.Sprintf("U+%04X got %v want %v", r, got, want))
		}
		between := []rune(jsstr.ToLower("A" + s + sigmaStr))
		if got, want := between[len(between)-1] == finalSigma, inRanges(f.SigmaBetween, r); got != want && len(betweenMismatch) < 5 {
			betweenMismatch = append(betweenMismatch, fmt.Sprintf("U+%04X got %v want %v", r, got, want))
		}
		blocked := []rune(jsstr.ToLower("A" + sigmaStr + s))
		if got, want := blocked[1] != finalSigma, inRanges(f.SigmaBlocked, r); got != want && len(blockedMismatch) < 5 {
			blockedMismatch = append(blockedMismatch, fmt.Sprintf("U+%04X got %v want %v", r, got, want))
		}
	}
	// A divergence caused by a toolchain bump must read as one in a CI log.
	ctx := fmt.Sprintf("(oracle Node %s, Unicode %s; running %s, unicode.Version %s)", f.Node, f.Unicode, runtime.Version(), unicode.Version)
	if len(punctMismatch) > 0 {
		t.Errorf("IsPunct differs from V8 \\p{P} %s: %v", ctx, punctMismatch)
	}
	if len(lowerMismatch) > 0 {
		t.Errorf("ToLower differs from V8 toLowerCase %s: %v", ctx, lowerMismatch)
	}
	if len(afterMismatch) > 0 {
		t.Errorf("ToLower of a code point and a capital sigma differs from V8 (final sigma after) %s: %v", ctx, afterMismatch)
	}
	if len(betweenMismatch) > 0 {
		t.Errorf("ToLower of a letter, a code point and a capital sigma differs from V8 (final sigma between) %s: %v", ctx, betweenMismatch)
	}
	if len(blockedMismatch) > 0 {
		t.Errorf("ToLower of a letter, a capital sigma and a code point differs from V8 (final sigma blocked) %s: %v", ctx, blockedMismatch)
	}
}

// TestUnicode16SupplementSize is a diagnostic of how far the tables of the running Go
// are from the pinned V8 tables, which jsstr no longer reads. It also pins the size of
// the recording.
func TestUnicode16SupplementSize(t *testing.T) {
	f := loadUnicode16(t)
	if f.Unicode != "16.0" {
		t.Skipf("the corpus was recorded under Unicode %s", f.Unicode)
	}
	if len(f.Punct) != 855 {
		t.Errorf("the corpus holds %d punctuation code points, want 855", len(f.Punct))
	}
	for _, c := range []struct {
		name   string
		ranges [][2]rune
		want   int
	}{
		{"cased", f.Cased, 159},
		{"caseIgnorable", f.CaseIgnorable, 452},
		{"sigmaAfter", f.SigmaAfter, 152},
		{"sigmaBetween", f.SigmaBetween, 557},
		{"sigmaBlocked", f.SigmaBlocked, 152},
	} {
		if len(c.ranges) != c.want {
			t.Errorf("the corpus holds %d %s ranges, want %d", len(c.ranges), c.name, c.want)
		}
	}
	punct := map[rune]bool{}
	var extraPunct int // V8 classifies, the running Go does not
	for _, r := range f.Punct {
		punct[r] = true
		if !unicode.Is(unicode.P, r) {
			extraPunct++
		}
	}
	var morePunct int // the running Go classifies, V8 does not
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.P, r) && !punct[r] {
			morePunct++
		}
	}
	rows := f.lowerRows(t)
	mapped := map[rune]bool{}
	var extraLower int // V8 maps, the running Go does not
	for _, row := range rows {
		mapped[row.from] = true
		if row.to != baselineLower(row.from) {
			extraLower++
		}
	}
	var moreLower int // the running Go maps, V8 does not
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !mapped[r] && unicode.ToLower(r) != r {
			moreLower++
		}
	}
	// Go 1.25 is Unicode 15: 13 punctuation and 27 lowercase differences. A newer Go
	// has fewer; the number may only shrink.
	if extraPunct > 13 || extraLower > 27 {
		t.Errorf("the gap to Go's tables grew: %d punctuation and %d lowercase code points", extraPunct, extraLower)
	}
	t.Logf("V8 has what %s (unicode.Version %s) lacks: %d punctuation, %d lowercase", runtime.Version(), unicode.Version, extraPunct, extraLower)
	t.Logf("%s (unicode.Version %s) has what V8 lacks: %d punctuation, %d lowercase", runtime.Version(), unicode.Version, morePunct, moreLower)
}
