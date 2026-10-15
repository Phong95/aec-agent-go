package skills_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/skills"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// builtin-skills-view.json is recorded by wiregen from the reference SkillViewTool and the
// real copySkillToWorkspace and removeSkillFromWorkspace, run over a stub ToolContext
// and the SDK's real in-memory sandbox (see tools/wiregen/skills/view.cases.mjs). Each
// case carries its inputs and what the reference code returned, which writeFile and exec
// calls it made and the files the workspace held afterwards, so this replay stays
// generic: it rebuilds the same environment from aecmock's in-memory sandbox, runs
// the Go code and compares everything byte for byte.

type fixtureSandbox struct {
	Mode   string `json:"mode"`
	Limits *struct {
		MaxWriteBytes *int `json:"maxWriteBytes"`
	} `json:"limits"`
	Capabilities []string `json:"capabilities"`
	Planted      []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"planted"`
	Symlinks []struct {
		Link   string `json:"link"`
		Target string `json:"target"`
	} `json:"symlinks"`
	NoFiles bool `json:"noFiles"`
}

type execRecord struct {
	Command        []string `json:"command"`
	TimeoutMs      int      `json:"timeoutMs"`
	MaxOutputBytes int      `json:"maxOutputBytes"`
	ExitCode       *int     `json:"exitCode"`
}

type viewCase struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Skills    *[]string       `json:"skills"`
	Args      json.RawMessage `json:"args"`
	Skill     string          `json:"skill"`
	Replace   bool            `json:"replace"`
	SkillName string          `json:"skillName"`
	Sandbox   *fixtureSandbox `json:"sandbox"`
	Output    json.RawMessage `json:"output"`
	Result    json.RawMessage `json:"result"`
	Writes    []string        `json:"writes"`
	Execs     []execRecord    `json:"execs"`
	Files     *[][2]string    `json:"files"`
}

type viewFixture struct {
	Tool struct {
		Name       string          `json:"name"`
		Version    string          `json:"version"`
		Definition json.RawMessage `json:"definition"`
	} `json:"tool"`
	Library map[string]skill.Skill `json:"library"`
	Cases   []viewCase             `json:"cases"`
}

// stubSkills is the small AgentSkills of the recorder: Get finds a skill by name in
// the list it was given and List returns the list.
type stubSkills struct{ list []skill.Skill }

func (s stubSkills) List(context.Context) ([]skill.Skill, error) { return s.list, nil }

func (s stubSkills) Get(_ context.Context, name string) (skill.Skill, bool, error) {
	for _, sk := range s.list {
		if sk.Name == name {
			return sk, true, nil
		}
	}
	return skill.Skill{}, false, nil
}

var errNotUsed = errors.New("not used")

func (stubSkills) Create(context.Context, skill.Package) (skill.Skill, error) {
	return skill.Skill{}, errNotUsed
}

func (stubSkills) Update(context.Context, string, skill.Package) (skill.Skill, error) {
	return skill.Skill{}, errNotUsed
}
func (stubSkills) Delete(context.Context, string) error { return errNotUsed }

// recordingView logs every WriteFile and Exec and delegates everything else, like the
// recorder's logged() wrapper.
type recordingView struct {
	sandboxhub.View
	writes []string
	execs  []execRecord
}

func (v *recordingView) WriteFile(ctx context.Context, path, content string, opts sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	v.writes = append(v.writes, path)
	return v.View.WriteFile(ctx, path, content, opts)
}

func (v *recordingView) Exec(ctx context.Context, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	res, err := v.View.Exec(ctx, req, opts)
	if err == nil {
		v.execs = append(v.execs, execRecord{Command: req.Command, TimeoutMs: req.TimeoutMs, MaxOutputBytes: req.MaxOutputBytes, ExitCode: res.ExitCode})
	}
	return res, err
}

// workDir is the working directory of the agent's view in the shared in-memory sandbox.
const workDir = "/workspace/agents/agent-1"

// environment is the sandbox side of one case: the getSandbox of the tool context,
// the logging view (nil without a sandbox) and the plain view the files are dumped
// through afterwards.
type environment struct {
	getSandbox func(context.Context) (sandboxhub.View, error)
	logged     *recordingView
	plain      sandboxhub.View
}

func newEnvironment(t *testing.T, spec *fixtureSandbox) environment {
	t.Helper()
	return newEnvironmentFor(t, spec, "agent-1")
}

// newEnvironmentFor is newEnvironment for the agent with the given id: the shared in-memory
// sandbox gives it the working directory /workspace/agents/<id>, which the planted and
// symlink paths of the spec are relative to. The skill_manage corpus records the agent a1.
func newEnvironmentFor(t *testing.T, spec *fixtureSandbox, agentID string) environment {
	t.Helper()
	if spec == nil || spec.Mode == "none" {
		return environment{}
	}
	if spec.Mode == "throws" {
		return environment{getSandbox: func(context.Context) (sandboxhub.View, error) { return nil, errors.New("no sandbox for this agent") }}
	}
	var opts []aecmock.InMemoryOption
	if spec.Mode == "host" {
		opts = append(opts, aecmock.WithKind(sandboxhub.KindHost))
	}
	if spec.Limits != nil && spec.Limits.MaxWriteBytes != nil {
		opts = append(opts, aecmock.WithLimits(sandboxhub.LimitsPatch{MaxWriteBytes: spec.Limits.MaxWriteBytes}))
	}
	if spec.Capabilities != nil {
		caps := make([]sandboxhub.Capability, len(spec.Capabilities))
		for i, c := range spec.Capabilities {
			caps[i] = sandboxhub.Capability(c)
		}
		opts = append(opts, aecmock.WithCapabilities(caps...))
	}
	provider := aecmock.NewInMemoryProvider(opts...)
	if _, err := provider.Create(t.Context(), sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sb, ok := provider.Sandbox("docker:shared")
	if !ok {
		t.Fatal("the provider lost its sandbox")
	}
	view, err := sb.ForAgent(t.Context(), agentID)
	if err != nil {
		t.Fatalf("ForAgent: %v", err)
	}
	for _, p := range spec.Planted {
		if err := sb.PlantFile("/workspace/agents/"+agentID+"/"+p.Path, p.Content); err != nil {
			t.Fatalf("PlantFile(%s): %v", p.Path, err)
		}
	}
	for _, l := range spec.Symlinks {
		if err := sb.Symlink("/workspace/agents/"+agentID+"/"+l.Link, l.Target); err != nil {
			t.Fatalf("Symlink(%s): %v", l.Link, err)
		}
	}
	logged := &recordingView{View: view}
	return environment{
		getSandbox: func(context.Context) (sandboxhub.View, error) { return logged, nil },
		logged:     logged,
		plain:      view,
	}
}

// filesOf lists every file of the working directory with its content, sorted by path,
// like the recorder's filesOf.
func filesOf(t *testing.T, view sandboxhub.View) [][2]string {
	t.Helper()
	yes, limit := true, 1000
	listed, err := view.List(t.Context(), ".", sandboxhub.ListOptions{Recursive: &yes, IncludeHidden: &yes, MaxEntries: &limit})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := [][2]string{}
	for _, e := range listed.Entries {
		if e.Type != sandboxhub.EntryFile {
			continue
		}
		fc, err := view.ReadFile(t.Context(), e.Path, sandboxhub.ReadOptions{})
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", e.Path, err)
		}
		out = append(out, [2]string{e.Path, fc.Content})
	}
	slices.SortFunc(out, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return out
}

// resultJSON is the wire form of a workspace result: {"dir":..}, {"error":..} or null.
func resultJSON(t *testing.T, r skills.WorkspaceResult) string {
	t.Helper()
	var v any
	switch {
	case r.Dir != "":
		v = struct {
			Dir string `json:"dir"`
		}{r.Dir}
	case r.Err != "":
		v = struct {
			Error string `json:"error"`
		}{r.Err}
	}
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSkillViewMatchesGolden replays builtin-skills-view.json: the tool's name, version and
// definition, the output of skill_view (the render, the file view, every error text,
// the metadata with no copiedTo unless a copy happened), the result of the
// workspace copy and removal, and the write, exec and file effects in the sandbox.
func TestSkillViewMatchesGolden(t *testing.T) {
	var fx viewFixture
	if err := json.Unmarshal(wiretest.Load(t, "builtin-skills-view.json"), &fx); err != nil {
		t.Fatalf("decode builtin-skills-view: %v", err)
	}
	if len(fx.Cases) < 70 {
		t.Fatalf("the corpus holds %d cases, want at least 70", len(fx.Cases))
	}

	handler := skills.NewViewTool()
	if handler.Name() != fx.Tool.Name || handler.Version() != fx.Tool.Version {
		t.Errorf("tool = %s %s, want the TS %s %s", handler.Name(), handler.Version(), fx.Tool.Name, fx.Tool.Version)
	}
	def, err := kernel.EncodeJSON(handler.Definition())
	if err != nil {
		t.Fatal(err)
	}
	if string(def) != string(fx.Tool.Definition) {
		t.Errorf("definition differs from TS\n got: %s\nwant: %s", def, fx.Tool.Definition)
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			env := newEnvironment(t, c.Sandbox)
			tc := toolhub.Context{SessionID: "s1", AgentID: "agent-1", GetSandbox: env.getSandbox}

			switch c.Kind {
			case "view":
				if c.Skills != nil {
					list := make([]skill.Skill, len(*c.Skills))
					for i, id := range *c.Skills {
						list[i] = fx.Library[id]
					}
					tc.Skills = stubSkills{list: list}
				}
				args, err := kernel.NewArguments(c.Args)
				if err != nil {
					t.Fatalf("arguments: %v", err)
				}
				out, err := handler.Execute(t.Context(), args, tc)
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				got, err := kernel.EncodeJSON(out)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(c.Output) {
					t.Errorf("output differs from TS\n got: %s\nwant: %s", got, c.Output)
				}
			case "copy":
				res := skills.CopyToWorkspace(t.Context(), fx.Library[c.Skill], tc, c.Replace)
				if got := resultJSON(t, res); got != string(c.Result) {
					t.Errorf("result differs from TS\n got: %s\nwant: %s", got, c.Result)
				}
			case "remove":
				res := skills.RemoveFromWorkspace(t.Context(), c.SkillName, tc)
				if got := resultJSON(t, res); got != string(c.Result) {
					t.Errorf("result differs from TS\n got: %s\nwant: %s", got, c.Result)
				}
			default:
				t.Fatalf("unknown case kind %q", c.Kind)
			}

			var writes []string
			var execs []execRecord
			if env.logged != nil {
				writes, execs = env.logged.writes, env.logged.execs
			}
			if !slices.Equal(writes, c.Writes) {
				t.Errorf("writeFile calls differ from TS\n got: %q\nwant: %q", writes, c.Writes)
			}
			if !reflect.DeepEqual(nilIfEmpty(execs), nilIfEmpty(c.Execs)) {
				t.Errorf("exec calls differ from TS\n got: %+v\nwant: %+v", execs, c.Execs)
			}
			if c.Files != nil && env.plain != nil {
				if got := filesOf(t, env.plain); !reflect.DeepEqual(got, *c.Files) {
					t.Errorf("workspace files differ from TS\n got: %q\nwant: %q", got, *c.Files)
				}
			}
			if (c.Files != nil) != (env.plain != nil && !specNoFiles(c.Sandbox)) {
				t.Errorf("the fixture has files = %v for a case with sandbox %+v", c.Files != nil, c.Sandbox)
			}
		})
	}
}

func specNoFiles(s *fixtureSandbox) bool { return s != nil && s.NoFiles }

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// The corpus must exercise the effects it was recorded for, so a recorder that stopped
// covering a branch cannot go unnoticed.
func TestSkillViewCorpusCoversEveryBranch(t *testing.T) {
	var fx viewFixture
	if err := json.Unmarshal(wiretest.Load(t, "builtin-skills-view.json"), &fx); err != nil {
		t.Fatal(err)
	}
	var copied, refusedHost, writeFailed, skippedUpToDate, rmRan, rmFailed, noSandbox, errorOutputs, fileViews int
	for _, c := range fx.Cases {
		out := string(c.Output) + string(c.Result)
		switch {
		case strings.Contains(out, `"copiedTo"`):
			copied++
		case strings.Contains(out, "the workspace is a host folder"):
			refusedHost++
		case strings.Contains(out, "could not be copied to your workspace (Content is"):
			writeFailed++
		}
		if strings.Contains(c.Name, "skips-up-to-date") && len(c.Writes) > 0 && len(c.Writes) < 4 {
			skippedUpToDate++
		}
		if len(c.Execs) > 0 && c.Execs[0].ExitCode != nil && *c.Execs[0].ExitCode == 0 {
			rmRan++
		}
		if len(c.Execs) > 0 && c.Execs[0].ExitCode != nil && *c.Execs[0].ExitCode != 0 {
			rmFailed++
		}
		if (c.Sandbox == nil || c.Sandbox.Mode == "none") && c.Kind == "view" && strings.Contains(c.Name, "with-files-no-sandbox") {
			noSandbox++
		}
		if strings.Contains(string(c.Output), `"isError":true`) {
			errorOutputs++
		}
		if strings.HasPrefix(c.Name, "file-") {
			fileViews++
		}
	}
	for name, n := range map[string]int{
		"copied": copied, "refusedHost": refusedHost, "writeFailed": writeFailed, "skippedUpToDate": skippedUpToDate,
		"rmRan": rmRan, "rmFailed": rmFailed, "noSandbox": noSandbox, "errorOutputs": errorOutputs, "fileViews": fileViews,
	} {
		if n == 0 {
			t.Errorf("the corpus has no %s case", name)
		}
	}
}

// TestSkillView_HugeNumberArgument is the regression of RESEARCH Pitfall 7, the latent
// Phase 10 bug: the argument helper decoded the whole object into a map and returned an
// empty string when ONE value was out of range, so a number such as 1e999 (Infinity in
// JS) next to a valid name made skill_view report an empty name. A JSON decoder must keep
// every other field. The helper now decodes per key through internal/toolkit.
func TestSkillView_HugeNumberArgument(t *testing.T) {
	sk := skill.Skill{Name: "rfi-triage", Description: "Triage requests.", Content: "---\nname: rfi-triage\n---\nStep 1.", Body: "Step 1."}
	tc := toolhub.Context{SessionID: "s1", AgentID: "agent-1", Skills: stubSkills{list: []skill.Skill{sk}}}

	for _, tcase := range []struct {
		name string
		args string
		want string
	}{
		{"number beside the name", `{"name":"rfi-triage","path":1e999}`, `<skill_content name="rfi-triage">`},
		{"number before the name", `{"depth":-1e999,"name":"rfi-triage"}`, `<skill_content name="rfi-triage">`},
		{"number nested in another key", `{"name":"rfi-triage","extra":{"n":1e999}}`, `<skill_content name="rfi-triage">`},
	} {
		t.Run(tcase.name, func(t *testing.T) {
			args, err := kernel.NewArguments([]byte(tcase.args))
			if err != nil {
				t.Fatalf("arguments: %v", err)
			}
			out, err := skills.NewViewTool().Execute(t.Context(), args, tc)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if out.IsError != nil && *out.IsError {
				t.Fatalf("output is an error: %q", out.Content)
			}
			if !strings.Contains(out.Content, tcase.want) {
				t.Errorf("content = %q, want it to contain %q", out.Content, tcase.want)
			}
		})
	}
}
