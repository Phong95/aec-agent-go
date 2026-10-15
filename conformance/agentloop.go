package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// AgentLoopTurn is one scripted model answer of an AgentLoopScenario.
type AgentLoopTurn struct {
	// Content is the answer text.
	Content string
	// ToolCalls are the tool calls the answer asks for.
	ToolCalls []kernel.ToolCall
	// StopReason is why the answer ended. Empty lets the model script choose
	// (tool_use for an answer with tool calls, end_turn otherwise).
	StopReason kernel.StopReason
}

// AgentLoopScenario is what the suite asks a factory to build: a model that
// answers the scripted turns, the agent's tools and skills, and its limits.
//
// The scripted model works as follows. The n-th model call
// consumes the n-th turn. Chat answers with the turn's content, tool calls and
// stop reason and a usage of 10 input and 5 output tokens. Stream yields each
// word of the content followed by one space as a text chunk, then one tool_call
// chunk per call, then a done chunk without usage. A call past the last turn
// fails. No context middleware changes the messages, no memory is recalled and
// the compactor config is the default one (a 200000 token window with a 0.8
// threshold), which these short runs never reach.
type AgentLoopScenario struct {
	// AgentID and SessionID identify the agent (its profile id, stamped on every
	// event) and the session the suite runs in. The suite fills them with
	// test-agent and test-session when a case leaves them empty, so a factory
	// always sees both. The factory must start the harness's session under
	// SessionID (for example with an id source that returns it): the suite passes
	// SessionID to Run and Stream, and the loop refuses a sessionID that is not the
	// id of the conversation's active session (agentloop.ErrSessionNotActive).
	AgentID, SessionID string
	// Turns are the scripted model answers.
	Turns []AgentLoopTurn
	// Tools are registered with the agent's tool hub and allowed for the agent.
	Tools []toolhub.Handler
	// SkillTools are the skill tools (skill_view and friends) the agent's skills
	// hand to the harness. The loop routes them as skill dispatches.
	SkillTools []toolhub.Handler
	// Interaction is the host's channel to the human user (clarify and approval).
	// Nil means there is no interactive user.
	Interaction toolhub.InteractionHandler
	// LoopConfig is the agent's loop limits. Nil means the SDK defaults (10
	// iterations, 2 tool retries, 500 ms backoff).
	LoopConfig *profile.LoopConfig
	// MaxParallelTools bounds the tool calls of one model response that run at
	// the same time. Zero means the agentloop default.
	MaxParallelTools int
}

// AgentLoopHarness is the loop a factory built from a scenario, with the views
// the suite needs. Every field is built inside the subtest's synctest bubble (see
// AgentLoop). The active session of the loop's conversation must have the id
// AgentLoopScenario.SessionID, because that is the id the suite runs the loop with.
type AgentLoopHarness struct {
	// Loop is the loop under test.
	Loop *agentloop.Loop
	// Bus is the event bus the loop emits to.
	Bus events.Bus
	// Trace is the trace service the loop records spans on. Nil skips the
	// span-closure subtest, with a message naming this field.
	Trace *trace.Service
	// Messages returns the persisted history of the active session, in order.
	Messages func() []kernel.Message
	// ModelCalls returns how many times the model was called.
	ModelCalls func() int
}

// agentLoopFactory is the factory type of AgentLoop, named for the helpers.
type agentLoopFactory func(t *testing.T, s AgentLoopScenario) AgentLoopHarness

// The defaults the suite fills into a scenario.
const (
	defaultAgentID   = "test-agent"
	defaultSessionID = "test-session"
)

