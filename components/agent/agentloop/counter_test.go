package agentloop_test

import (
	"math"
	"strconv"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// counterFixture is the recorded fixture agentloop-counter.json.
type counterFixture struct {
	Counts []struct {
		Name    string         `json:"name"`
		Message kernel.Message `json:"message"`
		Tokens  int            `json:"tokens"`
	} `json:"counts"`
	Calibrations []struct {
		Name  string                     `json:"name"`
		Seed  *kernel.CounterCalibration `json:"seed"`
		Steps []struct {
			Messages    []kernel.Message          `json:"messages"`
			Usage       kernel.TokenUsage         `json:"usage"`
			Calibration kernel.CounterCalibration `json:"calibration"`
			Probe       struct {
				Message kernel.Message `json:"message"`
				Tokens  int            `json:"tokens"`
			} `json:"probe"`
		} `json:"steps"`
	} `json:"calibrations"`
	Seeds []struct {
		CharsPerToken float64                   `json:"charsPerToken"`
		Samples       int                       `json:"samples"`
		Calibration   kernel.CounterCalibration `json:"calibration"`
	} `json:"seeds"`
}

// sameCalibration compares the ratio bit for bit: the ratio is persisted on the
// session, and a difference in the last bit would be a visible drift.
func sameCalibration(got, want kernel.CounterCalibration) bool {
	return math.Float64bits(got.CharsPerToken) == math.Float64bits(want.CharsPerToken) && got.Samples == want.Samples
}

// TestCounterMatchesGolden replays agentloop-counter.json: the counts of a fresh
// counter, scripts of calibrate calls with the calibration after each (float64 bits
// and sample counts) and a probe count at the new ratio, and seed clamping.
func TestCounterMatchesGolden(t *testing.T) {
	var f counterFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "agentloop-counter.json"), &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Counts) < 10 || len(f.Calibrations) < 3 || len(f.Seeds) < 5 {
		t.Fatalf("the corpus is thin: %d counts, %d calibration scripts, %d seeds", len(f.Counts), len(f.Calibrations), len(f.Seeds))
	}

	t.Run("counts", func(t *testing.T) {
		for _, e := range f.Counts {
			if got := agentloop.NewCounter().Count(e.Message); got != e.Tokens {
				t.Errorf("%s: Count = %d, TS countTokens = %d", e.Name, got, e.Tokens)
			}
		}
	})

	t.Run("calibrations", func(t *testing.T) {
		for _, script := range f.Calibrations {
			c := agentloop.NewCounter()
			if script.Seed != nil {
				c.Seed(script.Seed.CharsPerToken, script.Seed.Samples)
			}
			for i, step := range script.Steps {
				c.Calibrate(step.Messages, step.Usage)
				if got := c.Calibration(); !sameCalibration(got, step.Calibration) {
					t.Fatalf("%s step %d: calibration = %+v (bits %x), TS = %+v (bits %x)", script.Name, i, got, math.Float64bits(got.CharsPerToken), step.Calibration, math.Float64bits(step.Calibration.CharsPerToken))
				}
				if got := c.Count(step.Probe.Message); got != step.Probe.Tokens {
					t.Fatalf("%s step %d: probe count = %d, TS = %d", script.Name, i, got, step.Probe.Tokens)
				}
			}
		}
	})

	t.Run("seeds", func(t *testing.T) {
		for _, s := range f.Seeds {
			c := agentloop.NewCounter()
			c.Seed(s.CharsPerToken, s.Samples)
			if got := c.Calibration(); !sameCalibration(got, s.Calibration) {
				t.Errorf("Seed(%v, %d): calibration = %+v, TS = %+v", s.CharsPerToken, s.Samples, got, s.Calibration)
			}
		}
	})
}

// A fresh counter starts at four characters per token with no samples.
func TestCounterStartsCold(t *testing.T) {
	c := agentloop.NewCounter()
	if got := c.Calibration(); got.CharsPerToken != 4 || got.Samples != 0 {
		t.Fatalf("a new counter calibration = %+v, want 4 and 0", got)
	}
	if got := c.Count(kernel.Message{Role: kernel.RoleUser, Content: ""}); got != 1 {
		t.Fatalf("an empty message counts %d, want the minimum of 1", got)
	}
}

