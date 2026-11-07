package mcpserver

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// A-213: Config.Permissions rules - which MCP tools run without asking, always ask, or
// are not offered (mcp-permissions.ts of core/ts). A rule names a server or tools of one
// server:
//
//	mcp__github            every tool of the github server
//	mcp__github__*         the same
//	mcp__github__get_*     its tools whose name starts with get_ ("*" = any characters)
//	mcp__github__get_issue one tool
//
// The server part never has a "*" (a rule always names a server you configured).
// ---------------------------------------------------------------------------

var ruleRe = regexp.MustCompile(`^mcp__([a-z][a-z0-9_-]*?)(?:__([A-Za-z0-9_*-]+))?$`)

// CheckRule says why rule is invalid, or "" when it is fine.
func CheckRule(rule string) string {
	if strings.TrimSpace(rule) == "" {
		return "a rule must be a non-empty string"
	}
	if !ruleRe.MatchString(strings.TrimSpace(rule)) {
		return `"` + rule + `" is not an MCP rule (mcp__<server>, mcp__<server>__*, mcp__<server>__<tool>)`
	}
	return ""
}

// MatchesRule reports whether rule covers the tool toolName (mcp__<server>__<tool>) of the
// server serverID.
func MatchesRule(rule, toolName, serverID string) bool {
	r := strings.TrimSpace(rule)
	prefix := "mcp__" + serverID
	if r == prefix || r == prefix+"__*" {
		return strings.HasPrefix(toolName, prefix+"__")
	}
	if !strings.HasPrefix(r, prefix+"__") || !strings.HasPrefix(toolName, prefix+"__") {
		return false
	}
	pattern := r[len(prefix)+2:]
	tool := toolName[len(prefix)+2:]
	if !strings.Contains(pattern, "*") {
		return pattern == tool
	}
	parts := strings.Split(pattern, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$")
	return err == nil && re.MatchString(tool)
}
