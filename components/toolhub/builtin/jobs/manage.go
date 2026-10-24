package jobs

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// actions are the actions of manage_job, in the order of its enum.
var actions = []string{"create", "update", "enable", "disable", "delete", "run_now"}

// whens are the values of "when".
var whens = []string{"schedule", "event", "webhook"}

// sessionModes are the values of "session_mode".
var sessionModes = []string{string(job.SessionNew), string(job.SessionReuse)}

// deliveryWhen are the values of deliver_when.
var deliveryWhen = []string{string(job.DeliverAlways), string(job.DeliverOnOutput), string(job.DeliverOnFailure)}

// followUps are the values of follow_up (A-103).
var followUps = []string{string(job.FollowUpNever), string(job.FollowUpOnFailure), string(job.FollowUpAlways)}

// ManageTool is manage_trigger: lets an agent set up work that starts when something
// happens: "when a scheduled job fails, find out why", "report whenever a new model is
// uploaded" (a webhook from that system). Each run is a full agent turn with the
// trigger's prompt plus the event data, in a new session the user can open later.
//
// Update (A-126): manage_job - schedule_job (A-69) and manage_trigger in one tool: a job
// runs on a schedule, on the agent's own events, or on a webhook.
//
// Update (A-103): a schedule job may run one tool call instead of a turn (tool,
// tool_args, follow_up) - the exact call is in this tool's arguments, so the user
// approves exactly what will run unattended.
//
// Risk "consequential": a job makes the agent act (and spend) later without the user
// watching, so every call waits for the user's approval. Job runs deny every approval, so
// an unattended run (and the event data it reads) can never create or change jobs. What
// the agent may set on an event or webhook job is capped: no chains (maxDepth 0), at most
// AgentJobMaxRunsPerHour runs an hour and a daily token budget; the user can change any
// of it on the Jobs page. Build one with NewManageTool. The tool holds no state, so it is
// safe for concurrent use.
type ManageTool struct{}

var _ toolhub.Handler = (*ManageTool)(nil)

// NewManageTool returns the manage_job tool.
func NewManageTool() *ManageTool { return &ManageTool{} }

// Name returns the plugin name.
func (*ManageTool) Name() string { return "tool:manage-job" }

