package openaisub

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// roundTripFunc is an http.RoundTripper made of a function: a transport that never
// opens a connection.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// failingBody is a response body that returns its chunks one per Read and then fails
// with err (io.EOF makes it an ordinary end of stream).
type failingBody struct {
	chunks []string
	err    error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.chunks) > 0 {
		n := copy(p, b.chunks[0])
		if n == len(b.chunks[0]) {
			b.chunks = b.chunks[1:]
		} else {
			b.chunks[0] = b.chunks[0][n:]
		}
		return n, nil
	}
	return 0, b.err
}

func (b *failingBody) Close() error { return nil }

// endlessBody is a body that never ends and never writes a newline.
type endlessBody struct{ read atomic.Int64 }

func (b *endlessBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	b.read.Add(int64(len(p)))
	return len(p), nil
}

func (b *endlessBody) Close() error { return nil }

// providerOnBody builds a provider whose every request is answered with a 200 and
// body, through a transport that opens no connection. The endpoints are never dialed.
func providerOnBody(t *testing.T, body io.ReadCloser) *Provider {
	t.Helper()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    r,
		}, nil
	})
	p, err := New(llmhub.ProviderConfig{OAuthAccessToken: "tok", ChatgptAccountID: "acct"},
		WithEndpoints("http://127.0.0.1:1"+responsesPath, "http://127.0.0.1:1"+modelsPath),
		WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// textEvent is one output_text delta event with a delta that needs no escaping.
func textEvent(delta string) string {
	return "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + delta + "\"}\n\n"
}

var helloMsgs = []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}

// ---------------------------------------------------------------------------
// Endpoints are fixed
// ---------------------------------------------------------------------------

