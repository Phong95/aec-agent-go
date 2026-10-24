package skills

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The actions of skill_manage, in their fixed order.
const (
	actionCreate     = "create"
	actionRewrite    = "rewrite"
	actionPatch      = "patch"
	actionWriteFile  = "write_file"
	actionRemoveFile = "remove_file"
	actionDelete     = "delete"
)

// midDot is U+00B7, built from a rune value so this source stays ASCII-only (emDash, the other
// non-ASCII character of the tool texts, is defined in skill_view.go).
var midDot = string(rune(0x00b7))

var actions = []string{actionCreate, actionRewrite, actionPatch, actionWriteFile, actionRemoveFile, actionDelete}

// ManageTool is skill_manage: it lets an agent create and
// maintain skills in the shared skill library (used by the built-in skill-creator
// skill). A created skill is enabled for the agent right away; other agents can enable it
// too. Only custom skills can be changed, and an agent can only change skills that are
// enabled for it.
//
// It is a normal hub tool with the risk consequential: a skill changes how agents behave
// from then on, so every call waits for the user's approval, which shows the full content
// being saved, and with no interaction handler the call fails closed. It is registered by
// the app (AgentCore.UseTool), unlike skill_view, which the agent's SkillManager hands to
// the loop. The skills it works on come from toolhub.Context.Skills, the agent's own handle,
// and never from an agent id in the model's arguments (T-06-24).
//
// After each change the skill's .skills/<name>/ folder in an isolated workspace is brought
// in line with the library (rewritten, or removed on delete), so the agent never works
// from a stale copy. A failed sync is reported in the text but does not fail the call: the
// skill is saved either way.
//
// A ManageTool holds no state, so it is safe for concurrent use. Build one with
// NewManageTool.
type ManageTool struct{}

var _ toolhub.Handler = (*ManageTool)(nil)

// NewManageTool returns the skill_manage tool.
func NewManageTool() *ManageTool { return &ManageTool{} }

// Name returns the plugin name.
func (*ManageTool) Name() string { return "tool:skill-manage" }

// Version returns the plugin version.
func (*ManageTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a consequential
// tool with a required action. The files property is encoded type, items, description:
// the field order of toolhub.JSONSchema is fixed for every tool.
func (*ManageTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "skill_manage",
		Description: "Create, change or delete a skill (a SKILL.md with YAML frontmatter `name` + `description`, then instructions; " +
			"optional extra files such as scripts/ and references/). Follow the skill-creator skill when you have it. " +
			"Actions: create (content, files?) " + midDot + " rewrite (name, content, files? " + emDash + " replaces SKILL.md, and all files when given) " + midDot + " " +
			"patch (name, old_string, new_string, path? " + emDash + " exact replacement in SKILL.md or a file) " + midDot + " write_file (name, path, content) " + midDot + " " +
			"remove_file (name, path) " + midDot + " delete (name). Each call asks the user for approval.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "action", Schema: toolhub.JSONSchema{Type: "string", Enum: slices.Clone(actions), Description: "What to do"}},
				toolhub.Property{Name: "name", Schema: toolhub.JSONSchema{Type: "string", Description: "The skill to change (every action except create)"}},
				toolhub.Property{Name: "content", Schema: toolhub.JSONSchema{Type: "string", Description: "create/rewrite: the full SKILL.md text; write_file: the file content"}},
				toolhub.Property{Name: "files", Schema: toolhub.JSONSchema{
					Type:        "array",
					Description: "create/rewrite: extra files of the skill",
					Items: &toolhub.JSONSchema{
						Type: "object",
						Properties: toolhub.NewProperties(
							toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: `Relative path, e.g. "scripts/run.py" or "references/guide.md"`}},
							toolhub.Property{Name: "content", Schema: toolhub.JSONSchema{Type: "string", Description: "File content (text)"}},
						),
						Required: []string{"path", "content"},
					},
				}},
				toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: "patch/write_file/remove_file: a file in the skill (patch defaults to SKILL.md)"}},
				toolhub.Property{Name: "old_string", Schema: toolhub.JSONSchema{Type: "string", Description: "patch: exact text to replace " + emDash + " must occur exactly once"}},
				toolhub.Property{Name: "new_string", Schema: toolhub.JSONSchema{Type: "string", Description: "patch: the replacement text"}},
			),
			Required: []string{"action"},
		},
		Risk: toolhub.RiskConsequential,
		// A-137: no lasting changes from a subagent.
		Subagent: toolhub.SubagentNever,
	}
}

