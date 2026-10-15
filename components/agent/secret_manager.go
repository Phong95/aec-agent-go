package agent

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
)

// secretManagerDeps is what a secretManager needs (the SecretManagerDeps of core/ts).
type secretManagerDeps struct {
	agentID string
	// profile is read on every call, so UpdateSecrets applies at once.
	profile func() profile.AgentProfile
	// hub is the host's SecretHub; nil means the agent has no secrets.
	hub *secrethub.SecretHub
	// canManage says whether the agent has manage_secret (the index then explains how to
	// add one).
	canManage func() bool
	logger    *slog.Logger
}

// secretManager is the agent's secrets (A-101): its own (owner = this agent) and the
// global ones its profile enables (Secrets.Use). It hands values to tools after the
// SecretHub checked the use, keeps the <available_secrets> index (names, never values),
// and writes the agent's own secrets for manage_secret. Agent only wires it. It is safe
// for concurrent use.
type secretManager struct {
	d secretManagerDeps
}

var _ secret.Secrets = (*secretManager)(nil)

func newSecretManager(d secretManagerDeps) *secretManager {
	if d.logger == nil {
		d.logger = slog.Default()
	}
	return &secretManager{d: d}
}

// enabled is the global secrets the profile enables.
func (m *secretManager) enabled() []string {
	if c := m.d.profile().Secrets; c != nil {
		return c.Use
	}
	return nil
}

// List returns the agent's own secrets, then the global ones it enabled (an own secret
// hides a global one of the same name).
func (m *secretManager) List(ctx context.Context) ([]secret.AgentInfo, error) {
	hub := m.d.hub
	if hub == nil {
		return []secret.AgentInfo{}, nil
	}
	own, err := hub.List(ctx, m.d.agentID)
	if err != nil {
		return nil, err
	}
	out := make([]secret.AgentInfo, 0, len(own))
	for _, s := range own {
		out = append(out, secret.AgentInfo{Info: s, Scope: secret.ScopeAgent})
	}
	enabled := m.enabled()
	if len(enabled) == 0 {
		return out, nil
	}
	globals, err := hub.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, s := range globals {
		if slices.Contains(enabled, s.Name) && !slices.ContainsFunc(own, func(o secret.Info) bool { return o.Name == s.Name }) {
			out = append(out, secret.AgentInfo{Info: s, Scope: secret.ScopeGlobal})
		}
	}
	return out, nil
}

// Resolve returns the values of names for one use, each after the hub checked it.
func (m *secretManager) Resolve(ctx context.Context, names []string, use secret.Use, rc secret.ResolveContext) (map[string]string, error) {
	hub, err := m.requireHub()
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, name := range names {
		if _, done := values[name]; done {
			continue
		}
		owner, err := m.ownerOf(ctx, hub, name)
		if err != nil {
			return nil, err
		}
		v, err := hub.Reveal(ctx, owner, name, use, secrethub.RevealContext{AgentID: m.d.agentID, SessionID: rc.SessionID})
		if err != nil {
			return nil, err
		}
		values[name] = v
	}
	return values, nil
}

// Redact returns text with every value revealed in this process replaced.
func (m *secretManager) Redact(text string) string {
	if m.d.hub == nil {
		return text
	}
	return m.d.hub.Redactor().Redact(text)
}

// Create creates one of the agent's own secrets.
func (m *secretManager) Create(ctx context.Context, def secret.Definition, value string) (secret.AgentInfo, error) {
	hub, err := m.requireHub()
	if err != nil {
		return secret.AgentInfo{}, err
	}
	s, err := hub.Create(ctx, secrethub.CreateInput{Definition: def, Value: value, OwnerAgentID: m.d.agentID, CreatedBy: secret.CreatorAgent})
	if err != nil {
		return secret.AgentInfo{}, err
	}
	return secret.AgentInfo{Info: s, Scope: secret.ScopeAgent}, nil
}

