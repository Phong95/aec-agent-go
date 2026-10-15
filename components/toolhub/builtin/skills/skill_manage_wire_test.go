package skills_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/skills"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// builtin-skill-manage.json is recorded by wiregen from the reference SkillManageTool over a
// real SkillManager, a real SkillHub with the in-memory skill store and the SDK's real
// in-memory sandbox (see tools/wiregen/builtin/skill_manage.cases.mjs). Each case carries
// the library it started from, the agent's allowlist, the calls and, for every call, the
// output the reference tool returned; then the library, the allowlist and the changes the manager
// handed to its hook, the writeFile and exec calls the workspace sync made, and the files
// the workspace held afterwards. This replay rebuilds the same environment on the Go
// agent.SkillManager over a real skillhub.SkillHub and aecmock's in-memory sandbox, runs the Go
// tool and compares everything.

type manageSetup struct {
	Op        string       `json:"op"`
	Source    string       `json:"source"`
	Content   string       `json:"content"`
	Files     []skill.File `json:"files"`
	CreatedBy string       `json:"createdBy"`
}

// projected is a skill as the recorder writes it: no timestamps, because the hub stamps a
// custom skill with the clock.
type projected struct {
	Name      string       `json:"name"`
	Source    string       `json:"source"`
	Version   int          `json:"version"`
	Content   string       `json:"content"`
	Files     []skill.File `json:"files"`
	CreatedBy string       `json:"createdBy"`
}

type manageCase struct {
	Name       string            `json:"name"`
	Setup      []manageSetup     `json:"setup"`
	Allowlist  []string          `json:"allowlist"`
	Sandbox    *fixtureSandbox   `json:"sandbox"`
	StoreFails *manageStoreFails `json:"storeFails"`
	NoSkills   bool              `json:"noSkills"`
	Calls      []json.RawMessage `json:"calls"`
	Outputs    []json.RawMessage `json:"outputs"`
	Skills     []projected       `json:"skills"`

	AllowlistAfter []string     `json:"allowlistAfter"`
	Changes        [][]string   `json:"changes"`
	Writes         []string     `json:"writes"`
	Execs          []execRecord `json:"execs"`
	Files          *[][2]string `json:"files"`
}

type manageStoreFails struct {
	Op      string `json:"op"`
	Message string `json:"message"`
}

type manageFixture struct {
	Tool struct {
		Name       string          `json:"name"`
		Version    string          `json:"version"`
		Definition json.RawMessage `json:"definition"`
	} `json:"tool"`
	Cases []manageCase `json:"cases"`
}

func loadManageFixture(t *testing.T) manageFixture {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-skill-manage.json")))
	dec.DisallowUnknownFields()
	var fx manageFixture
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-skill-manage: %v", err)
	}
	return fx
}

// failingStore is a skill store whose one operation (save or delete) fails with a plain
// error, like the recorder's store.
type failingStore struct {
	persistence.SkillStore
	op, message string
}

func (s failingStore) Save(ctx context.Context, r persistence.SkillRecord) error {
	if s.op == "save" {
		return errors.New(s.message)
	}
	return s.SkillStore.Save(ctx, r)
}

func (s failingStore) Delete(ctx context.Context, name string) error {
	if s.op == "delete" {
		return errors.New(s.message)
	}
	return s.SkillStore.Delete(ctx, name)
}

// world is the library and the agent of one case.
type world struct {
	hub     *skillhub.SkillHub
	manager *agent.SkillManager
	changes [][]string
	after   []string
}

// newWorld builds the library of a case (custom skills through the hub, app and built-in
// skills registered) on a store that fails the recorded operation after the setup, and the
// agent a1's SkillManager over it. The manager's hook records every list and applies it,
// as AgentCore does.
func newWorld(t *testing.T, c manageCase) *world {
	t.Helper()
	var store persistence.SkillStore = persistence.NewMemorySkillStore()
	w := &world{after: slices.Clone(c.Allowlist)}
	failing := false
	hub := skillhub.NewSkillHub(func() persistence.SkillStore {
		if failing && c.StoreFails != nil {
			return failingStore{SkillStore: store, op: c.StoreFails.Op, message: c.StoreFails.Message}
		}
		return store
	})
	for _, s := range c.Setup {
		pkg := skill.Package{Content: s.Content, Files: s.Files}
		switch s.Op {
		case "create":
			if _, err := hub.Create(t.Context(), pkg, skillhub.CreateOptions{CreatedBy: s.CreatedBy}); err != nil {
				t.Fatalf("setup create: %v", err)
			}
		case "register":
			if _, err := hub.Register(pkg, skill.Source(s.Source)); err != nil {
				t.Fatalf("setup register: %v", err)
			}
		default:
			t.Fatalf("unknown setup op %q", s.Op)
		}
	}
	failing = true
	w.hub = hub
	w.manager = agent.NewSkillManager(hub, profile.AgentProfile{ID: "a1", Skills: c.Allowlist},
		agent.WithOnAllowlistChange(func(_ context.Context, names []string) error {
			w.changes = append(w.changes, slices.Clone(names))
			w.after = slices.Clone(names)
			w.manager.SetAllowedSkills(names)
			return nil
		}))
	return w
}

