// Package toolhub holds the tool contracts of the SDK: the JSON schema subset used
// to describe tool parameters, the handler interface, the policy interface and
// the ToolHub that registers tools and runs them only through the policy.
//
// The types are grouped under three section comments:
//
//	JSON Schema (subset for tool parameter definitions)
//	Tool registry - tools register themselves with a schema
//	Tool policy - authorization boundary before tool dispatch
package toolhub

import (
	"bytes"
	"context"
	"iter"
	"slices"
	"sync/atomic"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// JSON Schema (subset for tool parameter definitions)
// ---------------------------------------------------------------------------

// JSONSchema is the subset of JSON Schema used for tool parameter definitions.
//
// Type is a plain string, not an enum: the usual types are object, string,
// number, boolean and array, but a tool may still declare another type (such as
// integer) and it must reach the policy, which then checks values against it.
//
// The field order is type, properties, required, items, enum, description. It
// matches the key order of the built-in tool schemas (a property schema is
// written type, enum, description or type, items, description, and an object
// schema type, properties, required), so a schema re-encodes with the bytes of
// the recorded fixtures. A schema authored with description before enum
// re-encodes with enum first.
//
// Required and Enum use omitzero: a nil slice is omitted, an empty non-nil slice
// is kept on the wire. The two are distinct on purpose, because the policy checks
// that an enum is present, not that it is non-empty: an empty enum denies every
// string value.
type JSONSchema struct {
	Type        string      `json:"type"`
	Properties  *Properties `json:"properties,omitempty"`
	Required    []string    `json:"required,omitzero"`
	Items       *JSONSchema `json:"items,omitempty"`
	Enum        []string    `json:"enum,omitzero"`
	Description string      `json:"description,omitempty"`
}

// Clone returns a deep copy: Properties and Items are pointers, so a plain
// struct copy would share them with the original. A nil slice stays nil and an
// empty non-nil slice stays non-nil, because the wire form tells them apart.
func (s JSONSchema) Clone() JSONSchema {
	out := s
	out.Properties = s.Properties.clone()
	if s.Items != nil {
		items := s.Items.Clone()
		out.Items = &items
	}
	out.Required = slices.Clone(s.Required)
	out.Enum = slices.Clone(s.Enum)
	return out
}

// Property is one named property schema of an object schema.
type Property struct {
	Name   string
	Schema JSONSchema
}

// Properties is the ordered set of named property schemas of an object schema.
// Insertion (and decode) order is the wire order and also the order in which
// the default policy visits the properties, so the first violation it reports
// is the first one in that order.
//
// It wraps an unexported ordered map: a field typed directly from an internal
// package could not be constructed by code outside this module (server/go,
// third-party tools). The zero value is an empty set and the read methods are
// safe on a nil receiver; Set is not (it panics on a nil receiver, because it
// has nothing to store into, so use NewProperties or a non-nil value).
//
// Properties is a pointer in JSONSchema, so a plain copy of a JSONSchema shares
// it. Use JSONSchema.Clone or Definition.Clone for an independent copy; the ToolHub
// stores and returns such copies.
type Properties struct {
	m *ordered.Map[JSONSchema]
}

// NewProperties builds a Properties from props in the given order. A repeated
// name replaces the earlier schema and keeps its position.
func NewProperties(props ...Property) *Properties {
	p := &Properties{}
	for _, prop := range props {
		p.Set(prop.Name, prop.Schema)
	}
	return p
}

// Set stores schema under name, appending a new name or replacing an existing
// one in place.
func (p *Properties) Set(name string, schema JSONSchema) {
	if p.m == nil {
		p.m = ordered.New[JSONSchema]()
	}
	p.m.Set(name, schema)
}

// clone returns a deep copy, or nil for a nil receiver.
func (p *Properties) clone() *Properties {
	if p == nil {
		return nil
	}
	// Keep a non-nil empty map: an empty set must clone to an empty set, not to
	// the zero state, so a decoded {} stays {} after Clone.
	out := &Properties{m: ordered.New[JSONSchema]()}
	for name, schema := range p.All() {
		out.Set(name, schema.Clone())
	}
	return out
}

// Get returns the schema stored under name.
func (p *Properties) Get(name string) (JSONSchema, bool) {
	if p == nil {
		return JSONSchema{}, false
	}
	return p.m.Get(name)
}

// Has reports whether name is present.
func (p *Properties) Has(name string) bool {
	_, ok := p.Get(name)
	return ok
}

// Len returns the number of properties.
func (p *Properties) Len() int {
	if p == nil {
		return 0
	}
	return p.m.Len()
}

// Names returns a fresh slice of the property names in order.
func (p *Properties) Names() []string {
	if p == nil {
		return nil
	}
	return p.m.Keys()
}

// All iterates the properties in order. It yields nothing on a nil receiver.
func (p *Properties) All() iter.Seq2[string, JSONSchema] {
	if p == nil {
		return func(func(string, JSONSchema) bool) {}
	}
	return p.m.All()
}

// MarshalJSON writes the properties as a JSON object in order.
func (p *Properties) MarshalJSON() ([]byte, error) {
	// A nil receiver and the zero value (m == nil) are both the empty set;
	// ordered.Map.MarshalJSON has a value receiver and would dereference a nil m.
	if p == nil || p.m == nil {
		return []byte("{}"), nil
	}
	return p.m.MarshalJSON()
}

// UnmarshalJSON reads a JSON object, keeping the order of its keys. The input
// null leaves the receiver unchanged (the encoding/json convention).
func (p *Properties) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	m := ordered.New[JSONSchema]()
	if err := m.UnmarshalJSON(data); err != nil {
		return err
	}
	p.m = m
	return nil
}