// Update changes one of the agent's own secrets' description, hosts or tools.
func (m *secretManager) Update(ctx context.Context, name string, patch secret.Definition) (secret.AgentInfo, error) {
	hub, err := m.requireHub()
	if err != nil {
		return secret.AgentInfo{}, err
	}
	s, err := hub.Update(ctx, m.d.agentID, name, patch)
	if err != nil {
		return secret.AgentInfo{}, err
	}
	return secret.AgentInfo{Info: s, Scope: secret.ScopeAgent}, nil
}

// SetValue gives one of the agent's own secrets a new value.
func (m *secretManager) SetValue(ctx context.Context, name, value string) (secret.AgentInfo, error) {
	hub, err := m.requireHub()
	if err != nil {
		return secret.AgentInfo{}, err
	}
	s, err := hub.SetValue(ctx, m.d.agentID, name, value)
	if err != nil {
		return secret.AgentInfo{}, err
	}
	return secret.AgentInfo{Info: s, Scope: secret.ScopeAgent}, nil
}

// Delete deletes one of the agent's own secrets.
func (m *secretManager) Delete(ctx context.Context, name string) error {
	hub, err := m.requireHub()
	if err != nil {
		return err
	}
	return hub.Delete(ctx, m.d.agentID, name)
}

// buildIndex is <available_secrets> for the system prompt - names, what they are for and
// where they may be used; false when the agent has none and can't add one. canManage is
// false for a subagent type's loop (it never gets manage_secret).
func (m *secretManager) buildIndex(ctx context.Context, canManage bool) (string, bool) {
	if m.d.hub == nil {
		return "", false
	}
	secrets, err := m.List(ctx)
	if err != nil {
		m.d.logger.Warn("secrets: could not list the agent's secrets", "agent", m.d.agentID, "error", err)
		secrets = nil
	}
	if len(secrets) == 0 && !canManage {
		return "", false
	}
	lines := []string{
		"<available_secrets>",
		"Secrets are keys and tokens you can use without seeing them. Never ask the user to paste a key into the chat, and never try to print or reveal a secret's value.",
		"- http_request: write {{secret:NAME}} in the url, a header or the body, e.g. Authorization: Bearer {{secret:NAME}}. Only the secret's allowed hosts get it.",
		"- terminal / run_code: list the names in `secrets`; each becomes an environment variable of the same name ($NAME).",
	}
	if canManage {
		lines = append(lines, "- To add a key the user wants you to use, call manage_secret (action create): the user types the value into a masked field "+emDash+" it never passes through the chat.")
	}
	lines = append(lines, "")
	if len(secrets) == 0 {
		lines = append(lines, "(no secrets yet)")
	}
	for _, s := range secrets {
		tools := strings.Join(s.AllowedTools, ", ")
		if tools == "" {
			tools = "none"
		}
		where := "tools: " + tools
		if slices.Contains(s.AllowedTools, "http_request") {
			hosts := strings.Join(s.AllowedHosts, ", ")
			if hosts == "" {
				hosts = "none yet"
			}
			where += "; hosts: " + hosts
		}
		line := "- " + s.Name
		if s.Description != "" {
			line += " " + emDash + " " + s.Description
		}
		lines = append(lines, line+" ("+where+")")
	}
	lines = append(lines, "</available_secrets>")
	return strings.Join(lines, "\n"), true
}

// ownerOf is who owns the secret this agent means by name: itself, or "" for a global
// secret its profile enables.
func (m *secretManager) ownerOf(ctx context.Context, hub *secrethub.SecretHub, name string) (string, error) {
	if _, ok, err := hub.Get(ctx, m.d.agentID, name); err != nil || ok {
		return m.d.agentID, err
	}
	if slices.Contains(m.enabled(), name) {
		if _, ok, err := hub.Get(ctx, "", name); err != nil || ok {
			return "", err
		}
	}
	return "", secret.NewError("no secret named "+name+" is available to this agent", secret.CodeNotFound)
}

func (m *secretManager) requireHub() (*secrethub.SecretHub, error) {
	if m.d.hub == nil {
		return nil, secret.NewError("this host has no secret store", secret.CodeUnavailable)
	}
	return m.d.hub, nil
}
