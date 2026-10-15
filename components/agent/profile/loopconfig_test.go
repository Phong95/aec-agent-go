package profile

import (
	"testing"
	"time"
)

func intp(v int) *int { return &v }

// The loop config cases (L1-L5) over the integer subset. NaN, Infinity and
// fractions (L6) cannot be a *int.

func TestResolveLoopConfigDefaults(t *testing.T) {
	want := ResolvedLoopConfig{MaxIterations: 100, MaxToolRetries: 2, RetryBackoffMs: 500}
	if got := ResolveLoopConfig(nil); got != want {
		t.Fatalf("ResolveLoopConfig(nil) = %+v, want %+v", got, want)
	}
	if got := ResolveLoopConfig(&LoopConfig{}); got != want {
		t.Fatalf("ResolveLoopConfig(empty) = %+v, want %+v", got, want)
	}
	if DefaultMaxIterations != 100 || DefaultMaxToolRetries != 2 || DefaultRetryBackoffMs != 500 {
		t.Fatalf("default constants = %d, %d, %d", DefaultMaxIterations, DefaultMaxToolRetries, DefaultRetryBackoffMs)
	}
}

func TestResolveLoopConfigValidValuesPassThrough(t *testing.T) {
	got := ResolveLoopConfig(&LoopConfig{MaxIterations: intp(3), MaxToolRetries: intp(0), RetryBackoffMs: intp(0)})
	want := ResolvedLoopConfig{MaxIterations: 3, MaxToolRetries: 0, RetryBackoffMs: 0}
	if got != want {
		t.Fatalf("got %+v, want %+v (0 retries and 0 backoff are valid)", got, want)
	}
}

func TestResolveLoopConfigFallsBackPerField(t *testing.T) {
	cases := []struct {
		name string
		in   LoopConfig
		want ResolvedLoopConfig
	}{
		{"maxIterations 0", LoopConfig{MaxIterations: intp(0), MaxToolRetries: intp(7)}, ResolvedLoopConfig{100, 7, 500, 0}},
		{"maxIterations -1", LoopConfig{MaxIterations: intp(-1), RetryBackoffMs: intp(40)}, ResolvedLoopConfig{100, 2, 40, 0}},
		{"maxToolRetries -1", LoopConfig{MaxToolRetries: intp(-1), MaxIterations: intp(4)}, ResolvedLoopConfig{4, 2, 500, 0}},
		{"retryBackoffMs -5", LoopConfig{RetryBackoffMs: intp(-5), MaxToolRetries: intp(1)}, ResolvedLoopConfig{100, 1, 500, 0}},
		{"all invalid", LoopConfig{MaxIterations: intp(-9), MaxToolRetries: intp(-9), RetryBackoffMs: intp(-9)}, ResolvedLoopConfig{100, 2, 500, 0}},
		{"minimums", LoopConfig{MaxIterations: intp(1), MaxToolRetries: intp(0), RetryBackoffMs: intp(0)}, ResolvedLoopConfig{1, 0, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveLoopConfig(&tc.in); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// RetryBackoff is the resolved base backoff as a time.Duration: the set value, or
// the default for a nil config, an unset field and an invalid value.
func TestLoopConfigRetryBackoff(t *testing.T) {
	ms := func(v int) *int { return &v }
	for name, tc := range map[string]struct {
		c    *LoopConfig
		want time.Duration
	}{
		"nil":     {nil, DefaultRetryBackoffMs * time.Millisecond},
		"unset":   {&LoopConfig{}, DefaultRetryBackoffMs * time.Millisecond},
		"set":     {&LoopConfig{RetryBackoffMs: ms(250)}, 250 * time.Millisecond},
		"zero":    {&LoopConfig{RetryBackoffMs: ms(0)}, 0},
		"invalid": {&LoopConfig{RetryBackoffMs: ms(-1)}, DefaultRetryBackoffMs * time.Millisecond},
	} {
		if got := tc.c.RetryBackoff(); got != tc.want {
			t.Errorf("%s: RetryBackoff() = %v, want %v", name, got, tc.want)
		}
	}
	if got := (ResolvedLoopConfig{RetryBackoffMs: 1500}).RetryBackoff(); got != 1500*time.Millisecond {
		t.Errorf("ResolvedLoopConfig.RetryBackoff() = %v, want 1.5s", got)
	}
}

// A-97: MaxConcurrentTurns defaults to 0 (no cap), keeps any value >= 0 and falls back
// to 0 for a negative one, without touching the other fields.
func TestResolveLoopConfigMaxConcurrentTurns(t *testing.T) {
	for name, tc := range map[string]struct {
		in   *int
		want int
	}{
		"unset":    {nil, 0},
		"zero":     {intp(0), 0},
		"set":      {intp(3), 3},
		"negative": {intp(-1), 0},
	} {
		got := ResolveLoopConfig(&LoopConfig{MaxConcurrentTurns: tc.in, MaxIterations: intp(4)})
		if got.MaxConcurrentTurns != tc.want || got.MaxIterations != 4 {
			t.Errorf("%s: got %+v, want MaxConcurrentTurns %d and MaxIterations 4", name, got, tc.want)
		}
	}
	if DefaultMaxConcurrentTurns != 0 {
		t.Fatalf("DefaultMaxConcurrentTurns = %d, want 0", DefaultMaxConcurrentTurns)
	}
}
