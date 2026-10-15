package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// scriptedToolTurns is the script of a scripted tool-calling model: the model calls the
// tool toolName on each of the first n turns and then ends the turn with the text "done".
// An unbounded script (a tool call on every turn) is a script longer than any cap in the
// test.
func scriptedToolTurns(toolName string, n int) []aecmock.ScriptedTurn {
	var turns []aecmock.ScriptedTurn
	for i := range n {
		turns = append(turns, toolTurn(toolCallOf(fmt.Sprintf("tc-%d", i+1), toolName, "{}")))
	}
	return append(turns, aecmock.ScriptedTurn{Content: "done"})
}

// loopLimits sets the profile LoopConfig of the test agent; a nil field is left unset.
func loopLimits(maxIterations, maxToolRetries, retryBackoffMs *int) func(*profile.AgentProfile) {
	return func(p *profile.AgentProfile) {
		p.LoopConfig = &profile.LoopConfig{MaxIterations: maxIterations, MaxToolRetries: maxToolRetries, RetryBackoffMs: retryBackoffMs}
	}
}

func intRef(v int) *int { return &v }

// succeedingTool is a tool whose Execute always succeeds with "ok".
func succeedingTool(name string) *loopTool {
	return newLoopTool(name, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		return toolhub.Output{Content: "ok"}, nil
	})
}

// throwingTool is a tool whose Execute fails with "<name> crashed".
func throwingTool(name string) *loopTool {
	return newLoopTool(name, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		return toolhub.Output{}, errors.New(name + " crashed")
	})
}

