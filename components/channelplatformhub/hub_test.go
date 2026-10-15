package channelplatformhub_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
)

func TestHubClaims(t *testing.T) {
	h := channelplatformhub.New()
	if holder := h.Claim("telegram", "42", "c1"); holder != "" {
		t.Fatalf("first claim held by %q", holder)
	}
	if holder := h.Claim("telegram", "42", "c1"); holder != "" {
		t.Fatal("a channel must be able to re-claim its own bot")
	}
	if holder := h.Claim("telegram", "42", "c2"); holder != "c1" {
		t.Fatalf("second claim: holder %q, want c1", holder)
	}
	h.Release("c1")
	if holder := h.Claim("telegram", "42", "c2"); holder != "" {
		t.Fatalf("claim after release held by %q", holder)
	}
	if err := h.Register(nil); err == nil {
		t.Fatal("a nil platform must be refused")
	}
}
