package agentloop

import (
	"crypto/sha256"
	"math"
	"sync"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The Counter constants. The ratio is tuned for mixed English,
// code and JSON; Calibrate refines it at runtime from real provider usage.
const (
	defaultCharsPerToken  = 4.0
	minCharsPerToken      = 2.0
	maxCharsPerToken      = 8.0
	maxCalibrationSamples = 20

	// maxCounterCacheEntries bounds the count cache of a long-lived Loop: when it is
	// full it is dropped and refilled (RESEARCH A6). Nothing but a count is kept per
	// entry, so a dropped entry costs one recount.
	maxCounterCacheEntries = 8192
)

// Counter estimates the tokens of a message with three layers of accuracy:
//
//  1. a cache hit: a message counted before returns its count at once;
//  2. a calibrated estimate: after every model response Calibrate feeds the real
//     input token count back, and a moving average tunes the chars-per-token
//     ratio so the estimates converge to the provider's tokenizer;
//  3. the character ratio: max(1, ceil(length / charsPerToken)).
//
// The length is counted in UTF-16 code units, over the content plus the JSON of
// the tool calls and the tool results.
//
// The cache is keyed by a sha256 digest of the message and is never invalidated
// after Calibrate (CONTEXT "Counter cache"), so a message keeps the count it had
// when it was first seen. A mutex guards the state, because one Loop can serve
// concurrent runs. The zero value is not usable; create one with NewCounter.
type Counter struct {
	mu            sync.Mutex
	charsPerToken float64
	samples       int
	cache         map[[sha256.Size]byte]int
}

// NewCounter returns a Counter at the default ratio of 4 characters per token
// with no samples.
func NewCounter() *Counter {
	return &Counter{
		charsPerToken: defaultCharsPerToken,
		cache:         make(map[[sha256.Size]byte]int),
	}
}

// Seed restores a saved calibration so the counter does not start cold:
// the ratio is clamped to 2..8 and the sample count to 0..20.
func (c *Counter) Seed(charsPerToken float64, samples int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.charsPerToken = max(minCharsPerToken, min(maxCharsPerToken, charsPerToken))
	c.samples = max(0, min(maxCalibrationSamples, samples))
}

// Calibration returns the current calibration, for saving on the session.
func (c *Counter) Calibration() kernel.CounterCalibration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return kernel.CounterCalibration{CharsPerToken: c.charsPerToken, Samples: c.samples}
}

// Calibrate refines the ratio from the usage a model reported for msgs, the
// messages that were sent. It does nothing when the input token
// count is not positive, when there are no messages or when they have no
// characters. The ratio moves by an exponential moving average whose weight is
// one over the sample count, so early samples count more; the count stops at 20 so
// the ratio can still follow a change of model. The result is clamped to 2..8.
//
// Each product is converted to float64 on its own. Go may fuse x*y + z into one
// FMA instruction on some architectures, which would change the last bit of the
// ratio that is persisted on the session; the explicit conversion forbids the
// fusion (the Go spec: an explicit conversion rounds to the type), so the result
// is the same on every architecture.
//
// The cache is deliberately not invalidated: counts taken before a calibration
// stay as they are.
//
// A-104 audit: tools are the tool definitions sent with msgs. The provider's input
// tokens include them, so their characters are counted too (toolDefinitionChars) -
// without them the ratio collapsed to the floor (2) and every estimate came out about
// 2x too high in a chat with 22 tools.
func (c *Counter) Calibrate(msgs []kernel.Message, usage kernel.TokenUsage, tools ...toolhub.Definition) {
	// A-165: the images' estimated tokens are taken out first, so they do not skew the text ratio.
	actual := usage.InputTokens
	for _, m := range msgs {
		actual -= m.ImageCount() * kernel.ImageTokenEstimate // Update (A-180): tool results' images too
	}
	if actual <= 0 || len(msgs) == 0 {
		return
	}
	total := 0
	for _, m := range msgs {
		_, _, n := measure(m)
		total += n
	}
	for _, d := range tools {
		total += toolDefinitionChars(d)
	}
	if total == 0 {
		return
	}
	observed := float64(total) / float64(actual)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.samples < maxCalibrationSamples {
		c.samples++
	}
	weight := 1 / float64(c.samples)
	next := float64(c.charsPerToken*(1-weight)) + float64(observed*weight)
	c.charsPerToken = max(minCharsPerToken, min(maxCharsPerToken, next))
}

