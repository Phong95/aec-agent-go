package viewkit_test

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/internal/viewkit"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// viewkitCorpus mirrors tests/testdata/golden/viewkit-corpus.json, which wiregen
// computes with the real Node runtime (Buffer.toString, Number, RegExp).
type viewkitCorpus struct {
	LossyUTF8 []struct {
		Hex  string `json:"hex"`
		Text string `json:"text"`
	} `json:"lossyUtf8"`
	SliceBytes []struct {
		Text string `json:"text"`
		Max  int    `json:"max"`
		Out  string `json:"out"`
	} `json:"sliceBytes"`
	JSNumber []struct {
		Text  string          `json:"text"`
		Value json.RawMessage `json:"value"`
	} `json:"jsNumber"`
	RegexValidity []struct {
		Pattern   string `json:"pattern"`
		Flags     string `json:"flags"`
		Valid     bool   `json:"valid"`
		Message   string `json:"message"`
		Portable  bool   `json:"portable"`
		GoRejects bool   `json:"goRejects"`
	} `json:"regexValidity"`
	RegexMatch []struct {
		Pattern   string `json:"pattern"`
		Flags     string `json:"flags"`
		Text      string `json:"text"`
		Match     bool   `json:"match"`
		Divergent bool   `json:"divergent"`
	} `json:"regexMatch"`
}

func loadViewkitCorpus(t *testing.T) viewkitCorpus {
	t.Helper()
	var c viewkitCorpus
	if err := json.Unmarshal(wiretest.Load(t, "viewkit-corpus.json"), &c); err != nil {
		t.Fatalf("decode viewkit-corpus.json: %v", err)
	}
	return c
}

func TestViewkitLossyUTF8MatchesBuffer(t *testing.T) {
	c := loadViewkitCorpus(t)
	if len(c.LossyUTF8) < 50 {
		t.Fatalf("only %d lossyUtf8 rows", len(c.LossyUTF8))
	}
	for _, row := range c.LossyUTF8 {
		raw, err := hex.DecodeString(row.Hex)
		if err != nil {
			t.Fatalf("bad hex %q: %v", row.Hex, err)
		}
		if got := viewkit.LossyUTF8(raw); got != row.Text {
			t.Errorf("LossyUTF8(%s) = %q, want %q", row.Hex, got, row.Text)
		}
	}
}

func TestViewkitSliceBytesMatchesGolden(t *testing.T) {
	c := loadViewkitCorpus(t)
	if len(c.SliceBytes) < 50 {
		t.Fatalf("only %d sliceBytes rows", len(c.SliceBytes))
	}
	for _, row := range c.SliceBytes {
		if got := viewkit.SliceBytes(row.Text, row.Max); got != row.Out {
			t.Errorf("SliceBytes(%q, %d) = %q, want %q", row.Text, row.Max, got, row.Out)
		}
	}
}

func TestViewkitJSNumberMatchesNode(t *testing.T) {
	c := loadViewkitCorpus(t)
	if len(c.JSNumber) < 50 {
		t.Fatalf("only %d jsNumber rows", len(c.JSNumber))
	}
	for _, row := range c.JSNumber {
		got := viewkit.JSNumber(row.Text)
		var want float64
		switch s := strings.TrimSpace(string(row.Value)); s {
		case `"NaN"`:
			want = math.NaN()
		case `"Infinity"`:
			want = math.Inf(1)
		case `"-Infinity"`:
			want = math.Inf(-1)
		default:
			if err := json.Unmarshal(row.Value, &want); err != nil {
				t.Fatalf("row %q: bad value %s", row.Text, s)
			}
		}
		if math.IsNaN(want) {
			if !math.IsNaN(got) {
				t.Errorf("JSNumber(%q) = %v, want NaN", row.Text, got)
			}
			continue
		}
		if got != want {
			t.Errorf("JSNumber(%q) = %v, want %v", row.Text, got, want)
		}
	}
}

// TestViewkitRegexValidity checks the pre-scan and the RE2 compile against the real
// JS engine: a pattern JS rejects is never accepted, the constructs whose meaning
// differs are rejected although RE2 accepts them, and the portable subset compiles.
func TestViewkitRegexValidity(t *testing.T) {
	c := loadViewkitCorpus(t)
	var portable, rejected, invalid int
	for _, row := range c.RegexValidity {
		_, err := viewkit.CompileSearchRegex(row.Pattern, row.Flags == "i")
		switch {
		case !row.Valid:
			invalid++
			if err == nil {
				t.Errorf("JS rejects %q (flags %q: %s) but CompileSearchRegex accepted it", row.Pattern, row.Flags, row.Message)
			}
		case row.GoRejects:
			rejected++
			if err == nil {
				t.Errorf("%q (flags %q) must be rejected: its meaning differs from JS or RE2 cannot run it", row.Pattern, row.Flags)
			}
		case row.Portable:
			portable++
			if err != nil {
				t.Errorf("portable pattern %q (flags %q) did not compile: %v", row.Pattern, row.Flags, err)
			}
		}
	}
	if portable < 80 || rejected < 40 || invalid < 40 {
		t.Errorf("corpus too small: %d portable, %d rejected, %d invalid rows", portable, rejected, invalid)
	}
}

