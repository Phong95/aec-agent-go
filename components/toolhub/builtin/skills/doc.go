// Package skills holds the built-in tools of the skill system: skill_view, which loads
// one of the agent's skills (its SKILL.md instructions and the list of its files, or
// one file), skill_manage, and the workspace helpers that copy a skill into the agent's sandbox.
//
// skill_view is not registered in the tool hub. The agent's SkillManager hands it to
// the loop as the Skills port, so enabling a skill is enough to make it loadable.
// skill_manage, the tool that creates and edits skills, is a normal hub tool and the app
// registers it (AgentCore.UseTool): its risk is consequential, so every call goes through
// the approval gate and fails closed with no interaction handler, only custom skills can be
// changed, and after each change the workspace copy of the skill is brought in line with
// CopyToWorkspace and RemoveFromWorkspace. skill_view stays outside the policy and
// approval gate; its guards are the isolated-sandbox rule and the checked skill
// names and paths of the workspace helpers (T-10-18, T-10-19).
//
// The package is a host-access guarded package: it never imports os, os/exec,
// path/filepath, net or net/http. Skill files reach the agent's workspace only through
// sandboxhub.View, and only when the sandbox is isolated (T-05-08, T-06-15).
package skills
