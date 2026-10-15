package skill

import (
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// jsonStringify is JSON.stringify of a parsed YAML value, which is how a list item
// that is not a string is rendered (only a nested [list] can be one).
func jsonStringify(v any) string {
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// joinItems is items.map(x => typeof x === "string" ? x : JSON.stringify(x)).join(sep).
func joinItems(items []any, sep string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		if s, ok := item.(string); ok {
			parts[i] = s
		} else {
			parts[i] = jsonStringify(item)
		}
	}
	return strings.Join(parts, sep)
}

// asString reads a scalar field: a string as it is, a list joined with spaces
// (a non-string item through JSON.stringify), and nothing for a missing value or
// a map.
func asString(value any, found bool) (string, bool) {
	if !found {
		return "", false
	}
	switch x := value.(type) {
	case string:
		return x, true
	case []any:
		return joinItems(x, " "), true
	}
	return "", false
}

// asList reads a list field: the non-blank strings of a list (trimmed), or a
// string split on whitespace; anything else is an empty list.
func asList(value any, found bool) []string {
	if !found {
		return nil
	}
	switch x := value.(type) {
	case []any:
		var out []string
		for _, v := range x {
			if s, ok := v.(string); ok && jsstr.Trim(s) != "" {
				out = append(out, jsstr.Trim(s))
			}
		}
		return out
	case string:
		return jsstr.Fields(x)
	}
	return nil
}

// flattenMetadata flattens a metadata map to string key/values. Nested maps
// become dotted keys ("hermes.requires_tools"), a list becomes its items joined
// with ", " (a non-string item through JSON.stringify). Anything that is not a map
// gives an empty result. Like every JS object, the result lists integer-like keys
// first in ascending order.
func flattenMetadata(value any, found bool) ordered.Map[string] {
	var out ordered.Map[string]
	obj, ok := value.(ordered.Object)
	if !found || !ok {
		return out
	}
	flattenInto(obj, "", &out)
	return jsOrderedMap(out)
}

func flattenInto(obj ordered.Object, prefix string, out *ordered.Map[string]) {
	for key, v := range obj.All() {
		full := key
		if prefix != "" {
			full = prefix + "." + key
		}
		switch x := v.(type) {
		case string:
			out.Set(full, x)
		case []any:
			out.Set(full, joinItems(x, ", "))
		case ordered.Object:
			flattenInto(x, full, out)
		}
	}
}

// jsOrderedMap reorders m the way a JS object lists its keys.
func jsOrderedMap(m ordered.Map[string]) ordered.Map[string] {
	keys := m.Keys()
	var out ordered.Map[string]
	for _, k := range jsKeyOrder(keys) {
		v, _ := m.Get(k)
		out.Set(k, v)
	}
	return out
}

// requiredTools reads `requires-tools` from the metadata (also Hermes'
// `hermes.requires_tools`): space or comma separated tool names, each once, in
// the order they first appear.
func requiredTools(metadata ordered.Map[string]) []string {
	var parts []string
	for _, key := range []string{"requires-tools", "hermes.requires_tools"} {
		if v, ok := metadata.Get(key); ok && v != "" {
			parts = append(parts, v)
		}
	}
	raw := strings.Join(parts, " ")
	out := []string{}
	seen := map[string]bool{}
	for _, t := range jsstr.FieldsFunc(raw, func(r rune) bool { return jsstr.IsSpace(r) || r == ',' }) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
