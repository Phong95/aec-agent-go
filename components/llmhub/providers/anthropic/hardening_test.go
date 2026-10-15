package anthropic

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Transport hardening of the Phase 8 review (WR-01 to WR-03): the redirect policy,
// the body bounds and the request timeout of the client the provider gives the SDK.

// TestAnthropicRedirectDropsAPIKey redirects the Messages request from the 127.0.0.1
// upstream to a localhost upstream (another host name, the same machine) and requires
// the second server to receive no X-Api-Key. net/http alone drops only Authorization,
// so without the provider's redirect policy the key would reach the second host
// (WR-01). A redirect inside one host keeps the key.
func TestAnthropicRedirectDropsAPIKey(t *testing.T) {
	second := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalMessage) })
	target := strings.Replace(second.URL, "127.0.0.1", "localhost", 1) + "/v1/messages"
	first := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		if r.Path == "/same-host/v1/messages" {
			return llmtest.Reply{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": "/landed"}}
		}
		if r.Path == "/landed" {
			return jsonReply(minimalMessage)
		}
		return llmtest.Reply{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": target}}
	})

	// Does localhost reach the machine the loopback listener is on?
	probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.Replace(second.URL, "127.0.0.1", "localhost", 1)+"/probe", nil)
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	resp, err := first.Client().Do(probe)
	if err != nil {
		t.Skipf("localhost does not reach the loopback listener on this host: %v", err)
	}
	_ = resp.Body.Close()

	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}
	p, err := New(llmhub.ProviderConfig{APIKey: "sk-secret", BaseURL: first.URL}, WithHTTPClient(first.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := first.Requests()[0].Header.Get("X-Api-Key"); got != "sk-secret" {
		t.Errorf("the first server got X-Api-Key %q", got)
	}
	reqs := second.Requests()
	if len(reqs) != 2 { // the probe and the redirected request
		t.Fatalf("the second server saw %d requests, want 2", len(reqs))
	}
	if got := reqs[1].Header.Get("X-Api-Key"); got != "" {
		t.Errorf("the redirect to another host carried X-Api-Key %q", got)
	}
	if got := reqs[1].Header.Get("Authorization"); got != "" {
		t.Errorf("the redirect to another host carried Authorization %q", got)
	}

	same, err := New(llmhub.ProviderConfig{APIKey: "sk-secret", BaseURL: first.URL + "/same-host"}, WithHTTPClient(first.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := same.Chat(t.Context(), msgs, llmhub.ModelOptions{}); err != nil {
		t.Fatalf("Chat (same host): %v", err)
	}
	all := first.Requests()
	if got := all[len(all)-1].Header.Get("X-Api-Key"); got != "sk-secret" {
		t.Errorf("a redirect inside one host lost the key: %q", got)
	}
}

// TestAnthropicErrorBodyIsBounded serves a 2 MiB error body for Chat, Stream and
// ListModels and requires that no message is longer than the 1 MiB cap (WR-02): the
// unbounded version read all of it into memory and into the error text. The text
// put into the ModelError is cut further, to llmtransport.MaxErrorMessage bytes
// (error mapping review), so the Chat message is about 2 KB.
func TestAnthropicErrorBodyIsBounded(t *testing.T) {
	body := strings.Repeat("e", 2<<20)
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Status: http.StatusInternalServerError, Header: map[string]string{"Content-Type": "text/plain"}, Chunks: []string{body}}
	})
	p := newTestProvider(t, up)
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}
	limit := llmtransport.MaxErrorBody + len("500 ")

	_, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
	if me := failureOf(t, err); len(me.Message) > llmtransport.MaxErrorMessage+len("500 ") || len(me.Message) < llmtransport.MaxErrorMessage-8 {
		t.Errorf("Chat message is %d bytes, want about %d", len(me.Message), llmtransport.MaxErrorMessage)
	}
	chunks := drain(t, p, msgs, llmhub.ModelOptions{})
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || len(chunks[0].Error) > limit {
		t.Errorf("stream gave %d chunks; the first error is longer than %d bytes", len(chunks), limit)
	}
	_, err = p.ListModels(t.Context())
	if me := failureOf(t, err); len(me.Message) > limit {
		t.Errorf("ListModels message is %d bytes, want at most %d", len(me.Message), limit)
	}
}

// TestAnthropicResponseBodyIsBounded serves a successful non-streaming answer above
// the 32 MiB cap and requires a failure rather than an unbounded read.
func TestAnthropicResponseBodyIsBounded(t *testing.T) {
	big := `{"type":"message","padding":"` + strings.Repeat("p", llmtransport.MaxResponseBody) + `"}`
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(big) })
	p := newTestProvider(t, up)
	_, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{})
	if err == nil {
		t.Fatal("Chat accepted a body above the cap")
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the error carries the address of the upstream: %v", err)
	}
}