// Version returns the plugin version.
func (*ManageTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call).
func (*ManageTool) Definition() toolhub.Definition {
	str := func(desc string) toolhub.JSONSchema { return toolhub.JSONSchema{Type: "string", Description: desc} }
	return toolhub.Definition{
		Name: "manage_job",
		Description: "Set up work for yourself that runs later without the user in the chat " + emDash + " each run is a full turn with your tools, " +
			"in a conversation the user can open later. A job runs on a schedule (cron / every_minutes / at), " +
			`on your own events (when: "event" ` + emDash + " " + strings.Join(job.InternalEventTypes(), ", ") + "; give event_types), " +
			`or when another system POSTs a signed webhook (when: "webhook", e.g. "model uploaded"). ` +
			"Actions: create (name, prompt, and a schedule or when) " + middot + " update (job, then fields to change) " + middot + " enable / disable / delete / run_now (job). " +
			"The prompt must stand on its own: a run does NOT see this conversation, so include every detail it needs. " +
			`For "tell me when X happens" use ONE schedule job that checks for X and replies with what it found, or exactly [[SILENT]] ` +
			`when there is nothing new (nothing is posted), or [[FAILED]] <reason> when it could not check; use session_mode "reuse" ` +
			"so it remembers what it already reported. Never chain a schedule job into an event job just to filter its reply. " +
			`Use an event job for "when one of my jobs fails, look into it", a webhook job when an outside system can notify. ` +
			`To run the SAME command or script on a schedule (e.g. "run report.py every 3 hours"), make a tool job: give tool ` +
			`(e.g. "terminal") and tool_args (its exact arguments) ` + emDash + ` it runs without you, uses no tokens, and its output is the ` +
			`result (no output = nothing posted). Add follow_up "on-failure" or "always" with a prompt to get a turn after the tool. ` +
			"Prompts may use {{now}}, {{job.lastRunAt}}, {{job.lastSuccessAt}} and {{job.lastResult}} (the previous result). " +
			"When asked from a chat app, results are posted back to that chat. Approval-gated tools are denied during runs. " +
			"Each call asks the user for approval. Use list_jobs to see existing jobs.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "action", Schema: toolhub.JSONSchema{Type: "string", Enum: slices.Clone(actions), Description: "What to do"}},
				toolhub.Property{Name: "job", Schema: str("update/enable/disable/delete/run_now: the job's name or id")},
				toolhub.Property{Name: "name", Schema: str("create: a short unique name; update: a new name")},
				toolhub.Property{Name: "description", Schema: str("create/update: what the job is for, for the user")},
				toolhub.Property{Name: "prompt", Schema: str("create/update: the full, self-contained instruction you will receive on each run. Event and webhook jobs may use " +
					"{{event.type}}, {{event.subject}}, {{events.count}}, {{job.name}}; the event data itself is attached separately")},
				toolhub.Property{Name: "when", Schema: toolhub.JSONSchema{
					Type: "string", Enum: slices.Clone(whens),
					Description: `create/update: "schedule" (give cron, every_minutes or at), "event" or "webhook"`,
				}},
				toolhub.Property{Name: "cron", Schema: str(`schedule: 5-field cron "minute hour day-of-month month day-of-week", e.g. "0 8 * * 1" = Mondays 08:00, "*/30 9-17 * * 1-5" = every 30 min in office hours`)},
				toolhub.Property{Name: "timezone", Schema: str(`With cron: IANA timezone, e.g. "Asia/Ho_Chi_Minh". Ask the user if unsure.`)},
				toolhub.Property{Name: "every_minutes", Schema: toolhub.JSONSchema{Type: "number", Description: "schedule: run every N minutes (at least 1)"}},
				toolhub.Property{Name: "at", Schema: str(`schedule: run once at this ISO 8601 time with offset, e.g. "2026-10-07T09:00:00+07:00"`)},
				toolhub.Property{Name: "tool", Schema: str(`create/update, schedule jobs only: run this tool instead of a turn (e.g. "terminal"); it must be one of your tools ` +
					`that can be scheduled. "none" turns a tool job back into a prompt job`)},
				toolhub.Property{Name: "tool_args", Schema: str(`With tool: its exact arguments as a JSON object, e.g. {"command": "python report.py"}. They are never changed at run time`)},
				toolhub.Property{Name: "follow_up", Schema: toolhub.JSONSchema{
					Type: "string", Enum: slices.Clone(followUps),
					Description: `With tool: also run a turn with the prompt after the tool ` + emDash + ` "never" (default), "on-failure" or "always"`,
				}},
				toolhub.Property{Name: "session_mode", Schema: toolhub.JSONSchema{
					Type: "string", Enum: slices.Clone(sessionModes),
					Description: `create/update: "new" = a new conversation per run (default), "reuse" = every run appends to one conversation`,
				}},
				toolhub.Property{Name: "event_types", Schema: toolhub.JSONSchema{
					Type: "array", Items: &toolhub.JSONSchema{Type: "string"},
					Description: `event/webhook: event types to accept; "prefix.*" matches a prefix. Required for when "event"`,
				}},
				toolhub.Property{Name: "subjects", Schema: toolhub.JSONSchema{
					Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: "event/webhook: only events about these subjects (e.g. one job's id)",
				}},
				toolhub.Property{Name: "where", Schema: toolhub.JSONSchema{
					Type:        "array",
					Description: `event/webhook: conditions that must all hold, e.g. [{ "path": "data.project", "op": "eq", "value": "tower-a" }]`,
					Items: &toolhub.JSONSchema{
						Type: "object",
						Properties: toolhub.NewProperties(
							toolhub.Property{Name: "path", Schema: str(`"type", "subject", "source" or "data.<dot.path>"`)},
							toolhub.Property{Name: "op", Schema: toolhub.JSONSchema{Type: "string", Enum: []string{"eq", "neq", "in", "exists", "contains"}}},
							toolhub.Property{Name: "value", Schema: str("eq / neq / contains: the value (numbers compare as text)")},
							toolhub.Property{Name: "values", Schema: toolhub.JSONSchema{Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: "in: the accepted values"}},
						),
						Required: []string{"path", "op"},
					},
				}},
				toolhub.Property{Name: "signature", Schema: toolhub.JSONSchema{
					Type: "string", Enum: []string{"standard", "github"},
					Description: `webhook: how deliveries are signed ` + emDash + ` "standard" (Standard Webhooks, default) or "github"`,
				}},
				toolhub.Property{Name: "subject_path", Schema: str(`webhook: where the subject is in the body, e.g. "data.modelId"`)},
				toolhub.Property{Name: "revision_path", Schema: str(`webhook: a growing revision number in the body, e.g. "data.revision"`)},
				toolhub.Property{Name: "max_runs_per_hour", Schema: toolhub.JSONSchema{
					Type:        "number",
					Description: "event/webhook: at most " + strconv.Itoa(AgentJobMaxRunsPerHour) + " (default " + strconv.Itoa(AgentJobDefaultRunsPerHour) + "). Not used by schedule jobs",
				}},
				toolhub.Property{Name: "max_tokens_per_day", Schema: toolhub.JSONSchema{
					Type:        "number",
					Description: "event/webhook: daily token budget, at most " + strconv.Itoa(AgentJobMaxTokensPerDay) + " (default " + strconv.Itoa(AgentJobDefaultTokensPerDay) + "). Not used by schedule jobs",
				}},
				toolhub.Property{Name: "deliver", Schema: toolhub.JSONSchema{
					Type: "string", Enum: []string{"here", "home", "none"},
					Description: `"here" = post each result to the chat app conversation you are in (the default there; in any other conversation ` +
						`it means "home"), "home" = post to the home chats of the agent's chat app channels (all of them, or those in ` +
						`deliver_channels; the default outside a chat app), "none" = keep results in each run's conversation`,
				}},
				toolhub.Property{Name: "deliver_channels", Schema: toolhub.JSONSchema{
					Type: "array", Items: &toolhub.JSONSchema{Type: "string"},
					Description: `Only these channels' home chats get the results (a channel's name or platform, e.g. "telegram"). ` +
						`Leave it out for all of them. Ask the user when they did not say which`,
				}},
				toolhub.Property{Name: "deliver_when", Schema: toolhub.JSONSchema{
					Type: "string", Enum: slices.Clone(deliveryWhen),
					Description: "With a delivery: post always (default), only with output, or only on failure",
				}},
			),
			Required: []string{"action"},
		},
		Risk: toolhub.RiskConsequential,
		// A-137: no lasting changes from a subagent.
		Subagent: toolhub.SubagentNever,
	}
}