// AgentLoop runs the conformance suite for the agent loop (agentloop.Loop): the
// 15 core cases, then the subtests for cancellation, the parallel bound,
// panics, approval, backoff timing and span closure, each suffixed " (Go-only)".
//
// newHarness builds the whole loop from a scenario, so the suite needs no
// knowledge of how the models, tools or conversation are made; the suite itself
// never imports a test double. The scripted model, the in-memory conversation
// and the rest come from the caller (the aecmock package does this for the SDK).
//
// Every subtest runs in its own testing/synctest bubble, so the retry timings (a
// 500 ms then 1000 ms backoff) cost no wall time and every wait is
// deterministic. For that reason the factory receives the subtest's *testing.T:
// synctest forbids t.Run inside a bubble, so the suite calls t.Run outside and
// synctest.Test inside it, and the fakes, their timers and their t.Cleanup
// registrations must belong to the bubble's t. The factory may be called more
// than once in one subtest (the run/stream equivalence cases build two loops). The
// factory starts the harness's session under s.SessionID, which the suite passes to
// Run and Stream (a loop refuses a session that is not the active one). A
// factory that starts goroutines must stop them before the subtest body returns:
// a bubble that ends with a live goroutine deadlocks. A goroutine leak check
// such as goleak belongs in the package's TestMain, outside every bubble.
func AgentLoop(t *testing.T, newHarness func(t *testing.T, s AgentLoopScenario) AgentLoopHarness) {
	t.Helper()

	mk := agentLoopFactory(newHarness)

	// -- 1. Basic lifecycle: a simple question and answer with no tools --

	runCase(t, mk, "run() returns a final message on end_turn",
		AgentLoopScenario{Turns: []AgentLoopTurn{{Content: "Hello back!", StopReason: kernel.StopEndTurn}}},
		func(t *testing.T, r runner) {
			res, err := r.run(newCtx(t), "Hello")
			check.NoErr(t, err)
			check.Equal(t, res.Message.Role, kernel.RoleAssistant)
			check.Equal(t, res.Message.Content, "Hello back!")
			check.Equal(t, res.Iterations, 1)
		})

	runCase(t, mk, "stream() yields text chunks and a done chunk",
		AgentLoopScenario{Turns: []AgentLoopTurn{endTurn("Hello back!")}},
		func(t *testing.T, r runner) {
			chunks := drainStream(t, r.stream(newCtx(t), "Hello"))
			check.True(t, countType(chunks, kernel.ChunkText) > 0, "no text chunk was yielded")
			// The loop yields its own terminal done chunk.
			check.True(t, countType(chunks, kernel.ChunkDone) >= 1, "no done chunk was yielded")
			if len(chunks) > 0 {
				check.Equal(t, chunks[len(chunks)-1].Type, kernel.ChunkDone, "the last chunk")
			}
		})

	// -- 2. Tool exposure: an unregistered tool returns an error, not a crash --

	runCase(t, mk, "unregistered tool call returns error result to the model",
		AgentLoopScenario{Turns: []AgentLoopTurn{
			toolTurn(callOf("tc-1", "nonexistent_tool", "{}")),
			endTurn("I see the tool was not available."),
		}},
		func(t *testing.T, r runner) {
			res, err := r.run(newCtx(t), "Use a tool")
			check.NoErr(t, err)

			// The loop did not crash: it returned a final message.
			check.Equal(t, res.Message.Role, kernel.RoleAssistant)
			check.Equal(t, res.Iterations, 2)

			// The tool result was an error fed back to the model.
			results := toolResults(r.Messages())
			check.True(t, len(results) > 0 && isError(results[0]), "no persisted tool result with isError")
		})

	runCase(t, mk, "unregistered tool via stream() returns error result, not crash",
		AgentLoopScenario{Turns: []AgentLoopTurn{
			toolTurn(callOf("tc-1", "nonexistent_tool", "{}")),
			endTurn("Acknowledged."),
		}},
		func(t *testing.T, r runner) {
			chunks := drainStream(t, r.stream(newCtx(t), "Use a tool"))

			// The stream did not crash: it completed with a done chunk.
			if len(chunks) > 0 {
				check.Equal(t, chunks[len(chunks)-1].Type, kernel.ChunkDone, "the last chunk")
			}

			// The error tool result was yielded as a chunk.
			var results []kernel.ToolResult
			for _, c := range chunks {
				if c.Type == kernel.ChunkToolResult && c.ToolResult != nil {
					results = append(results, *c.ToolResult)
				}
			}
			check.Len(t, results, 1, "tool_result chunks")
			check.True(t, len(results) == 1 && isError(results[0]), "the tool_result chunk is not an error")
		})

	// -- 3. run()/stream() equivalence: the same input produces equivalent state --

	runBubble(t, "run() and stream() produce identical persisted messages", func(t *testing.T) {
		echo := mockTool("echo", toolhub.Output{Content: "echoed: test"})
		scenario := AgentLoopScenario{
			Turns: []AgentLoopTurn{
				toolTurn(callOf("tc-1", "echo", `{"input":"test"}`)),
				endTurn("Done with the tool."),
			},
			Tools: []toolhub.Handler{echo},
		}
		viaRun := mk.build(t, scenario)
		viaStream := mk.build(t, scenario)

		_, err := viaRun.run(newCtx(t), "Use echo")
		check.NoErr(t, err)
		drainStream(t, viaStream.stream(newCtx(t), "Use echo"))

		// Compare the structural shape: the role sequence and the presence of tool
		// calls and results. The content may differ slightly (stream adds a trailing
		// space per word).
		check.DeepEqual(t, normalize(viaRun.Messages()), normalize(viaStream.Messages()))
	})

	runBubble(t, "run() and stream() produce equivalent event type sequences", func(t *testing.T) {
		scenario := AgentLoopScenario{Turns: []AgentLoopTurn{endTurn("Simple answer.")}}
		viaRun := mk.build(t, scenario)
		viaStream := mk.build(t, scenario)
		runEvents := collectEvents(viaRun.Bus)
		streamEvents := collectEvents(viaStream.Bus)

		_, err := viaRun.run(newCtx(t), "Hi")
		check.NoErr(t, err)
		drainStream(t, viaStream.stream(newCtx(t), "Hi"))

		check.DeepEqual(t, runEvents.types(), streamEvents.types())
		check.DeepEqual(t, runEvents.types(), simpleRunEvents)
	})

	// -- 4. Edge cases: retry, max iterations, parallel tools, skill routing --

	runCase(t, mk, "retries a tool that returns isError, up to MAX_TOOL_RETRIES",
		AgentLoopScenario{
			// The tool fails twice and then succeeds (MAX_TOOL_RETRIES is 2, so 3
			// attempts in total).
			Tools: []toolhub.Handler{failingTool("flaky", 2)},
			Turns: []AgentLoopTurn{
				toolTurn(callOf("tc-1", "flaky", "{}")),
				endTurn("Got the result."),
			},
		},
		func(t *testing.T, r runner) {
			log := collectEvents(r.Bus)
			res, err := r.run(newCtx(t), "Use flaky tool")
			check.NoErr(t, err)
			check.Equal(t, res.Message.Content, "Got the result.")

			// The retry events were emitted.
			check.Equal(t, log.count(events.ToolRetry.Name), 2, "ToolRetry events")

			// The final tool-end was emitted, not tool-error.
			check.Equal(t, log.count(events.ToolCompleted.Name), 1, "ToolCompleted events")
		})

	runCase(t, mk, "surfaces error after exhausting all retries on a throwing tool",
		AgentLoopScenario{
			Tools: []toolhub.Handler{throwingTool("crasher")},
			Turns: []AgentLoopTurn{
				toolTurn(callOf("tc-1", "crasher", "{}")),
				endTurn("I see the error."),
			},
		},
		func(t *testing.T, r runner) {
			log := collectEvents(r.Bus)
			res, err := r.run(newCtx(t), "Use crasher")
			check.NoErr(t, err)
			check.Equal(t, res.Message.Role, kernel.RoleAssistant)

			// tool-error was emitted, not tool-end.
			check.Equal(t, log.count(events.ToolFailed.Name), 1, "ToolFailed events")

			// The error result was fed back to the model.
			results := toolResults(r.Messages())
			check.True(t, len(results) > 0 && isError(results[0]), "no persisted tool result with isError")
			if len(results) > 0 {
				check.True(t, strings.Contains(results[0].Content, "crasher crashed"),
					"the result %q does not contain the tool's failure text", results[0].Content)
			}
		})

	// Script 101 tool turns: the loop must stop at the default cap of 100 with the
	// fallback instead of asking for the 101st.
	capTurns := make([]AgentLoopTurn, 101)
	for i := range capTurns {
		capTurns[i] = toolTurn(callOf("tc-"+strconv.Itoa(i), "echo", "{}"))
	}
	runCase(t, mk, "terminates with fallback when max iterations are exceeded",
		AgentLoopScenario{Tools: []toolhub.Handler{mockTool("echo", toolhub.Output{Content: "ok"})}, Turns: capTurns},
		func(t *testing.T, r runner) {
			log := collectEvents(r.Bus)
			res, err := r.run(newCtx(t), "Keep going")
			check.NoErr(t, err)
			check.Equal(t, res.Iterations, 100)
			check.True(t, strings.Contains(res.Message.Content, "maximum"), "the fallback %q does not mention the maximum", res.Message.Content)

			// The max-iterations event was emitted, the complete event was not.
			check.Equal(t, log.count(events.MaxIterationsReached.Name), 1, "MaxIterationsReached events")
			check.Equal(t, log.count(events.RunCompleted.Name), 0, "RunCompleted events")
		})

	runBubble(t, "executes multiple tool calls in parallel", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		record := func(entry string) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, entry)
		}
		// Each tool waits until both have started (a barrier, not a wall-clock
		// sleep). A loop that ran the calls one after another never reaches the
		// barrier: the wait ends by the timeout and the order check fails.
		var started atomic.Int32
		both := make(chan struct{})
		barrier := func(name string) *suiteTool {
			return newTool(name, strings.ToUpper(name[:1])+name[1:], func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
				record(name + "-start")
				if started.Add(1) == 2 {
					close(both)
				}
				select {
				case <-both:
				case <-time.After(time.Second):
				}
				record(name + "-end")
				return toolhub.Output{Content: name + " done"}, nil
			})
		}
		r := mk.build(t, AgentLoopScenario{
			Tools: []toolhub.Handler{barrier("alpha"), barrier("beta")},
			Turns: []AgentLoopTurn{
				toolTurn(callOf("tc-a", "alpha", "{}"), callOf("tc-b", "beta", "{}")),
				endTurn("Both done."),
			},
		})
		_, err := r.run(newCtx(t), "Run both")
		check.NoErr(t, err)

		mu.Lock()
		got := slices.Clone(order)
		mu.Unlock()
		alphaStart, betaStart := slices.Index(got, "alpha-start"), slices.Index(got, "beta-start")
		alphaEnd, betaEnd := slices.Index(got, "alpha-end"), slices.Index(got, "beta-end")

		// Both started.
		check.True(t, alphaStart >= 0 && betaStart >= 0, "not both tools started: %v", got)

		// Parallel: both start before both end.
		check.True(t, max(alphaStart, betaStart) < min(alphaEnd, betaEnd), "the calls did not overlap: %v", got)

		// Both results appear in a single message.
		found := false
		for _, m := range r.Messages() {
			if len(m.ToolResults) == 2 {
				found = true
			}
		}
		check.True(t, found, "no persisted message holds both results")
	})

	runCase(t, mk, "routes skill_view to the skill manager as a skill dispatch",
		AgentLoopScenario{
			SkillTools: []toolhub.Handler{skillViewTool(map[string]string{"generate-report": "Report instructions"})},
			Turns: []AgentLoopTurn{
				toolTurn(callOf("tc-1", "skill_view", `{"name":"generate-report"}`)),
				endTurn("Report is ready."),
			},
		},
		func(t *testing.T, r runner) {
			log := collectEvents(r.Bus)
			_, err := r.run(newCtx(t), "Generate report")
			check.NoErr(t, err)

			// Skill events were emitted, not tool events.
			check.Equal(t, log.count(events.SkillDispatched.Name), 1, "SkillDispatched events")
			check.Equal(t, log.count(events.ToolDispatched.Name), 0, "ToolDispatched events")

			// The result was fed back to the model.
			found := false
			for _, res := range toolResults(r.Messages()) {
				if res.Content == "Report instructions" {
					found = true
				}
			}
			check.True(t, found, "the skill's instructions were not fed back to the model")
		})

	// -- 5. Event emission order: the right sequence per iteration --

	runCase(t, mk, "emits events in correct order for a simple end_turn",
		AgentLoopScenario{Turns: []AgentLoopTurn{endTurn("Answer.")}},
		func(t *testing.T, r runner) {
			log := collectEvents(r.Bus)
			_, err := r.run(newCtx(t), "Hi")
			check.NoErr(t, err)
			check.DeepEqual(t, log.types(), simpleRunEvents)
		})

	runCase(t, mk, "emits tool-start/end between model-response and next iteration",
		AgentLoopScenario{
			Tools: []toolhub.Handler{mockTool("echo", toolhub.Output{Content: "echoed"})},
			Turns: []AgentLoopTurn{
				toolTurn(callOf("tc-1", "echo", "{}")),
				endTurn("Done."),
			},
		},
		func(t *testing.T, r runner) {
			log := collectEvents(r.Bus)
			_, err := r.run(newCtx(t), "Use echo")
			check.NoErr(t, err)
			types := log.types()

			// The order is model-response, tool-start, tool-end, iteration 2.
			modelResponseIdx := slices.Index(types, events.ModelResponded.Name)
			toolStartIdx := slices.Index(types, events.ToolDispatched.Name)
			toolEndIdx := slices.Index(types, events.ToolCompleted.Name)
			secondIterationIdx := -1
			if modelResponseIdx >= 0 {
				if i := slices.Index(types[modelResponseIdx+1:], events.Iteration.Name); i >= 0 {
					secondIterationIdx = modelResponseIdx + 1 + i
				}
			}
			check.True(t, modelResponseIdx >= 0 && toolStartIdx >= 0 && toolEndIdx >= 0 && secondIterationIdx >= 0,
				"a required event is missing: %v", types)
			check.True(t, modelResponseIdx < toolStartIdx, "tool-start before model-response: %v", types)
			check.True(t, toolStartIdx < toolEndIdx, "tool-end before tool-start: %v", types)
			check.True(t, toolEndIdx < secondIterationIdx, "iteration 2 before tool-end: %v", types)
		})

	runCase(t, mk, "all emitted events carry the agentId in payload",
		AgentLoopScenario{AgentID: "my-agent-42", Turns: []AgentLoopTurn{endTurn("Answer.")}},
		func(t *testing.T, r runner) {
			log := collectEvents(r.Bus)
			_, err := r.run(newCtx(t), "Hi")
			check.NoErr(t, err)

			all := log.all()
			check.True(t, len(all) > 0, "no event was emitted")
			for _, e := range all {
				b, err := kernel.EncodeJSON(e.Payload)
				check.NoErr(t, err, "encode the %s payload", e.Type)
				var payload map[string]any
				check.NoErr(t, json.Unmarshal(b, &payload), "decode the %s payload", e.Type)
				check.Equal(t, fmt.Sprint(payload["agentId"]), "my-agent-42", e.Type+" agentId")
			}
		})

	// -- 6. Token usage accumulation --

	runCase(t, mk, "accumulates usage across iterations",
		AgentLoopScenario{
			Tools: []toolhub.Handler{mockTool("echo", toolhub.Output{Content: "ok"})},
			Turns: []AgentLoopTurn{
				toolTurn(callOf("tc-1", "echo", "{}")),
				endTurn("Done."),
			},
		},
		func(t *testing.T, r runner) {
			res, err := r.run(newCtx(t), "Go")
			check.NoErr(t, err)

			// The scripted model answers 10 input and 5 output tokens per turn: 2
			// turns make 20 and 10.
			check.Equal(t, res.Usage.InputTokens, 20)
			check.Equal(t, res.Usage.OutputTokens, 10)
		})

	// -- Go-only: stopping a run --

	// The model script gives four text chunks ("one ", "two ", "three ", "four ").
	fourWords := AgentLoopScenario{Turns: []AgentLoopTurn{endTurn("one two three four")}}

	runCase(t, mk, "consumer break runs the aborted cleanup (Go-only)", fourWords, func(t *testing.T, r runner) {
		log := collectEvents(r.Bus)
		var seen []kernel.StreamChunk
		texts := 0
		for c, err := range r.stream(newCtx(t), "go") {
			check.NoErr(t, err)
			seen = append(seen, c)
			if c.Type == kernel.ChunkText {
				texts++
				if texts == 2 {
					break
				}
			}
		}
		check.Equal(t, countType(seen, kernel.ChunkDone), 0, "done chunks received")

		// The cleanup ran when the loop was left: one RunCompleted marked aborted,
		// and the two chunks the consumer received are the persisted partial reply.
		completed := runCompleted(t, log)
		check.Len(t, completed, 1, "RunCompleted events")
		if len(completed) == 1 {
			check.True(t, completed[0].Aborted, "RunCompleted is not marked aborted")
		}
		msgs := r.Messages()
		check.Len(t, msgs, 2, "persisted messages")
		if len(msgs) == 2 {
			check.Equal(t, msgs[0].Role, kernel.RoleUser)
			check.Equal(t, msgs[1].Role, kernel.RoleAssistant)
			check.Equal(t, msgs[1].Content, "one two ", "the partial reply")
		}
	})

	runCase(t, mk, "ctx cancel mid-stream returns done aborted and persists the partial reply (Go-only)", fourWords, func(t *testing.T, r runner) {
		log := collectEvents(r.Bus)
		ctx, cancel := newCancelCtx(t)
		var chunks []kernel.StreamChunk
		var received strings.Builder
		texts := 0
		for c, err := range r.stream(ctx, "go") {
			// A stop is a done chunk, never an error.
			check.NoErr(t, err)
			chunks = append(chunks, c)
			if c.Type == kernel.ChunkText && c.Content != nil {
				received.WriteString(*c.Content)
				texts++
				if texts == 2 {
					cancel()
				}
			}
		}
		check.Equal(t, countType(chunks, kernel.ChunkDone), 1, "done chunks")
		if n := len(chunks); n > 0 {
			last := chunks[n-1]
			check.Equal(t, last.Type, kernel.ChunkDone, "the last chunk")
			check.True(t, last.Aborted, "the done chunk is not marked aborted")
		}

		// The partial reply is what the consumer received, and it is persisted.
		msgs := r.Messages()
		check.Len(t, msgs, 2, "persisted messages")
		if len(msgs) == 2 {
			check.Equal(t, msgs[1].Content, received.String(), "the persisted partial reply")
			check.NotEqual(t, msgs[1].Content, "one two three four ", "the partial reply is the whole answer")
		}
		completed := runCompleted(t, log)
		check.Len(t, completed, 1, "RunCompleted events")
		if len(completed) == 1 {
			check.True(t, completed[0].Aborted, "RunCompleted is not marked aborted")
		}
	})

	runCase(t, mk, "already cancelled ctx persists the user message and makes no model call (Go-only)", fourWords, func(t *testing.T, r runner) {
		ctx, cancel := newCancelCtx(t)
		cancel()
		res, err := r.run(ctx, "hi")
		check.NoErr(t, err)
		check.True(t, res.Aborted, "the result is not marked aborted")
		check.Equal(t, res.Iterations, 0)
		check.Equal(t, r.ModelCalls(), 0, "model calls")
		msgs := r.Messages()
		check.Len(t, msgs, 1, "persisted messages")
		if len(msgs) == 1 {
			check.Equal(t, msgs[0].Role, kernel.RoleUser)
			check.Equal(t, msgs[0].Content, "hi")
		}
	})

	runCase(t, mk, "Run returns Aborted, not an error (Go-only)", fourWords, func(t *testing.T, r runner) {
		ctx, cancel := newCancelCtx(t)
		// The subscriber stops the run when the model is about to be asked.
		r.Bus.On(events.ModelRequested.Name, func(kernel.Event) { cancel() })
		res, err := r.run(ctx, "hi")
		check.NoErr(t, err, "a stop is a result, never an error")
		check.True(t, res.Aborted, "the result is not marked aborted")
		check.Equal(t, res.Message.Role, kernel.RoleAssistant)
		check.Equal(t, res.Message.Content, "")
	})

	runBubble(t, "a tool that ignores ctx does not delay the abort (Go-only)", func(t *testing.T) {
		ctx, cancel := newCancelCtx(t)
		release := make(chan struct{})
		// The tool stops the run and then blocks on a channel without looking at ctx.
		stuck := newTool("stuck_tool", "Stuck tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			cancel()
			<-release
			return toolhub.Output{Content: "late"}, nil
		})
		r := mk.build(t, AgentLoopScenario{
			Tools: []toolhub.Handler{stuck},
			Turns: []AgentLoopTurn{toolTurn(callOf("tc-1", "stuck_tool", "{}")), endTurn("never")},
		})

		start := time.Now()
		res, err := r.run(ctx, "hi")
		check.NoErr(t, err)
		check.True(t, res.Aborted, "the result is not marked aborted")
		check.Equal(t, time.Since(start), time.Duration(0), "time between the stop and the return of Run")
		results := toolResults(r.Messages())
		check.Len(t, results, 1, "persisted tool results")
		if len(results) == 1 {
			check.Equal(t, results[0].Content, "Cancelled: the user stopped the run before this tool call finished.")
		}

		// Release the tool so the bubble can end.
		close(release)
		synctest.Wait()
	})

	// -- Go-only: the tool pipeline --

	runBubble(t, "parallel calls never exceed the bound (Go-only)", func(t *testing.T) {
		var inflight, peak atomic.Int32
		gate := newTool("gate", "Gate", func(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
			n := inflight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			inflight.Add(-1)
			var in struct {
				N int `json:"n"`
			}
			if err := args.Decode(&in); err != nil {
				return toolhub.Output{}, err
			}
			return toolhub.Output{Content: "result-" + strconv.Itoa(in.N)}, nil
		})
		calls := make([]kernel.ToolCall, 6)
		for i := range calls {
			calls[i] = callOf("tc-"+strconv.Itoa(i), "gate", fmt.Sprintf(`{"n":%d}`, i))
		}
		r := mk.build(t, AgentLoopScenario{
			Tools:            []toolhub.Handler{gate},
			MaxParallelTools: 2,
			Turns:            []AgentLoopTurn{toolTurn(calls...), endTurn("Done.")},
		})
		_, err := r.run(newCtx(t), "go")
		check.NoErr(t, err)

		// The bound holds, and it is reached, so the calls did run side by side.
		check.Equal(t, int(peak.Load()), 2, "highest number of calls in flight")

		// The results keep the order of the calls.
		results := toolResults(r.Messages())
		check.Len(t, results, 6, "persisted tool results")
		for i, res := range results {
			check.Equal(t, res.ToolCallID, "tc-"+strconv.Itoa(i), "result "+strconv.Itoa(i)+" call id")
			check.Equal(t, res.Content, "result-"+strconv.Itoa(i), "result "+strconv.Itoa(i)+" content")
		}
	})

	runCase(t, mk, "a panicking tool is recovered and not retried (Go-only)",
		AgentLoopScenario{
			Tools: []toolhub.Handler{newTool("exploder", "Exploder", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
				panic("boom")
			})},
			Turns: []AgentLoopTurn{toolTurn(callOf("tc-1", "exploder", "{}")), endTurn("Survived.")},
		},
		func(t *testing.T, r runner) {
			log := collectEvents(r.Bus)
			res, err := r.run(newCtx(t), "go")

			// The turn never crashes: the run completes with the model's next answer.
			check.NoErr(t, err)
			check.Equal(t, res.Message.Content, "Survived.")
			// A panic is final (never retried) and the model sees a generic text, not the
			// panic value, which may hold a credential or a payload fragment.
			check.Equal(t, log.count(events.ToolRetry.Name), 0, "ToolRetry events")
			check.Equal(t, log.count(events.ToolFailed.Name), 1, "ToolFailed events")
			results := toolResults(r.Messages())
			check.Len(t, results, 1, "persisted tool results")
			if len(results) == 1 {
				check.True(t, isError(results[0]), "the result is not an error")
				check.Equal(t, results[0].Content, "tool exploder failed: internal error")
			}
		})

	runBubble(t, "approval gates a consequential tool until the scripted decision arrives (Go-only)", func(t *testing.T) {
		handler := &approvalHandler{decide: make(chan toolhub.ApprovalDecision)}
		send := newTool("send_rfi", "Send an RFI", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			return toolhub.Output{Content: "sent"}, nil
		})
		send.def.Risk = toolhub.RiskConsequential
		r := mk.build(t, AgentLoopScenario{
			Tools:       []toolhub.Handler{send},
			Interaction: handler,
			Turns:       []AgentLoopTurn{toolTurn(callOf("tc-1", "send_rfi", "{}")), endTurn("Sent.")},
		})
		log := collectEvents(r.Bus)

		type outcome struct {
			res agentloop.RunResult
			err error
		}
		done := make(chan outcome, 1)
		ctx := newCtx(t)
		go func() {
			res, err := r.run(ctx, "Send the RFI")
			done <- outcome{res, err}
		}()

		// The run waits for the user: the request is out, the tool has not run.
		synctest.Wait()
		check.Len(t, handler.requests(), 1, "approval requests")
		check.Equal(t, log.count(events.ToolApprovalRequested.Name), 1, "ToolApprovalRequested events")
		check.Equal(t, send.executions(), 0, "executions before the decision")
		select {
		case <-done:
			t.Fatal("the run finished before the decision arrived")
		default:
		}

		// The scripted decision arrives and the tool runs once.
		handler.decide <- toolhub.ApprovalDecision{Approved: true}
		out := <-done
		check.NoErr(t, out.err)
		check.Equal(t, send.executions(), 1, "executions after the decision")
		check.Equal(t, log.count(events.ToolApprovalResolved.Name), 1, "ToolApprovalResolved events")
		results := toolResults(r.Messages())
		check.Len(t, results, 1, "persisted tool results")
		if len(results) == 1 {
			check.Equal(t, results[0].Content, "sent")
		}
	})

	runBubble(t, "retry backoff is 500 then 1000 ms (Go-only)", func(t *testing.T) {
		var mu sync.Mutex
		var at []time.Time
		flaky := newTool("flaky", "Flaky", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			mu.Lock()
			defer mu.Unlock()
			at = append(at, time.Now())
			if len(at) <= 2 {
				return toolhub.Output{Content: "transient failure", IsError: ptr(true)}, nil
			}
			return toolhub.Output{Content: "ok"}, nil
		})
		r := mk.build(t, AgentLoopScenario{
			Tools: []toolhub.Handler{flaky},
			Turns: []AgentLoopTurn{toolTurn(callOf("tc-1", "flaky", "{}")), endTurn("Got it.")},
		})
		start := time.Now()
		_, err := r.run(newCtx(t), "go")
		check.NoErr(t, err)

		// The attempts are 500 ms and then 1000 ms apart, and the whole turn takes
		// exactly their sum in fake time.
		mu.Lock()
		defer mu.Unlock()
		check.Len(t, at, 3, "attempts")
		if len(at) == 3 {
			check.Equal(t, at[1].Sub(at[0]), 500*time.Millisecond, "backoff before the second attempt")
			check.Equal(t, at[2].Sub(at[1]), 1000*time.Millisecond, "backoff before the third attempt")
		}
		check.Equal(t, time.Since(start), 1500*time.Millisecond, "the whole turn")
	})

	// -- Go-only: every opened span is closed --

	t.Run("every opened span is closed on every exit path (Go-only)", func(t *testing.T) {
		skip := ""
		synctest.Test(t, func(t *testing.T) {
			endTurnRun := mk.build(t, AgentLoopScenario{Turns: []AgentLoopTurn{endTurn("Done.")}})
			if endTurnRun.Trace == nil {
				skip = "AgentLoopHarness.Trace is nil: the harness records no spans, so span closure cannot be checked"
				return
			}

			// An end_turn answer.
			ledger := watchSpans(endTurnRun)
			_, err := endTurnRun.run(newCtx(t), "Hi")
			check.NoErr(t, err)
			ledger.check(t, "end_turn")

			// The iteration cap.
			capped := mk.build(t, AgentLoopScenario{
				Tools: []toolhub.Handler{mockTool("echo", toolhub.Output{Content: "ok"})},
				Turns: capTurns,
			})
			ledger = watchSpans(capped)
			_, err = capped.run(newCtx(t), "Keep going")
			check.NoErr(t, err)
			ledger.check(t, "the iteration cap")

			// An abort.
			aborted := mk.build(t, AgentLoopScenario{Turns: []AgentLoopTurn{endTurn("never")}})
			ledger = watchSpans(aborted)
			ctx, cancel := newCancelCtx(t)
			aborted.Bus.On(events.ModelRequested.Name, func(kernel.Event) { cancel() })
			res, err := aborted.run(ctx, "Hi")
			check.NoErr(t, err)
			check.True(t, res.Aborted, "the abort path did not abort")
			ledger.check(t, "an abort")

			// A consumer that stops reading a stream.
			broken := mk.build(t, fourWords)
			ledger = watchSpans(broken)
			for c, err := range broken.stream(newCtx(t), "go") {
				check.NoErr(t, err)
				if c.Type == kernel.ChunkText {
					break
				}
			}
			ledger.check(t, "a consumer break")
		})
		if skip != "" {
			t.Skip(skip)
		}
	})
}

