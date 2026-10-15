package profile

import (
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// TestDefaultCompactorConfig: the default is the conformance suite's default compactor and
// the config CreateAgent hard-codes, with no optional field set, and every call
// returns a fresh value.
func TestDefaultCompactorConfig(t *testing.T) {
	got := DefaultCompactorConfig()
	want := `{"contextWindowTokens":200000,"threshold":0.8,"minTailUserMessages":4,"protectFirstN":2,"proactivePruneTokens":50000,"oldToolResultMinTokens":2000,"tailTargetRatio":0.2,"protectLastN":10,"summarizeMaxOutputTokens":4096}`
	b, err := jsonx.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("DefaultCompactorConfig() = %s, want %s", b, want)
	}
	got.Threshold = 0
	if DefaultCompactorConfig().Threshold != 0.8 {
		t.Fatal("changing a returned config changed the default")
	}
}
