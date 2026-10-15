package skill

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Frontmatter
//
// Everything below is a hand-written parser, not a YAML library, and every
// pattern that uses JS \s or "." semantics is a hand-written scanner over jsstr:
// RE2 and strings.TrimSpace disagree with JS on whitespace (U+FEFF, U+0085) and
// on line terminators (U+2028, U+2029, CR). The rejections of the parser are part
// of its contract.
// ---------------------------------------------------------------------------

// maxYAMLDepth is the deepest map nesting parseYAMLSubset accepts. It is an
// intentional limit: a model can author arbitrarily deep input, so the parser
// stops with an error ("line N: nesting is deeper than 100 levels") instead of
// recursing without bound.
const maxYAMLDepth = 100

// splitFrontmatter splits a SKILL.md into its frontmatter and body. It is the
// regular expression /^---[ \t]*\n([\s\S]*?)\n---[ \t]*(?:\n|$)/ applied after
// stripping one leading U+FEFF: the first line is "---" plus spaces or tabs, then
// the earliest later "\n---" plus spaces or tabs that is followed by LF or the
// end closes the block. The search starts after the first newline, so
// "---\n---\nbody" does not split while "---\n\n---\nbody" does (an empty
// frontmatter). ok is false when there is no frontmatter.
func splitFrontmatter(content string) (frontmatter, body string, ok bool) {
	text := strings.TrimPrefix(content, "\U0000feff")
	if !strings.HasPrefix(text, "---") {
		return "", "", false
	}
	i := 3
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	if i >= len(text) || text[i] != '\n' {
		return "", "", false
	}
	start := i + 1
	for q := start; q < len(text); q++ {
		if text[q] != '\n' || !strings.HasPrefix(text[q+1:], "---") {
			continue
		}
		j := q + 4
		for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
			j++
		}
		switch {
		case j == len(text):
			return text[start:q], "", true
		case text[j] == '\n':
			return text[start:q], text[j+1:], true
		}
	}
	return "", "", false
}

// yamlMap is a parsed YAML map. own holds the entries in JS object key order
// (integer-like keys first, ascending, then the others in the order written). A
// "__proto__" key is special: JS assigns it through the Object.prototype setter,
// so a string value is dropped and a map or list value becomes the hidden
// prototype of the map, which the top-level lookups of ParsePackage read
// through. proto is that map; a list prototype supplies nothing, so it is nil.
type yamlMap struct {
	own   ordered.Object
	proto *yamlMap
}

// lookup is raw[key] in JS: the own entry, else the one of the prototype chain.
func (m *yamlMap) lookup(key string) (any, bool) {
	for ; m != nil; m = m.proto {
		if v, ok := m.own.Get(key); ok {
			return v, true
		}
	}
	return nil, false
}

// yamlLine is one source line: its indentation (the count of leading JS
// whitespace characters), its text with trailing whitespace removed, and its
// 1-based number.
type yamlLine struct {
	indent int
	text   string
	no     int
}

// parseYAMLSubset parses the YAML subset SKILL.md frontmatter uses. It returns an
// error on structures it cannot read. Scalars stay strings (no number/boolean
// coercion - every spec field is text); the values are string, []any (of string
// and []any) and ordered.Object. A hidden "__proto__" prototype (see yamlMap) is not
// part of the result; parseYAMLDoc keeps it for the package lookups.
func parseYAMLSubset(source string) (ordered.Object, error) {
	m, err := parseYAMLDoc(source)
	if err != nil {
		return ordered.Object{}, err
	}
	return m.own, nil
}

// parseYAMLDoc is parseYAMLSubset returning the whole map, prototype included.
func parseYAMLDoc(source string) (*yamlMap, error) {
	raw := strings.Split(source, "\n")
	p := &yamlParser{lines: make([]yamlLine, len(raw))}
	for i, r := range raw {
		p.lines[i] = yamlLine{indent: jsstr.CountLeading(r), text: jsstr.TrimEnd(r), no: i + 1}
	}
	result, err := p.parseMap(0, 1)
	if err != nil {
		return nil, err
	}
	p.skipBlank()
	if p.i < len(p.lines) {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.lines[p.i].no)
	}
	return result, nil
}

// yamlParser is the cursor over the lines.
type yamlParser struct {
	lines []yamlLine
	i     int
}

