package trace

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// ConsoleSink: human-readable trace output for development
// ---------------------------------------------------------------------------

// kindLabel maps a span kind to its short console label. Kinds without an
// entry (compaction, memory-recall, sandbox-op) print the raw kind.
var kindLabel = map[SpanKind]string{
	KindRun:             "RUN",
	KindIteration:       "ITER",
	KindContextAssembly: "CTX",
	KindModelCall:       "MODEL",
	KindToolDispatch:    "TOOL",
	KindSkillDispatch:   "SKILL",
}

// ConsoleSink writes one line per span start and end in the console trace format. It is
// an opt-in development sink: it prints span attributes, which can include prompts or
// tool arguments, only to the writer the developer passes.
//
// A nil value prints null. Attributes are printed in insertion order, the
// order the span's attribute object holds them in.
type ConsoleSink struct {
	mu sync.Mutex // serialises writes so concurrent spans never interleave a line
	w  io.Writer
}

// NewConsoleSink returns a ConsoleSink writing to w (nil means os.Stdout).
func NewConsoleSink(w io.Writer) *ConsoleSink {
	if w == nil {
		w = os.Stdout
	}
	return &ConsoleSink{w: w}
}

var _ Sink = (*ConsoleSink)(nil)

// OnSpanStart prints
// `[trace] ▶ LABEL name span=<first 8 of spanId>[ parent=<first 8>][ {attrs}]`.
func (c *ConsoleSink) OnSpanStart(span Span) {
	var b strings.Builder
	b.WriteString("[trace] ▶ ")
	b.WriteString(label(span.Kind))
	b.WriteByte(' ')
	b.WriteString(span.Name)
	b.WriteString(" span=")
	b.WriteString(prefix8(span.SpanID))
	if span.ParentSpanID != "" {
		b.WriteString(" parent=")
		b.WriteString(prefix8(span.ParentSpanID))
	}
	b.WriteString(formatAttributes(span.Attributes))
	b.WriteByte('\n')
	c.write(b.String())
}

// OnSpanEnd prints `[trace] ◀ LABEL name[ <durationMs>ms][ ERROR][ {attrs}]`.
// A duration of 0 prints as 0ms.
func (c *ConsoleSink) OnSpanEnd(span Span) {
	var b strings.Builder
	b.WriteString("[trace] ◀ ")
	b.WriteString(label(span.Kind))
	b.WriteByte(' ')
	b.WriteString(span.Name)
	if span.DurationMs != nil {
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(*span.DurationMs, 10))
		b.WriteString("ms")
	}
	if span.Status == StatusError {
		b.WriteString(" ERROR")
	}
	b.WriteString(formatAttributes(span.Attributes))
	b.WriteByte('\n')
	c.write(b.String())
}

// Flush does nothing: every line is written as it happens.
func (*ConsoleSink) Flush(context.Context) error { return nil }

// write emits one whole line. A write error is ignored: tracing must never
// break the agent.
func (c *ConsoleSink) write(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = io.WriteString(c.w, line)
}

func label(kind SpanKind) string {
	if l, ok := kindLabel[kind]; ok {
		return l
	}
	return string(kind)
}

// prefix8 shortens a UUID to its first 8 bytes, or the whole string when shorter. It
// never cuts a multi-byte rune in half.
func prefix8(s string) string {
	if len(s) <= 8 {
		return s
	}
	n := 8
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// formatAttributes renders ` {k=JSON, k=JSON}` in insertion order, or "" when
// there are none. NaN and Infinity print null and -0 prints 0 (jsonx
// normalises them the way JSON.stringify does); any other value that cannot be
// encoded also prints null.
func formatAttributes(attrs ordered.Object) string {
	if attrs.Len() == 0 {
		return ""
	}
	parts := make([]string, 0, attrs.Len())
	for k, v := range attrs.All() {
		enc, err := jsonx.Marshal(v)
		if err != nil {
			enc = []byte("null")
		}
		parts = append(parts, k+"="+string(enc))
	}
	return " {" + strings.Join(parts, ", ") + "}"
}
