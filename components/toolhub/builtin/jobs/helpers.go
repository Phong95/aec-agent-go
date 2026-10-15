package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The caps of an agent-made trigger. The user can raise any of them on the Triggers page.
// Update (A-126): of an agent-made event or webhook job (the Jobs page); a schedule job
// is bounded by its clock.
const (
	// AgentJobDefaultRunsPerHour is the runs per hour an agent-made event or webhook job
	// gets when the agent names none.
	AgentJobDefaultRunsPerHour = 4
	// AgentJobMaxRunsPerHour is the most runs per hour the agent may set.
	AgentJobMaxRunsPerHour = 10
	// AgentJobDefaultTokensPerDay is the daily token budget an agent-made event or
	// webhook job gets when the agent names none.
	AgentJobDefaultTokensPerDay = 200_000
	// AgentJobMaxTokensPerDay is the largest daily token budget the agent may set.
	AgentJobMaxTokensPerDay = 1_000_000
)

// The non-ASCII characters of the tool texts are built from code points, so this source
// stays ASCII-only.
var (
	emDash   = string(rune(0x2014)) // U+2014 EM DASH
	enDash   = string(rune(0x2013)) // U+2013 EN DASH
	middot   = string(rune(0x00b7)) // U+00B7 MIDDLE DOT
	ellipsis = string(rune(0x2026)) // U+2026 HORIZONTAL ELLIPSIS
)

// withJobs runs fn with the agent's jobs. Every failure becomes an output the model can
// act on: no jobs on the context is a non-retryable "<tool>: jobs are not available
// here"; a *job.Error is "<tool> failed: <message>", not retryable (every code is
// deterministic); a *kernel.SDKError keeps its retryable flag; any other error is
// retryable.
func withJobs(tc toolhub.Context, name string, fn func(jobs job.Jobs) (toolhub.Output, error)) toolhub.Output {
	if nilcheck.IsNil(tc.Jobs) {
		return toolkit.Fail(name + ": jobs are not available here")
	}
	out, err := fn(tc.Jobs)
	if err == nil {
		return out
	}
	content := name + " failed: " + err.Error()
	var je *job.Error
	if errors.As(err, &je) {
		meta := ordered.NewObject()
		meta.Set("code", string(je.Code))
		return toolkit.Failure(content, false, meta)
	}
	var se *kernel.SDKError
	if errors.As(err, &se) {
		return toolkit.Failure(content, se.Retryable, ordered.Object{})
	}
	return toolhub.Error(content)
}

