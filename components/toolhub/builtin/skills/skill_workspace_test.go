package skills_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/skills"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The recorded cases of TestSkillViewMatchesGolden cover the recorded reference
// behaviour. These tests add what only this package does or what the recorder cannot build: the
// Windows rule of removeDir, the refusal of an unsafe skill name or file path
// (T-10-18), and the metadata of a skill with no source or version.

// testSkill is a skill with two files.
func testSkill() skill.Skill {
	return skill.Skill{
		Name:    "report-format",
		Body:    "Use the template.",
		Content: "---\nname: report-format\n---\n\nUse the template.\n",
		Files: []skill.File{
			{Path: "scripts/run.py", Content: "print('x')\n"},
			{Path: "references/format.md", Content: "# Format\n"},
		},
		Source:  skill.SourceApp,
		Version: 1,
	}
}

// sandboxFor builds an in-memory sandbox with the options and returns the sandbox, the
// logging view and a tool context that resolves it.
func sandboxFor(t *testing.T, opts ...aecmock.InMemoryOption) (*aecmock.InMemorySandbox, *recordingView, toolhub.Context) {
	t.Helper()
	provider := aecmock.NewInMemoryProvider(opts...)
	if _, err := provider.Create(t.Context(), sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared}); err != nil {
		t.Fatal(err)
	}
	sb, _ := provider.Sandbox("docker:shared")
	view, err := sb.ForAgent(t.Context(), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	logged := &recordingView{View: view}
	tc := toolhub.Context{GetSandbox: func(context.Context) (sandboxhub.View, error) { return logged, nil }}
	return sb, logged, tc
}

func TestCopySkillToWorkspace_SkipsUpToDate(t *testing.T) {
	sb, logged, tc := sandboxFor(t)
	s := testSkill()

	// A first copy writes SKILL.md and both files, in that order.
	if res := skills.CopyToWorkspace(t.Context(), s, tc, false); res.Dir != ".skills/report-format" || res.Err != "" {
		t.Fatalf("first copy = %+v", res)
	}
	want := []string{".skills/report-format/SKILL.md", ".skills/report-format/scripts/run.py", ".skills/report-format/references/format.md"}
	if !slices.Equal(logged.writes, want) {
		t.Fatalf("first copy wrote %q, want %q", logged.writes, want)
	}

	// A second copy of the same skill writes nothing: every sha256 matches.
	logged.writes = nil
	if res := skills.CopyToWorkspace(t.Context(), s, tc, false); res.Dir == "" {
		t.Fatalf("second copy = %+v", res)
	}
	if len(logged.writes) != 0 {
		t.Fatalf("second copy wrote %q, want nothing", logged.writes)
	}

	// One changed file is the only one rewritten.
	s.Files[0].Content = "print('y')\n"
	logged.writes = nil
	skills.CopyToWorkspace(t.Context(), s, tc, false)
	if want := []string{".skills/report-format/scripts/run.py"}; !slices.Equal(logged.writes, want) {
		t.Fatalf("copy after one change wrote %q, want %q", logged.writes, want)
	}

	// A file the skill no longer has stays without replace, and is removed with it.
	if err := sb.PlantFile(workDir+"/.skills/report-format/old.txt", "stale\n"); err != nil {
		t.Fatal(err)
	}
	skills.CopyToWorkspace(t.Context(), s, tc, false)
	if !contains(filesOf(t, logged.View), workDir+"/.skills/report-format/old.txt") {
		t.Fatal("a stale file must stay when replace is false")
	}
	skills.CopyToWorkspace(t.Context(), s, tc, true)
	if contains(filesOf(t, logged.View), workDir+"/.skills/report-format/old.txt") {
		t.Fatal("replace must remove the stale file")
	}
}

func contains(files [][2]string, path string) bool {
	return slices.ContainsFunc(files, func(f [2]string) bool { return f[0] == path })
}

// osView reports another operating system in its traits and nothing else.
type osView struct {
	sandboxhub.View
	os sandboxhub.OS
}

func (v osView) Traits() sandboxhub.Traits {
	tr := v.View.Traits()
	tr.OS = v.os
	return tr
}

