package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/artifacts"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/clarify"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/skills"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/web"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/workspace"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file compares the definitions of all 16 built-in tools with the recorded fixture
// builtin-tool-definitions (recorded by tools/wiregen/builtin/definitions.cases.mjs). The handlers are built through the root
// aliases (aec.New..., workspace.NewTools, artifacts.NewTools), so the test also
// proves the facade reaches the real tools. A mismatch in any description, schema bound or
// key order fails here, except the one allow-listed difference: the key order of the files
// property of skill_manage.

// definitionOptions are the constructor options the recorder used, with camelCase keys. A
// key that is absent is zero, the default.
type definitionOptions struct {
	MaxReadBytes          int `json:"maxReadBytes"`
	MaxDisplayOutputBytes int `json:"maxDisplayOutputBytes"`
	DefaultTimeoutMs      int `json:"defaultTimeoutMs"`
	MaxTimeoutMs          int `json:"maxTimeoutMs"`
	MaxListEntries        int `json:"maxListEntries"`
	MaxSearchMatches      int `json:"maxSearchMatches"`
	DefaultMaxResults     int `json:"defaultMaxResults"`
	MaxResultsLimit       int `json:"maxResultsLimit"`
	MaxUrls               int `json:"maxUrls"`
	MaxCharsLimit         int `json:"maxCharsLimit"`
	DefaultMaxChars       int `json:"defaultMaxChars"`
}

func (o definitionOptions) workspace() workspace.Options {
	return workspace.Options{
		MaxReadBytes:          o.MaxReadBytes,
		MaxDisplayOutputBytes: o.MaxDisplayOutputBytes,
		DefaultTimeoutMs:      o.DefaultTimeoutMs,
		MaxTimeoutMs:          o.MaxTimeoutMs,
		MaxListEntries:        o.MaxListEntries,
		MaxSearchMatches:      o.MaxSearchMatches,
	}
}

func (o definitionOptions) artifacts() artifacts.Options {
	return artifacts.Options{MaxReadBytes: o.MaxReadBytes, MaxListEntries: o.MaxListEntries}
}

// definitionRow is one recorded handler: the tool name the model sees, the handler name and
// version, the option variant (nil for the default options) and the recorded definition.
type definitionRow struct {
	Tool       string             `json:"tool"`
	Name       string             `json:"name"`
	Version    string             `json:"version"`
	Options    *definitionOptions `json:"options"`
	Definition json.RawMessage    `json:"definition"`
}

type definitionFixture struct {
	Tools []definitionRow `json:"tools"`
}

// goHandlers builds the Go handlers of one option variant through the root aliases and
// returns them keyed by tool name. The workspace and artifact tools come from the factories
// (workspace.NewTools, artifacts.NewTools) and from the per-tool constructors, and
// both must agree.
func goHandlers(t *testing.T, o definitionOptions) map[string]toolhub.Handler {
	t.Helper()
	byName := map[string]toolhub.Handler{}
	add := func(h toolhub.Handler) {
		t.Helper()
		name := h.Definition().Name
		if _, dup := byName[name]; dup {
			t.Fatalf("two handlers build the tool %s", name)
		}
		byName[name] = h
	}
	add(web.NewSearchTool(web.SearchToolOptions{DefaultMaxResults: o.DefaultMaxResults, MaxResultsLimit: o.MaxResultsLimit}))
	add(web.NewExtractTool(web.ExtractToolOptions{MaxUrls: o.MaxUrls, MaxCharsLimit: o.MaxCharsLimit, DefaultMaxChars: o.DefaultMaxChars}))
	add(clarify.NewTool())
	add(skills.NewViewTool())
	add(skills.NewManageTool())

	ws := o.workspace()
	factory := workspace.NewTools(ws)
	for _, h := range factory {
		add(h)
	}
	for _, h := range []toolhub.Handler{
		workspace.NewReadFileTool(ws), workspace.NewWriteFileTool(ws), workspace.NewEditFileTool(ws), workspace.NewListFilesTool(ws),
		workspace.NewSearchFilesTool(ws), workspace.NewTerminalTool(ws), workspace.NewRunCodeTool(ws),
	} {
		if got, want := encodeDefinition(t, h), encodeDefinition(t, byName[h.Definition().Name]); got != want {
			t.Errorf("the constructor and the factory build different %s definitions\n ctor: %s\n fact: %s", h.Definition().Name, got, want)
		}
	}

	art := o.artifacts()
	for _, h := range artifacts.NewTools(art) {
		add(h)
	}
	for _, h := range []toolhub.Handler{
		artifacts.NewSaveTool(art), artifacts.NewUpdateTool(art), artifacts.NewReadTool(art), artifacts.NewListTool(art),
	} {
		if got, want := encodeDefinition(t, h), encodeDefinition(t, byName[h.Definition().Name]); got != want {
			t.Errorf("the constructor and the factory build different %s definitions\n ctor: %s\n fact: %s", h.Definition().Name, got, want)
		}
	}
	return byName
}

func encodeDefinition(t *testing.T, h toolhub.Handler) string {
	t.Helper()
	b, err := kernel.EncodeJSON(h.Definition())
	if err != nil {
		t.Fatalf("encode the definition of %s: %v", h.Definition().Name, err)
	}
	return string(b)
}

