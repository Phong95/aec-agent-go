package artifacts

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// listLabel is the label list errors carry (A-209: the artifact tool's "list" action).
const listLabel = "artifact list"

// statusConcurrency is the most tracked files whose status list checks at once.
const statusConcurrency = 8

// listArtifacts is list_artifacts: what this agent has already
// made, most recently updated first. It returns metadata only (no content), so the model
// can decide what to read or update without pulling whole deliverables into context. It
// reads no file, so it needs no sandbox.
// Update (A-209): it is the artifact tool's "list" action (listArtifacts of core/ts).
// There is no read action any more, so each line also says whether the tracked file
// still matches its last version (changed / missing), and the model reads the file
// itself with read_file. The status needs the sandbox; without one (or when the
// sandbox accessor fails) the list is still given, with no status.
//
// The limit is the requested limit when it is a number above
// zero, else Options.MaxListEntries, never above Options.MaxListEntries, floored and at
// least 1. this_session_only filters by the session of the artifact view, and is ignored
// when the view carries no session.
func listArtifacts(ctx context.Context, a toolkit.Args, tc toolhub.Context, opts resolved) toolhub.Output {
	return withArtifacts(tc, listLabel, func(view artifact.View) (toolhub.Output, error) {
		limit := boundedLimit(a, "limit", opts.maxListEntries)
		q := artifact.Query{Tags: stringList(a, "tags"), Limit: &limit}
		if typ := trimmedString(a, "type"); typ != "" {
			q.Types = []string{typ}
		}
		if only, _ := a.Bool("this_session_only"); only {
			q.SessionID = view.SessionID()
		}

		found, err := view.List(ctx, q)
		if err != nil {
			return toolhub.Output{}, err
		}

		if len(found) == 0 {
			meta := ordered.NewObject()
			meta.Set("count", 0)
			return toolkit.Result(`No artifacts yet. Use action "save" when you produce a deliverable worth keeping.`, meta), nil
		}

		statuses := fileStatuses(ctx, found, view, tc)
		changed := false

		lines := make([]string, len(found))
		listed := make([]any, len(found))
		for i, art := range found {
			parts := []string{
				art.Name + " (" + art.Type + ") v" + strconv.Itoa(art.Version),
				toolkit.FormatBytes(int64(art.Bytes)),
				"tracks " + art.SandboxPath,
			}
			parts = append(parts, statusPart(art, statuses[i])...)
			parts = append(parts, "updated "+art.UpdatedAt)
			if len(art.Tags) > 0 {
				parts = append(parts, "tags: "+strings.Join(art.Tags, ", "))
			}
			if art.Description != "" {
				parts = append(parts, art.Description)
			}
			parts = append(parts, "id "+art.ID)
			lines[i] = strings.Join(parts, " "+midDot+" ")

			entry := ordered.NewObject()
			entry.Set("id", art.ID)
			entry.Set("name", art.Name)
			entry.Set("type", art.Type)
			entry.Set("version", art.Version)
			entry.Set("bytes", art.Bytes)
			entry.Set("sandboxPath", art.SandboxPath)
			if statuses[i] != nil {
				entry.Set("status", string(statuses[i].Status))
				if statuses[i].Status == artifact.FileChanged {
					changed = true
				}
			}
			listed[i] = entry
		}
		plural := "s"
		if len(found) == 1 {
			plural = ""
		}
		footer := "Read one with read_file on its path."
		if changed {
			footer += ` Record a changed file as a new version with action "update" and just the name.`
		}
		meta := ordered.NewObject()
		meta.Set("count", len(found))
		meta.Set("artifacts", listed)
		return toolkit.Result(strconv.Itoa(len(found))+" artifact"+plural+":\n"+strings.Join(lines, "\n")+"\n"+footer, meta), nil
	})
}

// statusPart is the status part of a list line: "changed since v3" or "missing:
// <reason>"; nothing for a current file or an unknown status.
func statusPart(art artifact.Artifact, file *artifact.File) []string {
	switch {
	case file == nil:
		return nil
	case file.Status == artifact.FileMissing:
		return []string{"missing: " + file.Reason}
	case file.Status == artifact.FileChanged:
		return []string{"changed since v" + strconv.Itoa(art.Version)}
	default:
		return nil
	}
}

// fileStatuses is the live status of each artifact's tracked file (current / changed /
// missing), checked statusConcurrency at a time; a one-byte read is enough, the sandbox
// hashes the whole file. Nil for an artifact whose status could not be read, and for all
// of them when the agent has no sandbox or its accessor fails - the list is still useful
// without it.
func fileStatuses(ctx context.Context, found []artifact.Artifact, view artifact.View, tc toolhub.Context) []*artifact.File {
	out := make([]*artifact.File, len(found))
	env, err := toolkit.SandboxFor(ctx, tc)
	if err != nil || nilcheck.IsNil(env) {
		return out
	}
	sem := make(chan struct{}, statusConcurrency)
	var wg sync.WaitGroup
	for i := range found {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fileStatus(ctx, view, found[i].ID, env)
		}(i)
	}
	wg.Wait()
	return out
}

// fileStatus reads one tracked file's status; nil when the read fails (or panics).
func fileStatus(ctx context.Context, view artifact.View, id string, env sandboxhub.View) (file *artifact.File) {
	defer func() {
		if r := recover(); r != nil {
			file = nil
		}
	}()
	one := 1
	got, err := view.Read(ctx, artifact.Ref{ID: id}, env, artifact.ReadOptions{MaxBytes: &one})
	if err != nil {
		return nil
	}
	return &got.File
}

// boundedLimit is the rule of read_artifact (max_bytes) and list_artifacts (limit):
// Math.max(1, Math.floor(Math.min(requested && requested > 0 ? requested : ceiling,
// ceiling))). A requested value that is not a finite number above zero is not used.
// (Update (A-209): only the list action's limit uses it now.)
func boundedLimit(a toolkit.Args, key string, ceiling int) int {
	limit := float64(ceiling)
	if requested, ok := a.Number(key); ok && requested > 0 {
		limit = requested
	}
	limit = math.Floor(math.Min(limit, float64(ceiling)))
	return int(math.Max(1, limit))
}
