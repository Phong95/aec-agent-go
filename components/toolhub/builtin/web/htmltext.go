package web

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// HTML to readable text. Linear-time indexOf scanning only: the input is
// attacker-controlled, so no backtracking regular expression runs over the raw body. The
// scanners are hand written (no x/net/html, no regexp) with a fixed scan order, so the
// text of a page is deterministic.
// ---------------------------------------------------------------------------

// ellipsis is U+2026, written as bytes so that this source stays ASCII.
const ellipsis = "\xE2\x80\xA6"

// droppedElements are the elements whose whole content is dropped (not readable text),
// including the page chrome (menus, sidebars and footers crowd out the content).
var droppedElements = []string{"script", "style", "noscript", "template", "svg", "head", "iframe", "canvas", "nav", "aside", "footer"}

// blockTags are the tags that end a line of readable text.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "ul": true, "ol": true, "tr": true, "table": true, "section": true,
	"article": true, "header": true, "footer": true, "nav": true, "aside": true, "main": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "pre": true, "blockquote": true, "hr": true, "dt": true, "dd": true,
	"figcaption": true, "form": true, "title": true,
}

// namedEntities is the entity table (the values are code points, so this
// source stays ASCII). Only these names decode; every other &name; stays as it is.
var namedEntities = map[string]rune{
	"amp": '&', "lt": '<', "gt": '>', "quot": '"', "apos": '\'', "nbsp": ' ', "ndash": 0x2013, "mdash": 0x2014,
	"hellip": 0x2026, "lsquo": 0x2018, "rsquo": 0x2019, "ldquo": 0x201C, "rdquo": 0x201D, "copy": 0xA9, "reg": 0xAE, "trade": 0x2122,
	"deg": 0xB0, "middot": 0xB7, "bull": 0x2022, "times": 0xD7, "laquo": 0xAB, "raquo": 0xBB,
}

// decodeEntities is decodeEntities: /&(#x[0-9a-f]{1,6}|#[0-9]{1,7}|[a-z]{2,8});/gi, hand
// matched. A numeric entity outside 1..0x10FFFF stays as written. A numeric entity for a
// surrogate (&#xD800;) is a lone surrogate in JS; a Go string cannot hold one, so it
// becomes U+FFFD.
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		amp := strings.IndexByte(s[i:], '&')
		if amp < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+amp])
		i += amp
		body, n := matchEntity(s[i:])
		if n == 0 {
			b.WriteByte('&')
			i++
			continue
		}
		if r, ok := entityValue(body); ok {
			b.WriteString(r)
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// matchEntity matches the entity pattern at the start of s (which begins with '&') and
// returns the text between '&' and ';' and the length of the whole match (0 when there is
// none). The alternatives are tried in the regexp order and each one ends in ';'.
func matchEntity(s string) (string, int) {
	rest := s[1:]
	if len(rest) > 0 && rest[0] == '#' {
		if len(rest) > 1 && (rest[1] == 'x' || rest[1] == 'X') {
			j := 2
			for j < len(rest) && j-2 < 6 && isHexDigit(rest[j]) {
				j++
			}
			if j > 2 && j < len(rest) && rest[j] == ';' {
				return rest[:j], 1 + j + 1
			}
		}
		j := 1
		for j < len(rest) && j-1 < 7 && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j > 1 && j < len(rest) && rest[j] == ';' {
			return rest[:j], 1 + j + 1
		}
		return "", 0
	}
	j := 0
	for j < len(rest) && j < 8 && (rest[j] >= 'a' && rest[j] <= 'z' || rest[j] >= 'A' && rest[j] <= 'Z') {
		j++
	}
	if j >= 2 && j < len(rest) && rest[j] == ';' {
		return rest[:j], 1 + j + 1
	}
	return "", 0
}

func isHexDigit(c byte) bool {
	_, ok := hexVal(c)
	return ok
}

// entityValue is the text an entity body stands for; false means "leave the match as it is".
func entityValue(body string) (string, bool) {
	if body[0] == '#' {
		var code int64
		var err error
		if body[1] == 'x' || body[1] == 'X' {
			code, err = strconv.ParseInt(body[2:], 16, 64)
		} else {
			code, err = strconv.ParseInt(body[1:], 10, 64)
		}
		if err != nil || code < 1 || code > 0x10ffff {
			return "", false
		}
		if code >= 0xd800 && code <= 0xdfff {
			return replacementChar, true
		}
		return string(rune(code)), true
	}
	r, ok := namedEntities[asciiLower(body)]
	if !ok {
		return "", false
	}
	return string(r), true
}

func hasPrefixAt(s string, off int, prefix string) bool {
	return off >= 0 && off <= len(s) && strings.HasPrefix(s[off:], prefix)
}

// dropElements removes every `<name ...> ... </name>` block of the dropped elements and
// every HTML comment. An unterminated dropped element or comment drops the rest of the
// page.
func dropElements(html string) string {
	lower := asciiLower(html)
	var out strings.Builder
	i := 0
	for i < len(html) {
		lt := strings.IndexByte(html[i:], '<')
		if lt < 0 {
			out.WriteString(html[i:])
			break
		}
		lt += i
		out.WriteString(html[i:lt])
		// HTML comments are dropped too.
		if hasPrefixAt(lower, lt, "<!--") {
			end := strings.Index(html[lt+4:], "-->")
			if end < 0 {
				break
			}
			i = lt + 4 + end + 3
			continue
		}
		dropped := ""
		for _, name := range droppedElements {
			if !hasPrefixAt(lower, lt+1, name) {
				continue
			}
			p := lt + 1 + len(name)
			if p >= len(lower) {
				dropped = name
				break
			}
			r, _ := utf8.DecodeRuneInString(lower[p:])
			if r == '>' || r == '/' || jsstr.IsSpace(r) {
				dropped = name
				break
			}
		}
		if dropped != "" {
			closeIdx := strings.Index(lower[lt+1:], "</"+dropped)
			if closeIdx < 0 {
				break // unterminated: drop the rest
			}
			closeIdx += lt + 1
			closeEnd := strings.IndexByte(html[closeIdx:], '>')
			if closeEnd < 0 {
				break
			}
			i = closeIdx + closeEnd + 1
			continue
		}
		out.WriteByte('<')
		i = lt + 1
	}
	return out.String()
}

// stripTags removes the remaining tags, turning block-level tags into line breaks and
// table cells into " | ".
func stripTags(html string) string {
	var out strings.Builder
	i := 0
	for i < len(html) {
		lt := strings.IndexByte(html[i:], '<')
		if lt < 0 {
			out.WriteString(html[i:])
			break
		}
		lt += i
		out.WriteString(html[i:lt])
		gt := strings.IndexByte(html[lt+1:], '>')
		if gt < 0 {
			// A stray "<" with no closing ">": keep it as text and stop scanning for tags.
			out.WriteString(html[lt:])
			break
		}
		gt += lt + 1
		tag := asciiLower(jsstr.Trim(html[lt+1 : gt]))
		tag = strings.TrimPrefix(tag, "/")
		name := tag
		if k := strings.IndexFunc(tag, func(r rune) bool { return jsstr.IsSpace(r) || r == '/' }); k >= 0 {
			name = tag[:k]
		}
		if blockTags[name] {
			out.WriteByte('\n')
		} else if name == "td" || name == "th" {
			out.WriteString(" | ")
		}
		i = gt + 1
	}
	return out.String()
}

// mainContent is the page's primary content: the inside of the first <main> (else
// <article>) element, or the whole document when neither exists.
func mainContent(html string) string {
	lower := asciiLower(html)
	for _, name := range []string{"main", "article"} {
		start := strings.Index(lower, "<"+name)
		for start != -1 {
			p := start + 1 + len(name)
			if p < len(lower) {
				r, _ := utf8.DecodeRuneInString(lower[p:])
				if r == '>' || jsstr.IsSpace(r) {
					break
				}
			}
			next := strings.Index(lower[start+1:], "<"+name)
			if next < 0 {
				start = -1
			} else {
				start = start + 1 + next
			}
		}
		if start == -1 {
			continue
		}
		open := strings.IndexByte(html[start:], '>')
		closeIdx := strings.LastIndex(lower, "</"+name)
		if open != -1 && closeIdx > start+open {
			return html[start+open+1 : closeIdx]
		}
	}
	return html
}

// collapseSpaces is .replace(/\s+/g, " ").
func collapseSpaces(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if jsstr.IsSpace(r) {
			if !in {
				b.WriteByte(' ')
			}
			in = true
			continue
		}
		in = false
		b.WriteRune(r)
	}
	return b.String()
}

