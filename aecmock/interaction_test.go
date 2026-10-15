package aecmock_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A ScriptedInteraction is a toolhub.InteractionHandler, so it can be set on a tool hub.
var _ toolhub.InteractionHandler = (*aecmock.ScriptedInteraction)(nil)

// TestScriptedInteraction_Defaults: with no option it gives the default answers:
// the answer Tower A and an approval with no reason, and it records both requests.
func TestScriptedInteraction_Defaults(t *testing.T) {
	s := aecmock.NewScriptedInteraction()

	resp, err := s.AskUser(t.Context(), toolhub.ClarifyRequest{SessionID: "s1", AgentID: "a1", Question: "Which tower?", Choices: []string{"A", "B"}})
	if err != nil || resp.Answer != "Tower A" {
		t.Fatalf("AskUser = %+v, %v, want the answer Tower A", resp, err)
	}
	dec, err := s.RequestApproval(t.Context(), toolhub.ApprovalRequest{SessionID: "s1", AgentID: "a1", ToolCallID: "tc-1", ToolName: "send_rfi", Risk: toolhub.RiskConsequential})
	if err != nil || !dec.Approved || dec.Reason != nil {
		t.Fatalf("RequestApproval = %+v, %v, want approved with no reason", dec, err)
	}

	asked := s.Asked()
	if len(asked) != 1 || asked[0].Question != "Which tower?" || !slices.Equal(asked[0].Choices, []string{"A", "B"}) {
		t.Errorf("Asked = %+v", asked)
	}
	approvals := s.Approvals()
	if len(approvals) != 1 || approvals[0].ToolName != "send_rfi" || approvals[0].Risk != toolhub.RiskConsequential {
		t.Errorf("Approvals = %+v", approvals)
	}
}

// TestScriptedInteraction_Functions: the options replace the answers, and a nil function
// or a nil option keeps the default.
func TestScriptedInteraction_Functions(t *testing.T) {
	boom := errors.New("boom")
	reason := "wrong recipient"
	s := aecmock.NewScriptedInteraction(
		aecmock.WithAnswerFunc(func(_ context.Context, req toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
			return toolhub.ClarifyResponse{Answer: "re: " + req.Question}, boom
		}),
		aecmock.WithApprovalFunc(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
			return toolhub.ApprovalDecision{Approved: false, Reason: &reason}, nil
		}),
		nil,
	)
	resp, err := s.AskUser(t.Context(), toolhub.ClarifyRequest{Question: "Q"})
	if !errors.Is(err, boom) || resp.Answer != "re: Q" {
		t.Errorf("AskUser = %+v, %v", resp, err)
	}
	dec, err := s.RequestApproval(t.Context(), toolhub.ApprovalRequest{})
	if err != nil || dec.Approved || dec.Reason == nil || *dec.Reason != reason {
		t.Errorf("RequestApproval = %+v, %v", dec, err)
	}
	if len(s.Asked()) != 1 {
		t.Error("a request whose answer failed is still recorded")
	}

	keep := aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(nil), aecmock.WithApprovalFunc(nil))
	if resp, _ := keep.AskUser(t.Context(), toolhub.ClarifyRequest{}); resp.Answer != "Tower A" {
		t.Errorf("a nil answer function must keep the default, got %q", resp.Answer)
	}
	if dec, _ := keep.RequestApproval(t.Context(), toolhub.ApprovalRequest{}); !dec.Approved {
		t.Error("a nil approval function must keep the default")
	}
}

