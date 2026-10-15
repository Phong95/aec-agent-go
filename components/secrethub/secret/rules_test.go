package secret_test

import (
	"encoding/base64"
	"errors"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/kernel"
)

func TestHostAllowed(t *testing.T) {
	cases := []struct {
		host     string
		patterns []string
		want     bool
	}{
		{"api.example.com", []string{"api.example.com"}, true},
		{"API.example.com.", []string{"api.example.com"}, true},
		{"evil-api.example.com", []string{"api.example.com"}, false},
		{"eu.api.example.com", []string{"*.example.com"}, true},
		{"example.com", []string{"*.example.com"}, false},
		{"badexample.com", []string{"*.example.com"}, false},
		{"api.example.com", nil, false},
	}
	for _, c := range cases {
		if got := secret.HostAllowed(c.host, c.patterns); got != c.want {
			t.Errorf("HostAllowed(%q, %v) = %v, want %v", c.host, c.patterns, got, c.want)
		}
	}
}

func TestRedactorRedactsEncodingsLongestFirstButNotTinyValues(t *testing.T) {
	var r secret.Redactor
	r.Add("KEY", "abc+def/ghi=")
	r.Add("TINY", "abc")
	b64 := base64.StdEncoding.EncodeToString([]byte("abc+def/ghi="))
	in := "a abc+def/ghi= b " + secret.EncodeURIComponent("abc+def/ghi=") + " c " + b64 + " abc"
	if got, want := r.Redact(in), "a [secret:KEY] b [secret:KEY] c [secret:KEY] abc"; got != want {
		t.Fatalf("Redact = %q, want %q", got, want)
	}
	if secret.EncodeURIComponent("abc+def/ghi=") != "abc%2Bdef%2Fghi%3D" {
		t.Fatalf("EncodeURIComponent = %q", secret.EncodeURIComponent("abc+def/ghi="))
	}
	r.Forget("KEY")
	if r.Len() != 0 || r.Redact("abc+def/ghi=") != "abc+def/ghi=" {
		t.Fatal("Forget did not drop the value")
	}
}

func TestCheckNameAndValue(t *testing.T) {
	if n, err := secret.CheckName("  WEATHER_KEY "); err != nil || n != "WEATHER_KEY" {
		t.Fatalf("CheckName = %q, %v", n, err)
	}
	for _, bad := range []string{"lower", "1KEY", "", "A-B"} {
		if _, err := secret.CheckName(bad); !errors.Is(err, &secret.Error{Code: secret.CodeInvalidArgument}) {
			t.Errorf("CheckName(%q) = %v, want invalid_argument", bad, err)
		}
	}
	if err := secret.CheckValue(""); !errors.Is(err, &secret.Error{Code: secret.CodeInvalidArgument}) {
		t.Errorf("an empty value must be invalid: %v", err)
	}
}

func TestNormalizeDefinition(t *testing.T) {
	m, err := secret.NormalizeDefinition(secret.Definition{AllowedHosts: []string{"https://API.example.com/v1", "api.example.com", "*.example.org"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.AllowedHosts, []string{"api.example.com", "*.example.org"}) || !slices.Equal(m.AllowedTools, []string{"http_request"}) {
		t.Fatalf("normalized = %+v", m)
	}
	for _, def := range []secret.Definition{
		{AllowedHosts: []string{"not a host"}},
		{AllowedTools: []string{"Bad-Tool"}},
	} {
		if _, err := secret.NormalizeDefinition(def, nil); !errors.Is(err, &secret.Error{Code: secret.CodeInvalidArgument}) {
			t.Errorf("NormalizeDefinition(%+v) = %v, want invalid_argument", def, err)
		}
	}
	// A patch keeps what it leaves nil and clears a description set to "".
	empty := ""
	base := secret.Meta{Description: "old", AllowedHosts: []string{"a.example.com"}, AllowedTools: []string{"terminal"}}
	m, err = secret.NormalizeDefinition(secret.Definition{Description: &empty}, &base)
	if err != nil || m.Description != "" || !slices.Equal(m.AllowedHosts, base.AllowedHosts) || !slices.Equal(m.AllowedTools, base.AllowedTools) {
		t.Fatalf("patch = %+v, %v", m, err)
	}
}

func TestCheckUse(t *testing.T) {
	s := secret.Info{Name: "K", AllowedHosts: []string{"api.example.com"}, AllowedTools: []string{"http_request"}}
	if err := secret.CheckUse(s, secret.Use{Tool: "http_request", Host: "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := secret.CheckUse(s, secret.Use{Tool: "terminal"}); !errors.Is(err, &secret.Error{Code: secret.CodeNotAllowed}) {
		t.Fatalf("terminal must be refused: %v", err)
	}
	s.AllowedHosts = nil
	err := secret.CheckUse(s, secret.Use{Tool: "http_request", Host: "api.example.com"})
	var se *secret.Error
	if !errors.As(err, &se) || se.Message != "secret K has no allowed hosts yet — the user has to set them before it can be sent anywhere" {
		t.Fatalf("no hosts: %v", err)
	}
}

func TestPlaceholders(t *testing.T) {
	text := "Bearer {{secret:A}} and {{ secret:B }} and {{secret:A}} and {{secret:lower}}"
	if got := secret.PlaceholderNames(text); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("PlaceholderNames = %v", got)
	}
	if got := secret.FillPlaceholders(text, map[string]string{"A": "x"}); got != "Bearer x and {{ secret:B }} and x and {{secret:lower}}" {
		t.Fatalf("FillPlaceholders = %q", got)
	}
}

func TestErrorIsNotRetryable(t *testing.T) {
	if kernel.IsRetryable(secret.NewError("x", secret.CodeNotAllowed)) {
		t.Fatal("a secret error must not be retryable")
	}
}