// projection lists the library as the recorder does: every skill without its timestamps,
// sorted by name, with a non-nil file list.
func (w *world) projection(t *testing.T) []projected {
	t.Helper()
	all, err := w.hub.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := make([]projected, 0, len(all))
	for _, s := range all {
		files := s.Files
		if files == nil {
			files = []skill.File{}
		}
		out = append(out, projected{Name: s.Name, Source: string(s.Source), Version: s.Version, Content: s.Content, Files: files, CreatedBy: s.CreatedBy})
	}
	slices.SortFunc(out, func(a, b projected) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func normalizedFiles(p []projected) []projected {
	out := slices.Clone(p)
	for i := range out {
		if out[i].Files == nil {
			out[i].Files = []skill.File{}
		}
	}
	return out
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// TestSkillManageMatchesGolden replays builtin-skill-manage.json: the tool's name, version and
// definition (the files key order aside, see TestSkillManageDefinitionDivergence), and for
// every case the output of every call, the resulting library, the allowlist and its
// changes, and the write, exec and file effects in the sandbox.
func TestSkillManageMatchesGolden(t *testing.T) {
	fx := loadManageFixture(t)
	if len(fx.Cases) < 150 {
		t.Fatalf("the corpus holds %d cases, want at least 150", len(fx.Cases))
	}

	handler := skills.NewManageTool()
	if handler.Name() != fx.Tool.Name || handler.Version() != fx.Tool.Version {
		t.Errorf("tool = %s %s, want the TS %s %s", handler.Name(), handler.Version(), fx.Tool.Name, fx.Tool.Version)
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			w := newWorld(t, c)
			env := newEnvironmentFor(t, c.Sandbox, "a1")
			tc := toolhub.Context{SessionID: "s1", AgentID: "a1", Emit: func(string, any) {}, GetSandbox: env.getSandbox}
			if !c.NoSkills {
				tc.Skills = w.manager
			}
			if len(c.Calls) != len(c.Outputs) {
				t.Fatalf("the case has %d calls and %d outputs", len(c.Calls), len(c.Outputs))
			}

			if want, ok := q29DivergentCases[c.Name]; ok {
				// The untrimmed argument is applied as sent, so the outcome differs from the
				// recorded one by design.
				for i, raw := range c.Calls {
					args, err := kernel.NewArguments(raw)
					if err != nil {
						t.Fatalf("arguments of call %d: %v", i, err)
					}
					out, err := handler.Execute(t.Context(), args, tc)
					if err != nil {
						t.Fatalf("call %d: %v", i, err)
					}
					got, _ := kernel.EncodeJSON(out)
					if string(got) == string(c.Outputs[i]) {
						t.Fatalf("call %d: the Go output equals the TS one now; drop the case from q29DivergentCases", i)
					}
					if string(got) != want[i] {
						t.Errorf("output of call %d (Q29 Go divergence)\n got: %s\nwant: %s", i, got, want[i])
					}
				}
				return
			}

			for i, raw := range c.Calls {
				raw = q29TSInput(t, raw)
				args, err := kernel.NewArguments(raw)
				if err != nil {
					t.Fatalf("arguments of call %d: %v", i, err)
				}
				out, err := handler.Execute(t.Context(), args, tc)
				if err != nil {
					t.Fatalf("call %d: Execute returned an error (every failure must be an output): %v", i, err)
				}
				got, err := kernel.EncodeJSON(out)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(c.Outputs[i]) {
					t.Errorf("output of call %d (%s) differs from TS\n got: %s\nwant: %s", i, raw, got, c.Outputs[i])
				}
			}

			if got := w.projection(t); !reflect.DeepEqual(got, normalizedFiles(c.Skills)) {
				t.Errorf("the library differs from TS\n got: %+v\nwant: %+v", got, c.Skills)
			}
			if !slices.Equal(w.after, c.AllowlistAfter) {
				t.Errorf("allowlist = %v, want %v", w.after, c.AllowlistAfter)
			}
			if !reflect.DeepEqual(nonNil(w.changes), nonNil(c.Changes)) {
				t.Errorf("allowlist changes = %v, want %v", w.changes, c.Changes)
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

// q29DivergentCases are the recorded cases whose point is a trimmed patch old_string: the
// tool applies old_string as sent (the full create/rewrite content is still trimmed,
// because a SKILL.md must open with its frontmatter). Each holds the expected output
// of every call: an old_string with surrounding spaces that the file does not hold is not
// found.
var q29DivergentCases = map[string][]string{
	"patch trims the old string": {`{"content":"skill_manage patch: old_string was not found in SKILL.md","isError":true,"retryable":false}`},
}

// q29TSInput rewrites a recorded call to the arguments the reference tool applied: it
// trimmed the patch "old_string", which this tool reads as sent; both trim the
// create/rewrite "content".
// Replaying the trimmed values keeps every other recorded effect (outputs, library, sync
// writes, files) byte-for-byte comparable; the behaviour itself is pinned by
// TestSkillManageTrimQuirk and q29DivergentCases.
func q29TSInput(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	var action string
	if json.Unmarshal(obj["action"], &action) != nil {
		return raw
	}
	key := ""
	switch jsstr.Trim(action) {
	case "patch":
		key = "old_string"
	}
	var v string
	if key == "" || json.Unmarshal(obj[key], &v) != nil || jsstr.Trim(v) == v {
		return raw
	}
	enc, err := json.Marshal(jsstr.Trim(v))
	if err != nil {
		t.Fatal(err)
	}
	obj[key] = enc
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// filesProperty is the recorded encoding of the files property of skill_manage, and
// filesPropertyGo the encoding this package produces: the field order of toolhub.JSONSchema
// is type, properties, required, items, enum, description for every tool, so the property
// is type, items, description, while the recording has type, description, items.
const (
	filesDescription = `"description":"create/rewrite: extra files of the skill"`
	filesPrefixTS    = `"files":{"type":"array",` + filesDescription + `,"items":`
	filesPrefixGo    = `"files":{"type":"array","items":`
)

// TestSkillManageDefinitionDivergence asserts that the definition equals the recorded
// definition except for the key order of the files property, which is the one expected
// difference.
func TestSkillManageDefinitionDivergence(t *testing.T) {
	fx := loadManageFixture(t)
	got, err := kernel.EncodeJSON(skills.NewManageTool().Definition())
	if err != nil {
		t.Fatal(err)
	}
	want := string(fx.Tool.Definition)

	if string(got) == want {
		t.Fatal("the Go definition equals the TS one byte for byte: the files key order divergence is gone, so the ledger row and this test are stale")
	}
	i := strings.Index(want, filesPrefixTS)
	if i < 0 {
		t.Fatalf("the recorded definition has no files property in the TS key order:\n%s", want)
	}
	rest := want[i+len(filesPrefixTS):]
	dec := json.NewDecoder(strings.NewReader(rest))
	var items json.RawMessage
	if err := dec.Decode(&items); err != nil {
		t.Fatalf("decode the recorded items: %v", err)
	}
	// After the items value comes the closing brace of the files property.
	tail := rest[len(items):]
	if !strings.HasPrefix(tail, "}") {
		t.Fatalf("the recorded files property does not end after items: %q", tail)
	}
	rebuilt := want[:i] + filesPrefixGo + string(items) + "," + filesDescription + tail
	if string(got) != rebuilt {
		t.Errorf("the definition differs from TS in more than the files key order\n got: %s\nwant: %s", got, rebuilt)
	}
}

// Every recorded branch must have been exercised: a recorder that stopped covering one
// cannot go unnoticed. The SkillError codes not_found and not_allowed of the manager are not
// reachable through the tool (Get hides a skill that is not enabled, so the tool says the
// agent has no such skill before the manager is asked to change it).
func TestSkillManageCorpusCoversEveryBranch(t *testing.T) {
	fx := loadManageFixture(t)
	counts := map[string]int{}
	for _, c := range fx.Cases {
		for _, raw := range c.Outputs {
			out := string(raw)
			for needle, key := range map[string]string{
				`Created skill`:                    "created",
				`Rewrote skill`:                    "rewrote",
				`Patched `:                         "patched",
				`Added `:                           "added",
				`Replaced `:                        "replaced",
				`Removed `:                         "removed",
				`Deleted skill`:                    "deleted",
				`(read_only)`:                      "read_only",
				`(invalid)`:                        "invalid",
				`(already_exists)`:                 "already_exists",
				`you have no skill named`:          "not_enabled",
				`old_string was not found`:         "not_found_string",
				`old_string occurs more than once`: "more_than_once",
				`could not be synced`:              "sync_failed",
				`the workspace is a host folder`:   "host_folder",
				`skills are not available here`:    "no_skills",
				`named from the frontmatter`:       "named_from",
				`\"files\" must be a list`:         "files_not_list",
				`every entry of \"files\" needs`:   "files_bad_entry",
				`failed: the store is down`:        "store_fails",
				`\"action\" must be one of`:        "bad_action",
				`invalid path`:                     "invalid_path",
			} {
				if strings.Contains(out, needle) {
					counts[key]++
				}
			}
		}
	}
	for _, key := range []string{
		"created", "rewrote", "patched", "added", "replaced", "removed", "deleted", "read_only", "invalid", "already_exists",
		"not_enabled", "not_found_string", "more_than_once", "sync_failed", "host_folder", "no_skills",
		"named_from", "files_not_list", "files_bad_entry", "store_fails", "bad_action", "invalid_path",
	} {
		if counts[key] == 0 {
			t.Errorf("no recorded output exercises %q", key)
		}
	}
}
