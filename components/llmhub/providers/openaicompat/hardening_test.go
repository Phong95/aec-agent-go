package openaicompat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Transport hardening of the Phase 8 review (WR-01 to WR-03): the redirect policy,
// the body bounds and the request timeout of the client the provider gives the SDK.

// TestOpenAICompatRedirectDropsCredentialHeaders redirects the chat request from the
// 127.0.0.1 upstream to a localhost upstream (another host name, the same machine)
// and requires the second server to receive none of Authorization,
// OpenAI-Organization and OpenAI-Project (WR-01). net/http drops only Authorization;
// the other two are added to the call as an operator's gateway or the SDK could add
// them, through the provider's own service so the provider's client is the one tested.
// The openai SDK (v3.68) already refuses a redirect to another origin, so the call
// fails and the second server sees only the probe; should a later SDK follow the
// redirect, the provider's redirect policy must still strip the headers (the policy
// itself is proven in internal/llmtransport). A redirect inside one host keeps them.
func TestOpenAICompatRedirectDropsCredentialHeaders(t *testing.T) {
	second := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalCompletion) })
	target := strings.Replace(second.URL, "127.0.0.1", "localhost", 1) + "/v1/chat/completions"
	first := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		if r.Path == "/same-host/chat/completions" {
			return llmtest.Reply{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": "/landed"}}
		}
		if r.Path == "/landed" {
			return jsonReply(minimalCompletion)
		}
		return llmtest.Reply{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": target}}
	})

	probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.Replace(second.URL, "127.0.0.1", "localhost", 1)+"/probe", nil)
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	resp, err := first.Client().Do(probe)
	if err != nil {
		t.Skipf("localhost does not reach the loopback listener on this host: %v", err)
	}
	_ = resp.Body.Close()

	params := sdk.ChatCompletionNewParams{
		Model:    "m",
		Messages: []sdk.ChatCompletionMessageParamUnion{sdk.UserMessage("hello")},
	}
	extra := []option.RequestOption{
		option.WithHeader("OpenAI-Organization", "org-secret"),
		option.WithHeader("OpenAI-Project", "proj-secret"),
	}
	p, err := New(llmhub.ProviderConfig{APIKey: "sk-secret", BaseURL: first.URL + "/v1"}, WithHTTPClient(first.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _ = p.chat.New(t.Context(), params, extra...) // refused or followed: only what the second server saw matters
	if got := first.Requests()[0].Header.Get("OpenAI-Organization"); got != "org-secret" {
		t.Errorf("the first server got OpenAI-Organization %q", got)
	}
	for i, r := range second.Requests()[1:] { // [0] is the probe
		for _, h := range []string{"Authorization", "OpenAI-Organization", "OpenAI-Project"} {
			if v := r.Header.Get(h); v != "" {
				t.Errorf("redirected request %d to another host carried %s: %q", i, h, v)
			}
		}
	}

	same, err := New(llmhub.ProviderConfig{APIKey: "sk-secret", BaseURL: first.URL + "/same-host"}, WithHTTPClient(first.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := same.chat.New(t.Context(), params, extra...); err != nil {
		t.Fatalf("chat (same host): %v", err)
	}
	all := first.Requests()
	last := all[len(all)-1]
	if last.Header.Get("Authorization") != "Bearer sk-secret" || last.Header.Get("OpenAI-Organization") != "org-secret" {
		t.Errorf("a redirect inside one host lost the credentials: %v", last.Header)
	}
}

// TestOpenAICompatErrorBodyIsBounded serves a 2 MiB error body for Chat, Stream and
// ListModels and requires that no message is longer than the 1 MiB cap (WR-02). The
// text put into the ModelError is cut further, to llmtransport.MaxErrorMessage
// bytes (error mapping review), so the Chat message is about 2 KB.
func TestOpenAICompatErrorBodyIsBounded(t *testing.T) {
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

// TestOpenAICompatResponseBodyIsBounded serves a successful non-streaming answer
// above the 32 MiB cap and requires a failure rather than an unbounded read.
func TestOpenAICompatResponseBodyIsBounded(t *testing.T) {
	big := `{"object":"chat.completion","padding":"` + strings.Repeat("p", llmtransport.MaxResponseBody) + `"}`
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(big) })
	p := newTestProvider(t, up)
	if _, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{}); err == nil {
		t.Fatal("Chat accepted a body above the cap")
	}
}

// TestOpenAICompatStreamWithUnknownContentTypeIsNotCapped serves a stream of more
// than the 32 MiB body cap whose gateway labels it application/json, and requires
// every event to arrive with no error chunk: the whole-body cap is for answers that
// are not streams, and the request says it is one (review IN-201).
func TestOpenAICompatStreamWithUnknownContentTypeIsNotCapped(t *testing.T) {
	const eventBytes = 2 << 20
	events := llmtransport.MaxResponseBody/eventBytes + 2
	delta := strings.Repeat("d", eventBytes)
	chunks := make([]string, 0, events+2)
	for range events {
		chunks = append(chunks, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"`+delta+`"},"finish_reason":null}]}`+"\n\n")
	}
	chunks = append(chunks,
		`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
		"data: [DONE]\n\n")
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

// modelPage is a model list page with n entries.
func modelPage(n int) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = `{"id":"m","object":"model","created":1,"owned_by":"x"}`
	}
	return `{"object":"list","data":[` + strings.Join(entries, ",") + `],"has_more":true,"first_id":"m","last_id":"m"}`
}

// TestOpenAICompatModelListIsBounded serves a model list with more than the entry cap
// and requires a provider error, and requires that a cursor the server advertises is
// not followed (the SDK page has no next page): one request, never an endless loop
// (WR-02).
func TestOpenAICompatModelListIsBounded(t *testing.T) {
	t.Run("too many entries", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return jsonReply(modelPage(llmtransport.MaxModels + 1))
		})
		p := newTestProvider(t, up)
		_, err := p.ListModels(t.Context())
		if me := failureOf(t, err); me.Code != kernel.ModelCodeProvider || me.Retryable || !strings.Contains(me.Message, "entries") {
			t.Errorf("error = %+v, want a non-retryable provider error about the entries", me)
		}
	})
	t.Run("a cursor is not followed", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(modelPage(3)) })
		models, err := newTestProvider(t, up).ListModels(t.Context())
		if err != nil || len(models) != 3 {
			t.Fatalf("ListModels gave %d models, err %v; want 3", len(models), err)
		}
		if n := len(up.Requests()); n != 1 {
			t.Errorf("the upstream saw %d requests, want 1", n)
		}
	})
	t.Run("a body above the cap fails", func(t *testing.T) {
		big := `{"object":"list","data":[],"padding":"` + strings.Repeat("p", llmtransport.MaxResponseBody) + `"}`
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(big) })
		if _, err := newTestProvider(t, up).ListModels(t.Context()); err == nil {
			t.Fatal("ListModels accepted a body above the cap")
		}
	})
}

// TestOpenAICompatRequestTimeout holds the response headers back and requires the
// text "Request timed out." from Chat and Stream once the request timeout expires
// (WR-03), and requires the default to be 10 minutes. It is reported as a retryable
// timeout (error mapping review).
func TestOpenAICompatRequestTimeout(t *testing.T) {
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

// TestOpenAICompatCallerContextWinsOverTheTimeout gives the call a context deadline
// shorter than the request timeout and requires the bare context error, the
// llmhub.Provider contract, not "Request timed out.".
func TestOpenAICompatCallerContextWinsOverTheTimeout(t *testing.T) {
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