func isBlank(line yamlLine) bool {
	t := jsstr.Trim(line.text)
	return t == "" || strings.HasPrefix(t, "#")
}

func (p *yamlParser) skipBlank() {
	for p.i < len(p.lines) && isBlank(p.lines[p.i]) {
		p.i++
	}
}

// currentNo is the number of the line under the cursor, or of the last line.
func (p *yamlParser) currentNo() int {
	if p.i < len(p.lines) {
		return p.lines[p.i].no
	}
	return len(p.lines)
}

// parseKeyLine is the key-line expression /^([^\s:#][^:]*?):(?:\s+(.*))?$/
// applied to a trimmed line. The key is the text before the FIRST colon (at
// index 1 or more; the first character is not whitespace, ":" or "#"). After the
// colon the line ends, or one or more whitespace characters follow and then a
// rest that holds no line terminator ("." does not match LF, CR, U+2028,
// U+2029). So "a:b", "k:v: w", ": nokey" and "name: a<U+2028>b" all fail.
func parseKeyLine(t string) (key, inline string, ok bool) {
	if t == "" {
		return "", "", false
	}
	first, size := utf8.DecodeRuneInString(t)
	if jsstr.IsSpace(first) || first == ':' || first == '#' {
		return "", "", false
	}
	colon := strings.IndexByte(t[size:], ':')
	if colon < 0 {
		return "", "", false
	}
	colon += size
	key, rest := t[:colon], t[colon+1:]
	if rest == "" {
		return key, "", true
	}
	if jsstr.CountLeading(rest) == 0 {
		return "", "", false
	}
	tail := jsstr.TrimStart(rest)
	if jsstr.HasLineTerminator(tail) {
		return "", "", false
	}
	return key, tail, true
}

func (p *yamlParser) parseMap(indent, depth int) (*yamlMap, error) {
	if depth > maxYAMLDepth {
		return nil, fmt.Errorf("line %d: nesting is deeper than %d levels", p.currentNo(), maxYAMLDepth)
	}
	m := &yamlMap{}
	for {
		p.skipBlank()
		if p.i >= len(p.lines) || p.lines[p.i].indent < indent {
			m.finalize()
			return m, nil
		}
		line := p.lines[p.i]
		if line.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", line.no)
		}
		keyText, inline, ok := parseKeyLine(jsstr.Trim(line.text))
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", line.no)
		}
		key := jsstr.Trim(keyText)
		inline = jsstr.Trim(inline)
		p.i++
		value, err := p.parseValue(indent, inline, line.no, depth)
		if err != nil {
			return nil, err
		}
		key = unquote(key)
		if key == "__proto__" {
			// A "__proto__" key behaves like the JS Object.prototype.__proto__
			// setter. A string is ignored, a map becomes the prototype (later reads
			// such as raw["name"] fall through to it) and a list replaces the
			// prototype with one that supplies nothing.
			switch x := value.(type) {
			case *yamlMap:
				m.proto = x
			case []any:
				m.proto = nil
			}
			continue
		}
		if child, isMap := value.(*yamlMap); isMap {
			m.own.Set(key, child.own)
		} else {
			m.own.Set(key, value)
		}
	}
}

// finalize puts the entries in JS object key order.
func (m *yamlMap) finalize() {
	keys := m.own.Keys()
	inOrder := jsKeyOrder(keys)
	if slices.Equal(keys, inOrder) {
		return
	}
	next := ordered.NewObject()
	for _, k := range inOrder {
		v, _ := m.own.Get(k)
		next.Set(k, v)
	}
	m.own = next
}

// isArrayIndex reports whether k is a canonical JS array index: "0", or a digit
// string without a leading zero whose value is below 2^32-1. JS lists such keys
// of a plain object first, in ascending numeric order.
func isArrayIndex(k string) bool {
	if k == "" || len(k) > 10 {
		return false
	}
	if k == "0" {
		return true
	}
	if k[0] == '0' {
		return false
	}
	var n uint64
	for i := range len(k) {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
		n = n*10 + uint64(k[i]-'0')
	}
	return n < 4294967295
}

