package workspace

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// builtin-workspace-format.json is recorded by wiregen from reference formatBytes,
// headTail and formatExecResult helpers, called directly (see
// tools/wiregen/builtin/workspace.cases.mjs). The inputs reach the tricky places of the
// formatting: toFixed ties, Math.round of the timeout seconds, and a head or tail
// cut that lands inside a euro sign, an emoji or a combining sequence.

type formatFixture struct {
	FormatBytes [][2]json.RawMessage `json:"formatBytes"`
	HeadTail    []struct {
		Text     string `json:"text"`
		MaxBytes int    `json:"maxBytes"`
		Result   struct {
			Text string `json:"text"`
			Cut  bool   `json:"cut"`
		} `json:"result"`
	} `json:"headTail"`
	FormatExecResult []struct {
		Label           string                `json:"label"`
		Result          sandboxhub.ExecResult `json:"result"`
		TimeoutMs       int                   `json:"timeoutMs"`
		MaxDisplayBytes int                   `json:"maxDisplayBytes"`
		Output          json.RawMessage       `json:"output"`
	} `json:"formatExecResult"`
}

func loadFormatFixture(t *testing.T) formatFixture {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-workspace-format.json")))
	dec.DisallowUnknownFields()
	var fx formatFixture
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-workspace-format: %v", err)
	}
	return fx
}

// TestFormatBytesMatchesGolden replays the formatBytes rows, including the exact ties
// (1280 bytes is "1.3 KB") and the 1 MB boundary (1048575 bytes is "1024.0 KB").
func TestFormatBytesMatchesGolden(t *testing.T) {
	fx := loadFormatFixture(t)
	if len(fx.FormatBytes) < 25 {
		t.Fatalf("the corpus holds %d formatBytes rows, want at least 25", len(fx.FormatBytes))
	}
	for _, row := range fx.FormatBytes {
		var n float64
		var want string
		if err := json.Unmarshal(row[0], &n); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(row[1], &want); err != nil {
			t.Fatal(err)
		}
		if n != math.Trunc(n) {
			t.Fatalf("row %v is not a whole byte count", n)
		}
		if got := toolkit.FormatBytes(int64(n)); got != want {
			t.Errorf("FormatBytes(%v) = %q, want %q", n, got, want)
		}
	}
}

// TestHeadTailMatchesGolden replays the headTail rows: the 1/3 head and 2/3 tail cut on
// UTF-8 bytes, the omitted-bytes marker, and Node's per-subpart U+FFFD where a cut lands
// inside a character.
func TestHeadTailMatchesGolden(t *testing.T) {
	fx := loadFormatFixture(t)
	if len(fx.HeadTail) < 25 {
		t.Fatalf("the corpus holds %d headTail rows, want at least 25", len(fx.HeadTail))
	}
	var cuts, replaced int
	for _, row := range fx.HeadTail {
		got, cut := headTail(row.Text, row.MaxBytes)
		if got != row.Result.Text || cut != row.Result.Cut {
			t.Errorf("headTail(%.40q..., %d) = (%q, %v), want (%q, %v)", row.Text, row.MaxBytes, got, cut, row.Result.Text, row.Result.Cut)
		}
		if row.Result.Cut {
			cuts++
		}
		if strings.ContainsRune(row.Result.Text, '\ufffd') {
			replaced++
		}
	}
	if cuts == 0 || replaced == 0 {
		t.Errorf("the corpus must hold cut rows (%d) and rows with a replacement character (%d)", cuts, replaced)
	}
}

// TestFormatExecResultMatchesGolden replays the formatExecResult rows: the status line (exit
// code, timeout, cancel, null exit, output limit), the JS number formatting of the
// duration and the timeout, the 70% and 30% display budgets and the metadata.
func TestFormatExecResultMatchesGolden(t *testing.T) {
	fx := loadFormatFixture(t)
	if len(fx.FormatExecResult) < 30 {
		t.Fatalf("the corpus holds %d formatExecResult rows, want at least 30", len(fx.FormatExecResult))
	}
	var errors, timeouts, cancels, displayCut int
	for _, row := range fx.FormatExecResult {
		out := formatExecResult(row.Label, row.Result, row.TimeoutMs, row.MaxDisplayBytes)
		got, err := kernel.EncodeJSON(out)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(row.Output) {
			t.Errorf("formatExecResult(%q, %+v, %d, %d)\n got: %s\nwant: %s", row.Label, row.Result, row.TimeoutMs, row.MaxDisplayBytes, got, row.Output)
		}
		s := string(row.Output)
		if strings.Contains(s, `"isError":true`) {
			errors++
		}
		if row.Result.TimedOut {
			timeouts++
		}
		if row.Result.Cancelled {
			cancels++
		}
		if strings.Contains(s, `"displayTruncated":true`) {
			displayCut++
		}
	}
	for name, n := range map[string]int{"error": errors, "timeout": timeouts, "cancel": cancels, "display cut": displayCut} {
		if n == 0 {
			t.Errorf("the corpus has no %s row", name)
		}
	}
}

// TestFormatExecResultOutcomes states the three outcome classes in plain terms, so a
// regression of the isError rule names itself: only an exit code 0 with no timeout and
// no cancel is a success, and a failure is never retried.
func TestFormatExecResultOutcomes(t *testing.T) {
	zero, one := 0, 1
	for _, tc := range []struct {
		name   string
		result sandboxhub.ExecResult
		isErr  bool
	}{
		{"exit 0", sandboxhub.ExecResult{ExitCode: &zero}, false},
		{"exit 1", sandboxhub.ExecResult{ExitCode: &one}, true},
		{"killed with no exit code", sandboxhub.ExecResult{Truncated: true}, true},
		{"timed out", sandboxhub.ExecResult{TimedOut: true}, true},
		{"cancelled", sandboxhub.ExecResult{Cancelled: true}, true},
		{"exit 0 but timed out", sandboxhub.ExecResult{ExitCode: &zero, TimedOut: true}, true},
	} {
		out := formatExecResult("cmd", tc.result, 1000, 1000)
		if gotErr := out.IsError != nil && *out.IsError; gotErr != tc.isErr {
			t.Errorf("%s: isError = %v, want %v", tc.name, gotErr, tc.isErr)
		}
		if tc.isErr && (out.Retryable == nil || *out.Retryable) {
			t.Errorf("%s: a failed command must be marked not retryable", tc.name)
		}
		if !tc.isErr && out.Retryable != nil {
			t.Errorf("%s: a success carries no retryable flag", tc.name)
		}
	}
}
