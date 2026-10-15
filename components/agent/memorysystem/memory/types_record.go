package memory

// ---------------------------------------------------------------------------
// Core memory record - the typed unit every pipeline stage operates on
//
// The enums are string-typed and are not validated on decode: a newer build may
// add values, and a stored record must still load.
// ---------------------------------------------------------------------------

// Kind says what a memory record holds.
type Kind string

// The kinds of a memory record.
const (
	KindUserPreference  Kind = "user_preference"
	KindFact            Kind = "fact"
	KindDecision        Kind = "decision"
	KindProcedure       Kind = "procedure"
	KindTaskState       Kind = "task_state"
	KindSourceReference Kind = "source_reference"
)

// Scope says how widely a memory record applies.
type Scope string

// The scopes of a memory record.
const (
	ScopeUser         Scope = "user"
	ScopeProject      Scope = "project"
	ScopeProfile      Scope = "profile"
	ScopeOrganization Scope = "organization"
	ScopeTask         Scope = "task"
)

// Status is the lifecycle state of a memory record.
type Status string

// The statuses of a memory record.
const (
	StatusActive     Status = "active"
	StatusSuperseded Status = "superseded"
	StatusDisputed   Status = "disputed"
	StatusExpired    Status = "expired"
)

// Confidence says how sure the pipeline is of a memory record.
type Confidence string

// The confidence levels of a memory record.
const (
	ConfidenceExplicit   Confidence = "explicit"
	ConfidenceVerified   Confidence = "verified"
	ConfidenceInferred   Confidence = "inferred"
	ConfidenceUnverified Confidence = "unverified"
)

// SourceTrust is the trust boundary of a record: it controls whether a source
// can auto-promote to durable memory.
type SourceTrust string

// The trust levels of a memory source.
const (
	TrustUserApproved     SourceTrust = "user_approved"
	TrustVerifiedInternal SourceTrust = "verified_internal"
	TrustTeamAuthored     SourceTrust = "team_authored"
	TrustExternalDocument SourceTrust = "external_document"
	TrustUntrusted        SourceTrust = "untrusted"
	TrustModelSpeculation SourceTrust = "model_speculation"
)

// Record is the core memory record. Every timestamp is an ISO string, never a
// time.Time, so a stored record round-trips byte-identical. Optional strings are
// omitted when empty (an explicit empty string is never written there).
type Record struct {
	ID string `json:"id"`
	// AgentID is the owning agent (AgentProfile.id). Every memory belongs to
	// exactly one agent; all reads are filtered by it.
	AgentID     string      `json:"agentId"`
	Kind        Kind        `json:"kind"`
	Scope       Scope       `json:"scope"`
	Subject     string      `json:"subject"`
	Key         string      `json:"key"`
	Value       string      `json:"value"`
	Status      Status      `json:"status"`
	Confidence  Confidence  `json:"confidence"`
	Source      string      `json:"source"`
	SourceTrust SourceTrust `json:"sourceTrust"`
	// ObservedAt is when the record was observed (ISO string).
	ObservedAt string `json:"observedAt"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
	// LastRecalledAt is the last time recall returned this record. It keeps
	// memories in active use from expiring; it does not affect recency ranking.
	LastRecalledAt string   `json:"lastRecalledAt,omitempty"`
	SupersededBy   string   `json:"supersededBy,omitempty"`
	ExpiresAt      string   `json:"expiresAt,omitempty"`
	Tags           []string `json:"tags,omitzero"`
}