// jsKeyOrder returns keys (which are distinct and in insertion order) in the
// order Object.keys gives them: integer-like keys ascending, then the others in
// insertion order.
func jsKeyOrder(keys []string) []string {
	var index, rest []string
	for _, k := range keys {
		if isArrayIndex(k) {
			index = append(index, k)
		} else {
			rest = append(rest, k)
		}
	}
	if len(index) == 0 {
		return keys
	}
	// Digit strings without leading zeros compare numerically by length, then text.
	slices.SortFunc(index, func(a, b string) int {
		if c := cmp.Compare(len(a), len(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return append(index, rest...)
}

// parseValue reads the value of a "key:" whose inline text is inline. It returns
// a string, a []any (strings and nested []any) or a *yamlMap.
func (p *yamlParser) parseValue(indent int, inline string, lineNo, depth int) (any, error) {
	if folded, ok := blockHeader(inline); ok {
		return p.parseBlockScalar(indent, folded), nil
	}

	if inline == "" || strings.HasPrefix(inline, "#") {
		p.skipBlank()
		if p.i >= len(p.lines) || p.lines[p.i].indent <= indent {
			return "", nil
		}
		next := p.lines[p.i]
		if t := jsstr.Trim(next.text); strings.HasPrefix(t, "- ") || t == "-" {
			return p.parseList(next.indent, depth)
		}
		return p.parseMap(next.indent, depth+1)
	}

	if strings.HasPrefix(inline, "[") {
		if !strings.HasSuffix(inline, "]") {
			return nil, fmt.Errorf("line %d: unterminated [list]", lineNo)
		}
		inner := jsstr.Trim(inline[1 : len(inline)-1])
		items := []any{}
		if inner != "" {
			for _, item := range splitFlow(inner) {
				items = append(items, unquote(jsstr.Trim(item)))
			}
		}
		return items, nil
	}
	if strings.HasPrefix(inline, "{") {
		return nil, fmt.Errorf("line %d: {inline maps} are not supported \U00002014 use an indented map", lineNo)
	}

	if strings.HasPrefix(inline, "\"") || strings.HasPrefix(inline, "'") {
		return unquote(stripComment(inline)), nil
	}

	// Plain scalar, possibly continued on more-indented lines (folded with spaces).
	parts := []string{stripComment(inline)}
	for p.i < len(p.lines) {
		next := p.lines[p.i]
		if jsstr.Trim(next.text) == "" {
			break
		}
		if next.indent <= indent {
			break
		}
		parts = append(parts, jsstr.Trim(next.text))
		p.i++
	}
	return strings.Join(parts, " "), nil
}

// blockHeader is the expression /^([|>])([+-]?)(\d*)[+-]?(?:\s+#.*)?$/: a
// block scalar header ("|" literal or ">" folded), an optional chomping sign, an
// optional indentation digit run and another optional sign (all accepted and
// ignored), then optionally whitespace and a comment with no line terminator.
func blockHeader(inline string) (folded, ok bool) {
	if inline == "" || (inline[0] != '|' && inline[0] != '>') {
		return false, false
	}
	folded = inline[0] == '>'
	i := 1
	if i < len(inline) && (inline[i] == '+' || inline[i] == '-') {
		i++
	}
	for i < len(inline) && inline[i] >= '0' && inline[i] <= '9' {
		i++
	}
	if i < len(inline) && (inline[i] == '+' || inline[i] == '-') {
		i++
	}
	rest := inline[i:]
	if rest == "" {
		return folded, true
	}
	if jsstr.CountLeading(rest) == 0 {
		return false, false
	}
	comment := jsstr.TrimStart(rest)
	if comment == "" || comment[0] != '#' || jsstr.HasLineTerminator(comment[1:]) {
		return false, false
	}
	return folded, true
}

func (p *yamlParser) parseBlockScalar(indent int, folded bool) string {
	var collected []yamlLine
	for p.i < len(p.lines) {
		line := p.lines[p.i]
		if jsstr.Trim(line.text) != "" && line.indent <= indent {
			break
		}
		collected = append(collected, line)
		p.i++
	}
	for len(collected) > 0 && jsstr.Trim(collected[len(collected)-1].text) == "" {
		collected = collected[:len(collected)-1]
	}
	contentIndent := 0
	first := true
	for _, l := range collected {
		if jsstr.Trim(l.text) != "" && (first || l.indent < contentIndent) {
			contentIndent = l.indent
			first = false
		}
	}
	rows := make([]string, len(collected))
	for i, l := range collected {
		if jsstr.Trim(l.text) == "" {
			rows[i] = ""
		} else {
			rows[i] = jsstr.DropRunes(l.text, contentIndent)
		}
	}
	if !folded {
		return strings.Join(rows, "\n")
	}
	// Folded: single newlines become spaces, blank lines become newlines.
	var out strings.Builder
	last := ""
	for _, row := range rows {
		switch {
		case row == "":
			out.WriteString("\n")
			last = "\n"
		case last == "" || strings.HasSuffix(last, "\n"):
			out.WriteString(row)
			last = row
		default:
			out.WriteString(" " + row)
			last = row
		}
	}
	return out.String()
}

func (p *yamlParser) parseList(indent, depth int) ([]any, error) {
	out := []any{}
	for {
		p.skipBlank()
		if p.i >= len(p.lines) || p.lines[p.i].indent < indent {
			return out, nil
		}
		line := p.lines[p.i]
		text := jsstr.Trim(line.text)
		if line.indent > indent || (!strings.HasPrefix(text, "- ") && text != "-") {
			return nil, fmt.Errorf("line %d: expected \"- item\"", line.no)
		}
		item := jsstr.Trim(text[1:])
		if looksLikeMapEntry(item) {
			return nil, fmt.Errorf("line %d: lists of maps are not supported", line.no)
		}
		p.i++
		if strings.HasPrefix(item, "[") {
			v, err := p.parseValue(indent, item, line.no, depth)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		} else {
			out = append(out, unquote(stripComment(item)))
		}
	}
}

// looksLikeMapEntry is the expression /^[^\s"'][^:]*:(\s|$)/ on a list item:
// the first character is not whitespace or a quote, and the first colon at index
// 1 or more is followed by whitespace or the end of the item.
func looksLikeMapEntry(item string) bool {
	if item == "" {
		return false
	}
	first, size := utf8.DecodeRuneInString(item)
	if jsstr.IsSpace(first) || first == '"' || first == '\'' {
		return false
	}
	colon := strings.IndexByte(item[size:], ':')
	if colon < 0 {
		return false
	}
	after := item[size+colon+1:]
	if after == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(after)
	return jsstr.IsSpace(r)
}

// splitFlow splits the inside of a [flow list] on commas that are outside
// quotes. A quote opens at " or ' and closes at the same character; there are no
// escapes here.
func splitFlow(inner string) []string {
	items := []string{}
	start := 0
	var quote byte
	for i := range len(inner) {
		ch := inner[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '"' || ch == '\'':
			quote = ch
		case ch == ',':
			items = append(items, inner[start:i])
			start = i + 1
		}
	}
	if current := inner[start:]; jsstr.Trim(current) != "" {
		items = append(items, current)
	}
	return items
}

// stripComment cuts a trailing "# comment" from a scalar. A quoted value keeps
// everything up to its closing quote (a backslash escapes in double quotes, a
// doubled quote in single quotes). For a plain value the comment starts at the
// first whitespace character followed by "#", so "plain#nocomment" is kept.
func stripComment(value string) string {
	if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
		quote := value[0]
		i := 1
		for ; i < len(value); i++ {
			if quote == '"' && value[i] == '\\' {
				i++
			} else if value[i] == quote {
				if quote == '\'' && i+1 < len(value) && value[i+1] == '\'' {
					i++
				} else {
					break
				}
			}
		}
		return value[:min(i+1, len(value))]
	}
	for i, r := range value {
		if jsstr.IsSpace(r) && i+utf8.RuneLen(r) < len(value) && value[i+utf8.RuneLen(r)] == '#' {
			return jsstr.Trim(value[:i])
		}
	}
	return jsstr.Trim(value)
}

// unquote removes the quotes of a quoted scalar. In single quotes a doubled
// quote is one quote; in double quotes a backslash escape is \n, \t or the
// escaped character itself (the JS "." that follows the backslash does not match
// a line terminator, so a backslash before one stays).
func unquote(value string) string {
	v := jsstr.Trim(value)
	if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	if len(v) >= 2 && strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"") {
		inner := v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(inner); {
			r, size := utf8.DecodeRuneInString(inner[i:])
			if r == '\\' && i+size < len(inner) {
				c, csize := utf8.DecodeRuneInString(inner[i+size:])
				if !jsstr.IsLineTerminator(c) {
					switch c {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteString(inner[i+size : i+size+csize])
					}
					i += size + csize
					continue
				}
			}
			b.WriteString(inner[i : i+size])
			i += size
		}
		return b.String()
	}
	return v
}
