package jobs

import (
	"context"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// listJobs is list_triggers: the agent's triggers with their source, filter, delivery
// and last run. Read-only, so it never needs approval (unlike manage_trigger). It never
// shows a webhook secret.
// Update (A-126): list_jobs - every job (schedule, event, webhook); it replaces
// list_triggers and list_scheduled_jobs.
// Update (A-211): the job tool's list action (list_jobs is gone).
func listJobs(ctx context.Context, tc toolhub.Context) toolhub.Output {
	return withJobs(tc, "job list", func(jobs job.Jobs) (toolhub.Output, error) {
		all, err := jobs.List(ctx)
		if err != nil {
			return toolhub.Output{}, err
		}
		meta := ordered.NewObject()
		meta.Set("count", len(all))
		if len(all) == 0 {
			return toolkit.Result(`You have no jobs. Create one with action "create".`, meta), nil
		}
		lines := make([]string, len(all))
		clean := make([]job.Job, len(all))
		for i, j := range all {
			runs, err := jobs.ListRuns(ctx, j.ID, 1)
			if err != nil {
				return toolhub.Output{}, err
			}
			var last *job.Run
			if len(runs) > 0 {
				last = &runs[0]
			}
			lines[i] = describeJob(j, last)
			clean[i] = withoutSecret(j)
		}
		meta.Set("jobs", clean)
		plural := "s"
		if len(all) == 1 {
			plural = ""
		}
		return toolkit.Result(strconv.Itoa(len(all))+" job"+plural+":\n"+strings.Join(lines, "\n"), meta), nil
	})
}
