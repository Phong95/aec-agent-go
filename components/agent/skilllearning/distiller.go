package skilllearning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Distiller - stage 3 of skill learning: turn a confirmed episode (the site diary) into
// a small SKILL.md (the method statement) that keeps the winning path as steps and the
// dead ends as pitfalls. One model call through the agent's ModelManager, like memory
// extraction; bad output is an error (the episode is tried again), it is never read as
// "nothing to learn".
// ---------------------------------------------------------------------------

// DistillSystemPrompt is the system prompt of the distill call (byte-identical to
// core/ts DISTILL_SYSTEM_PROMPT).
const DistillSystemPrompt = `You turn one finished agent task into a reusable skill (a SKILL.md procedure) for the same agent.

You receive:
- REQUEST: what the user asked for.
- TRAJECTORY: the tool calls the agent made, in order, with short excerpts of their arguments and results. Failed calls are marked FAILED and keep their error text.
- FINAL REPLY: the agent's last answer, which the user accepted.
- EXISTING SKILLS: the skills the agent already has (name: description).
- TARGET SKILL (only when present): a skill the agent followed during this task and still struggled with, with its current SKILL.md.

Decide whether the task is a reusable procedure: something the user or a teammate is likely to ask again with different inputs. A one-off answer, a pure conversation or a task without a clear method is not reusable.

If it is reusable:
- Prefer "patch" over "create" when an existing skill already covers the same kind of task: set "target" to its exact name and write the full improved skill. When a TARGET SKILL is shown, patch it unless this task was clearly a different kind of work.
- Keep only the winning path, as short numbered steps a capable agent can follow. Leave the dead ends out of the steps.
- Turn every dead end into a pitfall: what was tried, why it failed, and what to do instead. Keep error text short.
- Generalise: replace this task's file names, paths, ids and values with named parameters. Keep versions that mattered (for example "use Python 3.11, there is no wheel for 3.13").
- Never copy secret values, tokens, passwords or keys. Keep {{secret:NAME}} placeholders exactly as written.
- Keep the skill focused on one kind of task: at most about 10 steps and 8 pitfalls.
- name: lowercase letters, digits and single hyphens, at most 64 characters, naming the kind of task (for example ifc-element-schedule-export).
- description: at most 300 characters: what the skill does, then "Use when ..." with the requests that should trigger it.
- requiresTools: only tool names that appear in the trajectory and that the steps need.
- Write in the language of the user's request; keep technical terms in English.

Respond with ONLY a JSON object (no markdown fences):
{"reusable": true, "action": "create" or "patch", "target": "existing skill name, for patch", "name": "...", "description": "...", "steps": ["..."], "pitfalls": ["..."], "parameters": ["..."], "requiresTools": ["..."]}
or, when it is not reusable:
{"reusable": false, "reason": "..."}`

// DistillMaxTokens is the output budget of the distill call - reasoning models spend
// tokens before they answer.
const DistillMaxTokens = 8192

// The limits of the distill input and output.
const (
	maxRequestChars    = 4000
	maxFinalReplyChars = 2000
	maxArgumentChars   = 300
	maxResultChars     = 400
	maxErrorChars      = 600
	maxTextChars       = 300
	// maxTrajectoryChars is the most characters of the whole trajectory; beyond it the
	// middle is cut out.
	maxTrajectoryChars  = 40_000
	maxDescriptionChars = 300
	maxListItems        = 12
	maxItemChars        = 600
)

// LearnedSkillMetadataKey and LearnedSkillMetadataValue mark a skill the learner wrote
// (frontmatter metadata), for the UI and for health tracking.
const (
	LearnedSkillMetadataKey   = "learned-by"
	LearnedSkillMetadataValue = "skill-learning"
)

var (
	skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	toolNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
	spaces           = regexp.MustCompile(`\s+`)
)

// distilled is what the distiller returned for a reusable task.
type distilled struct {
	action        string // "create" or "patch"
	target        string
	name          string
	description   string
	steps         []string
	pitfalls      []string
	parameters    []string
	requiresTools []string
}

