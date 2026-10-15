package bedrock

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Transport hardening of the Phase 8 review (WR-02): the AWS SDK deserialises an
// error document and a success body without a limit, so the clients get a wrapper
// that bounds both.

// TestBedrockErrorBodyIsBounded serves a 2 MiB error document for Chat, Stream and
// ListModels through the REAL clients and requires an error whose text is not longer
// than the 1 MiB cap plus the SDK's own framing (WR-02).
func TestBedrockErrorBodyIsBounded(t *testing.T) {
	body := `{"message":"` + strings.Repeat("e", 2<<20) + `"}`
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Status: 500, Header: map[string]string{"Content-Type": "application/json", "X-Amzn-Errortype": "InternalServerException"}, Chunks: []string{body}}
	})
	p := realProvider(t, up, providerConfig())
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "x"}}
	limit := llmtransport.MaxErrorBody + 4096

	_, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
	if me := failureOf(t, err); len(me.Message) > limit {
		t.Errorf("Chat message is %d bytes, want at most %d", len(me.Message), limit)
	}
	chunks := drain(t, p, msgs, llmhub.ModelOptions{})
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || len(chunks[0].Error) > limit {
		t.Errorf("stream gave %d chunks; the error is longer than %d bytes", len(chunks), limit)
	}
	_, err = p.ListModels(t.Context())
	if me := failureOf(t, err); len(me.Message) > limit {
		t.Errorf("ListModels message is %d bytes, want at most %d", len(me.Message), limit)
	}
}

// TestBedrockResponseBodyIsBounded serves a successful answer above the 32 MiB cap
// for Chat and ListModels and requires a failure rather than an unbounded read.
func TestBedrockResponseBodyIsBounded(t *testing.T) {
	big := strings.Repeat("p", llmtransport.MaxResponseBody)
	up := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		if strings.HasSuffix(r.Path, "/converse") {
			return jsonReply(`{"output":{"message":{"role":"assistant","content":[{"text":"` + big + `"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)
		}
		return jsonReply(`{"modelSummaries":[],"padding":"` + big + `"}`)
	})
	p := realProvider(t, up, providerConfig())
	if _, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "x"}}, llmhub.ModelOptions{}); err == nil {
		t.Error("Chat accepted a body above the cap")
	}
	if _, err := p.ListModels(t.Context()); err == nil {
		t.Error("ListModels accepted a body above the cap")
	}
}

// TestBedrockDefaultClientIsBounded builds both clients with no injected HTTP client
// and requires that the one the SDK resolved (its default client) is wrapped, so the
// bound holds for a production provider and not only for a test client.
func TestBedrockDefaultClientIsBounded(t *testing.T) {
	var o options
	rt := newRuntimeClient(providerConfig(), "us-east-1", &o).Options().HTTPClient
	ct := newControlClient(providerConfig(), "us-east-1", &o).Options().HTTPClient
	for name, c := range map[string]aws.HTTPClient{"runtime": rt, "control": ct} {
		if _, bare := c.(*awshttp.BuildableClient); bare {
			t.Errorf("%s client is the bare AWS client: its bodies are unbounded", name)
		}
		if got := fmt.Sprintf("%T", c); !strings.Contains(got, "llmtransport") {
			t.Errorf("%s client is %s, want the llmtransport wrapper", name, got)
		}
	}
}
