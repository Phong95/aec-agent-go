package bedrock

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/smithy-go"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// These tests run the REAL runtime and control-plane clients against a loopback
// httptest server (WithBaseEndpoint, WithRetryMaxAttempts(1)): the request is
// signed, sent, and the answer decoded by the AWS SDK itself, so what the injected
// fakes cannot show is covered here: the eventstream binary framing, the error
// documents of the service, cancellation of the real reader, and environment
// isolation.

// frame is one binary eventstream message of the kind ConverseStream sends: an
// event of the given type (":event-type") with a JSON payload.
func frame(t *testing.T, eventType, payload string) string {
	t.Helper()
	var b bytes.Buffer
	err := eventstream.NewEncoder().Encode(&b, eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue(eventType)},
			{Name: ":content-type", Value: eventstream.StringValue("application/json")},
		},
		Payload: []byte(payload),
	})
	if err != nil {
		t.Fatalf("encode the %s frame: %v", eventType, err)
	}
	return b.String()
}

// exceptionFrame is an exception message in the middle of a stream
// (":message-type" exception), as the service sends a failure after the response
// started.
func exceptionFrame(t *testing.T, exceptionType, payload string) string {
	t.Helper()
	var b bytes.Buffer
	err := eventstream.NewEncoder().Encode(&b, eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("exception")},
			{Name: ":exception-type", Value: eventstream.StringValue(exceptionType)},
			{Name: ":content-type", Value: eventstream.StringValue("application/json")},
		},
		Payload: []byte(payload),
	})
	if err != nil {
		t.Fatalf("encode the %s frame: %v", exceptionType, err)
	}
	return b.String()
}

const eventStreamType = "application/vnd.amazon.eventstream"

// streamReply serves frames one write at a time, as an event stream, then ends.
func streamReply(frames ...string) llmtest.Reply {
	return llmtest.Reply{Header: map[string]string{"Content-Type": eventStreamType}, Chunks: frames}
}

// TestBedrockWireStream serves a ConverseStream response as real eventstream frames
// and requires the chunks the provider makes of it: text, a tool call whose
// arguments arrive in pieces, usage in the metadata event, and done (with the stop
// reason of messageStop, A-88).
func TestBedrockWireStream(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return streamReply(
			frame(t, "messageStart", `{"role":"assistant"}`),
			frame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hello "}}`),
			frame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"world"}}`),
			frame(t, "contentBlockStop", `{"contentBlockIndex":0}`),
			frame(t, "contentBlockStart", `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tu_1","name":"read_file"}}}`),
			frame(t, "contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"path\":"}}}`),
			frame(t, "contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"\"a.txt\"}"}}}`),
			frame(t, "contentBlockStop", `{"contentBlockIndex":1}`),
			frame(t, "messageStop", `{"stopReason":"tool_use"}`),
			frame(t, "metadata", `{"usage":{"inputTokens":12,"outputTokens":34,"totalTokens":46},"metrics":{"latencyMs":5}}`),
		)
	})
	p := realProvider(t, up, providerConfig())
	got := llmtest.ChunkTexts(t, drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{}))
	want := []string{
		`{"type":"text","content":"Hello "}`,
		`{"type":"text","content":"world"}`,
		`{"type":"tool_call_start","toolCallProgress":{"id":"tu_1","name":"read_file"}}`,
		`{"type":"tool_call_delta","toolCallProgress":{"id":"tu_1","name":"read_file","argumentsDelta":"{\"path\":"}}`,
		`{"type":"tool_call_delta","toolCallProgress":{"id":"tu_1","name":"read_file","argumentsDelta":"\"a.txt\"}"}}`,
		`{"type":"tool_call","toolCall":{"id":"tu_1","name":"read_file","arguments":{"path":"a.txt"}}}`,
		`{"type":"done","usage":{"inputTokens":12,"outputTokens":34},"stopReason":"tool_use"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("chunks differ\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
	reqs := up.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/model/us.anthropic.claude-sonnet-4-20250514-v1%3A0/converse-stream" {
		t.Errorf("requests: %+v", reqs)
	}
}

// TestBedrockWireStreamException serves an exception frame after some text and
// requires one in-band error chunk after the text, with the message of the
// service, and no done.
func TestBedrockWireStreamException(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return streamReply(
			frame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"partial"}}`),
			exceptionFrame(t, "internalServerException", `{"message":"the model broke"}`),
		)
	})
	p := realProvider(t, up, providerConfig())
	chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{})
	if len(chunks) != 2 || chunks[0].Type != kernel.ChunkText || chunks[1].Type != kernel.ChunkError {
		t.Fatalf("chunks %v, want a text chunk and one error chunk", chunks)
	}
	if chunks[1].Error != "the model broke" {
		t.Errorf("the error text is %q, want the service message", chunks[1].Error)
	}
}

