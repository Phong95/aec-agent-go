package web

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// This file is a small WHATWG-compatible parser, serializer and resolver for http and
// https URLs, matching the WHATWG URL class (new URL(raw, base).toString()). The SSRF guard
// depends on its canonical forms: net/url leaves 2130706433, 0x7f.1 and 1.2.3 as host names
// and prints IPv4-mapped IPv6 addresses in dotted form, so a guard built on it would check
// a different string than the one that is fetched. Only the special schemes http and https
// are handled; a syntactically valid scheme of any other kind is reported by schemeOf and
// refused by the guard with the scheme error. A host with a non-ASCII character is refused
// as an invalid URL: the Go floor (1.25) has no x/net/idna to convert it to Punycode.

// replacementChar is U+FFFD. It is written as bytes so that this source stays ASCII.
const replacementChar = "\xEF\xBF\xBD"

// errInvalidURL is what `new URL(x)` rejects.
var errInvalidURL = errors.New("invalid URL")

// webURL is the parsed form of a special (http or https) URL.
type webURL struct {
	Scheme      string // "http" or "https"
	Username    string
	Password    string
	Host        string // canonical: lowercase, IPv4 dotted decimal, IPv6 in brackets
	Port        string // "" when absent or the default
	Path        string // always starts with "/"
	Query       string
	HasQuery    bool
	Fragment    string
	HasFragment bool
}

// hostname is the host without the IPv6 brackets.
func (u *webURL) hostname() string {
	return strings.TrimSuffix(strings.TrimPrefix(u.Host, "["), "]")
}

// hasCredentials is the check `url.username || url.password`.
func (u *webURL) hasCredentials() bool { return u.Username != "" || u.Password != "" }

// String serializes like URL.toString().
func (u *webURL) String() string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.hasCredentials() {
		b.WriteString(u.Username)
		if u.Password != "" {
			b.WriteByte(':')
			b.WriteString(u.Password)
		}
		b.WriteByte('@')
	}
	b.WriteString(u.Host)
	if u.Port != "" {
		b.WriteByte(':')
		b.WriteString(u.Port)
	}
	b.WriteString(u.Path)
	if u.HasQuery {
		b.WriteByte('?')
		b.WriteString(u.Query)
	}
	if u.HasFragment {
		b.WriteByte('#')
		b.WriteString(u.Fragment)
	}
	return b.String()
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isSchemeChar(c byte) bool {
	return isASCIILetter(c) || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
}

// schemeOf returns the lowercase scheme of raw ("" when it has none) after the WHATWG
// pre-processing (strip C0 and space, drop tab and newline).
func schemeOf(raw string) string { return schemeOfPreprocessed(preprocess(raw)) }

func schemeOfPreprocessed(s string) string {
	if s == "" || !isASCIILetter(s[0]) {
		return ""
	}
	for i := 1; i < len(s); i++ {
		if s[i] == ':' {
			return strings.ToLower(s[:i])
		}
		if !isSchemeChar(s[i]) {
			return ""
		}
	}
	return ""
}