// TestSubscriptionIgnoresBaseURL puts a trap upstream in ProviderConfig.BaseURL and
// requires that no request ever reaches it: the bearer token and the account id go to
// the fixed ChatGPT endpoints only (T-08-28). The test client refuses every host that
// is not loopback before it dials, so the request that would have gone to chatgpt.com
// stops there, and its error text names the host it was meant for.
func TestSubscriptionIgnoresBaseURL(t *testing.T) {
	trap := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseBytes([]string{minimalStream}) })
	good := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseBytes([]string{minimalStream}) })

	p, err := New(llmhub.ProviderConfig{BaseURL: trap.URL, OAuthAccessToken: "tok", ChatgptAccountID: "acct"}, WithHTTPClient(good.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	chunks := drain(t, p, helloMsgs, llmhub.ModelOptions{})
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || !strings.Contains(chunks[0].Error, "chatgpt.com") {
		t.Fatalf("stream gave %+v, want one error chunk about chatgpt.com (the fixed endpoint)", chunks)
	}
	if _, err := p.ListModels(t.Context()); err == nil || !strings.Contains(err.Error(), "chatgpt.com") {
		t.Errorf("ListModels error %v does not name the fixed endpoint chatgpt.com", err)
	}
	for _, up := range []*llmtest.Upstream{trap, good} {
		if n := len(up.Requests()); n != 0 {
			t.Errorf("an upstream saw %d requests, want none (the endpoints are fixed)", n)
		}
	}
	if p.responsesURL != ResponsesURL || p.modelsURL != ModelsURL {
		t.Errorf("endpoints %s and %s differ from the constants", p.responsesURL, p.modelsURL)
	}
}

// TestSubscriptionIsRegistered requires the factory under its registered name, and the
// provider it builds to be an llmhub.TokenUpdater (the server finds it with a type
// assertion on the provider the hub holds).
func TestSubscriptionIsRegistered(t *testing.T) {
	p, err := llmhub.ResolveProvider("openai-subscription", llmhub.ProviderConfig{OAuthAccessToken: "tok", ChatgptAccountID: "acct"})
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	sub, ok := p.(*Provider)
	if !ok {
		t.Fatalf("ResolveProvider built %T, want *openaisub.Provider", p)
	}
	if sub.Name() != "openai-subscription" || sub.Version() != "1.0.0" {
		t.Errorf("name %q version %q", sub.Name(), sub.Version())
	}
	if _, ok := p.(llmhub.TokenUpdater); !ok {
		t.Error("the provider is not an llmhub.TokenUpdater")
	}
	if _, ok := p.(llmhub.ModelLister); !ok {
		t.Error("the provider is not an llmhub.ModelLister")
	}
	if sub.model != "gpt-5.6-terra" {
		t.Errorf("default model %q, want gpt-5.6-terra", sub.model)
	}
}

// TestSubscriptionTransportTimeouts requires the provider's own client to have no
// Client.Timeout (a stream can stay open for minutes while it sends), and the shared
// transport timeouts of the other providers: the 10-minute response-header timeout
// and the 5-minute stream idle timeout (the review fix of OQ6; it used to have none,
// so a server that never answered hung the call for as long as the caller's
// context). The options change both, and zero disables them.
func TestSubscriptionTransportTimeouts(t *testing.T) {
	p, err := New(llmhub.ProviderConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.client.Timeout != 0 {
		t.Errorf("Client.Timeout is %v", p.client.Timeout)
	}
	if p.timeout != llmtransport.DefaultRequestTimeout || p.idle != llmtransport.DefaultStreamIdleTimeout {
		t.Errorf("timeouts %v and %v, want %v and %v", p.timeout, p.idle, llmtransport.DefaultRequestTimeout, llmtransport.DefaultStreamIdleTimeout)
	}
	if p.client.Transport == http.DefaultTransport {
		t.Error("the provider shares the default transport")
	}
	off, err := New(llmhub.ProviderConfig{}, WithRequestTimeout(0), WithStreamIdleTimeout(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if off.timeout != 0 || off.idle != 0 {
		t.Errorf("disabled timeouts are %v and %v", off.timeout, off.idle)
	}

	t.Run("no response headers", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Hang: true} })
		p := newTestProvider(t, up, caseConfig{Token: "tok"}, WithRequestTimeout(150*time.Millisecond))
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		_, err := p.Chat(ctx, helloMsgs, llmhub.ModelOptions{})
		if me := failureOf(t, err); me.Code != kernel.ModelCodeTimeout || !me.Retryable {
			t.Errorf("Chat error = %+v, want a retryable timeout", me)
		}
	})
	t.Run("a stream that stalls", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return llmtest.Reply{
				Header: map[string]string{"Content-Type": "text/event-stream"},
				Chunks: []string{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n"},
				Hang:   true,
			}
		})
		p := newTestProvider(t, up, caseConfig{Token: "tok"}, WithStreamIdleTimeout(150*time.Millisecond))
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var chunks []kernel.StreamChunk
		for c, err := range p.Stream(ctx, helloMsgs, llmhub.ModelOptions{}) {
			if err != nil {
				t.Fatalf("stream yielded an error: %v", err)
			}
			chunks = append(chunks, c)
		}
		if len(chunks) != 2 || chunks[0].Type != kernel.ChunkText || chunks[1].Type != kernel.ChunkError {
			t.Fatalf("chunks %+v, want text then one error chunk", chunks)
		}
		_, err := p.Chat(ctx, helloMsgs, llmhub.ModelOptions{})
		if me := failureOf(t, err); me.Code != kernel.ModelCodeTimeout || !me.Retryable {
			t.Errorf("Chat error = %+v, want a retryable timeout", me)
		}
	})
}

// ---------------------------------------------------------------------------
// Redirects
// ---------------------------------------------------------------------------

