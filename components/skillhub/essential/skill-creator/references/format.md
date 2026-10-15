# Skill format reference

## Frontmatter

| Field | Required | Rules |
|---|---|---|
| `name` | yes | 1–64 chars: `a-z`, `0-9`, single hyphens, no leading or trailing hyphen. Unique in the library. |
| `description` | yes | 1–1024 chars. What it does **and** when to use it. |
| `license` | no | e.g. `MIT`, or "Proprietary". |
| `compatibility` | no | ≤500 chars of environment requirements, e.g. "Needs python3 in the sandbox". |
| `allowed-tools` | no | Space-separated tool names. A hint only, not a permission. |
| `metadata` | no | Map of string keys and values. |
| `metadata.requires-tools` | no | Space- or comma-separated tool names. The skill is hidden from agents that lack any of them. |

Values are plain YAML strings. For a long description use a folded block:

```yaml
description: >
  First part of the description,
  continued here.
```

## Files

- Paths are relative, e.g. `scripts/extract.py` or `references/codes.md`. They cannot use `..`, a leading `/`, or a backslash.
- Conventional folders: `scripts/` (runnable code), `references/` (docs read on demand), `assets/` (templates, sample data).
- Limits: at most 50 files, 200 KB per file and 1 MB in total; SKILL.md itself up to 100 KB. Files are text only.
- When a skill is loaded, its files are copied into the agent's workspace at `.skills/<name>/`, so scripts run with e.g. `python3 .skills/<name>/scripts/extract.py input.csv`.
- Never include credentials. Saves that look like they contain keys or tokens are rejected.

## How agents see skills

1. **Index**: every turn, the name and description of each enabled skill is in the system prompt.
2. **Body**: `skill_view(name)` returns the SKILL.md instructions and the list of files.
3. **Files**: `skill_view(name, path)` returns one file, or scripts are run from `.skills/<name>/`.

## Complete example

```markdown
---
name: rfi-log-summary
description: Summarize an RFI log (CSV or Excel export) into open items by trade, overdue responses and a short status note. Use when the user shares an RFI log, asks which RFIs are open or overdue, or wants an RFI status update for a meeting.
metadata:
  requires-tools: read_file run_code
---

# RFI Log Summary

Turns an exported RFI log into a one-page status: counts, overdue items and follow-ups.

## Procedure
1. Find the file with `list_files`. If it is not CSV, ask the user to export it as CSV.
2. Run `python3 .skills/rfi-log-summary/scripts/summarize.py <file.csv>` with `terminal`. It prints JSON with totals, the open items by trade and the overdue items.
3. Write the summary in the format below. Take the "as of" date from the newest row, not from today.

## Output
**RFI status as of <date>** — <open>/<total> open, <overdue> overdue
- Overdue: RFI #, subject, days late, ball-in-court
- Open by trade: trade — count
- Follow-ups: up to 3 concrete actions

## Pitfalls
- Column names vary by platform (Procore, ACC). See references/columns.md for the mapping.
- A response date that is empty or "TBD" means the RFI is open.

## Verification
The open and overdue counts in the note must match the script output.
```

## Checklist before saving

- [ ] The name is specific and matches the pattern.
- [ ] The description says what the skill does and when to use it, with trigger words.
- [ ] Procedure steps are numbered, imperative, and name the tools.
- [ ] The output format is defined.
- [ ] Long detail is in references/, and each reference is mentioned in SKILL.md.
- [ ] Scripts were run once and their usage is documented.
- [ ] requires-tools lists only hard requirements.
- [ ] No credentials, personal data or one-off facts.
