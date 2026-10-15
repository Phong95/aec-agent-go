package viewkit

import (
	"fmt"
	"regexp"
	"strings"
)

// CompileSearchRegex compiles a model-supplied search pattern with Go's RE2 engine
// after rejecting the constructs whose meaning differs from the JavaScript RegExp
// the JS-based sandboxes and the Docker helper use. RE2 runs in linear time, so a ReDoS
// pattern cannot stall a search (the JS engine would); the price is that RE2 cannot
// do lookaround or backreferences, which fail to compile with Go's own message.
//
// A pattern that JavaScript rejects must never be accepted and a pattern that is
// accepted must not match different text than the JS engine, so the pattern is
// scanned first (outside escaped positions) and these are rejected with an error
// naming the construct, although RE2 would accept them:
//
//   - inline-flag groups "(?i)", "(?s:", "(?-i)" (JS: invalid group) and the
//     Python-style "(?P<name>" (JS: invalid group)
//   - "\Q...\E" (JS: the letters Q and E), "\p" and "\P" (JS: the letters p and
//     P), "\z" and "\A" (JS: the letters z and A), "\a" (RE2: the bell character,
//     JS: the letter a) and "\x{...}" (RE2: a code point, JS: x repeated)
//   - the POSIX class "[[:alpha:]]" (JS: a set of characters)
//   - an empty class "[]" or "[^]" (JS: matches nothing, or any character; RE2
//     reads the "]" as a member of the class)
//   - a named group whose name starts with a digit or repeats an earlier name (JS:
//     invalid group name)
//   - a quantifier right after "^", "$", "\b" or "\B" (JS: nothing to repeat;
//     RE2 accepts it)
//
// What stays different and is documented: RE2's "\s" is ASCII only and
// JS's includes U+00A0 and U+FEFF; "." matches "\r" and U+2028 in RE2 and not in
// JS; case folding is the simple Unicode folding (the Kelvin sign U+212A matches
// "k" with ignoreCase, which JS does not); the engine works on characters, not
// UTF-16 units, so an astral character is one character here and two in JS.
//
// With ignoreCase the pattern is compiled with a "(?i)" prefix (the JS "i" flag).
// The error of a pattern that Go cannot compile is Go's text unchanged.
func CompileSearchRegex(pattern string, ignoreCase bool) (*regexp.Regexp, error) {
	if err := scanDivergentConstructs(pattern); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if !ignoreCase {
		return re, nil
	}
	return regexp.Compile("(?i)" + pattern)
}

func divergent(construct, why string) error {
	return fmt.Errorf("unsupported construct %s: %s", construct, why)
}

// scanDivergentConstructs walks the pattern once, tracking whether it is inside a
// character class, and returns an error for the first construct of the list
// documented at CompileSearchRegex.
func scanDivergentConstructs(p string) error {
	inClass := false
	names := map[string]bool{}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' {
			if i+1 >= len(p) {
				return nil // a trailing backslash is Go's compile error
			}
			next := p[i+1]
			switch next {
			case 'Q':
				return divergent(`\Q`, `literal-text escape; in JavaScript it is the letter Q`)
			case 'p', 'P':
				return divergent(`\`+string(next), `Unicode class escape; in JavaScript it is the letter `+string(next))
			case 'z', 'A':
				return divergent(`\`+string(next), `text anchor escape; in JavaScript it is the letter `+string(next))
			case 'a':
				return divergent(`\a`, `bell escape; in JavaScript it is the letter a`)
			case 'x':
				if i+2 < len(p) && p[i+2] == '{' {
					return divergent(`\x{`, `wide hex escape; in JavaScript it is x followed by a quantifier`)
				}
			case 'b', 'B':
				if !inClass && startsQuantifier(p[i+2:]) {
					return divergent(`\`+string(next)+` followed by a quantifier`, `nothing to repeat in JavaScript`)
				}
			}
			i++
			continue
		}
		if inClass {
			switch {
			case c == '[' && i+1 < len(p) && p[i+1] == ':':
				return divergent(`[[:`, `POSIX class; in JavaScript a plain set of characters`)
			case c == ']':
				inClass = false
			}
			continue
		}
		switch c {
		case '[':
			inClass = true
			j := i + 1
			if j < len(p) && p[j] == '^' {
				j++
			}
			if j < len(p) && p[j] == ']' {
				return divergent(`an empty character class`, `JavaScript reads "[]" and "[^]" as a class of its own, RE2 as a class with a member "]"`)
			}
		case '(':
			if i+2 < len(p) && p[i+1] == '?' {
				switch d := p[i+2]; {
				case d == 'P':
					return divergent(`(?P`, `Python-style group; JavaScript rejects it`)
				case d == 'i' || d == 'm' || d == 's' || d == 'U' || d == '-':
					return divergent(`(?`+string(d), `inline flags; JavaScript rejects them`)
				case d == '<' && i+3 < len(p) && p[i+3] != '=' && p[i+3] != '!':
					if err := checkGroupName(p[i+3:], names); err != nil {
						return err
					}
				}
			}
		case '^', '$':
			if startsQuantifier(p[i+1:]) {
				return divergent(`"`+string(c)+`" followed by a quantifier`, `nothing to repeat in JavaScript`)
			}
		}
	}
	return nil
}

// checkGroupName validates the name of a named group, rest being the text after
// "(?<": RE2 accepts a name that starts with a digit and the same name twice, JS
// rejects both. A name that is not closed or not made of word characters is left to
// the compiler.
func checkGroupName(rest string, seen map[string]bool) error {
	end := strings.IndexByte(rest, '>')
	if end <= 0 {
		return nil
	}
	name := rest[:end]
	if isDigit(name[0]) {
		return divergent("(?<"+name+">", "a group name cannot start with a digit in JavaScript")
	}
	if seen[name] {
		return divergent("(?<"+name+">", "duplicate group name; JavaScript rejects it")
	}
	seen[name] = true
	return nil
}

// quantifierBrace is a brace quantifier at the start of s: {n}, {n,} or {n,m}.
var quantifierBrace = regexp.MustCompile(`^\{[0-9]+(,[0-9]*)?\}`)

// startsQuantifier reports whether s starts with *, + or ? or a brace quantifier.
func startsQuantifier(s string) bool {
	if s == "" {
		return false
	}
	if strings.ContainsRune("*+?", rune(s[0])) {
		return true
	}
	return quantifierBrace.MatchString(s)
}