// -- Harness plumbing ---------------------------------------------------------

// runner is a built harness together with the scenario it was built from.
type runner struct {
	AgentLoopHarness
	scenario AgentLoopScenario
}

// run is Loop.Run with the scenario's session and no overrides.
func (r runner) run(ctx context.Context, userMessage string) (agentloop.RunResult, error) {
	return r.Loop.Run(ctx, userMessage, r.scenario.SessionID, llmhub.ModelOptions{}, agentloop.RunOptions{})
}

// stream is Loop.Stream with the scenario's session and no overrides.
func (r runner) stream(ctx context.Context, userMessage string) iter.Seq2[kernel.StreamChunk, error] {
	return r.Loop.Stream(ctx, userMessage, r.scenario.SessionID, llmhub.ModelOptions{}, agentloop.RunOptions{})
}

// withDefaults fills the identity fields of a scenario.
func withDefaults(s AgentLoopScenario) AgentLoopScenario {
	if s.AgentID == "" {
		s.AgentID = defaultAgentID
	}
	if s.SessionID == "" {
		s.SessionID = defaultSessionID
	}
	return s
}

// build asks the factory for a harness for the scenario. It is called inside a
// bubble.
func (mk agentLoopFactory) build(t *testing.T, s AgentLoopScenario) runner {
	t.Helper()
	s = withDefaults(s)
	return runner{AgentLoopHarness: mk(t, s), scenario: s}
}

