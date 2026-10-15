package toolhub_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The fakes embed the interface they stand for, so only the one method a test
// calls is implemented; any other call would be a nil dereference and fail loudly.

type fakeSandboxView struct {
	sandboxhub.View
	id string
}

func (f fakeSandboxView) ID() string { return f.id }

type fakeArtifactView struct {
	artifact.View
	agent string
}

func (f fakeArtifactView) AgentID() string { return f.agent }

type fakeSkills struct {
	skill.AgentSkills
	names []string
}

func (f fakeSkills) List(context.Context) ([]skill.Skill, error) {
	out := make([]skill.Skill, 0, len(f.names))
	for _, n := range f.names {
		out = append(out, skill.Skill{Name: n})
	}
	return out, nil
}

type fakeInteraction struct {
	asked    []toolhub.ClarifyRequest
	approved []toolhub.ApprovalRequest
}

func (f *fakeInteraction) AskUser(_ context.Context, req toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	f.asked = append(f.asked, req)
	return toolhub.ClarifyResponse{Answer: "East"}, nil
}

func (f *fakeInteraction) RequestApproval(_ context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	f.approved = append(f.approved, req)
	return toolhub.ApprovalDecision{Approved: true}, nil
}

// authorizeAndRun registers h, authorizes one call with tc and executes it.
func authorizeAndRun(t *testing.T, h *stubTool, tc toolhub.Context) toolhub.Output {
	t.Helper()
	hub := toolhub.NewToolHub()
	if err := hub.Register(h); err != nil {
		t.Fatal(err)
	}
	args, err := kernel.NewArguments([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, decision, err := hub.Authorize(t.Context(), h.name, args, tc, []string{h.name})
	if err != nil || !decision.Allowed {
		t.Fatalf("authorize: decision %+v err %v", decision, err)
	}
	out, err := auth.Execute(t.Context())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out
}

func TestContextReachesLeafContracts(t *testing.T) {
	ih := &fakeInteraction{}
	var reached []string
	h := &stubTool{name: "leafy", exec: func(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
		sb, err := tc.GetSandbox(ctx)
		if err != nil {
			return toolhub.Output{}, err
		}
		reached = append(reached, "sandbox:"+sb.ID())
		reached = append(reached, "artifacts:"+tc.GetArtifacts().AgentID())
		skills, err := tc.Skills.List(ctx)
		if err != nil || len(skills) != 1 {
			t.Errorf("skills list: %v, %v", skills, err)
		}
		reached = append(reached, "skills:"+skills[0].Name)
		ans, err := tc.Interaction.AskUser(ctx, toolhub.ClarifyRequest{SessionID: tc.SessionID, Question: "Which wing?"})
		if err != nil {
			return toolhub.Output{}, err
		}
		reached = append(reached, "ask:"+ans.Answer)
		dec, err := tc.Interaction.RequestApproval(ctx, toolhub.ApprovalRequest{SessionID: tc.SessionID, ToolName: "leafy"})
		if err != nil {
			return toolhub.Output{}, err
		}
		if !dec.Approved {
			t.Error("fake approval must approve")
		}
		reached = append(reached, "approval")
		return toolhub.Output{Content: "done"}, nil
	}}

	out := authorizeAndRun(t, h, toolhub.Context{
		SessionID: "s1",
		AgentID:   "a1",
		GetSandbox: func(context.Context) (sandboxhub.View, error) {
			return fakeSandboxView{id: "docker:shared"}, nil
		},
		GetArtifacts: func() artifact.View { return fakeArtifactView{agent: "a1"} },
		Skills:       fakeSkills{names: []string{"deploy"}},
		Interaction:  ih,
	})
	if out.Content != "done" {
		t.Fatalf("output = %+v", out)
	}
	want := []string{"sandbox:docker:shared", "artifacts:a1", "skills:deploy", "ask:East", "approval"}
	if len(reached) != len(want) {
		t.Fatalf("reached = %v, want %v", reached, want)
	}
	for i := range want {
		if reached[i] != want[i] {
			t.Fatalf("reached = %v, want %v", reached, want)
		}
	}
	if len(ih.asked) != 1 || ih.asked[0].SessionID != "s1" || len(ih.approved) != 1 {
		t.Errorf("interaction calls: asked %+v approved %+v", ih.asked, ih.approved)
	}
}

// TestContextGetSandboxError proves a failing GetSandbox reaches the handler as
// an error (fail closed), not as a nil view.
func TestContextGetSandboxError(t *testing.T) {
	boom := errors.New("no sandbox for this agent")
	h := &stubTool{name: "nosb", exec: func(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
		sb, err := tc.GetSandbox(ctx)
		if sb != nil || !errors.Is(err, boom) {
			t.Errorf("GetSandbox = %v, %v; want nil and the error", sb, err)
		}
		return toolhub.Output{Content: "handled"}, nil
	}}
	authorizeAndRun(t, h, toolhub.Context{
		SessionID:  "s1",
		GetSandbox: func(context.Context) (sandboxhub.View, error) { return nil, boom },
	})
}

// TestContextNilLeafFields proves that an unwired Context hands the handler nil
// capabilities - never a default host implementation (T-05-11) - and that Emit is
// replaced with a no-op.
func TestContextNilLeafFields(t *testing.T) {
	ran := false
	h := &stubTool{name: "bare", exec: func(_ context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
		ran = true
		if tc.GetSandbox != nil || tc.GetArtifacts != nil || tc.Skills != nil || tc.Interaction != nil {
			t.Errorf("unwired leaf fields must stay nil: %+v", tc)
		}
		if tc.Emit == nil {
			t.Error("Hub.Authorize must replace a nil Emit with a no-op")
		} else {
			tc.Emit("x", nil)
		}
		return toolhub.Output{Content: "ok"}, nil
	}}
	authorizeAndRun(t, h, toolhub.Context{SessionID: "s1"})
	if !ran {
		t.Fatal("handler did not run")
	}
}

func TestHubInteractionHandler(t *testing.T) {
	hub := toolhub.NewToolHub()
	if hub.InteractionHandler() != nil {
		t.Fatal("a new hub has no interaction handler")
	}
	ih := &fakeInteraction{}
	hub.SetInteractionHandler(ih)
	if got := hub.InteractionHandler(); got != toolhub.InteractionHandler(ih) {
		t.Fatalf("InteractionHandler = %v, want the one set", got)
	}
	hub.SetInteractionHandler(nil)
	if hub.InteractionHandler() != nil {
		t.Fatal("SetInteractionHandler(nil) must clear the handler")
	}
	var zero toolhub.ToolHub
	zero.SetInteractionHandler(ih)
	if zero.InteractionHandler() == nil {
		t.Fatal("a zero-value hub must accept an interaction handler")
	}
}
