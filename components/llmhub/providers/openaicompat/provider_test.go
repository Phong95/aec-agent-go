package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// secret is a credential placed in the base URL, in the query and in a path
// segment (a tenant or token a gateway URL can carry). ProviderConfig treats the
// query of BaseURL as secret, and an SDK error string embeds the request URL, so
// the secret must never reach an error text. The SDK drops the base URL query from
// the requests it builds, so it is the path segment that proves the check is sharp.
const secret = "SECRET-OAI"

// secretBase returns base with the secret in its path and its query.
func secretBase(base string) string {
	return base + "/tenant-" + secret + "/v1?key=" + secret
}

// leaks reports what in text must not be there: the secret, the loopback address
// the test upstream listens on, and the "POST \"<url>\"" form of an SDK error.
func leaks(text string) []string {
	var found []string
	for _, bad := range []string{secret, "127.0.0.1", `POST "`, `GET "`, "localhost"} {
		if strings.Contains(text, bad) {
			found = append(found, bad)
		}
	}
	return found
}

// TestOpenAIErrorTextNeverCarriesTheBaseURL puts a secret in the path and the query
// of the base URL and requires that no error text, for any status or for a refused
// connection, through Chat, Stream or ListModels, contains it, the address of the
// upstream or the SDK's request line (T-08-16).
func TestOpenAIErrorTextNeverCarriesTheBaseURL(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}
	check := func(t *testing.T, what, text string) {
		t.Helper()
		if text == "" {
			t.Errorf("%s: the error text is empty", what)
		}
		if found := leaks(text); len(found) > 0 {
			t.Errorf("%s: the error text %q contains %q", what, text, found)
		}
	}

	bodies := map[string]string{
		"json envelope":  `{"error":{"message":"boom","type":"x_error"}}`,
		"string error":   `{"error":"boom"}`,
		"plain text":     "boom",
		"no error key":   `{"message":"boom"}`,
		"empty response": "",
	}
	for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 503, 529} {
		for kind, body := range bodies {
			t.Run(fmt.Sprintf("status %d %s", status, kind), func(t *testing.T) {
				up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{Status: status, Header: map[string]string{"Content-Type": "application/json", "Retry-After": "7"}, Chunks: []string{body}}
				})
				p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: secretBase(up.URL)}, WithHTTPClient(up.Client()), WithMaxRetries(0))
				if err != nil {
					t.Fatalf("New: %v", err)
				}

				_, err = p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
				me := failureOf(t, err)
				check(t, "Chat", me.Message)
				// The status and the Retry-After of the response are kept on the error.
				if me.StatusCode != status || me.RetryAfter != 7*time.Second {
					t.Errorf("Chat StatusCode %d RetryAfter %v, want %d 7s", me.StatusCode, me.RetryAfter, status)
				}

				chunks := drain(t, p, msgs, llmhub.ModelOptions{})
				if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError {
					t.Fatalf("stream gave %+v, want one error chunk", chunks)
				}
				check(t, "Stream", chunks[0].Error)

				_, err = p.ListModels(t.Context())
				me = failureOf(t, err)
				check(t, "ListModels", me.Message)
				if me.StatusCode != status {
					t.Errorf("ListModels StatusCode %d, want %d", me.StatusCode, status)
				}

				// The check is only sharp when the SDK really put the secret in the URL it
				// requested, which is what its own Error() string would then print.
				reqs := up.Requests()
				if len(reqs) == 0 || !strings.Contains(reqs[0].Path, secret) {
					t.Fatalf("the secret did not reach the upstream path, so the check proves nothing: %+v", reqs)
				}
			})
		}
	}

	t.Run("in-band stream error", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return sseReply(`data: {"error":{"message":"overloaded","type":"server_error"}}` + "\n\n")
		})
		p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: secretBase(up.URL)}, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		chunks := drain(t, p, msgs, llmhub.ModelOptions{})
		if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || chunks[0].Error != "overloaded" {
			t.Fatalf("stream gave %+v, want one error chunk saying overloaded", chunks)
		}
	})

	t.Run("refused connection", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return llmtest.Reply{} })
		p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: secretBase(llmtest.ClosedURL(t))}, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
		me := failureOf(t, err)
		check(t, "Chat", me.Message)
		if me.Message != "Connection error." {
			t.Errorf("Chat text = %q, want %q", me.Message, "Connection error.")
		}
		// The transport error is the cause: reachable, but not in the text.
		var urlErr *url.Error
		if !errors.As(err, &urlErr) || me.StatusCode != 0 {
			t.Errorf("Chat error: errors.As *url.Error = %v, StatusCode %d; want the cause and no status", urlErr != nil, me.StatusCode)
		}
		chunks := drain(t, p, msgs, llmhub.ModelOptions{})
		if len(chunks) != 1 {
			t.Fatalf("stream gave %+v, want one error chunk", chunks)
		}
		check(t, "Stream", chunks[0].Error)
		_, err = p.ListModels(t.Context())
		check(t, "ListModels", failureOf(t, err).Message)
	})
}