// runBubble runs one subtest: t.Run outside, a synctest bubble inside.
func runBubble(t *testing.T, title string, body func(t *testing.T)) {
	t.Helper()
	t.Run(title, func(t *testing.T) {
		synctest.Test(t, body)
	})
}

// runCase runs one subtest over a harness built from the scenario inside the
// bubble.
func runCase(t *testing.T, mk agentLoopFactory, title string, s AgentLoopScenario, body func(t *testing.T, r runner)) {
	t.Helper()
	runBubble(t, title, func(t *testing.T) {
		body(t, mk.build(t, s))
	})
}

// newCtx returns a context created inside the bubble and cancelled when the
// subtest ends.
func newCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, _ := newCancelCtx(t)
	return ctx
}

// newCancelCtx returns a cancellable context created inside the bubble, which is
// also cancelled when the subtest ends.
func newCancelCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

// drainStream collects a stream and fails the test on any yielded error: a run
// that is not stopped never uses the error slot.
func drainStream(t *testing.T, seq iter.Seq2[kernel.StreamChunk, error]) []kernel.StreamChunk {
	t.Helper()
	var out []kernel.StreamChunk
	for c, err := range seq {
		check.NoErr(t, err, "stream yielded an error")
		out = append(out, c)
	}
	return out
}

