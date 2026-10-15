package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// ux expands the {U+XXXX} placeholders of a test string to code points, so the test sources
// stay ASCII (and no escape can be rewritten into a literal character by an editor).
func ux(s string) string {
	return regexp.MustCompile(`\{U\+([0-9A-Fa-f]{4,6})\}`).ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseInt(m[3:len(m)-1], 16, 32)
		if err != nil {
			panic(err)
		}
		return string(rune(n))
	})
}

// TestHTMLToText_DropsScriptsStylesAndComments is the first htmlToText case.
func TestHTMLToText_DropsScriptsStylesAndComments(t *testing.T) {
	html := `<html><head><title>T &amp; C</title><style>.a{}</style></head>
      <body><script>alert(1)</script><!-- hidden --><h1>Heading</h1><p>First&nbsp;para</p>
      <ul><li>One</li><li>Two &#8211; &#x2014;</li></ul></body></html>`
	text := htmlToText(html)
	if regexp.MustCompile(`alert|\.a\{\}|hidden`).MatchString(text) {
		t.Fatalf("dropped content survived: %q", text)
	}
	if !strings.Contains(text, "Heading") {
		t.Fatalf("missing Heading: %q", text)
	}
	if !strings.Contains(text, "First para") {
		t.Fatalf("missing First para: %q", text)
	}
	if !regexp.MustCompile(ux(`One\n+Two {U+2013} {U+2014}`)).MatchString(text) {
		t.Fatalf("missing the list: %q", text)
	}
	if title, ok := extractTitle(html); !ok || title != "T & C" {
		t.Fatalf("title = %q %v", title, ok)
	}
}

// TestMainContent is the second htmlToText case.
func TestMainContent(t *testing.T) {
	page := "<body><nav>Menu</nav><main class=x><h1>Body</h1><aside>Ads</aside></main><footer>F</footer></body>"
	for _, c := range []struct{ in, want string }{
		{page, "Body"},
		{"<div>Menu</div><article><p>Story</p></article>", "Story"},
		{"<mainframe>x</mainframe><p>All</p>", "x\nAll"},
	} {
		if got := htmlToText(mainContent(c.in)); got != c.want {
			t.Errorf("htmlToText(mainContent(%q)) = %q, want %q", c.in, got, c.want)
		}
	}
}

// linearInput is the pathological page, scaled.
func linearInput(scale int) string {
	return strings.Repeat("<script", 50_000*scale) + strings.Repeat("<", 200_000*scale) + strings.Repeat("<div", 50_000*scale)
}

// TestHTMLToText_StaysLinear is the third htmlToText case (under 2 s), plus a
// ten times larger input to see the slope, and the other inputs that make a naive scanner
// quadratic: unclosed dropped elements, a "<" without a ">", entities, comments, a "<main"
// flood and a marker flood for the DuckDuckGo parser.
func TestHTMLToText_StaysLinear(t *testing.T) {
	durations := map[int]time.Duration{}
	for _, scale := range []int{1, 10} {
		in := linearInput(scale)
		start := time.Now()
		_ = htmlToText(in)
		durations[scale] = time.Since(start)
		limit := 2 * time.Second
		if scale > 1 {
			limit = 20 * time.Second
		}
		if durations[scale] > limit {
			t.Fatalf("scale %d (%d bytes) took %v", scale, len(in), durations[scale])
		}
	}
	// ten times the input must take about ten times as long, not a hundred (generous bound, the
	// floor keeps a very fast first run from making the ratio meaningless)
	floor := durations[1]
	if floor < 20*time.Millisecond {
		floor = 20 * time.Millisecond
	}
	if durations[10] > 40*floor {
		t.Errorf("not linear: scale 1 took %v, scale 10 took %v", durations[1], durations[10])
	}

	traps := map[string]string{
		"unclosed-script": strings.Repeat("<script>", 100_000),
		"lt-without-gt":   strings.Repeat("<a ", 200_000),
		"amp":             strings.Repeat("&", 500_000) + strings.Repeat("&#x41", 100_000),
		"comments":        strings.Repeat("<!--", 200_000),
		"main-scan":       strings.Repeat("<mainx", 200_000),
		"ddg-markers":     strings.Repeat(`<a class="result__a" href="https://a.example/`, 20_000),
		"ddg-no-anchor":   strings.Repeat(`class="result__a" `, 100_000),
		"ddg-no-gt":       strings.Repeat(`class="result__a"`, 100_000),
	}
	for name, in := range traps {
		start := time.Now()
		_ = htmlToText(mainContent(in))
		_ = parseDuckDuckGoHTML(in, 10)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %d bytes took %v", name, len(in), d)
		}
	}
}

