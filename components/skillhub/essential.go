package skillhub

import (
	"embed"
	"io/fs"
	"path"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
)

// essentialFS holds the text of the essential skills, one folder per skill:
// essential/<name>/SKILL.md plus every other file of the skill under the same
// folder. The files are written from the recorded essential skills dump
// (tests/testdata/golden/essential-skills.json) and a test compares them byte for
// byte; never edit them by hand, and never convert their line endings
// (.gitattributes marks them -text).
//
//go:embed essential
var essentialFS embed.FS

// essentialSkillNames is the canonical order of the essential skills.
// Update (A-70): watch-and-react is appended as the fourth.
// Update (A-126): watch-and-notify replaces it - one schedule job that stays
// [[SILENT]] when there is nothing new; it needs manage_job.
// Update (A-182): visual-self-check is appended - render, capture, look, fix, look
// again; it needs screenshot (A-181), offered only to a model that can see images.
var essentialSkillNames = [...]string{"skill-creator", "plan", "web-research", "watch-and-notify", "visual-self-check"}

// EssentialSkillNames returns the names of the essential skills - the default
// profile.skills for a new agent. Every call returns a fresh slice.
func EssentialSkillNames() []string {
	return append([]string(nil), essentialSkillNames[:]...)
}

// EssentialSkills returns the essential skills every agent should have, in
// canonical order (skill-creator, plan, web-research). AgentCore registers them as
// "built-in" skills; an agent uses one when it is in its profile.skills and it
// has the tools the skill requires (skill-creator needs skill_manage;
// web-research needs web_search and web_extract). Every call returns fresh
// packages, so a caller may modify them freely.
// Update (A-70): the canonical order ends with watch-and-react, which pairs a
// watcher job with a reaction trigger; it needs schedule_job and manage_trigger.
//
// The text is embedded, so a read failure cannot happen at run time; if one did,
// the package would hold only what was read and the completeness test would fail.
func EssentialSkills() []skill.Package {
	out := make([]skill.Package, 0, len(essentialSkillNames))
	for _, name := range essentialSkillNames {
		out = append(out, readEssential(name))
	}
	return out
}

// readEssential builds the package of one embedded skill: SKILL.md as content and
// every other file under the folder, in lexical path order with forward slashes.
func readEssential(name string) skill.Package {
	root := path.Join("essential", name)
	var pkg skill.Package
	if data, err := essentialFS.ReadFile(path.Join(root, "SKILL.md")); err == nil {
		pkg.Content = string(data)
	}
	_ = fs.WalkDir(essentialFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable embedded entry is skipped; the completeness test catches it
		}
		rel := p[len(root)+1:]
		if rel == "SKILL.md" {
			return nil
		}
		if data, readErr := essentialFS.ReadFile(p); readErr == nil {
			pkg.Files = append(pkg.Files, skill.File{Path: rel, Content: string(data)})
		}
		return nil
	})
	return pkg
}
