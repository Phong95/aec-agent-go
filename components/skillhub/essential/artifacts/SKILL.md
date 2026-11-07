---
name: artifacts
description: Save, update and find lasting outputs with the artifact tool. Use when you produce something the user will keep, reuse or hand to another agent (a report, a spec section, a takeoff, an estimate, a page, an app, a logo, a data file), when you change a deliverable you saved before, and when the user asks what you have made or refers to an earlier deliverable by name.
metadata:
  requires-tools: artifact
---

# Artifacts

An artifact is a file in your workspace that you track by name. The file is the deliverable; the artifact adds a name, a type, a version history and a link the user can open in their Artifacts panel. Later turns, other agents and the user refer to it by name, so you never paste its content into the conversation again.

## When to save one

Save when the output is the point of the request and will outlive this reply:

- documents: reports, specs, summaries the user asked for, letters, checklists;
- data: takeoffs, estimates, schedules, CSV or JSON tables;
- built things: a web page, an app, a game, a diagram, a logo or image file.

Don't save scratch work: intermediate notes, test files, a quick answer that fits in the reply, a file the user only wanted to look at once.

Before saving, run `artifact` with `action: "list"`: if there is already an artifact for this deliverable, update it instead of creating a near-duplicate.

## Save (version 1)

`artifact` with `action: "save"`, a stable `name` unique for this agent (`clash-report.md`, `reports/q3-takeoff.csv`) and a `type`:

- Use a real MIME type when there is one (`text/markdown`, `text/csv`, `text/html`, `application/json`, `image/png`): the Artifacts panel renders it (formatted markdown, a table, a page) instead of plain text. A domain type (`aec/takeoff`, `aec/clash-report`) is fine when no MIME type fits.
- Give the file in one of two ways, not both:
  - `content`: the complete text; it is written to `artifacts/<name>` and tracked.
  - `from_path`: a file you already wrote (with write_file, a script or a build); it is tracked where it is, not copied. Prefer this for large or generated files.
- Add a one-line `description` for the user's list, `tags` for filtering, and a `note` on why this version exists.

A name you already used is refused: update that artifact instead.

## Update (a new version)

`artifact` with `action: "update"` and the `name` (or `id`), then one of:

- `content`: the complete new text (not a patch); it rewrites the tracked file;
- `from_path`: a different file to track from now on;
- neither: after you changed the tracked file yourself (edit_file, write_file, a build), this records its current state as the next version.

Say what changed in `note`. When a person may have edited the artifact meanwhile, pass `expected_version` (the version you last saw); the update fails if it moved on, and you look again before deciding.

## Find and read

`action: "list"` shows your artifacts, newest first: name, type, version, size, the tracked path, and whether the file **changed** since its last version or is **missing**. Filter with `type`, `tags` or `this_session_only`.

To read an artifact, read its file with read_file on the path the list shows. If it changed and the change is yours to keep, update with just the name to record it.

## After saving

The result carries a link: cite it in your reply as it is given, so the user can open the artifact, and summarise what it contains instead of repeating it.