// TestViewkitRegexRejectsEveryDivergentConstruct is the explicit list of the
// constructs RE2 accepts with another meaning (CONTEXT D2); each must be refused
// with an error that names the construct.
func TestViewkitRegexRejectsEveryDivergentConstruct(t *testing.T) {
	tests := []struct{ pattern, mention string }{
		{`(?i)abc`, "(?i"},
		{`(?s).`, "(?s"},
		{`(?m)^a`, "(?m"},
		{`(?U)a+`, "(?U"},
		{`(?-i)a`, "(?-"},
		{`(?i:a)`, "(?i"},
		{`(?P<n>a)`, "(?P"},
		{`\Qa.b\E`, `\Q`},
		{`\pL`, `\p`},
		{`\PL`, `\P`},
		{`\p{L}`, `\p`},
		{`[\pL]`, `\p`},
		{`[[:alpha:]]`, `[[:`},
		{`\z`, `\z`},
		{`\A`, `\A`},
		{`\a`, `\a`},
		{`\x{41}`, `\x{`},
		{`[]a]`, "empty character class"},
		{`[^]a]`, "empty character class"},
		{`^*`, "quantifier"},
		{`$+`, "quantifier"},
		{`\b?`, "quantifier"},
		{`\B{2}`, "quantifier"},
		{`^{1}`, "quantifier"},
	}
	for _, tc := range tests {
		_, err := viewkit.CompileSearchRegex(tc.pattern, false)
		if err == nil {
			t.Errorf("%q accepted", tc.pattern)
			continue
		}
		if !strings.Contains(err.Error(), tc.mention) {
			t.Errorf("%q: error %q does not name %q", tc.pattern, err, tc.mention)
		}
	}
	// An escaped backslash is not an escape of the next character, and a class may
	// contain what is otherwise a construct.
	for _, ok := range []string{`\\p`, `\\Qx`, `[(?]`, `a\(?i`, `[a^]`, `a^b`, `a$b`, `x{2}`, `^{`, `\b\d`, `[\]]`} {
		if _, err := viewkit.CompileSearchRegex(ok, false); err != nil {
			t.Errorf("%q must compile: %v", ok, err)
		}
	}
}

func TestViewkitRegexIgnoreCase(t *testing.T) {
	re, err := viewkit.CompileSearchRegex("hello", true)
	if err != nil || !re.MatchString("HELLO") {
		t.Fatalf("ignoreCase hello: %v", err)
	}
	re, err = viewkit.CompileSearchRegex("a|b", true)
	if err != nil || !re.MatchString("B") || !re.MatchString("A") {
		t.Fatalf("ignoreCase must cover every alternative: %v", err)
	}
	re, err = viewkit.CompileSearchRegex("hello", false)
	if err != nil || re.MatchString("HELLO") {
		t.Fatalf("case sensitive hello: %v", err)
	}
	// The error of an uncompilable pattern does not mention the added prefix.
	_, err = viewkit.CompileSearchRegex("(", true)
	if err == nil || strings.Contains(err.Error(), "(?i)") {
		t.Fatalf("error = %v, want Go's text without the (?i) prefix", err)
	}
}

// TestViewkitRegexMatchesJS compares matching with the real JS engine. Rows with
// divergent true are the documented RE2 differences; the test requires that they
// still differ, so a change that closes one is noticed and the documented list updated.
func TestViewkitRegexMatchesJS(t *testing.T) {
	c := loadViewkitCorpus(t)
	if len(c.RegexMatch) < 50 {
		t.Fatalf("only %d regexMatch rows", len(c.RegexMatch))
	}
	for _, row := range c.RegexMatch {
		re, err := viewkit.CompileSearchRegex(row.Pattern, row.Flags == "i")
		if err != nil {
			t.Errorf("pattern %q (flags %q) did not compile: %v", row.Pattern, row.Flags, err)
			continue
		}
		got := re.MatchString(row.Text)
		if row.Divergent {
			if got == row.Match {
				t.Errorf("documented divergence closed: %q (flags %q) on %q matches %v in both engines; update the corpus row", row.Pattern, row.Flags, row.Text, got)
			}
			continue
		}
		if got != row.Match {
			t.Errorf("%q (flags %q) on %q = %v in Go, %v in JS", row.Pattern, row.Flags, row.Text, got, row.Match)
		}
	}
}