// The files property of skill_manage: the fixture has type, description, items; Go writes the
// field order of toolhub.JSONSchema (type, properties, required, items, enum, description),
// so the property is type, items, description. See builtin/skills TestSkillManageDefinitionDivergence.
const (
	manageFilesDescription = `"description":"create/rewrite: extra files of the skill"`
	manageFilesPrefixTS    = `"files":{"type":"array",` + manageFilesDescription + `,"items":`
	manageFilesPrefixGo    = `"files":{"type":"array","items":`
)

// goKeyOrderOfSkillManage rewrites the recorded definition of skill_manage into the Go key
// order of the files property, the one allowed difference.
func goKeyOrderOfSkillManage(t *testing.T, ts string) string {
	t.Helper()
	i := strings.Index(ts, manageFilesPrefixTS)
	if i < 0 {
		t.Fatalf("the recorded skill_manage definition has no files property in the TS key order:\n%s", ts)
	}
	rest := ts[i+len(manageFilesPrefixTS):]
	var items json.RawMessage
	if err := json.NewDecoder(strings.NewReader(rest)).Decode(&items); err != nil {
		t.Fatalf("decode the recorded items of the files property: %v", err)
	}
	tail := rest[len(items):]
	if !strings.HasPrefix(tail, "}") {
		t.Fatalf("the recorded files property does not end after items: %q", tail)
	}
	return ts[:i] + manageFilesPrefixGo + string(items) + "," + manageFilesDescription + tail
}

// TestBuiltinToolDefinitions replays builtin-tool-definitions.json: for every recorded
// handler (the 16 built-in tools with their default options and every option variant that
// changes a description) the Go handler built through the root aliases has the same name,
// version and definition bytes. skill_manage is the single allow-listed difference, and the
// test asserts it is present, so a fix or a new mismatch fails.
func TestBuiltinToolDefinitions(t *testing.T) {
	var fx definitionFixture
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-tool-definitions.json")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-tool-definitions.json: %v", err)
	}

	tools := map[string]bool{}
	defaults := 0
	manageChecked := 0
	cache := map[string]map[string]toolhub.Handler{}
	for i, row := range fx.Tools {
		tools[row.Tool] = true
		var opts definitionOptions
		variant := "default options"
		if row.Options == nil {
			defaults++
		} else {
			opts = *row.Options
			variant = fmt.Sprintf("options %+v", opts)
		}
		t.Run(fmt.Sprintf("%02d_%s", i, row.Tool), func(t *testing.T) {
			key := fmt.Sprintf("%+v", opts)
			handlers, ok := cache[key]
			if !ok {
				handlers = goHandlers(t, opts)
				cache[key] = handlers
			}
			h, ok := handlers[row.Tool]
			if !ok {
				t.Fatalf("the Go side builds no tool %s", row.Tool)
			}
			if h.Name() != row.Name || h.Version() != row.Version {
				t.Errorf("%s (%s): handler is %s@%s, TS is %s@%s", row.Tool, variant, h.Name(), h.Version(), row.Name, row.Version)
			}
			got := encodeDefinition(t, h)
			want := string(row.Definition)
			if row.Tool == "skill_manage" {
				manageChecked++
				if got == want {
					t.Fatal("the Go skill_manage definition equals the TS one byte for byte: the files key order divergence is gone, so the ledger row and this allow-list are stale")
				}
				want = goKeyOrderOfSkillManage(t, want)
			}
			if got != want {
				t.Errorf("%s (%s) differs from the TS definition\n got: %s\nwant: %s", row.Tool, variant, got, want)
			}
		})
	}

	wantTools := []string{
		"web_search", "web_extract", "clarify", "read_file", "write_file", "edit_file", "list_files", "search_files",
		"terminal", "run_code", "save_artifact", "update_artifact", "read_artifact", "list_artifacts", "skill_view", "skill_manage",
	}
	for _, name := range wantTools {
		if !tools[name] {
			t.Errorf("the fixture records no definition of %s", name)
		}
	}
	if len(tools) != len(wantTools) {
		got := make([]string, 0, len(tools))
		for name := range tools {
			got = append(got, name)
		}
		slices.Sort(got)
		t.Errorf("the fixture records %d tools %v, want the 16 built-in tools", len(tools), got)
	}
	if defaults != 16 {
		t.Errorf("the fixture records %d default-option rows, want one per tool (16)", defaults)
	}
	if manageChecked == 0 {
		t.Error("the skill_manage allow-listed difference was never asserted")
	}

	// The name lists of the facade follow the factories.
	if got, want := workspace.ToolNames(), []string{"read_file", "write_file", "edit_file", "list_files", "search_files", "terminal", "run_code"}; !slices.Equal(got, want) {
		t.Errorf("workspace.ToolNames() = %v, want %v", got, want)
	}
	if got, want := artifacts.ToolNames(), []string{"save_artifact", "update_artifact", "read_artifact", "list_artifacts"}; !slices.Equal(got, want) {
		t.Errorf("artifacts.ToolNames() = %v, want %v", got, want)
	}
}