// simpleRunEvents is the event sequence of a run that answers once with end_turn.
var simpleRunEvents = []string{
	events.UserMessage.Name,
	events.Iteration.Name,
	events.ModelRequested.Name,
	events.ModelResponded.Name,
	// A-104: the context breakdown follows every model response.
	events.ContextUsage.Name,
	events.RunCompleted.Name,
}

// eventLog records every SDK event of a bus, in order.
type eventLog struct {
	mu     sync.Mutex
	events []kernel.Event
}

// collectEvents subscribes to every event kind of the bus.
func collectEvents(bus events.Bus) *eventLog {
	l := &eventLog{}
	for _, name := range events.KindNames() {
		bus.On(name, func(e kernel.Event) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.events = append(l.events, e)
		})
	}
	return l
}

// all returns a copy of the recorded events.
func (l *eventLog) all() []kernel.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]kernel.Event(nil), l.events...)
}

// types returns the event type names, in order.
func (l *eventLog) types() []string {
	var out []string
	for _, e := range l.all() {
		out = append(out, e.Type)
	}
	return out
}

// count is how many events of the type were emitted.
func (l *eventLog) count(eventType string) int {
	n := 0
	for _, ty := range l.types() {
		if ty == eventType {
			n++
		}
	}
	return n
}

// runCompleted returns the typed payloads of the RunCompleted events.
func runCompleted(t *testing.T, l *eventLog) []events.RunCompletedPayload {
	t.Helper()
	var out []events.RunCompletedPayload
	for _, e := range l.all() {
		if e.Type != events.RunCompleted.Name {
			continue
		}
		p, err := events.PayloadAs[events.RunCompletedPayload](e)
		check.NoErr(t, err, "decode the RunCompleted payload")
		out = append(out, p)
	}
	return out
}

