package jsstr_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// jsstrCorpus mirrors tests/testdata/golden/jsstr-corpus.json, which wiregen records
// from the real Node localeCompare (locale en-US) and toLowerCase.
type jsstrCorpus struct {
	LocaleOrder  []int           `json:"localeOrder"`
	LocalePairs  [][]interface{} `json:"localePairs"`
	LocaleSorted []struct {
		Input  []string `json:"input"`
		Sorted []string `json:"sorted"`
	} `json:"localeSorted"`
	DefaultLocaleAgrees bool `json:"defaultLocaleAgrees"`
	ToLower             []struct {
		Text  string `json:"text"`
		Lower string `json:"lower"`
	} `json:"toLower"`
}

func loadCorpus(t *testing.T) jsstrCorpus {
	t.Helper()
	var c jsstrCorpus
	if err := json.Unmarshal(wiretest.Load(t, "jsstr-corpus.json"), &c); err != nil {
		t.Fatalf("decode jsstr-corpus.json: %v", err)
	}
	return c
}

// TestLocaleCompareMatchesICU is the proof of the rank table in locale.go: the
// order of the 128 ASCII characters, about 380 recorded pairs and ten sorted
// lists all come from the real localeCompare.
func TestLocaleCompareMatchesICU(t *testing.T) {
	c := loadCorpus(t)
	if !c.DefaultLocaleAgrees {
		t.Fatal("the fixture was recorded where the default locale disagrees with en-US")
	}

	t.Run("order of the 128 ASCII characters", func(t *testing.T) {
		chars := make([]string, 128)
		for i := range chars {
			chars[i] = string(rune(i))
		}
		slices.SortStableFunc(chars, jsstr.LocaleCompare)
		got := make([]int, len(chars))
		for i, s := range chars {
			got[i] = int(s[0])
		}
		if !slices.Equal(got, c.LocaleOrder) {
			t.Fatalf("order differs from the fixture\n got: %v\nwant: %v", got, c.LocaleOrder)
		}
	})

	t.Run("recorded pairs", func(t *testing.T) {
		if len(c.LocalePairs) < 300 {
			t.Fatalf("only %d pairs in the fixture", len(c.LocalePairs))
		}
		for _, p := range c.LocalePairs {
			a, b, want := p[0].(string), p[1].(string), int(p[2].(float64))
			if got := jsstr.LocaleCompare(a, b); got != want {
				t.Errorf("LocaleCompare(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	})

	t.Run("sorted lists", func(t *testing.T) {
		if len(c.LocaleSorted) != 10 {
			t.Fatalf("%d lists in the fixture, want 10", len(c.LocaleSorted))
		}
		for i, l := range c.LocaleSorted {
			got := slices.Clone(l.Input)
			slices.SortStableFunc(got, jsstr.LocaleCompare)
			if !slices.Equal(got, l.Sorted) {
				t.Errorf("list %d sorted to %q, want %q", i, got, l.Sorted)
			}
		}
	})
}

func TestLocaleCompareDocumentedShapes(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"a", "A", -1},
		{"A", "b", -1},
		{"b", "B", -1}, // a < A < b < B
		{"A", "ab", -1},
		{"ab", "aB", -1},
		{"aB", "Ab", -1},
		{"Ab", "AB", -1},
		{"10", "2", -1}, // no numeric collation
		{"a_b", "a-b", -1},
		{"a-b", "a.txt", -1},
		{"a.txt", "A.txt", -1},
		{"a\x00b", "ab", 0}, // ignorable control character
		{"same", "same", 0},
		{"z", "\u00e9", -1}, // non-ASCII sorts after ASCII, by code point
		{"\u00e9", "\u00ea", -1},
	}
	for _, tc := range cases {
		if got := jsstr.LocaleCompare(tc.a, tc.b); got != tc.want {
			t.Errorf("LocaleCompare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := jsstr.LocaleCompare(tc.b, tc.a); got != -tc.want {
			t.Errorf("LocaleCompare(%q, %q) = %d, want %d (antisymmetry)", tc.b, tc.a, got, -tc.want)
		}
	}
}

// jsstrFixtures is every golden fixture owned by package jsstr. The three Phase 10
// corpora are replayed by TestUnicode16Supplement, TestParseIntMatchesGolden and
// TestParseDateMatchesGolden.
var jsstrFixtures = []string{"jsstr-corpus.json", "jsstr-unicode16.json", "jsstr-parseint.json", "jsstr-date-parse.json"}

// TestJsstrFixturesCovered fails when a fixture with the jsstr- prefix has no entry
// above, and when an entry has no file.
func TestJsstrFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "jsstr-") && !slices.Contains(jsstrFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in jsstrFixtures", e.Name())
		}
	}
	for _, f := range jsstrFixtures {
		if !present[f] {
			t.Errorf("jsstrFixtures entry %s has no fixture file", f)
		}
	}
}