// TestAnthropicStreamWithUnknownContentTypeIsNotCapped serves a stream of more than
// the 32 MiB body cap whose gateway labels it application/json, and requires every
// event to arrive with no error chunk: the whole-body cap is for answers that are not
// streams, and the request says it is one (review IN-201).
func TestAnthropicStreamWithUnknownContentTypeIsNotCapped(t *testing.T) {
	const eventBytes = 2 << 20
	events := llmtransport.MaxResponseBody/eventBytes + 2
	delta := strings.Repeat("d", eventBytes)
	event := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	chunks := []string{
		event("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	}
	for range events {
		chunks = append(chunks, event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+delta+`"}}`))
	}
	chunks = append(chunks,
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`),
		event("message_stop", `{"type":"message_stop"}`))
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: chunks}
	})
	p := newTestProvider(t, up)
	var text int
	for _, c := range drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{}) {
		if c.Type == kernel.ChunkError {
			t.Fatalf("the stream failed: %.200s", c.Error)
		}
		if c.Content != nil {
			text += len(*c.Content)
		}
	}
	if text < llmtransport.MaxResponseBody {
		t.Errorf("only %d bytes of text arrived, want more than %d", text, llmtransport.MaxResponseBody)
	}
}

// modelPage is a model list page with n entries and a cursor that never ends.
func modelPage(n int) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = `{"type":"model","id":"m","display_name":"M","created_at":"2025-01-01T00:00:00Z"}`
	}
	return `{"data":[` + strings.Join(entries, ",") + `],"has_more":true,"first_id":"m","last_id":"m"}`
}

// TestAnthropicModelListIsBounded serves a model list whose has_more never clears
// and one whose single page has more than the entry cap, and requires a provider
// error from each instead of a list that grows until the context ends (WR-02).
func TestAnthropicModelListIsBounded(t *testing.T) {
	t.Run("endless cursor", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(modelPage(1)) })
		p := newTestProvider(t, up)
		_, err := p.ListModels(t.Context())
		me := failureOf(t, err)
		if me.Code != kernel.ModelCodeProvider || me.Retryable || !strings.Contains(me.Message, "pages") {
			t.Errorf("error = %+v, want a non-retryable provider error about the pages", me)
		}
		if n := len(up.Requests()); n > llmtransport.MaxModelPages+1 {
			t.Errorf("the upstream saw %d requests, want at most %d", n, llmtransport.MaxModelPages+1)
		}
	})
	t.Run("too many entries", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return jsonReply(strings.Replace(modelPage(llmtransport.MaxModels+1), `"has_more":true`, `"has_more":false`, 1))
		})
		p := newTestProvider(t, up)
		_, err := p.ListModels(t.Context())
		if me := failureOf(t, err); me.Code != kernel.ModelCodeProvider || !strings.Contains(me.Message, "entries") {
			t.Errorf("error = %+v, want a provider error about the entries", me)
		}
	})
	t.Run("a normal list is complete", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return jsonReply(strings.Replace(modelPage(3), `"has_more":true`, `"has_more":false`, 1))
		})
		models, err := newTestProvider(t, up).ListModels(t.Context())
		if err != nil || len(models) != 3 {
			t.Fatalf("ListModels gave %d models, err %v; want 3", len(models), err)
		}
	})
}

// TestAnthropicRequestTimeout holds the response headers back and requires the
// text "Request timed out." from Chat and Stream once the request timeout expires
// (WR-03), and requires the default to be 10 minutes. It is reported as a retryable
// timeout (error mapping review).
func TestAnthropicRequestTimeout(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Hang: true} })
	p := newTestProvider(t, up, WithRequestTimeout(150*time.Millisecond))
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, err := p.Chat(ctx, msgs, llmhub.ModelOptions{})
	me := failureOf(t, err)
	if me.Message != "Request timed out." || me.Code != kernel.ModelCodeTimeout || !me.Retryable {
		t.Errorf("Chat error = %+v, want the timeout text \"Request timed out.\", retryable", me)
	}
	var out []kernel.StreamChunk
	for c, err := range p.Stream(ctx, msgs, llmhub.ModelOptions{}) {
		if err != nil {
			t.Fatalf("stream yielded an error: %v", err)
		}
		out = append(out, c)
	}
	if len(out) != 1 || out[0].Type != kernel.ChunkError || out[0].Error != "Request timed out." {
		t.Errorf("stream gave %+v, want one error chunk \"Request timed out.\"", out)
	}

	d, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.timeout != 10*time.Minute {
		t.Errorf("the default request timeout is %v, want 10m", d.timeout)
	}
	z, _ := New(llmhub.ProviderConfig{APIKey: "k"}, WithRequestTimeout(0))
	if z.timeout != 0 {
		t.Errorf("WithRequestTimeout(0) left %v", z.timeout)
	}
}

// TestAnthropicCallerContextWinsOverTheTimeout gives the call a context deadline
// shorter than the request timeout and requires the bare context error, the
// llmhub.Provider contract, not "Request timed out.".
func TestAnthropicCallerContextWinsOverTheTimeout(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Hang: true} })
	p := newTestProvider(t, up, WithRequestTimeout(time.Minute))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := p.Chat(ctx, []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{})
	var me *kernel.ModelError
	if errors.As(err, &me) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Chat returned %v (%T), want the bare context deadline error", err, err)
	}
}
