// Package triggers holds the trigger tools (A-70): manage_trigger and list_triggers.
// They let an agent set up "when X happens, do Y" from chat ("make me a report whenever
// a new model is uploaded"); each run is a full agent turn with the trigger's prompt plus
// the event data, in a new session the user can open later.
//
// The tools see the agent's triggers only through toolhub.Context.Triggers, which is
// already scoped to the agent, so a trigger is always owned by the agent that made it.
// manage_trigger is consequential: every call waits for the user's approval, and trigger
// runs and scheduled runs deny every approval, so an unattended run (and the event data
// it reads) can never create or change triggers. What the agent may set is capped: no
// chains (maxDepth 0), at most AgentTriggerMaxRunsPerHour runs an hour and a daily token
// budget; the user can change any of it on the Triggers page. The webhook secret never
// reaches the model: the user copies it from the Triggers page. list_triggers is
// read-only.
//
// Update (A-126): package jobs - the job tools manage_job and list_jobs, which replace
// both the scheduled-job tools (schedule_job, list_scheduled_jobs - A-69) and the
// trigger tools. A job runs on a schedule, on the agent's own events, or on a webhook;
// the tools see the agent's jobs only through toolhub.Context.Jobs. The caps above apply
// to event and webhook jobs (AgentJobMaxRunsPerHour); a schedule job is bounded by its
// clock. Models with strict tool schemas fill every parameter, so an empty string, 0 or
// an empty list means "not given".
//
// The tool texts are those of the core/ts tools of the same names.
package jobs
