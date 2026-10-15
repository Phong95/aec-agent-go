package sandboxhub

import "github.com/Phong95/aec-agent-go/internal/jsonx"

// ---------------------------------------------------------------------------
// SandboxHub policy - host-controlled rules applied to profile-supplied selections.
//
// This file holds HubPolicy, ApprovalPolicy and PolicyUpdate (Selection and
// OwnConfig are in types.go, Operation is in types_view.go).
// ---------------------------------------------------------------------------

// HubPolicy holds the host-controlled rules applied to profile-supplied
// selections.
type HubPolicy struct {
	// AllowedImages are the images an agent may request for its own sandbox,
	// besides the shared image.
	AllowedImages []string `json:"allowedImages"`
	// MaxLimits is the ceiling for own-sandbox resources; requested values above
	// are clamped.
	MaxLimits ResourceLimits `json:"maxLimits"`
	// AllowedNetwork are the network modes an own sandbox may request. Add "full"
	// only as a deliberate host opt-in (unrestricted internet).
	AllowedNetwork []NetworkMode `json:"allowedNetwork"`
	// OwnKinds are the provider kinds a profile may pick for its own sandbox.
	// Default ["docker"].
	OwnKinds []Kind `json:"ownKinds"`
	// DefaultOwnKind is the provider kind used for own sandboxes when the profile
	// names none. Default "docker".
	DefaultOwnKind Kind `json:"defaultOwnKind"`
}

// MarshalJSON writes a nil AllowedImages, AllowedNetwork or OwnKinds as [], because
// the wire fields are required arrays and a zero or partly built HubPolicy must not
// encode null.
func (p HubPolicy) MarshalJSON() ([]byte, error) {
	type plain HubPolicy
	if p.AllowedImages == nil {
		p.AllowedImages = []string{}
	}
	if p.AllowedNetwork == nil {
		p.AllowedNetwork = []NetworkMode{}
	}
	if p.OwnKinds == nil {
		p.OwnKinds = []Kind{}
	}
	return jsonx.Marshal(plain(p))
}

// ApprovalPolicy decides whether an operation needs explicit user approval in a
// given sandbox. Wired into the A-58 approval gate by A-61.
type ApprovalPolicy func(traits Traits, op Operation) bool

// PolicyUpdate is a partial HubPolicy: every field is optional, so each is a
// pointer, and a set field replaces the policy field at the top level (an empty
// list replaces the list with an empty one). MaxLimits is the one exception: it
// merges key by key, each set limit replacing the current one and the others kept.
// It is not wire data.
type PolicyUpdate struct {
	AllowedImages  *[]string       `json:"allowedImages,omitempty"`
	MaxLimits      *LimitsOverride `json:"maxLimits,omitempty"`
	AllowedNetwork *[]NetworkMode  `json:"allowedNetwork,omitempty"`
	OwnKinds       *[]Kind         `json:"ownKinds,omitempty"`
	DefaultOwnKind *Kind           `json:"defaultOwnKind,omitempty"`
}