// toolResults returns every tool result of the messages, in order.
func toolResults(msgs []kernel.Message) []kernel.ToolResult {
	var out []kernel.ToolResult
	for _, m := range msgs {
		out = append(out, m.ToolResults...)
	}
	return out
}

// isError reports whether a tool result carries an explicit true isError.
func isError(r kernel.ToolResult) bool { return r.IsError != nil && *r.IsError }

// messageShape is the structural shape of a message that the run/stream
// equivalence case compares.
type messageShape struct {
	Role            kernel.Role
	HasContent      bool
	HasToolCalls    bool
	HasToolResults  bool
	ToolCallCount   int
	ToolResultCount int
}

// normalize reduces messages to their shape.
func normalize(msgs []kernel.Message) []messageShape {
	out := make([]messageShape, len(msgs))
	for i, m := range msgs {
		out[i] = messageShape{
			Role:            m.Role,
			HasContent:      strings.TrimSpace(m.Content) != "",
			HasToolCalls:    len(m.ToolCalls) > 0,
			HasToolResults:  len(m.ToolResults) > 0,
			ToolCallCount:   len(m.ToolCalls),
			ToolResultCount: len(m.ToolResults),
		}
	}
	return out
}

// ptr returns a pointer to a copy of v.
func ptr[T any](v T) *T { return &v }