// TestBedrockWireErrors serves the error documents of the service and requires the
// expected mapping: the name comes from the x-amzn-errortype header (with its URL
// suffix stripped by the SDK), the message from the body, the status from the
// response.
func TestBedrockWireErrors(t *testing.T) {
	const iamDenied = "User: arn:aws:iam::123456789012:user/dev is not authorized to perform: bedrock:InvokeModel on resource: x"
	cases := []struct {
		name      string
		status    int
		errType   string
		body      string
		wantMsg   string
		wantCode  kernel.ModelErrorCode
		retryable bool
	}{
		{
			"access denied", 403, "AccessDeniedException:http://internal.amazon.com/coral/com.amazon.bedrock/",
			`{"message":"` + iamDenied + `"}`,
			`AWS Bedrock access denied for model "us.anthropic.claude-sonnet-4-20250514-v1:0" (AccessDeniedException: ` + iamDenied + `)`,
			kernel.ModelCodeAuth, false,
		},
		{
			"invalid credentials", 403, "UnrecognizedClientException",
			`{"message":"The security token included in the request is invalid."}`,
			"Invalid AWS credentials (UnrecognizedClientException: The security token included in the request is invalid.)",
			kernel.ModelCodeAuth, false,
		},
		{
			"throttled", 429, "ThrottlingException", `{"message":"Too many requests"}`,
			"Rate limited", kernel.ModelCodeRateLimit, true,
		},
		{
			"model timeout", 408, "ModelTimeoutException", `{"message":"took too long"}`,
			"Request timeout", kernel.ModelCodeTimeout, true,
		},
		{
			"validation", 400, "ValidationException", `{"message":"The provided model identifier is invalid."}`,
			"The provided model identifier is invalid.", kernel.ModelCodeInvalidRequest, false,
		},
		{
			"internal server error", 500, "InternalServerException", `{"message":"boom"}`,
			"boom", kernel.ModelCodeProvider, true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				return llmtest.Reply{
					Status: c.status,
					Header: map[string]string{"Content-Type": "application/json", "X-Amzn-Errortype": c.errType, "Retry-After": "3"},
					Chunks: []string{c.body},
				}
			})
			p := realProvider(t, up, providerConfig())

			_, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hey"}}, llmhub.ModelOptions{})
			me := failureOf(t, err)
			if me.Message != c.wantMsg || me.Code != c.wantCode || me.Retryable != c.retryable {
				t.Errorf("Chat error\n got: %q %s %v\nwant: %q %s %v", me.Message, me.Code, me.Retryable, c.wantMsg, c.wantCode, c.retryable)
			}
			// The status and the Retry-After of the response are kept on the error,
			// and the AWS error is its cause.
			if me.StatusCode != c.status || me.RetryAfter != 3*time.Second {
				t.Errorf("Chat StatusCode %d RetryAfter %v, want %d 3s", me.StatusCode, me.RetryAfter, c.status)
			}
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || !kernel.HasCode(err, c.wantCode) {
				t.Errorf("Chat error: errors.As smithy.APIError = %v, HasCode(%s) = %v", apiErr != nil, c.wantCode, kernel.HasCode(err, c.wantCode))
			}

			chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "hey"}}, llmhub.ModelOptions{})
			if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || chunks[0].Error != c.wantMsg {
				t.Errorf("Stream chunks %v, want one error chunk with %q", chunks, c.wantMsg)
			}
		})
	}

	t.Run("a 403 whose body is not an AWS document is still an auth error", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return llmtest.Reply{Status: 403, Header: map[string]string{"Content-Type": "text/plain"}, Chunks: []string{"Forbidden by upstream proxy"}}
		})
		p := realProvider(t, up, providerConfig())
		_, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hey"}}, llmhub.ModelOptions{})
		me := failureOf(t, err)
		if me.Code != kernel.ModelCodeAuth || me.Retryable {
			t.Errorf("code %s retryable %v, want auth and not retryable", me.Code, me.Retryable)
		}
		const prefix = `AWS Bedrock access denied for model "us.anthropic.claude-sonnet-4-20250514-v1:0" (`
		if !strings.HasPrefix(me.Message, prefix) || strings.Contains(strings.ToLower(me.Message), "invalid aws credentials") {
			t.Errorf("message %q", me.Message)
		}
		// The text of the body is not kept by the SDK: the detail is the
		// decoding error, and it must not carry the endpoint.
		if strings.Contains(me.Message, up.URL) {
			t.Errorf("the message carries the endpoint: %q", me.Message)
		}
	})

	t.Run("list models: a 403 keeps the message of AWS and names no model", func(t *testing.T) {
		const denied = "User: arn:aws:iam::123456789012:user/dev is not authorized to perform: bedrock:ListFoundationModels"
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return llmtest.Reply{
				Status: 403,
				Header: map[string]string{"Content-Type": "application/json", "X-Amzn-Errortype": "AccessDeniedException"},
				Chunks: []string{`{"message":"` + denied + `"}`},
			}
		})
		p := realProvider(t, up, providerConfig())
		_, err := p.ListModels(t.Context())
		me := failureOf(t, err)
		if me.Code != kernel.ModelCodeAuth || me.Message != "AWS Bedrock access denied (AccessDeniedException: "+denied+")" {
			t.Errorf("error %+v", me)
		}
	})
}