// TestOpenAIIgnoresEnvironment sets every variable the vendor SDK reads and requires
// that they change nothing the upstream sees (T-08-15, research SDK-isolation
// decision): the explicit key is sent as the bearer token, no organisation, project
// or custom header is added from the environment, and a trap upstream named by
// OPENAI_BASE_URL is never called, neither when the base URL is explicit nor when it
// is left empty (then the SDK default host is the target, which the loopback-only
// test client refuses before dialing). The SDK's own client constructor would send
// OpenAI-Organization and OpenAI-Project here.
func TestOpenAIIgnoresEnvironment(t *testing.T) {
	trap := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		t.Errorf("the environment base URL was used: %s %s", r.Method, r.Path)
		return llmtest.Reply{Status: 500}
	})
	t.Setenv("OPENAI_API_KEY", "ENV-KEY-LEAK")
	t.Setenv("OPENAI_ADMIN_KEY", "ENV-ADMIN-LEAK")
	t.Setenv("OPENAI_BASE_URL", trap.URL)
	t.Setenv("OPENAI_ORG_ID", "ORG-LEAK")
	t.Setenv("OPENAI_PROJECT_ID", "PROJECT-LEAK")
	t.Setenv("OPENAI_WEBHOOK_SECRET", "WEBHOOK-LEAK")
	t.Setenv("OPENAI_CUSTOM_HEADERS", "X-Env-Leak: HEADER-LEAK")

	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}

	checkHeaders := func(t *testing.T, reqs []llmtest.Request) {
		t.Helper()
		if len(reqs) != 3 {
			t.Fatalf("the upstream saw %d requests, want 3 (chat, stream, models)", len(reqs))
		}
		for i, r := range reqs {
			if got := r.Header.Get("Authorization"); got != "Bearer explicit-key" {
				t.Errorf("request %d: authorization = %q, want the explicit key", i, got)
			}
			for _, name := range []string{"Openai-Organization", "Openai-Project", "X-Env-Leak"} {
				if got := r.Header.Get(name); got != "" {
					t.Errorf("request %d: header %s = %q came from the environment", i, name, got)
				}
			}
			for name, values := range r.Header {
				for _, v := range values {
					if strings.Contains(v, "LEAK") {
						t.Errorf("request %d: header %s = %q carries an environment value", i, name, v)
					}
				}
			}
		}
	}

	t.Run("explicit base URL", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
			switch {
			case strings.HasSuffix(r.Path, "/models"):
				return jsonReply(`{"object":"list","data":[]}`)
			case strings.Contains(string(r.Body), `"stream":true`):
				return sseReply(minimalStream)
			}
			return jsonReply(minimalCompletion)
		})
		p, err := New(llmhub.ProviderConfig{APIKey: "explicit-key", BaseURL: up.URL + "/v1"}, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		drain(t, p, msgs, llmhub.ModelOptions{})
		if _, err := p.ListModels(t.Context()); err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		checkHeaders(t, up.Requests())
	})

	t.Run("empty base URL does not read OPENAI_BASE_URL", func(t *testing.T) {
		// The upstream only lends its loopback-only client: the SDK default host is
		// api.openai.com, and that client refuses it before any dial or lookup.
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalCompletion) })
		p, err := New(llmhub.ProviderConfig{APIKey: "explicit-key"}, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
		if me := failureOf(t, err); me.Message != "Connection error." {
			t.Errorf("Chat text = %q, want the refused default host", me.Message)
		}
		if n := len(up.Requests()); n != 0 {
			t.Errorf("the explicit upstream saw %d requests, want 0", n)
		}
	})

	t.Run("the key comes from the config only", func(t *testing.T) {
		// OPENAI_API_KEY is set, and an empty config key sends no Authorization at
		// all: the environment does not stand in for the key (an empty key is
		// accepted).
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalCompletion) })
		p, err := New(llmhub.ProviderConfig{BaseURL: up.URL + "/v1"}, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New with an empty key: %v", err)
		}
		if _, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		reqs := up.Requests()
		if len(reqs) != 1 {
			t.Fatalf("the upstream saw %d requests, want 1", len(reqs))
		}
		if h, ok := reqs[0].Header["Authorization"]; ok {
			t.Errorf("authorization = %q, want none (OPENAI_API_KEY must not be read)", h)
		}
	})

	if n := len(trap.Requests()); n != 0 {
		t.Errorf("the trap upstream saw %d requests", n)
	}
}

