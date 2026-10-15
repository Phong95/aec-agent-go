package skill

import (
	"fmt"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// SKILL.md format - parse and validate a skill package (Agent Skills spec,
// agentskills.io/specification). Frontmatter is read with a small YAML subset
// parser: scalars (plain, quoted, | and > blocks), [flow] and "- item" lists,
// nested maps. That covers what skills use without a YAML dependency; anything
// it cannot read is reported as an "invalid" error.
//
// The error messages are part of the contract and are pinned by golden tests.
// ---------------------------------------------------------------------------

// ParsePackage parses and validates a skill package. The error is a
// *kernel.SkillError with code invalid listing every problem, so a model (or a
// user) can fix them all in one go.
//
// A "__proto__" key of the frontmatter is read through a hidden prototype map
// (see yamlMap).
func ParsePackage(pkg Package) (Parsed, error) {
	problems := []string{}
	content := strings.ReplaceAll(pkg.Content, "\r\n", "\n")
	name := ""

	frontmatter, rest, ok := splitFrontmatter(content)
	if !ok {
		return Parsed{}, invalid(name, []string{
			"SKILL.md must start with YAML frontmatter between \"---\" lines, e.g.\n---\nname: my-skill\ndescription: What it does and when to use it.\n---",
		})
	}

	raw, err := parseYAMLDoc(frontmatter)
	if err != nil {
		return Parsed{}, invalid(name, []string{"frontmatter: " + err.Error()})
	}

	nameValue, nameFound := raw.lookup("name")
	name = jsstr.Trim(firstString(asString(nameValue, nameFound)))
	descriptionValue, descriptionFound := raw.lookup("description")
	description := jsstr.Trim(firstString(asString(descriptionValue, descriptionFound)))
	licenseValue, licenseFound := raw.lookup("license")
	license := jsstr.Trim(firstString(asString(licenseValue, licenseFound)))
	compatibilityValue, compatibilityFound := raw.lookup("compatibility")
	compatibility := jsstr.Trim(firstString(asString(compatibilityValue, compatibilityFound)))
	allowedToolsValue, allowedToolsFound := raw.lookup("allowed-tools")
	allowedTools := asList(allowedToolsValue, allowedToolsFound)
	metadataValue, metadataFound := raw.lookup("metadata")
	metadata := flattenMetadata(metadataValue, metadataFound)
	body := jsstr.Trim(rest)

	if name == "" {
		problems = append(problems, "frontmatter \"name\" is required")
	} else {
		if kernel.UTF16Len(name) > MaxNameLength {
			problems = append(problems, fmt.Sprintf("\"name\" is longer than %d characters", MaxNameLength))
		}
		if !NamePattern.MatchString(name) {
			problems = append(problems, fmt.Sprintf("\"name\" must be lowercase letters, digits and single hyphens (e.g. \"quantity-takeoff\"), got \"%s\"", name))
		}
	}
	if description == "" {
		problems = append(problems, "frontmatter \"description\" is required \U00002014 say what the skill does and when to use it")
	} else if n := kernel.UTF16Len(description); n > MaxDescriptionLength {
		problems = append(problems, fmt.Sprintf("\"description\" is %d characters; the limit is %d", n, MaxDescriptionLength))
	}
	if compatibility != "" && kernel.UTF16Len(compatibility) > MaxCompatibilityLength {
		problems = append(problems, fmt.Sprintf("\"compatibility\" is longer than %d characters", MaxCompatibilityLength))
	}
	if _, isMap := metadataValue.(ordered.Object); metadataFound && !isMap {
		problems = append(problems, "\"metadata\" must be a map of keys to values")
	}
	if body == "" {
		problems = append(problems, "SKILL.md has no instructions after the frontmatter")
	}

	if contentBytes := len(content); contentBytes > MaxContentBytes {
		problems = append(problems, fmt.Sprintf("SKILL.md is %d bytes; the limit is %d \U00002014 move detail into references/ files", contentBytes, MaxContentBytes))
	}

	files := validateFiles(pkg.Files, &problems)

	texts := make([]File, 0, len(files)+1)
	texts = append(texts, File{Path: "SKILL.md", Content: content})
	texts = append(texts, files...)
	for _, t := range texts {
		for _, s := range secretPatterns {
			if s.pattern.MatchString(t.Content) {
				problems = append(problems, fmt.Sprintf("%s appears to contain %s \U00002014 never put credentials in a skill", t.Path, s.label))
				break
			}
		}
	}

	if len(problems) > 0 {
		return Parsed{}, invalid(name, problems)
	}

	fm := Frontmatter{Name: name, Description: description, Metadata: metadata}
	fm.License = license
	fm.Compatibility = compatibility
	if len(allowedTools) > 0 {
		fm.AllowedTools = allowedTools
	}
	return Parsed{
		Frontmatter:   fm,
		Body:          body,
		Files:         files,
		RequiresTools: requiredTools(metadata),
	}, nil
}

// firstString drops the found flag of asString: a missing value reads as "".
func firstString(s string, _ bool) string { return s }

// invalid builds the SkillError that lists every problem.
func invalid(name string, problems []string) error {
	var b strings.Builder
	b.WriteString("Invalid skill")
	if name != "" {
		b.WriteString(" \"" + name + "\"")
	}
	b.WriteString(":\n")
	for i, p := range problems {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- " + p)
	}
	return kernel.NewSkillError(b.String(), kernel.SkillCodeInvalid, name, false)
}