// -- Turn and tool builders ---------------------------------------------------

// endTurn is a scripted answer that ends the turn.
func endTurn(content string) AgentLoopTurn {
	return AgentLoopTurn{Content: content, StopReason: kernel.StopEndTurn}
}

// toolTurn is a scripted answer that asks for tool calls.
func toolTurn(calls ...kernel.ToolCall) AgentLoopTurn {
	return AgentLoopTurn{ToolCalls: calls, StopReason: kernel.StopToolUse}
}

// callOf builds a tool call with the given JSON arguments.
func callOf(id, name, args string) kernel.ToolCall {
	return kernel.ToolCall{ID: id, Name: name, Arguments: kernel.Arguments(args)}
}

// suiteTool is a toolhub.Handler whose behaviour the suite supplies. It counts its
// executions. Handlers are defined here, not in a test double package, so every
// implementation of the suite runs the same tools.
type suiteTool struct {
	def   toolhub.Definition
	run   func(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error)
	calls atomic.Int32
}

func (s *suiteTool) Name() string                   { return "tool:" + s.def.Name }
func (*suiteTool) Version() string                  { return "1.0.0" }
func (s *suiteTool) Definition() toolhub.Definition { return s.def.Clone() }

func (s *suiteTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	s.calls.Add(1)
	return s.run(ctx, args, tc)
}

