package kernel

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// isoLayout is the shape of JS Date.prototype.toISOString. Go's .000 layout
// truncates (it does not round), exactly like toISOString.
const isoLayout = "2006-01-02T15:04:05.000Z"

// NewID returns a random UUIDv4 formatted 8-4-4-4-12 in lowercase hex, the
// Go counterpart of crypto.randomUUID. It uses crypto/rand, never math/rand.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])  // Go 1.24+: Read never returns an error (it crashes the program irrecoverably instead)
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10xx
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// NowISO returns the current time as an ISO string in the shape of JS
// toISOString (UTC, millisecond precision, Z suffix).
func NowISO() string {
	return formatISO(time.Now())
}

// formatISO formats t as a toISOString-shaped string in UTC.
func formatISO(t time.Time) string {
	return t.UTC().Format(isoLayout)
}

// NowMillis returns the current time as epoch milliseconds (Date.now()).
func NowMillis() int64 {
	return time.Now().UnixMilli()
}

// MillisToISO formats epoch milliseconds as the text of JS
// new Date(ms).toISOString() (UTC, millisecond precision, Z suffix). The hubs
// use it to stamp records from an injectable epoch-ms clock.
func MillisToISO(ms int64) string {
	return formatISO(time.UnixMilli(ms))
}