// Execute runs one action. Every problem the model can fix is a non-retryable error
// output: a missing or bad argument, a skill the agent does not have, a skill that is not
// custom, a patch that does not match once, a failure of the library with a SkillError
// code. Any other failure of the library is an error output with no retryable flag, so the
// harness may retry it. Execute never returns an error itself.
func (*ManageTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	if tc.Skills == nil {
		return toolkit.Fail("skill_manage: skills are not available here"), nil
	}
	a := toolkit.DecodeArgs(args)
	action := str(a, "action")
	if !slices.Contains(actions, action) {
		return toolkit.Fail(`skill_manage: "action" must be one of ` + strings.Join(actions, ", ")), nil
	}

	out, err := run(ctx, tc, a, action)
	if err == nil {
		return out, nil
	}
	var se *kernel.SkillError
	if errors.As(err, &se) {
		// Validation and permission failures are deterministic: the model must change the input.
		meta := ordered.NewObject()
		meta.Set("code", string(se.Code))
		return toolkit.Failure("skill_manage "+action+" failed ("+string(se.Code)+"): "+se.Message, false, meta), nil
	}
	return toolhub.Error("skill_manage " + action + " failed: " + err.Error()), nil
}

// run is the body of one action. The error it returns is the failure of the skill
// library, which Execute maps; every other outcome is an output.
func run(ctx context.Context, tc toolhub.Context, a toolkit.Args, action string) (toolhub.Output, error) {
	skills := tc.Skills

	if action == actionCreate {
		// The full SKILL.md is trimmed: its frontmatter must open the file, so
		// surrounding blank lines are never meaningful there (only a patch is exact).
		content := str(a, "content")
		if content == "" {
			return toolkit.Fail(`skill_manage create: "content" (the full SKILL.md) is required`), nil
		}
		files, _, problem := filesArg(a)
		if problem != "" {
			return toolkit.Fail(problem), nil
		}
		created, err := skills.Create(ctx, skill.Package{Content: content, Files: files})
		if err != nil {
			return toolhub.Output{}, err
		}
		note := ""
		if requested := str(a, "name"); requested != "" && requested != created.Name {
			note = ` (named from the frontmatter, not "` + requested + `")`
		}
		synced := CopyToWorkspace(ctx, created, tc, true)
		return done(
			`Created skill "`+created.Name+`"`+note+" with "+countFiles(created)+". It is enabled for you and appears in your skill index "+
				"from your next turn. Other agents can enable it on their Capabilities page."+workspaceNote(synced, "written to"),
			created, synced), nil
	}

	name := str(a, "name")
	if name == "" {
		return toolkit.Fail("skill_manage " + action + `: "name" is required`), nil
	}
	existing, found, err := skills.Get(ctx, name)
	if err != nil {
		return toolhub.Output{}, err
	}
	if !found {
		return toolkit.Fail("skill_manage " + action + `: you have no skill named "` + name + `"`), nil
	}
	if existing.Source != skill.SourceCustom {
		return toolkit.Fail("skill_manage " + action + " failed (read_only): \"" + name + `" is a ` + string(existing.Source) +
			" skill and cannot be changed " + emDash + " create a new skill with a different name instead"), nil
	}

	if action == actionDelete {
		if err := skills.Delete(ctx, name); err != nil {
			return toolhub.Output{}, err
		}
		removed := RemoveFromWorkspace(ctx, name, tc)
		meta := ordered.NewObject()
		meta.Set("skill", name)
		meta.Set("action", action)
		setWorkspaceMetadata(&meta, removed)
		return toolkit.Result(`Deleted skill "`+name+`".`+workspaceNote(removed, "removed from"), meta), nil
	}

	content := existing.Content
	files := slices.Clone(existing.Files)
	var summary string

	switch action {
	case actionRewrite:
		next := str(a, "content")
		if next == "" {
			return toolkit.Fail(`skill_manage rewrite: "content" (the full new SKILL.md) is required`), nil
		}
		nextFiles, hasFiles, problem := filesArg(a)
		if problem != "" {
			return toolkit.Fail(problem), nil
		}
		content = next
		summary = `Rewrote skill "` + name + `"`
		if hasFiles {
			files = nextFiles
			summary += " and replaced its files"
		}
	case actionPatch:
		// old_string and new_string are both read untrimmed (rawStr), so the patch replaces
		// exactly the text the model sent and that the approval prompt showed. Trimming only
		// old_string would leave a carried indentation or newline in place while new_string
		// adds its own.
		oldString := rawStr(a, "old_string")
		newString, hasNew := a.String("new_string")
		if oldString == "" || !hasNew {
			return toolkit.Fail(`skill_manage patch: "old_string" and "new_string" are required`), nil
		}
		rawPath := str(a, "path")
		path := "SKILL.md"
		if rawPath != "" {
			p, ok := skill.NormalizeSkillPath(rawPath)
			if !ok {
				return toolkit.Fail(`skill_manage patch: invalid path "` + rawPath + `"`), nil
			}
			path = p
		}
		if jsstr.ToLower(path) == "skill.md" {
			patched, problem := replaceOnce(content, oldString, newString)
			if problem != "" {
				return toolkit.Fail("skill_manage patch: " + problem + " in SKILL.md"), nil
			}
			content = patched
		} else {
			i := slices.IndexFunc(files, func(f skill.File) bool { return f.Path == path })
			if i < 0 {
				return toolkit.Fail(`skill_manage patch: skill "` + name + `" has no file "` + path + `"`), nil
			}
			patched, problem := replaceOnce(files[i].Content, oldString, newString)
			if problem != "" {
				return toolkit.Fail("skill_manage patch: " + problem + " in " + path), nil
			}
			files[i] = skill.File{Path: path, Content: patched}
		}
		summary = "Patched " + path + ` of skill "` + name + `"`
	case actionWriteFile:
		path, ok := optionalSkillPath(str(a, "path"))
		fileContent, hasContent := a.String("content")
		if !ok || !hasContent {
			return toolkit.Fail(`skill_manage write_file: a relative "path" and "content" are required`), nil
		}
		verb := "Added"
		if i := slices.IndexFunc(files, func(f skill.File) bool { return f.Path == path }); i >= 0 {
			files[i] = skill.File{Path: path, Content: fileContent}
			verb = "Replaced"
		} else {
			files = append(files, skill.File{Path: path, Content: fileContent})
		}
		summary = verb + " " + path + ` in skill "` + name + `"`
	case actionRemoveFile:
		rawPath := str(a, "path")
		path, ok := optionalSkillPath(rawPath)
		if ok {
			ok = slices.ContainsFunc(files, func(f skill.File) bool { return f.Path == path })
		}
		if !ok {
			return toolkit.Fail(`skill_manage remove_file: skill "` + name + `" has no file "` + rawPath + `"`), nil
		}
		files = slices.DeleteFunc(files, func(f skill.File) bool { return f.Path == path })
		summary = "Removed " + path + ` from skill "` + name + `"`
	}

	updated, err := skills.Update(ctx, name, skill.Package{Content: content, Files: files})
	if err != nil {
		return toolhub.Output{}, err
	}
	synced := CopyToWorkspace(ctx, updated, tc, true)
	return done(summary+" (version "+strconv.Itoa(updated.Version)+", "+countFiles(updated)+")."+workspaceNote(synced, "updated in"), updated, synced), nil
}

