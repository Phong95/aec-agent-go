package slack

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// The agent's Markdown -> Slack mrkdwn (A-100 phase 3), the markdownToSlackMrkdwn of
// core/ts.
//
// Slack's mrkdwn differs from Markdown: *bold*, _italic_, ~strike~, <url|label> links,
// no headings. "&", "<" and ">" must be escaped everywhere except in the link syntax
// produced here. Code spans and blocks keep their backticks; their content is only
// escaped.
//
// Go's regexp has no lookaround, so the italic and rule rules of core/ts are matched
// by hand below; the results are the same.
// ---------------------------------------------------------------------------

var (
	mdFence   = regexp.MustCompile("^\\s*(```+|~~~+)\\s*[\\w+#.-]*\\s*$")
	mdQuote   = regexp.MustCompile(`^\s*>`)
	mdQuoteRm = regexp.MustCompile(`^\s*>\s?`)
	mdHeading = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	mdBullet  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdCode    = regexp.MustCompile("`+[^`]*?`+")
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+|mailto:[^\s)]+)\)`)
	mdBold    = regexp.MustCompile(`\*\*(\S(?:[\s\S]*?\S)??)\*\*`)
	mdBold2   = regexp.MustCompile(`__(\S(?:[\s\S]*?\S)??)__`)
	mdStrike  = regexp.MustCompile(`~~(\S(?:[\s\S]*?\S)??)~~`)
	mdLinkRef = regexp.MustCompile("\x00(\\d+)\x00")
)

// The signs used in place of Markdown bullets, rules and "|" in a link label, built from
// rune values so this source stays ASCII-only.
var (
	mdBulletSign = string(rune(0x2022))
	mdRule       = strings.Repeat(string(rune(0x2500)), 10)
	brokenBar    = string(rune(0x00a6))
)

// MarkdownToMrkdwn renders the agent's Markdown as Slack mrkdwn.
func MarkdownToMrkdwn(markdown string) string {
	var out []string
	text := strings.ReplaceAll(strings.ReplaceAll(markdown, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); {
		line := lines[i]
		if m := mdFence.FindStringSubmatch(line); m != nil {
			marker := m[1]
			var body []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), marker) {
				body = append(body, lines[i])
				i++
			}
			i++
			out = append(out, "```\n"+EscapeMrkdwn(strings.Join(body, "\n"))+"\n```")
			continue
		}
		switch {
		case mdHeading.MatchString(line):
			out = append(out, "*"+strings.ReplaceAll(inline(mdHeading.FindStringSubmatch(line)[1]), "*", "")+"*")
		case mdQuote.MatchString(line):
			out = append(out, ">"+inline(mdQuoteRm.ReplaceAllString(line, " ")))
		case isRule(line):
			out = append(out, mdRule)
		default:
			if m := mdBullet.FindStringSubmatch(line); m != nil {
				out = append(out, m[1]+mdBulletSign+" "+inline(m[2]))
			} else {
				out = append(out, inline(line))
			}
		}
		i++
	}
	return strings.Join(out, "\n")
}

// EscapeMrkdwn escapes the three characters Slack reads as markup.
func EscapeMrkdwn(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// isRule is /^\s{0,3}([-*_])(\s*\1){2,}\s*$/.
func isRule(line string) bool {
	lead := len(line) - len(strings.TrimLeft(line, " \t"))
	if lead > 3 {
		return false
	}
	var mark rune
	count := 0
	for _, r := range strings.TrimSpace(line) {
		switch {
		case r == ' ' || r == '\t':
		case (r == '-' || r == '*' || r == '_') && (mark == 0 || r == mark):
			mark = r
			count++
		default:
			return false
		}
	}
	return count >= 3
}

// inline converts inline Markdown in one line: code spans are kept literal, the rest is
// converted.
func inline(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range mdCode.FindAllStringIndex(text, -1) {
		b.WriteString(emphasis(text[last:loc[0]]))
		b.WriteString(EscapeMrkdwn(text[loc[0]:loc[1]]))
		last = loc[1]
	}
	b.WriteString(emphasis(text[last:]))
	return b.String()
}

var linkURLEscaper = strings.NewReplacer("<", "%3C", ">", "%3E", "|", "%7C")

func emphasis(raw string) string {
	var links []string
	text := mdLink.ReplaceAllStringFunc(raw, func(m string) string {
		sub := mdLink.FindStringSubmatch(m)
		label := strings.ReplaceAll(EscapeMrkdwn(sub[1]), "|", brokenBar)
		links = append(links, "<"+linkURLEscaper.Replace(sub[2])+"|"+label+">")
		return "\x00" + strconv.Itoa(len(links)-1) + "\x00"
	})
	text = EscapeMrkdwn(text)
	// Markers for bold/strike, so the single-"*" italic rule below cannot see them.
	text = mdBold.ReplaceAllString(text, "\x01$1\x01")
	text = mdBold2.ReplaceAllString(text, "\x01$1\x01")
	text = mdStrike.ReplaceAllString(text, "\x02$1\x02")
	text = italicStar(text)
	text = strings.NewReplacer("\x01", "*", "\x02", "~").Replace(text)
	return mdLinkRef.ReplaceAllStringFunc(text, func(m string) string {
		n, _ := strconv.Atoi(strings.Trim(m, "\x00"))
		if n < len(links) {
			return links[n]
		}
		return ""
	})
}

// italicStar turns *x* into _x_: /(^|[^\w*])\*(?=\S)([^*]*?\S)\*(?!\w)/ of core/ts.
func italicStar(text string) string {
	rs := []rune(text)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] != '*' || (i > 0 && (isWord(rs[i-1]) || rs[i-1] == '*')) {
			b.WriteRune(rs[i])
			continue
		}
		j := i + 1
		for j < len(rs) && rs[j] != '*' {
			j++
		}
		if j >= len(rs) || j == i+1 || unicode.IsSpace(rs[i+1]) || unicode.IsSpace(rs[j-1]) || (j+1 < len(rs) && isWord(rs[j+1])) {
			b.WriteRune(rs[i])
			continue
		}
		b.WriteString("_" + string(rs[i+1:j]) + "_")
		i = j
	}
	return b.String()
}

func isWord(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
