package agent

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/internal/jobkit"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Trigger configuration rules (A-70): validation, defaults, the webhook envelope and
// the agent's own SDK events as trigger events. Pure functions except resolvedSince,
// which reads the scheduled job store. The matching.ts, webhook.ts (parse) and
// internal-events.ts of core/ts.
//
// Update (A-126): job rules - the trigger rules plus a schedule job's (A-69): one When
// (schedule, event or webhook) and defaults per kind (the rules.ts of core/ts).
// resolvedSince reads the job store.
// ---------------------------------------------------------------------------

const (
	hourMs = 60 * 60_000
	dayMs  = 24 * hourMs
)

var (
	conditionPathRe = regexp.MustCompile(`^(type|subject|source|data(\.[A-Za-z0-9_\-]+)*)$`)
	mappingPathRe   = regexp.MustCompile(`^(subject|data(\.[A-Za-z0-9_\-]+)*)$`)
)

var conditionOps = []job.ConditionOp{job.OpEq, job.OpNeq, job.OpIn, job.OpExists, job.OpContains}

func jobInvalid(message, id string) *job.Error {
	return job.NewError(message, job.CodeInvalidArgument, id)
}

func validateJobName(v, id string) (string, error) {
	name := jsstr.Trim(v)
	if name == "" {
		return "", jobInvalid(`A job needs a "name"`, id)
	}
	if jobkit.UTF16Len(name) > job.MaxNameLength {
		return "", jobInvalid("Job name is longer than "+strconv.Itoa(job.MaxNameLength)+" characters", id)
	}
	return name, nil
}

func validateJobPrompt(v, id string) (string, error) {
	prompt := jsstr.Trim(v)
	if prompt == "" {
		return "", jobInvalid(`A job needs a "prompt" `+emDash+` the message the agent receives on each run`, id)
	}
	if jobkit.UTF16Len(prompt) > job.MaxPromptLength {
		return "", jobInvalid("Job prompt is longer than "+strconv.Itoa(job.MaxPromptLength)+" characters", id)
	}
	return prompt, nil
}

func validateJobDescription(v, id string) (string, error) {
	description := jsstr.Trim(v)
	if jobkit.UTF16Len(description) > job.MaxDescriptionLength {
		return "", jobInvalid("Job description is longer than "+strconv.Itoa(job.MaxDescriptionLength)+" characters", id)
	}
	return description, nil
}

func validateSessionMode(m job.SessionMode, id string) error {
	if m != job.SessionNew && m != job.SessionReuse {
		return jobInvalid(`"sessionMode" must be "new" or "reuse"`, id)
	}
	return nil
}

// validateJobWhen checks a schedule ("at" / "every" / "cron"), "event" or "webhook".
func validateJobWhen(w job.When, now time.Time, id string) (job.When, error) {
	switch w.Kind {
	case job.KindCron, job.KindEvery, job.KindAt:
		s, err := ValidateSchedule(w, now)
		if err != nil {
			if je, ok := err.(*job.Error); ok && id != "" { //nolint:errorlint // ValidateSchedule returns *job.Error itself
				return job.When{}, job.NewError(je.Message, je.Code, id)
			}
			return job.When{}, err
		}
		return s, nil
	case job.KindEvent:
		return job.When{Kind: job.KindEvent}, nil
	case job.KindWebhook:
		signature := w.Signature
		if signature == "" {
			signature = job.SignatureStandard
		}
		if signature != job.SignatureStandard && signature != job.SignatureGithub {
			return job.When{}, jobInvalid(`"when.signature" must be "standard" or "github"`, id)
		}
		subjectPath, err := optionalMappingPath(w.SubjectPath, "when.subjectPath", id)
		if err != nil {
			return job.When{}, err
		}
		revisionPath, err := optionalMappingPath(w.RevisionPath, "when.revisionPath", id)
		if err != nil {
			return job.When{}, err
		}
		return job.When{Kind: job.KindWebhook, Signature: signature, SubjectPath: subjectPath, RevisionPath: revisionPath}, nil
	default:
		return job.When{}, jobInvalid(`"when.kind" must be "at", "every", "cron", "event" or "webhook"`, id)
	}
}

