package agent_test

import (
	"context"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-104: what fills a session's context window, by category (the same cases as the TS
// SDK's tests/unit/context-usage.test.ts).

func categoryTokens(u agentloop.ContextUsage) map[agentloop.ContextCategoryKey]int {
	out := map[agentloop.ContextCategoryKey]int{}
	for _, c := range u.Categories {
		out[c.Key] = c.Tokens
	}
	return out
}

func TestContextManager_PrepareSections(t *testing.T) {
	p := testProfile("a-1")
	p.SystemPrompt = "You are a test agent."
	p.InitialContext = &profile.InitialContext{UserMd: "USER", SoulMd: "SOUL"}
	cm := agent.NewContextManager(p,
		agent.WithSkillIndex(func(context.Context) (string, bool) { return "SKILLS", true }),
		agent.WithSecretIndex(func(context.Context) (string, bool) { return "SECRETS", true }),
	)
	history := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}
	mc := agentloop.MiddlewareContext{Session: kernel.Session{ID: "s-1"}}

	msgs, sections, err := cm.Prepare(t.Context(), history, mc)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := cm.PrepareMessages(t.Context(), history, mc)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(plain) || msgs[0].Content != plain[0].Content {
		t.Fatalf("Prepare and PrepareMessages differ: %+v vs %+v", msgs, plain)
	}
	if want := "You are a test agent.\n\nUSER\n\nSOUL\n\nSKILLS\n\nSECRETS"; msgs[0].Content != want {
		t.Fatalf("system message = %q, want %q", msgs[0].Content, want)
	}
	want := []agentloop.ContextSection{
		{Key: agentloop.ContextSystemPrompt, Text: "You are a test agent."},
		{Key: agentloop.ContextMemoryFiles, Text: "USER\n\nSOUL"},
		{Key: agentloop.ContextSkills, Text: "SKILLS"},
		{Key: agentloop.ContextSecrets, Text: "SECRETS"},
	}
	if len(sections) != len(want) {
		t.Fatalf("sections = %+v, want %+v", sections, want)
	}
	for i := range want {
		if sections[i] != want[i] {
			t.Fatalf("section %d = %+v, want %+v", i, sections[i], want[i])
		}
	}
}

func TestAgent_ContextUsageOnDemand(t *testing.T) {
	a, _ := newTestAgent(t, testProfile("a-1"))
	s, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	u, ok, err := a.ContextUsage(t.Context(), s.ID)
	if err != nil || !ok {
		t.Fatalf("ContextUsage = %v, %v", ok, err)
	}
	c := categoryTokens(u)
	if u.SessionID != s.ID || u.UsedSource != agentloop.UsedEstimated || c[agentloop.ContextMessages] != 0 || c[agentloop.ContextSystemPrompt] == 0 {
		t.Fatalf("new session usage = %+v", u)
	}
	cfg := testProfile("a-1").Compactor
	if u.ContextWindowTokens != cfg.ContextWindowTokens || u.CompactAtTokens != int(float64(cfg.ContextWindowTokens)*cfg.Threshold) {
		t.Fatalf("window %d compactAt %d", u.ContextWindowTokens, u.CompactAtTokens)
	}
	if _, ok, err := a.ContextUsage(t.Context(), "missing"); ok || err != nil {
		t.Fatalf("missing session = %v, %v", ok, err)
	}
}

func TestAgent_ContextUsageEvent(t *testing.T) {
	for _, mode := range []string{"run", "stream"} {
		t.Run(mode, func(t *testing.T) {
			a, e := newTestAgent(t, testProfile("a-1"))
			s, err := a.StartSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var types []string
			e.bus.OnAny(func(ev kernel.Event) {
				if ev.SessionID == s.ID {
					types = append(types, ev.Type)
				}
			})
			var seen []events.ContextUsagePayload
			events.Subscribe(e.bus, events.ContextUsage, func(_ kernel.Event, p events.ContextUsagePayload) {
				seen = append(seen, p)
			})

			if mode == "run" {
				if _, err := a.Chat(t.Context(), "hi", agent.WithChatSession(s.ID)); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, err := range a.ChatStream(t.Context(), "hi", agent.WithChatSession(s.ID)) {
					if err != nil {
						t.Fatal(err)
					}
				}
			}

			i := indexOf(types, events.ModelResponded.Name)
			if i < 0 || i+1 >= len(types) || types[i+1] != events.ContextUsage.Name {
				t.Fatalf("events = %v, want context-usage right after model-response", types)
			}
			if len(seen) != 1 || seen[0].Iteration != 1 || seen[0].AgentID != "a-1" {
				t.Fatalf("context usage events = %+v", seen)
			}
			sum := 0
			for _, c := range seen[0].Categories {
				sum += c.Tokens
			}
			if sum != seen[0].UsedTokens {
				t.Fatalf("categories sum %d, used %d", sum, seen[0].UsedTokens)
			}
			// The mock reports 100/50 on Chat; its stream reports no usage.
			wantSource := agentloop.UsedReported
			if mode == "stream" {
				wantSource = agentloop.UsedEstimated
			}
			if seen[0].UsedSource != wantSource || (mode == "run" && seen[0].UsedTokens != 150) {
				t.Fatalf("used = %d (%s)", seen[0].UsedTokens, seen[0].UsedSource)
			}

			after, ok, err := a.ContextUsage(t.Context(), s.ID)
			if err != nil || !ok || categoryTokens(after)[agentloop.ContextMessages] == 0 {
				t.Fatalf("on-demand after the turn = %+v, %v, %v", after, ok, err)
			}
		})
	}
}

func TestAgent_ContextUsageFollowsTheSessionWindow(t *testing.T) {
	a, e := newTestAgent(t, testProfile("a-1"))
	s, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.ContextWindowTokens = 5000
	if err := e.hub.Sessions.Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	u, _, err := a.ContextUsage(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testProfile("a-1").Compactor
	if u.ContextWindowTokens != 5000 || u.CompactAtTokens != int(5000*cfg.Threshold) {
		t.Fatalf("window %d compactAt %d", u.ContextWindowTokens, u.CompactAtTokens)
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}
