package group

import (
	"encoding/json"
	"slices"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// PlanKind is what one user message sets off. The string values are stable wire values.
type PlanKind string

// The plan kinds.
const (
	// PlanSingle: one member answers alone (AgentID).
	PlanSingle PlanKind = "single"
	// PlanRound: several members answer side by side (AgentIDs); then the lead sums up
	// when Summarize.
	PlanRound PlanKind = "round"
	// PlanLead: the lead answers (LeadID) and may hand parts to Helpers (the active members
	// besides it).
	PlanLead PlanKind = "lead"
	// PlanNone: nobody answers ("only when mentioned" and nobody was addressed); Reason
	// says so.
	PlanNone PlanKind = "none"
)

// Plan is what one user message sets off (the GroupPlan union of core/ts). Only the
// fields of its Kind are used and written: single {agentId}, round {agentIds,
// summarize}, lead {leadId, helpers}, none {reason}.
type Plan struct {
	Kind PlanKind
	// AgentID is the member of a single plan.
	AgentID string
	// AgentIDs are the members of a round.
	AgentIDs []string
	// Summarize: the lead sums the round up.
	Summarize bool
	// LeadID is the lead of a lead plan.
	LeadID string
	// Helpers are the members the lead may hand parts to.
	Helpers []string
	// Reason is why nobody answers (none).
	Reason string
}

// Clone returns a deep copy (the id lists are copied).
func (p Plan) Clone() Plan {
	p.AgentIDs = slices.Clone(p.AgentIDs)
	p.Helpers = slices.Clone(p.Helpers)
	return p
}

// planWire is the JSON of every plan kind.
type planWire struct {
	Kind      PlanKind  `json:"kind"`
	AgentID   *string   `json:"agentId,omitempty"`
	AgentIDs  *[]string `json:"agentIds,omitempty"`
	Summarize *bool     `json:"summarize,omitempty"`
	LeadID    *string   `json:"leadId,omitempty"`
	Helpers   *[]string `json:"helpers,omitempty"`
	Reason    *string   `json:"reason,omitempty"`
}

// MarshalJSON writes the fields of the plan's kind, in the order core/ts writes them.
func (p Plan) MarshalJSON() ([]byte, error) {
	w := planWire{Kind: p.Kind}
	orEmpty := func(ids []string) *[]string {
		if ids == nil {
			ids = []string{}
		}
		return &ids
	}
	switch p.Kind {
	case PlanSingle:
		w.AgentID = &p.AgentID
	case PlanRound:
		w.AgentIDs = orEmpty(p.AgentIDs)
		w.Summarize = &p.Summarize
	case PlanLead:
		w.LeadID = &p.LeadID
		w.Helpers = orEmpty(p.Helpers)
	case PlanNone:
		w.Reason = &p.Reason
	}
	return jsonx.Marshal(w)
}

// UnmarshalJSON reads a plan of any kind.
func (p *Plan) UnmarshalJSON(data []byte) error {
	var w planWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	out := Plan{Kind: w.Kind}
	if w.AgentID != nil {
		out.AgentID = *w.AgentID
	}
	if w.AgentIDs != nil {
		out.AgentIDs = *w.AgentIDs
	}
	if w.Summarize != nil {
		out.Summarize = *w.Summarize
	}
	if w.LeadID != nil {
		out.LeadID = *w.LeadID
	}
	if w.Helpers != nil {
		out.Helpers = *w.Helpers
	}
	if w.Reason != nil {
		out.Reason = *w.Reason
	}
	*p = out
	return nil
}