func TestDecodeEntities(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"&amp;&lt;&gt;&quot;&apos;", `&<>"'`},
		{"&AMP; &Copy; &NBSP;", ux("& {U+00A9}  ")},
		{"&bogus; &a; &abcdefghi; &amp", "&bogus; &a; &abcdefghi; &amp"},
		{"&#65;&#x41;&#X41;&#x1F600;", ux("AAA{U+1F600}")},
		{"&#0; &#1114112; &#99999999; &#x1234567;", "&#0; &#1114112; &#99999999; &#x1234567;"},
		{"&#xD800; &#xdfff;", ux("{U+FFFD} {U+FFFD}")},
		{"&#xZ; &#x; &# ;", "&#xZ; &#x; &# ;"},
		{"no entities", "no entities"},
		{"&&amp;&", "&&&"},
	} {
		if got := decodeEntities(c.in); got != c.want {
			t.Errorf("decodeEntities(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// htmlFixture is builtin-html-text.json, recorded from reference helpers by
// tools/wiregen/builtin/html_text.cases.mjs.
type htmlFixture struct {
	HTML []struct {
		In       string  `json:"in"`
		Text     string  `json:"text"`
		Main     string  `json:"main"`
		MainText string  `json:"mainText"`
		Title    *string `json:"title"`
	} `json:"html"`
	Truncate []struct {
		Text      string `json:"text"`
		N         int    `json:"n"`
		Out       string `json:"out"`
		Truncated bool   `json:"truncated"`
	} `json:"truncate"`
}

// htmlGoDivergence pins the fields of a recorded html row where the helpers deliberately
// differ from the recording (the recording has the U+0130 lowercase offset bug). A nil
// field equals the recorded value.
type htmlGoDivergence struct{ text, main, mainText *string }

func strp(s string) *string { return &s }

func htmlDivergences() map[string]htmlGoDivergence {
	return map[string]htmlGoDivergence{
		// Recorded: abI-with-dot then x (the script body is kept: the offsets of the lowered
		// copy are one unit too far); the helper strips the script.
		ux("ab{U+0130}<script>x</script>cd"): {text: strp(ux("ab{U+0130}cd")), mainText: strp(ux("ab{U+0130}cd"))},
		// Recorded: main is "m<" (the close tag is found one unit early); expected: "m".
		ux("{U+0130}<main>m</main>"): {main: strp("m"), mainText: strp("m")},
	}
}

// TestHTMLTextCorpus replays the recorded corpus (htmlToText, mainContent, extractTitle,
// truncate) through the helpers. Every row is equal except the pinned divergent rows,
// which are asserted to have the pinned value and to differ from the recording, so a
// difference that is fixed or a new one fails the test.
func TestHTMLTextCorpus(t *testing.T) {
	var fx htmlFixture
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-html-text.json")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-html-text: %v", err)
	}
	if len(fx.HTML) < 50 || len(fx.Truncate) < 15 {
		t.Fatalf("fixture too small: %d html rows, %d truncate rows", len(fx.HTML), len(fx.Truncate))
	}

	pinned := htmlDivergences()
	seen := map[string]bool{}
	check := func(label, in, field, ts, got string, want *string) {
		t.Helper()
		if want == nil {
			if got != ts {
				t.Errorf("%s %q %s: TS %q, Go %q", label, in, field, ts, got)
			}
			return
		}
		if got != *want {
			t.Errorf("%s %q %s: pinned Go value %q, got %q", label, in, field, *want, got)
		}
		if ts == *want {
			t.Errorf("%s %q %s: the TS value equals the pinned Go value now; drop the pin", label, in, field)
		}
	}
	for _, row := range fx.HTML {
		d, isPinned := pinned[row.In]
		if isPinned {
			seen[row.In] = true
		}
		main := mainContent(row.In)
		check("html", row.In, "text", row.Text, htmlToText(row.In), d.text)
		check("html", row.In, "main", row.Main, main, d.main)
		check("html", row.In, "mainText", row.MainText, htmlToText(main), d.mainText)
		title, ok := extractTitle(row.In)
		if ok != (row.Title != nil) || (row.Title != nil && title != *row.Title) {
			t.Errorf("html %q title: TS %v, Go %q/%v", row.In, row.Title, title, ok)
		}
	}
	for in := range pinned {
		if !seen[in] {
			t.Errorf("pinned divergent row %q is not in the corpus", in)
		}
	}

	// truncate: a cut in the middle of a surrogate pair is the one pinned difference
	truncPinned := map[string]string{
		fmt.Sprintf("%q/%d", ux("a{U+1F600}b"), 2):                    "a\n" + ellipsis + "[truncated 2 chars]",
		fmt.Sprintf("%q/%d", strings.Repeat(ux("{U+1F600}"), 10), 7):  strings.Repeat(ux("{U+1F600}"), 3) + "\n" + ellipsis + "[truncated 13 chars]",
		fmt.Sprintf("%q/%d", strings.Repeat(ux("{U+1F600}"), 10), 19): strings.Repeat(ux("{U+1F600}"), 9) + "\n" + ellipsis + "[truncated 1 chars]",
	}
	truncSeen := map[string]bool{}
	for _, row := range fx.Truncate {
		out, truncated := truncate(row.Text, row.N)
		key := fmt.Sprintf("%q/%d", row.Text, row.N)
		if want, ok := truncPinned[key]; ok {
			truncSeen[key] = true
			if out != want || !truncated {
				t.Errorf("truncate %s: pinned Go value %q, got %q", key, want, out)
			}
			if out == row.Out {
				t.Errorf("truncate %s: the TS value equals the Go value now; drop the pin", key)
			}
			continue
		}
		if out != row.Out || truncated != row.Truncated {
			t.Errorf("truncate %s: TS %q/%v, Go %q/%v", key, row.Out, row.Truncated, out, truncated)
		}
	}
	for key := range truncPinned {
		if !truncSeen[key] {
			t.Errorf("pinned truncate row %s is not in the corpus", key)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got, cut := truncate("hello", 5); got != "hello" || cut {
		t.Errorf("a text that fits is returned whole: %q %v", got, cut)
	}
	got, cut := truncate("hello world", 5)
	if got != "hello\n"+ellipsis+"[truncated 6 chars]" || !cut {
		t.Errorf("got %q %v", got, cut)
	}
	// the count is in UTF-16 units: an astral character counts 2
	if got, _ := truncate(ux("{U+1F600}{U+1F600}b"), 2); got != ux("{U+1F600}")+"\n"+ellipsis+"[truncated 3 chars]" {
		t.Errorf("got %q", got)
	}
}

// TestExtractTitleEdges: the cases that are not recorded one by one.
func TestExtractTitleEdges(t *testing.T) {
	for _, c := range []struct {
		in    string
		want  string
		found bool
	}{
		{"<title>a</title>", "a", true},
		{"<TITLE>Upper</TITLE>", "Upper", true},
		{"<title>  x \n y  </title>", "x y", true},
		{"<title></title>", "", false},
		{"<title>   </title>", "", false},
		{"<title>never closed", "", false},
		{"<title", "", false},
		{"no title", "", false},
	} {
		got, ok := extractTitle(c.in)
		if got != c.want || ok != c.found {
			t.Errorf("extractTitle(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.found)
		}
	}
}