// extractTitle is extractTitle: the decoded, whitespace-collapsed text of the first
// <title> element, and false when there is none or it is empty.
func extractTitle(html string) (string, bool) {
	lower := asciiLower(html)
	start := strings.Index(lower, "<title")
	if start == -1 {
		return "", false
	}
	open := strings.IndexByte(html[start:], '>')
	if open == -1 {
		return "", false
	}
	open += start
	closeIdx := strings.Index(lower[open:], "</title")
	if closeIdx == -1 {
		return "", false
	}
	closeIdx += open
	title := jsstr.Trim(collapseSpaces(decodeEntities(html[open+1 : closeIdx])))
	return title, title != ""
}

// htmlToText turns HTML into readable text: dropped elements and comments removed, tags
// stripped with line breaks for block tags, entities decoded, every line collapsed and
// trimmed, and a blank line kept only after a non-blank one.
//
// The lowercase copy used to find tags is ASCII-only (asciiLower), so its offsets stay
// aligned with the original. A full Unicode lowercase turns U+0130 into two UTF-16 units,
// so offsets found in the lowered copy would be off by one after such a character (keeping
// <script> bodies and cutting mainContent in the wrong place); the ASCII copy strips and
// cuts correctly.
func htmlToText(html string) string {
	text := decodeEntities(stripTags(dropElements(html)))
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		var b strings.Builder
		in := false
		for _, r := range l {
			if r == ' ' || r == '\t' || r == '\f' || r == '\v' || r == '\r' {
				if !in {
					b.WriteByte(' ')
				}
				in = true
				continue
			}
			in = false
			b.WriteRune(r)
		}
		lines[i] = jsstr.Trim(b.String())
	}
	kept := make([]string, 0, len(lines))
	for i, l := range lines {
		if l != "" || (i > 0 && lines[i-1] != "") {
			kept = append(kept, l)
		}
	}
	return jsstr.Trim(strings.Join(kept, "\n"))
}

// truncate cuts text to maxChars UTF-16 units and marks the cut so the model knows content
// is missing. The count in the marker is the nominal text.length - maxChars (in UTF-16
// units). A cut in the middle of a surrogate pair drops the pair (jsstr.TruncateUTF16).
func truncate(text string, maxChars int) (string, bool) {
	n := kernel.UTF16Len(text)
	if n <= maxChars {
		return text, false
	}
	return jsstr.TruncateUTF16(text, maxChars) + "\n" + ellipsis + "[truncated " + strconv.Itoa(n-maxChars) + " chars]", true
}
