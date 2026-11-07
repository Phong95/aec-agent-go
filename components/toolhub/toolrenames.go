package toolhub

import (
	"maps"
	"slices"
)

// renamedTools (A-209, RENAMED_TOOLS of core/ts) are the built-in tools that were renamed
// or merged, old name -> current name. A stored profile (or a subagent type) that still
// lists an old name gets the current tool instead, so switching a tool's name never
// silently takes the tool away from an agent.
//
// The artifact tools before A-209 (save_artifact, update_artifact, read_artifact,
// list_artifacts) are each the one "artifact" tool now. They are written out here
// because toolhub cannot import builtin/artifacts (it imports toolhub); a test of that
// package checks them against artifacts.RetiredToolNames.
//
// Update (A-210): and the preview tools - list_apps, open_in_app and share_preview are
// "preview"; start_service, stop_service, list_services and service_logs are "service".
// A test of builtin/preview checks them against preview.RetiredToolNames.
var renamedTools = map[string]string{
	"save_artifact":   "artifact",
	"update_artifact": "artifact",
	"read_artifact":   "artifact",
	"list_artifacts":  "artifact",
	"list_apps":       "preview",
	"open_in_app":     "preview",
	"share_preview":   "preview",
	"start_service":   "service",
	"stop_service":    "service",
	"list_services":   "service",
	"service_logs":    "service",
	// Update (A-211): the job tools; a test of builtin/jobs checks them against
	// jobs.RetiredToolNames.
	"manage_job": "job",
	"list_jobs":  "job",
	// Update (A-212): the secret and web tools; tests of builtin/secrets and builtin/web
	// check them against secrets.RetiredToolNames and web.RetiredToolNames.
	"list_secrets":  "secret",
	"manage_secret": "secret",
	"web_search":    "web",
	"web_extract":   "web",
}

// renamedToolActions (A-212, RENAMED_TOOL_ACTIONS of core/ts) is the action an old tool's
// call becomes in its merged tool, for a stored call (RenamedToolCall): a tool job saved
// with web_search runs web with action "search". Only tools whose old arguments carry over
// as they are; manage_job and manage_secret already had an action.
var renamedToolActions = map[string]string{
	"save_artifact":   "save",
	"update_artifact": "update",
	"list_artifacts":  "list",
	"start_service":   "start",
	"stop_service":    "stop",
	"list_services":   "list",
	"service_logs":    "logs",
	"list_jobs":       "list",
	"list_secrets":    "list",
	"web_search":      "search",
	"web_extract":     "extract",
}

// RenamedToolCall returns a stored call (a tool job's tool and arguments) as it runs today
// (A-212, renamedToolCall of core/ts): a renamed tool's current name, with the action its
// old name stood for added when the arguments have none. Any other call is returned as it
// is; args is never changed in place.
func RenamedToolCall(name string, args map[string]any) (string, map[string]any) {
	current, ok := renamedTools[name]
	if !ok {
		return name, args
	}
	action, ok := renamedToolActions[name]
	if _, has := args["action"]; !ok || has {
		return current, args
	}
	out := make(map[string]any, len(args)+1)
	maps.Copy(out, args)
	out["action"] = action
	return current, out
}

// companionSkills (A-211, COMPANION_SKILLS of core/ts) is the essential skill that took
// over a merged tool's guidance (its old, long tool descriptions). An agent converted from
// the old tool names gets it too (CompanionSkills), because the essential skills are only
// the default for new agents.
var companionSkills = map[string]string{
	"artifact": "artifacts",
	"preview":  "live-preview",
	"job":      "jobs",
}

// CompanionSkillsOf returns the merged tool -> skill map (A-211) as a fresh map.
func CompanionSkillsOf() map[string]string { return maps.Clone(companionSkills) }

// CompanionSkills returns the skills a stored profile gains when its old tool names are
// converted (A-211, companionSkills of core/ts): the companion skill of each merged tool
// it lists under an old name, when skills does not have it yet, in order. Only old names
// count, so a profile saved after the conversion (new names only) gains nothing, and a
// skill the user turned off afterwards stays off.
// Update (A-212): an old name keep reports (see RenamedToolNamesKeeping) gains nothing; a
// nil keep keeps nothing.
func CompanionSkills(tools, skills []string, keep func(string) bool) []string {
	var out []string
	for _, name := range tools {
		current, ok := renamedTools[name]
		if !ok || (keep != nil && keep(name)) {
			continue
		}
		skill, ok := companionSkills[current]
		if ok && !slices.Contains(skills, skill) && !slices.Contains(out, skill) {
			out = append(out, skill)
		}
	}
	return out
}

// RenamedTools returns the renamed or merged built-in tools, old name -> current name
// (A-209), as a fresh map.
func RenamedTools() map[string]string { return maps.Clone(renamedTools) }

// RenamedToolNames returns names with every renamed tool replaced by its current name, in
// order, without duplicates (A-209, renamedToolNames of core/ts). It never returns nil
// for a non-nil input, and returns nil for nil.
func RenamedToolNames(names []string) []string {
	return RenamedToolNamesKeeping(names, nil)
}

// RenamedToolNamesKeeping is RenamedToolNames that leaves the old names keep reports as
// they are (A-212, the keep of renamedToolNames in core/ts): a host that still registers an
// old tool under its old name (web.NewSearchTool, "web_search") keeps it, instead of being
// converted to a tool it never registered. A nil keep keeps nothing.
func RenamedToolNamesKeeping(names []string, keep func(string) bool) []string {
	if names == nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		current := name
		if renamed, ok := renamedTools[name]; ok && (keep == nil || !keep(name)) {
			current = renamed
		}
		if !slices.Contains(out, current) {
			out = append(out, current)
		}
	}
	return out
}
