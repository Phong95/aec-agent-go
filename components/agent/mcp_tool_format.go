package agent

import (
	"fmt"
	"hash/fnv"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-213: how an MCP tool looks to the model (mcp-tool-format.ts of core/ts) - its agent
// tool name (mcp__<server>__<tool>), its input schema made acceptable to every provider,
// its description, and how a tools/call result becomes a tool result.
// ---------------------------------------------------------------------------

// McpToolPrefix starts every MCP tool name.
const McpToolPrefix = "mcp__"

// McpToolNameMax is the longest agent tool name: OpenAI-style APIs allow 64 characters
// (the Claude API 128).
const McpToolNameMax = 64

// The description limits of an MCP tool: sent upfront, and loaded through tool_search.
const (
	McpDescriptionMax       = 4_096
	McpSearchDescriptionMax = 16_384
)

// McpResultCharsCeiling is the most a tool may raise its result limit to with
// _meta["anthropic/maxResultSizeChars"].
const McpResultCharsCeiling = 500_000

var (
	mcpNameChar     = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	mcpPropertyName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	mcpDefRef       = regexp.MustCompile(`^#/(?:\$defs|definitions)/(.+)$`)
)

// McpToolName is the agent tool name of a server's tool: mcp__<server>__<tool>, with
// characters outside A-Za-z0-9_- replaced by "_". A name over 64 characters keeps its
// start and ends in a short hash of the full tool name, so it stays unique.
func McpToolName(serverID, toolName string) string {
	server := mcpNameChar.ReplaceAllString(serverID, "_")
	tool := mcpNameChar.ReplaceAllString(toolName, "_")
	full := McpToolPrefix + server + "__" + tool
	if len(full) <= McpToolNameMax {
		return full
	}
	hash := fnv1a(toolName)[:6]
	room := McpToolNameMax - len(McpToolPrefix) - len(server) - 2 - len(hash) - 1
	// A long server id leaves no room for the tool's name: the hash alone tells the tools apart.
	if room < 4 {
		return McpToolPrefix + server[:McpToolNameMax-len(McpToolPrefix)-2-len(hash)] + "__" + hash
	}
	return McpToolPrefix + server + "__" + tool[:room] + "_" + hash
}

// McpServerOf is the server id part of an MCP tool name; false for any other tool.
func McpServerOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, McpToolPrefix)
	if !ok {
		return "", false
	}
	cut := strings.Index(rest, "__")
	if cut <= 0 {
		return "", false
	}
	return rest[:cut], true
}

// NormalizedSchema is what the model is given for an MCP tool's arguments (OK), or why
// the tool can't be offered (Reason).
type NormalizedSchema struct {
	OK     bool
	Schema toolhub.JSONSchema
	// Note says which argument groups go together (a root anyOf / oneOf).
	Note   string
	Reason string
}