// ---------------------------------------------------------------------------
// Tool registry - tools register themselves with a schema
// ---------------------------------------------------------------------------

// Risk is the side-effect class of a tool; it decides whether a call needs
// human approval.
//   - RiskReadOnly: retrieves data, changes nothing. Runs after policy checks.
//   - RiskDraft: creates something reversible (a draft, a proposal). Runs after
//     policy checks.
//   - RiskConsequential: external or irreversible effect (send, delete, spend).
//     The harness pauses for explicit user approval immediately before
//     execution and fails closed when no InteractionHandler is configured.
//
// Tools that omit Risk (or set a value that is none of these) are treated as
// RiskConsequential: the harness fails closed and asks for approval, so a tool
// that needs no approval must declare RiskReadOnly or RiskDraft explicitly.
type Risk string

// The risk classes of a tool.
const (
	RiskReadOnly      Risk = "read-only"
	RiskDraft         Risk = "draft"
	RiskConsequential Risk = "consequential"
)

// SubagentUse (A-137) is Definition.Subagent: whether a subagent may use a tool.
type SubagentUse string

// The values of Definition.Subagent.
const (
	SubagentNever     SubagentUse = "never"
	SubagentAllowed   SubagentUse = "allowed"
	SubagentPreferred SubagentUse = "preferred"
)

// Definition describes a tool to the model and to the policy.
//
// SandboxOperation is declared after Risk, matching the recorded wire key order.
type Definition struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Parameters   JSONSchema `json:"parameters"`
	AllowedModes []string   `json:"allowedModes,omitzero"`
	Risk         Risk       `json:"risk,omitempty"`
	// SandboxOperation is set by tools that work in the agent's sandbox (file and
	// terminal tools). Before dispatch the harness resolves the sandbox and asks
	// the sandbox hub approval policy whether this operation needs user approval
	// THERE - for example auto in an isolated Docker sandbox, approval for writes
	// and commands on a host folder.
	SandboxOperation sandboxhub.Operation `json:"sandboxOperation,omitempty"`
	// SessionWorkspace (A-153): the tool works in the session's workspace - the folder
	// on the user's machine when the user attached a runner to the session
	// (sandboxhub.SandboxHub.AttachRunner), else the agent's sandbox. Set on the file and
	// terminal tools; tools whose files the server reads back (artifacts, previews,
	// skills) leave it out and stay in the agent's sandbox.
	// Update (A-160): the preview, app and service tools set it too - the host resolves
	// their links in the session's workspace (a preview link carries the session).
	SessionWorkspace bool `json:"sessionWorkspace,omitempty"`
	// Schedulable (A-103): a tool job may run this tool on a schedule, without the model
	// - its exact arguments are approved when the job is created. Set on tools that are
	// safe to repeat unattended (terminal, run_code, web_search, web_extract); never on
	// clarify, the job tools, skill_manage or file writes.
	Schedulable bool `json:"schedulable,omitempty"`
	// Subagent (A-137) says whether a subagent may use the tool. Empty means
	// SubagentAllowed.
	//   - SubagentNever: a child never gets it (no nesting, no questions to the user,
	//     no lasting self-changes).
	//   - SubagentAllowed: a child may get it, still scoped by its access (a read-only
	//     child gets read-only tools).
	//   - SubagentPreferred: the work is meant to run in a subagent, so its large
	//     output stays out of the parent's context: the parent does not see the tool
	//     while an enabled subagent type of its own has it (the subagent manager's
	//     DelegatedTools); without one the parent keeps it.
	// It is declared last, the key order of core/ts.
	Subagent SubagentUse `json:"subagent,omitempty"`
	// NeedsVision (A-180): the tool's results are images for the model to look at
	// (screenshot), so it is offered only when the session's model can see images
	// (model.Manager.AcceptsImages; a model that reports nothing counts as able). False
	// means always offered. Declared after Subagent, the key order of core/ts.
	NeedsVision bool `json:"needsVision,omitempty"`
}

