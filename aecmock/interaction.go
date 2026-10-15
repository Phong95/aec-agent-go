package aecmock

import (
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that ScriptedInteraction is a toolhub.InteractionHandler.
var _ toolhub.InteractionHandler = (*ScriptedInteraction)(nil)

// InteractionOption changes how a ScriptedInteraction answers. Options are applied in
// order by NewScriptedInteraction; a nil option is ignored.
type InteractionOption func(*ScriptedInteraction)

// WithAnswerFunc sets the function that answers AskUser (clarify prompts). The default
// answers "Tower A". A nil function keeps the default.
func WithAnswerFunc(f func(ctx context.Context, req toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error)) InteractionOption {
	return func(s *ScriptedInteraction) {
		if f != nil {
			s.answer = f
		}
	}
}

// WithApprovalFunc sets the function that answers RequestApproval. The default approves
// with no reason. A nil function keeps the default.
func WithApprovalFunc(f func(ctx context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error)) InteractionOption {
	return func(s *ScriptedInteraction) {
		if f != nil {
			s.approval = f
		}
	}
}

// ScriptedInteraction is a public toolhub.InteractionHandler for tests: it records every
// request and answers it with a configurable function, so a test can drive a clarify
// prompt or an approval through a real AgentCore without a human user, then inspect
// the recorded calls. Set it on the tool hub with
// toolhub.ToolHub.SetInteractionHandler; it receives both the clarify prompts and the approval
// requests of the agent loop.
//
// It is safe for concurrent use: the agent loop may run several tool calls at once (up
// to agentloop.WithMaxParallelTools), so two requests can overlap. A mutex guards the
// recorded requests only. The answer functions are called outside the mutex, so a
// function that blocks never blocks the recorder or another request. A function must
// honour ctx, which is cancelled when the run stops, like a real handler.
//
// The zero value is not usable; create one with NewScriptedInteraction.
type ScriptedInteraction struct {
	answer   func(ctx context.Context, req toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error)
	approval func(ctx context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error)

	mu        sync.Mutex
	asked     []toolhub.ClarifyRequest
	approvals []toolhub.ApprovalRequest
}

// NewScriptedInteraction returns a ScriptedInteraction. With no option it answers every
// clarify prompt with "Tower A" and approves every request.
func NewScriptedInteraction(opts ...InteractionOption) *ScriptedInteraction {
	s := &ScriptedInteraction{
		answer: func(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
			return toolhub.ClarifyResponse{Answer: "Tower A"}, nil
		},
		approval: func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
			return toolhub.ApprovalDecision{Approved: true}, nil
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// AskUser records a deep copy of the request, then returns what the answer function
// returns for it.
func (s *ScriptedInteraction) AskUser(ctx context.Context, req toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	s.mu.Lock()
	s.asked = append(s.asked, cloneClarify(req))
	s.mu.Unlock()
	return s.answer(ctx, req)
}

// RequestApproval records a deep copy of the request, then returns what the approval
// function returns for it.
func (s *ScriptedInteraction) RequestApproval(ctx context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	s.mu.Lock()
	s.approvals = append(s.approvals, cloneApproval(req))
	s.mu.Unlock()
	return s.approval(ctx, req)
}

// Asked returns the clarify requests received so far, in order. The result and every
// slice in it are copies, so the caller may change them freely.
func (s *ScriptedInteraction) Asked() []toolhub.ClarifyRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]toolhub.ClarifyRequest, len(s.asked))
	for i, r := range s.asked {
		out[i] = cloneClarify(r)
	}
	return out
}

// Approvals returns the approval requests received so far, in order. The result and
// everything it holds, the argument bytes included, are copies.
func (s *ScriptedInteraction) Approvals() []toolhub.ApprovalRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]toolhub.ApprovalRequest, len(s.approvals))
	for i, r := range s.approvals {
		out[i] = cloneApproval(r)
	}
	return out
}

// cloneClarify copies the choices of a request.
func cloneClarify(r toolhub.ClarifyRequest) toolhub.ClarifyRequest {
	r.Choices = slices.Clone(r.Choices)
	return r
}

// cloneApproval copies the argument bytes and the sandbox of a request.
func cloneApproval(r toolhub.ApprovalRequest) toolhub.ApprovalRequest {
	r.Arguments = kernel.Arguments(slices.Clone([]byte(r.Arguments)))
	if r.Sandbox != nil {
		sb := *r.Sandbox
		r.Sandbox = &sb
	}
	return r
}
