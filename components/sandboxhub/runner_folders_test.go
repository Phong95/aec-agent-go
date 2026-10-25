package sandboxhub_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-159: the folder names and hellos of protocol 2 (the same rules as core/ts).

func TestRunnerFolderName(t *testing.T) {
	cases := []struct {
		base  string
		taken []string
		want  string
	}{
		{"my app", nil, "my app"},
		{"a:b*c", nil, "a-b-c"},
		{"a::b", nil, "a-b"},
		{"...", nil, "folder"},
		{" .app. ", nil, "app"},
		{"App", []string{"app"}, "App-2"},
		{"app", []string{"app", "app-2"}, "app-3"},
	}
	for _, c := range cases {
		if got := sandboxhub.RunnerFolderName(c.base, c.taken); got != c.want {
			t.Errorf("RunnerFolderName(%q, %v) = %q, want %q", c.base, c.taken, got, c.want)
		}
	}
}

func TestParseRunnerFolders(t *testing.T) {
	got, err := sandboxhub.ParseRunnerFolders([]sandboxhub.RunnerFolder{{Name: "app", AllowExec: true}, {Name: "app-2"}})
	if err != nil || len(got) != 2 || got[0] != (sandboxhub.RunnerFolder{Name: "app", AllowExec: true}) || got[1] != (sandboxhub.RunnerFolder{Name: "app-2"}) {
		t.Fatalf("ParseRunnerFolders = %+v, %v", got, err)
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, " app", "app.", "c:"} {
		_, err := sandboxhub.ParseRunnerFolders([]sandboxhub.RunnerFolder{{Name: name, AllowExec: true}})
		wantInvalid(t, name, err)
	}
	_, err = sandboxhub.ParseRunnerFolders([]sandboxhub.RunnerFolder{{Name: "App"}, {Name: "app"}})
	wantInvalid(t, "App+app", err)
	_, err = sandboxhub.ParseRunnerFolders(nil)
	wantInvalid(t, "none", err)
	many := make([]sandboxhub.RunnerFolder, sandboxhub.RunnerMaxFolders+1)
	for i := range many {
		many[i] = sandboxhub.RunnerFolder{Name: fmt.Sprintf("f%d", i)}
	}
	_, err = sandboxhub.ParseRunnerFolders(many)
	wantInvalid(t, "too many", err)
}

func TestParseRunnerHelloProtocols(t *testing.T) {
	v1, err := sandboxhub.ParseRunnerHello([]byte(`{"type":"hello","protocol":1,"os":"linux","folderName":" my-app ","allowExec":true}`))
	if err != nil || v1.FolderName != "my-app" || !v1.AllowExec || v1.Folders != nil {
		t.Fatalf("v1 = %+v, %v", v1, err)
	}
	v2, err := sandboxhub.ParseRunnerHello([]byte(`{"type":"hello","protocol":2,"os":"linux","folderName":"ignored","allowExec":false,"folders":[{"name":"app","allowExec":false},{"name":"docs","allowExec":true}]}`))
	if err != nil || v2.FolderName != "app" || !v2.AllowExec || len(v2.Folders) != 2 {
		t.Fatalf("v2 = %+v, %v", v2, err)
	}
	for _, bad := range []string{
		`{"type":"hello","protocol":2,"os":"linux","folderName":"x","folders":[]}`,
		`{"type":"hello","protocol":2,"os":"linux","folderName":"x"}`,
	} {
		_, err := sandboxhub.ParseRunnerHello([]byte(bad))
		wantInvalid(t, bad, err)
	}
	_, err = sandboxhub.ParseRunnerHello([]byte(`{"type":"hello","protocol":99,"os":"linux","folderName":"x"}`))
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied || se.Message != "The runner speaks protocol 99; this server speaks 1 and 2" {
		t.Fatalf("protocol 99 = %v", err)
	}
}

func wantInvalid(t *testing.T, what string, err error) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeInvalidArgument {
		t.Errorf("%s: error = %v, want invalid_argument", what, err)
	}
}