// Execute runs one action against the agent's jobs.
//
//nolint:gocyclo // one linear pass over the actions, the same steps as core/ts
func (*ManageTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	action := str(a, "action")
	if !slices.Contains(actions, action) {
		return toolkit.Fail(`manage_job: "action" must be one of ` + strings.Join(actions, ", ")), nil
	}
	return withJobs(tc, "manage_job", func(jobs job.Jobs) (toolhub.Output, error) {
		deliverWhen := job.DeliveryWhen(str(a, "deliver_when"))
		if deliverWhen != "" && !slices.Contains(deliveryWhen, string(deliverWhen)) {
			return toolkit.Fail("manage_job " + action + `: "deliver_when" must be one of ` + strings.Join(deliveryWhen, ", ")), nil
		}
		deliver := str(a, "deliver")
		if deliver != "" && deliver != "here" && deliver != "home" && deliver != "none" {
			return toolkit.Fail("manage_job " + action + `: "deliver" must be "here", "home" or "none"`), nil
		}
		// A-127: the chats the arguments ask for (every home chat when they name none).
		deliveries, deliveryGiven, deliveryErr := resolveDeliveries(tc, deliver, strList(a, "deliver_channels"), deliverWhen)
		if deliveryErr != "" {
			return toolkit.Fail("manage_job " + action + ": " + deliveryErr), nil
		}
		sessionMode := job.SessionMode(str(a, "session_mode"))
		if sessionMode != "" && !slices.Contains(sessionModes, string(sessionMode)) {
			return toolkit.Fail("manage_job " + action + `: "session_mode" must be "new" or "reuse"`), nil
		}
		limits, limitsErr := limitsArg(a)
		if limitsErr != "" {
			return toolkit.Fail("manage_job " + action + ": " + limitsErr), nil
		}
		toolAction, toolErr := toolActionArg(a)
		if toolErr != "" {
			return toolkit.Fail("manage_job " + action + ": " + toolErr), nil
		}
		// "here" outside a chat app has nowhere to post: the job keeps its results in its
		// runs' conversations.
		// Update (A-127): nor has "home" when no channel has a home chat.
		deliveryIgnored := (deliver == "here" || deliver == "home") && len(deliveries) == 0

		if action == "create" {
			name, prompt := str(a, "name"), str(a, "prompt")
			if name == "" {
				return toolkit.Fail(`manage_job create: "name" is required`), nil
			}
			var tool *toolActionInput
			if toolAction != nil && !toolAction.prompt {
				if toolAction.tool == "" {
					return toolkit.Fail(`manage_job create: "tool_args" and "follow_up" go with "tool"`), nil
				}
				tool = toolAction
			}
			// A tool job needs a prompt only for its follow-up turn.
			if prompt == "" && (tool == nil || tool.followUp != "") {
				if tool != nil {
					return toolkit.Fail(`manage_job create: a follow-up needs a "prompt" ` + emDash + ` the message of the turn after the tool`), nil
				}
				return toolkit.Fail(`manage_job create: "prompt" is required`), nil
			}
			when, hasWhen, whenErr := whenArg(a, nil)
			if whenErr != "" {
				return toolkit.Fail("manage_job create: " + whenErr), nil
			}
			if !hasWhen {
				return toolkit.Fail(`manage_job create: give a schedule (one of "cron", "every_minutes" or "at"), or when "event" or "webhook"`), nil
			}
			in := job.CreateInput{
				Name:               name,
				Prompt:             prompt,
				Description:        str(a, "description"),
				When:               when,
				SessionMode:        sessionMode,
				CreatedBy:          job.CreatedByAgent,
				CreatedInSessionID: tc.SessionID,
			}
			if tool != nil {
				in.Action = tool.action()
			}
			if !when.IsSchedule() {
				filter := filterArg(a, nil)
				in.Filter = &filter
				// A schedule job is bounded by its clock; an event or webhook job gets the
				// agent caps.
				runs, tokens := AgentJobDefaultRunsPerHour, AgentJobDefaultTokensPerDay
				if limits.MaxRunsPerHour != nil {
					runs = *limits.MaxRunsPerHour
				}
				if limits.MaxTokensPerDay != nil {
					tokens = *limits.MaxTokensPerDay
				}
				in.Limits = &job.LimitsInput{
					MaxRunsPerHour:    intPtr(runs),
					MaxTokensPerDay:   intPtr(tokens),
					MaxConcurrentRuns: intPtr(1),
					// No chains: an event caused by a run of this job never wakes a job.
					MaxDepth: intPtr(0),
				}
			}
			// A-100 phase 3: scheduled from a chat - each run's result is posted back there.
			// Update (A-127): or, outside a chat app, to every home chat (or those named).
			in.Deliveries = deliveries
			created, err := jobs.Create(ctx, in)
			if err != nil {
				return toolhub.Output{}, err
			}
			return toolkit.Result(createdText(created, deliveryIgnored, tc.JobDeliveryOptions), jobMeta(action, created)), nil
		}

		ref := str(a, "job")
		if ref == "" {
			return toolkit.Fail("manage_job " + action + `: "job" (the job's name or id) is required`), nil
		}
		existing, err := findJob(ctx, jobs, ref)
		if err != nil {
			return toolhub.Output{}, err
		}
		if existing == nil {
			return toolkit.Fail("manage_job " + action + `: you have no job "` + ref + `" ` + emDash + " use list_jobs"), nil
		}

		switch action {
		case "delete":
			if err := jobs.Delete(ctx, existing.ID); err != nil {
				return toolhub.Output{}, err
			}
			meta := ordered.NewObject()
			meta.Set("action", action)
			meta.Set("jobId", existing.ID)
			return toolkit.Result(`Deleted job "`+existing.Name+`" with its event and run history.`, meta), nil
		case "run_now":
			run, err := jobs.RunNow(ctx, existing.ID)
			if err != nil {
				return toolhub.Output{}, err
			}
			where := ""
			if len(existing.Deliveries) > 0 {
				where = " and in its delivery chats"
			}
			meta := ordered.NewObject()
			meta.Set("action", action)
			meta.Set("jobId", existing.ID)
			meta.Set("runId", run.ID)
			return toolkit.Result(`Started a run of "`+existing.Name+`" [run `+run.ID+`]. Its result shows up in the run's conversation`+where+".", meta), nil
		}

		var patch job.UpdateInput
		switch action {
		case "enable":
			patch.Enabled = boolPtr(true)
		case "disable":
			patch.Enabled = boolPtr(false)
		default:
			changed := false
			if name := str(a, "name"); name != "" && name != existing.Name {
				patch.Name, changed = &name, true
			}
			if prompt := str(a, "prompt"); prompt != "" && prompt != existing.Prompt {
				patch.Prompt, changed = &prompt, true
			}
			if description := str(a, "description"); description != "" && description != existing.Description {
				patch.Description, changed = &description, true
			}
			if sessionMode != "" && sessionMode != existing.SessionMode {
				patch.SessionMode, changed = &sessionMode, true
			}
			clearAction, nextAction, actionErr := mergeToolAction(toolAction, existing.Action)
			if actionErr != "" {
				return toolkit.Fail("manage_job update: " + actionErr), nil
			}
			if clearAction {
				patch.ClearAction, changed = true, true
			} else if nextAction != nil {
				patch.Action, changed = nextAction, true
			}
			when, hasWhen, whenErr := whenArg(a, &existing.When)
			if whenErr != "" {
				return toolkit.Fail("manage_job update: " + whenErr), nil
			}
			if hasWhen {
				patch.When, changed = &when, true
			}
			nextWhen := existing.When
			if hasWhen {
				nextWhen = when
			}
			if !nextWhen.IsSchedule() {
				var current *job.Filter
				fromClock := hasWhen && existing.When.IsSchedule()
				if !fromClock {
					current = &existing.Filter
				}
				filter := filterArg(a, current)
				if !sameFilter(filter, existing.Filter) || fromClock {
					patch.Filter, changed = &filter, true
				}
				if limits.MaxRunsPerHour != nil && !sameInt(limits.MaxRunsPerHour, existing.Limits.MaxRunsPerHour) {
					patch.Limits = withLimits(patch.Limits)
					patch.Limits.MaxRunsPerHour, changed = limits.MaxRunsPerHour, true
				}
				if limits.MaxTokensPerDay != nil && !sameInt(limits.MaxTokensPerDay, existing.Limits.MaxTokensPerDay) {
					patch.Limits = withLimits(patch.Limits)
					patch.Limits.MaxTokensPerDay, changed = limits.MaxTokensPerDay, true
				}
			}
			// Update (A-127): the deliveries are replaced as a whole.
			switch {
			case deliver == "none":
				if len(existing.Deliveries) > 0 {
					patch.ClearDeliveries, changed = true, true
				}
			case deliveryGiven && len(deliveries) > 0:
				if deliverWhen == "" && len(existing.Deliveries) > 0 {
					for i := range deliveries {
						deliveries[i].When = existing.Deliveries[0].When
					}
				}
				patch.Deliveries, changed = deliveries, true
			case deliverWhen != "" && len(existing.Deliveries) > 0:
				next := slices.Clone(existing.Deliveries)
				for i := range next {
					next[i].When = deliverWhen
				}
				if !slices.Equal(next, existing.Deliveries) {
					patch.Deliveries, changed = next, true
				}
			}
			if !changed {
				return toolkit.Fail("manage_job update: nothing to change " + emDash + " give a new name, prompt, schedule, when, tool, filter, session_mode, limits or a delivery"), nil
			}
		}
		updated, err := jobs.Update(ctx, existing.ID, patch)
		if err != nil {
			return toolhub.Output{}, err
		}
		text := "Updated:\n" + describeJob(updated, nil)
		if deliveryIgnored && action == "update" {
			text += "\n" + deliveryText(updated.Deliveries, tc.JobDeliveryOptions)
		}
		return toolkit.Result(text, jobMeta(action, updated)), nil
	}), nil
}