// TestProvidersKeepTheResponseAliveOnInvalidToolArguments is the OpenAI-compatible
// case of "providers keep the response alive on invalid tool arguments": a tool call
// whose streamed arguments are not valid JSON must not fail the stream; it becomes a
// call with empty arguments and an ArgumentsError the harness can answer. The provider
// is the novita one.
func TestProvidersKeepTheResponseAliveOnInvalidToolArguments(t *testing.T) {
	t.Run("OpenAI-compatible", func(t *testing.T) {
		const badJSON = `{"path": "notes/report.md", "content": "He said "hi""}`
		argsChunk, err := jsonx.Marshal(badJSON)
		if err != nil {
			t.Fatal(err)
		}
		transcript := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + string(argsChunk) + `}}]},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(transcript) })
		p := newTestProvider(t, up, WithName("novita"))

		chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "write the report"}}, llmhub.ModelOptions{})

		var call *kernel.ToolCall
		for _, c := range chunks {
			if c.Type == kernel.ChunkError {
				t.Fatalf("the stream failed: %s", c.Error)
			}
			if c.Type == kernel.ChunkToolCall {
				call = c.ToolCall
			}
		}
		if len(chunks) == 0 || chunks[len(chunks)-1].Type != kernel.ChunkDone {
			t.Fatalf("the stream did not end with done: %+v", chunks)
		}
		if call == nil {
			t.Fatal("no tool_call chunk")
		}
		if call.ID != "call_1" || call.Name != "write_file" {
			t.Errorf("call = %s %s, want call_1 write_file", call.ID, call.Name)
		}
		if len(call.Arguments) != 0 {
			t.Errorf("arguments = %s, want empty", call.Arguments)
		}
		if !strings.Contains(call.ArgumentsError, "not valid JSON") || !strings.Contains(call.ArgumentsError, `"path": "notes/report.md"`) {
			t.Errorf("ArgumentsError = %q", call.ArgumentsError)
		}
	})
}

// TestOpenAIIsRegisteredAndNamed checks what a consumer sees after importing the
// package: the factories are registered as "openai" and "novita", the provider
// reports its name and version, and the lifecycle hooks and the token estimate work.
func TestOpenAIIsRegisteredAndNamed(t *testing.T) {
	for _, name := range []string{"openai", "novita"} {
		t.Run(name, func(t *testing.T) {
			p, err := llmhub.ResolveProvider(name, llmhub.ProviderConfig{APIKey: "k"})
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			if _, ok := p.(*Provider); !ok {
				t.Fatalf("ResolveProvider returned %T, want *openaicompat.Provider", p)
			}
			if _, ok := p.(llmhub.ModelLister); !ok {
				t.Error("the provider is not an llmhub.ModelLister")
			}
			if p.Name() != name || p.Version() != "1.0.0" {
				t.Errorf("name %q version %q, want %s 1.0.0", p.Name(), p.Version(), name)
			}
			if err := kernel.Initialize(t.Context(), p); err != nil {
				t.Errorf("Initialize: %v", err)
			}
			if err := kernel.Dispose(t.Context(), p); err != nil {
				t.Errorf("Dispose: %v", err)
			}
			msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "abcde"}}
			n, err := p.CountTokens(t.Context(), msgs)
			if err != nil || n != llmhub.EstimateTokens(msgs) || n != 2 {
				t.Errorf("CountTokens = %d, %v; want 2, nil", n, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := p.CountTokens(ctx, msgs); !errors.Is(err, context.Canceled) {
				t.Errorf("CountTokens on a cancelled context returned %v, want context.Canceled", err)
			}
		})
	}
	// The default name of a provider built directly is "openai".
	p, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "openai" {
		t.Errorf("default name = %q, want openai", p.Name())
	}
}

// TestOpenAIRegisteredDefaults checks the base URL of the registry entries without
// sending a request: an empty BaseURL gives the default of each name, and an
// explicit BaseURL wins. The provider's unexported base URL field is read, so
// nothing leaves the process.
func TestOpenAIRegisteredDefaults(t *testing.T) {
	cases := []struct {
		name, baseURL, want string
	}{
		{"openai", "", "https://api.openai.com/v1"},
		{"novita", "", "https://api.novita.ai/v3/openai"},
		{"openai", "http://127.0.0.1:9/custom/v1", "http://127.0.0.1:9/custom/v1"},
		{"novita", "http://127.0.0.1:9/custom/v1", "http://127.0.0.1:9/custom/v1"},
	}
	for _, c := range cases {
		t.Run(c.name+" "+c.baseURL, func(t *testing.T) {
			p, err := llmhub.ResolveProvider(c.name, llmhub.ProviderConfig{APIKey: "k", BaseURL: c.baseURL})
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			prov, ok := p.(*Provider)
			if !ok {
				t.Fatalf("ResolveProvider returned %T", p)
			}
			if prov.baseURL != c.want {
				t.Errorf("base URL = %q, want %q", prov.baseURL, c.want)
			}
			if prov.Name() != c.name {
				t.Errorf("name = %q, want %q", prov.Name(), c.name)
			}
		})
	}
	// A provider built directly with no base URL talks to the OpenAI default too.
	p, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.baseURL != "https://api.openai.com/v1" {
		t.Errorf("New default base URL = %q", p.baseURL)
	}
}

// TestOpenAIDefaultsAndOverrides checks the model fallbacks: the provider default
// model, cfg.Model, and ModelOptions.Model per call, and the 4096 max_tokens default
// (gone since A-88: max_tokens is left out when ModelOptions.MaxTokens is nil).
func TestOpenAIDefaultsAndOverrides(t *testing.T) {
	model := func(t *testing.T, cfg llmhub.ProviderConfig, opts llmhub.ModelOptions) string {
		t.Helper()
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalCompletion) })
		cfg.BaseURL = up.URL + "/v1"
		p, err := New(cfg, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, opts); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		body := string(up.Requests()[0].Body)
		if strings.Contains(body, `"max_tokens"`) && opts.MaxTokens == nil {
			t.Errorf("max_tokens is sent without ModelOptions.MaxTokens: %s", body)
		}
		return body
	}
	if body := model(t, llmhub.ProviderConfig{APIKey: "k"}, llmhub.ModelOptions{}); !strings.HasPrefix(body, `{"model":"gpt-4o-mini",`) {
		t.Errorf("default model: %s", body)
	}
	if body := model(t, llmhub.ProviderConfig{APIKey: "k", Model: "from-config"}, llmhub.ModelOptions{}); !strings.HasPrefix(body, `{"model":"from-config",`) {
		t.Errorf("cfg.Model: %s", body)
	}
	if body := model(t, llmhub.ProviderConfig{APIKey: "k", Model: "from-config"}, llmhub.ModelOptions{Model: "from-call"}); !strings.HasPrefix(body, `{"model":"from-call",`) {
		t.Errorf("ModelOptions.Model: %s", body)
	}
}

// TestMapErrorFallbacks covers the errors the goldens cannot produce on a loopback
// server: a request that timed out, transport errors of every shape (whose own text
// carries a URL or an address), a stream event that is not JSON, and an error the SDK
// made up itself. A timeout and a network failure are retryable, and a long text is cut
// at llmtransport.MaxErrorMessage (error mapping review).
func TestMapErrorFallbacks(t *testing.T) {
	long := strings.Repeat("x", 3*llmtransport.MaxErrorMessage)
	cases := []struct {
		name      string
		err       error
		message   string
		code      kernel.ModelErrorCode
		retryable bool
	}{
		{"deadline", context.DeadlineExceeded, "Request timed out.", kernel.ModelCodeTimeout, true},
		{"deadline inside a url error", &url.Error{Op: "Post", URL: "https://x.example/?key=" + secret, Err: context.DeadlineExceeded}, "Request timed out.", kernel.ModelCodeTimeout, true},
		{"url error", &url.Error{Op: "Post", URL: "https://x.example/?key=" + secret, Err: errors.New("boom")}, "Connection error.", kernel.ModelCodeProvider, true},
		{"net op error", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, "Connection error.", kernel.ModelCodeProvider, true},
		{"a body cut short", fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), "Connection error.", kernel.ModelCodeProvider, true},
		{"malformed event", &json.SyntaxError{}, "Error reading response: malformed server-sent event JSON.", kernel.ModelCodeProvider, false},
		{"anything else keeps its text", errors.New("something else"), "something else", kernel.ModelCodeProvider, false},
		{"a long text is cut", errors.New(long), llmtransport.TruncateMessage(long), kernel.ModelCodeProvider, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			me := mapError(c.err, nil)
			if me.Message != c.message || me.Code != c.code || me.Retryable != c.retryable {
				t.Errorf("mapError = %q %q retryable %v, want %q %q retryable %v", me.Message, me.Code, me.Retryable, c.message, c.code, c.retryable)
			}
			if found := leaks(me.Message); len(found) > 0 {
				t.Errorf("the text %q leaks %q", me.Message, found)
			}
		})
	}

	t.Run("a stream error event", func(t *testing.T) {
		se := &ssestream.StreamError{
			Message: "received error while streaming: {...}",
			Event:   ssestream.Event{Data: []byte(`{"error":{"message":"overloaded"}}` + "\n")},
		}
		me := mapError(se, nil)
		if me.Message != "overloaded" || me.Code != kernel.ModelCodeProvider || me.Retryable {
			t.Errorf("mapError = %q %q retryable %v", me.Message, me.Code, me.Retryable)
		}
		none := mapError(&ssestream.StreamError{Message: "x", Event: ssestream.Event{Data: []byte("{}")}}, nil)
		if none.Message != "(no status code or body)" {
			t.Errorf("an event with no error value gave %q", none.Message)
		}
	})

	t.Run("an API error the failure did not see", func(t *testing.T) {
		// Without a failure the SDK error supplies its own status and the body kept in
		// its response, which is read more than once without being consumed.
		e := &sdk.Error{StatusCode: 429}
		me := mapError(e, nil)
		if me.Message != "Rate limited" || me.Code != kernel.ModelCodeRateLimit || !me.Retryable {
			t.Errorf("429: %q %q %v", me.Message, me.Code, me.Retryable)
		}
		e = &sdk.Error{StatusCode: 400, Response: &http.Response{Body: io.NopCloser(strings.NewReader(`{"error":{"message":"bad field"}}`))}}
		for range 2 {
			me = mapError(e, nil)
			if me.Message != "400 bad field" || me.Code != kernel.ModelCodeInvalidRequest {
				t.Errorf("400: %q %q", me.Message, me.Code)
			}
		}
		// No response and no error object: the text is the "no body" text.
		me = mapError(&sdk.Error{StatusCode: 500}, nil)
		if me.Message != "500 status code (no body)" || !me.Retryable {
			t.Errorf("500: %q %v", me.Message, me.Retryable)
		}
	})
}

// TestOpenAIFinishReasons pins the finish-reason table: only content_filter is an
// error, and a normal or unknown end is end_turn.
func TestOpenAIFinishReasons(t *testing.T) {
	cases := map[string]kernel.StopReason{
		"stop":           kernel.StopEndTurn,
		"":               kernel.StopEndTurn,
		"eos":            kernel.StopEndTurn,
		"tool_calls":     kernel.StopToolUse,
		"function_call":  kernel.StopToolUse,
		"length":         kernel.StopMaxTokens,
		"content_filter": kernel.StopError,
	}
	for reason, want := range cases {
		if got := mapFinishReason(reason); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

// TestOpenAIToolCallsWithStopAreToolUse covers a server that answers a tool call
// with finish_reason "stop": the response is tool_use, so the calls run.
func TestOpenAIToolCallsWithStopAreToolUse(t *testing.T) {
	body := `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"stop"}]}`
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(body) })
	resp, err := newTestProvider(t, up).Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.StopReason != kernel.StopToolUse || len(resp.Message.ToolCalls) != 1 {
		t.Errorf("stop %q with %d calls, want tool_use with 1", resp.StopReason, len(resp.Message.ToolCalls))
	}
}

// TestOpenAIUserToolResultsKeepTheirText covers the tool-results mapping of a user
// message that carries results and text: one tool message per result, then the text as
// a user message; a tool message is its results only.
func TestOpenAIUserToolResultsKeepTheirText(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "here you go", ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "r1"}}},
		{Role: kernel.RoleTool, Content: "ignored", ToolResults: []kernel.ToolResult{{ToolCallID: "c2", Content: "r2"}}},
	}
	got, err := jsonx.Marshal(mapMessages(msgs, llmhub.ModelOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"role":"tool","tool_call_id":"c1","content":"r1"},{"role":"user","content":"here you go"},{"role":"tool","tool_call_id":"c2","content":"r2"}]`
	if string(got) != want {
		t.Errorf("messages\n got: %s\nwant: %s", got, want)
	}
}

// TestOpenAIStreamEndings covers the terminal events of a stream: [DONE] in any
// spelling the SDK accepts, or a finish_reason, ends it with done; a body that ends
// with neither ends with one error chunk.
func TestOpenAIStreamEndings(t *testing.T) {
	text := `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	cases := []struct {
		name string
		sse  string
		last kernel.StreamChunk
	}{
		{"done line", text + "data: [DONE]\n\n", kernel.StreamChunk{Type: kernel.ChunkDone}},
		{"done without a space", text + "data:[DONE]\n\n", kernel.StreamChunk{Type: kernel.ChunkDone}},
		{"done with CRLF", text + "data: [DONE]\r\n\r\n", kernel.StreamChunk{Type: kernel.ChunkDone}},
		{"finish without done", text + finish, kernel.StreamChunk{Type: kernel.ChunkDone}},
		{"cut after text", text, kernel.StreamChunk{Type: kernel.ChunkError, Error: errTruncated}},
		{"a comment that mentions done", text + ": " + strings.Repeat("x", 100) + " data: [DONE]\n\n", kernel.StreamChunk{Type: kernel.ChunkError, Error: errTruncated}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(c.sse) })
			chunks := drain(t, newTestProvider(t, up), []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{})
			if len(chunks) == 0 {
				t.Fatal("no chunk")
			}
			last := chunks[len(chunks)-1]
			if last.Type != c.last.Type || last.Error != c.last.Error {
				t.Errorf("last chunk %+v, want %+v", last, c.last)
			}
		})
	}
}

// TestOpenAIStreamDoneStopReason requires the done chunk to carry the finish_reason,
// mapped like Chat maps it, and none when no chunk carried one (A-88). The request
// leaves max_tokens out when ModelOptions.MaxTokens is nil.
func TestOpenAIStreamDoneStopReason(t *testing.T) {
	chunk := func(finish string) string {
		reason := "null"
		if finish != "" {
			reason = `"` + finish + `"`
		}
		return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":` + reason + `}]}` + "\n\n"
	}
	cases := map[string]kernel.StopReason{
		"stop":           kernel.StopEndTurn,
		"tool_calls":     kernel.StopToolUse,
		"length":         kernel.StopMaxTokens,
		"content_filter": kernel.StopError,
		"":               "",
	}
	for reported, want := range cases {
		t.Run("reported "+reported, func(t *testing.T) {
			sse := chunk(reported) + "data: [DONE]\n\n"
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(sse) })
			chunks := drain(t, newTestProvider(t, up), []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{})
			done := chunks[len(chunks)-1]
			if done.Type != kernel.ChunkDone || done.StopReason != want {
				t.Fatalf("last chunk = %+v, want done with stop reason %q", done, want)
			}
			if body := string(up.Requests()[0].Body); strings.Contains(body, `"max_tokens"`) {
				t.Fatalf("max_tokens sent without ModelOptions.MaxTokens: %s", body)
			}
		})
	}
}

// TestOpenAIMaxTokensOnlyWhenSet: max_tokens is sent only when ModelOptions.MaxTokens
// is set, an explicit zero included (A-88).
func TestOpenAIMaxTokensOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		max  *int
		want string
	}{
		{llmhub.Int(100), `{"model":"gpt-4o-mini","max_tokens":100,"messages":`},
		{llmhub.Int(0), `{"model":"gpt-4o-mini","max_tokens":0,"messages":`},
		{nil, `{"model":"gpt-4o-mini","messages":`},
	} {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalCompletion) })
		p := newTestProvider(t, up)
		if _, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{MaxTokens: tc.max}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if body := string(up.Requests()[0].Body); !strings.HasPrefix(body, tc.want) {
			t.Errorf("body %s, want the prefix %s", body, tc.want)
		}
	}
}
