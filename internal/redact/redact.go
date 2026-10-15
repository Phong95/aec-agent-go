// Package redact is the one URL redaction the credential-holding types share
// (llmhub.ProviderConfig and persistence.ProviderRecord, Phase 8 review IN-01 and
// WR-201). A base URL can carry a key in its userinfo, path, query or fragment, so
// both types print it through URL and nothing else. Phase 12 adds Panic, the one
// way a recovered panic value reaches a log (10-SECURITY UF-1). It imports only
// the standard library (fmt, net/url, reflect and runtime).
package redact

import (
	"fmt"
	"net/url"
	"reflect"
	"runtime"
)

// Marker replaces a value that is shown in no part: the same text both types use
// for a secret.
const Marker = "[REDACTED]"

// URL returns raw with every part that can hold a credential removed. Only the
// scheme and the host (with its port) are shown as they are:
//
//   - userinfo becomes REDACTED, a query becomes ?REDACTED, a path other than "/"
//     becomes /REDACTED and a fragment is dropped (a redaction cannot tell a token
//     segment from a version segment, so no path is shown);
//   - a value that does not parse, that has no scheme, or that parses as an opaque
//     URL (u.Opaque != "") is replaced whole by Marker. net/url reads a scheme-less
//     "host:port/path" as scheme = host and Opaque = "port/path" and leaves Path
//     empty, so only the Opaque test catches a key in that path (review WR-201).
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Scheme == "" {
		return Marker
	}
	if u.User != nil {
		u.User = url.User("REDACTED")
	}
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	u.ForceQuery = false
	if u.Path != "" && u.Path != "/" {
		u.Path, u.RawPath = "/REDACTED", ""
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// Panic returns what a log may say about a recovered panic value. A panic value can
// carry user, model or memory text, so only its dynamic type is logged
// (fmt "%T", for example "string" or "*errors.errorString"). The one exception is a
// runtime error raised by the Go runtime itself (a value that implements
// runtime.Error and whose type is declared in package runtime, such as an index,
// nil-map or type-assertion failure, or the *runtime.PanicNilError of panic(nil)):
// its message carries no user data, so it follows the type after ": ". A user type
// that implements runtime.Error keeps only its type. A nil v gives "<nil>".
func Panic(v any) string {
	name := fmt.Sprintf("%T", v)
	re, ok := v.(runtime.Error)
	if !ok {
		return name
	}
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() != "runtime" {
		return name
	}
	return name + ": " + re.Error()
}