// TestAgentCoreLoopConfig holds the end-to-end loop config cases:
// AgentProfile.LoopConfig honoured through Agent.Chat and Agent.ChatStream and through
// the tool dispatch of a real AgentCore. The resolution cases do not use AgentCore; they
// are profile.ResolveLoopConfig's own tests. Each case
// runs in a synctest bubble, so the retry backoff is measured on virtual time.
func TestAgentCoreLoopConfig(t *testing.T) {
	t.Run("caps chat() at maxIterations, emits exactly N Iteration events, and returns the fallback message", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{succeedingTool("test_tool")}, profile: loopLimits(intRef(2), nil, nil)},
				scriptedToolTurns("test_tool", 5))
			defer l.dispose(t)

			result, err := l.chat(t.Context(), "keep going")
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if result.Iterations != 2 {
				t.Fatalf("Iterations = %d, want 2", result.Iterations)
			}
			if want := "Reached maximum reasoning steps. Please refine your request."; result.Message.Content != want {
				t.Fatalf("message = %q, want %q", result.Message.Content, want)
			}
			if n := l.log.count(events.Iteration.Name); n != 2 {
				t.Fatalf("%d Iteration events, want exactly 2", n)
			}
			var maxReached []events.MaxIterationsPayload
			for _, e := range l.log.all() {
				if e.Type == events.MaxIterationsReached.Name {
					p, err := events.PayloadAs[events.MaxIterationsPayload](e)
					if err != nil {
						t.Fatal(err)
					}
					maxReached = append(maxReached, p)
				}
			}
			if len(maxReached) != 1 || maxReached[0].Iterations != 2 {
				t.Fatalf("MaxIterationsReached payloads = %+v, want one with iterations 2", maxReached)
			}
		})
	})

	t.Run("an unset loopConfig still stops at the default of 100 iterations", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{succeedingTool("test_tool")}}, scriptedToolTurns("test_tool", 102))
			defer l.dispose(t)

			result, err := l.chat(t.Context(), "keep going")
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if result.Iterations != 100 {
				t.Fatalf("Iterations = %d, want the default cap 100", result.Iterations)
			}
		})
	})

	t.Run("caps chatStream() at maxIterations: 1, yielding the max-iterations error chunk", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{succeedingTool("test_tool")}, profile: loopLimits(intRef(1), nil, nil)},
				scriptedToolTurns("test_tool", 3))
			defer l.dispose(t)

			// Go: the error chunk comes with a *kernel.SDKError carrying its text.
			chunks, err := drainStream(l.stream(t.Context(), "keep going"))
			var sdkErr *kernel.SDKError
			if !errors.As(err, &sdkErr) || sdkErr.Message != "Reached maximum reasoning steps." {
				t.Fatalf("ChatStream error = %v, want a *kernel.SDKError with the max-iterations text", err)
			}
			want := kernel.StreamChunk{Type: kernel.ChunkError, Error: "Reached maximum reasoning steps."}
			if last := chunks[len(chunks)-1]; !reflect.DeepEqual(last, want) {
				t.Fatalf("last chunk = %+v, want %+v", last, want)
			}
			if n := l.log.count(events.Iteration.Name); n != 1 {
				t.Fatalf("%d Iteration events, want 1", n)
			}
		})
	})

	t.Run("Agent.updateLoopConfig() changes the cap for the next turn, and undefined restores the default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{succeedingTool("test_tool")}, profile: loopLimits(intRef(2), nil, nil)},
				scriptedToolTurns("test_tool", 2+3+100+1))
			defer l.dispose(t)

			first, err := l.chat(t.Context(), "first")
			if err != nil || first.Iterations != 2 {
				t.Fatalf("first turn: Iterations = %d, %v, want 2", first.Iterations, err)
			}

			l.agent.UpdateLoopConfig(&profile.LoopConfig{MaxIterations: intRef(3)})
			second, err := l.chat(t.Context(), "second")
			if err != nil || second.Iterations != 3 {
				t.Fatalf("second turn: Iterations = %d, %v, want 3", second.Iterations, err)
			}

			l.agent.UpdateLoopConfig(nil)
			third, err := l.chat(t.Context(), "third")
			if err != nil || third.Iterations != 100 {
				t.Fatalf("third turn: Iterations = %d, %v, want the default 100", third.Iterations, err)
			}
		})
	})

	t.Run("maxToolRetries: 0 runs the failing tool exactly once, 0 retries, 1 failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			crasher := throwingTool("crasher")
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{crasher}, profile: loopLimits(nil, intRef(0), intRef(0))},
				scriptedToolTurns("crasher", 1))
			defer l.dispose(t)

			if _, err := l.chat(t.Context(), "use crasher"); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if crasher.executions() != 1 {
				t.Fatalf("the tool ran %d times, want 1", crasher.executions())
			}
			if n := l.log.count(events.ToolRetry.Name); n != 0 {
				t.Fatalf("%d ToolRetry events, want 0", n)
			}
			if n := l.log.count(events.ToolFailed.Name); n != 1 {
				t.Fatalf("%d ToolFailed events, want 1", n)
			}
		})
	})

	t.Run("maxToolRetries: 3 runs exactly 4 times, 3 retries, 1 failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			crasher := throwingTool("crasher")
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{crasher}, profile: loopLimits(nil, intRef(3), intRef(0))},
				scriptedToolTurns("crasher", 1))
			defer l.dispose(t)

			if _, err := l.chat(t.Context(), "use crasher"); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if crasher.executions() != 4 {
				t.Fatalf("the tool ran %d times, want 4", crasher.executions())
			}
			if n := l.log.count(events.ToolRetry.Name); n != 3 {
				t.Fatalf("%d ToolRetry events, want 3", n)
			}
			if n := l.log.count(events.ToolFailed.Name); n != 1 {
				t.Fatalf("%d ToolFailed events, want 1", n)
			}
		})
	})

	t.Run("retryBackoffMs: 0 finishes well under the default backoff time", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{throwingTool("crasher")}, profile: loopLimits(nil, intRef(2), intRef(0))},
				scriptedToolTurns("crasher", 1))
			defer l.dispose(t)

			start := time.Now()
			if _, err := l.chat(t.Context(), "use crasher"); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			elapsed := time.Since(start)

			// The defaults (500 ms base, 2 retries) would need at least 500 + 1000 ms; on
			// fake time the wait is exactly none.
			if elapsed != 0 {
				t.Fatalf("the turn took %v of fake time, want no backoff at all", elapsed)
			}
		})
	})

	t.Run("retryBackoffMs: 60 with 1 retry takes at least 55ms", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{throwingTool("crasher")}, profile: loopLimits(nil, intRef(1), intRef(60))},
				scriptedToolTurns("crasher", 1))
			defer l.dispose(t)

			start := time.Now()
			if _, err := l.chat(t.Context(), "use crasher"); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			elapsed := time.Since(start)

			if elapsed < 55*time.Millisecond {
				t.Fatalf("the turn took %v, want at least 55ms", elapsed)
			}
			// On fake time the one retry waits exactly the base backoff.
			if elapsed != 60*time.Millisecond {
				t.Fatalf("the turn took %v of fake time, want exactly 60ms for one retry", elapsed)
			}
		})
	})
}
