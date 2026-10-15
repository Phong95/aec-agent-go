package skill

import (
	"fmt"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// NormalizeSkillPath normalises a skill file path ("./scripts/a.py" becomes
// "scripts/a.py"). The bool is false when it is not a safe relative path: empty
// after trimming, longer than MaxPathLength UTF-16
// units, absolute, containing a backslash or a NUL, starting with a drive
// letter, or with an empty, "." or ".." segment. Inner spaces are kept.
//
// The steps use JS string semantics: trimming removes the JS whitespace set
// (jsstr.Trim), exactly one leading "./" is removed, and the length is UTF-16
// units.
func NormalizeSkillPath(path string) (string, bool) {
	trimmed := strings.TrimPrefix(jsstr.Trim(path), "./")
	if trimmed == "" || kernel.UTF16Len(trimmed) > MaxPathLength {
		return "", false
	}
	if strings.HasPrefix(trimmed, "/") || strings.Contains(trimmed, "\\") || strings.Contains(trimmed, "\x00") || hasDriveLetter(trimmed) {
		return "", false
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
	}
	return trimmed, true
}

// hasDriveLetter is /^[a-zA-Z]:/.
func hasDriveLetter(s string) bool {
	if len(s) < 2 || s[1] != ':' {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isSkillMdName is path.toLowerCase() === "skill.md". JS lowercases per code
// point, with one multi-character exception: U+0130 (capital I with dot) becomes
// "i" plus a combining dot, so it never matches an ASCII letter, while Go's
// unicode.ToLower maps it to a plain "i". The Kelvin sign (U+212A) lowercases to
// "k" in both. The JS rule now lives in jsstr.ToLower (which also holds the final
// sigma rule); this function only compares its result.
func isSkillMdName(path string) bool {
	return jsstr.ToLower(path) == "skill.md"
}

// validateFiles checks the extra files of a package and returns the accepted
// ones (normalised paths). Every problem is appended to problems so the model
// can fix them all in one go; a file with a problem is skipped, except a file
// that is only too large, which is kept so the secret scan still sees it. No
// "files must be a list" or "must have text content" check is needed: package
// skill's Package holds typed strings.
func validateFiles(files []File, problems *[]string) []File {
	if len(files) > MaxFiles {
		*problems = append(*problems, fmt.Sprintf("a skill can have at most %d files, got %d", MaxFiles, len(files)))
	}

	out := []File{}
	seen := map[string]bool{}
	total := 0
	for _, file := range files {
		path, ok := NormalizeSkillPath(file.Path)
		if !ok {
			*problems = append(*problems, fmt.Sprintf("file path \"%s\" must be a relative path inside the skill (e.g. \"scripts/run.py\"), without \"..\" or a leading \"/\"", file.Path))
			continue
		}
		if isSkillMdName(path) {
			*problems = append(*problems, "\"SKILL.md\" is the skill content itself \U00002014 do not add it to files")
			continue
		}
		if seen[path] {
			*problems = append(*problems, fmt.Sprintf("file \"%s\" is listed twice", path))
			continue
		}
		bytes := len(file.Content)
		if bytes > MaxFileBytes {
			*problems = append(*problems, fmt.Sprintf("file \"%s\" is %d bytes; the limit is %d", path, bytes, MaxFileBytes))
		}
		total += bytes
		seen[path] = true
		out = append(out, File{Path: path, Content: file.Content})
	}
	if total > MaxTotalBytes {
		*problems = append(*problems, fmt.Sprintf("the skill's files total %d bytes; the limit is %d", total, MaxTotalBytes))
	}
	return out
}
