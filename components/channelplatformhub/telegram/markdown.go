package telegram

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// The agent's Markdown -> Telegram HTML (parse_mode "HTML"), A-100 phase 2, the
// markdownToTelegramHtml of core/ts.
//
// Telegram's HTML subset is small (b, i, s, u, code, pre, a, blockquote) and strict: an
// unknown tag or a stray "<" fails the whole message. So every piece of text is escaped
// first and only these tags are produced. Anything that is not understood stays as text.
// Lists, tables and rules are kept as text (bullets become a bullet sign); headings
// become bold lines.
//
// The adapter sends plain text instead when Telegram still rejects the result.
//
// Go's regexp has no lookaround or backreferences, so the italic and rule rules of
// core/ts are matched by hand below; the results are the same.
// ---------------------------------------------------------------------------

var (
	mdFence   = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([\\w+#.-]*)\\s*$")
	mdQuote   = regexp.MustCompile(`^\s*>`)
	mdQuoteRm = regexp.MustCompile(`^\s*>\s?`)
	mdHeading = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	mdBullet  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdCode    = regexp.MustCompile("`+[^`]*?`+")
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+|tg://[^\s)]+|mailto:[^\s)]+)\)`)
	mdBold    = regexp.MustCompile(`\*\*(\S(?:[\s\S]*?\S)??)\*\*`)
	mdBold2   = regexp.MustCompile(`__(\S(?:[\s\S]*?\S)??)__`)
	mdStrike  = regexp.MustCompile(`~~(\S(?:[\s\S]*?\S)??)~~`)
	mdLinkRef = regexp.MustCompile("\x00(\\d+)\x00")
)

// The signs used in place of Markdown list bullets and rules, built from rune values so
// this source stays ASCII-only.
var (
	mdBulletSign = string(rune(0x2022))
	mdRule       = strings.Repeat(string(rune(0x2500)), 10)
)

// MarkdownToHTML renders the agent's Markdown as Telegram's HTML subset.
func MarkdownToHTML(markdown string) string {
	var out []string
	text := strings.ReplaceAll(strings.ReplaceAll(markdown, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); {
		line := lines[i]

		// Fenced code block.
		if m := mdFence.FindStringSubmatch(line); m != nil {
			marker, lang := m[1], m[2]
			var body []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), marker) {
				body = append(body, lines[i])
				i++
			}
			i++ // the closing fence (or past the end)
			code := escapeHTML(strings.Join(body, "\n"))
			if lang != "" {
				out = append(out, `<pre><code class="language-`+escapeHTML(lang)+`">`+code+`</code></pre>`)
			} else {
				out = append(out, "<pre>"+code+"</pre>")
			}
			continue
		}

		// Block quote: consecutive "> " lines.
		if mdQuote.MatchString(line) {
			var body []string
			for i < len(lines) && mdQuote.MatchString(lines[i]) {
				body = append(body, inline(mdQuoteRm.ReplaceAllString(lines[i], "")))
				i++
			}
			out = append(out, "<blockquote>"+strings.Join(body, "\n")+"</blockquote>")
			continue
		}

		switch {
		case mdHeading.MatchString(line):
			out = append(out, "<b>"+inline(mdHeading.FindStringSubmatch(line)[1])+"</b>")
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

// isRule is /^\s{0,3}([-*_])(\s*\1){2,}\s*$/: three or more of one of - * _, with
// spaces between them.
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

// inline renders inline Markdown in one line: code spans first (their content is
// literal), then links and emphasis.
func inline(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range mdCode.FindAllStringIndex(text, -1) {
		b.WriteString(emphasis(text[last:loc[0]]))
		part := text[loc[0]:loc[1]]
		code := strings.Trim(part, "`")
		if code != "" {
			b.WriteString("<code>" + escapeHTML(code) + "</code>")
		} else {
			b.WriteString(escapeHTML(part))
		}
		last = loc[1]
	}
	b.WriteString(emphasis(text[last:]))
	return b.String()
}

func emphasis(raw string) string {
	// Links are taken out before escaping (the URL must not be touched by emphasis rules).
	var links []string
	text := mdLink.ReplaceAllStringFunc(raw, func(m string) string {
		sub := mdLink.FindStringSubmatch(m)
		links = append(links, `<a href="`+escapeHTML(sub[2])+`">`+emphasis(sub[1])+`</a>`)
		return "\x00" + strconv.Itoa(len(links)-1) + "\x00"
	})
	text = escapeHTML(text)
	text = mdBold.ReplaceAllString(text, "<b>$1</b>")
	text = mdBold2.ReplaceAllString(text, "<b>$1</b>")
	text = mdStrike.ReplaceAllString(text, "<s>$1</s>")
	text = italic(text, '*')
	text = italic(text, '_')
	return mdLinkRef.ReplaceAllStringFunc(text, func(m string) string {
		n, _ := strconv.Atoi(strings.Trim(m, "\x00"))
		if n < len(links) {
			return links[n]
		}
		return ""
	})
}

// italic wraps marker-delimited spans in <i>: the opening marker follows the start or a
// character that is not a word character (nor "*" for "*"), the span has no marker,
// starts and ends with a non-space, and the closing marker is not followed by a word
// character. It is /(^|[^\w*])\*(?=\S)([^*]*?\S)\*(?!\w)/ (and the "_" one) of core/ts.
func italic(text string, marker rune) string {
	rs := []rune(text)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] != marker || (i > 0 && (isWord(rs[i-1]) || (marker == '*' && rs[i-1] == '*'))) {
			b.WriteRune(rs[i])
			continue
		}
		j := i + 1
		for j < len(rs) && rs[j] != marker {
			j++
		}
		if j >= len(rs) || j == i+1 || unicode.IsSpace(rs[i+1]) || unicode.IsSpace(rs[j-1]) || (j+1 < len(rs) && isWord(rs[j+1])) {
			b.WriteRune(rs[i])
			continue
		}
		b.WriteString("<i>" + string(rs[i+1:j]) + "</i>")
		i = j
	}
	return b.String()
}

func isWord(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func escapeHTML(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(text)
}