// replaceOnce replaces the one occurrence of oldString in text. The second result is the
// problem when there is none or more than one.
func replaceOnce(text, oldString, newString string) (string, string) {
	first := strings.Index(text, oldString)
	if first < 0 {
		return "", "old_string was not found"
	}
	end := first + len(oldString)
	if strings.Contains(text[end:], oldString) {
		return "", "old_string occurs more than once " + emDash + " include more surrounding text"
	}
	return text[:first] + newString + text[end:], ""
}

// filesArg reads the files argument. The bool says whether it was given: an
// absent key and a JSON null are not, a list (even an empty one) is. The last result is
// the problem text when the value is not a list of { path, content } objects with string
// members.
func filesArg(a toolkit.Args) ([]skill.File, bool, string) {
	raw, present := a.Raw("files")
	if !present || string(raw) == "null" {
		return nil, false, ""
	}
	var items []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &items) != nil {
		return nil, false, `skill_manage: "files" must be a list of { "path", "content" }`
	}
	files := make([]skill.File, 0, len(items))
	for _, item := range items {
		var obj map[string]json.RawMessage
		if len(item) > 0 && item[0] == '{' {
			_ = json.Unmarshal(item, &obj) // a failed decode leaves obj empty: the entry is refused below
		}
		path, okPath := stringValue(obj["path"])
		content, okContent := stringValue(obj["content"])
		if !okPath || !okContent {
			return nil, false, `skill_manage: every entry of "files" needs a string "path" and "content"`
		}
		files = append(files, skill.File{Path: path, Content: content})
	}
	return files, true, ""
}