func validateJobFilter(f *job.Filter, when job.When, id string) (job.Filter, error) {
	var in job.Filter
	if f != nil {
		in = *f
	}
	var out job.Filter
	types, err := optionalStringList(in.Types, "filter.types", 20, id)
	if err != nil {
		return job.Filter{}, err
	}
	out.Types = types
	subjects, err := optionalStringList(in.Subjects, "filter.subjects", 50, id)
	if err != nil {
		return job.Filter{}, err
	}
	out.Subjects = subjects
	if len(in.Where) > 20 {
		return job.Filter{}, jobInvalid(`"filter.where" must be a list of at most 20 conditions`, id)
	}
	for i, c := range in.Where {
		vc, err := validateCondition(c, i, id)
		if err != nil {
			return job.Filter{}, err
		}
		out.Where = append(out.Where, vc)
	}
	if when.IsSchedule() {
		// A clock has no events to filter.
		if len(out.Types) > 0 || len(out.Subjects) > 0 || len(out.Where) > 0 {
			return job.Filter{}, jobInvalid("A schedule job takes no filter "+emDash+" filters are for event and webhook jobs", id)
		}
		return job.Filter{}, nil
	}
	if when.Kind == job.KindEvent {
		known := job.InternalEventTypes()
		// Never a catch-all: an event job names the SDK events it wants.
		if len(out.Types) == 0 {
			return job.Filter{}, jobInvalid(`An event job needs "filter.types" `+emDash+` one or more of `+strings.Join(known, ", "), id)
		}
		for _, typ := range out.Types {
			ok := false
			for _, k := range known {
				if job.TypeMatches(typ, k) {
					ok = true
					break
				}
			}
			if !ok {
				return job.Filter{}, jobInvalid(`"`+typ+`" is not an SDK event a job can listen for (`+strings.Join(known, ", ")+`)`, id)
			}
		}
	}
	return out, nil
}

func validateCondition(c job.Condition, index int, id string) (job.Condition, error) {
	field := `"filter.where[` + strconv.Itoa(index) + `]`
	path := jsstr.Trim(c.Path)
	if !conditionPathRe.MatchString(path) {
		return job.Condition{}, jobInvalid(field+`.path" must be type, subject, source or data.<field>`, id)
	}
	known := false
	names := make([]string, len(conditionOps))
	for i, op := range conditionOps {
		names[i] = string(op)
		known = known || c.Op == op
	}
	if !known {
		return job.Condition{}, jobInvalid(field+`.op" must be one of `+strings.Join(names, ", "), id)
	}
	switch c.Op {
	case job.OpIn:
		if !isList(c.Value) {
			return job.Condition{}, jobInvalid(field+`.value" must be a list for "in"`, id)
		}
	case job.OpExists:
		if _, ok := c.Value.(bool); c.Value != nil && !ok {
			return job.Condition{}, jobInvalid(field+`.value" must be true or false for "exists"`, id)
		}
	case job.OpEq, job.OpNeq, job.OpContains:
		if c.Value == nil {
			return job.Condition{}, jobInvalid(field+`.value" is required for "`+string(c.Op)+`"`, id)
		}
	}
	return job.Condition{Path: path, Op: c.Op, Value: jobkit.CloneValue(c.Value)}, nil
}

func isList(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array)
}

func optionalMappingPath(v, field, id string) (string, error) {
	if v == "" {
		return "", nil
	}
	p := jsstr.Trim(v)
	if !mappingPathRe.MatchString(p) {
		return "", jobInvalid(`"`+field+`" must be "subject" or data.<field>`, id)
	}
	return p, nil
}

