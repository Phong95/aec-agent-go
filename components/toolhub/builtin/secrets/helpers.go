package secrets

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
)

// midDot is U+00B7, written as an escape so this source stays ASCII.
const midDot = "·"

// describeSecret is one secret as the model sees it - metadata only.
func describeSecret(s secret.AgentInfo) string {
	whose := "global"
	if s.Scope == secret.ScopeAgent {
		whose = "yours"
	}
	parts := []string{s.Name + " (" + whose + ")"}
	if s.Description != "" {
		parts = append(parts, s.Description)
	}
	parts = append(parts, "tools: "+orNone(s.AllowedTools), "hosts: "+orNone(s.AllowedHosts))
	if s.LastUsedAt != "" {
		parts = append(parts, "last used "+s.LastUsedAt)
	} else {
		parts = append(parts, "never used")
	}
	return strings.Join(parts, " "+midDot+" ")
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}

// secretToolError is a failed secret operation as a tool error. A *secret.Error is never
// retried (the same call fails again); another error may be.
func secretToolError(tool string, err error) toolhub.Output {
	var se *secret.Error
	if errors.As(err, &se) {
		return toolkit.Fail(tool + ": " + se.Message)
	}
	return toolhub.Error(tool + ": " + err.Error())
}

// stringList is a string list argument, trimmed, blanks dropped; nil when absent or null.
// A value that is not a list of strings is an *secret.Error ("invalid_argument").
func stringList(a toolkit.Args, key string) ([]string, error) {
	raw, ok := a.Raw(key)
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, secret.NewError(`"`+key+`" must be a list of strings`, secret.CodeInvalidArgument)
	}
	out := []string{}
	for _, it := range items {
		s, isString := it.(string)
		if !isString {
			return nil, secret.NewError(`"`+key+`" must be a list of strings`, secret.CodeInvalidArgument)
		}
		if s = jsstr.Trim(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}