// preprocess is the URL parser's input clean-up: invalid UTF-8 becomes U+FFFD (a JS
// string cannot hold it), leading and trailing C0 controls and spaces go, and every tab,
// carriage return and line feed is removed.
func preprocess(s string) string {
	if !utf8.ValidString(s) {
		s = jsstr.DecodeUTF8([]byte(s))
	}
	start, end := 0, len(s)
	for start < end && s[start] <= 0x20 {
		start++
	}
	for end > start && s[end-1] <= 0x20 {
		end--
	}
	s = s[start:end]
	if strings.ContainsAny(s, "\t\n\r") {
		s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)
	}
	return s
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// parseURL parses an absolute http or https URL, or resolves raw against base when base
// is not nil. A URL whose scheme is neither http nor https is not handled here (callers
// use schemeOf first and report the scheme error); it is reported as errInvalidURL.
func parseURL(raw string, base *webURL) (*webURL, error) {
	s := preprocess(raw)
	scheme := schemeOfPreprocessed(s)
	u := &webURL{}
	switch {
	case scheme == "http" || scheme == "https":
		rest := s[len(scheme)+1:]
		if base != nil && base.Scheme == scheme && !strings.HasPrefix(rest, "//") {
			return resolveRelative(rest, base)
		}
		u.Scheme = scheme
		rest = strings.TrimLeft(rest, `/\`)
		return parseAuthorityAndRest(u, rest)
	case scheme != "":
		return nil, errInvalidURL
	case base == nil:
		return nil, errInvalidURL
	default:
		return resolveRelative(s, base)
	}
}

func startsWithSlash(s string) bool { return s != "" && (s[0] == '/' || s[0] == '\\') }

func resolveRelative(ref string, base *webURL) (*webURL, error) {
	u := &webURL{Scheme: base.Scheme}
	switch {
	case len(ref) >= 2 && startsWithSlash(ref) && startsWithSlash(ref[1:]):
		return parseAuthorityAndRest(u, strings.TrimLeft(ref, `/\`))
	case startsWithSlash(ref):
		u.copyAuthority(base)
		return finishPath(u, ref, "")
	}
	u.copyAuthority(base)
	switch {
	case ref == "":
		u.Path, u.Query, u.HasQuery = base.Path, base.Query, base.HasQuery
		return u, nil
	case ref[0] == '?':
		u.Path = base.Path
		return finishQueryFragment(u, ref)
	case ref[0] == '#':
		u.Path, u.Query, u.HasQuery = base.Path, base.Query, base.HasQuery
		return finishQueryFragment(u, ref)
	}
	dir := base.Path[:strings.LastIndexByte(base.Path, '/')+1]
	return finishPath(u, ref, dir)
}

func (u *webURL) copyAuthority(b *webURL) {
	u.Username, u.Password, u.Host, u.Port = b.Username, b.Password, b.Host, b.Port
}

func parseAuthorityAndRest(u *webURL, rest string) (*webURL, error) {
	end := strings.IndexAny(rest, `/\?#`)
	if end < 0 {
		end = len(rest)
	}
	auth, tail := rest[:end], rest[end:]
	if at := strings.LastIndexByte(auth, '@'); at >= 0 {
		userinfo := auth[:at]
		auth = auth[at+1:]
		user, pass, _ := strings.Cut(userinfo, ":")
		u.Username, u.Password = encode(user, userinfoSet), encode(pass, userinfoSet)
	}
	hostPart, portPart := auth, ""
	if strings.HasPrefix(auth, "[") {
		closeIdx := strings.IndexByte(auth, ']')
		if closeIdx < 0 {
			return nil, errInvalidURL
		}
		hostPart = auth[:closeIdx+1]
		portPart = auth[closeIdx+1:]
		if portPart != "" && portPart[0] != ':' {
			return nil, errInvalidURL
		}
		portPart = strings.TrimPrefix(portPart, ":")
	} else if i := strings.IndexByte(auth, ':'); i >= 0 {
		hostPart, portPart = auth[:i], auth[i+1:]
	}
	host, err := parseHost(hostPart)
	if err != nil {
		return nil, err
	}
	u.Host = host
	if portPart != "" {
		n := 0
		for i := range len(portPart) {
			if portPart[i] < '0' || portPart[i] > '9' {
				return nil, errInvalidURL
			}
			n = n*10 + int(portPart[i]-'0')
			if n > 65535 {
				return nil, errInvalidURL
			}
		}
		if p := strconv.Itoa(n); p != defaultPort(u.Scheme) {
			u.Port = p
		}
	}
	return finishPath(u, tail, "")
}

// finishPath parses "path?query#fragment" (the path may be empty) with a base directory
// for relative paths, applying dot-segment removal and the percent-encode sets.
func finishPath(u *webURL, tail, baseDir string) (*webURL, error) {
	pathEnd := strings.IndexAny(tail, "?#")
	pathPart, qf := tail, ""
	if pathEnd >= 0 {
		pathPart, qf = tail[:pathEnd], tail[pathEnd:]
	}
	u.Path = normalizePath(baseDir, pathPart)
	return finishQueryFragment(u, qf)
}

func finishQueryFragment(u *webURL, qf string) (*webURL, error) {
	if qf != "" && qf[0] == '?' {
		q := qf[1:]
		frag := ""
		hasFrag := false
		if i := strings.IndexByte(q, '#'); i >= 0 {
			q, frag, hasFrag = q[:i], q[i+1:], true
		}
		u.Query, u.HasQuery = encode(q, querySet), true
		if hasFrag {
			u.Fragment, u.HasFragment = encode(frag, fragmentSet), true
		}
		return u, nil
	}
	if qf != "" && qf[0] == '#' {
		u.Fragment, u.HasFragment = encode(qf[1:], fragmentSet), true
	}
	return u, nil
}

func isDoubleDot(seg string) bool {
	switch strings.ToLower(seg) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

func isSingleDot(seg string) bool {
	return seg == "." || strings.EqualFold(seg, "%2e")
}

func normalizePath(baseDir, p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	var segs []string
	if baseDir != "" {
		segs = strings.Split(strings.TrimSuffix(strings.TrimPrefix(baseDir, "/"), "/"), "/")
		if len(segs) == 1 && segs[0] == "" {
			segs = nil
		}
	} else {
		p = strings.TrimPrefix(p, "/")
	}
	parts := strings.Split(p, "/")
	trailingSlash := false
	for i, seg := range parts {
		last := i == len(parts)-1
		switch {
		case isDoubleDot(seg):
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			if last {
				trailingSlash = true
			}
		case isSingleDot(seg):
			if last {
				trailingSlash = true
			}
		default:
			segs = append(segs, encode(seg, pathSet))
		}
	}
	out := "/" + strings.Join(segs, "/")
	if trailingSlash && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// encSet is a percent-encode set (URL Standard), indexed by byte; a byte at or above
// 0x80 is always encoded.
type encSet [128]bool

func mkSet(extra string) encSet {
	var s encSet
	for i := range 0x20 {
		s[i] = true
	}
	s[0x7f] = true
	for i := range len(extra) {
		s[extra[i]] = true
	}
	return s
}

var (
	fragmentSet = mkSet(" \"<>`")
	querySet    = mkSet(" \"#<>'")
	pathSet     = mkSet(" \"#<>?`{}")
	userinfoSet = mkSet(" \"#<>?`{}/:;=@[\\]^|")
)

const upperHex = "0123456789ABCDEF"

func encode(s string, set encSet) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 0x80 || set[c&0x7f] {
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0xf])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ---- host parsing ------------------------------------------------------------

func parseHost(in string) (string, error) {
	if in == "" {
		return "", errInvalidURL
	}
	if in[0] == '[' {
		if in[len(in)-1] != ']' {
			return "", errInvalidURL
		}
		pieces, ok := parseIPv6(in[1 : len(in)-1])
		if !ok {
			return "", errInvalidURL
		}
		return "[" + serializeIPv6(pieces) + "]", nil
	}
	dec := percentDecode(in)
	if !utf8.ValidString(dec) {
		return "", errInvalidURL
	}
	for i := range len(dec) {
		if dec[i] >= 0x80 {
			return "", errInvalidURL // IDN: no x/net/idna
		}
	}
	ascii := strings.ToLower(dec)
	if ascii == "" {
		return "", errInvalidURL
	}
	for i := range len(ascii) {
		c := ascii[i]
		if c <= 0x20 || c == 0x7f || strings.IndexByte("#/:<>?@[\\]^|%", c) >= 0 {
			return "", errInvalidURL
		}
	}
	if endsInANumber(ascii) {
		v4, ok := parseIPv4(ascii)
		if !ok {
			return "", errInvalidURL
		}
		return v4, nil
	}
	return ascii, nil
}

func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			h, hok := hexVal(s[i+1])
			l, lok := hexVal(s[i+2])
			if hok && lok {
				b = append(b, h<<4|l)
				i += 2
				continue
			}
		}
		b = append(b, s[i])
	}
	return string(b)
}

