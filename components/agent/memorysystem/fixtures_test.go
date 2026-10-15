package memorysystem

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// memorysystemFixtures are the memorysystem- fixtures of tests/testdata/golden, each with
// the test that replays it. TestMemorysystemFixturesCovered fails when a fixture on
// disk is not in this list (a recorded corpus nobody compares) or a name here has no
// file. Later plans of the phase add their fixtures (10-03 added the schedule and recall-for-turn ones).
var memorysystemFixtures = map[string]string{
	"memorysystem-evaluate":    "TestEvaluateMatchesGolden",
	"memorysystem-prompts":     "TestPromptsMatchGolden",
	"memorysystem-tokenize":    "TestTokenizeMatchesGolden",
	"memorysystem-recall":      "TestRecallMatchesGolden",
	"memorysystem-consolidate": "TestConsolidateMatchesGolden",

	// Plan 10-03: the recorded schedule transcripts and recallForTurn results.
	"memorysystem-schedule":        "TestScheduleMatchesGolden",
	"memorysystem-recall-for-turn": "TestRecallForTurnMatchesGolden",
}

func TestMemorysystemFixturesCovered(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(wiretest.Dir(t), "memorysystem-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, f := range files {
		onDisk = append(onDisk, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	for _, name := range onDisk {
		if _, ok := memorysystemFixtures[name]; !ok {
			t.Errorf("fixture %s is recorded but no test replays it: add it to memorysystemFixtures", name)
		}
	}
	for name, test := range memorysystemFixtures {
		if !slices.Contains(onDisk, name) {
			t.Errorf("%s (%s) has no fixture file", name, test)
		}
		if _, err := os.Stat(filepath.Join(wiretest.Dir(t), name+".json")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