// toolActionInput is the tool part of the arguments (A-103), with which parts were given
// (for an update).
type toolActionInput struct {
	// prompt: tool "none" - turn a tool job back into a prompt job.
	prompt     bool
	tool       string
	args       map[string]any
	followUp   job.FollowUp
	argsGiven  bool
	followUpOK bool
}

// action is the job.Action of a parsed tool argument.
func (t *toolActionInput) action() *job.Action {
	return &job.Action{Kind: job.ActionTool, Tool: t.tool, Args: t.args, FollowUp: t.followUp}
}

const toolArgsError = `"tool_args" must be a JSON object, e.g. {"command": "python report.py"}`

// toolActionArg is the tool part of the arguments: a tool action, a prompt action for
// tool "none", a tool action with tool "" when only tool_args / follow_up were given, nil
// when none of them was, or why it is refused (A-103).
func toolActionArg(a toolkit.Args) (*toolActionInput, string) {
	tool := str(a, "tool")
	followUp := str(a, "follow_up")
	if followUp != "" && !slices.Contains(followUps, followUp) {
		return nil, `"follow_up" must be one of ` + strings.Join(followUps, ", ")
	}
	var toolArgs map[string]any
	given := false
	if s, ok := a.String("tool_args"); ok {
		if jsstr.Trim(s) != "" {
			given = true
			if json.Unmarshal([]byte(s), &toolArgs) != nil || toolArgs == nil {
				return nil, toolArgsError
			}
		}
	} else if raw, ok := a.Raw("tool_args"); ok && string(raw) != "null" {
		given = true
		if json.Unmarshal(raw, &toolArgs) != nil || toolArgs == nil {
			return nil, toolArgsError
		}
	}
	if tool == "none" {
		return &toolActionInput{prompt: true}, ""
	}
	// Strict-schema models fill every field: without a tool, "never" and {} say nothing.
	argsGiven := given && (tool != "" || len(toolArgs) > 0)
	followUpGiven := followUp != "" && (tool != "" || followUp != string(job.FollowUpNever))
	if tool == "" && !argsGiven && !followUpGiven {
		return nil, ""
	}
	if toolArgs == nil {
		toolArgs = map[string]any{}
	}
	out := &toolActionInput{tool: tool, args: toolArgs, argsGiven: argsGiven, followUpOK: followUpGiven}
	if followUp != "" && followUp != string(job.FollowUpNever) {
		out.followUp = job.FollowUp(followUp)
	}
	return out, ""
}