// TestSubscriptionRedirectDropsAuthorization redirects the Responses request from the
// 127.0.0.1 upstream to a localhost upstream (another host name, the same machine) and
// requires the second server to receive no credential header: not Authorization, not
// the account id, not the Responses flag (T-08-29; RESEARCH Assumption A9). The
// stdlib drops Authorization on its own; the provider's redirect policy also drops the
// others. A redirect to another path of the same host keeps the credentials.
func TestSubscriptionRedirectDropsAuthorization(t *testing.T) {
	second := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseBytes([]string{minimalStream}) })
	target := strings.Replace(second.URL, "127.0.0.1", "localhost", 1) + responsesPath
	first := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		switch r.Path {
		case "/same-host":
			return llmtest.Reply{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": responsesPath}}
		default:
			return llmtest.Reply{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": target}}
		}
	})

	// Is localhost the machine the loopback listener is on? On a host where it
	// resolves elsewhere the redirect cannot be followed, and there is nothing to test.
	probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.Replace(second.URL, "127.0.0.1", "localhost", 1)+modelsPath, nil)
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	resp, err := first.Client().Do(probe)
	if err != nil {
		t.Skipf("localhost does not reach the loopback listener on this host: %v", err)
	}
	_ = resp.Body.Close()

	p := newTestProvider(t, first, caseConfig{Token: "tok-secret", Account: "acct-secret"})
	chunks := drain(t, p, helloMsgs, llmhub.ModelOptions{})
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkDone {
		t.Fatalf("stream gave %+v, want the done of the second server", chunks)
	}
	if got := first.Requests()[0].Header.Get("Authorization"); got != "Bearer tok-secret" {
		t.Errorf("the first server got Authorization %q", got)
	}
	reqs := second.Requests()
	if len(reqs) != 2 { // the probe and the redirected request
		t.Fatalf("the second server saw %d requests, want 2", len(reqs))
	}
	redirected := reqs[1]
	if redirected.Method != http.MethodPost || redirected.Path != responsesPath {
		t.Fatalf("the redirected request is %s %s", redirected.Method, redirected.Path)
	}
	for _, h := range []string{"Authorization", "Chatgpt-Account-Id", "Openai-Beta"} {
		if v := redirected.Header.Get(h); v != "" {
			t.Errorf("the redirect to another host carried %s: %q", h, v)
		}
	}
	if len(redirected.Body) == 0 {
		t.Error("the 307 redirect did not carry the body")
	}

	// The same host keeps the credentials.
	same := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		if r.Path == "/entry" {
			return llmtest.Reply{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": responsesPath}}
		}
		return sseBytes([]string{minimalStream})
	})
	ps := newTestProvider(t, same, caseConfig{Token: "tok-secret", Account: "acct-secret"}, WithEndpoints(same.URL+"/entry", same.URL+modelsPath))
	drain(t, ps, helloMsgs, llmhub.ModelOptions{})
	sreqs := same.Requests()
	if len(sreqs) != 2 {
		t.Fatalf("the same-host upstream saw %d requests, want 2", len(sreqs))
	}
	if sreqs[1].Header.Get("Authorization") != "Bearer tok-secret" || sreqs[1].Header.Get("ChatGPT-Account-Id") != "acct-secret" {
		t.Errorf("a redirect inside one host lost the credentials: %v", sreqs[1].Header)
	}
}

// TestSubscriptionRedirectLoopStops requires a redirect loop to end as an in-band
// error chunk after the stdlib limit of ten redirects.
func TestSubscriptionRedirectLoopStops(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": responsesPath}}
	})
	p := newTestProvider(t, up, caseConfig{Token: "tok", Account: "acct"})
	chunks := drain(t, p, helloMsgs, llmhub.ModelOptions{})
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || !strings.Contains(chunks[0].Error, "10 redirects") {
		t.Fatalf("stream gave %+v, want one error chunk about the redirect limit", chunks)
	}
	if n := len(up.Requests()); n != maxRedirects {
		t.Errorf("the upstream saw %d requests, want %d", n, maxRedirects)
	}
}

// ---------------------------------------------------------------------------
// Bounds
// ---------------------------------------------------------------------------

// TestSubscriptionErrorBodyIsBounded serves a 2 MiB error body and requires the
// message to be its first 1 MiB: for the stream, and for the
// model list. The text put into the ModelError is then cut to
// llmtransport.MaxErrorMessage bytes (error mapping review).
func TestSubscriptionErrorBodyIsBounded(t *testing.T) {
	body := strings.Repeat("e", 2<<20)
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Status: http.StatusInternalServerError, Chunks: []string{body}}
	})
	p := newTestProvider(t, up, caseConfig{Token: "tok", Account: "acct"})
	want := llmtransport.TruncateMessage(body[:maxErrorBody])
	chunks := drain(t, p, helloMsgs, llmhub.ModelOptions{})
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || chunks[0].Error != want {
		t.Fatalf("stream gave %d chunks, the message is %d bytes, want one error chunk of %d bytes", len(chunks), len(chunks[0].Error), len(want))
	}
	_, err := p.ListModels(t.Context())
	if me := failureOf(t, err); me.Message != want {
		t.Errorf("ListModels message is %d bytes, want %d", len(me.Message), len(want))
	}
}

