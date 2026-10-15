package kernel

import (
	"testing"
	"time"
)

// The epoch-ms and ISO timestamp fields read back as time.Time values.
func TestTimeAccessors(t *testing.T) {
	at := time.Date(2026, time.October, 1, 8, 0, 0, 5_000_000, time.UTC)
	ms := at.UnixMilli()
	if got := (Event{Timestamp: ms}).Time(); !got.Equal(at) {
		t.Errorf("Event.Time() = %v, want %v", got, at)
	}
	s := Session{CreatedAt: ms, UpdatedAt: ms + 1000}
	if got := s.Created(); !got.Equal(at) {
		t.Errorf("Session.Created() = %v, want %v", got, at)
	}
	if got := s.Updated(); !got.Equal(at.Add(time.Second)) {
		t.Errorf("Session.Updated() = %v, want %v", got, at.Add(time.Second))
	}
	if got, ok := (Message{Timestamp: MillisToISO(ms)}).Time(); !ok || !got.Equal(at) || got.Location() != time.UTC {
		t.Errorf("Message.Time() = %v, %v, want %v in UTC", got, ok, at)
	}
	for _, ts := range []string{"", "yesterday"} {
		if got, ok := (Message{Timestamp: ts}).Time(); ok || !got.IsZero() {
			t.Errorf("Message{Timestamp: %q}.Time() = %v, %v, want the zero time and false", ts, got, ok)
		}
	}
}
