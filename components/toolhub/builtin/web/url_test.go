package web

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// urlFixture is builtin-url-serialization.json: new URL(raw, base).toString() of the real
// Node URL class (or its error), recorded by tools/wiregen/builtin/network_guard.cases.mjs.
type urlFixture struct {
	Rows []struct {
		Raw   string  `json:"raw"`
		Base  *string `json:"base"`
		Href  *string `json:"href"`
		Error *string `json:"error"`
	} `json:"rows"`
}

// TestURLSerialization replays the recorded serialization corpus through parseURL and
// String: every printable ASCII character in a path, a query and a fragment, relative
// references against a base, percent escapes, ports, dot segments and user information.
// A row the recorded URL class rejects is an error here; a row with a scheme other than http and
// https is the scheme path of the guard (schemeOf) and not parsed here.
func TestURLSerialization(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-url-serialization.json")))
	dec.DisallowUnknownFields()
	var fx urlFixture
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-url-serialization: %v", err)
	}
	if len(fx.Rows) < 340 {
		t.Fatalf("the corpus has %d rows, want at least 340", len(fx.Rows))
	}
	var checked int
	for _, row := range fx.Rows {
		var base *webURL
		if row.Base != nil {
			var err error
			if base, err = parseURL(*row.Base, nil); err != nil {
				t.Fatalf("base %q: %v", *row.Base, err)
			}
		}
		if sc := schemeOf(row.Raw); sc != "" && sc != "http" && sc != "https" {
			if row.Error != nil {
				t.Errorf("%q: a scheme row cannot be an error in TS", row.Raw)
			}
			continue
		}
		checked++
		u, err := parseURL(row.Raw, base)
		switch {
		case row.Href != nil && err != nil:
			t.Errorf("%q (base %v): TS gave %q, Go failed: %v", row.Raw, row.Base, *row.Href, err)
		case row.Href != nil && u.String() != *row.Href:
			t.Errorf("%q (base %v): TS gave %q, Go gave %q", row.Raw, row.Base, *row.Href, u.String())
		case row.Href == nil && err == nil:
			t.Errorf("%q (base %v): TS rejected it (%v), Go gave %q", row.Raw, row.Base, *row.Error, u.String())
		}
	}
	if checked < 330 {
		t.Fatalf("only %d rows were checked", checked)
	}
}

// TestParseURLPieces pins the parts of a parsed URL that the guard reads.
func TestParseURLPieces(t *testing.T) {
	u, err := parseURL("HTTP://User:Pa%40ss@[2606:4700:4700:0:0:0:0:1111]:8080/a/../b?q=1#f", nil)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "http" || u.Username != "User" || u.Password != "Pa%40ss" || u.Host != "[2606:4700:4700::1111]" || u.Port != "8080" {
		t.Fatalf("pieces: %+v", u)
	}
	if got := u.hostname(); got != "2606:4700:4700::1111" {
		t.Fatalf("hostname = %q", got)
	}
	if !u.hasCredentials() {
		t.Fatal("hasCredentials")
	}
	if got, want := u.String(), "http://User:Pa%40ss@[2606:4700:4700::1111]:8080/b?q=1#f"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	// an empty user information is not a credential (url.username || url.password)
	if e, err := parseURL("http://@example.com/", nil); err != nil || e.hasCredentials() {
		t.Fatalf("empty userinfo: %v %v", e, err)
	}
	// a syntactically valid foreign scheme is reported by schemeOf and is not an http URL
	if got := schemeOf("  JavaScript:alert(1)"); got != "javascript" {
		t.Fatalf("schemeOf = %q", got)
	}
	if _, err := parseURL("ftp://example.com/", nil); err == nil {
		t.Fatal("parseURL must not accept the ftp scheme")
	}
	if got := schemeOf("not a url"); got != "" {
		t.Fatalf("schemeOf(not a url) = %q", got)
	}
}