// distillInput is what one distill call reads.
type distillInput struct {
	// messages is the turn's messages, its user message first (later turns cut off).
	messages []kernel.Message
	// skills is the agent's skills now, for "patch an existing skill" over a near-duplicate.
	skills []skill.Skill
	// target is a skill the turn followed and still struggled with (nil = none).
	target *skill.Skill
}

// turnMessages returns the turn's messages from its user message to the message before
// the next user message.
func turnMessages(msgs []kernel.Message) []kernel.Message {
	start := -1
	for i, m := range msgs {
		if m.Role == kernel.RoleUser {
			start = i
			break
		}
	}
	if start < 0 {
		return []kernel.Message{}
	}
	for i := start + 1; i < len(msgs); i++ {
		if msgs[i].Role == kernel.RoleUser {
			return msgs[start:i]
		}
	}
	return msgs[start:]
}

func clip(text string, n int) string {
	t := strings.TrimSpace(spaces.ReplaceAllString(text, " "))
	if len([]rune(t)) > n {
		return head(t, n) + "..."
	}
	return t
}

func argumentsText(a kernel.Arguments) string {
	if len(a) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, a); err != nil {
		return string(a)
	}
	return buf.String()
}

func isErrorResult(r kernel.ToolResult) bool { return r.IsError != nil && *r.IsError }

// formatTrajectory compresses the turn: each call with an argument excerpt, each result
// (failures kept with their error text).
func formatTrajectory(msgs []kernel.Message) (request, trajectory, finalReply string) {
	if len(msgs) > 0 && msgs[0].Role == kernel.RoleUser {
		request = head(msgs[0].Content, maxRequestChars)
	}
	names := map[string]string{}
	var lines []string
	step := 0
	for _, m := range msgs[min(1, len(msgs)):] {
		switch m.Role {
		case kernel.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				finalReply = m.Content
				if len(m.ToolCalls) > 0 {
					lines = append(lines, "[agent] "+clip(m.Content, maxTextChars))
				}
			}
			for _, c := range m.ToolCalls {
				names[c.ID] = c.Name
				step++
				lines = append(lines, strconv.Itoa(step)+". call "+c.Name+"("+clip(argumentsText(c.Arguments), maxArgumentChars)+")")
			}
		case kernel.RoleTool:
			for _, r := range m.ToolResults {
				name := names[r.ToolCallID]
				if name == "" {
					name = "tool"
				}
				if isErrorResult(r) {
					lines = append(lines, "   FAILED "+name+": "+clip(r.Content, maxErrorChars))
				} else {
					lines = append(lines, "   ok "+name+": "+clip(r.Content, maxResultChars))
				}
			}
		default:
		}
	}
	trajectory = strings.Join(lines, "\n")
	if r := []rune(trajectory); len(r) > maxTrajectoryChars {
		half := maxTrajectoryChars / 2
		trajectory = string(r[:half]) + "\n... (middle of the trajectory cut) ...\n" + string(r[len(r)-half:])
	}
	return request, trajectory, tail(finalReply, maxFinalReplyChars)
}

// buildDistillPrompt is the user content of the distill call.
func buildDistillPrompt(in distillInput) string {
	request, trajectory, finalReply := formatTrajectory(in.messages)
	skills := "(none)"
	if len(in.skills) > 0 {
		items := make([]string, 0, len(in.skills))
		for _, s := range in.skills {
			items = append(items, "- "+s.Name+": "+clip(s.Description, 200))
		}
		skills = strings.Join(items, "\n")
	}
	if trajectory == "" {
		trajectory = "(no tool calls)"
	}
	parts := []string{
		"REQUEST:\n" + request,
		"TRAJECTORY:\n" + trajectory,
		"FINAL REPLY:\n" + finalReply,
		"EXISTING SKILLS:\n" + skills,
	}
	if in.target != nil {
		parts = append(parts, "TARGET SKILL ("+in.target.Name+"):\n"+in.target.Content)
	}
	return strings.Join(parts, "\n\n")
}

func stringList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, item := range raw {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, head(strings.TrimSpace(s), maxItemChars))
		if len(out) == maxListItems {
			break
		}
	}
	return out
}

