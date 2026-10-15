package kernel

import (
	"regexp"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		id := NewID()
		if !re.MatchString(id) {
			t.Fatalf("NewID() = %q does not match the UUIDv4 pattern", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID() returned a duplicate: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNowISO(t *testing.T) {
	re := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	if got := NowISO(); !re.MatchString(got) {
		t.Fatalf("NowISO() = %q does not match the toISOString shape", got)
	}
}

// toISOString truncates milliseconds, it does not round them.
func TestFormatISO(t *testing.T) {
	at := time.Date(2026, time.October, 1, 8, 0, 0, 999_900_000, time.UTC)
	if got, want := formatISO(at), "2026-10-01T08:00:00.999Z"; got != want {
		t.Fatalf("formatISO = %q, want %q", got, want)
	}
	// A non-UTC time is converted to UTC.
	local := time.Date(2026, time.October, 1, 10, 0, 0, 5_000_000, time.FixedZone("x", 2*60*60))
	if got, want := formatISO(local), "2026-10-01T08:00:00.005Z"; got != want {
		t.Fatalf("formatISO(local) = %q, want %q", got, want)
	}
}

func TestNowMillis(t *testing.T) {
	before := time.Now().UnixMilli()
	got := NowMillis()
	if got < before || got > time.Now().UnixMilli() {
		t.Fatalf("NowMillis() = %d outside [%d, now]", got, before)
	}
}

func TestMillisToISO(t *testing.T) {
	if got, want := MillisToISO(1700000000000), "2023-11-14T22:13:20.000Z"; got != want {
		t.Fatalf("MillisToISO(1700000000000) = %q, want %q", got, want)
	}
	if got, want := MillisToISO(1700000000123), "2023-11-14T22:13:20.123Z"; got != want {
		t.Fatalf("MillisToISO(1700000000123) = %q, want %q", got, want)
	}
	if got, want := MillisToISO(0), "1970-01-01T00:00:00.000Z"; got != want {
		t.Fatalf("MillisToISO(0) = %q, want %q", got, want)
	}
}