// hexVal is the value of one hex digit.
func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func endsInANumber(host string) bool {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" {
		if len(parts) == 1 {
			return false
		}
		parts = parts[:len(parts)-1]
	}
	last := parts[len(parts)-1]
	if last != "" && allDigits(last) {
		return true
	}
	_, ok := parseIPv4Number(last)
	return ok
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseIPv4Number is the WHATWG IPv4 number parser: decimal, 0x hex or 0 octal. A value
// beyond 2^40 saturates, because every such value is out of range anyway.
func parseIPv4Number(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var radix uint64 = 10
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s, radix = s[2:], 16
	} else if len(s) >= 2 && s[0] == '0' {
		s, radix = s[1:], 8
	}
	if s == "" {
		return 0, true
	}
	var n uint64
	for i := range len(s) {
		d, ok := hexVal(s[i])
		if !ok || uint64(d) >= radix {
			return 0, false
		}
		n = n*radix + uint64(d)
		if n > 1<<40 {
			n = 1 << 40
		}
	}
	return n, true
}

// ipv4LastMax is the exclusive bound of the last part of an IPv4 host with 1 to 4 parts.
var ipv4LastMax = [5]uint64{0, 1 << 32, 1 << 24, 1 << 16, 1 << 8}

func parseIPv4(host string) (string, bool) {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 4 {
		return "", false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := parseIPv4Number(p)
		if !ok {
			return "", false
		}
		nums[i] = n
	}
	for i := range len(nums) - 1 {
		if nums[i] > 255 {
			return "", false
		}
	}
	if nums[len(nums)-1] >= ipv4LastMax[len(nums)] {
		return "", false
	}
	ip := nums[len(nums)-1]
	for i := range len(nums) - 1 {
		ip += nums[i] << (8 * (3 - i))
	}
	return strconv.FormatUint(ip>>24&255, 10) + "." + strconv.FormatUint(ip>>16&255, 10) + "." +
		strconv.FormatUint(ip>>8&255, 10) + "." + strconv.FormatUint(ip&255, 10), true
}

