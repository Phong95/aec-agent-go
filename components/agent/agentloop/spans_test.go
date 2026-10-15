package agentloop_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// requireEverySpanClosed requires that the run left no span open: at least one span
// was started, and every started span id was ended exactly once. The service ends a
// span at most once, so "exactly once" holds as soon as it ended; what this catches is
// a span that was started and never ended on some exit path.
func requireEverySpanClosed(t *testing.T, r *rig) {
	t.Helper()
	starts := map[string]int{}
	ends := map[string]int{}
	for _, rec := range r.sink.all() {
		switch rec.phase {
		case "start":
			starts[rec.span.SpanID]++
		case "end":
			ends[rec.span.SpanID]++
		}
	}
	if len(starts) == 0 {
		t.Fatal("no span was started: the check proves nothing")
	}
	for id, n := range starts {
		if n != 1 || ends[id] != 1 {
			t.Errorf("span %s started %d times and ended %d times, want once each", id, n, ends[id])
		}
	}
	for id := range ends {
		if starts[id] == 0 {
			t.Errorf("span %s ended but never started", id)
		}
	}
}

// failingContext is a Context port whose PrepareMessages fails.
type failingContext struct {
	agentloop.Context
	err error
}

func (c failingContext) PrepareMessages(context.Context, []kernel.Message, agentloop.MiddlewareContext) ([]kernel.Message, error) {
	return nil, c.err
}

