package events

// SDK event type constants - the single source of truth for all event type
// strings. Each Kind pairs an event type string with the payload type that
// events of that type carry.
var (
	UserMessage    = Kind[UserMessagePayload]{Name: "agent-loop:message"}
	Iteration      = Kind[IterationPayload]{Name: "agent-loop:iteration"}
	ModelRequested = Kind[ModelRequestedPayload]{Name: "agent-loop:model-request"}
	ModelResponded = Kind[ModelRespondedPayload]{Name: "agent-loop:model-response"}
	// ContextUsage (A-104): what fills the session's context window, once per iteration
	// after ModelResponded.
	ContextUsage          = Kind[ContextUsagePayload]{Name: "agent-loop:context-usage"}
	ToolDispatched        = Kind[DispatchStartPayload]{Name: "agent-loop:tool-start"}
	ToolCompleted         = Kind[DispatchEndPayload]{Name: "agent-loop:tool-end"}
	ToolRetry             = Kind[DispatchRetryPayload]{Name: "agent-loop:tool-retry"}
	ToolFailed            = Kind[DispatchFailedPayload]{Name: "agent-loop:tool-error"}
	ToolDenied            = Kind[ToolDeniedPayload]{Name: "agent-loop:tool-denied"}
	ToolApprovalRequested = Kind[ToolApprovalRequestedPayload]{Name: "agent-loop:tool-approval-request"}
	ToolApprovalResolved  = Kind[ToolApprovalResolvedPayload]{Name: "agent-loop:tool-approval-result"}
	// ToolAutoApproved (A-135): a call that needed approval ran without asking - its
	// session is in YOLO mode.
	ToolAutoApproved     = Kind[ToolAutoApprovedPayload]{Name: "agent-loop:tool-auto-approved"}
	SkillDispatched      = Kind[DispatchStartPayload]{Name: "agent-loop:skill-start"}
	SkillCompleted       = Kind[DispatchEndPayload]{Name: "agent-loop:skill-end"}
	SkillRetry           = Kind[DispatchRetryPayload]{Name: "agent-loop:skill-retry"}
	SkillFailed          = Kind[DispatchFailedPayload]{Name: "agent-loop:skill-error"}
	RunCompleted         = Kind[RunCompletedPayload]{Name: "agent-loop:complete"}
	MaxIterationsReached = Kind[MaxIterationsPayload]{Name: "agent-loop:max-iterations"}
	ArtifactSaved        = Kind[ArtifactWrittenPayload]{Name: "artifact:saved"}
	ArtifactUpdated      = Kind[ArtifactWrittenPayload]{Name: "artifact:updated"}
	ArtifactDeleted      = Kind[ArtifactDeletedPayload]{Name: "artifact:deleted"}
	// Update (A-126): the scheduled-job:* kinds of code jobs were removed with scheduled
	// tasks (plugin code). Every job emits the job:* kinds below.

	// Lifecycle kinds (notifications: "X happened, 0..N parties react"). They carry no
	// session, so they are emitted with SessionID "". Hosts subscribe with On / OnAny.
	//
	//   kind                   | emitted by
	//   -----------------------+------------------------------------------------------------
	//   agent:spawned          | AgentCore.SpawnAgent (not Initialize rehydration)
	//   agent:removed          | AgentCore.RemoveAgent (Publish with its ctx); subscribers: sandboxhub.SandboxHub and
	//                          | artifacthub.ArtifactHub release the agent's data (WithEvents), then the hosts'
	//   agent:profile-changed  | AgentCore, after an agent's skill allowlist change is saved
	//   skill:deleted          | skillhub.SkillHub.Delete (WithEvents), after the OnDeleted listeners
	//
	// Unlike core/ts, the built-in work (releases, saves, disabling) stays direct calls
	// in AgentCore: a Handler has no ctx and no error return, so moving that work into
	// subscribers would lose cancellation and the errors RemoveAgent and SkillManager
	// report today. The events are the notification on top.
	// Exception: agent:removed is published with ctx (Bus.Publish, OnCtx), so the
	// sandbox and artifact hubs release the agent's data as its subscribers.
	AgentSpawned        = Kind[AgentSpawnedPayload]{Name: "agent:spawned"}
	AgentRemoved        = Kind[AgentRemovedPayload]{Name: "agent:removed"}
	AgentProfileChanged = Kind[AgentProfileChangedPayload]{Name: "agent:profile-changed"}
	SkillDeleted        = Kind[SkillDeletedPayload]{Name: "skill:deleted"}

	// Channels (A-100), emitted by an agent's channel manager. channel:status and
	// channel:pairing-requested carry no session (SessionID ""); the message kinds and
	// channel:error carry the chat's session. No built-in subscribers (hosts).
	ChannelStatusChanged    = Kind[ChannelStatusChangedPayload]{Name: "channel:status"}
	ChannelMessageReceived  = Kind[ChannelMessageReceivedPayload]{Name: "channel:message-received"}
	ChannelMessageSent      = Kind[ChannelMessageSentPayload]{Name: "channel:message-sent"}
	ChannelPairingRequested = Kind[ChannelPairingRequestedPayload]{Name: "channel:pairing-requested"}
	ChannelFailed           = Kind[ChannelFailedPayload]{Name: "channel:error"}

	// Triggers (A-70), emitted by an agent's trigger manager: the run kinds with the
	// run's session, trigger:event-received with SessionID "". No built-in subscribers
	// (hosts).
	//
	// Each agent's trigger manager listens for its own scheduled-job:error /
	// scheduled-job:completed / channel:error and turns them into trigger events (the
	// "event" trigger source). trigger:* events are never a trigger source, so a
	// trigger's own runs cannot set off another trigger.
	//
	// Update (A-126): jobs of every kind (schedule, event, webhook), emitted by an agent's
	// job manager. Built-in subscribers of job:run-finished: the channel manager
	// (delivery) and the job manager itself (the "event" job source, with
	// channel:error). The loop guard (an event caused by an event job's run is one level
	// deeper, Limits.MaxDepth) keeps an event job's runs from setting off another one.
	JobEventReceived = Kind[JobEventReceivedPayload]{Name: "job:event-received"}
	JobRunStarted    = Kind[JobRunStartedPayload]{Name: "job:run-started"}
	JobRunRetrying   = Kind[JobRunRetryingPayload]{Name: "job:run-retry"}
	JobRunFinished   = Kind[JobRunFinishedPayload]{Name: "job:run-finished"}

	// A-105: each parent agent's subagent manager emits subagent:* for every child run it
	// starts, with the PARENT's session id; they announce lifecycle facts only (nobody needs
	// them to work) - waiting for a child and collecting its report go through the
	// subagent manager directly. completed covers "completed" and "partial"; failed covers
	// "failed", "timed_out" and "budget_exhausted" (Status says which). No built-in
	// subscribers (hosts).
	SubagentSpawned   = Kind[SubagentSpawnedPayload]{Name: "subagent:spawned"}
	SubagentCompleted = Kind[SubagentFinishedPayload]{Name: "subagent:completed"}
	SubagentFailed    = Kind[SubagentFinishedPayload]{Name: "subagent:failed"}
	SubagentCancelled = Kind[SubagentFinishedPayload]{Name: "subagent:cancelled"}
	// A-105 revision: subagent-type:deleted (SessionID "") is emitted by
	// subagenthub.SubagentHub.Delete (WithEvents) once a custom type is removed from the
	// library; built-in subscriber: AgentCore, which disables the type for every agent
	// that had it (saved through the current persistence hub) before Delete returns.
	SubagentTypeDeleted = Kind[SubagentTypeDeletedPayload]{Name: "subagent-type:deleted"}

	// A-101: emitted by the secrethub.SecretHub (WithEvents) with SessionID ""; they carry
	// names only, never values. secret:created / secret:updated (SecretHub.Create, Update,
	// SetValue) and secret:deleted (SecretHub.Delete) are published with the call's ctx and
	// awaited; built-in subscriber of secret:deleted: AgentCore, which drops a deleted
	// GLOBAL secret from every agent's AgentProfile.Secrets.Use (saved through the current
	// persistence hub) before Delete returns. secret:used is emitted (not awaited) by
	// SecretHub.Reveal each time a tool is handed a value, with the turn's session id.
	SecretCreated = Kind[SecretChangedPayload]{Name: "secret:created"}
	SecretUpdated = Kind[SecretChangedPayload]{Name: "secret:updated"}
	SecretDeleted = Kind[SecretChangedPayload]{Name: "secret:deleted"}
	SecretUsed    = Kind[SecretUsedPayload]{Name: "secret:used"}

	// A-135: a session's approval mode was set (Agent.SetApprovalMode, through the
	// conversation manager), with that session's id. No built-in subscribers (hosts).
	SessionApprovalModeChanged = Kind[SessionApprovalModeChangedPayload]{Name: "session:approval-mode"}

	// A-136: a session's settings changed outside a turn (its title from /rename, its model
	// from /model, its working set from /compact), emitted by the conversation manager with
	// that session's id. No built-in subscribers (hosts).
	SessionUpdated = Kind[SessionUpdatedPayload]{Name: "session:updated"}
	// A-153: a runner (a folder on the user's machine) was attached to a session, went
	// offline or was detached (the sandbox hub's AttachRunner / DetachRunner), with that
	// session's id. No built-in subscribers (hosts).
	SandboxRunnerChanged = Kind[SandboxRunnerChangedPayload]{Name: "sandbox:runner-changed"}
	// A-136: a slash command ran (the agent's command manager, Agent.Commands().Run),
	// whatever its outcome, with the session it ran in. No built-in subscribers (hosts).
	CommandExecuted = Kind[CommandExecutedPayload]{Name: "command:executed"}

	// A-106: the grouphub.Hub emits group:* - group:changed with SessionID "" (a group was
	// created, changed or deleted), and the rest with the GROUP session's id: the thread's
	// messages as they are added and updated, text deltas of answers streaming in, each
	// member's presence, and the start and end of a group turn. Hosts stream them to the
	// thread's viewers; nothing in the SDK needs them to work. The Hub itself listens for
	// agent:removed (drops the agent from every group) and for its lead's subagent:* (a
	// hand-off's final status).
	GroupChanged        = Kind[GroupChangedPayload]{Name: "group:changed"}
	GroupMessageAdded   = Kind[GroupMessagePayload]{Name: "group:message-added"}
	GroupMessageUpdated = Kind[GroupMessagePayload]{Name: "group:message-updated"}
	GroupMessageDelta   = Kind[GroupMessageDeltaPayload]{Name: "group:message-delta"}
	GroupPresence       = Kind[GroupPresencePayload]{Name: "group:presence"}
	GroupTurnStarted    = Kind[GroupTurnStartedPayload]{Name: "group:turn-started"}
	GroupTurnFinished   = Kind[GroupTurnFinishedPayload]{Name: "group:turn-finished"}

	// A-166: each agent's skill learner (skilllearning.System, only while skill learning
	// is enabled) emits skill-learning:* with the episode's session id: episode-captured
	// when a hard-won turn was confirmed a success, proposed when a draft skill joined the
	// review queue, accepted / rejected when it was reviewed, skill-updated when a learned
	// skill was created, patched or retired, and skill-used (A-172) when a learned skill
	// was used in a turn. They announce facts only; the learner itself subscribes to its
	// own episode-captured (the distiller).
	SkillLearningEpisodeCaptured = Kind[SkillLearningEpisodeCapturedPayload]{Name: "skill-learning:episode-captured"}
	SkillLearningProposed        = Kind[SkillLearningProposedPayload]{Name: "skill-learning:proposed"}
	SkillLearningAccepted        = Kind[SkillLearningReviewedPayload]{Name: "skill-learning:accepted"}
	SkillLearningRejected        = Kind[SkillLearningReviewedPayload]{Name: "skill-learning:rejected"}
	SkillLearningSkillUpdated    = Kind[SkillLearningSkillUpdatedPayload]{Name: "skill-learning:skill-updated"}
	SkillLearningSkillUsed       = Kind[SkillLearningSkillUsedPayload]{Name: "skill-learning:skill-used"}
)