// Clone returns a deep copy of the definition: the schema (including its
// Properties and Items pointers) and AllowedModes are copied, so the copy can be
// changed or read concurrently with the original.
func (d Definition) Clone() Definition {
	out := d
	out.Parameters = d.Parameters.Clone()
	out.AllowedModes = slices.Clone(d.AllowedModes)
	return out
}

// Handler is a registered tool.
//
// The embedded kernel.Plugin (Name, Version) is the tool's plugin identity, for
// logs and tooling; the ToolHub does not use it and keys every tool by
// Definition().Name. New and NewTypedTool build a Handler from a definition and a
// function and supply Name and Version, so most tools need no type of their own.
//
// Args are the exact bytes the model sent (TLS-07); decode them with
// args.Decode when the fields are needed. A returned error is converted into an
// isError Output by the executor (Phase 9); Output.IsError stays the normal way
// to report a tool-level error.
type Handler interface {
	kernel.Plugin
	Definition() Definition
	Execute(ctx context.Context, args kernel.Arguments, tc Context) (Output, error)
}

// CallRisker is the optional part of a Handler whose risk depends on the call's arguments
// (A-101, ToolHandler.riskFor of core/ts): http_request is read-only for GET and HEAD and
// consequential otherwise. RiskFor decides the approval gate (and the retry rule) for that
// one call; Definition().Risk stays the worst case, which is what a read-only subagent
// scope checks. An empty result counts as consequential.
type CallRisker interface {
	RiskFor(args kernel.Arguments) Risk
}

// Context is what a tool receives next to its arguments.
//
// Cancellation arrives through the ctx parameter of Handler.Execute. GetSandbox,
// GetArtifacts, Interaction and Skills are nil-able: nil means the host wired
// nothing, so a handler must check before use and a missing capability is never
// replaced by a default host implementation.
type Context struct {
	SessionID string
	// RunID (A-105) is the turn the call is made in - one id per agent run (a tool job's
	// call gets its own). Children a turn starts share that turn's budget. "" outside a
	// run.
	RunID string
	// ToolCallID (A-105) is the id of the model's tool call being executed. "" outside a
	// dispatch.
	ToolCallID string
	// GetSandbox returns the agent's sandbox, resolved lazily - the container
	// starts on the first call. It returns an error (typically a
	// *kernel.SandboxError) when the agent has none (fail closed). File and
	// terminal tools must use this instead of the os or os/exec packages. Nil means
	// the host has no sandbox wiring at all.
	GetSandbox func(ctx context.Context) (sandboxhub.View, error)
	// GetArtifacts returns the agent's artifacts (A-62), scoped to this agent and
	// session. Nil when the host wired no artifact store. Artifacts track files in
	// the agent's sandbox - the artifact tools hand the context's sandbox (from
	// GetSandbox) to every view call.
	GetArtifacts func() artifact.View
	// AgentID is the ID of the agent running the tool; "" means unknown.
	AgentID string
	// Interaction is the host-provided channel to the human user (clarify and
	// approval). Nil means there is no interactive user.
	Interaction InteractionHandler
	// Skills is the agent's skills - read by skill_view, written by skill_manage.
	// Nil outside an agent.
	Skills skill.AgentSkills
	// ScheduledJobs is the agent's prompt jobs (A-69) - used by the scheduled-job tools.
	// Nil outside an agent.
	// Triggers is the agent's triggers (A-70) - used by the trigger tools. Nil outside an
	// agent.
	// Update (A-126): Jobs is the agent's jobs of every kind (both merged) - used by
	// manage_job / list_jobs. Nil outside an agent.
	Jobs job.Jobs
	// JobDelivery (A-100 phase 3) is where a job scheduled from this turn sends its
	// results: the chat the turn came from ("home" when it is the channel's home chat).
	// Nil outside a channel turn (a web chat, a scheduled run): the job keeps its results
	// in its conversation.
	// Update (A-70): a trigger created from this turn (manage_trigger) delivers there too.
	// Update (A-126): a job created from this turn (manage_job) delivers there.
	// Update (A-127): JobDeliveryOptions - that chat (Here, nil outside a channel turn)
	// and the agent's channels with a home chat (Homes). By default a job delivers to Here,
	// else to every home chat; manage_job can name the home chats instead.
	JobDeliveryOptions job.DeliveryOptions
	// Subagents (A-105) is the agent's subagents - used by spawn_subagent. Nil outside an
	// agent, or when the host wired none.
	Subagents subagent.Subagents
	// Secrets (A-101) is the agent's secrets - tools that support them (http_request,
	// terminal, run_code) resolve {{secret:NAME}} / secrets: [...] through it; list_secrets
	// and manage_secret use it too. Nil outside an agent.
	Secrets secret.Secrets
	// Emit publishes an event. ToolHub.Authorize replaces a nil Emit with a no-op, so
	// a handler may always call it.
	Emit func(eventType string, payload any)
}

