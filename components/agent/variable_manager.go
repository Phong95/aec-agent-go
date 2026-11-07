package agent

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/variablehub"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// variableIndexValueMax is how long a value may be in <available_variables> before it is cut
// (the tools still get all of it), in UTF-16 code units.
const variableIndexValueMax = 300

// variableManagerDeps is what a variableManager needs (the VariableManagerDeps of core/ts).
type variableManagerDeps struct {
	agentID string
	// profile is read on every call, so UpdateVariables applies at once.
	profile func() profile.AgentProfile
	// hub is the host's VariableHub; nil means the agent has no variables.
	hub *variablehub.VariableHub
	// canManage says whether the agent has the variable tool (the index then explains how
	// to add one).
	canManage func() bool
	logger    *slog.Logger
}

// variableManager is the agent's variables (A-215): its own (owner = this agent) and the
// global ones its profile enables (Variables.Use). It resolves {{var:NAME}} and
// variables: [...] for the tools, keeps the <available_variables> index (names,
// descriptions and values: they are not secret), and writes the agent's own variables for
// the variable tool. Agent only wires it. It is safe for concurrent use.
type variableManager struct {
	d variableManagerDeps
}

var _ variable.Variables = (*variableManager)(nil)

func newVariableManager(d variableManagerDeps) *variableManager {
	if d.logger == nil {
		d.logger = slog.Default()
	}
	return &variableManager{d: d}
}

// enabled is the global variables the profile enables.
func (m *variableManager) enabled() []string {
	if c := m.d.profile().Variables; c != nil {
		return c.Use
	}
	return nil
}

// List returns the agent's own variables, then the global ones it enabled (an own one
// hides a global one of the same name).
func (m *variableManager) List(ctx context.Context) ([]variable.AgentInfo, error) {
	hub := m.d.hub
	if hub == nil {
		return []variable.AgentInfo{}, nil
	}
	own, err := hub.List(ctx, m.d.agentID)
	if err != nil {
		return nil, err
	}
	out := make([]variable.AgentInfo, 0, len(own))
	for _, v := range own {
		out = append(out, variable.AgentInfo{Info: v, Scope: variable.ScopeAgent})
	}
	enabled := m.enabled()
	if len(enabled) == 0 {
		return out, nil
	}
	globals, err := hub.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, v := range globals {
		if slices.Contains(enabled, v.Name) && !slices.ContainsFunc(own, func(o variable.Info) bool { return o.Name == v.Name }) {
			out = append(out, variable.AgentInfo{Info: v, Scope: variable.ScopeGlobal})
		}
	}
	return out, nil
}

