package bedrock

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-176: ModelOptions.ReasoningEffort reaches a Claude model on Bedrock as
// additionalModelRequestFields.output_config.effort, through the real client (the body
// the AWS SDK serializes), for Chat and Stream; it is left out when unset and for
// non-Anthropic models.
func TestRequestCarriesReasoningEffort(t *testing.T) {
	up := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		if strings.HasSuffix(r.Path, "/converse-stream") {
			return streamReply(frame(t, "messageStop", `{"stopReason":"end_turn"}`))
		}
		return jsonReply(`{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)
	})
	p := realProvider(t, up, providerConfig())
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}
	chat := func(opts llmhub.ModelOptions) {
		t.Helper()
		if _, err := p.Chat(t.Context(), msgs, opts); err != nil {
			t.Fatalf("Chat: %v", err)
		}
	}

	chat(llmhub.ModelOptions{Model: "anthropic.claude-opus-5-5", ReasoningEffort: "high"})
	drain(t, p, msgs, llmhub.ModelOptions{Model: "anthropic.claude-opus-5-5", ReasoningEffort: "xhigh"})
	chat(llmhub.ModelOptions{Model: "global.anthropic.claude-opus-4-5-20251101-v1:0", ReasoningEffort: "low"})
	chat(llmhub.ModelOptions{Model: "anthropic.claude-opus-5-5"})
	chat(llmhub.ModelOptions{Model: "openai.gpt-oss-120b-1:0", ReasoningEffort: "high"})

	reqs := up.Requests()
	if len(reqs) != 5 {
		t.Fatalf("got %d requests", len(reqs))
	}
	wants := []struct{ path, fields string }{
		// A-178: a model that thinks by default also asks for its summarized thinking text.
		{"/model/us.anthropic.claude-opus-5-5/converse", `"additionalModelRequestFields":{"output_config":{"effort":"high"},"thinking":{"display":"summarized","type":"adaptive"}}`},
		{"/model/us.anthropic.claude-opus-5-5/converse-stream", `"additionalModelRequestFields":{"output_config":{"effort":"xhigh"},"thinking":{"display":"summarized","type":"adaptive"}}`},
		{"/model/global.anthropic.claude-opus-4-5-20251101-v1%3A0/converse", `"additionalModelRequestFields":{"anthropic_beta":["effort-2025-11-24"],"output_config":{"effort":"low"}}`},
		{"/model/us.anthropic.claude-opus-5-5/converse", `"additionalModelRequestFields":{"thinking":{"display":"summarized","type":"adaptive"}}`},
	}
	for i, w := range wants {
		if reqs[i].Path != w.path || !strings.Contains(string(reqs[i].Body), w.fields) {
			t.Errorf("request %d: path %s body %s; want path %s with %s", i, reqs[i].Path, reqs[i].Body, w.path, w.fields)
		}
	}
	for _, r := range reqs[4:] {
		if strings.Contains(string(r.Body), "additionalModelRequestFields") {
			t.Errorf("request to %s carries additional fields: %s", r.Path, r.Body)
		}
	}
}