// TestScriptedInteraction_RecordsCopies: the recorded requests do not alias the caller's
// slices, and the lists it returns are copies the caller may change.
func TestScriptedInteraction_RecordsCopies(t *testing.T) {
	s := aecmock.NewScriptedInteraction()
	choices := []string{"A", "B"}
	args := kernel.Arguments(`{"to":"x"}`)
	sb := &toolhub.ApprovalSandbox{ID: "docker:shared", Kind: sandboxhub.Kind("docker"), Isolated: true}

	if _, err := s.AskUser(t.Context(), toolhub.ClarifyRequest{Question: "Q", Choices: choices}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestApproval(t.Context(), toolhub.ApprovalRequest{ToolName: "t", Arguments: args, Sandbox: sb}); err != nil {
		t.Fatal(err)
	}

	// The caller changes its own values after the call.
	choices[0] = "changed"
	args[2] = 'X'
	sb.ID = "changed"

	asked := s.Asked()
	if asked[0].Choices[0] != "A" {
		t.Errorf("the recorded choices follow the caller: %v", asked[0].Choices)
	}
	approvals := s.Approvals()
	if string(approvals[0].Arguments) != `{"to":"x"}` {
		t.Errorf("the recorded arguments follow the caller: %s", approvals[0].Arguments)
	}
	if approvals[0].Sandbox == nil || approvals[0].Sandbox.ID != "docker:shared" {
		t.Errorf("the recorded sandbox follows the caller: %+v", approvals[0].Sandbox)
	}

	// The caller changes what the recorder returned.
	asked[0].Choices[0] = "mutated"
	asked[0].Question = "mutated"
	approvals[0].Arguments[2] = 'Y'
	approvals[0].Sandbox.ID = "mutated"
	if again := s.Asked(); again[0].Choices[0] != "A" || again[0].Question != "Q" {
		t.Errorf("Asked returned a shared slice: %+v", again[0])
	}
	if again := s.Approvals(); string(again[0].Arguments) != `{"to":"x"}` || again[0].Sandbox.ID != "docker:shared" {
		t.Errorf("Approvals returned a shared value: %+v", again[0])
	}
}

// TestScriptedInteraction_Concurrent: 8 goroutines ask and request approval at once, as
// the loop's parallel tool calls can; under -race nothing may race and every request is
// recorded exactly once.
func TestScriptedInteraction_Concurrent(t *testing.T) {
	s := aecmock.NewScriptedInteraction()
	const workers, each = 8, 25

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				id := strconv.Itoa(w*each + i)
				if _, err := s.AskUser(t.Context(), toolhub.ClarifyRequest{Question: id, Choices: []string{id}}); err != nil {
					t.Errorf("AskUser: %v", err)
				}
				if _, err := s.RequestApproval(t.Context(), toolhub.ApprovalRequest{ToolCallID: id}); err != nil {
					t.Errorf("RequestApproval: %v", err)
				}
				// Readers run next to the writers.
				_ = s.Asked()
				_ = s.Approvals()
			}
		})
	}
	wg.Wait()

	seenQ, seenA := map[string]int{}, map[string]int{}
	for _, r := range s.Asked() {
		seenQ[r.Question]++
	}
	for _, r := range s.Approvals() {
		seenA[r.ToolCallID]++
	}
	if len(seenQ) != workers*each || len(seenA) != workers*each {
		t.Fatalf("recorded %d distinct questions and %d approvals, want %d of each", len(seenQ), len(seenA), workers*each)
	}
	for id, n := range seenQ {
		if n != 1 || seenA[id] != 1 {
			t.Errorf("request %s recorded %d and %d times, want once", id, n, seenA[id])
		}
	}
}

// TestScriptedInteraction_BlockingFunctionDoesNotBlockTheRecorder: the answer functions run
// outside the mutex, so a function that waits for ctx never blocks Asked or another
// request.
func TestScriptedInteraction_BlockingFunctionDoesNotBlockTheRecorder(t *testing.T) {
	entered := make(chan struct{})
	s := aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(func(ctx context.Context, req toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
		if req.Question == "block" {
			close(entered)
			<-ctx.Done()
			return toolhub.ClarifyResponse{}, ctx.Err()
		}
		return toolhub.ClarifyResponse{Answer: "ok"}, nil
	}))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.AskUser(ctx, toolhub.ClarifyRequest{Question: "block"})
		done <- err
	}()
	<-entered

	// While the first call waits, a second request and the readers go through.
	if resp, err := s.AskUser(t.Context(), toolhub.ClarifyRequest{Question: "free"}); err != nil || resp.Answer != "ok" {
		t.Errorf("a second AskUser = %+v, %v", resp, err)
	}
	if got := len(s.Asked()); got != 2 {
		t.Errorf("Asked holds %d requests while one waits, want 2", got)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("the blocked call returned %v, want context.Canceled", err)
	}
}
