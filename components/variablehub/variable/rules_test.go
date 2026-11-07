package variable_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
)

func TestPlaceholders(t *testing.T) {
	got := variable.PlaceholderNames("{{var:A}}/x/{{ var:B }}?{{var:A}}&{{secret:C}}")
	if strings.Join(got, ",") != "A,B" {
		t.Fatalf("names = %v", got)
	}
	if s := variable.FillPlaceholders("{{var:A}}/{{var:Z}}", map[string]string{"A": "https://x"}); s != "https://x/{{var:Z}}" {
		t.Fatalf("fill = %q", s)
	}
}

func TestChecks(t *testing.T) {
	for _, n := range []string{"api_base", "1X", "A-B", "", strings.Repeat("A", 65)} {
		if _, err := variable.CheckName(n); !errors.Is(err, &variable.Error{Code: variable.CodeInvalidArgument}) {
			t.Errorf("CheckName(%q) = %v", n, err)
		}
	}
	if n, err := variable.CheckName(" API_BASE "); err != nil || n != "API_BASE" {
		t.Fatalf("CheckName = %q, %v", n, err)
	}
	if err := variable.CheckValue(strings.Repeat("x", 4097)); err == nil {
		t.Fatal("a long value passed")
	}
	if err := variable.CheckValue(""); err != nil {
		t.Fatal("an empty value was refused")
	}
	if _, err := variable.CheckDescription(strings.Repeat("d", 501)); err == nil {
		t.Fatal("a long description passed")
	}
}
