---
name: skill-creator
description: Create a new skill, or improve one of your existing skills, from the user's description or from a workflow that just worked. Use when the user asks to make, write, save, teach, or add a skill, says "remember how to do this", "turn this into a skill", "make this repeatable", "next time do it like this", or asks to change, fix or extend a skill.
metadata:
  requires-tools: skill_manage
---

# Skill Creator

A skill is a folder of instructions that you and other agents load on demand: `SKILL.md` plus optional `scripts/`, `references/` and `assets/` files. It holds procedural know-how: how to do one kind of task well with the tools you already have. It does not add new tools.

You save skills with the `skill_manage` tool. Every save asks the user for approval, so show them a clear summary first.

## When not to make a skill

- A fact about the user or the project. That belongs in memory, not a skill.
- Something one tool call already does.
- Anything that would need credentials in its files. Never write keys, tokens or passwords into a skill.

## Workflow

### 1. Capture the intent

Work these out from the conversation, or by asking. Use `clarify` if you have it: one focused question at a time, at most three rounds.

1. What should the skill let an agent do, and what is the end result?
2. When should it trigger? Collect 3–5 example requests a user might make.
3. What inputs does it expect, and what output should it produce (format, files, where to save them)?
4. Which steps, rules and pitfalls matter to the user? Which tools does it need (e.g. `web_search`, `terminal`, `read_file`)?

If the user says "turn this into a skill", extract all of this from the conversation first: the steps you took, the corrections the user made, the output they accepted. Then ask only about the gaps.

Before drafting, check your skill index. If an existing skill already covers this, propose improving it instead, and `skill_view` it first.

### 2. Choose the name and description

- **name**: lowercase letters, digits and hyphens, at most 64 characters, and specific. `rfi-response-draft`, not `helper`.
- **description**: at most 1024 characters, in the third person, saying **what the skill does and when to use it**. This is the only part an agent sees before loading the skill, so it decides whether the skill ever gets used. Include the trigger phrases and situations from step 1, and lean towards triggering: "Use whenever the user mentions X, Y or Z, even if they don't ask for a skill."

### 3. Write SKILL.md

```markdown
---
name: rfi-response-draft
description: Draft a response to a construction RFI … Use when …
metadata:
  requires-tools: read_file write_file
---

# RFI Response Draft

One paragraph: the purpose, and what a good result looks like.

## When to use
## Procedure
1. Numbered, imperative steps. Name the exact tools to call.
## Output
The format or template of the result.
## Pitfalls
## Verification
How to check the result before reporting it as done.
```

Guidelines:

- Write for another capable agent: imperative, concrete, no filler. Explain *why* behind any rule that isn't obvious.
- Prefer a worked example or template over abstract advice.
- Keep SKILL.md under about 300 lines. Put long reference material (standards, schemas, checklists, big examples) in `references/<topic>.md`, and say when to read it: "For the column mapping, read references/columns.md." Keep references one level deep.
- Add a script under `scripts/` only for deterministic, repeatable work that is error-prone to redo by hand each time (parsing, calculations, format conversion). Scripts run in the sandbox through `terminal` or `run_code`, and the sandbox has no network. Say in SKILL.md how to run the script and what it prints.
- `requires-tools` hides the skill from agents that don't have those tools, so list only hard requirements.
- Don't restate what the tools already describe.

### 4. Test before saving

Do this when the skill has scripts, or when the user asks for it.

- If you have a sandbox, write the scripts to a scratch folder with `write_file`, run them on a small realistic input with `terminal`, and fix any errors.
- Walk through the procedure against one or two of the example requests from step 1, and check that every step can be done with the tools it names.

### 5. Show the draft, then save

1. Summarize for the user: the name, the description, the steps and the files. Ask about anything you are unsure of.
2. Save in one call:
   `skill_manage(action="create", content="<the full SKILL.md>", files=[{"path": "scripts/x.py", "content": "…"}])`
   The user approves the call. If validation fails, fix every listed problem and call again.
3. The skill is now enabled for you and appears in your skill index from your next turn. Tell the user its name, what will trigger it, and that other agents can enable it on their Capabilities page.

## Improving an existing skill

1. `skill_view` the skill, and any files you will change.
2. For a small change, use `skill_manage(action="patch", name=…, old_string=…, new_string=…)`. Add `path` to patch a file other than SKILL.md. `old_string` must match exactly once.
3. For a large change, use `action="rewrite"` with the full new SKILL.md. To add or replace a file use `action="write_file"`; to remove one use `action="remove_file"`.
4. Keep the name; versions are tracked automatically.

Only custom skills can be changed. Built-in skills are read-only: to adapt one, create a new skill with a different name.

For the full format rules, the limits and a complete example, read `references/format.md` with `skill_view(name="skill-creator", path="references/format.md")`.