// findJob finds the agent's job by id, else by name.
func findJob(ctx context.Context, jobs job.Jobs, ref string) (*job.Job, error) {
	if j, err := jobs.Get(ctx, ref); err != nil || j != nil {
		return j, err
	}
	all, err := jobs.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, j := range all {
		if j.Name == ref {
			return &j, nil
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means the agent has no such job
}

// withoutSecret is the job without its webhook secret: what tool results and their
// metadata may carry.
func withoutSecret(j job.Job) job.Job {
	j = j.Clone()
	j.Secret = ""
	return j
}

// scheduleArg is the schedule from the model's flat arguments: exactly one of cron (+
// timezone), every_minutes or at. ok is false when none is given; errText says what is
// wrong.
func scheduleArg(a toolkit.Args) (w job.When, ok bool, errText string) {
	cron := str(a, "cron")
	every, hasEvery := a.Number("every_minutes")
	hasEvery = hasEvery && every > 0
	at := str(a, "at")
	given := 0
	for _, g := range []bool{cron != "", hasEvery, at != ""} {
		if g {
			given++
		}
	}
	switch {
	case given == 0:
		return job.When{}, false, ""
	case given > 1:
		return job.When{}, false, `give only one of "cron", "every_minutes" or "at"`
	case cron != "":
		return job.When{Kind: job.KindCron, Expr: cron, TZ: str(a, "timezone")}, true, ""
	case hasEvery:
		return job.When{Kind: job.KindEvery, Ms: int64(math.Round(every * 60_000))}, true, ""
	default:
		return job.When{Kind: job.KindAt, At: at}, true, ""
	}
}

// describeWhen is how a job runs, in words.
func describeWhen(j job.Job) string {
	switch {
	case j.When.IsSchedule():
		return job.Describe(j.When)
	case j.When.Kind == job.KindEvent:
		return "on your own events"
	default:
		return "on webhook POST /webhooks/jobs/" + j.ID + " (" + string(j.When.Signature) + " signature)"
	}
}

// describeJob is one block per job for the model. It never includes the secret. lastRun
// is nil when the job has not run.
func describeJob(j job.Job, lastRun *job.Run) string {
	schedule := j.When.IsSchedule()
	var state string
	switch {
	case !j.Enabled && schedule:
		state = "paused"
	case !j.Enabled:
		state = "disabled"
	case schedule:
		next := emDash
		if j.NextRunAt != nil {
			next = *j.NextRunAt
		}
		state = "next run " + next
	default:
		state = "enabled"
	}
	by := ", made by the user"
	if j.CreatedBy == job.CreatedByAgent {
		by = ", made by you"
	}
	delivery := ", results stay in each run's conversation"
	// Update (A-127): to one chat or several.
	switch {
	case len(j.Deliveries) == 1:
		delivery = ", results posted to a chat (" + string(j.Deliveries[0].When) + ")"
	case len(j.Deliveries) > 1:
		delivery = ", results posted to " + strconv.Itoa(len(j.Deliveries)) + " chats (" + string(j.Deliveries[0].When) + ")"
	}
	var parts []string
	if len(j.Filter.Types) > 0 {
		parts = append(parts, "types "+strings.Join(j.Filter.Types, ", "))
	}
	if len(j.Filter.Subjects) > 0 {
		parts = append(parts, "subjects "+strings.Join(j.Filter.Subjects, ", "))
	}
	if len(j.Filter.Where) > 0 {
		parts = append(parts, strconv.Itoa(len(j.Filter.Where))+" condition(s)")
	}
	filter := strings.Join(parts, "; ")
	var limits []string
	if j.Limits.MaxRunsPerHour != nil {
		limits = append(limits, strconv.Itoa(*j.Limits.MaxRunsPerHour)+" runs/hour")
	}
	if j.Limits.MaxTokensPerDay != nil {
		limits = append(limits, strconv.Itoa(*j.Limits.MaxTokensPerDay)+" tokens/day")
	}
	last := ""
	switch {
	case lastRun != nil:
		at := lastRun.FinishedAt
		if at == "" {
			at = lastRun.CreatedAt
		}
		silent := ""
		if lastRun.Silent {
			silent = " (silent)"
		}
		last = strings.TrimRight("\n  last run: "+string(lastRun.Status)+silent+" "+at, " \t\n")
	case j.LastRunAt != "":
		status := string(j.LastStatus)
		if status == "" {
			status = "?"
		}
		last = "\n  last run: " + status + " " + j.LastRunAt
	}
	var b strings.Builder
	b.WriteString(`- "` + j.Name + `" [` + j.ID + "] " + describeWhen(j) + ", " + state + by + delivery)
	if j.SessionMode == job.SessionReuse {
		b.WriteString(", one conversation for all runs")
	}
	if filter != "" {
		b.WriteString("\n  filter: " + filter)
	}
	if len(limits) > 0 {
		b.WriteString("\n  limits: " + strings.Join(limits, ", "))
	}
	// A-103: a tool job runs its stored call; its prompt is only the follow-up's message.
	if a := j.Action; a != nil && a.Kind == job.ActionTool {
		args, err := jsonx.Marshal(a.Args)
		if err != nil || string(args) == "null" {
			args = []byte("{}")
		}
		b.WriteString("\n  runs: " + a.Tool + " " + truncate(string(args), 200))
		switch a.FollowUp {
		case job.FollowUpAlways:
			b.WriteString(", then a turn every time")
		case job.FollowUpOnFailure:
			b.WriteString(", then a turn when the tool fails")
		default:
			b.WriteString(", no model turn")
		}
	}
	if j.Prompt != "" {
		b.WriteString("\n  prompt: " + truncate(j.Prompt, 200))
	}
	if !j.Enabled && strings.HasPrefix(j.LastError, "Paused after") {
		paused, _, _ := strings.Cut(j.LastError, ":")
		b.WriteString("\n  " + paused)
	}
	b.WriteString(last)
	return b.String()
}

// deliveryHere is the delivery for a job asked for in this turn: the chat this turn came
// from, when there is one (nil otherwise). when defaults to "always".
// Update (A-127): resolveDeliveries - the chats a job asked for in this turn posts to.
// deliver is "here" (this chat app conversation; outside one, every home chat), "home"
// (home chats) or "none"; channels names the home chats (a channel's name, platform or
// id) and alone means "home". Nothing given is the default: here, else every home chat.
// given says whether the arguments chose anything (an update leaves the deliveries
// otherwise). when defaults to "always".
func resolveDeliveries(tc toolhub.Context, deliver string, channels []string, when job.DeliveryWhen) (job.Deliveries, bool, string) {
	opts := tc.JobDeliveryOptions
	var out job.Deliveries
	switch {
	case deliver == "none":
		return nil, true, ""
	case len(channels) > 0:
		homes, errText := matchHomes(opts.Homes, channels)
		if errText != "" {
			return nil, true, errText
		}
		if deliver == "here" && opts.Here != nil {
			out = append(out, *opts.Here)
		}
		for _, d := range opts.HomeDeliveries(homes) {
			if !slices.ContainsFunc(out, func(o job.Delivery) bool { return o.ChannelID == d.ChannelID && o.Target == d.Target }) {
				out = append(out, d)
			}
		}
	case deliver == "home":
		out = opts.HomeDeliveries(opts.Homes)
	default:
		out = opts.Default()
	}
	if when == "" {
		when = job.DeliverAlways
	}
	for i := range out {
		out[i].When = when
	}
	return out, deliver != "" || len(channels) > 0, ""
}

// matchHomes picks the home chats named: by channel name, platform or id, ignoring case.
func matchHomes(homes []job.HomeChat, names []string) ([]job.HomeChat, string) {
	var picked []job.HomeChat
	for _, name := range names {
		found := false
		for _, h := range homes {
			if strings.EqualFold(h.Name, name) || strings.EqualFold(h.Platform, name) || h.ChannelID == name {
				found = true
				if !slices.Contains(picked, h) {
					picked = append(picked, h)
				}
			}
		}
		if !found {
			return nil, `no channel "` + name + `" has a home chat ` + emDash + " " + homesText(homes)
		}
	}
	return picked, ""
}

// homesText names the channels that have a home chat, for the model.
func homesText(homes []job.HomeChat) string {
	if len(homes) == 0 {
		return "no channel has a home chat yet (the user sends /sethome in the chat that should get the results)"
	}
	names := make([]string, 0, len(homes))
	for _, h := range homes {
		names = append(names, `"`+h.Name+`" (`+h.Platform+`)`)
	}
	return "channels with a home chat: " + strings.Join(names, ", ")
}

// deliveryText is what a result says about where a job's results go.
// Update (A-127): to every chat of the job; homes name the home chats.
func deliveryText(list job.Deliveries, opts job.DeliveryOptions) string {
	if len(list) == 0 {
		return "Each run's result stays in its own conversation (the user finds it on the Jobs page)."
	}
	var where []string
	for _, d := range list {
		switch {
		case opts.Here != nil && d.ChannelID == opts.Here.ChannelID && d.Target == opts.Here.Target && d.ThreadID == opts.Here.ThreadID:
			where = append(where, "this chat")
		case d.Target == job.DeliveryHome:
			name := "a channel"
			for _, h := range opts.Homes {
				if h.ChannelID == d.ChannelID {
					name = h.Name + " (" + h.Platform + ")"
				}
			}
			where = append(where, "the home chat of "+name)
		default:
			where = append(where, "a chat")
		}
	}
	in := strings.Join(where, ", ")
	switch list[0].When {
	case job.DeliverOnFailure:
		return "Failed runs will be posted in " + in + "."
	case job.DeliverOnOutput:
		return "Each run's reply will be posted in " + in + " (a [[SILENT]] reply posts nothing)."
	default:
		return "Each run's result will be posted in " + in + " (a [[SILENT]] reply posts nothing)."
	}
}

// str returns the trimmed string value of key, "" when absent or not a string.
func str(a toolkit.Args, key string) string {
	s, _ := a.String(key)
	return jsstr.Trim(s)
}

// strList is a list of strings from a string array or a comma-separated string, trimmed,
// blanks dropped; nil when there is none.
func strList(a toolkit.Args, key string) []string {
	var items []string
	if s, ok := a.String(key); ok {
		items = strings.Split(s, ",")
	} else {
		items = a.Strings(key)
	}
	var out []string
	for _, item := range items {
		if v := jsstr.Trim(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// conditions are the "where" conditions; an "in" condition takes its list from "values".
// The job manager validates the rest. Nil when "where" is not a list or is empty.
func conditions(a toolkit.Args) []job.Condition {
	raw, ok := a.Raw("where")
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil
	}
	out := make([]job.Condition, 0, len(items))
	for _, item := range items {
		var v struct {
			Path   any   `json:"path"`
			Op     any   `json:"op"`
			Value  any   `json:"value"`
			Values []any `json:"values"`
		}
		// A value that is not an object stays an empty condition, which the manager
		// rejects.
		_ = json.Unmarshal(item, &v)
		path, _ := v.Path.(string)
		op, _ := v.Op.(string)
		c := job.Condition{Path: path, Op: job.ConditionOp(op), Value: v.Value}
		if c.Op == job.OpIn && v.Values != nil {
			c.Value = v.Values
		}
		out = append(out, c)
	}
	return out
}

// integer is the rounded number value of key; false when absent, not a number, or not
// positive (0 means "not given").
func integer(a toolkit.Args, key string) (int, bool) {
	f, ok := a.Number(key)
	if !ok || f <= 0 {
		return 0, false
	}
	// Clamped so a huge number stays out of range rather than wrapping.
	return int(math.Max(math.Min(math.Round(f), math.MaxInt32), math.MinInt32)), true
}

// truncate keeps the first n characters of s and marks the cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + ellipsis
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }
