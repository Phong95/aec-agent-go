package jobs

import (
	"context"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ListTool is list_triggers: the agent's triggers with their source, filter, delivery
// and last run. Read-only, so it never needs approval (unlike manage_trigger). It never
// shows a webhook secret.
// Update (A-126): list_jobs - every job (schedule, event, webhook); it replaces
// list_triggers and list_scheduled_jobs. Build one with NewListTool. The tool holds no
// state, so it is safe for concurrent use.
type ListTool struct{}

var _ toolhub.Handler = (*ListTool)(nil)

// NewListTool returns the list_jobs tool.
func NewListTool() *ListTool { return &ListTool{} }

// Name returns the plugin name.
func (*ListTool) Name() string { return "tool:list-jobs" }

// Version returns the plugin version.
func (*ListTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call). Its required
// list is empty, not absent.
func (*ListTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "list_jobs",
		Description: "List your jobs " + emDash + " name, id, when it runs (schedule, event or webhook), next run, filter, whether results are posted to a chat, " +
			"limits, prompt and the last run. Check this before creating a job, so you update an existing one instead of making a duplicate.",
		Parameters: toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties(), Required: []string{}},
		Risk:       toolhub.RiskReadOnly,
	}
}

// Execute lists the agent's jobs.
func (*ListTool) Execute(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	return withJobs(tc, "list_jobs", func(jobs job.Jobs) (toolhub.Output, error) {
		all, err := jobs.List(ctx)
		if err != nil {
			return toolhub.Output{}, err
		}
		meta := ordered.NewObject()
		meta.Set("count", len(all))
		if len(all) == 0 {
			return toolkit.Result("You have no jobs. Use manage_job to create one.", meta), nil
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
	}), nil
}
