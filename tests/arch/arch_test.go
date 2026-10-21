package arch_test

// Architecture guard (FND-02). It runs `go list` over the whole module and
// checks the direct-import graph against a layer table, so a future change that
// erodes the layering (for example kernel importing a persistence package)
// fails the build instead of surviving review. The test is stdlib only.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const archModule = "github.com/Phong95/aec-agent-go"

// pkgInfo is the subset of `go list -json` output the layer check needs.
type pkgInfo struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// listPackages lists every package of the module with its direct imports. goos
// overrides GOOS when non-empty, so a Windows-only file cannot smuggle an
// import past a Linux run.
func listPackages(t *testing.T, goos string) []pkgInfo {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-json=ImportPath,Imports,TestImports,XTestImports", "./...")
	if goos != "" {
		cmd.Env = append(os.Environ(), "GOOS="+goos)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list (GOOS=%q): %v\n%s", goos, err, stderr.String())
	}
	var pkgs []pkgInfo
	// go list prints concatenated objects, not a JSON array.
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// layerTable maps a package path relative to the module ("." is the root) to
// the module-internal packages its non-test code may import. A package missing
// from the table fails the test, which forces every later phase to declare the
// layer of the packages it adds.
var layerTable = map[string][]string{
	"internal/jsonx":      {},
	"internal/wiretest":   {"internal/jsonx"},
	"kernel/ordered":      {"internal/jsonx"},
	"internal/clonecheck": {"kernel/ordered", "internal/jsonx"},
	// A-124: the shared fs.stream suite (a non-test file importing testing, like clonecheck).
	"internal/streamcheck":  {"kernel", "components/sandboxhub"},
	"internal/servicecheck": {"kernel", "components/sandboxhub"}, // A-124 phase 2: the shared "services" suite
	"internal/nilcheck":     {},
	"internal/jsstr":        {},
	"internal/viewkit":      {"internal/jsstr"},
	"internal/llmtransport": {},
	"internal/redact":       {},
	"kernel":                {"internal/jsonx", "internal/nilcheck", "kernel/ordered"},
	// Phase 12 (12-02): events, agent and memorysystem log a recovered panic through
	// internal/redact.Panic (10-SECURITY UF-1).
	"components/events":                    {"kernel", "internal/jsonx", "internal/redact"},
	"components/trace":                     {"kernel", "internal/jsonx", "kernel/ordered"},
	"components/sandboxhub":                {"kernel", "components/events", "internal/jsonx", "internal/jsstr", "internal/nilcheck", "kernel/ordered"},
	"components/sandboxhub/host":           {"kernel", "components/sandboxhub", "internal/jsstr", "internal/viewkit"},
	"components/sandboxhub/docker":         {"kernel", "components/sandboxhub", "internal/jsonx", "internal/jsstr", "internal/viewkit"},
	"components/agent/memorysystem/memory": {"kernel", "internal/jsonx"},
	"components/skillhub/skill":            {"kernel", "kernel/ordered", "internal/jsonx", "internal/jsstr"},
	"components/artifacthub/artifact":      {"components/sandboxhub", "kernel", "internal/jsonx"},
	// A-69: promptjob is the type-only contract of scheduled prompt jobs (Job, Run,
	// Schedule, Runner, Jobs and its Error), shared by persistence, toolhub and agent.
	// Update (A-126): merged into components/agent/job below.
	// A-100: channel is the contract of chat-app channels (Channel, Adapter, Platform,
	// TurnRunner, Channels, its Error and the text helpers), shared by persistence,
	// channelplatformhub, agent and the root. channelplatformhub is the hub AgentCore owns: the registered
	// platforms and which bot each channel holds. channelplatformhub/telegram is the opt-in
	// Telegram adapter (net/http to the Bot API), outside the host-access guarded
	// components/agent tree, like the LLM providers under llmhub/providers.
	"components/channelplatformhub/channel": {"kernel"},
	// A-70: trigger is the contract of event triggers (Trigger, Event, Run, Runner,
	// Triggers, its Error, the filter, the prompt and the webhook signatures), shared by
	// persistence, agent, conformance and the root. agent/internal/triggerkit holds the
	// JSON value helpers trigger and agent share (paths, subject / revision, JS equality).
	// Update (A-126): job is the contract of jobs of every kind (Job, Event, Run, Runner,
	// Jobs, its Error, the filter, the prompt, the reply markers and the webhook
	// signatures) - promptjob and trigger merged; agent/internal/jobkit is triggerkit.
	"components/agent/internal/jobkit": {"internal/jsonx", "internal/jsstr", "kernel/ordered"},
	"components/agent/job":             {"kernel", "kernel/ordered", "internal/jsonx", "internal/jsstr", "components/agent/internal/jobkit"},
	// A-105: subagent is the contract of subagents (Config, Request, Run, Runner, Subagents,
	// its Error and the brief helpers), shared by profile, persistence, toolhub, agentloop,
	// agent, conformance and the root.
	"components/agent/subagent": {"kernel", "kernel/ordered", "internal/jsonx", "internal/jsstr"},
	// A-136: the slash-command contracts (a type-only leaf); the agent's command manager
	// implements them, and the root's AgentCore.UseCommand takes them.
	"components/agent/command": {"kernel", "kernel/ordered"},
	// A-105 revision: subagenttype is the contract of subagent types (Definition, Type,
	// Source, the limits), shared by the subagenthub library and its store
	// (persistence.SubagentTypeStore); subagenthub is the library AgentCore owns.
	"components/subagenthub/subagenttype": {"components/agent/subagent"},
	// A-101: secret is the contract of secrets (Definition, Info, Stored, Use, Config,
	// Secrets, its Error and the rules: names, hosts, placeholders, redaction), shared by
	// persistence, profile, toolhub, agentloop, agent, the built-in tools and the root;
	// secrethub is the hub AgentCore owns (sealing, SecretHub providers, CRUD, reveal, rotation).
	"components/secrethub/secret":            {"kernel", "internal/jsonx", "internal/jsstr"},
	"components/secrethub":                   {"kernel", "components/secrethub/secret", "components/persistence", "components/events", "internal/jsstr", "internal/nilcheck"},
	"components/subagenthub":                 {"kernel", "components/subagenthub/subagenttype", "components/agent/subagent", "components/persistence", "components/events", "internal/jsstr", "internal/nilcheck"},
	"components/channelplatformhub":          {"kernel", "components/channelplatformhub/channel"},
	"components/channelplatformhub/telegram": {"components/channelplatformhub/channel", "internal/jsonx"},
	// A-100 phase 3: the Slack (Socket Mode), Discord (Gateway) and Microsoft Teams (Bot
	// Framework webhook) adapters, opt-in siblings of channelplatformhub/telegram, and the small
	// helpers they share (adapterkit: sleep, backoff, the API error, Retry-After).
	"components/channelplatformhub/internal/adapterkit": {},
	"components/channelplatformhub/slack":               {"components/channelplatformhub/channel", "components/channelplatformhub/internal/adapterkit", "internal/jsonx"},
	"components/channelplatformhub/discord":             {"components/channelplatformhub/channel", "components/channelplatformhub/internal/adapterkit", "internal/jsonx"},
	"components/channelplatformhub/teams":               {"components/channelplatformhub/channel", "components/channelplatformhub/internal/adapterkit", "internal/jsonx"},
	"components/agent/profile":                          {"components/sandboxhub", "components/agent/memorysystem/memory", "kernel", "internal/jsonx", "components/agent/subagent", "components/secrethub/secret"},                                                                   // A-105: AgentProfile.Subagents; A-101: AgentProfile.Secrets
	"components/toolhub":                                {"kernel", "components/sandboxhub", "components/artifacthub/artifact", "components/skillhub/skill", "internal/jsonx", "kernel/ordered", "components/agent/job", "components/agent/subagent", "components/secrethub/secret"}, // Update (A-70 agent tools): toolhub.Context.Triggers is a trigger.Triggers; A-105: toolhub.Context.Subagents; A-101: toolhub.Context.Secrets
	"components/llmhub":                                 {"kernel", "components/toolhub", "internal/jsonx", "internal/jsstr", "internal/viewkit", "internal/redact", "kernel/ordered"},
	"components/persistence":                            {"kernel", "components/agent/profile", "components/agent/memorysystem/memory", "components/artifacthub/artifact", "components/skillhub/skill", "components/llmhub", "kernel/ordered", "internal/jsonx", "internal/nilcheck", "internal/redact", "components/channelplatformhub/channel", "components/agent/job", "components/agent/subagent", "components/subagenthub/subagenttype", "components/secrethub/secret"}, // A-105: SubagentRunStore; A-105 revision: SubagentTypeStore; A-101: SecretStore
	"components/skillhub":                               {"kernel", "components/skillhub/skill", "components/persistence", "components/events", "kernel/ordered", "internal/nilcheck"},
	"components/artifacthub":                            {"kernel", "components/artifacthub/artifact", "components/sandboxhub", "components/persistence", "components/events", "internal/jsstr", "internal/nilcheck"},
	// Phase 9: agentloop is the OTAR loop. It talks to the rest of the SDK only
	// through its consumer-side ports, so it imports neither persistence nor a
	// concrete tool hub; aecmock and conformance sit above it.
	"components/agent/agentloop": {"kernel", "internal/redact", "components/events", "components/trace", "components/toolhub", "components/llmhub", "components/agent/profile", "components/agent/memorysystem/memory", "components/sandboxhub", "components/artifacthub/artifact", "components/skillhub/skill", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/nilcheck", "components/agent/job", "components/agent/subagent", "components/secrethub/secret"}, // Update (A-70 agent tools): agentloop.Deps.Triggers, handed to the tools; A-105: agentloop.Deps.Subagents; A-101: agentloop.Deps.Secrets
	"aecmock":                    {"components/channelplatformhub/channel", "kernel", "components/toolhub", "components/llmhub", "components/persistence", "components/sandboxhub", "components/agent/agentloop", "components/agent/profile", "components/agent/memorysystem/memory", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/viewkit"},
	"conformance":                {"kernel", "components/events", "components/toolhub", "components/llmhub", "components/persistence", "components/agent/memorysystem/memory", "components/artifacthub/artifact", "components/skillhub/skill", "components/agent/profile", "components/sandboxhub", "kernel/ordered", "components/agent/agentloop", "components/trace", "conformance/internal/check", "components/channelplatformhub/channel", "components/agent/job", "components/agent/subagent", "components/secrethub/secret"},
	"conformance/internal/check": {},
	// Phase 8: the four provider leaves, each registering itself in init(), and
	// llm/all, the one blank-import package that links them. A leaf may import
	// only its own vendor module (vendorAllowed); a plan that needs a wider row
	// widens it with a stated reason.
	"components/llmhub/providers/anthropic":    {"kernel", "components/llmhub", "components/toolhub", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/llmtransport"},
	"components/llmhub/providers/openaicompat": {"kernel", "components/llmhub", "components/toolhub", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/llmtransport"},
	"components/llmhub/providers/bedrock":      {"kernel", "components/llmhub", "components/toolhub", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/llmtransport"},
	"components/llmhub/providers/openaisub":    {"kernel", "components/llmhub", "components/toolhub", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/llmtransport"},
	"components/llmhub/providers/all":          {"components/llmhub/providers/anthropic", "components/llmhub/providers/openaicompat", "components/llmhub/providers/bedrock", "components/llmhub/providers/openaisub"},
	"internal/llmtest":                         {"kernel", "internal/jsonx"},
	// Phase 10: agent is the Agent and its managers (L7) and
	// orchestration is the registry of agents (L8). agent is the first package that
	// joins the loop with the hubs, so its row is the widest; it never imports the
	// root, aecmock or conformance. The row also names the packages later plans of
	// the phase add (memorysystem, builtin/skills); an unused entry is harmless.
	"components/agent": {
		"components/agent/agentloop", "components/agent/memorysystem", "components/persistence", "components/skillhub", "components/artifacthub", "components/sandboxhub", "components/toolhub", "components/llmhub", "components/trace", "components/events",
		"components/agent/profile", "components/skillhub/skill", "components/artifacthub/artifact", "components/agent/memorysystem/memory", "kernel", "kernel/ordered",
		"internal/jsonx", "internal/jsstr", "internal/nilcheck", "internal/redact", "components/toolhub/builtin/skills",
		// A-69: the prompt-job contracts the agent's scheduler implements.
		// Update (A-126): the job contracts, below.
		// A-100: the channel contracts the agent's channel manager implements, and the
		// hub it builds adapters from.
		"components/channelplatformhub/channel", "components/channelplatformhub",
		// A-70: the trigger contracts the agent's trigger manager implements, and their
		// shared JSON helpers.
		// Update (A-126): the job contracts the agent's job manager implements.
		"components/agent/job", "components/agent/internal/jobkit",
		// A-105: the subagent contracts the agent's subagent manager implements.
		"components/agent/subagent",
		// A-105 revision: the library of subagent types (Deps.SubagentHub) and their contract.
		"components/subagenthub", "components/subagenthub/subagenttype",
		// A-101: the secret hub (Deps.SecretHub) and its contract, which the agent's secret
		// manager implements.
		"components/secrethub", "components/secrethub/secret",
		// A-136: the slash-command contracts the agent's command manager implements.
		"components/agent/command",
	},
	"components/orchestration": {"components/agent", "components/agent/profile", "kernel"},
	// Phase 10 (10-04): builtin/skills holds skill_view and the workspace copy of a skill.
	// It reaches the agent's files only through sandboxhub.View, so it is also host-access
	// guarded (hostAccessGuarded); the agent package imports it for its SkillManager.
	"components/toolhub/builtin/skills": {"kernel", "components/toolhub", "components/skillhub/skill", "components/sandboxhub", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/toolkit"},
	// Phase 11: the shared plumbing of the built-in tool packages and the packages
	// that hold the built-in tools. internal/toolkit holds the argument decoding and
	// the output builders (11-01); builtin/clarify has no I/O of its own and reaches the
	// user only through toolhub.Context.Interaction (11-01). The rows of builtin/workspace,
	// builtin/artifacts and builtin/web are declared here too, for the plans of the phase
	// that add them (an unused row is harmless; a plan that needs one more entry widens
	// its row with a stated reason). All four builtin packages and internal/toolkit are
	// host-access guarded (hostAccessGuarded).
	"internal/toolkit":                     {"kernel", "components/toolhub", "components/sandboxhub", "kernel/ordered", "internal/jsonx", "internal/jsstr"},
	"components/toolhub/builtin/clarify":   {"kernel", "components/toolhub", "kernel/ordered", "internal/jsstr", "internal/nilcheck", "internal/toolkit"},
	"components/toolhub/builtin/workspace": {"kernel", "components/toolhub", "components/sandboxhub", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/nilcheck", "internal/toolkit", "components/secrethub/secret"}, // A-101: the secrets parameter of terminal / run_code
	"components/toolhub/builtin/artifacts": {"kernel", "components/toolhub", "components/artifacthub/artifact", "components/sandboxhub", "components/events", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/nilcheck", "internal/toolkit"},
	// A-69: builtin/scheduling holds schedule_job and list_scheduled_jobs. It reaches the
	// agent's jobs only through toolhub.Context.ScheduledJobs; host-access guarded.
	// Update (A-126): merged into builtin/jobs below.
	// A-124: builtin/preview holds preview, list_apps and open_in_app; it reaches files only
	// through the agent's sandbox (host-access guarded).
	"components/toolhub/builtin/preview": {"kernel", "components/toolhub", "components/sandboxhub", "kernel/ordered", "internal/jsstr", "internal/toolkit"},
	"components/toolhub/builtin/web":     {"kernel", "components/toolhub", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/nilcheck", "internal/toolkit", "components/secrethub/secret"}, // A-101: http_request puts the agent's secrets in
	// A-70: builtin/triggers holds manage_trigger and list_triggers. It reaches the agent's
	// triggers only through toolhub.Context.Triggers; host-access guarded.
	// Update (A-126): builtin/jobs holds manage_job and list_jobs; it reaches the agent's
	// jobs only through toolhub.Context.Jobs; host-access guarded.
	"components/toolhub/builtin/jobs": {"kernel", "components/toolhub", "components/agent/job", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/nilcheck", "internal/toolkit"},
	// A-105: builtin/subagents holds spawn_subagent; it reaches the agent's subagents only
	// through toolhub.Context.Subagents; host-access guarded.
	"components/toolhub/builtin/subagents": {"kernel", "components/toolhub", "components/agent/subagent", "kernel/ordered", "internal/jsstr", "internal/nilcheck", "internal/toolkit"},
	// A-101: builtin/secrets holds list_secrets and manage_secret; it reaches the agent's
	// secrets only through toolhub.Context.Secrets; host-access guarded.
	"components/toolhub/builtin/secrets": {"kernel", "components/toolhub", "components/secrethub/secret", "kernel/ordered", "internal/jsstr", "internal/nilcheck", "internal/toolkit"},
	// Phase 10 (10-02): memorysystem is the memory system (L5): the
	// extraction, indexing, retrieval and consolidation pipeline. It reaches the model
	// through a consumer port (Model), so it imports neither agent, agentloop, aecmock
	// nor conformance; JS text semantics come from internal/jsstr.
	"components/agent/memorysystem": {"kernel", "components/events", "components/agent/memorysystem/memory", "components/persistence", "components/llmhub", "kernel/ordered", "internal/jsonx", "internal/jsstr", "internal/redact"},
	// The test-only packages under tests/: their non-test code is a package doc and
	// imports nothing; what their tests import is checked like any other test import.
	"tests/arch": {},
	"tests/e2e":  {},
	".": {
		"kernel", "components/events", "components/trace", "components/sandboxhub", "components/agent/memorysystem/memory", "components/agent/profile", "components/toolhub", "components/llmhub", "components/persistence", "components/skillhub/skill",
		"components/agent", "components/orchestration", "components/skillhub", "components/artifacthub", "internal/nilcheck",
		// Phase 11 (11-01): AgentConfig redacts its keys through the shared URL and secret
		// marker of internal/redact, like llmhub.ProviderConfig.
		"internal/redact",
		// A-69: AgentCore.UseScheduledJobRunner takes a promptjob.Runner.
		// A-100: AgentCore.UseChannelPlatform / UseChannelTurnRunner take channel types;
		// AgentCore owns the channel platform hub.
		"components/channelplatformhub/channel", "components/channelplatformhub",
		// A-70: AgentCore.UseTriggerRunner / HandleTriggerWebhook take trigger types.
		// Update (A-126): AgentCore.UseJobRunner / HandleJobWebhook take job types.
		"components/agent/job",
		// A-105: AgentCore.UseSubagentRunner takes a subagent.Runner.
		"components/agent/subagent",
		// A-105 revision: AgentCore owns the subagent type library (UseSubagentType).
		"components/subagenthub", "components/subagenthub/subagenttype",
		// A-101: AgentCore owns the secret hub (SecretHub, UseSecretHubProvider).
		"components/secrethub",
		// A-136: AgentCore.UseCommand takes a command.Spec.
		"components/agent/command",
	},
}

// testingAllowed are the only packages whose non-test code may import
// "testing": the public conformance suites and the shared wire test helpers.
var testingAllowed = map[string]bool{
	"conformance":                true,
	"conformance/internal/check": true,
	"internal/wiretest":          true,
	"internal/clonecheck":        true,
	"internal/streamcheck":       true, // A-124
	"internal/servicecheck":      true, // A-124 phase 2
	"internal/llmtest":           true,
}

// testExternalAllowed are the external modules test files may import.
var testExternalAllowed = map[string]bool{
	"go.uber.org/goleak": true,
}

func relPath(importPath string) string {
	if importPath == archModule {
		return "."
	}
	return strings.TrimPrefix(importPath, archModule+"/")
}

func isModuleImport(imp string) bool {
	return imp == archModule || strings.HasPrefix(imp, archModule+"/")
}

// isStdlib reports whether an import path is part of the standard library: its
// first path element has no dot.
func isStdlib(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return !strings.Contains(first, ".")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// checkLayers is a pure function: the real test feeds it `go list` output, the
// synthetic tests feed it hand-built violations.
func checkLayers(pkgs []pkgInfo, table map[string][]string) []string {
	var violations []string
	for _, p := range pkgs {
		rel := relPath(p.ImportPath)
		allowed, classified := table[rel]
		if !classified {
			violations = append(violations, "unclassified package: "+rel+" is not in the layer table")
			continue
		}
		for _, imp := range p.Imports {
			switch {
			case imp == archModule:
				violations = append(violations, "root import: "+rel+" imports the root package "+archModule)
			case isModuleImport(imp):
				irel := relPath(imp)
				if irel == "internal/wiretest" {
					violations = append(violations, "wiretest in non-test code: "+rel+" imports internal/wiretest")
					continue
				}
				if !contains(allowed, irel) {
					violations = append(violations, "forbidden import: "+rel+" imports "+irel)
				}
			case isStdlib(imp):
				if imp == "testing" && !testingAllowed[rel] {
					violations = append(violations, "testing import: "+rel+" imports testing in non-test code")
				}
			case vendorImportAllowed(vendorAllowed, rel, imp):
				// A provider leaf importing its own vendor module.
			default:
				violations = append(violations, "external import: "+rel+" imports "+imp+" in non-test code")
			}
		}
		for _, imp := range append(append([]string(nil), p.TestImports...), p.XTestImports...) {
			if isModuleImport(imp) || isStdlib(imp) || testExternalAllowed[imp] ||
				vendorImportAllowed(testVendorAllowed, rel, imp) || vendorImportAllowed(vendorAllowed, rel, imp) {
				continue
			}
			violations = append(violations, "external test import: "+rel+" imports "+imp)
		}
	}
	sort.Strings(violations)
	return violations
}

func TestLayers(t *testing.T) {
	for _, goos := range []string{"", "windows"} {
		pkgs := listPackages(t, goos)
		if len(pkgs) == 0 {
			t.Fatalf("go list (GOOS=%q) returned no packages", goos)
		}
		if v := checkLayers(pkgs, layerTable); len(v) > 0 {
			t.Errorf("layer violations (GOOS=%q):\n%s", goos, strings.Join(v, "\n"))
		}
	}
}

func synth(path string, imports ...string) pkgInfo {
	return pkgInfo{ImportPath: archModule + "/" + path, Imports: imports}
}

func TestLayersCatchForbiddenImport(t *testing.T) {
	v := checkLayers([]pkgInfo{
		synth("kernel", archModule+"/components/persistence"),
	}, layerTable)
	if len(v) != 1 {
		t.Fatalf("want exactly one violation, got %v", v)
	}
	if !strings.Contains(v[0], "forbidden import") || !strings.Contains(v[0], "kernel") || !strings.Contains(v[0], "components/persistence") {
		t.Fatalf("violation does not name kernel and persistence: %q", v[0])
	}
}

func TestLayersCatchUnclassified(t *testing.T) {
	v := checkLayers([]pkgInfo{synth("mystery")}, layerTable)
	if len(v) != 1 || !strings.Contains(v[0], "unclassified") || !strings.Contains(v[0], "mystery") {
		t.Fatalf("unclassified package not reported: %v", v)
	}
}

func TestLayersCatchRootImport(t *testing.T) {
	v := checkLayers([]pkgInfo{synth("components/events", archModule)}, layerTable)
	if len(v) != 1 || !strings.Contains(v[0], "root import") || !strings.Contains(v[0], "components/events") {
		t.Fatalf("root import not reported: %v", v)
	}
}

func TestLayersCatchExternalImport(t *testing.T) {
	v := checkLayers([]pkgInfo{synth("kernel", "github.com/x/y")}, layerTable)
	if len(v) != 1 || !strings.Contains(v[0], "external import") || !strings.Contains(v[0], "github.com/x/y") {
		t.Fatalf("external import not reported: %v", v)
	}
	// Test files may use goleak but nothing else external.
	ok := checkLayers([]pkgInfo{{ImportPath: archModule + "/kernel", TestImports: []string{"go.uber.org/goleak"}}}, layerTable)
	if len(ok) != 0 {
		t.Fatalf("goleak in test imports must be allowed: %v", ok)
	}
	bad := checkLayers([]pkgInfo{{ImportPath: archModule + "/kernel", XTestImports: []string{"github.com/x/y"}}}, layerTable)
	if len(bad) != 1 {
		t.Fatalf("external test import not reported: %v", bad)
	}
}

func TestLayersCatchTestingImport(t *testing.T) {
	v := checkLayers([]pkgInfo{synth("kernel", "testing")}, layerTable)
	if len(v) != 1 || !strings.Contains(v[0], "testing import") || !strings.Contains(v[0], "kernel") {
		t.Fatalf("testing import not reported: %v", v)
	}
	if ok := checkLayers([]pkgInfo{synth("conformance", "testing")}, layerTable); len(ok) != 0 {
		t.Fatalf("conformance may import testing: %v", ok)
	}
}

func TestLayersCatchWiretestImport(t *testing.T) {
	v := checkLayers([]pkgInfo{synth("kernel", archModule+"/internal/wiretest")}, layerTable)
	if len(v) != 1 || !strings.Contains(v[0], "wiretest") || !strings.Contains(v[0], "kernel") {
		t.Fatalf("wiretest import not reported: %v", v)
	}
}