// parseIPv6 follows the WHATWG IPv6 parser (zone ids are not allowed).
func parseIPv6(in string) ([8]uint16, bool) {
	var addr [8]uint16
	piece, compress := 0, -1
	p := 0
	at := func(i int) byte {
		if i < len(in) {
			return in[i]
		}
		return 0
	}
	if at(p) == ':' {
		if at(p+1) != ':' {
			return addr, false
		}
		p += 2
		piece++
		compress = piece
	}
	for p < len(in) {
		if piece == 8 {
			return addr, false
		}
		if in[p] == ':' {
			if compress != -1 {
				return addr, false
			}
			p++
			piece++
			compress = piece
			continue
		}
		var value uint16
		length := 0
		for length < 4 && p < len(in) {
			d, ok := hexVal(in[p])
			if !ok {
				break
			}
			value = value*16 + uint16(d)
			p++
			length++
		}
		if at(p) == '.' {
			if length == 0 {
				return addr, false
			}
			p -= length
			if piece > 6 {
				return addr, false
			}
			seen := 0
			for p < len(in) {
				var v4 uint16
				have := false
				if seen > 0 {
					if in[p] == '.' && seen < 4 {
						p++
					} else {
						return addr, false
					}
				}
				if p >= len(in) || in[p] < '0' || in[p] > '9' {
					return addr, false
				}
				for p < len(in) && in[p] >= '0' && in[p] <= '9' {
					n := uint16(in[p] - '0')
					switch {
					case !have:
						v4, have = n, true
					case v4 == 0:
						return addr, false
					default:
						v4 = v4*10 + n
					}
					if v4 > 255 {
						return addr, false
					}
					p++
				}
				addr[piece] = addr[piece]*0x100 + v4
				seen++
				if seen == 2 || seen == 4 {
					piece++
				}
			}
			if seen != 4 {
				return addr, false
			}
			break
		} else if at(p) == ':' {
			p++
			if p >= len(in) {
				return addr, false
			}
		} else if p < len(in) {
			return addr, false
		}
		addr[piece] = value
		piece++
	}
	if compress != -1 {
		swaps := piece - compress
		piece = 7
		for piece != 0 && swaps > 0 {
			addr[piece], addr[compress+swaps-1] = addr[compress+swaps-1], addr[piece]
			piece--
			swaps--
		}
	} else if piece != 8 {
		return addr, false
	}
	return addr, true
}

// serializeIPv6 compresses the first longest run of at least two zero pieces and never
// prints the dotted IPv4 form (so ::ffff:7f00:1 stays hex, unlike netip.Addr.String).
func serializeIPv6(a [8]uint16) string {
	best, bestLen := -1, 0
	for i := 0; i < 8; {
		if a[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && a[j] == 0 {
			j++
		}
		if j-i > bestLen {
			best, bestLen = i, j-i
		}
		i = j
	}
	if bestLen < 2 {
		best = -1
	}
	var b strings.Builder
	ignore0 := false
	for i := range 8 {
		if ignore0 && a[i] == 0 {
			continue
		}
		ignore0 = false
		if best == i {
			if i == 0 {
				b.WriteString("::")
			} else {
				b.WriteString(":")
			}
			ignore0 = true
			continue
		}
		b.WriteString(strconv.FormatUint(uint64(a[i]), 16))
		if i != 7 {
			b.WriteByte(':')
		}
	}
	return b.String()
}