// Resolve returns the values of names; an unknown name is an *variable.Error ("not_found")
// naming every one that is missing.
func (m *variableManager) Resolve(ctx context.Context, names []string) (map[string]string, error) {
	values := map[string]string{}
	if len(names) == 0 {
		return values, nil
	}
	all, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	available := make(map[string]string, len(all))
	for _, v := range all {
		available[v.Name] = v.Value
	}
	var missing []string
	for _, name := range names {
		if _, done := values[name]; done || slices.Contains(missing, name) {
			continue
		}
		v, ok := available[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		values[name] = v
	}
	if len(missing) > 0 {
		return nil, variable.NewError("no variable named "+strings.Join(missing, ", ")+" is available to this agent", variable.CodeNotFound)
	}
	return values, nil
}

// Fill returns text with each {{var:NAME}} the agent can use filled in; other
// placeholders stay as written (job prompts).
func (m *variableManager) Fill(ctx context.Context, text string) (string, error) {
	if len(variable.PlaceholderNames(text)) == 0 {
		return text, nil
	}
	all, err := m.List(ctx)
	if err != nil {
		return text, err
	}
	values := make(map[string]string, len(all))
	for _, v := range all {
		values[v.Name] = v.Value
	}
	return variable.FillPlaceholders(text, values), nil
}

// Create creates one of the agent's own variables.
func (m *variableManager) Create(ctx context.Context, def variable.Definition) (variable.AgentInfo, error) {
	hub, err := m.requireHub()
	if err != nil {
		return variable.AgentInfo{}, err
	}
	v, err := hub.Create(ctx, variablehub.CreateInput{Definition: def, OwnerAgentID: m.d.agentID, CreatedBy: variable.CreatorAgent})
	if err != nil {
		return variable.AgentInfo{}, err
	}
	return variable.AgentInfo{Info: v, Scope: variable.ScopeAgent}, nil
}

// Update changes one of the agent's own variables.
func (m *variableManager) Update(ctx context.Context, name string, patch variable.Patch) (variable.AgentInfo, error) {
	hub, err := m.requireHub()
	if err != nil {
		return variable.AgentInfo{}, err
	}
	n, err := variable.CheckName(name)
	if err != nil {
		return variable.AgentInfo{}, err
	}
	v, err := hub.Update(ctx, m.d.agentID, n, patch)
	if err != nil {
		return variable.AgentInfo{}, err
	}
	return variable.AgentInfo{Info: v, Scope: variable.ScopeAgent}, nil
}

// Delete deletes one of the agent's own variables.
func (m *variableManager) Delete(ctx context.Context, name string) error {
	hub, err := m.requireHub()
	if err != nil {
		return err
	}
	n, err := variable.CheckName(name)
	if err != nil {
		return err
	}
	return hub.Delete(ctx, m.d.agentID, n)
}

// buildIndex is <available_variables> for the system prompt - names, what they are for,
// and values (long ones cut); false when the agent has none and can't add one. canManage
// is false for a subagent type's loop (it never gets the variable tool).
func (m *variableManager) buildIndex(ctx context.Context, canManage bool) (string, bool) {
	if m.d.hub == nil {
		return "", false
	}
	vars, err := m.List(ctx)
	if err != nil {
		m.d.logger.Warn("variables: could not list the agent's variables", "agent", m.d.agentID, "error", err)
		vars = nil
	}
	if len(vars) == 0 && !canManage {
		return "", false
	}
	lines := []string{
		"<available_variables>",
		"Variables are settings that are not secret (URLs, ids, regions). Use them instead of writing the value out, so a change of the variable reaches every use:",
		"- http_request (url, headers, body) and job prompts: write {{var:NAME}}.",
		"- terminal / run_code: list the names in `variables`; each becomes an environment variable of the same name ($NAME).",
		"Never put a secret's value in a variable: secrets stay {{secret:NAME}}.",
	}
	if canManage {
		lines = append(lines, "- To add or change one, call variable (action create / update).")
	}
	lines = append(lines, "")
	if len(vars) == 0 {
		lines = append(lines, "(no variables yet)")
	}
	for _, v := range vars {
		value := v.Value
		if n := kernel.UTF16Len(value); n > variableIndexValueMax {
			value = jsstr.TruncateUTF16(value, variableIndexValueMax) + "… (" + strconv.Itoa(n) + " characters)"
		}
		line := "- " + v.Name + " = " + quoteJSON(value)
		if v.Description != "" {
			line += " " + emDash + " " + v.Description
		}
		if v.Scope == variable.ScopeGlobal {
			line += " (global)"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "</available_variables>")
	return strings.Join(lines, "\n"), true
}

func (m *variableManager) requireHub() (*variablehub.VariableHub, error) {
	if m.d.hub == nil {
		return nil, variable.NewError("this host has no variable store", variable.CodeUnavailable)
	}
	return m.d.hub, nil
}

// quoteJSON is JSON.stringify of a string (no HTML escaping), as core/ts writes it.
func quoteJSON(s string) string {
	b, err := jsonx.Marshal(s)
	if err != nil {
		return strconv.Quote(s)
	}
	return string(b)
}
