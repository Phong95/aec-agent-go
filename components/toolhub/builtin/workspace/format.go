package workspace

import (
	"math"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// headTail keeps the head and the tail of a long text, because the end of a build log is
// what matters. A text of at most maxBytes UTF-8 bytes is returned as it
// is. Longer text is cut on its UTF-8 BYTES: the first third (the head, floor(max/3)
// bytes) and the last two thirds (max minus the head) are kept, with a marker between
// them that says how much was left out, and the bool is true.
//
// The cuts land on byte offsets, so they can fall inside a character. Each part is
// decoded the way Node decodes a Buffer (jsstr.DecodeUTF8): one U+FFFD for each maximal
// ill-formed piece, which is not what strings.ToValidUTF8 gives.
func headTail(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	head := maxBytes / 3
	tail := maxBytes - head
	omitted := len(text) - head - tail
	return jsstr.DecodeUTF8([]byte(text[:head])) + "\n" + ellipsis + " [" + toolkit.FormatBytes(int64(omitted)) + " omitted] " +
		ellipsis + "\n" + jsstr.DecodeUTF8([]byte(text[len(text)-tail:])), true
}

// formatExecResult is a command result for the model: the line
// "$ <label>", a status line, then the bounded stdout and stderr. A non-zero exit, a
// timeout and a cancel are error results that are NEVER retried by the harness, because
// running a command again automatically could repeat its side effects.
//
// The status line joins its parts with " . " (a middle dot): "exit code N", or "timed
// out after S s (killed)" or "cancelled", then the duration with one decimal ("1.2 s"),
// then a note when the sandbox stopped the command at its output limit. Of the display
// budget stdout gets 70% (rounded up) and stderr 30% (rounded down). The numbers are
// formatted the JS way: jsstr.Round for the timeout seconds and jsstr.ToFixed for the
// duration, whose exact ties round up.
//
// The metadata keys are, in this order, exitCode (null when the process was killed),
// timedOut, cancelled, truncated, durationMs and displayTruncated.
func formatExecResult(label string, result sandboxhub.ExecResult, timeoutMs, maxDisplayBytes int) toolhub.Output {
	var first string
	switch {
	case result.TimedOut:
		first = "timed out after " + strconv.FormatFloat(jsstr.Round(float64(timeoutMs)/1000), 'f', -1, 64) + " s (killed)"
	case result.Cancelled:
		first = "cancelled"
	default:
		// A process with no exit code (killed) is printed as "null".
		code := "null"
		if result.ExitCode != nil {
			code = strconv.Itoa(*result.ExitCode)
		}
		first = "exit code " + code
	}
	status := []string{first, jsstr.ToFixed(float64(result.DurationMs)/1000, 1) + " s"}
	if result.Truncated {
		status = append(status, "output limit reached "+emDash+" the command was stopped")
	}

	stdout, stdoutCut := headTail(result.Stdout, int(math.Ceil(float64(maxDisplayBytes)*0.7)))
	stderr, stderrCut := headTail(result.Stderr, int(math.Floor(float64(maxDisplayBytes)*0.3)))
	sections := []string{"$ " + label, strings.Join(status, " "+midDot+" ")}
	if result.Stdout != "" {
		sections = append(sections, "stdout:\n"+stdout)
	}
	if result.Stderr != "" {
		sections = append(sections, "stderr:\n"+stderr)
	}
	if result.Stdout == "" && result.Stderr == "" {
		sections = append(sections, "(no output)")
	}

	meta := ordered.NewObject()
	if result.ExitCode != nil {
		meta.Set("exitCode", *result.ExitCode)
	} else {
		meta.Set("exitCode", nil)
	}
	meta.Set("timedOut", result.TimedOut)
	meta.Set("cancelled", result.Cancelled)
	meta.Set("truncated", result.Truncated)
	meta.Set("durationMs", result.DurationMs)
	meta.Set("displayTruncated", stdoutCut || stderrCut)

	content := strings.Join(sections, "\n")
	ok := result.ExitCode != nil && *result.ExitCode == 0 && !result.TimedOut && !result.Cancelled
	if ok {
		out := toolkit.Result(content, meta)
		// A-103: printed nothing - a tool job's run is silent (nothing delivered).
		out.Empty = result.Stdout == "" && result.Stderr == ""
		return out
	}
	return toolkit.Failure(content, false, meta)
}

// timeoutFrom is timeout_seconds as milliseconds, bounded by the tool options and by the
// sandbox. A missing, non-positive or non-finite value means the
// default timeout. The result is at least 1 ms and is floored.
//
// A sandbox limit that is not positive means "the sandbox sets no cap" (review WR-B05):
// a View that leaves MaxExecTimeoutMs at its zero value would otherwise turn every exec
// into a 1 ms timeout. The tool options still bound the request.
func timeoutFrom(a toolkit.Args, o resolved, env sandboxhub.View) int {
	requested := float64(o.defaultTimeoutMs)
	if seconds, ok := a.Number("timeout_seconds"); ok && seconds > 0 {
		requested = seconds * 1000
	}
	bounded := math.Min(requested, float64(o.maxTimeoutMs))
	if sbMax := float64(env.Limits().MaxExecTimeoutMs); sbMax > 0 {
		bounded = math.Min(bounded, sbMax)
	}
	return int(math.Max(1, math.Floor(bounded)))
}

// execOutputLimit is the output byte limit passed to View.Exec: the sandbox's
// MaxExecOutputBytes, or, when the sandbox leaves it at zero (no limit declared, Go only;
// review WR-B05), the display limit of the tool, so a zero value does not capture nothing.
func execOutputLimit(o resolved, env sandboxhub.View) int {
	if n := env.Limits().MaxExecOutputBytes; n > 0 {
		return n
	}
	return o.maxDisplayOutputBytes
}
