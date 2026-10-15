package memorysystem

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// idCounter is the package-level counter of the default record id: it never resets and
// is shared by every pipeline of the process, so two agents cannot mint the same id in
// the same millisecond. The counter separates ids within one process; the process tag
// (processTag) separates processes that share a store, because each process starts its
// own counter at zero.
var idCounter atomic.Int64

// processTag is 16 lowercase hex digits (8 bytes of crypto/rand), minted once per
// process on first use. It is the per-process part of a default id: two processes that
// share a memory store, with the same clock reading and the same counter value, still
// mint different ids (10-SECURITY UF-5, Phase 12 12-02).
var processTag = sync.OnceValue(func() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // Go 1.24+: Read never returns an error (it crashes the program irrecoverably instead)
	return hex.EncodeToString(b[:])
})

// defaultIDs returns the default id function: mem_<ms>_<n>, the clock in unix
// milliseconds and the counter value after incrementing it, followed by a
// per-process tag, mem_<ms>_<n>_<tag>. Ids are opaque, and every fixture replay
// passes its own WithIDs, so the tag changes no recorded behaviour.
func defaultIDs(now func() time.Time) func() string {
	return newDefaultIDs(now, processTag(), &idCounter)
}

// newDefaultIDs is defaultIDs with the process tag and the counter as parameters, so a
// test can simulate two processes.
func newDefaultIDs(now func() time.Time, tag string, counter *atomic.Int64) func() string {
	return func() string {
		return fmt.Sprintf("mem_%d_%d_%s", now().UnixMilli(), counter.Add(1), tag)
	}
}

// maxTimeMs is the largest absolute time value of a JS Date (8.64e15 ms).
const maxTimeMs = 8.64e15

// invalidTimeValue is the RangeError text of Date.prototype.toISOString on an
// invalid date.
const invalidTimeValue = "Invalid time value"

// isoFromMs is new Date(ms).toISOString() for a time value in ms. A fractional ms is
// truncated (the Date constructor does). A value outside the Date range, or NaN, is
// a range error, reported as ok == false. Years 0 to 9999 use the 24 character
// form, other years the signed six digit form.
func isoFromMs(ms float64) (string, bool) {
	if math.IsNaN(ms) || math.Abs(ms) > maxTimeMs {
		return "", false
	}
	t := time.UnixMilli(int64(math.Trunc(ms))).UTC()
	y := t.Year()
	if y >= 0 && y <= 9999 {
		return t.Format("2006-01-02T15:04:05.000Z"), true
	}
	sign := "+"
	if y < 0 {
		sign = "-"
		y = -y
	}
	return sign + fmt.Sprintf("%06d", y) + t.Format("-01-02T15:04:05.000Z"), true
}

// isoNow is new Date().toISOString() on the pipeline clock.
func isoNow(now func() time.Time) string {
	s, _ := isoFromMs(float64(now().UnixMilli()))
	return s
}

// jsNumberString is String(n) for a number parsed from JSON. For a finite value it is
// exactly what JSON.stringify prints, so the wire encoder does the formatting.
func jsNumberString(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	}
	b, err := jsonx.Marshal(f)
	if err != nil {
		return "NaN"
	}
	return string(b)
}
