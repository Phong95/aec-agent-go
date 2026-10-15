package llmhub_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

const (
	notValidJSONPrefix = "the arguments are not valid JSON ("
	notObjectPrefix    = "the arguments must be one JSON object: "
	ellipsis           = "\u2026"
)

// toolArgsFixture mirrors llm-tool-arguments.json: the recorded parse results
// over a corpus.
type toolArgsFixture struct {
	Entries []struct {
		Name        string `json:"name"`
		Raw         string `json:"raw"`
		GoException string `json:"goException,omitempty"`
		Out         struct {
			Arguments      json.RawMessage `json:"arguments"`
			ArgumentsError string          `json:"argumentsError,omitempty"`
		} `json:"out"`
	} `json:"entries"`
}

// tsPreview is the preview rule written independently of the code under
// test: the first 200 UTF-16 units of raw, then an ellipsis when it is longer.
// It is only used for entries whose cut does not fall inside a surrogate pair.
func tsPreview(raw string) string {
	units := utf16.Encode([]rune(raw))
	if len(units) <= 200 {
		return raw
	}
	return string(utf16.Decode(units[:200])) + ellipsis
}

func TestParseToolArgumentsMatchesGolden(t *testing.T) {
	var f toolArgsFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "llm-tool-arguments.json"), &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Entries) < 20 {
		t.Fatalf("the corpus has %d entries, want at least 20", len(f.Entries))
	}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			args, gotErr := llmhub.ParseToolArguments(e.Raw)
			tsErr := e.Out.ArgumentsError

			if e.GoException == "split-surrogate" {
				// The recorded value cuts inside the emoji and keeps a lone surrogate; a
				// Go string cannot hold one, so the pair is dropped before the ellipsis.
				if args != nil {
					t.Fatalf("arguments = %s, want none", args)
				}
				if !strings.HasPrefix(tsErr, notValidJSONPrefix) {
					t.Fatalf("the fixture must hold a not-valid-JSON text, got %q", tsErr)
				}
				before, _, _ := strings.Cut(e.Raw, "\U0001F600")
				wantPreview := before + ellipsis
				if !strings.HasPrefix(gotErr, notValidJSONPrefix) || !strings.HasSuffix(gotErr, "): "+wantPreview) {
					t.Fatalf("error = %q, want the prefix %q and the suffix %q", gotErr, notValidJSONPrefix, "): "+wantPreview)
				}
				return
			}

			switch {
			case tsErr == "":
				if gotErr != "" {
					t.Fatalf("error = %q, TS has none", gotErr)
				}
				want := string(e.Out.Arguments)
				got := string(args)
				if got == "" {
					got = "{}"
				}
				if got != want {
					t.Fatalf("arguments = %s, TS has %s", got, want)
				}
			case strings.HasPrefix(tsErr, notObjectPrefix):
				if args != nil {
					t.Fatalf("arguments = %s, want none", args)
				}
				if gotErr != tsErr {
					t.Fatalf("error = %q, TS has %q", gotErr, tsErr)
				}
			case strings.HasPrefix(tsErr, notValidJSONPrefix):
				if args != nil {
					t.Fatalf("arguments = %s, want none", args)
				}
				suffix := "): " + tsPreview(e.Raw)
				if !strings.HasPrefix(gotErr, notValidJSONPrefix) || !strings.HasSuffix(gotErr, suffix) {
					t.Fatalf("error = %q, want the prefix %q and the suffix %q", gotErr, notValidJSONPrefix, suffix)
				}
				if !strings.HasSuffix(tsErr, suffix) {
					t.Fatalf("TS text %q does not end with the preview %q", tsErr, suffix)
				}
			default:
				t.Fatalf("unknown TS error text %q", tsErr)
			}
		})
	}
}