// Output is the result of a tool call.
type Output struct {
	Content string `json:"content"`
	IsError *bool  `json:"isError,omitempty"`
	// Retryable is set to false on an error result that must not be retried by
	// the harness (for example a blocked URL, a user who already answered, a
	// missing interaction channel). Nil means retryable.
	Retryable *bool `json:"retryable,omitempty"`
	// Empty (A-103): the tool succeeded and produced nothing to report (a command that
	// printed nothing - its content still says "exit code 0 (no output)"). A tool job's
	// run is then silent, so nothing is delivered. Not stored with the conversation.
	// It is declared before Metadata, the key order of core/ts.
	Empty bool `json:"empty,omitempty"`
	// Link (A-132) is something the call produced that the user can open (e.g. the
	// artifact it saved). The harness copies it onto the kernel.ToolResult (successful
	// results only), so the UI can link to and open it. Ask the model to cite it with
	// CiteLinkNote.
	Link *kernel.ToolResultLink `json:"link,omitempty"`
	// CompactArguments (A-129) are the call's arguments as later model requests should
	// carry them - e.g. a large "content" replaced by a reference to the file it was
	// written to (WithContentReference). The harness copies them onto the
	// kernel.ToolResult (successful results only); the agent loop swaps them in for the
	// call's arguments, and the UI keeps the original. Empty means none.
	CompactArguments kernel.Arguments `json:"compactArguments,omitempty"`
	// Images (A-180) are images for the model to look at (e.g. a screenshot), at most
	// kernel.MaxImagesPerMessage and within kernel.CheckAttachments' limits (else they are
	// left out with a note). The harness copies them onto the kernel.ToolResult
	// (successful results only).
	Images []kernel.MessageAttachment `json:"images,omitempty"`
	// Metadata keeps the key order of the object at every nesting level
	// (ordered.Object). An absent object is omitted; an explicit {} survives.
	Metadata ordered.Object `json:"metadata,omitzero"`
}

// Clone returns a deep copy: IsError, Retryable, Link and Metadata are copied, so the
// copy shares nothing with the original. Add a line here when a pointer, slice
// or map field is added to Output; the clonecheck test fails until you do.
func (o Output) Clone() Output {
	if o.IsError != nil {
		v := *o.IsError
		o.IsError = &v
	}
	if o.Retryable != nil {
		v := *o.Retryable
		o.Retryable = &v
	}
	if o.Link != nil {
		l := *o.Link
		o.Link = &l
	}
	o.CompactArguments = slices.Clone(o.CompactArguments)
	o.Images = slices.Clone(o.Images) // A-180
	o.Metadata = o.Metadata.Clone()
	return o
}

// ---------------------------------------------------------------------------
// Tool policy - authorization boundary before tool dispatch
// ---------------------------------------------------------------------------

// Decision is the verdict of a policy or an authorization hook. Reason is a
// pointer because a hook may return an explicit empty reason, which must stay
// distinct from an absent one.
type Decision struct {
	Allowed bool    `json:"allowed"`
	Reason  *string `json:"reason,omitempty"`
}

// Allow returns an allowing Decision.
func Allow() Decision {
	return Decision{Allowed: true}
}

// Deny returns a denying Decision with the given reason.
func Deny(reason string) Decision {
	return Decision{Reason: &reason}
}

// AuthorizationHook is consumer-provided business logic that runs after the
// allowlist and the schema check have passed. A returned error or a panic is
// treated as a denial by DefaultPolicy.
type AuthorizationHook func(ctx context.Context, toolName string, args kernel.Arguments, tc Context) (Decision, error)

// Policy is checked before every tool dispatch. Check returns only a Decision,
// so a policy is fail-closed by construction.
type Policy interface {
	kernel.Plugin
	Check(ctx context.Context, toolName string, args kernel.Arguments, tc Context, def Definition, allowedTools []string) Decision
}

// DefaultPolicyOptions configures NewDefaultPolicy.
type DefaultPolicyOptions struct {
	// AuthorizationHook is consumer-provided business rules; nil means none.
	AuthorizationHook AuthorizationHook
}

// Authorized is proof that the policy allowed one exact call. It is the only
// value that can run a registered handler through the hub. It has no exported
// fields and cannot be built outside this package; the zero value refuses to
// execute.
//
// It is single-use: the first Execute consumes it (all copies share the flag), a
// second returns ErrAlreadyExecuted. It is also bound to the policy that allowed
// it: once SetPolicy has run, Execute returns ErrPolicyChanged and the caller
// must Authorize again.
type Authorized struct {
	hub     *ToolHub
	gen     uint64
	handler Handler
	def     Definition
	args    kernel.Arguments
	tc      Context
	used    *atomic.Bool
}
