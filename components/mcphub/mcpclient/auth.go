package mcpclient

import (
	"regexp"
	"strings"
)

// challengeParam matches one auth-param of a WWW-Authenticate challenge: name="quoted"
// or name=token.
var challengeParam = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)\s*=\s*(?:"((?:[^"\\]|\\.)*)"|([^\s,]+))`)

// unescape drops the backslash of a quoted-pair.
var unescape = regexp.MustCompile(`\\(.)`)

// ParseWWWAuthenticate returns the parameters of a Bearer WWW-Authenticate challenge
// (resource_metadata, scope, error, error_description...), names lower-cased - the
// parseWwwAuthenticate of core/ts (A-214). An empty header gives an empty map.
func ParseWWWAuthenticate(header string) map[string]string {
	out := map[string]string{}
	if header == "" {
		return out
	}
	text := header
	if i := strings.Index(strings.ToLower(header), "bearer"); i >= 0 {
		text = header[i+len("bearer"):]
	}
	for _, m := range challengeParam.FindAllStringSubmatch(text, -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		out[strings.ToLower(m[1])] = unescape.ReplaceAllString(v, "$1")
	}
	return out
}