// TestSubscriptionModelListIsBounded serves a model list above the limit and requires
// a provider error, not an unbounded read.
func TestSubscriptionModelListIsBounded(t *testing.T) {
	big := `{"models":[],"padding":"` + strings.Repeat("p", maxModelsBody) + `"}`
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{big}}
	})
	p := newTestProvider(t, up, caseConfig{Token: "tok", Account: "acct"})
	_, err := p.ListModels(t.Context())
	if me := failureOf(t, err); me.Code != kernel.ModelCodeProvider || me.Retryable {
		t.Errorf("got code %q retryable %v, want a non-retryable provider error", me.Code, me.Retryable)
	}
}

// TestSubscriptionLineOverTheCapIsOneErrorChunk serves a line that never ends, first
// through a body that counts what is read (the parser must stop at the cap, not read
// on without bound), then as 33 MiB over a real connection (the case of the plan): one
// in-band error chunk each, nothing after it.
func TestSubscriptionLineOverTheCapIsOneErrorChunk(t *testing.T) {
	t.Run("endless body", func(t *testing.T) {
		body := &endlessBody{}
		chunks := drain(t, providerOnBody(t, body), helloMsgs, llmhub.ModelOptions{})
		if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || !strings.Contains(chunks[0].Error, "longer than") {
			t.Fatalf("stream gave %+v, want one error chunk about the line limit", chunks)
		}
		if read := body.read.Load(); read > maxLineBytes+2*readBufferBytes {
			t.Errorf("the parser read %d bytes of a line with a limit of %d", read, maxLineBytes)
		}
	})
	t.Run("33 MiB over a connection", func(t *testing.T) {
		line := "data: " + strings.Repeat("x", 33<<20)
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseBytes([]string{line}) })
		p := newTestProvider(t, up, caseConfig{Token: "tok", Account: "acct"})
		chunks := drain(t, p, helloMsgs, llmhub.ModelOptions{})
		if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || !strings.Contains(chunks[0].Error, "longer than") {
			t.Fatalf("stream gave %d chunks, want one error chunk about the line limit", len(chunks))
		}
		_, err := p.Chat(t.Context(), helloMsgs, llmhub.ModelOptions{})
		if me := failureOf(t, err); !me.Retryable || me.Code != kernel.ModelCodeProvider {
			t.Errorf("Chat: got code %q retryable %v, want a retryable provider error", me.Code, me.Retryable)
		}
	})
}

// TestSubscriptionReadErrorAfterChunks requires a failure in the middle of the stream
// to be one in-band error chunk after the chunks already read, with the error's text.
func TestSubscriptionReadErrorAfterChunks(t *testing.T) {
	text := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"
	chunks := drain(t, providerOnBody(t, &failingBody{chunks: []string{text}, err: errors.New("connection reset")}), helloMsgs, llmhub.ModelOptions{})
	if len(chunks) != 2 || chunks[0].Type != kernel.ChunkText || chunks[1].Type != kernel.ChunkError || chunks[1].Error != "connection reset" {
		t.Fatalf("stream gave %+v, want the text chunk and then the error", chunks)
	}
}

// ---------------------------------------------------------------------------
// Context
// ---------------------------------------------------------------------------

// TestSubscriptionChatReturnsTheBareContextError cancels the context while the
// upstream holds the stream open and requires Chat to return the bare ctx.Err(), not a
// ModelError (CONTEXT C3: an aborted call is not a model failure), and the stream to
// yield it once in the error slot.
func TestSubscriptionChatReturnsTheBareContextError(t *testing.T) {
	text := textEvent("x")
	arrived := make(chan struct{}, 4)
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		arrived <- struct{}{}
		return llmtest.Reply{Header: map[string]string{"Content-Type": "text/event-stream"}, Chunks: []string{text}, Hang: true}
	})
	p := newTestProvider(t, up, caseConfig{Token: "tok", Account: "acct"})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := 0
	var last error
	for c, err := range p.Stream(ctx, helloMsgs, llmhub.ModelOptions{}) {
		if err != nil {
			last = err
			break
		}
		seen++
		if c.Type != kernel.ChunkText {
			t.Fatalf("chunk %+v", c)
		}
		cancel()
	}
	if seen != 1 || !errors.Is(last, context.Canceled) {
		t.Errorf("stream: saw %d chunks and error %v, want one chunk and context.Canceled", seen, last)
	}
	<-arrived

	// Chat: cancel once the request reached the upstream and the stream is open.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	go func() {
		select {
		case <-arrived:
			cancel2()
		case <-t.Context().Done():
		}
	}()
	_, err := p.Chat(ctx2, helloMsgs, llmhub.ModelOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat returned %v, want context.Canceled", err)
	}
	var me *kernel.ModelError
	if errors.As(err, &me) {
		t.Errorf("Chat returned a ModelError for an aborted call: %v", err)
	}

	if _, err := p.ListModels(ctx2); !errors.Is(err, context.Canceled) {
		t.Errorf("ListModels with a done context returned %v, want context.Canceled", err)
	}
	if _, err := p.CountTokens(ctx2, helloMsgs); !errors.Is(err, context.Canceled) {
		t.Errorf("CountTokens with a done context returned %v, want context.Canceled", err)
	}
}