// The cache is keyed by a digest of the message, so equal messages share an entry
// however they were built, messages that differ in a tool part do not, a count is
// never invalidated by Calibrate, and the cache
// stays bounded.
func TestCounterCacheIsBoundedAndKeyedByDigest(t *testing.T) {
	c := agentloop.NewCounter()
	a := kernel.Message{Role: kernel.RoleUser, Content: "abcdefgh"} // 2 tokens at 4 chars per token
	if got := c.Count(a); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}
	// Calibrate to 2 chars per token: an uncached 8-character message would now cost 4.
	c.Calibrate([]kernel.Message{{Role: kernel.RoleUser, Content: "xx"}}, kernel.TokenUsage{InputTokens: 1000})
	if cal := c.Calibration(); cal.CharsPerToken != 2 {
		t.Fatalf("setup: ratio %v, want the clamped 2", cal.CharsPerToken)
	}
	if got := c.Count(kernel.Message{Role: kernel.RoleUser, Content: "abcdefgh"}); got != 2 {
		t.Fatalf("an equal message built elsewhere counts %d, want the cached 2: a calibration must not invalidate the cache", got)
	}
	if got := c.Count(kernel.Message{Role: kernel.RoleUser, Content: "abcdefgi"}); got != 4 {
		t.Fatalf("a different message counts %d, want a fresh estimate of 4 at the new ratio", got)
	}
	if got := agentloop.CounterCacheLen(c); got != 2 {
		t.Fatalf("cache holds %d entries, want 2 (one per distinct message)", got)
	}

	// The same content with another role, or with a tool part, is another message.
	asAssistant := kernel.Message{Role: kernel.RoleAssistant, Content: "abcdefgh"}
	withCalls := kernel.Message{Role: kernel.RoleUser, Content: "abcdefgh", ToolCalls: []kernel.ToolCall{}}
	withResults := kernel.Message{Role: kernel.RoleUser, Content: "abcdefgh", ToolResults: []kernel.ToolResult{}}
	for _, m := range []kernel.Message{asAssistant, withCalls, withResults} {
		c.Count(m)
	}
	if got := agentloop.CounterCacheLen(c); got != 5 {
		t.Fatalf("cache holds %d entries, want 5: role and tool parts are part of the key", got)
	}
	// An empty but present list adds the two characters of "[]":
	// ten characters at two per token is 5, where the plain message is cached at 2.
	for name, m := range map[string]kernel.Message{"tool calls": withCalls, "tool results": withResults} {
		if got := c.Count(m); got != 5 {
			t.Fatalf("an empty %s list counts %d, want 5", name, got)
		}
	}

	// Bounded: far more distinct messages than the cap never grow the cache past it.
	big := agentloop.NewCounter()
	const distinct = 20000
	maxSeen := 0
	for i := range distinct {
		big.Count(kernel.Message{Role: kernel.RoleUser, Content: "message " + strconv.Itoa(i)})
		maxSeen = max(maxSeen, agentloop.CounterCacheLen(big))
	}
	if maxSeen > agentloop.CounterCacheCap {
		t.Fatalf("the cache grew to %d entries, cap %d", maxSeen, agentloop.CounterCacheCap)
	}
	if maxSeen < agentloop.CounterCacheCap {
		t.Fatalf("the cache never filled (max %d of %d): the test does not exercise the cap", maxSeen, agentloop.CounterCacheCap)
	}
}

// One Loop serves concurrent runs, so the counter must be safe to share. Run with
// -race.
func TestCounterIsSafeForConcurrentUse(t *testing.T) {
	c := agentloop.NewCounter()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 500 {
				m := kernel.Message{Role: kernel.RoleUser, Content: "goroutine " + strconv.Itoa(g) + " message " + strconv.Itoa(i%50)}
				c.Count(m)
				if i%10 == 0 {
					c.Calibrate([]kernel.Message{m}, kernel.TokenUsage{InputTokens: 7})
				}
				if i%97 == 0 {
					c.Seed(4, 3)
				}
				_ = c.Calibration()
			}
		})
	}
	wg.Wait()
	if cal := c.Calibration(); cal.CharsPerToken < 2 || cal.CharsPerToken > 8 || cal.Samples < 0 || cal.Samples > 20 {
		t.Fatalf("calibration left its bounds: %+v", cal)
	}
}
