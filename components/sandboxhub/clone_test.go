package sandboxhub_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Selection.Clone must share nothing with the original: the own config, its
// limits and every limit pointer are copied.
func TestSelectionCloneIsDeep(t *testing.T) {
	cpus, mem, pids := 0.5, 512, 128
	sample := sandboxhub.Selection{
		Mode: sandboxhub.SelectionOwn,
		Own: &sandboxhub.OwnConfig{
			Kind: sandboxhub.KindDocker, Image: "node:22", Network: sandboxhub.NetworkAllowlist,
			Limits: &sandboxhub.LimitsOverride{Cpus: &cpus, MemoryMb: &mem, Pids: &pids},
		},
	}
	clonecheck.AssertDeep(t, sample, sandboxhub.Selection.Clone)
}

// The string forms carry no own config and stay that way.
func TestSelectionCloneOfStringForms(t *testing.T) {
	for _, mode := range []sandboxhub.SelectionMode{sandboxhub.SelectionShared, sandboxhub.SelectionNone} {
		got := sandboxhub.Selection{Mode: mode}.Clone()
		if got.Mode != mode || got.Own != nil {
			t.Fatalf("Clone of %q = %#v", mode, got)
		}
	}
}

// Spec.Clone must share nothing with the original: the limits (and every limit
// pointer) and the Options object, including nested values.
func TestSpecCloneIsDeep(t *testing.T) {
	cpus, mem, pids := 0.5, 512, 128
	opts := ordered.NewObject()
	opts.Set("folder", "/work")
	opts.Set("nested", []any{"a", ordered.NewObject()})
	sample := sandboxhub.Spec{
		Kind:    sandboxhub.KindDocker,
		Image:   "node:22",
		Network: sandboxhub.NetworkAllowlist,
		Limits:  &sandboxhub.LimitsOverride{Cpus: &cpus, MemoryMb: &mem, Pids: &pids},
		Options: opts,
	}
	clonecheck.AssertDeep(t, sample, sandboxhub.Spec.Clone)
}

// An absent Options stays absent (omitzero) and a nil Limits stays nil.
func TestSpecCloneKeepsAbsentParts(t *testing.T) {
	got := sandboxhub.Spec{Kind: sandboxhub.KindHost}.Clone()
	if got.Limits != nil || !got.Options.IsZero() {
		t.Fatalf("Clone invented parts: %#v", got)
	}
}

// HubPolicy.Clone must share nothing with the original: the three slices are
// copied, so a caller and a hub never share a mutable part of a policy.
func TestHubPolicyCloneIsDeep(t *testing.T) {
	sample := sandboxhub.HubPolicy{
		AllowedImages:  []string{"node:22", "python:3.12-slim"},
		MaxLimits:      sandboxhub.ResourceLimits{Cpus: 2, MemoryMb: 2048, Pids: 256},
		AllowedNetwork: []sandboxhub.NetworkMode{sandboxhub.NetworkNone, sandboxhub.NetworkFull},
		OwnKinds:       []sandboxhub.Kind{sandboxhub.KindDocker, sandboxhub.KindHost},
		DefaultOwnKind: sandboxhub.KindDocker,
	}
	clonecheck.AssertDeep(t, sample, sandboxhub.HubPolicy.Clone)
}

// A nil slice stays nil and an empty one stays empty, so the copy encodes like
// the original.
func TestHubPolicyCloneKeepsNilAndEmptySlices(t *testing.T) {
	got := sandboxhub.HubPolicy{AllowedImages: []string{}}.Clone()
	if got.AllowedImages == nil || got.AllowedNetwork != nil || got.OwnKinds != nil {
		t.Fatalf("Clone changed nil-ness: %#v", got)
	}
}
