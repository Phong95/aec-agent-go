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
var renamedTools = map[string]string{
	"save_artifact":   "artifact",
	"update_artifact": "artifact",
	"read_artifact":   "artifact",
	"list_artifacts":  "artifact",
}

// RenamedTools returns the renamed or merged built-in tools, old name -> current name
// (A-209), as a fresh map.
func RenamedTools() map[string]string { return maps.Clone(renamedTools) }

// RenamedToolNames returns names with every renamed tool replaced by its current name, in
// order, without duplicates (A-209, renamedToolNames of core/ts). It never returns nil
// for a non-nil input, and returns nil for nil.
func RenamedToolNames(names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		current := name
		if renamed, ok := renamedTools[name]; ok {
			current = renamed
		}
		if !slices.Contains(out, current) {
			out = append(out, current)
		}
	}
	return out
}