// NormalizeMcpInputSchema turns an MCP input schema into a tool's parameters
// (normalizeInputSchema of core/ts):
//   - $ref to the schema's own $defs / definitions are inlined (cycles become {});
//   - a root anyOf / oneOf / allOf is flattened into one object: allOf keeps every
//     branch's required list; anyOf / oneOf merge the properties and say in Note which
//     groups go together (the server still checks the combination);
//   - the root is { type: "object", properties, required } with required names that exist.
//
// Top-level property names must be 1-64 characters of A-Za-z0-9_.- (what the APIs
// accept). The schema encodes as the full JSON (toolhub.JSONSchema.Raw, each property
// too); its fields are the policy's view of it.
func NormalizeMcpInputSchema(input map[string]any) NormalizedSchema {
	root := input
	if root == nil {
		root = map[string]any{}
	}
	defs := map[string]any{}
	if d, ok := root["definitions"].(map[string]any); ok {
		for k, v := range d {
			defs[k] = v
		}
	}
	if d, ok := root["$defs"].(map[string]any); ok {
		for k, v := range d {
			defs[k] = v
		}
	}
	resolved, _ := inlineRefs(root, defs, 0, nil).(map[string]any)
	if resolved == nil {
		resolved = map[string]any{}
	}

	properties := map[string]any{}
	if p, ok := resolved["properties"].(map[string]any); ok {
		for k, v := range p {
			properties[k] = v
		}
	}
	required := stringsOf(resolved["required"])
	note := ""
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := resolved[key].([]any)
		var groups [][]string
		found := false
		for _, b := range branches {
			branch, ok := b.(map[string]any)
			if !ok {
				continue
			}
			found = true
			if p, ok := branch["properties"].(map[string]any); ok {
				for k, v := range p {
					properties[k] = v
				}
			}
			br := stringsOf(branch["required"])
			if key == "allOf" {
				required = append(required, br...)
			} else {
				groups = append(groups, br)
			}
		}
		if !found || key == "allOf" || !slices.ContainsFunc(groups, func(g []string) bool { return len(g) > 0 }) {
			continue
		}
		shown := make([]string, 0, len(groups))
		for _, g := range groups {
			if len(g) > 0 {
				shown = append(shown, "("+strings.Join(g, ", ")+")")
			} else {
				shown = append(shown, "(nothing more)")
			}
		}
		sep, lead := " or ", ""
		if key == "oneOf" {
			sep, lead = " or exactly one of ", "exactly one of "
		}
		text := "Arguments: give " + lead + strings.Join(shown, sep) + "."
		if note != "" {
			note += " " + text
		} else {
			note = text
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		if !mcpPropertyName.MatchString(name) {
			return NormalizedSchema{Reason: `its argument "` + name + `" has a name the model APIs don't accept`}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var req []string
	for _, r := range required {
		if _, ok := properties[r]; ok && !slices.Contains(req, r) {
			req = append(req, r)
		}
	}

	full := map[string]any{"type": "object", "properties": properties}
	schema := toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties()}
	if len(req) > 0 {
		full["required"] = req
		schema.Required = req
	}
	if d, ok := resolved["description"].(string); ok {
		full["description"] = d
		schema.Description = d
	}
	if resolved["additionalProperties"] == false {
		full["additionalProperties"] = false
	}
	for _, name := range names {
		schema.Properties.Set(name, propertySchema(properties[name]))
	}
	raw, err := jsonx.Marshal(full)
	if err != nil {
		return NormalizedSchema{Reason: "its input schema can't be encoded: " + err.Error()}
	}
	schema.Raw = raw
	return NormalizedSchema{OK: true, Schema: schema, Note: note}
}

// propertySchema is one property's schema: its full JSON as Raw, its type (the first
// that is not "null" of a list) for the policy.
func propertySchema(v any) toolhub.JSONSchema {
	out := toolhub.JSONSchema{}
	m, ok := v.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	switch t := m["type"].(type) {
	case string:
		out.Type = t
	case []any:
		for _, x := range t {
			if s, isString := x.(string); isString && s != "null" {
				out.Type = s
				break
			}
		}
	default:
	}
	if raw, err := jsonx.Marshal(m); err == nil {
		out.Raw = raw
	}
	return out
}

// McpToolDescription is a tool's description for the model: its own (or title), the
// combinator note, cut at limit (UTF-16 code units, as in core/ts).
func McpToolDescription(tool mcpserver.Tool, serverName, note string, limit int) string {
	own := strings.TrimSpace(tool.Description)
	if own == "" {
		own = strings.TrimSpace(tool.Title)
	}
	if own == "" {
		own = `The "` + tool.Name + `" tool of the MCP server ` + serverName + "."
	}
	text := own
	if note != "" {
		text += "\n\n" + note
	}
	if kernel.UTF16Len(text) > limit {
		return jsstr.TruncateUTF16(text, limit-1) + "…"
	}
	return text
}

// McpResultLimit is the result limit a tool asks for (_meta["anthropic/maxResultSizeChars"]),
// up to McpResultCharsCeiling; 0 when it asks for none.
func McpResultLimit(tool mcpserver.Tool) int {
	v, ok := tool.Meta["anthropic/maxResultSizeChars"].(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0
	}
	return int(math.Min(math.Floor(v), McpResultCharsCeiling))
}

// McpRequiresUser reports _meta["anthropic/requiresUserInteraction"] == true: every call
// asks, whatever the rules or mode.
func McpRequiresUser(tool mcpserver.Tool) bool {
	v, ok := tool.Meta["anthropic/requiresUserInteraction"].(bool)
	return ok && v
}

var mcpImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// McpToolOutput turns a tools/call result into a tool result: text blocks in order;
// images for the model to look at (PNG, JPEG, GIF, WebP, at most
// kernel.MaxImagesPerMessage); other blocks as a short note; structuredContent as JSON
// when there is no text. isError results are not retried.
func McpToolOutput(result mcpserver.CallToolResult) toolhub.Output {
	var parts []string
	var images []kernel.MessageAttachment
	for _, block := range result.Content {
		parts = append(parts, describeMcpBlock(block, &images)...)
	}
	hasText := slices.ContainsFunc(parts, func(p string) bool { return strings.TrimSpace(p) != "" })
	if !hasText && result.StructuredContent != nil {
		if b, err := jsonx.Marshal(result.StructuredContent); err == nil {
			parts = append(parts, indentJSON(b))
		}
	}
	content := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if content == "" {
		if result.IsError {
			content = "The MCP tool failed and said nothing more."
		} else {
			content = "(the MCP tool returned nothing)"
		}
	}
	out := toolhub.Output{Content: content}
	if result.IsError {
		t, f := true, false
		out.IsError, out.Retryable = &t, &f
	} else if len(images) > 0 {
		out.Images = images
	}
	return out
}

func describeMcpBlock(block mcpserver.Content, images *[]kernel.MessageAttachment) []string {
	str := func(k string) string { s, _ := block[k].(string); return s }
	switch t := str("type"); t {
	case "text":
		if s, ok := block["text"].(string); ok {
			return []string{s}
		}
		return nil
	case "image":
		mime, data := str("mimeType"), str("data")
		if data != "" && mcpImageTypes[mime] && len(*images) < kernel.MaxImagesPerMessage {
			*images = append(*images, kernel.MessageAttachment{Type: kernel.AttachmentTypeImage, MIMEType: mime, Data: data})
			return []string{fmt.Sprintf("[image %d: %s]", len(*images), mime)}
		}
		if mime == "" {
			mime = "unknown type"
		}
		return []string{"[an image (" + mime + ") that can't be shown]"}
	case "audio":
		mime := str("mimeType")
		if mime == "" {
			mime = "unknown type"
		}
		return []string{"[audio (" + mime + "), not shown]"}
	case "resource_link":
		var label []string
		for _, k := range []string{"name", "uri"} {
			if s := str(k); s != "" {
				label = append(label, s)
			}
		}
		desc := ""
		if d := str("description"); d != "" {
			desc = " — " + d
		}
		return []string{"[resource: " + strings.Join(label, " ") + desc + "]"}
	case "resource":
		r, ok := block["resource"].(map[string]any)
		if !ok {
			return []string{"[resource content, not shown]"}
		}
		uri, _ := r["uri"].(string)
		if text, isText := r["text"].(string); isText {
			return []string{"[resource " + uri + "]\n" + text}
		}
		size := 0
		if blob, isBlob := r["blob"].(string); isBlob {
			size = len(blob) * 3 / 4
		}
		mime, _ := r["mimeType"].(string)
		if mime == "" {
			mime = "unknown type"
		}
		return []string{fmt.Sprintf("[binary resource %s (%s, %d bytes), not shown]", uri, mime, size)}
	default:
		return []string{"[" + t + " content, not shown]"}
	}
}

// indentJSON indents compact JSON with two spaces (JSON.stringify(v, null, 2)).
func indentJSON(compact []byte) string {
	var b strings.Builder
	depth := 0
	inString, escaped := false, false
	newline := func() {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat("  ", depth))
	}
	for i := 0; i < len(compact); i++ {
		c := compact[i]
		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			b.WriteByte(c)
		case '{', '[':
			b.WriteByte(c)
			if i+1 < len(compact) && (compact[i+1] == '}' || compact[i+1] == ']') {
				b.WriteByte(compact[i+1])
				i++
				continue
			}
			depth++
			newline()
		case '}', ']':
			depth--
			newline()
			b.WriteByte(c)
		case ',':
			b.WriteByte(c)
			newline()
		case ':':
			b.WriteString(": ")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func inlineRefs(node any, defs map[string]any, depth int, seen []string) any {
	switch n := node.(type) {
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = inlineRefs(v, defs, depth+1, seen)
		}
		return out
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			m := mcpDefRef.FindStringSubmatch(ref)
			if m == nil {
				return map[string]any{}
			}
			name := strings.ReplaceAll(strings.ReplaceAll(m[1], "~1", "/"), "~0", "~")
			target, known := defs[name]
			if !known || slices.Contains(seen, name) || depth > 12 {
				return map[string]any{}
			}
			resolved := inlineRefs(target, defs, depth+1, append(slices.Clone(seen), name))
			rm, isMap := resolved.(map[string]any)
			if !isMap {
				return resolved
			}
			out := map[string]any{}
			for k, v := range rm {
				out[k] = v
			}
			for k, v := range n {
				if k != "$ref" {
					out[k] = inlineRefs(v, defs, depth+1, seen)
				}
			}
			return out
		}
		out := map[string]any{}
		for k, v := range n {
			if k == "$defs" || k == "definitions" || k == "$schema" || k == "$id" {
				continue
			}
			out[k] = inlineRefs(v, defs, depth+1, seen)
		}
		return out
	default:
		return node
	}
}

func stringsOf(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func fnv1a(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