// mergeToolAction is an update's new action (A-103): the given parts over the job's
// current tool action. toPrompt turns a tool job back into a prompt job; next nil and toPrompt
// false mean unchanged.
func mergeToolAction(in *toolActionInput, current *job.Action) (toPrompt bool, next *job.Action, errText string) {
	if in == nil {
		return false, nil, ""
	}
	var base *job.Action
	if current != nil && current.Kind == job.ActionTool {
		base = current
	}
	if in.prompt {
		return base != nil, nil, ""
	}
	tool := in.tool
	if tool == "" && base != nil {
		tool = base.Tool
	}
	if tool == "" {
		return false, nil, `"tool_args" and "follow_up" go with "tool" ` + emDash + " this job runs a prompt"
	}
	// A different tool starts from its own arguments; the same tool keeps what wasn't given.
	sameTool := base != nil && base.Tool == tool
	args, followUp := in.args, in.followUp
	if sameTool && !in.argsGiven {
		args = base.Args
	}
	if sameTool && !in.followUpOK {
		followUp = base.FollowUp
	}
	n := &job.Action{Kind: job.ActionTool, Tool: tool, Args: args, FollowUp: followUp}
	if base != nil {
		x, errX := jsonx.Marshal(base)
		y, errY := jsonx.Marshal(n)
		if errX == nil && errY == nil && string(x) == string(y) {
			return false, nil, ""
		}
	}
	return false, n, ""
}

