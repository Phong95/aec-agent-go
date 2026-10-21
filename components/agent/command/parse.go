package command

import (
	"regexp"
	"slices"
	"strings"
)

// A-136: reading slash commands - shared by the agent's command manager and its chat-app
// channels (channel.ParseCommand reads by the same rule).

// Parsed is a command read from text.
type Parsed struct {
	// Name is lower case, without "/".
	Name string `json:"name"`
	// Args is the text after the command, trimmed.
	Args string `json:"args"`
	// Target is the "@bot" the command was addressed to ("/new@my_bot"), without "@"
	// ("" when none).
	Target string `json:"target,omitempty"`
}

// NamePattern is the rule of a command or alias name: lower-case letters, digits, "_"
// and "-", starting with a letter or digit, at most 32.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

var parsePattern = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9_-]{0,31})(?:@([A-Za-z0-9_]+))?(?:\s+([\s\S]*))?$`)

// Parse reads "/name[@bot] args" - only at the start of the text. False when text is
// not a command ("/path/to/file" is not: the name must end at a space or the end).
// Names may contain "-" (custom commands, skills such as /web-research).
func Parse(text string) (Parsed, bool) {
	m := parsePattern.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return Parsed{}, false
	}
	return Parsed{Name: strings.ToLower(m[1]), Args: strings.TrimSpace(m[3]), Target: m[2]}, true
}

// Validate checks a spec's name, aliases, handler and arguments. The error is an Error
// with CodeInvalid.
func Validate(spec Spec) error {
	for _, n := range append([]string{spec.Name}, spec.Aliases...) {
		if !NamePattern.MatchString(n) {
			return NewError(`Invalid command name "`+n+`": use lower-case letters, digits, "_" and "-" (at most 32, starting with a letter or digit)`, CodeInvalid)
		}
	}
	if (spec.Run == nil) == (spec.Prompt == "") {
		return NewError(`Command "/`+spec.Name+`" needs exactly one of run and prompt`, CodeInvalid)
	}
	for i, a := range spec.Arguments {
		if a.Rest && i < len(spec.Arguments)-1 {
			return NewError(`Command "/`+spec.Name+`": only the last argument can take the rest of the line`, CodeInvalid)
		}
	}
	return nil
}

// CheckClash returns an Error with CodeConflict when spec's name or an alias is taken by
// one of entries - unless it replaces a replaceable built-in of the same name with
// RegisterOptions.Replace. source is where spec comes from (for the message).
func CheckClash(spec Spec, opts RegisterOptions, entries []Entry, source Source) error {
	names := append([]string{spec.Name}, spec.Aliases...)
	for _, e := range entries {
		taken := append([]string{e.Spec.Name}, e.Spec.Aliases...)
		clash := ""
		for _, n := range names {
			if slices.Contains(taken, n) {
				clash = n
				break
			}
		}
		if clash == "" {
			continue
		}
		replaces := opts.Replace && e.Source == SourceBuiltIn && e.Replaceable && e.Spec.Name == spec.Name
		if replaces {
			continue
		}
		if opts.Replace && e.Spec.Name == spec.Name {
			return NewError("/"+spec.Name+" can't be replaced", CodeConflict)
		}
		kind := "agent"
		switch e.Source {
		case SourceBuiltIn:
			kind = "built-in"
		case SourceHost:
			kind = "host"
		case SourceAgent:
		}
		alias := ""
		if e.Spec.Name != clash {
			alias = " (an alias of /" + e.Spec.Name + ")"
		}
		return NewError("/"+clash+" is already a "+kind+" command"+alias+"; "+string(source)+" commands need their own names", CodeConflict)
	}
	return nil
}

// Usage is a spec's usage line, e.g. "/compact [focus]", "/rename <name>",
// "/permissions [ask|yolo|plan]".
func Usage(spec Spec) string {
	parts := []string{"/" + spec.Name}
	for _, a := range spec.Arguments {
		label := a.Name
		if len(a.Choices) > 0 {
			label = strings.Join(a.Choices, "|")
		}
		if a.Required {
			parts = append(parts, "<"+label+">")
		} else {
			parts = append(parts, "["+label+"]")
		}
	}
	return strings.Join(parts, " ")
}
