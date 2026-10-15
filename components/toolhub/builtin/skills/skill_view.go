package skills

import (
	"context"
	"strings"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// emDash is U+2014, built from a rune value so this source stays ASCII-only.
var emDash = string(rune(0x2014))

// ViewTool is skill_view: it loads a skill (progressive
// disclosure, level 2 and 3), the SKILL.md instructions plus the list of its files,
// or one file with the path argument.
//
// It is not registered in the tool hub: the agent's SkillManager adds it to the
// model's tools whenever the agent has at least one usable skill, so enabling a
// skill is enough. The skills it reads come from toolhub.Context.Skills, the agent's
// own handle, and the agent id never comes from the model's arguments (T-06-24).
//
// A ViewTool holds no state, so it is safe for concurrent use. Build one with
// NewViewTool.
type ViewTool struct{}

var _ toolhub.Handler = (*ViewTool)(nil)

// NewViewTool returns the skill_view tool.
func NewViewTool() *ViewTool { return &ViewTool{} }

// Name returns the plugin name.
func (*ViewTool) Name() string { return "tool:skill-view" }

// Version returns the plugin version.
func (*ViewTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a read-only
// tool with a required name and an optional path.
func (*ViewTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "skill_view",
		Description: "Load one of your skills (listed under <available_skills> in your instructions). Returns the skill's " +
			"instructions and its files " + emDash + " follow the instructions to do the task. Load a skill BEFORE starting a task it " +
			"matches. Pass `path` to read one of the skill's files (e.g. references/format.md).",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "name", Schema: toolhub.JSONSchema{Type: "string", Description: "The skill name, exactly as listed in <available_skills>"}},
				toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: "Optional: a file inside the skill to read instead of its instructions"}},
			),
			Required: []string{"name"},
		},
		Risk: toolhub.RiskReadOnly,
	}
}

// Execute loads the skill named by the arguments. Every failure the model can fix
// is a tool-level error output (isError true, not retryable); a failing skill store
// is returned as an error, which the executor turns into an error result.
func (t *ViewTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	name := stringArgument(args, "name")
	if name == "" {
		return fail(`skill_view: "name" must be a non-empty string`), nil
	}
	if tc.Skills == nil {
		return fail("skill_view: skills are not available here"), nil
	}

	sk, found, err := tc.Skills.Get(ctx, name)
	if err != nil {
		return toolhub.Output{}, err
	}
	if !found {
		available, err := tc.Skills.List(ctx)
		if err != nil {
			return toolhub.Output{}, err
		}
		names := make([]string, len(available))
		for i, s := range available {
			names[i] = s.Name
		}
		msg := `skill_view: no skill named "` + name + `". `
		if len(names) > 0 {
			msg += "Your skills: " + strings.Join(names, ", ") + "."
		} else {
			msg += "You have no skills enabled."
		}
		return fail(msg), nil
	}

	if rawPath := stringArgument(args, "path"); rawPath != "" {
		return viewFile(sk, rawPath), nil
	}

	// A skill with files is copied to .skills/<name>/ of an isolated sandbox, so its
	// scripts can be run from there.
	var copied WorkspaceResult
	if len(sk.Files) > 0 {
		copied = CopyToWorkspace(ctx, sk, tc, false)
	}
	return toolhub.Output{
		Content:  renderSkill(sk, copied),
		Metadata: viewMetadata(sk, copied),
	}, nil
}

// viewFile is the file branch of skill_view: SKILL.md (any case) gives the skill
// content block, a file of the skill gives its content block, and an unknown or
// unsafe path is an error that lists the files the skill has. The path is looked up
// in the skill package after skill.NormalizeSkillPath and never reaches a sandbox
// (T-10-18).
func viewFile(sk skill.Skill, rawPath string) toolhub.Output {
	path, ok := skill.NormalizeSkillPath(rawPath)
	if ok && jsstr.ToLower(path) == "skill.md" {
		return toolhub.Output{Content: `<skill_file skill="` + sk.Name + `" path="SKILL.md">` + "\n" + sk.Content + "\n</skill_file>"}
	}
	if ok {
		for _, f := range sk.Files {
			if f.Path != path {
				continue
			}
			meta := ordered.NewObject()
			meta.Set("skill", sk.Name)
			meta.Set("path", f.Path)
			return toolhub.Output{
				Content:  `<skill_file skill="` + sk.Name + `" path="` + f.Path + `">` + "\n" + f.Content + "\n</skill_file>",
				Metadata: meta,
			}
		}
	}
	paths := make([]string, len(sk.Files))
	for i, f := range sk.Files {
		paths[i] = f.Path
	}
	msg := `skill_view: skill "` + sk.Name + `" has no file "` + rawPath + `". `
	if len(paths) > 0 {
		msg += "Its files: " + strings.Join(paths, ", ") + "."
	} else {
		msg += "It has no files besides SKILL.md."
	}
	return fail(msg)
}

// viewMetadata is the metadata of a rendered skill, in a fixed key order: skill,
// source, version, files, and copiedTo only when a workspace folder was written.
// An empty source and a zero version are left out, as JSON.stringify leaves out an
// undefined value.
func viewMetadata(sk skill.Skill, copied WorkspaceResult) ordered.Object {
	m := ordered.NewObject()
	m.Set("skill", sk.Name)
	if sk.Source != "" {
		m.Set("source", string(sk.Source))
	}
	if sk.Version != 0 {
		m.Set("version", sk.Version)
	}
	m.Set("files", len(sk.Files))
	if copied.Dir != "" {
		m.Set("copiedTo", copied.Dir)
	}
	return m
}

// renderSkill is the skill_content block the model reads: the body, then, when the
// skill has files, the resource list, the hint to read a file and one line about the
// workspace copy (written, refused, or nothing when there is no sandbox).
func renderSkill(sk skill.Skill, copied WorkspaceResult) string {
	lines := []string{`<skill_content name="` + sk.Name + `">`, sk.Body}
	if len(sk.Files) > 0 {
		lines = append(lines, "", "<skill_resources>")
		for _, f := range sk.Files {
			lines = append(lines, f.Path)
		}
		lines = append(lines, "</skill_resources>")
		lines = append(lines, `Read a file with skill_view(name="`+sk.Name+`", path="<file>").`)
		switch {
		case copied.Dir != "":
			lines = append(lines, "The files are also in your workspace at "+copied.Dir+"/ "+emDash+" run scripts from there (e.g. python3 "+copied.Dir+"/scripts/<script>).")
		case copied.Err != "":
			lines = append(lines, "The files could not be copied to your workspace ("+copied.Err+"); read them with skill_view instead.")
		}
	}
	lines = append(lines, "</skill_content>")
	return strings.Join(lines, "\n")
}

// stringArgument reads a string argument and trims it with the JS trim. A missing,
// null or non-string value gives "". It delegates to the shared per-key decoding of
// internal/toolkit, so one out-of-range number in another argument (1e999, which JS
// reads as Infinity) no longer discards this one.
func stringArgument(args kernel.Arguments, key string) string {
	s, _ := toolkit.DecodeArgs(args).String(key)
	return jsstr.Trim(s)
}

// fail is the fail helper: an error output the harness must not retry. It delegates
// to toolkit.Fail, which every built-in tool package shares.
func fail(content string) toolhub.Output {
	return toolkit.Fail(content)
}
