package agent_test

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// drain collects every chunk of a sequence and its error, stopping at the first
// error.
func drain(t *testing.T, seq func(func(kernel.StreamChunk, error) bool)) ([]kernel.StreamChunk, error) {
	t.Helper()
	var chunks []kernel.StreamChunk
	var firstErr error
	seq(func(c kernel.StreamChunk, err error) bool {
		if err != nil {
			firstErr = err
			return false
		}
		chunks = append(chunks, c)
		return true
	})
	return chunks, firstErr
}

func TestAgent_ChatStream(t *testing.T) {
	a, e := newTestAgent(t, testProfile("a"))
	session, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	chunks, err := drain(t, a.ChatStream(t.Context(), "Hello"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var text strings.Builder
	for _, c := range chunks {
		if c.Type == kernel.ChunkText && c.Content != nil {
			text.WriteString(*c.Content)
		}
	}
	if got := strings.TrimSpace(text.String()); got != "reply from primary" {
		t.Fatalf("streamed text = %q, want the provider's reply", got)
	}
	if last := chunks[len(chunks)-1]; last.Type != kernel.ChunkDone || last.Aborted {
		t.Fatalf("last chunk = %+v, want the loop's done chunk", last)
	}
	stored, err := e.hub.Messages.Get(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Role != kernel.RoleUser || stored[1].Role != kernel.RoleAssistant {
		t.Fatalf("stored messages = %+v, want the user message and the reply", stored)
	}
}

func TestAgent_ChatStreamWithoutSession(t *testing.T) {
	a, e := newTestAgent(t, testProfile("a"))
	var pairs int
	var gotErr error
	for c, err := range a.ChatStream(t.Context(), "Hello") {
		pairs++
		gotErr = err
		if !reflect.DeepEqual(c, kernel.StreamChunk{}) {
			t.Fatalf("chunk = %+v, want the zero chunk", c)
		}
	}
	if pairs != 1 {
		t.Fatalf("the sequence yielded %d pairs, want exactly 1", pairs)
	}
	if !errors.Is(gotErr, agent.ErrNoActiveSession) || !errors.Is(gotErr, agentloop.ErrNoActiveSession) {
		t.Fatalf("error = %v, want agent.ErrNoActiveSession matching the loop's sentinel", gotErr)
	}
	if n := len(e.mocks["primary"].Calls()); n != 0 {
		t.Fatalf("the provider was called %d times without a session", n)
	}
}

func TestAgent_LoadSessionRestoresHistory(t *testing.T) {
	t.Run("messages", func(t *testing.T) {
		e := newEnv(t)
		first := e.spawn(t, testProfile("a1"))
		session, err := first.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		chat(t, first, "Hello")

		// A second agent over the same persistence hub loads the session and continues it.
		second := e.spawn(t, testProfile("a2"))
		loaded, ok, err := second.LoadSession(t.Context(), session.ID)
		if err != nil || !ok || loaded.ID != session.ID {
			t.Fatalf("LoadSession = %+v, %v, %v, want the stored session", loaded, ok, err)
		}
		chat(t, second, "And again")

		calls := e.mocks["primary"].Calls()
		sent := calls[len(calls)-1].Messages
		var roles []kernel.Role
		var contents []string
		for _, m := range sent {
			roles = append(roles, m.Role)
			contents = append(contents, m.Content)
		}
		wantRoles := []kernel.Role{kernel.RoleSystem, kernel.RoleUser, kernel.RoleAssistant, kernel.RoleUser}
		wantContents := []string{"You are a helpful assistant.", "Hello", "reply from primary", "And again"}
		if !reflect.DeepEqual(roles, wantRoles) || !reflect.DeepEqual(contents, wantContents) {
			t.Fatalf("input after LoadSession = %v %v, want the restored history then the new message", roles, contents)
		}
	})

	t.Run("compacted working set", func(t *testing.T) {
		e := newEnv(t)
		first := e.spawn(t, testProfile("a1"))
		session, err := first.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		chat(t, first, "Hello")
		// A stored compaction replaces the history as the model's working set.
		summary := []kernel.Message{{Role: kernel.RoleUser, Content: "summary of the past"}}
		if err := e.hub.CompactedMessages.Set(t.Context(), session.ID, summary); err != nil {
			t.Fatal(err)
		}

		second := e.spawn(t, testProfile("a2"))
		if _, ok, err := second.LoadSession(t.Context(), session.ID); err != nil || !ok {
			t.Fatalf("LoadSession = %v, %v", ok, err)
		}
		chat(t, second, "next")

		calls := e.mocks["primary"].Calls()
		sent := calls[len(calls)-1].Messages
		var contents []string
		for _, m := range sent {
			contents = append(contents, m.Content)
		}
		want := []string{"You are a helpful assistant.", "summary of the past", "next"}
		if !reflect.DeepEqual(contents, want) {
			t.Fatalf("input = %v, want the working set %v", contents, want)
		}
	})

	t.Run("an empty compacted list means none", func(t *testing.T) {
		e := newEnv(t)
		first := e.spawn(t, testProfile("a1"))
		session, err := first.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		chat(t, first, "Hello")
		if err := e.hub.CompactedMessages.Set(t.Context(), session.ID, []kernel.Message{}); err != nil {
			t.Fatal(err)
		}

		second := e.spawn(t, testProfile("a2"))
		if _, ok, err := second.LoadSession(t.Context(), session.ID); err != nil || !ok {
			t.Fatalf("LoadSession = %v, %v", ok, err)
		}
		chat(t, second, "next")

		calls := e.mocks["primary"].Calls()
		if n := len(calls[len(calls)-1].Messages); n != 4 {
			t.Fatalf("input has %d messages, want the full history of 4 (system, user, reply, user)", n)
		}
	})
}

// LoadSession of an unknown id leaves the active session alone (a miss does not
// deactivate the session): the bool is false, and the next Chat
// still runs on the session that was active, with its history.
func TestAgent_LoadUnknownSessionKeepsActive(t *testing.T) {
	a, e := newTestAgent(t, testProfile("a"))
	if _, err := a.StartSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	chat(t, a, "Hello")

	_, ok, err := a.LoadSession(t.Context(), "no-such-session")
	if err != nil || ok {
		t.Fatalf("LoadSession(unknown) = %v, %v, want false and no error", ok, err)
	}
	if _, err := a.Chat(t.Context(), "Hello again"); err != nil {
		t.Fatalf("Chat after an unknown LoadSession = %v, want it to run on the active session", err)
	}
	calls := e.mocks["primary"].Calls()
	if n := len(calls[len(calls)-1].Messages); n != 4 {
		t.Fatalf("input has %d messages, want the kept history of 4 (system, user, reply, user)", n)
	}
}

// ListSessions has no agent filter: it returns every session of the store.
func TestAgent_ListSessionsNoAgentFilter(t *testing.T) {
	e := newEnv(t)
	a1 := e.spawn(t, testProfile("a1"))
	a2 := e.spawn(t, testProfile("a2"))
	tagged := ordered.NewObject()
	tagged.Set("project", "x")
	s1, err := a1.StartSession(t.Context(), agent.WithMetadata(tagged))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := a2.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	all, err := a1.ListSessions(t.Context(), kernel.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != s1.ID || all[1].ID != s2.ID {
		t.Fatalf("ListSessions = %+v, want both sessions, whichever agent made them", all)
	}
	filtered, err := a2.ListSessions(t.Context(), kernel.SessionFilter{Metadata: tagged})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != s1.ID {
		t.Fatalf("filtered ListSessions = %+v, want only the tagged session", filtered)
	}
}

func TestAgent_ProfileNotAliased(t *testing.T) {
	maxTokens := 100
	p := testProfile("a")
	p.Tools = []string{"one", "two"}
	p.Skills = []string{"s1"}
	p.InferenceConfig = &profile.InferenceConfig{MaxTokens: &maxTokens, StopSequences: []string{"stop"}}
	p.InitialContext = &profile.InitialContext{UserMd: "user"}
	a, _ := newTestAgent(t, p)
	want := a.Profile()

	// The caller's struct and the slices behind it change after the spawn.
	p.Tools[0] = "changed"
	p.Skills[0] = "changed"
	p.InferenceConfig.StopSequences[0] = "changed"
	maxTokens = 7
	p.InitialContext.UserMd = "changed"
	if got := a.Profile(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the agent aliases the caller's profile:\n got %+v\nwant %+v", got, want)
	}

	// A copy returned by Profile is the caller's to change.
	got := a.Profile()
	got.Tools[0] = "mutated"
	*got.InferenceConfig.MaxTokens = 1
	got.InferenceConfig.StopSequences[0] = "mutated"
	if again := a.Profile(); !reflect.DeepEqual(again, want) {
		t.Fatalf("Profile() hands out the agent's own slices:\n got %+v\nwant %+v", again, want)
	}

	// The slices an Update* call received are not kept either.
	names := []string{"a", "b"}
	a.UpdateTools(names)
	names[0] = "changed"
	if got := a.Profile().Tools; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("UpdateTools keeps the caller's slice: %v", got)
	}
	cfg := &profile.InferenceConfig{StopSequences: []string{"x"}}
	a.UpdateInferenceConfig(cfg)
	cfg.StopSequences[0] = "changed"
	if got := a.Profile().InferenceConfig.StopSequences; !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("UpdateInferenceConfig keeps the caller's config: %v", got)
	}
}

func TestAgent_UpdatesApplyToNextRun(t *testing.T) {
	t.Run("model, provider and inference config", func(t *testing.T) {
		e := newEnv(t, "primary", "other")
		a := e.spawn(t, testProfile("a"))
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		last := func(name string) llmhub.ModelOptions {
			calls := e.mocks[name].Calls()
			if len(calls) == 0 {
				t.Fatalf("provider %s was not called", name)
			}
			return calls[len(calls)-1].Options
		}

		chat(t, a, "one")
		if o := last("primary"); o.Model != "" || o.MaxTokens != nil {
			t.Fatalf("first run options = %+v, want no defaults", o)
		}

		a.UpdateModel("model-2")
		maxTokens := 64
		a.UpdateInferenceConfig(&profile.InferenceConfig{MaxTokens: &maxTokens})
		chat(t, a, "two")
		o := last("primary")
		if o.Model != "model-2" || o.MaxTokens == nil || *o.MaxTokens != 64 {
			t.Fatalf("second run options = %+v, want model-2 and maxTokens 64", o)
		}
		if got := a.Profile(); got.LLMModel != "model-2" || got.InferenceConfig == nil || *got.InferenceConfig.MaxTokens != 64 {
			t.Fatalf("Profile() = %+v, want the updates", got)
		}

		a.UpdateProvider("other")
		chat(t, a, "three")
		if n := len(e.mocks["other"].Calls()); n != 1 {
			t.Fatalf("other provider calls = %d, want 1 after UpdateProvider", n)
		}
		if got := a.Profile().Provider; got != "other" {
			t.Fatalf("Profile().Provider = %q, want other", got)
		}
	})

	t.Run("tools", func(t *testing.T) {
		a, e := newTestAgent(t, testProfile("a"))
		mustRegister(t, e.tools, &aecmock.ScheduleLookupTool{}, &aecmock.BudgetQueryTool{})
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		toolCount := func() int {
			calls := e.mocks["primary"].Calls()
			return len(calls[len(calls)-1].Options.Tools)
		}

		chat(t, a, "one")
		if n := toolCount(); n != 0 {
			t.Fatalf("tools offered = %d, want 0 for an empty allowlist", n)
		}
		a.UpdateTools([]string{"schedule_lookup", "budget_query"})
		chat(t, a, "two")
		if n := toolCount(); n != 2 {
			t.Fatalf("tools offered = %d, want 2 after UpdateTools", n)
		}
		if got := a.Profile().Tools; !reflect.DeepEqual(got, []string{"schedule_lookup", "budget_query"}) {
			t.Fatalf("Profile().Tools = %v", got)
		}
	})

	t.Run("loop config", func(t *testing.T) {
		// A model that asks for a tool in every answer makes the loop run to its cap (a
		// max_tokens answer ends the turn). The unknown tool is answered at once.
		e := newEnv(t)
		endless := aecmock.NewMockProvider("endless", nil, aecmock.WithResponse(func([]kernel.Message) kernel.ModelResponse {
			return kernel.ModelResponse{
				Message: kernel.Message{
					Role: kernel.RoleAssistant, Content: "partial",
					ToolCalls: []kernel.ToolCall{{ID: "call", Name: "no_such_tool", Arguments: kernel.Arguments(`{}`)}},
				},
				Usage:      kernel.TokenUsage{InputTokens: 1, OutputTokens: 1},
				StopReason: kernel.StopToolUse,
			}
		}))
		if err := e.llm.Register(endless); err != nil {
			t.Fatal(err)
		}
		p := testProfile("a")
		p.Provider = "endless"
		a := e.spawn(t, p)
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}

		two := 2
		a.UpdateLoopConfig(&profile.LoopConfig{MaxIterations: &two})
		res := chat(t, a, "go")
		if res.Iterations != 2 {
			t.Fatalf("Iterations = %d, want the updated cap of 2", res.Iterations)
		}
		if got := a.Profile().LoopConfig; got == nil || got.MaxIterations == nil || *got.MaxIterations != 2 {
			t.Fatalf("Profile().LoopConfig = %+v, want maxIterations 2", got)
		}
	})
}

// Concurrent Chat, Update* and Profile calls on one agent are race-free. Serializing
// chats is the caller's job, so this test only asks for no data race and no error.
func TestAgent_ConcurrentCallsRace(t *testing.T) {
	e := newEnv(t, "primary", "other")
	a := e.spawn(t, testProfile("a"))
	mustRegister(t, e.tools, &aecmock.ScheduleLookupTool{})
	session, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	const workers, iterations = 8, 50
	work := []func(i int) error{
		func(int) error { // chat
			_, err := a.Chat(t.Context(), "hello")
			return err
		},
		func(int) error { // chat stream
			_, err := drainErr(a.ChatStream(t.Context(), "hello"))
			return err
		},
		func(i int) error { // UpdateTools
			if i%2 == 0 {
				a.UpdateTools([]string{"schedule_lookup"})
			} else {
				a.UpdateTools(nil)
			}
			return nil
		},
		func(i int) error { // UpdateProvider and UpdateModel
			a.UpdateProvider([]string{"primary", "other"}[i%2])
			a.UpdateModel("m")
			return nil
		},
		func(i int) error { // UpdateInferenceConfig and UpdateLoopConfig
			n := i + 1
			a.UpdateInferenceConfig(&profile.InferenceConfig{MaxTokens: &n})
			a.UpdateLoopConfig(&profile.LoopConfig{MaxIterations: &n})
			return nil
		},
		func(int) error { // Profile, mutating the copy
			p := a.Profile()
			if len(p.Tools) > 0 {
				p.Tools[0] = "mutated"
			}
			return nil
		},
		func(int) error { // Complete
			_, err := a.Complete(t.Context(), completePrompt(), agent.WithCompleteSession(session))
			return err
		},
		func(int) error { // sessions and providers
			if _, err := a.ListSessions(t.Context(), kernel.SessionFilter{}); err != nil {
				return err
			}
			_ = a.ListProviders()
			return nil
		},
	}
	if len(work) != workers {
		t.Fatalf("the race test has %d workers, want %d", len(work), workers)
	}
	errs := make(chan error, workers*iterations)
	var wg sync.WaitGroup
	for _, w := range work {
		wg.Go(func() {
			for i := range iterations {
				if err := w(i); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call failed: %v", err)
	}
}

// drainErr consumes a stream and returns its first error.
func drainErr(seq func(func(kernel.StreamChunk, error) bool)) (int, error) {
	n := 0
	var firstErr error
	seq(func(_ kernel.StreamChunk, err error) bool {
		if err != nil {
			firstErr = err
			return false
		}
		n++
		return true
	})
	return n, firstErr
}

// Chat runs on the active session: with two sessions started, the message and the
// reply go to the last one and every event carries its id.
func TestAgent_ChatRunsOnActiveSession(t *testing.T) {
	a, e := newTestAgent(t, testProfile("a"))
	sessionA, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var eventSessions []string
	for _, name := range events.KindNames() {
		e.bus.On(name, func(ev kernel.Event) {
			mu.Lock()
			defer mu.Unlock()
			eventSessions = append(eventSessions, ev.SessionID)
		})
	}

	chat(t, a, "Hello")

	inA, err := e.hub.Messages.Get(t.Context(), sessionA.ID)
	if err != nil {
		t.Fatal(err)
	}
	inB, err := e.hub.Messages.Get(t.Context(), sessionB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inA) != 0 || len(inB) != 2 {
		t.Fatalf("messages: session A has %d, session B has %d, want 0 and 2", len(inA), len(inB))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(eventSessions) == 0 {
		t.Fatal("the run emitted no event")
	}
	for _, id := range eventSessions {
		if id != sessionB.ID {
			t.Fatalf("an event carries session %q, want only %q", id, sessionB.ID)
		}
	}
}

// Regression test of the phase 10 code review warning WR-A04 (its unexported-level
// case is TestConversationManager_RunBoundToSession in conversation_manager_test.go).
// (A-97: now TestConversationManager_ConversationBoundToSession.)

// --- WR-A04: a turn stays in the session it started on -----------------------------------

// A StartSession while a turn is in its model phase must not move the rest of the turn
// into the new session: the turn fails at its next write, the new session stays empty.
// -> Superseded (A-97): the turn no longer fails. It works in the Conversation of its
// own session, so it finishes there whole (user message and reply), and the new session,
// now the default one, stays empty.
func TestAgent_SessionSwitchMidTurn(t *testing.T) {
	e := newEnv(t)
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	slow := aecmock.NewMockProvider("slow", nil, aecmock.WithResponse(func([]kernel.Message) kernel.ModelResponse {
		once.Do(func() { close(entered) })
		<-gate
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, Content: "late reply"},
			Usage:      kernel.TokenUsage{InputTokens: 7, OutputTokens: 3},
			StopReason: kernel.StopEndTurn,
		}
	}))
	if err := e.llm.Register(slow); err != nil {
		t.Fatal(err)
	}
	p := testProfile("a1")
	p.Provider = "slow"
	a := e.spawn(t, p)

	s1, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.Chat(t.Context(), "hello")
		done <- err
	}()
	<-entered
	s2, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("Chat = %v, want the turn to finish in its own session (A-97)", err)
	}
	msgs2, err := e.hub.Messages.Get(t.Context(), s2.ID)
	if err != nil || len(msgs2) != 0 {
		t.Fatalf("messages of the new session = %v, %v, want none", msgs2, err)
	}
	if stored, err := e.hub.Sessions.Get(t.Context(), s2.ID); err != nil || stored == nil || stored.CurrentTokenUsage != nil {
		t.Fatalf("new session = %+v, %v, want no token usage from the other session's turn", stored, err)
	}
	msgs1, err := e.hub.Messages.Get(t.Context(), s1.ID)
	if err != nil || len(msgs1) != 2 || msgs1[0].Role != kernel.RoleUser || msgs1[1].Content != "late reply" {
		t.Fatalf("messages of the first session = %v, %v, want the whole turn (A-97)", msgs1, err)
	}
	if stored, err := e.hub.Sessions.Get(t.Context(), s1.ID); err != nil || stored == nil || stored.CurrentTokenUsage == nil {
		t.Fatalf("first session = %+v, %v, want the turn's token usage", stored, err)
	}
}
