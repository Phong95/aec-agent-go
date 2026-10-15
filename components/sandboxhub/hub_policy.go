package sandboxhub

import (
	"math"
	"slices"

	"github.com/Phong95/aec-agent-go/kernel"
)

// DefaultHubPolicy returns the default hub policy: no extra images, a ceiling of 2 cpus, 2048 MB and 256 processes, only the "none"
// network, and docker as the only (and default) own-sandbox kind. Every slice is
// fresh, so a caller may modify the result.
func DefaultHubPolicy() HubPolicy {
	return HubPolicy{
		AllowedImages:  []string{},
		MaxLimits:      ResourceLimits{Cpus: 2, MemoryMb: 2048, Pids: 256},
		AllowedNetwork: []NetworkMode{NetworkNone},
		OwnKinds:       []Kind{KindDocker},
		DefaultOwnKind: KindDocker,
	}
}

// SetPolicy applies u to the hub policy. The slices of u are
// copied, so the caller may keep modifying them.
func (h *SandboxHub) SetPolicy(u PolicyUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u.AllowedImages != nil {
		h.policy.AllowedImages = slices.Clone(*u.AllowedImages)
	}
	if u.AllowedNetwork != nil {
		h.policy.AllowedNetwork = slices.Clone(*u.AllowedNetwork)
	}
	if u.OwnKinds != nil {
		h.policy.OwnKinds = slices.Clone(*u.OwnKinds)
	}
	if u.DefaultOwnKind != nil {
		h.policy.DefaultOwnKind = *u.DefaultOwnKind
	}
	if u.MaxLimits != nil {
		if u.MaxLimits.Cpus != nil {
			h.policy.MaxLimits.Cpus = *u.MaxLimits.Cpus
		}
		if u.MaxLimits.MemoryMb != nil {
			h.policy.MaxLimits.MemoryMb = *u.MaxLimits.MemoryMb
		}
		if u.MaxLimits.Pids != nil {
			h.policy.MaxLimits.Pids = *u.MaxLimits.Pids
		}
	}
}

// Policy returns a deep copy of the hub policy (a copy cannot be used to change
// the policy behind the hub's back).
func (h *SandboxHub) Policy() HubPolicy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.policy.Clone()
}

// AllowedOwnKinds are the kinds an own sandbox may use here: the default first,
// then the others the policy allows, each once.
func (h *SandboxHub) AllowedOwnKinds() []Kind {
	h.mu.Lock()
	defer h.mu.Unlock()
	return allowedOwnKinds(h.policy)
}

func allowedOwnKinds(p HubPolicy) []Kind {
	out := make([]Kind, 0, len(p.OwnKinds)+1)
	out = append(out, p.DefaultOwnKind)
	for _, k := range p.OwnKinds {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// SetApprovalPolicy sets which sandbox operations need user approval in which
// sandboxes. A nil policy restores DefaultApprovalPolicy.
func (h *SandboxHub) SetApprovalPolicy(p ApprovalPolicy) {
	if p == nil {
		p = DefaultApprovalPolicy
	}
	h.mu.Lock()
	h.approval = p
	h.mu.Unlock()
}

// RequiresApproval reports whether op in a sandbox with these traits needs
// explicit user approval. The policy runs outside the hub lock, so it may call
// back into the hub.
func (h *SandboxHub) RequiresApproval(traits Traits, op Operation) bool {
	h.mu.Lock()
	p := h.approval
	h.mu.Unlock()
	return p(traits, op)
}

// BuildOwnSpec validates a profile-supplied own-sandbox config against the hub
// policy and returns the spec. Every rejection is a policy_denied
// *kernel.SandboxError. The checks run in this order: kind, then (for container
// kinds) image, network and limits.
//
// An empty Kind, Image or Network means absent (an explicit "" image is treated
// as not set). A limit is kept only when it is finite and greater than zero, and
// is then clamped to the policy ceiling; the result always carries all three
// limits, the policy maximum where the config asked for nothing valid.
func (h *SandboxHub) BuildOwnSpec(own OwnConfig) (Spec, error) {
	h.mu.Lock()
	policy := h.policy.Clone()
	sharedImage := ""
	if h.sharedSpec != nil {
		sharedImage = h.sharedSpec.Image
	}
	h.mu.Unlock()
	return buildOwnSpec(policy, sharedImage, own)
}

func buildOwnSpec(policy HubPolicy, sharedImage string, own OwnConfig) (Spec, error) {
	kind := own.Kind
	if kind == "" {
		kind = policy.DefaultOwnKind
	}
	if !slices.Contains(allowedOwnKinds(policy), kind) {
		return Spec{}, kernel.NewSandboxError(
			`Own sandboxes of kind "`+string(kind)+`" are not allowed on this server`,
			kernel.SandboxCodePolicyDenied, "")
	}
	// A host folder runs on the machine itself - no image, network or resource settings.
	if kind == KindHost {
		return Spec{Kind: kind}, nil
	}

	image := own.Image
	if image == "" {
		image = sharedImage
	}
	if own.Image != "" && own.Image != sharedImage && !slices.Contains(policy.AllowedImages, own.Image) {
		return Spec{}, kernel.NewSandboxError(
			`Image "`+own.Image+`" is not in the allowed image list`,
			kernel.SandboxCodePolicyDenied, "")
	}

	network := own.Network
	if network == "" {
		network = NetworkNone
	}
	if !slices.Contains(policy.AllowedNetwork, network) {
		return Spec{}, kernel.NewSandboxError(
			`Network mode "`+string(network)+`" is not allowed for agent sandboxes`,
			kernel.SandboxCodePolicyDenied, "")
	}

	// limits: { ...policy.maxLimits, ...limits } - the policy maximum overlaid by
	// the valid requested values (clamped), keys in the order cpus, memoryMb, pids.
	cpus, memoryMb, pids := policy.MaxLimits.Cpus, policy.MaxLimits.MemoryMb, policy.MaxLimits.Pids
	if l := own.Limits; l != nil {
		if l.Cpus != nil && !math.IsNaN(*l.Cpus) && !math.IsInf(*l.Cpus, 0) && *l.Cpus > 0 {
			cpus = math.Min(*l.Cpus, policy.MaxLimits.Cpus)
		}
		if l.MemoryMb != nil && *l.MemoryMb > 0 {
			memoryMb = min(*l.MemoryMb, policy.MaxLimits.MemoryMb)
		}
		if l.Pids != nil && *l.Pids > 0 {
			pids = min(*l.Pids, policy.MaxLimits.Pids)
		}
	}
	return Spec{
		Kind:    kind,
		Image:   image,
		Network: network,
		Limits:  &LimitsOverride{Cpus: &cpus, MemoryMb: &memoryMb, Pids: &pids},
	}, nil
}