// limitsArg is the capped limits the agent asked for, or why they are refused.
func limitsArg(a toolkit.Args) (job.LimitsInput, string) {
	var out job.LimitsInput
	if runs, ok := integer(a, "max_runs_per_hour"); ok {
		if runs > AgentJobMaxRunsPerHour {
			return job.LimitsInput{}, `"max_runs_per_hour" must be 1` + enDash + strconv.Itoa(AgentJobMaxRunsPerHour) + "; the user can raise it on the Jobs page"
		}
		out.MaxRunsPerHour = intPtr(runs)
	}
	if tokens, ok := integer(a, "max_tokens_per_day"); ok {
		if tokens < 1_000 || tokens > AgentJobMaxTokensPerDay {
			return job.LimitsInput{}, `"max_tokens_per_day" must be 1000` + enDash + strconv.Itoa(AgentJobMaxTokensPerDay) + "; the user can raise it on the Jobs page"
		}
		out.MaxTokensPerDay = intPtr(tokens)
	}
	return out, ""
}

// whenArg is the When from the flat arguments: a schedule (cron / every_minutes / at),
// "event" or "webhook". For an update (current set), ok is false when nothing about it
// changes; errText says when it does not add up.
func whenArg(a toolkit.Args, current *job.When) (w job.When, ok bool, errText string) {
	schedule, hasSchedule, schedErr := scheduleArg(a)
	if schedErr != "" {
		return job.When{}, false, schedErr
	}
	kind := str(a, "when")
	if kind != "" && !slices.Contains(whens, kind) {
		return job.When{}, false, `"when" must be one of ` + strings.Join(whens, ", ")
	}
	if hasSchedule {
		if kind != "" && kind != "schedule" {
			return job.When{}, false, `"cron", "every_minutes" and "at" are for when "schedule", not "` + kind + `"`
		}
		return schedule, true, ""
	}
	currentKind := ""
	if current != nil {
		currentKind = string(current.Kind)
		if current.IsSchedule() {
			currentKind = "schedule"
		}
	}
	wanted := kind
	if wanted == "" {
		wanted = currentKind
	}
	switch wanted {
	case "schedule", "":
		if current == nil || currentKind != "schedule" {
			return job.When{}, false, `a schedule job needs one of "cron", "every_minutes" or "at"`
		}
		return job.When{}, false, ""
	case "event":
		if currentKind == "event" {
			return job.When{}, false, ""
		}
		return job.When{Kind: job.KindEvent}, true, ""
	}
	// A webhook: build it on top of the current one, and only report a change.
	var base *job.When
	if current != nil && current.Kind == job.KindWebhook {
		base = current
	}
	next := job.When{Kind: job.KindWebhook, Signature: job.SignatureScheme(str(a, "signature")), SubjectPath: str(a, "subject_path"), RevisionPath: str(a, "revision_path")}
	if base != nil {
		if next.Signature == "" {
			next.Signature = base.Signature
		}
		if next.SubjectPath == "" {
			next.SubjectPath = base.SubjectPath
		}
		if next.RevisionPath == "" {
			next.RevisionPath = base.RevisionPath
		}
	}
	if next.Signature == "" {
		next.Signature = job.SignatureStandard
	}
	if base != nil && base.Signature == next.Signature && base.SubjectPath == next.SubjectPath && base.RevisionPath == next.RevisionPath {
		return job.When{}, false, ""
	}
	return next, true, ""
}