// TestSubscriptionCountTokensIsTheSharedEstimate requires CountTokens to equal the
// estimate the other providers use.
func TestSubscriptionCountTokensIsTheSharedEstimate(t *testing.T) {
	p, err := New(llmhub.ProviderConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello world"}, {Role: kernel.RoleAssistant, Content: "ok"}}
	got, err := p.CountTokens(t.Context(), msgs)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if want := llmhub.EstimateTokens(msgs); got != want || got == 0 {
		t.Errorf("CountTokens = %d, EstimateTokens = %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// Headers and credentials
// ---------------------------------------------------------------------------

// TestSubscriptionHeaderCasing requires the headers of a Responses call and of a
// models call to be the expected sets, found by name without regard to case (Go sends the
// canonical forms OpenAI-Beta as Openai-Beta and ChatGPT-Account-Id as
// Chatgpt-Account-Id, which servers match the same).
func TestSubscriptionHeaderCasing(t *testing.T) {
	up := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		if r.Path == modelsPath {
			return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{`{"models":[]}`}}
		}
		return sseBytes([]string{minimalStream})
	})
	p := newTestProvider(t, up, caseConfig{Token: "tok", Account: "acct"})
	drain(t, p, helloMsgs, llmhub.ModelOptions{})
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	lower := func(h http.Header) map[string]string {
		out := map[string]string{}
		for k, v := range h {
			out[strings.ToLower(k)] = strings.Join(v, ",")
		}
		return out
	}
	reqs := up.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", len(reqs))
	}
	responses, models := lower(reqs[0].Header), lower(reqs[1].Header)
	for name, want := range map[string]string{
		"authorization":      "Bearer tok",
		"content-type":       "application/json",
		"openai-beta":        "responses=experimental",
		"chatgpt-account-id": "acct",
	} {
		if responses[name] != want {
			t.Errorf("responses header %s = %q, want %q", name, responses[name], want)
		}
	}
	if _, ok := responses["originator"]; ok {
		t.Error("the responses call sent originator")
	}
	for name, want := range map[string]string{
		"authorization":      "Bearer tok",
		"originator":         "aec-agent",
		"chatgpt-account-id": "acct",
	} {
		if models[name] != want {
			t.Errorf("models header %s = %q, want %q", name, models[name], want)
		}
	}
	for _, name := range []string{"openai-beta", "content-type"} {
		if v, ok := models[name]; ok {
			t.Errorf("the models call sent %s: %q", name, v)
		}
	}
}

// TestSubscriptionUpdateTokensChangesTheNextRequest requires a request that starts
// after UpdateTokens to carry the new pair, for all three calls.
func TestSubscriptionUpdateTokensChangesTheNextRequest(t *testing.T) {
	up := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		if r.Path == modelsPath {
			return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{`{"models":[]}`}}
		}
		return sseBytes([]string{minimalStream})
	})
	p := newTestProvider(t, up, caseConfig{Token: "old", Account: "acct-old"})
	drain(t, p, helloMsgs, llmhub.ModelOptions{})
	p.UpdateTokens("new", "acct-new")
	drain(t, p, helloMsgs, llmhub.ModelOptions{})
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	p.UpdateTokens("newer", "")
	drain(t, p, helloMsgs, llmhub.ModelOptions{})

	reqs := up.Requests()
	if len(reqs) != 4 {
		t.Fatalf("the upstream saw %d requests, want 4", len(reqs))
	}
	want := []struct{ auth, account string }{
		{"Bearer old", "acct-old"},
		{"Bearer new", "acct-new"},
		{"Bearer new", "acct-new"},
		{"Bearer newer", ""},
	}
	for i, w := range want {
		if got := reqs[i].Header.Get("Authorization"); got != w.auth {
			t.Errorf("request %d: Authorization %q, want %q", i, got, w.auth)
		}
		if got := reqs[i].Header.Get("ChatGPT-Account-Id"); got != w.account {
			t.Errorf("request %d: account id %q, want %q", i, got, w.account)
		}
	}
}