// parseDistilled parses the distill answer: nil when the task is not reusable. It fails
// on malformed output.
func parseDistilled(content string) (*distilled, error) {
	raw, err := parseJSONObject(content, "skill distillation")
	if err != nil {
		return nil, err
	}
	if raw["reusable"] != true {
		return nil, nil //nolint:nilnil // nil, nil means "nothing reusable"
	}
	name, _ := raw["name"].(string)
	name = strings.ToLower(strings.TrimSpace(name))
	description, _ := raw["description"].(string)
	description = strings.TrimSpace(spaces.ReplaceAllString(description, " "))
	steps := stringList(raw["steps"])
	if name == "" || !skillNamePattern.MatchString(name) || len(name) > 64 {
		return nil, fmt.Errorf("skill distillation returned an invalid name %q", name)
	}
	if description == "" {
		return nil, errors.New("skill distillation returned no description")
	}
	if len(steps) == 0 {
		return nil, errors.New("skill distillation returned no steps")
	}
	target, _ := raw["target"].(string)
	target = strings.TrimSpace(target)
	d := &distilled{
		action:      "create",
		name:        name,
		description: head(description, maxDescriptionChars),
		steps:       steps,
		pitfalls:    stringList(raw["pitfalls"]),
		parameters:  stringList(raw["parameters"]),
	}
	if raw["action"] == "patch" && target != "" {
		d.action, d.target = "patch", target
	}
	for _, t := range stringList(raw["requiresTools"]) {
		if toolNamePattern.MatchString(t) {
			d.requiresTools = append(d.requiresTools, t)
		}
	}
	return d, nil
}

func oneLine(text string) string { return strings.TrimSpace(spaces.ReplaceAllString(text, " ")) }

func title(name string) string {
	words := strings.Split(name, "-")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// renderSkill renders the SKILL.md. The description goes in a folded block, so quotes and
// colons need no escaping. It is validated with skill.ParsePackage (name, sizes, secret
// scan) and fails with its SkillError.
func renderSkill(d distilled) (skill.Package, error) {
	lines := []string{"---", "name: " + d.name, "description: >-", "  " + oneLine(d.description), "metadata:"}
	if len(d.requiresTools) > 0 {
		lines = append(lines, "  requires-tools: "+strings.Join(d.requiresTools, " "))
	}
	lines = append(lines, "  "+LearnedSkillMetadataKey+": "+LearnedSkillMetadataValue, "---", "", "# "+title(d.name), "", oneLine(d.description), "")
	if len(d.parameters) > 0 {
		lines = append(lines, "## Parameters", "")
		for _, p := range d.parameters {
			lines = append(lines, "- "+oneLine(p))
		}
		lines = append(lines, "")
	}
	lines = append(lines, "## Steps", "")
	for i, s := range d.steps {
		lines = append(lines, strconv.Itoa(i+1)+". "+oneLine(s))
	}
	lines = append(lines, "")
	if len(d.pitfalls) > 0 {
		lines = append(lines, "## Pitfalls", "")
		for _, p := range d.pitfalls {
			lines = append(lines, "- "+oneLine(p))
		}
		lines = append(lines, "")
	}
	pkg := skill.Package{Content: strings.Join(lines, "\n")}
	if _, err := skill.ParsePackage(pkg); err != nil {
		return skill.Package{}, err
	}
	return pkg, nil
}

// distill makes one distill call: the parsed result, or nil when the model found nothing
// reusable.
func distill(ctx context.Context, m Model, in distillInput) (*distilled, error) {
	res, err := m.Chat(ctx, []kernel.Message{
		{Role: kernel.RoleSystem, Content: DistillSystemPrompt},
		{Role: kernel.RoleUser, Content: buildDistillPrompt(in)},
	}, llmhub.ModelOptions{MaxTokens: intPtr(DistillMaxTokens)})
	if err != nil {
		return nil, err
	}
	if res.StopReason == kernel.StopMaxTokens {
		return nil, fmt.Errorf("skill distillation truncated (output tokens: %d)", res.Usage.OutputTokens)
	}
	return parseDistilled(res.Message.Content)
}

// FormatTrajectory compresses a turn's messages (its user message first) into the
// request, a trajectory of tool calls and results (failures marked FAILED) and the final
// reply - the input the distiller and the A-174 replay judge read.
func FormatTrajectory(msgs []kernel.Message) (request, trajectory, finalReply string) {
	return formatTrajectory(msgs)
}
