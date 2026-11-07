package variable

import (
	"regexp"
	"slices"
	"strconv"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-215: variable names, values and the {{var:NAME}} placeholder - shared by the
// VariableHub, each agent's variable manager and the tools (rules.ts of core/ts).
// ---------------------------------------------------------------------------

var nameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Placeholder matches {{var:NAME}} - what the model writes where a tool should put a
// variable's value. Group 1 is the name.
var Placeholder = regexp.MustCompile(`\{\{\s*var:([A-Z][A-Z0-9_]*)\s*\}\}`)

// PlaceholderNames returns the names a text refers to with {{var:NAME}}, in order of first
// use, without duplicates.
func PlaceholderNames(text string) []string {
	out := []string{}
	for _, m := range Placeholder.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// FillPlaceholders replaces each {{var:NAME}} with its value; a name missing from values
// is left as it is.
func FillPlaceholders(text string, values map[string]string) string {
	return Placeholder.ReplaceAllStringFunc(text, func(whole string) string {
		m := Placeholder.FindStringSubmatch(whole)
		if v, ok := values[m[1]]; ok {
			return v
		}
		return whole
	})
}

// CheckName returns the name trimmed, or an *Error ("invalid_argument") unless it is a
// valid variable name.
func CheckName(name string) (string, error) {
	n := jsstr.Trim(name)
	if !nameRe.MatchString(n) || len(n) > MaxNameChars {
		return "", NewError(`variable name "`+n+`" must be upper-case letters, digits and underscores, starting with a letter (max `+strconv.Itoa(MaxNameChars)+")", CodeInvalidArgument)
	}
	return n, nil
}

// CheckValue returns an *Error ("invalid_argument") unless value can be stored. An empty
// value is allowed.
func CheckValue(value string) error {
	if len(value) > MaxValueBytes {
		return NewError("a variable's value is larger than "+strconv.Itoa(MaxValueBytes)+" bytes", CodeInvalidArgument)
	}
	return nil
}

// CheckDescription returns the description trimmed ("" = none), or an *Error
// ("invalid_argument") when it is too long.
func CheckDescription(description string) (string, error) {
	d := jsstr.Trim(description)
	if kernel.UTF16Len(d) > MaxDescriptionChars {
		return "", NewError("description is longer than "+strconv.Itoa(MaxDescriptionChars)+" characters", CodeInvalidArgument)
	}
	return d, nil
}