// TestBedrockWireCancelMidStream serves one frame and then stops writing. When the
// caller cancels after the first chunk, the stream must end promptly with the
// caller's context.Canceled and no error chunk, and the real reader and its
// connection must be released (the package goleak check proves no goroutine is left).
func TestBedrockWireCancelMidStream(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		rep := streamReply(frame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"first"}}`))
		rep.Hang = true
		return rep
	})
	p := realProvider(t, up, providerConfig())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	start := time.Now()
	var texts []string
	var last error
	for c, err := range p.Stream(ctx, []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{}) {
		if err != nil {
			last = err
			continue
		}
		if c.Type == kernel.ChunkError || c.Type == kernel.ChunkDone {
			t.Fatalf("got a %s chunk after the cancel", c.Type)
		}
		texts = append(texts, *c.Content)
		cancel()
	}
	if len(texts) != 1 || texts[0] != "first" {
		t.Errorf("texts %v, want [first]", texts)
	}
	if !errors.Is(last, context.Canceled) {
		t.Errorf("the stream ended with %v, want context.Canceled", last)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the stream took %v to end after the cancel", elapsed)
	}
}

// TestBedrockIgnoresEnvironmentBearerToken sets every AWS variable that could change
// how a client authenticates or where it connects, with a trap server behind the
// endpoint variable, and requires that the request carries a SigV4 Authorization
// header made from the explicit access key id and region, sent to the explicit
// endpoint. The bearer token variable is the dangerous one: the SDK constructors
// read it and switch to bearer authentication unless the option function of
// client.go clears it. Both clients are checked: the runtime one (Chat, Stream) and
// the control-plane one (ListModels).
func TestBedrockIgnoresEnvironmentBearerToken(t *testing.T) {
	trap := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Status: 500} })
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bearer-token-from-the-environment")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-FROM-THE-ENVIRONMENT")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-from-the-environment")
	t.Setenv("AWS_SESSION_TOKEN", "session-from-the-environment")
	t.Setenv("AWS_REGION", "eu-west-3")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-3")
	t.Setenv("AWS_PROFILE", "profile-from-the-environment")
	t.Setenv("AWS_ENDPOINT_URL", trap.URL)
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK", trap.URL)
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", trap.URL)

	up := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		switch {
		case strings.HasSuffix(r.Path, "/converse-stream"):
			return streamReply(
				frame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"s"}}`),
				frame(t, "messageStop", `{"stopReason":"end_turn"}`),
			)
		case strings.HasSuffix(r.Path, "/converse"):
			return jsonReply(`{"output":{"message":{"role":"assistant","content":[{"text":"c"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)
		default:
			return jsonReply(`{"modelSummaries":[{"modelId":"m.one"}]}`)
		}
	})
	p := realProvider(t, up, providerConfig())

	if _, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "x"}}, llmhub.ModelOptions{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, c := range drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "x"}}, llmhub.ModelOptions{}) {
		if c.Type == kernel.ChunkError {
			t.Fatalf("Stream: %s", c.Error)
		}
	}
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	reqs := up.Requests()
	if len(reqs) != 3 {
		t.Fatalf("the endpoint saw %d requests, want 3 (Chat, Stream, ListModels)", len(reqs))
	}
	for _, r := range reqs {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
			t.Errorf("%s: Authorization %q does not start with AWS4-HMAC-SHA256", r.Path, auth)
		}
		if !strings.Contains(auth, "Credential=AKIA-TEST/") || !strings.Contains(auth, "/us-east-1/bedrock/aws4_request") {
			t.Errorf("%s: Authorization %q does not carry the explicit access key id and region", r.Path, auth)
		}
		for _, leak := range []string{"bearer", "FROM-THE-ENVIRONMENT", "eu-west-3"} {
			if strings.Contains(strings.ToLower(auth), strings.ToLower(leak)) {
				t.Errorf("%s: Authorization %q carries %q from the environment", r.Path, auth, leak)
			}
		}
		if tok := r.Header.Get("X-Amz-Security-Token"); tok != "" {
			t.Errorf("%s: a session token %q was sent", r.Path, tok)
		}
	}
	if n := len(trap.Requests()); n != 0 {
		t.Errorf("the endpoint of the environment received %d requests", n)
	}
}