// stringValue decodes a raw JSON value that must be a string.
func stringValue(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// optionalSkillPath normalises a skill path that may be absent: the normalised path, or
// false for an empty or unsafe one.
func optionalSkillPath(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	return skill.NormalizeSkillPath(raw)
}

// str reads a string argument trimmed with the JS trim, and "" for an absent, non-string
// or blank value.
func str(a toolkit.Args, key string) string {
	s, _ := a.String(key)
	return jsstr.Trim(s)
}

// rawStr reads a string argument untrimmed: the value as sent, and "" for an absent,
// non-string or blank value (the blank check uses a trimmed copy, so a whitespace-only
// value still counts as missing). It is used for content, old_string and the like, where
// the whitespace is part of the text (quirk Q29, fixed in Go).
func rawStr(a toolkit.Args, key string) string {
	s, _ := a.String(key)
	if jsstr.Trim(s) == "" {
		return ""
	}
	return s
}

// countFiles is "no extra files", "1 extra file" or "<n> extra files".
func countFiles(s skill.Skill) string {
	n := len(s.Files)
	switch n {
	case 0:
		return "no extra files"
	case 1:
		return "1 extra file"
	default:
		return strconv.Itoa(n) + " extra files"
	}
}

// workspaceNote is one sentence on the workspace copy, or "" when there is no sandbox.
// An empty Dir with an empty Err is the zero result (no sandbox).
func workspaceNote(r WorkspaceResult, verb string) string {
	switch {
	case r.Dir != "":
		return " Its folder was " + verb + " your workspace at " + r.Dir + "/."
	case r.Err != "":
		return " The saved skill is fine, but your workspace copy could not be synced (" + r.Err + ")."
	default:
		return ""
	}
}

// setWorkspaceMetadata adds workspaceDir or workspaceError to the metadata, or nothing for
// the zero result.
func setWorkspaceMetadata(meta *ordered.Object, r WorkspaceResult) {
	switch {
	case r.Dir != "":
		meta.Set("workspaceDir", r.Dir)
	case r.Err != "":
		meta.Set("workspaceError", r.Err)
	}
}

// done is the successful output of a create or an update: the text, and the metadata skill,
// version and files (the paths), then the workspace key.
func done(content string, s skill.Skill, synced WorkspaceResult) toolhub.Output {
	paths := make([]string, len(s.Files))
	for i, f := range s.Files {
		paths[i] = f.Path
	}
	meta := ordered.NewObject()
	meta.Set("skill", s.Name)
	meta.Set("version", s.Version)
	meta.Set("files", paths)
	setWorkspaceMetadata(&meta, synced)
	return toolkit.Result(content, meta)
}