// TestSubscriptionErrorTextsNeverCarryTheToken requires that no error text contains
// the bearer token unless the server itself sent it in a body that is passed through
// (400 and the other statuses that show the body); a 401 never shows the body
// (T-08-32).
func TestSubscriptionErrorTextsNeverCarryTheToken(t *testing.T) {
	const token = "tok-SECRET-9f3a"
	for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 502, 503} {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return llmtest.Reply{Status: status, Header: map[string]string{"Retry-After": "7"}, Chunks: []string{"the request was refused"}}
		})
		p := newTestProvider(t, up, caseConfig{Token: token, Account: "acct-SECRET"})
		chunks := drain(t, p, helloMsgs, llmhub.ModelOptions{})
		_, chatErr := p.Chat(t.Context(), helloMsgs, llmhub.ModelOptions{})
		_, listErr := p.ListModels(t.Context())
		// The status and the Retry-After of the response are kept on the error.
		for what, err := range map[string]error{"Chat": chatErr, "ListModels": listErr} {
			if me := failureOf(t, err); me.StatusCode != status || me.RetryAfter != 7*time.Second {
				t.Errorf("status %d: %s StatusCode %d RetryAfter %v, want %d 7s", status, what, me.StatusCode, me.RetryAfter, status)
			}
		}
		texts := []string{chunks[0].Error, failureOf(t, chatErr).Message, failureOf(t, listErr).Message}
		for _, text := range texts {
			if strings.Contains(text, token) || strings.Contains(text, "acct-SECRET") {
				t.Errorf("status %d: the error text %q carries a credential", status, text)
			}
		}
	}

	// A server that echoes the token in the body of a 401 does not get it shown; in the
	// body of a 400 or 500 the body is the message.
	for status, shown := range map[int]bool{401: false, 400: true, 500: true} {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return llmtest.Reply{Status: status, Chunks: []string{"you sent " + token}}
		})
		p := newTestProvider(t, up, caseConfig{Token: token, Account: "acct"})
		chunks := drain(t, p, helloMsgs, llmhub.ModelOptions{})
		if got := strings.Contains(chunks[0].Error, token); got != shown {
			t.Errorf("status %d: the echoed token shown = %v, want %v (text %q)", status, got, shown, chunks[0].Error)
		}
	}
}

// TestSubscriptionFinalStatus covers the reading of the final response status
// through Chat: the stop reason of an
// incomplete answer, and the typed error of a failed response or an error event.
func TestSubscriptionFinalStatus(t *testing.T) {
	text := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n"
	cases := []struct {
		name    string
		final   string
		stop    kernel.StopReason
		errCode kernel.ModelErrorCode
		retry   bool
	}{
		{"completed", `{"type":"response.completed","response":{"status":"completed"}}`, kernel.StopEndTurn, "", false},
		{"incomplete event", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, kernel.StopMaxTokens, "", false},
		{"content filter", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`, kernel.StopError, "", false},
		{"a member of another type does not hide the event", `{"type":"response.completed","code":7,"response":{"status":5}}`, kernel.StopEndTurn, "", false},
		{"failed rate limit", `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"slow down"}}}`, "", kernel.ModelCodeRateLimit, true},
		{"failed without an error", `{"type":"response.failed","response":{"status":"failed"}}`, "", kernel.ModelCodeProvider, false},
		{"error event", `{"type":"error","code":"context_length_exceeded","message":"too long"}`, "", kernel.ModelCodeInvalidRequest, false},
		{"nested error event", `{"type":"error","error":{"code":"rate_limit_exceeded","message":"slow down"}}`, "", kernel.ModelCodeRateLimit, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				return sseBytes([]string{text + "data: " + c.final + "\n\n"})
			})
			resp, err := newTestProvider(t, up, caseConfig{Token: "tok"}).Chat(t.Context(), helloMsgs, llmhub.ModelOptions{})
			if c.errCode != "" {
				if me := failureOf(t, err); me.Code != c.errCode || me.Retryable != c.retry {
					t.Errorf("error %+v, want code %q retryable %v", me, c.errCode, c.retry)
				}
				return
			}
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if resp.StopReason != c.stop || resp.Message.Content != "a" {
				t.Errorf("stop %q content %q, want %q and \"a\"", resp.StopReason, resp.Message.Content, c.stop)
			}
		})
	}
}

// TestSubscriptionUserToolResultsKeepTheirText covers a user message
// that carries results and text: its function_call_output items, then its text; a
// tool message is its outputs only.
func TestSubscriptionUserToolResultsKeepTheirText(t *testing.T) {
	got, err := jsonx.Marshal(inputItems([]kernel.Message{
		{Role: kernel.RoleUser, Content: "here you go", ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "r1"}}},
		{Role: kernel.RoleTool, Content: "ignored", ToolResults: []kernel.ToolResult{{ToolCallID: "c2", Content: "r2"}}},
	}, llmhub.ModelOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"function_call_output","call_id":"c1","output":"r1"},{"role":"user","content":[{"type":"input_text","text":"here you go"}]},` +
		`{"type":"function_call_output","call_id":"c2","output":"r2"}]`
	if string(got) != want {
		t.Errorf("input\n got: %s\nwant: %s", got, want)
	}
}

