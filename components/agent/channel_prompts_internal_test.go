package agent

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// A-100 phase 2: no answer in time denies an approval and fails a question, and the
// prompt shows it.
func TestChannelPromptsTimeOut(t *testing.T) {
	var mu sync.Mutex
	var edits []string
	chat := promptChat{channelID: "c1", key: "c1\x00chat", target: channel.Target{ChatID: "chat"}, chatType: channel.ChatDM, initiator: channel.Sender{ID: "u1"}}
	p := newChannelPrompts(channelPromptsDeps{
		send: func(context.Context, promptChat, channel.OutboundMessage) (channel.MessageRef, bool) {
			return channel.MessageRef{ChatID: "chat", MessageID: "m1"}, true
		},
		edit: func(_ promptChat, _ channel.MessageRef, m channel.OutboundMessage) {
			mu.Lock()
			defer mu.Unlock()
			edits = append(edits, m.Text)
		},
		async:     func(f func()) { f() },
		buttons:   func(string) bool { return false },
		approvers: func(string) []string { return nil },
		agentName: func() string { return "Agent" },
		timeout:   20 * time.Millisecond,
	})
	decision, err := p.requestApproval(t.Context(), chat, toolhub.ApprovalRequest{SessionID: "s1", ToolCallID: "t1", ToolName: "delete_drawings", ToolDescription: "Delete.", Risk: toolhub.RiskConsequential})
	if err != nil || decision.Approved || decision.Reason == nil || *decision.Reason != "nobody answered in the chat within 1 min" {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	if _, err := p.askUser(t.Context(), chat, toolhub.ClarifyRequest{SessionID: "s1", Question: "Which?"}); err == nil || !strings.Contains(err.Error(), "nobody answered") {
		t.Fatalf("askUser err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(edits) == 0 || !regexp.MustCompile(`(?s)^Approval needed \(#1\).*nobody answered in the chat within 1 min$`).MatchString(edits[0]) || strings.Contains(edits[0], "/approve") {
		t.Fatalf("edits = %q", edits)
	}
}

// A stopped run cancels its prompt at once.
func TestChannelPromptsEndWithTheRun(t *testing.T) {
	chat := promptChat{channelID: "c1", key: "k", chatType: channel.ChatDM, initiator: channel.Sender{ID: "u1"}}
	p := newChannelPrompts(channelPromptsDeps{
		send: func(context.Context, promptChat, channel.OutboundMessage) (channel.MessageRef, bool) {
			return channel.MessageRef{}, true
		},
		edit:      func(promptChat, channel.MessageRef, channel.OutboundMessage) {},
		buttons:   func(string) bool { return true },
		approvers: func(string) []string { return nil },
		agentName: func() string { return "Agent" },
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	decision, err := p.requestApproval(ctx, chat, toolhub.ApprovalRequest{ToolName: "x"})
	if err != nil || decision.Approved || decision.Reason == nil || *decision.Reason != "the run was stopped" {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	if p.hasQuestion("k") {
		t.Fatal("a prompt is left")
	}
}