func optionalStringList(list []string, field string, maxItems int, id string) ([]string, error) {
	if list == nil {
		return nil, nil
	}
	bad := len(list) > maxItems
	for _, s := range list {
		bad = bad || jsstr.Trim(s) == "" || jobkit.UTF16Len(s) > 200
	}
	if bad {
		return nil, jobInvalid(`"`+field+`" must be a list of at most `+strconv.Itoa(maxItems)+` non-empty strings`, id)
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range list {
		t := jsstr.Trim(s)
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

func intField(v *int, fallback, lo, hi int, field, id string) (int, error) {
	if v == nil {
		return fallback, nil
	}
	if *v < lo || *v > hi {
		return 0, jobInvalid(`"`+field+`" must be a whole number from `+strconv.Itoa(lo)+` to `+strconv.Itoa(hi), id)
	}
	return *v, nil
}

func resolveCoalesce(in *job.CoalesceInput, base job.Coalesce, id string) (job.Coalesce, error) {
	var v job.CoalesceInput
	if in != nil {
		v = *in
	}
	var out job.Coalesce
	var err error
	if out.QuietMs, err = intField(v.QuietMs, base.QuietMs, 0, hourMs, "coalesce.quietMs", id); err != nil {
		return job.Coalesce{}, err
	}
	if out.MaxWaitMs, err = intField(v.MaxWaitMs, base.MaxWaitMs, 0, dayMs, "coalesce.maxWaitMs", id); err != nil {
		return job.Coalesce{}, err
	}
	if out.MaxBatch, err = intField(v.MaxBatch, base.MaxBatch, 1, 50, "coalesce.maxBatch", id); err != nil {
		return job.Coalesce{}, err
	}
	if out.MaxWaitMs < out.QuietMs {
		return job.Coalesce{}, jobInvalid(`"coalesce.maxWaitMs" must be at least "coalesce.quietMs"`, id)
	}
	return out, nil
}

// limitField is one whole-number limit: where it goes, the input, the base value and
// its range.
type limitField struct {
	dst      *int
	val      *int
	fallback int
	lo, hi   int
	name     string
}

// optionalLimit resolves a limit that may be absent: Clear (the wire's null) removes it,
// a value replaces it, nothing keeps the base.
func optionalLimit(base, val *int, remove bool, lo, hi int, field, id string) (*int, error) {
	v := base
	if val != nil {
		v = val
	}
	if remove || v == nil {
		return nil, nil
	}
	n, err := intField(v, 0, lo, hi, field, id)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func resolveLimits(in *job.LimitsInput, base job.Limits, id string) (job.Limits, error) {
	var v job.LimitsInput
	if in != nil {
		v = *in
	}
	var out job.Limits
	fields := []limitField{
		{&out.MaxConcurrentRuns, v.MaxConcurrentRuns, base.MaxConcurrentRuns, 1, 10, "limits.maxConcurrentRuns"},
		{&out.MaxAttempts, v.MaxAttempts, base.MaxAttempts, 1, 10, "limits.maxAttempts"},
		{&out.RunDeadlineMs, v.RunDeadlineMs, base.RunDeadlineMs, 60_000, 7 * dayMs, "limits.runDeadlineMs"},
		{&out.CooldownMs, v.CooldownMs, base.CooldownMs, 0, dayMs, "limits.cooldownMs"},
		{&out.MaxDepth, v.MaxDepth, base.MaxDepth, 0, 5, "limits.maxDepth"},
	}
	for _, f := range fields {
		n, err := intField(f.val, f.fallback, f.lo, f.hi, f.name, id)
		if err != nil {
			return job.Limits{}, err
		}
		*f.dst = n
	}
	var err error
	// ClearMaxRunsPerHour (the wire's null) clears the hourly limit; absent keeps it.
	if out.MaxRunsPerHour, err = optionalLimit(base.MaxRunsPerHour, v.MaxRunsPerHour, v.ClearMaxRunsPerHour, 1, 1_000, "limits.maxRunsPerHour", id); err != nil {
		return job.Limits{}, err
	}
	// ClearMaxTokensPerDay (the wire's null) clears the budget; absent keeps it.
	if out.MaxTokensPerDay, err = optionalLimit(base.MaxTokensPerDay, v.MaxTokensPerDay, v.ClearMaxTokensPerDay, 1_000, 1_000_000_000, "limits.maxTokensPerDay", id); err != nil {
		return job.Limits{}, err
	}
	// A-103 (a job stored before has neither: the defaults apply).
	runMs, err := intField(v.MaxRunMs, intOr(base.MaxRunMs, job.DefaultMaxRunMs), 10_000, dayMs, "limits.maxRunMs", id)
	if err != nil {
		return job.Limits{}, err
	}
	pause, err := intField(v.PauseAfterFailures, intOr(base.PauseAfterFailures, job.DefaultPauseAfterFailures), 0, 100, "limits.pauseAfterFailures", id)
	if err != nil {
		return job.Limits{}, err
	}
	out.MaxRunMs, out.PauseAfterFailures = &runMs, &pause
	return out, nil
}

// intOr is *p, or fallback when p is nil.
func intOr(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

// ---------------------------------------------------------------------------
// Action (A-103)
// ---------------------------------------------------------------------------

// validateJobAction checks a job's action against its when. Nil for a prompt run (nil
// or kind "prompt"): jobs keep no action for the default. Whether the tool exists for the
// agent and is schedulable is the job manager's check (it knows the agent's tools).
func validateJobAction(a *job.Action, when job.When, id string) (*job.Action, error) {
	if a == nil || a.Kind == job.ActionPrompt {
		return nil, nil
	}
	if a.Kind != job.ActionTool {
		return nil, jobInvalid(`"action.kind" must be "prompt" or "tool"`, id)
	}
	if !when.IsSchedule() {
		return nil, jobInvalid("Only a schedule job can run a tool "+emDash+" an event or webhook job runs a prompt", id)
	}
	tool := jsstr.Trim(a.Tool)
	if tool == "" {
		return nil, jobInvalid(`"action.tool" must name one of the agent's tools`, id)
	}
	if _, n := dataBytes(a.Args); n > job.MaxToolArgsBytes {
		return nil, jobInvalid(`"action.args" is larger than `+strconv.Itoa(job.MaxToolArgsBytes/1024)+" KB", id)
	}
	args, err := plainArgs(a.Args)
	if err != nil {
		return nil, jobInvalid(`"action.args" must be an object `+emDash+` the tool's arguments`, id)
	}
	followUp := a.FollowUp
	if followUp == "" {
		followUp = job.FollowUpNever
	}
	switch followUp {
	case job.FollowUpNever, job.FollowUpOnFailure, job.FollowUpAlways:
	default:
		return nil, jobInvalid(`"action.followUp" must be one of never, on-failure, always`, id)
	}
	out := job.Action{Kind: job.ActionTool, Tool: tool, Args: args}
	if followUp != job.FollowUpNever {
		out.FollowUp = followUp
	}
	return &out, nil
}

// plainArgs is a detached copy of tool arguments as plain JSON values (map[string]any,
// []any, float64...); nil is an empty object.
func plainArgs(args map[string]any) (map[string]any, error) {
	raw, err := jobkit.Marshal(args)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// validatePromptFor is the prompt a job with this action needs: a prompt run always; a
// tool job only with a follow-up.
func validatePromptFor(a *job.Action, prompt, id string) (string, error) {
	if a == nil || a.Kind != job.ActionTool {
		return validateJobPrompt(prompt, id)
	}
	text := jsstr.Trim(prompt)
	if text == "" && a.FollowUp != "" {
		return "", jobInvalid(`A tool job with follow-up "`+string(a.FollowUp)+`" needs a "prompt" `+emDash+` the message of the follow-up turn`, id)
	}
	if text == "" {
		return "", nil
	}
	return validateJobPrompt(text, id)
}

// canonicalJSON is JSON with object keys sorted, so two equal argument objects compare
// equal (the pre-approval of a tool job's call).
func canonicalJSON(v any) string {
	raw, err := jobkit.Marshal(v)
	if err != nil {
		return ""
	}
	var plain any
	if err := json.Unmarshal(raw, &plain); err != nil {
		return string(raw)
	}
	out, err := jsonx.Marshal(plain)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// validateCreateInput returns everything Create needs, validated, with defaults for the
// job's kind filled in (no id, agent, version, secret, next run or timestamps).
func validateCreateInput(in job.CreateInput, now time.Time) (job.Job, error) {
	when, err := validateJobWhen(in.When, now, "")
	if err != nil {
		return job.Job{}, err
	}
	description, err := validateJobDescription(in.Description, "")
	if err != nil {
		return job.Job{}, err
	}
	name, err := validateJobName(in.Name, "")
	if err != nil {
		return job.Job{}, err
	}
	filter, err := validateJobFilter(in.Filter, when, "")
	if err != nil {
		return job.Job{}, err
	}
	// A-103: a tool job's prompt is only its follow-up turn's message.
	action, err := validateJobAction(in.Action, when, "")
	if err != nil {
		return job.Job{}, err
	}
	prompt, err := validatePromptFor(action, in.Prompt, "")
	if err != nil {
		return job.Job{}, err
	}
	coalesce, err := resolveCoalesce(in.Coalesce, job.DefaultCoalesce(), "")
	if err != nil {
		return job.Job{}, err
	}
	limits, err := resolveLimits(in.Limits, job.DefaultLimits(when), "")
	if err != nil {
		return job.Job{}, err
	}
	mode := in.SessionMode
	if mode == "" {
		mode = job.SessionNew
	}
	if err := validateSessionMode(mode, ""); err != nil {
		return job.Job{}, err
	}
	// Update (A-70 agent tools): who made it, and where its results go.
	createdBy := in.CreatedBy
	if createdBy == "" {
		createdBy = job.CreatedByUser
	}
	if createdBy != job.CreatedByUser && createdBy != job.CreatedByAgent {
		return job.Job{}, jobInvalid(`"createdBy" must be "user" or "agent"`, "")
	}
	deliveries, err := validateJobDeliveries(in.Deliveries, "")
	if err != nil {
		return job.Job{}, err
	}
	return job.Job{
		Name: name, Description: description, Enabled: in.Enabled == nil || *in.Enabled,
		When: when, Filter: filter, Prompt: prompt, Action: action, Coalesce: coalesce, Limits: limits, SessionMode: mode,
		CreatedBy: createdBy, CreatedInSessionID: jsstr.Trim(in.CreatedInSessionID), Deliveries: deliveries,
	}, nil
}

// validateJobDelivery checks a delivery { channelId, target, threadId?, when } (A-100
// phase 3); nil stays nil. When defaults to "always", and the thread is dropped for the
// home chat. Whether the channel is the agent's is checked when the result is sent.
func validateJobDelivery(d *job.Delivery, id string) (*job.Delivery, error) {
	if d == nil {
		return nil, nil
	}
	out := job.Delivery{
		ChannelID: jsstr.Trim(d.ChannelID),
		Target:    jsstr.Trim(d.Target),
		ThreadID:  jsstr.Trim(d.ThreadID),
		When:      d.When,
	}
	if out.ChannelID == "" {
		return nil, jobInvalid(`"delivery.channelId" is required`, id)
	}
	if out.Target == "" {
		return nil, jobInvalid(`"delivery.target" must be "home" or a chat id`, id)
	}
	if out.When == "" {
		out.When = job.DeliverAlways
	}
	switch out.When {
	case job.DeliverAlways, job.DeliverOnOutput, job.DeliverOnFailure:
	default:
		return nil, jobInvalid(`"delivery.when" must be one of always, on-output, on-failure`, id)
	}
	// A thread belongs to a named chat; the home chat carries its own.
	if out.Target == job.DeliveryHome {
		out.ThreadID = ""
	}
	return &out, nil
}

// validateJobDeliveries checks each delivery (A-127); a chat named twice is kept once
// (the first). Nil when there is none.
func validateJobDeliveries(list job.Deliveries, id string) (job.Deliveries, error) {
	var out job.Deliveries
	for i := range list {
		d, err := validateJobDelivery(&list[i], id)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(out, func(o job.Delivery) bool {
			return o.ChannelID == d.ChannelID && o.Target == d.Target && o.ThreadID == d.ThreadID
		}) {
			out = append(out, *d)
		}
	}
	return out, nil
}

// dataBytes is the JSON size of data in bytes, or MaxInt when it cannot be encoded.
func dataBytes(data any) (json.RawMessage, int) {
	raw, err := jobkit.Marshal(data)
	if err != nil {
		return nil, math.MaxInt
	}
	return raw, len(raw)
}

// ---------------------------------------------------------------------------
// Event envelope
// ---------------------------------------------------------------------------

// jobEnvelope is what a source says about its event - before filtering.
type jobEnvelope struct {
	sourceEventID string
	source        string
	typ           string
	subject       string
	revision      *float64
	// time is ISO, when the source says ("" otherwise).
	time string
	data json.RawMessage
}

// parseWebhookEvent is the event a verified delivery carries, or an error message. A
// CloudEvents body (with "specversion") supplies id, source, type, subject, time and
// data; a Standard Webhooks body supplies type, timestamp and data; a GitHub delivery is
// its whole body.
func parseWebhookEvent(source job.When, req job.WebhookRequest) (jobEnvelope, string) {
	parsed, err := jobkit.Decode(req.Body)
	if err != nil || len(strings.TrimSpace(string(req.Body))) == 0 {
		return jobEnvelope{}, "the body is not JSON"
	}
	b, ok := parsed.(ordered.Object)
	if !ok {
		return jobEnvelope{}, "the body must be a JSON object"
	}
	field := func(k string) any {
		v, _ := b.Get(k)
		return v
	}

	var env jobEnvelope
	var data any
	if _, isCE := field("specversion").(string); isCE {
		// CloudEvents (structured mode).
		id, typ := nonBlank(field("id")), nonBlank(field("type"))
		if id == "" || typ == "" {
			return jobEnvelope{}, "a CloudEvents body needs id and type"
		}
		env = jobEnvelope{sourceEventID: id, source: "webhook", typ: typ}
		if s := nonBlank(field("source")); s != "" {
			env.source = s
		}
		if s, ok := jobkit.ToSubject(field("subject")); ok {
			env.subject = s
		}
		env.time = isoOf(field("time"))
		data = field("data")
	} else if source.Signature == job.SignatureGithub {
		id, event := job.Header(req, "x-github-delivery"), job.Header(req, "x-github-event")
		if id == "" || event == "" {
			return jobEnvelope{}, "missing X-GitHub-Delivery or X-GitHub-Event"
		}
		typ := event
		if action := nonBlank(field("action")); action != "" {
			typ = event + "." + action
		}
		env = jobEnvelope{sourceEventID: id, source: "github", typ: typ}
		data = b
	} else {
		id := job.Header(req, "webhook-id")
		typ := nonBlank(field("type"))
		if id == "" {
			return jobEnvelope{}, "missing webhook-id"
		}
		if typ == "" {
			return jobEnvelope{}, `the body needs a "type"`
		}
		env = jobEnvelope{sourceEventID: id, source: "webhook", typ: typ, time: isoOf(field("timestamp"))}
		data = field("data")
		if data == nil {
			data = b
		}
	}

	// The job's mapping picks the subject / revision when the body did not set one.
	if env.subject == "" && source.SubjectPath != "" {
		if v, ok := jobkit.ReadPath(env.typ, "", env.source, data, source.SubjectPath); ok {
			if s, ok := jobkit.ToSubject(v); ok {
				env.subject = s
			}
		}
	}
	if source.RevisionPath != "" {
		if v, ok := jobkit.ReadPath(env.typ, env.subject, env.source, data, source.RevisionPath); ok {
			if r, ok := jobkit.ToRevision(v); ok {
				env.revision = &r
			}
		}
	}
	if jobkit.UTF16Len(env.sourceEventID) > 200 {
		return jobEnvelope{}, "the event id is longer than 200 characters"
	}
	if jobkit.UTF16Len(env.typ) > 200 {
		return jobEnvelope{}, "the event type is longer than 200 characters"
	}
	raw, err := jobkit.Marshal(data)
	if err != nil {
		return jobEnvelope{}, "the event data is not JSON"
	}
	env.data = raw
	return env, ""
}

// nonBlank is v when it is a string that is not blank, "" otherwise.
func nonBlank(v any) string {
	if s, ok := v.(string); ok && jsstr.Trim(s) != "" {
		return s
	}
	return ""
}

// isoOf reads a time field: a date string, or a number of seconds. "" when it is not one.
func isoOf(v any) string {
	var ms float64
	if s, ok := v.(string); ok {
		ms = jsstr.ParseDate(s, time.UTC)
	} else if f, ok := jobkit.Number(v); ok {
		ms = f * 1000
	} else {
		return ""
	}
	// The range of a JS Date (+-8.64e15 ms).
	if math.IsNaN(ms) || math.IsInf(ms, 0) || math.Abs(ms) > 8.64e15 {
		return ""
	}
	return isoTime(time.UnixMilli(int64(ms)))
}

// ---------------------------------------------------------------------------
// The agent's own SDK events as trigger events. The trigger manager subscribes to
// these bus kinds only - the scheduler and channels never know a trigger exists. Each
// maps to a dotted trigger type, with the id that makes a redelivery of the same thing
// a duplicate (a job run is investigated once).
//
// Update (A-126): the job manager subscribes, for "event" jobs. A job run ending is its
// own job:run-finished event (scheduled-job:* is gone), so:
//
//	bus event                               job event type    subject     id
//	job:run-finished (succeeded)            job.completed     job id      run id
//	job:run-finished (failed / blocked)     job.failed        job id      run id
//	job:run-finished (stopped)              job.stopped       job id      run id
//	channel:error                           channel.error     channel id  bus event id
//
// A suppressed run did no work and is not an event. The loop guard (an event caused by
// an event job's run is one level deeper) keeps an event job from setting itself off.
//
// The bus is in-process: an event emitted while no trigger manager listens (the host was
// down) is not seen. Durable capture of such events needs an outbox written with them,
// which this source does not claim to be.
// ---------------------------------------------------------------------------

// jobBusKinds are the bus kinds the job manager listens to.
var jobBusKinds = []string{events.JobRunFinished.Name, events.ChannelFailed.Name}

// maxJobOutputChars is the most characters of a job's reply carried in an event's data.
const maxJobOutputChars = 4_000

// internalEnvelope is the job envelope of one of the agent's bus events; false when it
// is not one of its own.
func internalEnvelope(agentID string, e kernel.Event) (jobEnvelope, bool) {
	at := isoTime(time.UnixMilli(e.Timestamp))
	data := ordered.NewObject()
	switch e.Type {
	case events.JobRunFinished.Name:
		p, err := events.PayloadAs[events.JobRunFinishedPayload](e)
		if err != nil || p.AgentID != agentID {
			return jobEnvelope{}, false
		}
		var typ string
		switch p.Status {
		case string(job.RunSucceeded):
			typ = "job.completed"
		case string(job.RunFailed), string(job.RunBlocked):
			typ = "job.failed"
		case string(job.RunStopped):
			typ = "job.stopped"
		default:
			return jobEnvelope{}, false
		}
		data.Set("jobId", p.JobID)
		data.Set("jobName", p.JobName)
		data.Set("runId", p.RunID)
		data.Set("cause", p.Cause)
		data.Set("status", p.Status)
		setIf(&data, "error", p.Error)
		setIf(&data, "reason", p.Reason)
		if p.Output != "" {
			data.Set("output", jobkit.SliceUTF16(p.Output, maxJobOutputChars))
		}
		if p.Silent {
			data.Set("silent", true)
		}
		setIf(&data, "sessionId", e.SessionID)
		raw, _ := jobkit.Marshal(data)
		return jobEnvelope{source: "aec:jobs", sourceEventID: p.RunID, typ: typ, subject: p.JobID, time: at, data: raw}, true
	case events.ChannelFailed.Name:
		p, err := events.PayloadAs[events.ChannelFailedPayload](e)
		if err != nil || p.AgentID != agentID {
			return jobEnvelope{}, false
		}
		data.Set("channelId", p.ChannelID)
		data.Set("platform", p.Platform)
		setIf(&data, "chatId", p.ChatID)
		data.Set("error", p.Error)
		setIf(&data, "jobId", p.JobID)
		raw, _ := jobkit.Marshal(data)
		return jobEnvelope{source: "aec:channels", sourceEventID: e.ID, typ: "channel.error", subject: p.ChannelID, time: at, data: raw}, true
	default:
		return jobEnvelope{}, false
	}
}

// setIf sets k to v when v is not empty (core/ts leaves an undefined key out of the JSON).
func setIf(o *ordered.Object, k, v string) {
	if v != "" {
		o.Set(k, v)
	}
}

// resolvedSince is the current state check before a run of internal events: a
// "job.failed" whose job has succeeded since is resolved, so it is not investigated. It
// returns the reason to suppress, or "" to go on.
func resolvedSince(ctx context.Context, evs []job.Event, jobs persistence.JobStore) (string, error) {
	if len(evs) == 0 || jobs == nil {
		return "", nil
	}
	for _, e := range evs {
		if e.Source != "aec:jobs" || e.Type != "job.failed" {
			return "", nil
		}
	}
	latest := evs[len(evs)-1]
	data, err := jobkit.Decode(latest.Data)
	if err != nil {
		return "", nil //nolint:nilerr // unreadable data is not a resolved job
	}
	jobID, ok := jobkit.ReadPath("", "", "", data, "data.jobId")
	id, isString := jobID.(string)
	if !ok || !isString {
		return "", nil
	}
	runs, err := jobs.ListRuns(ctx, id, 10)
	if err != nil {
		return "", err
	}
	for _, r := range runs {
		if r.Status == job.RunSucceeded && r.StartedAt != "" {
			if r.StartedAt > latest.Time {
				return "the job has succeeded since (run " + r.ID + ")", nil
			}
			break
		}
	}
	return "", nil
}