// removeDir needs exec and a non-Windows sandbox: without either it does nothing, and
// the copy still writes the current files over the old ones.
func TestRemoveDir_SkippedWithoutExecOrOnWindows(t *testing.T) {
	cases := []struct {
		name string
		opts []aecmock.InMemoryOption
		os   sandboxhub.OS
	}{
		{"no exec capability", []aecmock.InMemoryOption{aecmock.WithCapabilities(sandboxhub.CapabilityFSRead, sandboxhub.CapabilityFSWrite, sandboxhub.CapabilityFSSearch)}, ""},
		{"windows sandbox", nil, sandboxhub.OSWindows},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sb, logged, tc := sandboxFor(t, c.opts...)
			if err := sb.PlantFile(workDir+"/.skills/report-format/old.txt", "stale\n"); err != nil {
				t.Fatal(err)
			}
			var env sandboxhub.View = logged
			if c.os != "" {
				env = osView{View: logged, os: c.os}
			}
			tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return env, nil }

			// replace: no rm is run, the stale file stays, the new files are written.
			res := skills.CopyToWorkspace(t.Context(), testSkill(), tc, true)
			if res.Dir != ".skills/report-format" || res.Err != "" {
				t.Fatalf("copy = %+v, want a success", res)
			}
			if len(logged.execs) != 0 {
				t.Fatalf("exec calls = %+v, want none", logged.execs)
			}
			files := filesOf(t, logged.View)
			if !contains(files, workDir+"/.skills/report-format/old.txt") || !contains(files, workDir+"/.skills/report-format/SKILL.md") {
				t.Fatalf("files = %q, want the stale file kept and the new files written", files)
			}

			// remove: the folder exists, but nothing can remove it; the result is still the dir.
			res = skills.RemoveFromWorkspace(t.Context(), "report-format", tc)
			if res.Dir != ".skills/report-format" || res.Err != "" || len(logged.execs) != 0 {
				t.Fatalf("remove = %+v with %d execs, want the dir and no exec", res, len(logged.execs))
			}
		})
	}
}

// A killed rm (no exit code) reports "exit null".
type killedExecView struct{ sandboxhub.View }

func (killedExecView) Exec(context.Context, sandboxhub.ExecRequest, sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	return sandboxhub.ExecResult{ExitCode: nil, TimedOut: true}, nil
}

func TestRemoveDir_KilledProcessReportsExitNull(t *testing.T) {
	_, logged, tc := sandboxFor(t)
	tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return killedExecView{logged}, nil }
	res := skills.CopyToWorkspace(t.Context(), testSkill(), tc, true)
	if want := "could not remove .skills/report-format: exit null"; res.Err != want {
		t.Fatalf("Err = %q, want %q", res.Err, want)
	}
}

// A skill that comes from another AgentSkills may hold a name or file path that is not
// a safe relative path. The copy refuses it before it writes anything (T-10-18).
func TestCopySkillToWorkspace_RefusesUnsafePaths(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*skill.Skill)
		want   string
	}{
		{"parent segment in a file path", func(s *skill.Skill) { s.Files[0].Path = "../evil.sh" }, `the skill has an unsafe file path "../evil.sh"`},
		{"absolute file path", func(s *skill.Skill) { s.Files[1].Path = "/etc/cron.d/x" }, `the skill has an unsafe file path "/etc/cron.d/x"`},
		{"file path that is not normalized", func(s *skill.Skill) { s.Files[0].Path = "./scripts/run.py" }, `the skill has an unsafe file path "./scripts/run.py"`},
		{"name with a slash", func(s *skill.Skill) { s.Name = "a/b" }, `the skill name "a/b" is not a safe folder name`},
		{"name that climbs", func(s *skill.Skill) { s.Name = ".." }, `the skill name ".." is not a safe folder name`},
		{"empty name", func(s *skill.Skill) { s.Name = "" }, `the skill name "" is not a safe folder name`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, logged, tc := sandboxFor(t)
			s := testSkill()
			c.mutate(&s)
			res := skills.CopyToWorkspace(t.Context(), s, tc, true)
			if res.Dir != "" || res.Err != c.want {
				t.Fatalf("copy = %+v, want Err %q", res, c.want)
			}
			if len(logged.writes) != 0 || len(logged.execs) != 0 {
				t.Fatalf("nothing may reach the sandbox: writes %q, execs %+v", logged.writes, logged.execs)
			}
		})
	}

	t.Run("remove with an unsafe name", func(t *testing.T) {
		_, logged, tc := sandboxFor(t)
		res := skills.RemoveFromWorkspace(t.Context(), "../x", tc)
		if res.Dir != "" || !strings.Contains(res.Err, "not a safe folder name") || len(logged.execs) != 0 {
			t.Fatalf("remove = %+v with %d execs, want a refusal", res, len(logged.execs))
		}
	})
}

// With no source and no version (a hand-built skill) the metadata leaves both out, as
// JSON.stringify leaves out an undefined value; the key order is otherwise the fixed one.
func TestSkillView_MetadataOmitsEmptySourceAndVersion(t *testing.T) {
	s := skill.Skill{Name: "bare", Body: "b", Content: "c"}
	tc := toolhub.Context{Skills: stubSkills{list: []skill.Skill{s}}}
	args, err := kernel.NewArguments([]byte(`{"name":"bare"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := skills.NewViewTool().Execute(t.Context(), args, tc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"skill":"bare","files":0}`; string(got) != want {
		t.Fatalf("metadata = %s, want %s", got, want)
	}
}

// A failing skill store is not a tool-level error: Execute returns it, and the
// executor turns it into an error result.
type failingSkills struct{ stubSkills }

func (failingSkills) Get(context.Context, string) (skill.Skill, bool, error) {
	return skill.Skill{}, false, context.DeadlineExceeded
}

func TestSkillView_StoreErrorIsReturned(t *testing.T) {
	args, _ := kernel.NewArguments([]byte(`{"name":"x"}`))
	_, err := skills.NewViewTool().Execute(t.Context(), args, toolhub.Context{Skills: failingSkills{}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v, want the store's error", err)
	}
}