// TestSubscriptionStreamDoneStopReason requires the done chunk to carry the stop reason
// of the final event (A-88): a completed response is tool_use when a tool_call was
// given and end_turn otherwise; an incomplete response is max_tokens, or error when a
// content filter stopped it. The usage of the final event rides along, and Chat reports
// the same stop reason.
func TestSubscriptionStreamDoneStopReason(t *testing.T) {
	const (
		text = `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"
		call = `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{}"}}` + "\n\n"
		use  = `"usage":{"input_tokens":5,"output_tokens":9}`
	)
	cases := []struct {
		name string
		sse  string
		want kernel.StopReason
	}{
		{"completed", text + `data: {"type":"response.completed","response":{` + use + `}}` + "\n\n", kernel.StopEndTurn},
		{"completed with a call", call + `data: {"type":"response.completed","response":{` + use + `}}` + "\n\n", kernel.StopToolUse},
		{"incomplete at max_output_tokens", call + `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},` + use + `}}` + "\n\n", kernel.StopMaxTokens},
		{"incomplete by content_filter", text + `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"},` + use + `}}` + "\n\n", kernel.StopError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseBytes([]string{tc.sse}) })
			p := newTestProvider(t, up, caseConfig{Token: "tok", Account: "acc"})
			msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}
			chunks := drain(t, p, msgs, llmhub.ModelOptions{})
			done := chunks[len(chunks)-1]
			if done.Type != kernel.ChunkDone || done.StopReason != tc.want {
				t.Fatalf("last chunk = %+v, want done with stop reason %q", done, tc.want)
			}
			if done.Usage == nil || done.Usage.InputTokens != 5 || done.Usage.OutputTokens != 9 {
				t.Fatalf("done usage = %+v, want 5 in and 9 out", done.Usage)
			}
			resp, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if resp.StopReason != tc.want {
				t.Fatalf("Chat stop reason = %q, want %q", resp.StopReason, tc.want)
			}
		})
	}
}

// An error event without a message keeps the raw event in the failure, and a nested
// error event's message is read: the backend's reason is never lost.
func TestSubscriptionErrorEventDetail(t *testing.T) {
	cases := []struct{ name, event, want string }{
		{"no message", `{"type":"error","sequence_number":3,"detail":"x"}`, `the stream reported an error: {"type":"error","sequence_number":3,"detail":"x"}`},
		{"nested message", `{"type":"error","error":{"type":"server_error","message":"backend busy"}}`, "backend busy"},
		{"failed without a message", `{"type":"response.failed","response":{"status":"failed"}}`, `the response failed: {"type":"response.failed","response":{"status":"failed"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				return sseBytes([]string{"data: " + c.event + "\n\n"})
			})
			_, err := newTestProvider(t, up, caseConfig{Token: "tok"}).Chat(t.Context(), helloMsgs, llmhub.ModelOptions{})
			if me := failureOf(t, err); me.Message != c.want {
				t.Errorf("message %q, want %q", me.Message, c.want)
			}
		})
	}
}
