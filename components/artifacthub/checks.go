package artifacthub

import (
	"regexp"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Names may nest ("reports/q3.md") but must not look like paths that walk out of
// anywhere. Both patterns are ASCII only, so the {0,199} and {0,99} counts are
// the same in bytes and in UTF-16 units, and RE2 reads them as the JS regex does
// ($ without the multiline flag matches only at the very end of the text).
var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._\-/]{0,199}$`)
	typePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-/]{0,99}$`)
)

// The input limits, counted in UTF-16 units (kernel.UTF16Len).
const (
	maxNoteLength        = 500
	maxDescriptionLength = 1000
	maxTags              = 20
	maxTagLength         = 50
)

// missingFileCodes are the SandboxError codes that mean "the file just is not
// there" - mapped to a missing state instead of propagated as they are.
var missingFileCodes = map[kernel.SandboxErrorCode]struct{}{
	kernel.SandboxCodeNotFound:    {},
	kernel.SandboxCodeInvalidPath: {},
	kernel.SandboxCodePathEscape:  {},
}

// invalidArgument is the ArtifactError the checks fail with.
func invalidArgument(message string) error {
	return kernel.NewArtifactError(message, kernel.ArtifactCodeInvalidArgument, "")
}

// checkName trims the name and validates it: the pattern, no "..", no "//" and
// no trailing "/".
func checkName(name string) (string, error) {
	value := jsstr.Trim(name)
	if !namePattern.MatchString(value) || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.HasSuffix(value, "/") {
		return "", invalidArgument(`Artifact name must be 1-200 characters of letters, digits, spaces, ".", "_", "-" or "/" (e.g. "reports/clash-report.md")`)
	}
	return value, nil
}

// checkType trims the type and validates it against the MIME or domain type pattern.
func checkType(typ string) (string, error) {
	value := jsstr.Trim(typ)
	if !typePattern.MatchString(value) {
		return "", invalidArgument(`Artifact type must be a MIME type ("text/markdown") or a domain type ("aec/takeoff")`)
	}
	return value, nil
}

// checkDescription trims the description and cuts it to 1000 UTF-16 units. An
// empty result means "no description", which clears the field on update. No
// "must be a string" check is needed: the type is a string.
func checkDescription(description string) string {
	return jsstr.TruncateUTF16(jsstr.Trim(description), maxDescriptionLength)
}

// checkTags trims every tag, cuts it to 50 UTF-16 units, drops the empty ones,
// keeps the first 20 and THEN removes duplicates (first occurrence wins), so the
// result can be shorter than 20. It returns nil when nothing is left. No "must be
// an array of strings" check is needed: the type is []string.
func checkTags(tags []string) []string {
	cleaned := make([]string, 0, len(tags))
	for _, tag := range tags {
		t := jsstr.TruncateUTF16(jsstr.Trim(tag), maxTagLength)
		if t != "" {
			cleaned = append(cleaned, t)
		}
	}
	if len(cleaned) > maxTags {
		cleaned = cleaned[:maxTags]
	}
	if len(cleaned) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(cleaned))
	out := make([]string, 0, len(cleaned))
	for _, t := range cleaned {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// cleanNote trims the note and cuts it to 500 UTF-16 units; the version carries
// it only when the result is not empty.
func cleanNote(note string) string {
	return jsstr.TruncateUTF16(jsstr.Trim(note), maxNoteLength)
}