// KindNames returns the 32 event type strings in their canonical order.
// (Update (A-70): 36, with the four trigger kinds.)
// (Update (A-126): 33 - the four job kinds replace the three scheduled-job and four
// trigger kinds, and the two code-job kinds were removed with scheduled tasks.)
// (Update (A-105): 37, with the four subagent kinds; 38 with subagent-type:deleted.)
// (Update (A-101): 42, with the four secret kinds.)
// (Update (A-104): 43, with agent-loop:context-usage.)
// (Update (A-135): 45, with agent-loop:tool-auto-approved and session:approval-mode.)
// (Update (A-136): 47, with session:updated and command:executed.)
// (Update (A-106): 54, with the seven group kinds.)
// (Update (A-166): 60, with the six skill-learning kinds.)
// The slice is fresh on every call.
func KindNames() []string {
	return []string{
		UserMessage.Name,
		Iteration.Name,
		ModelRequested.Name,
		ModelResponded.Name,
		ContextUsage.Name,
		ToolDispatched.Name,
		ToolCompleted.Name,
		ToolRetry.Name,
		ToolFailed.Name,
		ToolDenied.Name,
		ToolApprovalRequested.Name,
		ToolApprovalResolved.Name,
		ToolAutoApproved.Name,
		SkillDispatched.Name,
		SkillCompleted.Name,
		SkillRetry.Name,
		SkillFailed.Name,
		RunCompleted.Name,
		MaxIterationsReached.Name,
		ArtifactSaved.Name,
		ArtifactUpdated.Name,
		ArtifactDeleted.Name,
		AgentSpawned.Name,
		AgentRemoved.Name,
		AgentProfileChanged.Name,
		SkillDeleted.Name,
		ChannelStatusChanged.Name,
		ChannelMessageReceived.Name,
		ChannelMessageSent.Name,
		ChannelPairingRequested.Name,
		ChannelFailed.Name,
		JobEventReceived.Name,
		JobRunStarted.Name,
		JobRunRetrying.Name,
		JobRunFinished.Name,
		SubagentSpawned.Name,
		SubagentCompleted.Name,
		SubagentFailed.Name,
		SubagentCancelled.Name,
		SubagentTypeDeleted.Name,
		SecretCreated.Name,
		SecretUpdated.Name,
		SecretDeleted.Name,
		SecretUsed.Name,
		SessionApprovalModeChanged.Name,
		SessionUpdated.Name,
		SandboxRunnerChanged.Name,
		CommandExecuted.Name,
		GroupChanged.Name,
		GroupMessageAdded.Name,
		GroupMessageUpdated.Name,
		GroupMessageDelta.Name,
		GroupPresence.Name,
		GroupTurnStarted.Name,
		GroupTurnFinished.Name,
		SkillLearningEpisodeCaptured.Name,
		SkillLearningProposed.Name,
		SkillLearningAccepted.Name,
		SkillLearningRejected.Name,
		SkillLearningSkillUpdated.Name,
		SkillLearningSkillUsed.Name,
	}
}
