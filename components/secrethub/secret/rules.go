package secret

import (
	"cmp"
	"encoding/base64"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-101: secret names, allowed uses, placeholders and redaction - shared by the
// SecretHub, each agent's secret manager and the tools (rules.ts of core/ts).
// ---------------------------------------------------------------------------

var (
	nameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	hostRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	toolRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// urlPrefixRe spots a pasted URL ("https://api.example.com/v1") in an allowed host.
	urlPrefixRe = regexp.MustCompile(`^[a-z]+://`)
)

// Placeholder matches {{secret:NAME}} - what the model writes where a tool should put a
// secret's value. Group 1 is the name. (\s is the ASCII white space of RE2, where core/ts
// accepts any JS white space around the name.)
var Placeholder = regexp.MustCompile(`\{\{\s*secret:([A-Z][A-Z0-9_]*)\s*\}\}`)

// PlaceholderNames returns the names a text refers to with {{secret:NAME}}, in order of
// first use, without duplicates.
func PlaceholderNames(text string) []string {
	out := []string{}
	for _, m := range Placeholder.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// FillPlaceholders replaces each {{secret:NAME}} with its value; a name missing from
// values is left as it is.
func FillPlaceholders(text string, values map[string]string) string {
	return Placeholder.ReplaceAllStringFunc(text, func(whole string) string {
		m := Placeholder.FindStringSubmatch(whole)
		if v, ok := values[m[1]]; ok {
			return v
		}
		return whole
	})
}

// CheckName returns the name trimmed, or an *Error ("invalid_argument") unless it is a
// valid secret name.
func CheckName(name string) (string, error) {
	n := jsstr.Trim(name)
	if !nameRe.MatchString(n) || len(n) > MaxNameChars {
		return "", NewError(`secret name "`+n+`" must be upper-case letters, digits and underscores, starting with a letter (max `+strconv.Itoa(MaxNameChars)+")", CodeInvalidArgument)
	}
	return n, nil
}

// CheckValue returns an *Error ("invalid_argument") unless value can be stored.
func CheckValue(value string) error {
	if value == "" {
		return NewError("a secret needs a value", CodeInvalidArgument)
	}
	if len(value) > MaxValueBytes {
		return NewError("a secret's value is larger than "+strconv.Itoa(MaxValueBytes)+" bytes", CodeInvalidArgument)
	}
	return nil
}

// NormalizeDefinition returns a definition's metadata checked and normalised: the
// description trimmed (blank = none), the hosts lower-cased and deduplicated (a pasted URL
// keeps its host), the tools trimmed and deduplicated. A field the definition leaves nil
// comes from base, or - for a new secret (base nil) - is the default (no description, no
// host, DefaultTools). It returns an *Error ("invalid_argument").
func NormalizeDefinition(def Definition, base *Meta) (Meta, error) {
	fail := func(m string) (Meta, error) { return Meta{}, NewError(m, CodeInvalidArgument) }
	var out Meta
	if base != nil {
		out = Meta{Description: base.Description, AllowedHosts: slices.Clone(base.AllowedHosts), AllowedTools: slices.Clone(base.AllowedTools)}
	}
	if out.AllowedHosts == nil {
		out.AllowedHosts = []string{}
	}
	if out.AllowedTools == nil {
		if base != nil {
			out.AllowedTools = []string{}
		} else {
			out.AllowedTools = DefaultTools()
		}
	}
	if def.Description != nil {
		out.Description = jsstr.Trim(*def.Description)
		if kernel.UTF16Len(out.Description) > MaxDescriptionChars {
			return fail("description is longer than " + strconv.Itoa(MaxDescriptionChars) + " characters")
		}
	}
	if def.AllowedHosts != nil {
		hosts := []string{}
		for _, h := range def.AllowedHosts {
			n, err := normalizeHost(h)
			if err != nil {
				return Meta{}, err
			}
			if !slices.Contains(hosts, n) {
				hosts = append(hosts, n)
			}
		}
		if len(hosts) > MaxHosts {
			return fail("at most " + strconv.Itoa(MaxHosts) + " hosts")
		}
		out.AllowedHosts = hosts
	}
	if def.AllowedTools != nil {
		tools := []string{}
		for _, t := range def.AllowedTools {
			n := jsstr.Trim(t)
			if !slices.Contains(tools, n) {
				tools = append(tools, n)
			}
		}
		for _, t := range tools {
			if !toolRe.MatchString(t) {
				return fail("allowedTools must be tool names (lower-case letters, digits, underscores)")
			}
		}
		if len(tools) > MaxTools {
			return fail("at most " + strconv.Itoa(MaxTools) + " tools")
		}
		out.AllowedTools = tools
	}
	return out, nil
}

// normalizeHost is one allowed host, lower-cased, without a trailing dot; a pasted URL
// keeps its host.
func normalizeHost(host string) (string, error) {
	bad := func() (string, error) {
		return "", NewError(`"`+host+`" is not a host name (e.g. api.example.com or *.example.com)`, CodeInvalidArgument)
	}
	h := jsstr.ToLower(jsstr.Trim(host))
	// Accept a pasted URL ("https://api.example.com/v1") - keep the host.
	if urlPrefixRe.MatchString(h) {
		u, err := url.Parse(h)
		if err != nil {
			return "", NewError(`"`+host+`" is not a host name`, CodeInvalidArgument)
		}
		h = u.Hostname()
	}
	h = strings.TrimSuffix(h, ".")
	if !hostRe.MatchString(h) || len(h) > 253 {
		return bad()
	}
	return h, nil
}

// HostAllowed reports whether host matches one of the patterns. "*.example.com" matches
// subdomains, not example.com.
func HostAllowed(host string, patterns []string) bool {
	h := strings.TrimSuffix(jsstr.ToLower(host), ".")
	for _, p := range patterns {
		if rest, ok := strings.CutPrefix(p, "*."); ok {
			suffix := "." + rest
			if strings.HasSuffix(h, suffix) && len(h) > len(suffix) {
				return true
			}
			continue
		}
		if h == p {
			return true
		}
	}
	return false
}

// CheckUse returns an *Error ("not_allowed") unless the secret may be used this way: the
// tool is allowed, and - when use names a host - the host is allowed too.
func CheckUse(s Info, use Use) error {
	name, allowedHosts, allowedTools := s.Name, s.AllowedHosts, s.AllowedTools
	if !slices.Contains(allowedTools, use.Tool) {
		allowed := strings.Join(allowedTools, ", ")
		if allowed == "" {
			allowed = "no tool"
		}
		return NewError("secret "+name+" may not be used by "+use.Tool+" (allowed: "+allowed+")", CodeNotAllowed)
	}
	if use.Host != "" && !HostAllowed(use.Host, allowedHosts) {
		if len(allowedHosts) > 0 {
			return NewError("secret "+name+" may not be sent to "+use.Host+" (allowed hosts: "+strings.Join(allowedHosts, ", ")+")", CodeNotAllowed)
		}
		return NewError("secret "+name+" has no allowed hosts yet "+emDash+" the user has to set them before it can be sent anywhere", CodeNotAllowed)
	}
	return nil
}

// emDash is U+2014, written as an escape so this source stays ASCII.
const emDash = "—"

// minRedactLength is the shortest value that is redacted, in UTF-16 code units: shorter
// values would mangle ordinary text.
const minRedactLength = 6

// Redactor remembers the secret values used in this process and removes them from text -
// tool output, errors - before it reaches the model, a store, a trace or the UI. A value
// is also caught URL-encoded (encodeURIComponent) and base64-encoded (e.g. in a Basic auth
// header echoed back). The zero Redactor is ready to use; it is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values map[string]string // value form -> secret name
}

// Add remembers a value (and its encodings) under its secret's name.
func (r *Redactor) Add(name, value string) {
	if kernel.UTF16Len(value) < minRedactLength {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = map[string]string{}
	}
	for _, v := range []string{value, EncodeURIComponent(value), base64.StdEncoding.EncodeToString([]byte(value))} {
		if kernel.UTF16Len(v) >= minRedactLength {
			r.values[v] = name
		}
	}
}

// Forget drops the values of a secret (deleted, or given a new value).
func (r *Redactor) Forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for v, n := range r.values {
		if n == name {
			delete(r.values, v)
		}
	}
}

// Len returns how many value forms are remembered.
func (r *Redactor) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.values)
}

// Redact returns text with every known value replaced by [secret:NAME]. Longer values go
// first, so a value that contains another is replaced whole.
func (r *Redactor) Redact(text string) string {
	if text == "" {
		return text
	}
	r.mu.RLock()
	if len(r.values) == 0 {
		r.mu.RUnlock()
		return text
	}
	type entry struct{ value, name string }
	entries := make([]entry, 0, len(r.values))
	for v, n := range r.values {
		entries = append(entries, entry{v, n})
	}
	r.mu.RUnlock()
	slices.SortFunc(entries, func(a, b entry) int {
		return cmp.Or(cmp.Compare(len(b.value), len(a.value)), cmp.Compare(a.value, b.value))
	})
	out := text
	for _, e := range entries {
		if strings.Contains(out, e.value) {
			out = strings.ReplaceAll(out, e.value, "[secret:"+e.name+"]")
		}
	}
	return out
}

// EncodeURIComponent is the JS encodeURIComponent of s: every byte of its UTF-8 form
// percent-encoded (upper-case hex) except the letters, the digits and - _ . ! ~ * ' ( ).
func EncodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
