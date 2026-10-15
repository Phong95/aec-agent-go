// Package sandboxhub is the data owner for the sandbox contracts of the SDK.
//
// Phase 4 holds only the types that an AgentProfile needs (the sandbox
// selection and its per-agent configuration), so that the profile fixture can
// be built without a layering violation. Phase 7 adds the hub, the providers
// and the behavioural contracts to this package.
//
// Phase 5 adds the View, Sandbox and Provider contracts and their data types
// (types_view.go, types_provider.go, types_policy.go).
//
// Phase 7 adds the SandboxHub (hub.go, hub_policy.go), the logical-path rules
// (logical_path.go), the default approval policy (approval.go) and TracedView
// (traced.go, WithTracing); the backends arrive in later Phase 7 plans as the
// sub-packages sandbox/host and sandbox/docker.
//
// This file holds the selection, policy and provider data types the profile
// needs (a subset of the sandbox contracts).
package sandboxhub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// Kind names the provider that runs a sandbox.
type Kind string

// The provider kinds.
const (
	KindDocker Kind = "docker"
	KindHost   Kind = "host"
	KindRemote Kind = "remote"
)

// NetworkMode is the network access of a sandbox ("none" | "allowlist" | "full").
type NetworkMode string

// The network modes an own sandbox may ask for.
const (
	NetworkNone      NetworkMode = "none"
	NetworkAllowlist NetworkMode = "allowlist"
	NetworkFull      NetworkMode = "full"
)

// ResourceLimits are the resources of one sandbox.
type ResourceLimits struct {
	Cpus     float64 `json:"cpus"`
	MemoryMb int     `json:"memoryMb"`
	// Pids is the max number of processes inside the sandbox.
	Pids int `json:"pids"`
}

// LimitsOverride is a partial ResourceLimits: every field is optional and zero
// is a meaningful value, so each is a pointer.
type LimitsOverride struct {
	Cpus     *float64 `json:"cpus,omitempty"`
	MemoryMb *int     `json:"memoryMb,omitempty"`
	Pids     *int     `json:"pids,omitempty"`
}

// OwnConfig configures a dedicated sandbox for one agent.
type OwnConfig struct {
	// Kind picks the provider that runs it. It must be one of the hub policy's
	// own kinds (the server decides where each kind lives; a profile only picks
	// the kind). Default: the hub's default own kind. Image, network and limits
	// below are container settings and are ignored for "host".
	Kind Kind `json:"kind,omitempty"`
	// Image must be in the hub policy's allowed images (or equal the shared image).
	Image string `json:"image,omitempty"`
	// Network must be in the hub policy's allowed network modes; "full" only when
	// the host opted in. Default "none".
	Network NetworkMode `json:"network,omitempty"`
	// Limits are clamped to the hub policy's max limits.
	Limits *LimitsOverride `json:"limits,omitempty"`
}

// SelectionMode says which of the three Selection forms is in use.
type SelectionMode string

// The three selection forms. Omitted in a profile (the zero Selection) means
// the shared sandbox.
const (
	// SelectionShared is the AgentCore-wide shared sandbox (wire form "shared").
	SelectionShared SelectionMode = "shared"
	// SelectionNone is no sandbox; file/terminal tools fail closed (wire form "none").
	SelectionNone SelectionMode = "none"
	// SelectionOwn is a dedicated sandbox for this agent only (wire form {"own":{...}}).
	SelectionOwn SelectionMode = "own"
)

// Selection is where an agent's file/terminal tools run.
//
// Wire forms: "shared", "none" or {"own": OwnConfig}. Profiles are editable
// over the API, so the decoder accepts exactly those three shapes and returns
// an error for anything else. The zero value is not a valid wire value:
// marshalling it fails, and AgentProfile omits it (tag omitzero).
type Selection struct {
	Mode SelectionMode
	// Own is set if and only if Mode is SelectionOwn.
	Own *OwnConfig
}

// ownEnvelope is the {"own": ...} object form.
type ownEnvelope struct {
	Own OwnConfig `json:"own"`
}

// MarshalJSON encodes shared and none as a JSON string and own as {"own":{...}}.
// Any other state, including the zero value, returns an error.
func (s Selection) MarshalJSON() ([]byte, error) {
	switch s.Mode {
	case SelectionShared, SelectionNone:
		if s.Own != nil {
			return nil, fmt.Errorf("sandbox: selection %q must not carry an own config", s.Mode)
		}
		return jsonx.Marshal(string(s.Mode))
	case SelectionOwn:
		if s.Own == nil {
			return nil, errors.New("sandbox: selection \"own\" requires an own config")
		}
		return jsonx.Marshal(ownEnvelope{Own: *s.Own})
	default:
		return nil, fmt.Errorf("sandbox: invalid selection mode %q", s.Mode)
	}
}

// UnmarshalJSON decodes the three wire forms. A string must be "shared" or
// "none"; an object must hold exactly the key "own" whose value strictly
// decodes into OwnConfig. Everything else (null, numbers, arrays, empty or
// extended objects, unknown strings) returns an error and leaves s unchanged.
func (s *Selection) UnmarshalJSON(data []byte) error {
	trimmed := bytes.Trim(data, " \t\r\n") // JSON whitespace only, not Unicode spaces
	if len(trimmed) == 0 {
		return errors.New("sandbox: empty selection")
	}
	switch trimmed[0] {
	case '"':
		var str string
		if err := json.Unmarshal(trimmed, &str); err != nil {
			return fmt.Errorf("sandbox: invalid selection string: %w", err)
		}
		switch SelectionMode(str) {
		case SelectionShared, SelectionNone:
			*s = Selection{Mode: SelectionMode(str)}
			return nil
		default:
			return fmt.Errorf("sandbox: selection string %q must be \"shared\" or \"none\"", str)
		}
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return fmt.Errorf("sandbox: invalid selection object: %w", err)
		}
		raw, ok := obj["own"]
		if !ok || len(obj) != 1 {
			return errors.New("sandbox: selection object must contain exactly the key \"own\"")
		}
		rawTrimmed := bytes.Trim(raw, " \t\r\n")
		if len(rawTrimmed) == 0 || rawTrimmed[0] != '{' {
			return errors.New("sandbox: selection \"own\" must be an object")
		}
		var own OwnConfig
		if err := jsonx.UnmarshalStrict(rawTrimmed, &own); err != nil {
			return fmt.Errorf("sandbox: invalid own config: %w", err)
		}
		*s = Selection{Mode: SelectionOwn, Own: &own}
		return nil
	default:
		return errors.New("sandbox: selection must be \"shared\", \"none\" or {\"own\":{...}}")
	}
}

// Layout is the default layout of an agent's view of a sandbox (its working
// directory and roots).
type Layout struct {
	WorkingDir string   `json:"workingDir"`
	Roots      []string `json:"roots"`
}