// executions is how often the tool ran.
func (s *suiteTool) executions() int { return int(s.calls.Load()) }

// newTool builds a tool whose schema accepts any object.
func newTool(name, description string, run func(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error)) *suiteTool {
	return &suiteTool{
		def: toolhub.Definition{Name: name, Description: description, Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly},
		run: run,
	}
}

// mockTool always returns the same output.
func mockTool(name string, out toolhub.Output) *suiteTool {
	return newTool(name, "Mock tool: "+name, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		return out, nil
	})
}

// failingTool returns an error result for its first failCount executions and then
// succeeds.
func failingTool(name string, failCount int) *suiteTool {
	var calls atomic.Int32
	return newTool(name, "Failing tool: "+name, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		n := int(calls.Add(1))
		if n <= failCount {
			return toolhub.Output{Content: "Temporary error on attempt " + strconv.Itoa(n), IsError: ptr(true)}, nil
		}
		return toolhub.Output{Content: "Success after retries"}, nil
	})
}

// throwingTool fails with an error on every execution.
func throwingTool(name string) *suiteTool {
	return newTool(name, "Throwing tool: "+name, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		return toolhub.Output{}, errors.New(name + " crashed")
	})
}

// skillViewTool is a stand-in for skill_view: it returns the named skill's
// instructions.
func skillViewTool(instructions map[string]string) *suiteTool {
	t := newTool("skill_view", "Load a skill", func(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
		var in struct {
			Name string `json:"name"`
		}
		if err := args.Decode(&in); err == nil {
			if text := instructions[in.Name]; text != "" {
				return toolhub.Output{Content: text}, nil
			}
		}
		return toolhub.Output{Content: "no such skill", IsError: ptr(true), Retryable: ptr(false)}, nil
	})
	t.def.Parameters.Required = []string{"name"}
	t.def.Risk = toolhub.RiskReadOnly
	return t
}

// approvalHandler is the scripted user of the approval subtest: it answers an
// approval request with the decision sent on decide, or fails when the run is
// stopped. It has no clarify channel.
type approvalHandler struct {
	decide chan toolhub.ApprovalDecision

	mu   sync.Mutex
	reqs []toolhub.ApprovalRequest
}

// AskUser reports that there is no clarify channel.
func (*approvalHandler) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, errors.New("conformance: the scripted user answers no questions")
}

// RequestApproval records the request and waits for the scripted decision.
func (h *approvalHandler) RequestApproval(ctx context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	h.mu.Lock()
	h.reqs = append(h.reqs, req)
	h.mu.Unlock()
	select {
	case d := <-h.decide:
		return d, nil
	case <-ctx.Done():
		return toolhub.ApprovalDecision{}, ctx.Err()
	}
}

// requests returns the approval requests received so far.
func (h *approvalHandler) requests() []toolhub.ApprovalRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reqs)
}

// spanLedger is a trace sink that counts how often each span started and ended.
type spanLedger struct {
	mu      sync.Mutex
	started map[string]string // span id -> name
	ended   map[string]int    // span id -> end notifications
}

// watchSpans adds a ledger to the harness's trace service.
func watchSpans(r runner) *spanLedger {
	l := &spanLedger{started: map[string]string{}, ended: map[string]int{}}
	r.Trace.AddSink(l)
	return l
}

func (l *spanLedger) OnSpanStart(sp trace.Span) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started[sp.SpanID] = sp.Name
}

func (l *spanLedger) OnSpanEnd(sp trace.Span) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended[sp.SpanID]++
}

func (*spanLedger) Flush(context.Context) error { return nil }

// check requires that at least one span was opened and that every one of them
// ended exactly once.
func (l *spanLedger) check(t *testing.T, path string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	check.True(t, len(l.started) > 0, "%s: no span was opened", path)
	for id, name := range l.started {
		check.Equal(t, l.ended[id], 1, path+": end notifications of the "+name+" span")
	}
	for id := range l.ended {
		if _, ok := l.started[id]; !ok {
			t.Errorf("%s: a span ended that never started (%s)", path, id)
		}
	}
}
