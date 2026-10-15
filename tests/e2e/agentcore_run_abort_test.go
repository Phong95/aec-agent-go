package e2e_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// abortToolProfile is the LoopConfig of the tool cases: two tool retries and no
// backoff, so a retry would show at once.
func abortToolProfile(p *profile.AgentProfile) {
	retries, backoff := 2, 0
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: &retries, RetryBackoffMs: &backoff}
}

// runCompletedAborted decodes the RunCompleted event of the log and reports its aborted
// flag.
func runCompletedAborted(t *testing.T, l *loopCore) bool {
	t.Helper()
	for _, e := range l.log.all() {
		if e.Type == events.RunCompleted.Name {
			p, err := events.PayloadAs[events.RunCompletedPayload](e)
			if err != nil {
				t.Fatalf("decode RunCompleted: %v", err)
			}
			return p.Aborted
		}
	}
	t.Fatalf("no RunCompleted event among %v", l.log.types())
	return false
}

// TestAgentCoreRunAbort holds the nine run-abort cases (A-65): they run against
// aec.AgentCore with Agent.Chat and Agent.ChatStream over an aecmock.ScriptedProvider,
// each under its own title. The abort signal is the ctx of the call. The Phase 9 rewrites of the same cases against the
// bare loop (agentloop TestStream_Abort* and TestRun_AbortWhileModelHangs) stay as the
// lower-level proof.
//
// The cases with a provider that stalls or a tool that never returns run in a synctest
// bubble, where they are blocked on virtual time, and every one disposes the core before
// its bubble ends.
func TestAgentCoreRunAbort(t *testing.T) {
	t.Run("abort mid model stream: ends promptly with done+aborted and a consistent partial history", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// "Hel", "lo", then the provider stalls: the stream never ends on its own.
			provider := []aecmock.ScriptedProviderOption{aecmock.WithHangAfterChunks(2)}
			l := newLoopCore(t, loopOptions{}, []aecmock.ScriptedTurn{{Chunks: []kernel.StreamChunk{
				textChunk("Hel"), textChunk("lo"), textChunk(" world"), {Type: kernel.ChunkDone},
			}}}, provider...)
			defer l.dispose(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			start := time.Now()
			texts := 0
			chunks, err := drainStreamEach(l.stream(ctx, "hi"), func(c kernel.StreamChunk) {
				if c.Type == kernel.ChunkText {
					texts++
					if texts == 2 {
						cancel()
					}
				}
			})
			if err != nil {
				t.Fatalf("a stop is a done chunk, never an error: %v", err)
			}

			requireAbortedDoneChunk(t, chunks)
			if elapsed := time.Since(start); elapsed != 0 {
				t.Fatalf("the stream ended after %v of fake time, want it to end at the instant of the stop", elapsed)
			}
			if n := len(l.provider.Calls()); n != 1 {
				t.Fatalf("the provider was called %d times, want 1", n)
			}

			msgs := l.stored(t)
			if len(msgs) != 2 {
				t.Fatalf("%d stored messages, want 2: %+v", len(msgs), msgs)
			}
			if msgs[0].Role != kernel.RoleUser || msgs[0].Content != "hi" {
				t.Fatalf("messages[0] = %+v, want the user message hi", msgs[0])
			}
			if msgs[1].Role != kernel.RoleAssistant || msgs[1].Content != "Hello" {
				t.Fatalf("messages[1] = %+v, want the assistant message with the streamed partial text Hello", msgs[1])
			}
			if msgs[1].ToolCalls != nil {
				t.Fatalf("messages[1].ToolCalls = %+v, want none", msgs[1].ToolCalls)
			}
			// Every span that was opened ended.
			if started, ended := l.spans.startedAndEnded(); started == 0 || started != ended {
				t.Fatalf("spans started %d, ended %d, want equal and not zero", started, ended)
			}
		})
	})

	t.Run("already aborted signal: persists the user message, never calls provider.stream(), yields exactly one done+aborted chunk", func(t *testing.T) {
		l := newLoopCore(t, loopOptions{}, []aecmock.ScriptedTurn{{Content: "never"}})
		defer l.dispose(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		chunks, err := drainStream(l.stream(ctx, "hi"))
		if err != nil {
			t.Fatalf("a stop is a done chunk, never an error: %v", err)
		}
		if len(chunks) != 1 {
			t.Fatalf("%d chunks, want exactly one", len(chunks))
		}
		requireAbortedDoneChunk(t, chunks)
		if n := len(l.provider.Calls()); n != 0 {
			t.Fatalf("the provider was called %d times, want 0", n)
		}
		msgs := l.stored(t)
		if len(msgs) != 1 || msgs[0].Role != kernel.RoleUser || msgs[0].Content != "hi" {
			t.Fatalf("stored messages = %+v, want only the user message hi", msgs)
		}
	})

	t.Run("no signal: completes normally with a done chunk that has no aborted flag", func(t *testing.T) {
		l := newLoopCore(t, loopOptions{}, []aecmock.ScriptedTurn{{Content: "hi there", Usage: &kernel.TokenUsage{InputTokens: 5, OutputTokens: 5}}})
		defer l.dispose(t)

		chunks, err := drainStream(l.stream(t.Context(), "hi"))
		if err != nil {
			t.Fatal(err)
		}
		last := chunks[len(chunks)-1]
		if last.Type != kernel.ChunkDone {
			t.Fatalf("last chunk = %+v, want a done chunk", last)
		}
		if last.Aborted {
			t.Fatal("the done chunk of a run that was not stopped carries the aborted flag")
		}
	})

	t.Run("tool call still being written when stopped is dropped: no dangling tool call in history", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			call := toolCallOf("tc-1", "ghost_tool", "{}")
			// One tool_call chunk, then the provider stalls and never sends done.
			l := newLoopCore(t, loopOptions{}, []aecmock.ScriptedTurn{{Chunks: []kernel.StreamChunk{
				{Type: kernel.ChunkToolCall, ToolCall: &call}, {Type: kernel.ChunkDone},
			}}}, aecmock.WithHangAfterChunks(1))
			defer l.dispose(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			sawToolCall := false
			chunks, err := drainStreamEach(l.stream(ctx, "hi"), func(c kernel.StreamChunk) {
				if c.Type == kernel.ChunkToolCall && !sawToolCall {
					sawToolCall = true
					cancel()
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			requireAbortedDoneChunk(t, chunks)

			// The model response never finished (no text, only a tool call that was never
			// dispatched): no assistant message is persisted at all, so there is no dangling
			// tool call and nothing for a tool result to attach to.
			msgs := l.stored(t)
			if len(msgs) != 1 || msgs[0].Role != kernel.RoleUser || msgs[0].Content != "hi" {
				t.Fatalf("stored messages = %+v, want only the user message hi", msgs)
			}
		})
	})

	t.Run("forwards the signal to the provider", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var mu sync.Mutex
			var providerCtx context.Context
			echo := newLoopTool("echo", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
				return toolhub.Output{Content: "ok"}, nil
			})
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{echo}}, []aecmock.ScriptedTurn{{Chunks: []kernel.StreamChunk{
				textChunk("Hel"), textChunk("lo"), textChunk(" world"), {Type: kernel.ChunkDone},
			}}},
				aecmock.WithHangAfterChunks(2),
				aecmock.WithCallHook(func(ctx context.Context, _ int) {
					mu.Lock()
					defer mu.Unlock()
					providerCtx = ctx
				}))
			defer l.dispose(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			texts := 0
			if _, err := drainStreamEach(l.stream(ctx, "hi"), func(c kernel.StreamChunk) {
				if c.Type == kernel.ChunkText {
					texts++
					if texts == 2 {
						cancel()
					}
				}
			}); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			got := providerCtx
			mu.Unlock()
			if got == nil {
				t.Fatal("the provider was never called")
			}
			// The run is stopped through the ctx the provider received, so that ctx is done
			// now.
			select {
			case <-got.Done():
			default:
				t.Fatal("the provider ctx is not done after the run was cancelled: it is not the run ctx")
			}
			// Normal model options still reach the provider alongside the signal.
			calls := l.provider.Calls()
			if len(calls) != 1 || len(calls[0].Options.Tools) != 1 || calls[0].Options.Tools[0].Name != "echo" {
				t.Fatalf("options reaching the provider = %+v, want the tool definitions", calls)
			}
		})
	})

	t.Run("abort while a tool runs: cancels the dispatch, persists a cancelled result, ends with done+aborted", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			dispatched := make(chan struct{})
			release := make(chan struct{})
			var mu sync.Mutex
			var toolCtx context.Context
			slow := newLoopTool("slow_tool", func(ctx context.Context, _ kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
				mu.Lock()
				toolCtx = ctx
				mu.Unlock()
				close(dispatched)
				<-release // ignores the signal: never returns on its own
				return toolhub.Output{Content: "never used"}, nil
			})
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{slow}, profile: abortToolProfile},
				[]aecmock.ScriptedTurn{toolTurn(toolCallOf("tc-1", "slow_tool", "{}"))})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var chunks []kernel.StreamChunk
			var streamErr error
			consumed := make(chan struct{})
			go func() {
				defer close(consumed)
				chunks, streamErr = drainStream(l.stream(ctx, "hi"))
			}()
			<-dispatched
			cancel()
			<-consumed

			if streamErr != nil {
				t.Fatalf("a stop is a done chunk, never an error: %v", streamErr)
			}
			requireAbortedDoneChunk(t, chunks)
			if n := len(l.provider.Calls()); n != 1 {
				t.Fatalf("the provider was called %d times, want 1", n)
			}
			if slow.executions() != 1 {
				t.Fatalf("the tool ran %d times, want 1", slow.executions())
			}
			mu.Lock()
			if toolCtx == nil || toolCtx.Err() == nil {
				t.Error("the tool ctx is not done: the cancel did not reach the dispatch")
			}
			mu.Unlock()

			msgs := l.stored(t)
			if len(msgs) != 3 {
				t.Fatalf("%d stored messages, want 3: %+v", len(msgs), msgs)
			}
			if msgs[0].Role != kernel.RoleUser || msgs[0].Content != "hi" {
				t.Fatalf("messages[0] = %+v, want the user message hi", msgs[0])
			}
			if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ID != "tc-1" || msgs[1].ToolCalls[0].Name != "slow_tool" || string(msgs[1].ToolCalls[0].Arguments) != "{}" {
				t.Fatalf("messages[1].ToolCalls = %+v, want the one call tc-1", msgs[1].ToolCalls)
			}
			if len(msgs[2].ToolResults) != 1 || msgs[2].ToolResults[0].ToolCallID != "tc-1" || msgs[2].ToolResults[0].IsError == nil || !*msgs[2].ToolResults[0].IsError {
				t.Fatalf("messages[2].ToolResults = %+v, want a cancelled error result for tc-1", msgs[2].ToolResults)
			}
			close(release)
			synctest.Wait()
			l.dispose(t)
		})
	})

	t.Run("abort while waiting for approval: fails closed, never executes, ends with done+aborted", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			send := newLoopTool("send_rfi", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
				return toolhub.Output{Content: "sent"}, nil
			})
			send.def.Risk = toolhub.RiskConsequential
			// The user never answers.
			handler := loopApproval(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
				<-release
				return toolhub.ApprovalDecision{Approved: true}, nil
			})
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{send}, interaction: handler, profile: abortToolProfile},
				[]aecmock.ScriptedTurn{toolTurn(toolCallOf("tc-1", "send_rfi", "{}"))})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			l.core.Events().On(events.ToolApprovalRequested.Name, func(kernel.Event) { cancel() })

			chunks, err := drainStream(l.stream(ctx, "hi"))
			if err != nil {
				t.Fatalf("a stop is a done chunk, never an error: %v", err)
			}
			if send.executions() != 0 {
				t.Fatal("the tool ran although nobody approved it")
			}
			requireAbortedDoneChunk(t, chunks)

			var results []kernel.ToolResult
			for _, m := range l.stored(t) {
				results = append(results, m.ToolResults...)
			}
			if len(results) != 1 || results[0].ToolCallID != "tc-1" || results[0].IsError == nil || !*results[0].IsError {
				t.Fatalf("stored tool results = %+v, want one error result for tc-1", results)
			}
			if !runCompletedAborted(t, l) {
				t.Fatal("RunCompleted does not say aborted")
			}
			close(release)
			synctest.Wait()
			l.dispose(t)
		})
	})

	t.Run("run() abort: resolves with aborted true and persists no assistant message for the cancelled call", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			// The provider never settles: it ignores the ctx and answers nothing.
			l := newLoopCore(t, loopOptions{}, []aecmock.ScriptedTurn{{Content: "never"}},
				aecmock.WithCallHook(func(context.Context, int) { <-release }))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			time.AfterFunc(5*time.Millisecond, cancel)

			start := time.Now()
			res, err := l.chat(ctx, "x")
			if err != nil {
				t.Fatalf("a stop is a result, never an error: %v", err)
			}
			if !res.Aborted {
				t.Fatalf("result = %+v, want aborted", res)
			}
			if res.Message.Content != "" {
				t.Fatalf("result message = %q, want empty", res.Message.Content)
			}
			if elapsed := time.Since(start); elapsed != 5*time.Millisecond {
				t.Fatalf("Chat returned after %v, want the instant of the stop (5ms)", elapsed)
			}
			msgs := l.stored(t)
			if len(msgs) != 1 || msgs[0].Role != kernel.RoleUser || msgs[0].Content != "x" {
				t.Fatalf("stored messages = %+v, want only the user message x", msgs)
			}
			close(release)
			synctest.Wait()
			l.dispose(t)
		})
	})

	t.Run("no retries after abort: aborting during the first attempt means no ToolRetry event follows", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			dispatched := make(chan struct{})
			flaky := newLoopTool("flaky_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
				close(dispatched)
				// A real delay so the stop lands while this attempt is still in flight, well
				// before it would resolve on its own.
				time.Sleep(20 * time.Millisecond)
				failed := true
				return toolhub.Output{Content: "transient failure", IsError: &failed}, nil
			})
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{flaky}, profile: abortToolProfile},
				[]aecmock.ScriptedTurn{toolTurn(toolCallOf("tc-1", "flaky_tool", "{}"))})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			consumed := make(chan struct{})
			go func() {
				defer close(consumed)
				_, _ = drainStream(l.stream(ctx, "hi")) // draining only
			}()
			<-dispatched
			cancel()
			<-consumed

			if flaky.executions() != 1 || l.log.count(events.ToolRetry.Name) != 0 {
				t.Fatalf("%d executions and %d ToolRetry events, want 1 and 0", flaky.executions(), l.log.count(events.ToolRetry.Name))
			}
			// Let the abandoned attempt finish: it must stay silent too.
			time.Sleep(time.Second)
			if flaky.executions() != 1 || l.log.count(events.ToolRetry.Name) != 0 {
				t.Fatalf("after the attempt ended: %d executions and %d ToolRetry events, want 1 and 0", flaky.executions(), l.log.count(events.ToolRetry.Name))
			}
			l.dispose(t)
		})
	})
}