// TestEverySpanClosedOnEveryExitPath: whatever way a run ends (a finished turn, the
// iteration cap, a model failure, a failed write, a failed port, every kind of stop,
// a consumer that stops iterating), every span it opened is closed, on failure paths
// too.
func TestEverySpanClosedOnEveryExitPath(t *testing.T) {
	echo := func() *fakeTool { return constTool("echo", toolhub.Output{Content: "ok"}) }
	capOne := func() *profile.AgentProfile {
		p := testProfile("test-agent")
		p.LoopConfig = &profile.LoopConfig{MaxIterations: intPtr(1)}
		return &p
	}
	cancelled := func(t *testing.T) context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}
	portErr := errors.New("context port failed")
	runOnce := func(r *rig, ctx context.Context) {
		_, _ = r.loop.Run(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
	}
	toolTurns := func() []aecmock.ScriptedTurn {
		return []aecmock.ScriptedTurn{{ToolCalls: []kernel.ToolCall{callOf("tc-1", "slow_tool", "{}")}}, {Content: "never"}}
	}

	// Each case runs one exit path and checks the spans. Cases that wait on a blocked
	// fake run in their own bubble.
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"Run: end_turn", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "ok"})
			runOnce(r, t.Context())
			requireEverySpanClosed(t, r)
		}},
		{"Run: iteration cap", func(t *testing.T) {
			r := newRig(t, rigOptions{prof: capOne()}, aecmock.ScriptedTurn{Content: "cut off", StopReason: kernel.StopMaxTokens})
			runOnce(r, t.Context())
			requireEverySpanClosed(t, r)
		}},
		{"Run: model failure", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Err: errors.New("boom")})
			runOnce(r, t.Context())
			requireEverySpanClosed(t, r)
		}},
		{"Run: failed write", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "ok"})
				// The user message is stored, the reply write blocks and times out.
				loop := r.withDeps(t, func(d *agentloop.Deps) { d.Conversation = &hangAfterConversation{Conversation: r.conv, after: 1} })
				_, err := loop.Run(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Run error = %v, want the persist deadline", err)
				}
				requireEverySpanClosed(t, r)
			})
		}},
		{"Run: failed port", func(t *testing.T) {
			r := newRig(t, rigOptions{})
			loop := r.withDeps(t, func(d *agentloop.Deps) { d.Context = failingContext{Context: d.Context, err: portErr} })
			if _, err := loop.Run(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}); !errors.Is(err, portErr) {
				t.Fatalf("Run error = %v, want the port's error", err)
			}
			requireEverySpanClosed(t, r)
		}},
		{"Run: stopped before the call", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never"})
			runOnce(r, cancelled(t))
			requireEverySpanClosed(t, r)
		}},
		{"Run: stopped during the model call", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, rigOptions{})
				hang := newHangingModels(r.models)
				loop := r.withDeps(t, func(d *agentloop.Deps) { d.Models = hang })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				time.AfterFunc(time.Millisecond, cancel)
				if res, err := loop.Run(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}); err != nil || !res.Aborted {
					t.Fatalf("Run = %+v, %v", res, err)
				}
				requireEverySpanClosed(t, r)
				close(hang.release)
				synctest.Wait()
			})
		}},
		{"Run: stopped during a tool", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				slow := newFakeTool("slow_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
					cancel()
					<-release
					return toolhub.Output{}, nil
				})
				r := newRig(t, rigOptions{handlers: []toolhub.Handler{slow}}, toolTurns()...)
				if res, err := r.loop.Run(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}); err != nil || !res.Aborted {
					t.Fatalf("Run = %+v, %v", res, err)
				}
				requireEverySpanClosed(t, r)
				close(release)
				synctest.Wait()
			})
		}},
		{"Stream: end_turn", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "ok"})
			_, _ = r.stream(t, "hi")
			requireEverySpanClosed(t, r)
		}},
		{"Stream: iteration cap", func(t *testing.T) {
			r := newRig(t, rigOptions{prof: capOne(), handlers: []toolhub.Handler{echo()}}, aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-1", "echo", "{}")}})
			_, _ = r.stream(t, "hi")
			requireEverySpanClosed(t, r)
		}},
		{"Stream: error chunk", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Err: errors.New("boom")})
			_, _ = r.stream(t, "hi")
			requireEverySpanClosed(t, r)
		}},
		{"Stream: failed write", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "ok"})
				loop := r.withDeps(t, func(d *agentloop.Deps) { d.Conversation = &hangAfterConversation{Conversation: r.conv, after: 1} })
				if _, err := drain(loop.Stream(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Stream error = %v, want the persist deadline", err)
				}
				requireEverySpanClosed(t, r)
			})
		}},
		{"Stream: failed port", func(t *testing.T) {
			r := newRig(t, rigOptions{})
			loop := r.withDeps(t, func(d *agentloop.Deps) { d.Context = failingContext{Context: d.Context, err: portErr} })
			if _, err := drain(loop.Stream(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})); !errors.Is(err, portErr) {
				t.Fatalf("Stream error = %v, want the port's error", err)
			}
			requireEverySpanClosed(t, r)
		}},
		{"Stream: stopped before the call", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never"})
			_, _ = drain(r.seq(cancelled(t), "hi"))
			requireEverySpanClosed(t, r)
		}},
		{"Stream: stopped mid-stream", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "one two three"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, _ = drainEach(r.seq(ctx, "hi"), func(kernel.StreamChunk) { cancel() })
			requireEverySpanClosed(t, r)
		}},
		{"Stream: stopped during a tool", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				slow := newFakeTool("slow_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
					cancel()
					<-release
					return toolhub.Output{}, nil
				})
				r := newRig(t, rigOptions{handlers: []toolhub.Handler{slow}}, toolTurns()...)
				_, _ = drain(r.seq(ctx, "hi"))
				requireEverySpanClosed(t, r)
				close(release)
				synctest.Wait()
			})
		}},
		{"Stream: consumer stops iterating", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "one two three"})
			for range r.seq(t.Context(), "hi") {
				break
			}
			requireEverySpanClosed(t, r)
		}},
		{"Stream: consumer stops after a tool result", func(t *testing.T) {
			r := newRig(t, rigOptions{handlers: []toolhub.Handler{echo()}},
				aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-1", "echo", "{}")}}, aecmock.ScriptedTurn{Content: "never"})
			for c := range r.seq(t.Context(), "hi") {
				if c.Type == kernel.ChunkToolResult {
					break
				}
			}
			requireEverySpanClosed(t, r)
		}},
		{"Stream: panic in the consumer", func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "one two three"})
			func() {
				defer func() { _ = recover() }()
				for range r.seq(t.Context(), "hi") {
					panic("consumer failed")
				}
			}()
			requireEverySpanClosed(t, r)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, c.run)
	}
}