func TestParseToolArgumentsNeverFails(t *testing.T) {
	// The three invalid-tool-call-arguments cases.
	const badJSON = `{"path": "notes/report.md", "content": "He said "hi""}`

	t.Run("parses an object and treats empty input as no arguments", func(t *testing.T) {
		args, errText := llmhub.ParseToolArguments(`{"a": 1}`)
		if string(args) != `{"a":1}` || errText != "" {
			t.Fatalf("object: %q %q", args, errText)
		}
		for _, raw := range []string{"", "  ", "\u00a0", "\ufeff"} {
			args, errText := llmhub.ParseToolArguments(raw)
			if args != nil || errText != "" {
				t.Fatalf("%q: %q %q", raw, args, errText)
			}
		}
	})

	t.Run("never throws: invalid JSON and non-objects become an argumentsError", func(t *testing.T) {
		args, errText := llmhub.ParseToolArguments(badJSON)
		if args != nil || !strings.HasPrefix(errText, notValidJSONPrefix) {
			t.Fatalf("bad json: %q %q", args, errText)
		}
		for _, raw := range []string{"[1, 2]", `"text"`, "null", "12", "true"} {
			args, errText := llmhub.ParseToolArguments(raw)
			if args != nil || errText != notObjectPrefix+raw {
				t.Fatalf("%s: %q %q", raw, args, errText)
			}
		}
	})

	t.Run("quotes at most 200 characters of the raw arguments", func(t *testing.T) {
		raw := `{"content": "` + strings.Repeat("x", 1000)
		_, errText := llmhub.ParseToolArguments(raw)
		if !strings.Contains(errText, raw[:200]+ellipsis) {
			t.Fatalf("the error does not quote the first 200 characters: %q", errText)
		}
		if strings.Contains(errText, raw[:201]) {
			t.Fatalf("the error quotes more than 200 characters: %q", errText)
		}
	})

	t.Run("nesting deeper than 10000", func(t *testing.T) {
		for _, raw := range []string{
			strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
			strings.Repeat(`{"a":`, 20000) + "1" + strings.Repeat("}", 20000),
		} {
			args, errText := llmhub.ParseToolArguments(raw)
			if args != nil || !strings.HasPrefix(errText, notValidJSONPrefix) {
				t.Fatalf("deep nesting: %q %q", args, errText[:min(len(errText), 80)])
			}
		}
	})

	t.Run("one MiB of invalid input keeps a short preview", func(t *testing.T) {
		raw := `{"a":"` + strings.Repeat("y", 1<<20)
		args, errText := llmhub.ParseToolArguments(raw)
		if args != nil || !strings.HasPrefix(errText, notValidJSONPrefix) {
			t.Fatalf("1 MiB: %q", errText[:min(len(errText), 80)])
		}
		_, preview, ok := strings.Cut(errText, "): ")
		if !ok || kernel.UTF16Len(preview) > 201 {
			t.Fatalf("preview has %d units, want at most 201", kernel.UTF16Len(preview))
		}
	})

	t.Run("invalid UTF-8 bytes", func(t *testing.T) {
		// Inside a valid object: read as U+FFFD, so the arguments stay valid UTF-8.
		args, errText := llmhub.ParseToolArguments("{\"a\":\"\xff\xfe\"}")
		if errText != "" || !utf8.Valid(args) || !json.Valid(args) {
			t.Fatalf("object with invalid bytes: %q %q", args, errText)
		}
		// In text that is not JSON: the error text stays valid UTF-8.
		args, errText = llmhub.ParseToolArguments("{\"a\":\xff")
		if args != nil || !utf8.ValidString(errText) || !strings.HasPrefix(errText, notValidJSONPrefix) {
			t.Fatalf("bad text with invalid bytes: %q %q", args, errText)
		}
	})

	t.Run("a number JS reads as Infinity is still one valid object", func(t *testing.T) {
		args, errText := llmhub.ParseToolArguments(`{"a":1e999}`)
		if errText != "" || string(args) != `{"a":1e999}` {
			t.Fatalf("got %q %q", args, errText)
		}
	})
}