// filterArg is the filter from the flat arguments; a part not given (or empty) keeps its
// current value.
func filterArg(a toolkit.Args, current *job.Filter) job.Filter {
	var out job.Filter
	out.Types = strList(a, "event_types")
	out.Subjects = strList(a, "subjects")
	out.Where = conditions(a)
	if current != nil {
		if len(out.Types) == 0 {
			out.Types = current.Types
		}
		if len(out.Subjects) == 0 {
			out.Subjects = current.Subjects
		}
		if len(out.Where) == 0 {
			out.Where = current.Where
		}
	}
	return out.Clone()
}

func sameFilter(a, b job.Filter) bool {
	x, errX := jsonx.Marshal(a)
	y, errY := jsonx.Marshal(b)
	return errX == nil && errY == nil && string(x) == string(y)
}

func sameInt(a, b *int) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func withLimits(l *job.LimitsInput) *job.LimitsInput {
	if l == nil {
		return &job.LimitsInput{}
	}
	return l
}

// createdText is what the model hears about a new job. It never includes the secret.
func createdText(j job.Job, deliveryIgnored bool, opts job.DeliveryOptions) string {
	var lines []string
	if j.When.IsSchedule() {
		first := "disabled"
		if j.NextRunAt != nil {
			first = "first run " + *j.NextRunAt
		}
		lines = []string{`Scheduled "` + j.Name + `" [` + j.ID + "] " + emDash + " " + describeWhen(j) + ", " + first + "."}
	} else {
		state := "disabled"
		if j.Enabled {
			state = "enabled"
		}
		lines = []string{`Created job "` + j.Name + `" (` + state + ").", describeJob(j, nil)}
	}
	if j.When.Kind == job.KindWebhook {
		lines = append(lines, "To connect it, the sending system must POST events to /webhooks/jobs/"+j.ID+" on this server, signed with the "+
			"job's secret ("+string(j.When.Signature)+" signature). You don't have the secret: ask the user to copy the full address "+
			"and the secret from the agent's Jobs page into the sending system.")
	}
	if deliveryIgnored {
		lines = append(lines, "Nothing can be posted: this conversation is not in a chat app and no channel has a home chat "+
			"(the user sends /sethome in the chat that should get the results).")
	}
	lines = append(lines, deliveryText(j.Deliveries, opts), "The user can see, edit, disable or delete it on the agent's Jobs page.")
	return strings.Join(lines, "\n")
}

// jobMeta is the metadata of an output about one job: the action and the job without
// its secret.
func jobMeta(action string, j job.Job) ordered.Object {
	meta := ordered.NewObject()
	meta.Set("action", action)
	meta.Set("job", withoutSecret(j))
	return meta
}
