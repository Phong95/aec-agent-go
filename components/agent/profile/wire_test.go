package profile_test

import (
	"os"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// profileCases lists every profile golden fixture; each must round-trip
// byte-identically through profile.AgentProfile.
var profileCases = []string{
	"profile-minimal.json",
	"profile-full.json",
	"profile-sandbox-shared.json",
	"profile-sandbox-none.json",
	"profile-sandbox-own.json",
	"profile-zero-values.json",
}

func TestProfileWireFixtures(t *testing.T) {
	for _, file := range profileCases {
		t.Run(strings.TrimSuffix(file, ".json"), func(t *testing.T) {
			wiretest.AssertRoundTrip(t, file, new(profile.AgentProfile))
		})
	}
}

// TestProfileFixturesCovered fails when a profile-* fixture has no table entry
// (or an entry has no fixture), so a fixture added to wiregen cannot go unchecked.
func TestProfileFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	covered := map[string]bool{}
	for _, f := range profileCases {
		covered[f] = true
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "profile-") {
			onDisk[e.Name()] = true
			if !covered[e.Name()] {
				t.Errorf("fixture %s has no entry in profileCases", e.Name())
			}
		}
	}
	for f := range covered {
		if !onDisk[f] {
			t.Errorf("profileCases entry %s has no fixture file", f)
		}
	}
}

// Required collections (tools, skills) encode as [] even when nil.
func TestProfileNilCollections(t *testing.T) {
	p := profile.AgentProfile{
		ID: "a", Name: "n", SystemPrompt: "s",
		Memory: memory.Config{},
	}
	for name, v := range map[string]any{"value": p, "pointer": &p} {
		b, err := jsonx.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(b), `"tools":[],"skills":[]`) {
			t.Fatalf("%s: nil collections not encoded as []: %s", name, b)
		}
		if strings.Contains(string(b), `"sandbox"`) {
			t.Fatalf("%s: zero sandbox selection must be omitted: %s", name, b)
		}
	}
}

// A profile that selects an own sandbox keeps the pointer-typed limits.
func TestProfileSandboxOwnDecodes(t *testing.T) {
	var p profile.AgentProfile
	wiretest.AssertRoundTrip(t, "profile-full.json", &p)
	if p.Sandbox.Mode != sandboxhub.SelectionOwn || p.Sandbox.Own == nil || p.Sandbox.Own.Limits == nil {
		t.Fatalf("profile-full sandbox not decoded as own with limits: %+v", p.Sandbox)
	}
	if p.LoopConfig == nil || p.LoopConfig.MaxToolRetries == nil || *p.LoopConfig.MaxToolRetries != 0 {
		t.Fatalf("explicit zero maxToolRetries lost: %+v", p.LoopConfig)
	}
}