func TestViewkitTimeoutDelay(t *testing.T) {
	const ms = 1_000_000
	tests := []struct {
		in   float64
		want int64 // milliseconds
	}{
		{math.NaN(), 1},
		{math.Inf(1), 1},
		{math.Inf(-1), 1},
		{-5, 1},
		{0, 1},
		{0.4, 1},
		{0.999, 1},
		{1, 1},
		{1.9, 1},
		{10, 10},
		{10.7, 10},
		{2147483647, 2147483647},
		{2147483648, 1},
		{1e13, 1},
	}
	for _, tc := range tests {
		if got := viewkit.TimeoutDelay(tc.in).Nanoseconds() / ms; got != tc.want {
			t.Errorf("TimeoutDelay(%v) = %d ms, want %d", tc.in, got, tc.want)
		}
	}
}

func TestViewkitGlobMatch(t *testing.T) {
	const (
		smile = "\U0001F600"
		ls    = "\U00002028"
	)
	tests := []struct {
		glob, name string
		fold, want bool
	}{
		{"*.txt", "a.txt", false, true},
		{"*.txt", "a.md", false, false},
		{"*.txt", ".txt", false, true},
		{"*", "", false, true},
		{"*", "anything", false, true},
		{"?", "a", false, true},
		{"?", "", false, false},
		{"?", "ab", false, false},
		{"a?c", "abc", false, true},
		{"a?c", "ac", false, false},
		{"a*c", "abbbc", false, true},
		{"a*c", "ac", false, true},
		{"a*c", "abbbd", false, false},
		{"a**c", "abc", false, true},
		{"*a*b*", "xxaxxbxx", false, true},
		{"*a*b*", "xxbxxax", false, false},
		{"a.b", "axb", false, false},
		{"a.b", "a.b", false, true},
		{"a+b", "a+b", false, true},
		{"a+b", "aab", false, false},
		{"[a]", "[a]", false, true},
		{"[a]", "a", false, false},
		{"(a)", "(a)", false, true},
		{"a|b", "a|b", false, true},
		{"a|b", "a", false, false},
		{"^a$", "^a$", false, true},
		{"a{2}", "a{2}", false, true},
		{"a\\b", "a\\b", false, true},
		{"*.TXT", "a.txt", false, false},
		{"*.TXT", "a.txt", true, true},
		{"ABC", "abc", true, true},
		{"?", smile, false, false},
		{"??", smile, false, true},
		{"*", smile, false, true},
		{smile, smile, false, true},
		{"?x", smile + "x", false, false},
		{"*", "a" + ls + "b", false, false},
		{"?", ls, false, false},
		{"a*b", "a\nb", false, false},
		{"a?b", "a\rb", false, false},
		{"a*", "a\tb", false, true},
		{"*b", "ab", false, true},
		{"*ab", "aab", false, true},
		{"a*a*a", "aaaaa", false, true},
		{"a*a*a", "aaaab", false, false},
	}
	for _, tc := range tests {
		if got := viewkit.GlobMatch(tc.glob, tc.name, tc.fold); got != tc.want {
			t.Errorf("GlobMatch(%q, %q, fold=%v) = %v, want %v", tc.glob, tc.name, tc.fold, got, tc.want)
		}
	}
}

// viewkitFixtures is every golden fixture owned by package viewkit.
var viewkitFixtures = []string{"viewkit-corpus.json"}

// TestViewkitFixturesCovered fails when a fixture with the viewkit- prefix has no
// entry above, and when an entry has no file.
func TestViewkitFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "viewkit-") && !slices.Contains(viewkitFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in viewkitFixtures", e.Name())
		}
	}
	for _, f := range viewkitFixtures {
		if !present[f] {
			t.Errorf("viewkitFixtures entry %s has no fixture file", f)
		}
	}
}

// TestViewkitExecTimeout pins the exec timeout rules: an exec ceiling of 0 or below is no
// cap, and the timer takes the real duration (no 2^31-1 ms rule, which TimeoutDelay keeps
// for the in-memory sandbox).
func TestViewkitExecTimeout(t *testing.T) {
	for _, tc := range []struct{ req, ceiling, want int }{
		{5000, 300, 300},
		{200, 300, 200},
		{5000, 0, 5000},
		{5000, -1, 5000},
		{3_000_000_000, 0, 3_000_000_000},
		{3_000_000_000, 2_500_000_000, 2_500_000_000},
	} {
		if got := viewkit.ExecTimeoutMs(tc.req, tc.ceiling); got != tc.want {
			t.Errorf("ExecTimeoutMs(%d, %d) = %d, want %d", tc.req, tc.ceiling, got, tc.want)
		}
	}
	for _, tc := range []struct {
		ms   int
		want time.Duration
	}{
		{1, time.Millisecond},
		{2147483648, 2147483648 * time.Millisecond},
		{3_000_000_000, 3_000_000_000 * time.Millisecond},
		{0, time.Millisecond},
		{math.MaxInt, time.Duration(math.MaxInt64)},
	} {
		if got := viewkit.ExecTimeout(tc.ms); got != tc.want {
			t.Errorf("ExecTimeout(%d) = %v, want %v", tc.ms, got, tc.want)
		}
	}
}