// Count returns the estimated token count of m (the per-message callback the
// Compactor takes). A cached count is returned as it
// is; otherwise the estimate is computed with the current ratio and cached.
func (c *Counter) Count(m kernel.Message) int {
	digest, length := measureDigest(m)

	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.cache[digest]; ok {
		return n
	}
	n := estimateTokens(length, c.charsPerToken) + m.ImageCount()*kernel.ImageTokenEstimate // A-165, A-180
	if len(c.cache) >= maxCounterCacheEntries {
		clear(c.cache)
	}
	c.cache[digest] = n
	return n
}

// countFresh estimates the tokens of a message that exists for one call only,
// with the current ratio, and neither reads nor fills the cache. Such a message (the
// tool list as a system message, the memory block, the system message that memory
// injection rewrote) is new on every call: it is never a cache hit, so the estimate
// always uses the ratio of the moment. Count, which keeps the count a
// message had when it was first seen, is for the messages of the conversation.
func (c *Counter) countFresh(m kernel.Message) int {
	_, _, length := measure(m)
	c.mu.Lock()
	defer c.mu.Unlock()
	return estimateTokens(length, c.charsPerToken) + m.ImageCount()*kernel.ImageTokenEstimate // A-165, A-180
}

// estimateTokens is max(1, ceil(length / charsPerToken)).
func estimateTokens(length int, charsPerToken float64) int {
	return max(1, int(math.Ceil(float64(length)/charsPerToken)))
}

// measure returns the JSON of the tool calls and of the tool results of m (empty
// when the list is nil) and the character length of m in UTF-16 code units: the
// content plus the JSON of each list that is present. A list that is present but
// empty adds the two characters of "[]", while
// a nil one adds nothing, which llmhub.EstimateTokens already follows. A list that
// cannot be encoded adds nothing: an estimate has no error to return.
func measure(m kernel.Message) (calls, results string, length int) {
	length = kernel.UTF16Len(m.Content)
	if m.ToolCalls != nil {
		if b, err := kernel.EncodeJSON(m.ToolCalls); err == nil {
			calls = string(b)
			length += kernel.UTF16Len(calls)
		}
	}
	if m.ToolResults != nil {
		// A-180: without their images, which count as kernel.ImageTokenEstimate each.
		if b, err := kernel.EncodeJSON(kernel.WithoutImages(m.ToolResults)); err == nil {
			results = string(b)
			length += kernel.UTF16Len(results)
		}
	}
	return calls, results, length
}

// measureDigest is measure plus the cache key of m: a sha256 over the role, the
// content, the tool call JSON and the tool result JSON, each followed by a NUL
// byte so that no two parts can run into each other. A digest, not the text, is
// the key so that a large tool output is never retained by the cache.
func measureDigest(m kernel.Message) (digest [sha256.Size]byte, length int) {
	calls, results, length := measure(m)
	h := sha256.New()
	for _, part := range []string{string(m.Role), m.Content, calls, results} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	// A-165: the images count too (their number changes the estimate).
	// Update (A-180): and the tool results' images.
	for _, img := range append(m.Images(), m.ToolResultImages()...) {
		h.Write([]byte(img.Data))
		h.Write([]byte{0})
	}
	h.Sum(digest[:0])
	return digest, length
}

// toolDefinitionChars (A-104) is the characters a tool definition costs: its name,
// description and parameters as JSON, in UTF-16 code units like every other length the
// Counter measures (the TS SDK's toolDefinitionChars).
func toolDefinitionChars(d toolhub.Definition) int {
	b, err := kernel.EncodeJSON(toolTokensShape{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	if err != nil {
		return 0
	}
	return kernel.UTF16Len(string(b))
}
