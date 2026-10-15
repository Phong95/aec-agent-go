package skill

import "regexp"

// The SKILL.md format limits. Name, description and compatibility lengths count
// UTF-16 code units (kernel.UTF16Len); every size counts bytes (len(s)).
const (
	MaxNameLength          = 64
	MaxDescriptionLength   = 1024
	MaxCompatibilityLength = 500
	// MaxContentBytes limits SKILL.md itself. The spec recommends < 500 lines;
	// move detail into references/.
	MaxContentBytes = 100_000
	MaxFiles        = 50
	MaxFileBytes    = 200_000
	MaxTotalBytes   = 1_000_000
	MaxPathLength   = 200
)

// NamePattern is the rule for a skill name: lowercase
// letters and digits in groups separated by single hyphens. It is ASCII only,
// so RE2 and the JS engine agree on every input.
var NamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// secretPattern is one credential shape and the label the error message uses.
type secretPattern struct {
	label   string
	pattern *regexp.Regexp
}

// secretPatterns are the values that look like credentials and must not be saved
// into a skill - skills are shared with other agents and shown in the UI. They
// are checked on content and every file, in this order (the first match per text
// wins). All five are ASCII only and RE2-safe: \b is an ASCII word boundary in
// both engines.
var secretPatterns = []secretPattern{
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"an AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"an API key (sk-\U00002026)", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`)},
	{"a GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
}
