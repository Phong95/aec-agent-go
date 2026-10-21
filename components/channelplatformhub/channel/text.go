package channel

import (
	"regexp"
	"strings"
	"unicode/utf16"
)

// SplitMessage splits text into parts of at most max UTF-16 units (the unit core/ts and
// most chat platforms count), preferring to break at a blank line, then a line break,
// then a space. It never splits a surrogate pair. Parts are trimmed; empty parts are
// dropped. A max below 1 is treated as 1.
func SplitMessage(text string, maxLen int) []string {
	if maxLen < 1 {
		maxLen = 1
	}
	var parts []string
	rest := strings.TrimSpace(text)
	for {
		u := utf16.Encode([]rune(rest))
		if len(u) <= maxLen {
			break
		}
		window := u[:maxLen]
		cut := lastBreak(window, "\n\n", maxLen)
		if cut < 0 {
			cut = lastBreak(window, "\n", maxLen)
		}
		if cut < 0 {
			cut = lastBreak(window, " ", maxLen)
		}
		if cut < 0 {
			cut = maxLen
			// Do not leave half of a surrogate pair at the end of a part.
			if utf16.IsSurrogate(rune(u[cut-1])) && u[cut-1] < 0xdc00 && cut > 1 {
				cut--
			}
		}
		if part := strings.TrimSpace(string(utf16.Decode(u[:cut]))); part != "" {
			parts = append(parts, part)
		}
		rest = strings.TrimSpace(string(utf16.Decode(u[cut:])))
	}
	if rest != "" {
		parts = append(parts, rest)
	}
	return parts
}

// lastBreak is the index of the last sep in window, ignoring breaks in the first
// quarter (they would make tiny parts); -1 when there is none.
func lastBreak(window []uint16, sep string, maxLen int) int {
	s := utf16.Encode([]rune(sep))
	for i := len(window) - len(s); i >= 0; i-- {
		match := true
		for j := range s {
			if window[i+j] != s[j] {
				match = false
				break
			}
		}
		if match {
			if i >= maxLen/4 {
				return i
			}
			return -1
		}
	}
	return -1
}

// Command is a chat command such as "/new@my_bot args".
type Command struct {
	// Name is lower case, without "/".
	Name string
	// Args is the text after the command, trimmed.
	Args string
	// Target is the "@bot" the command was addressed to, without "@" ("" when none).
	Target string
}

// commandPattern: the name starts with a letter or digit and holds letters, digits, "_"
// and "-", at most 32 (A-136: "-" too, for custom commands and skills such as
// /web-research; the same rule as the agent's command manager, package command).
var commandPattern = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9_-]{0,31})(?:@([A-Za-z0-9_]+))?(?:\s+([\s\S]*))?$`)

// ParseCommand reads "/name[@bot] args"; false when text is not a command.
// A-136: only at the start of the text; "/path/to/file" is not a command (the name must
// end at a space or the end). command.Parse reads commands by the same rule.
func ParseCommand(text string) (Command, bool) {
	m := commandPattern.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return Command{}, false
	}
	return Command{Name: strings.ToLower(m[1]), Args: strings.TrimSpace(m[3]), Target: m[2]}, true
}

// fenceReserve is the room kept in each part for the fence lines SplitMarkdown adds.
const fenceReserve = 16

var fenceLine = regexp.MustCompile("^\\s*(```+)(.*)$")

// SplitMarkdown is SplitMessage for Markdown: a part that ends inside a ``` code block
// gets the block closed, and the next part reopens it with the same language, so every
// part renders on its own. Parts stay within maxLen.
func SplitMarkdown(text string, maxLen int) []string {
	if maxLen <= fenceReserve*2 {
		return SplitMessage(text, maxLen)
	}
	raw := SplitMessage(text, maxLen-fenceReserve)
	parts := make([]string, 0, len(raw))
	open := "" // the fence line of a block left open by the previous part
	for _, part := range raw {
		body := part
		if open != "" {
			body = open + "\n" + part
		}
		open = openFenceAtEnd(body)
		if open != "" {
			body += "\n```"
		}
		parts = append(parts, body)
	}
	return parts
}

// openFenceAtEnd is the opening fence line ("```ts") when text ends inside a fenced code
// block, else "".
func openFenceAtEnd(text string) string {
	open := ""
	for line := range strings.SplitSeq(text, "\n") {
		m := fenceLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if open == "" {
			// The language is cut to 8 characters so a reopened block fits fenceReserve.
			lang := []rune(strings.TrimSpace(m[2]))
			if len(lang) > 8 {
				lang = lang[:8]
			}
			open = "```" + string(lang)
		} else {
			open = ""
		}
	}
	return open
}
