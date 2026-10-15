package memorysystem

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// The extraction path against the recorded fixture.
//
// memorysystem-evaluate.json is recorded by tools/wiregen/memory/evaluate.cases.mjs:
// every scenario ran the memory extraction over a scripted model and the SDK's
// in-memory memory store under a pinned clock and a fresh id counter. The Go test
// runs the same scenario with the same pinned clock and an id function that mimics
// the counter, and requires the same records, the same store and the same chat
// input, byte for byte.
// ---------------------------------------------------------------------------

type evalSeedKeys struct {
	Scope string   `json:"scope"`
	Keys  []string `json:"keys"`
}

type evalResult struct {
	Records json.RawMessage `json:"records"`
	Throws  *string         `json:"throws"`
	Store   json.RawMessage `json:"store"`
	Calls   json.RawMessage `json:"calls"`
}

type evalScenario struct {
	Name       string           `json:"name"`
	AgentID    string           `json:"agentId"`
	ClockMs    int64            `json:"clockMs"`
	Seed       []memory.Record  `json:"seed"`
	SeedKeys   *evalSeedKeys    `json:"seedKeys"`
	Messages   []kernel.Message `json:"messages"`
	Reply      reply            `json:"reply"`
	OmitStore  bool             `json:"omitStore"`
	Exception  string           `json:"exception"`
	ResultJSON evalResult       `json:"result"`
}

// seedRecords returns the records a scenario starts with: the listed ones, or the
// expansion of seedKeys (the template tools/wiregen/memory/evaluate.cases.mjs uses).
func (sc evalScenario) seedRecords() []memory.Record {
	if sc.SeedKeys == nil {
		return sc.Seed
	}
	out := make([]memory.Record, len(sc.SeedKeys.Keys))
	for i, key := range sc.SeedKeys.Keys {
		out[i] = memory.Record{
			ID: fmt.Sprintf("seed-%d", i), AgentID: "agent-1", Kind: memory.KindUserPreference,
			Scope: memory.Scope(sc.SeedKeys.Scope), Subject: "user", Key: key, Value: fmt.Sprintf("value %d", i),
			Status: memory.StatusActive, Confidence: memory.ConfidenceExplicit, Source: "conversation",
			SourceTrust: memory.TrustUserApproved, ObservedAt: "2023-11-01T00:00:00.000Z",
		}
	}
	return out
}

// evalExceptions names, for each exception scenario, the field the exception is
// about; see the doc comment of toExtraction.
var evalExceptions = map[string]string{
	"exception-value-is-a-number":          "value",
	"exception-kind-is-an-object":          "kind",
	"exception-confidence-is-a-bool":       "confidence",
	"exception-value-null":                 "value",
	"exception-subject-missing":            "subject",
	"exception-element-is-an-array":        "kind",
	"exception-reply-is-not-json":          "",
	"exception-reply-has-trailing-garbage": "",
}

func TestEvaluateMatchesGolden(t *testing.T) {
	var scenarios []evalScenario
	loadFixture(t, "memorysystem-evaluate", &scenarios)
	if len(scenarios) < 12 {
		t.Fatalf("the fixture holds %d scenarios, want at least 12", len(scenarios))
	}
	seen := map[string]bool{}
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if seen[sc.Name] {
				t.Fatalf("duplicate scenario name %q", sc.Name)
			}
			seen[sc.Name] = true

			seed := sc.seedRecords()
			store := seedStore(t, seed...)
			model := newFakeModel(sc.Reply)
			mem := newMemory(newTestDeps(sc.AgentID, WithClock(fixedClock(sc.ClockMs)), WithIDs(tsIDs(sc.ClockMs))), model, store)

			got, err := mem.evaluateMessages(t.Context(), sc.Messages)

			// The chat input is the same in every scenario, exception or not.
			calls := model.extractionCalls()
			if gotCalls := encode(t, nonNilCalls(calls)); gotCalls != string(sc.ResultJSON.Calls) {
				t.Errorf("chat calls differ\n got: %s\nwant: %s", gotCalls, sc.ResultJSON.Calls)
			}
			if !model.zeroOptions() {
				t.Errorf("the model was called with options, TS passes the messages only")
			}

			if sc.Exception != "" {
				checkException(t, sc, seed, got, err, store)
				return
			}

			if sc.ResultJSON.Throws != nil {
				if err == nil {
					t.Fatalf("no error, TS threw %q", *sc.ResultJSON.Throws)
				}
				if err.Error() != *sc.ResultJSON.Throws {
					t.Errorf("error text\n got: %q\nwant: %q", err.Error(), *sc.ResultJSON.Throws)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				// sourceTrust differs from the recording: see goReturnedRecords.
				if gotRecs, wantRecs := encode(t, got), goReturnedRecords(t, sc.ResultJSON.Records, seed); gotRecs != wantRecs {
					t.Errorf("returned records differ\n got: %s\nwant: %s", gotRecs, sc.ResultJSON.Records)
				}
			}
			if !sc.OmitStore {
				// supersededBy and sourceTrust differ from the recording: see goStoreJSON.
				if gotStore, wantStore := encode(t, storeRecords(t, store)), goStoreJSON(t, sc.ResultJSON.Store, seed); gotStore != wantStore {
					t.Errorf("store differs\n got: %s\nwant: %s", gotStore, sc.ResultJSON.Store)
				}
			}
		})
	}
}

// nonNilCalls makes a missing call list encode as [] like the fixture.
func nonNilCalls(calls [][]kernel.Message) [][]kernel.Message {
	if calls == nil {
		return [][]kernel.Message{}
	}
	return calls
}

// checkException asserts the documented Go behaviour of an exception scenario and, next
// to it, that the recorded fixture still shows the behaviour the exception names.
func checkException(t *testing.T, sc evalScenario, seed []memory.Record, got []memory.Record, err error, store persistence.MemoryStore) {
	t.Helper()
	field, known := evalExceptions[sc.Name]
	if !known {
		t.Fatalf("exception scenario %q has no documented Go behaviour in evalExceptions", sc.Name)
	}
	var tsRecords []map[string]any
	if sc.ResultJSON.Records != nil && string(sc.ResultJSON.Records) != "null" {
		if e := json.Unmarshal(sc.ResultJSON.Records, &tsRecords); e != nil {
			t.Fatalf("decode the TS records: %v", e)
		}
	}

	switch sc.Exception {
	case "field-not-a-string":
		// The fixture stores the value as it is; the pipeline fails the pass at that element.
		if len(tsRecords) != 1 {
			t.Fatalf("TS returned %d records, want 1", len(tsRecords))
		}
		if _, isString := tsRecords[0][field].(string); isString {
			t.Errorf("TS no longer stores a non-string %s: %v", field, tsRecords[0][field])
		}
		want := fmt.Sprintf("memory extraction field %q is not a string", field)
		if err == nil || err.Error() != want {
			t.Errorf("error = %v, want %q", err, want)
		}
		if got != nil {
			t.Errorf("a failed pass returned records: %v", got)
		}
		if after := storeRecords(t, store); encode(t, after) != encode(t, seed) {
			t.Errorf("the store changed although the element failed: %v", after)
		}
	case "null-or-missing-field":
		// The fixture stores null or omits the property; the pipeline stores the empty string.
		if len(tsRecords) != 1 {
			t.Fatalf("TS returned %d records, want 1", len(tsRecords))
		}
		if v, present := tsRecords[0][field]; present && v != nil {
			t.Errorf("TS now stores %s = %v, the exception expects null or absent", field, v)
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("returned %d records, want 1", len(got))
		}
		switch field {
		case "value":
			if got[0].Value != "" {
				t.Errorf("value = %q, want empty", got[0].Value)
			}
		case "subject":
			if got[0].Subject != "" {
				t.Errorf("subject = %q, want empty", got[0].Subject)
			}
		case "kind":
			if got[0].Kind != "" || got[0].Scope != "" || got[0].Key != "" || got[0].Value != "" {
				t.Errorf("an array element must store empty fields, got %+v", got[0])
			}
		}
		if len(storeRecords(t, store)) != len(seed)+1 {
			t.Errorf("the record was not stored")
		}
	case "json-syntax-error-text":
		// The fixture holds the V8 JSON.parse text; the pipeline reports its own, behind a fixed prefix.
		if sc.ResultJSON.Throws == nil || *sc.ResultJSON.Throws == "" {
			t.Errorf("TS no longer throws for this reply")
		}
		const prefix = "memory extraction returned invalid JSON"
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("error = %v, want it to start with %q", err, prefix)
		}
		if after := storeRecords(t, store); encode(t, after) != encode(t, seed) {
			t.Errorf("the store changed although the reply was rejected: %v", after)
		}
	default:
		t.Fatalf("unknown exception %q", sc.Exception)
	}
}

func TestPromptsMatchGolden(t *testing.T) {
	var f struct {
		ExtractionSystemPrompt string `json:"extractionSystemPrompt"`
		Recall                 []struct {
			Name   string          `json:"name"`
			Seed   []memory.Record `json:"seed"`
			Result struct {
				Content   *string  `json:"content"`
				RecordIDs []string `json:"recordIds"`
			} `json:"result"`
		} `json:"recall"`
	}
	loadFixture(t, "memorysystem-prompts", &f)

	if extractionSystemPrompt != f.ExtractionSystemPrompt {
		t.Errorf("the extraction system prompt differs from the TS runtime string\n got: %q\nwant: %q", extractionSystemPrompt, f.ExtractionSystemPrompt)
	}

	formatted := 0
	for _, rc := range f.Recall {
		t.Run(rc.Name, func(t *testing.T) {
			if rc.Result.Content == nil {
				if len(rc.Result.RecordIDs) != 0 {
					t.Errorf("TS recalled %v but returned no content", rc.Result.RecordIDs)
				}
				return
			}
			formatted++
			var results []memory.RetrievalResult
			for _, id := range rc.Result.RecordIDs {
				i := slices.IndexFunc(rc.Seed, func(r memory.Record) bool { return r.ID == id })
				if i < 0 {
					t.Fatalf("recalled record %q is not in the seed", id)
				}
				results = append(results, memory.RetrievalResult{Record: rc.Seed[i]})
			}
			if got := formatRecalledMemories(results); got != *rc.Result.Content {
				t.Errorf("recall block differs\n got: %q\nwant: %q", got, *rc.Result.Content)
			}
		})
	}
	if formatted < 2 {
		t.Fatalf("only %d recall scenarios carry content, want at least 2", formatted)
	}
}

// ---------------------------------------------------------------------------
// The three memory-key-catalog cases, over the narrow Model port: the memory
// component takes the one-method Model, so a model that only has chat is enough.
// ---------------------------------------------------------------------------

const catalogAgent = "agent-1"

// catalogRecord builds a test record.
func catalogRecord(id, scope, key string, mutate func(*memory.Record)) memory.Record {
	r := memory.Record{
		ID: id, AgentID: catalogAgent, Kind: memory.KindUserPreference, Scope: memory.Scope(scope), Subject: "user",
		Key: key, Value: "Reply in Vietnamese", Status: memory.StatusActive, Confidence: memory.ConfidenceExplicit,
		Source: "conversation", SourceTrust: memory.TrustUserApproved, ObservedAt: "2023-11-01T00:00:00.000Z",
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func TestKeyCatalog_GroupsDistinctActiveKeysByScope(t *testing.T) {
	store := seedStore(t,
		catalogRecord("1", "user", "response_language", nil),
		catalogRecord("2", "user", "unit_system", nil),
		catalogRecord("3", "project", "deploy_process", nil),
		catalogRecord("4", "user", "old_topic", func(r *memory.Record) { r.Status = memory.StatusSuperseded }),
		catalogRecord("5", "user", "foreign_key", func(r *memory.Record) { r.AgentID = "other-agent" }),
	)
	model := newFakeModel(reply{Content: "[]"})
	mem := newMemory(newTestDeps(catalogAgent), model, store)

	conversation := []kernel.Message{{Role: kernel.RoleUser, Content: "Please answer in English from now on"}}
	if _, err := mem.evaluateMessages(t.Context(), conversation); err != nil {
		t.Fatal(err)
	}

	calls := model.extractionCalls()
	if len(calls) != 1 {
		t.Fatalf("%d extraction calls, want 1", len(calls))
	}
	userContent := calls[0][1].Content
	for _, want := range []string{
		"EXISTING KEYS:\nuser: response_language, unit_system\nproject: deploy_process",
		"CONVERSATION:\n[user]: Please answer in English from now on",
	} {
		if !strings.Contains(userContent, want) {
			t.Errorf("user content lacks %q:\n%s", want, userContent)
		}
	}
	for _, unwanted := range []string{"old_topic", "foreign_key", "Reply in Vietnamese"} {
		if strings.Contains(userContent, unwanted) {
			t.Errorf("user content must not contain %q:\n%s", unwanted, userContent)
		}
	}
}

func TestKeyCatalog_OnlyConversationWhenNoKeys(t *testing.T) {
	model := newFakeModel(reply{Content: "[]"})
	mem := newMemory(newTestDeps(catalogAgent), model, seedStore(t))

	if _, err := mem.evaluateMessages(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "Please answer in English from now on"}}); err != nil {
		t.Fatal(err)
	}

	calls := model.extractionCalls()
	if len(calls) != 1 || calls[0][1].Content != "[user]: Please answer in English from now on" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestKeyCatalog_NormalizesReturnedKey(t *testing.T) {
	existing := catalogRecord("existing", "user", "response_language", nil)
	store := seedStore(t, existing)
	model := newFakeModel(reply{Content: encode(t, []map[string]string{{
		"kind": "user_preference", "scope": "user", "subject": "user", "key": "Response-Language ",
		"value": "Reply in English", "confidence": "explicit",
	}})})
	mem := newMemory(newTestDeps(catalogAgent), model, store)

	created, err := mem.evaluateMessages(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "Please answer in English from now on"}})
	if err != nil {
		t.Fatal(err)
	}

	if len(created) != 1 || created[0].Key != "response_language" {
		t.Fatalf("created = %+v", created)
	}
	old, err := store.Get(t.Context(), existing.ID)
	if err != nil || old == nil || old.Status != memory.StatusSuperseded {
		t.Fatalf("existing record = %+v, %v; want superseded", old, err)
	}
}

// ---------------------------------------------------------------------------
// Unit tests of the helpers the corpus exercises as a whole.
// ---------------------------------------------------------------------------

func TestMemory_AcceptsFencedJSON(t *testing.T) {
	body := encode(t, []map[string]string{{
		"kind": "fact", "scope": "project", "subject": "site", "key": "Site Name", "value": "Tower A", "confidence": "verified",
	}})
	for name, content := range map[string]string{
		"json fence":  "```json\n" + body + "\n```",
		"plain fence": "```\n" + body + "\n```",
		"upper case":  "  ```JSON  \n" + body + "\n```  \n",
		"no newlines": "```json" + body + "```",
		"no fence":    body,
	} {
		t.Run(name, func(t *testing.T) {
			store := seedStore(t)
			mem := newMemory(newTestDeps("a1", WithClock(fixedClock(1700000000000)), WithIDs(tsIDs(1700000000000))), newFakeModel(reply{Content: content}), store)
			got, err := mem.evaluateMessages(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}})
			if err != nil || len(got) != 1 || got[0].Key != "site_name" || got[0].Value != "Tower A" {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestNormalizeKey(t *testing.T) {
	for in, want := range map[string]string{
		"Response Language":      "response_language",
		"response-language":      "response_language",
		"  Spaces  Here ":        "spaces_here",
		"a__b":                   "a_b",
		"a_ -b":                  "a_b",
		"_x_":                    "x",
		"_":                      "",
		"--__ -":                 "",
		"":                       "",
		"UPPER_Case":             "upper_case",
		"x\u00a0nbsp\ufeffbom":   "x_nbsp_bom",
		"\u0085edge\u0085":       "\u0085edge\u0085", // U+0085 is not JS whitespace
		"tab\tand\nnewline":      "tab_and_newline",
		"\u00c9t\u00e9 Key":      "\u00e9t\u00e9_key",
		"keep.dots and-dashes ":  "keep.dots_and_dashes",
		"\ufeff  lead and trail": "lead_and_trail",
	} {
		if got := normalizeKey(in); got != want {
			t.Errorf("normalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripFences(t *testing.T) {
	for in, want := range map[string]string{
		"```json\n[1]\n```":      "[1]",
		"```JSON [1]```":         "[1]",
		"``` [1] ```":            "[1]",
		"```json```":             "",
		"```":                    "",
		"[1]":                    "[1]",
		"  [1]  ":                "[1]",
		"```jsonx\n[1]```":       "x\n[1]",
		"```j\u017fon\n[1]\n```": "j\u017fon\n[1]", // U+017F does not fold to s in a JS regexp
		"[1]```":                 "[1]",
		"````":                   "`",
	} {
		if got := stripFences(in); got != want {
			t.Errorf("stripFences(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareUTF16(t *testing.T) {
	astral := "a\U0001F600"
	fullwidth := "a\uFF21"
	if strings.Compare(fullwidth, astral) >= 0 {
		t.Fatal("the byte order of these two strings changed; the test needs new examples")
	}
	if compareUTF16(astral, fullwidth) >= 0 {
		t.Errorf("UTF-16 order must put %q before %q", astral, fullwidth)
	}
	if compareUTF16("a", "a") != 0 || compareUTF16("a", "ab") >= 0 || compareUTF16("b", "ab") <= 0 {
		t.Error("basic ordering is wrong")
	}
}

func TestIsoFromMs(t *testing.T) {
	for _, tc := range []struct {
		ms   float64
		want string
		ok   bool
	}{
		{1700000000000, "2023-11-14T22:13:20.000Z", true},
		{0, "1970-01-01T00:00:00.000Z", true},
		{1700000000000.9, "2023-11-14T22:13:20.000Z", true},
		{-1, "1969-12-31T23:59:59.999Z", true},
		{8.64e15, "+275760-09-13T00:00:00.000Z", true},
		{-8.64e15, "-271821-04-20T00:00:00.000Z", true},
		{8.64e15 + 1, "", false},
	} {
		got, ok := isoFromMs(tc.ms)
		if got != tc.want || ok != tc.ok {
			t.Errorf("isoFromMs(%v) = %q, %v; want %q, %v", tc.ms, got, ok, tc.want, tc.ok)
		}
	}
}

func TestJSNumberString(t *testing.T) {
	for in, want := range map[string]string{
		"7": "7", "1.5": "1.5", "-0": "0", "1e21": "1e+21", "1e-7": "1e-7", "100": "100",
		"1e400": "Infinity", "-1e400": "-Infinity", "0.000001": "0.000001", "123456789012345680000": "123456789012345680000",
	} {
		if got := jsToString(json.Number(in)); got != want {
			t.Errorf("String(%s) = %q, want %q", in, got, want)
		}
	}
}

// defaultIDPattern is the default record id: mem_<ms>_<n> and a tag of 16 lowercase
// hex digits, minted once per process (10-SECURITY UF-5).
var defaultIDPattern = regexp.MustCompile(`^mem_[0-9]+_[0-9]+_[0-9a-f]{16}$`)

func TestDefaultIDsFormat(t *testing.T) {
	next := defaultIDs(fixedClock(1700000000000))
	a, b := next(), next()
	for _, id := range []string{a, b} {
		if !defaultIDPattern.MatchString(id) {
			t.Fatalf("id %q does not match mem_<ms>_<n>_<16 hex>", id)
		}
	}
	var msA, nA, msB, nB int64
	if _, err := fmt.Sscanf(a, "mem_%d_%d", &msA, &nA); err != nil || msA != 1700000000000 {
		t.Fatalf("id %q does not match mem_<ms>_<n>", a)
	}
	if _, err := fmt.Sscanf(b, "mem_%d_%d", &msB, &nB); err != nil || nB != nA+1 {
		t.Fatalf("ids %q and %q: the counter must increase by one", a, b)
	}
	if a == b {
		t.Fatal("two ids are equal")
	}
	if tagOf(a) != tagOf(b) {
		t.Fatalf("ids %q and %q of one process carry different tags", a, b)
	}
}

// tagOf is the process tag of a default id.
func tagOf(id string) string { return id[strings.LastIndex(id, "_")+1:] }

// TestDefaultIDsDistinctAcrossProcesses simulates two processes that share a store:
// the same clock, each with its own fresh counter, differing only in the process tag.
// They never mint the same id, while the same tag with the same counter state does
// reproduce the ids, so the tag is what separates the processes (10-SECURITY UF-5).
func TestDefaultIDsDistinctAcrossProcesses(t *testing.T) {
	clock := fixedClock(1700000000000)
	var c1, c2, c3 atomic.Int64
	p1 := newDefaultIDs(clock, "00000000000000aa", &c1)
	p2 := newDefaultIDs(clock, "00000000000000bb", &c2)
	same := newDefaultIDs(clock, "00000000000000aa", &c3)

	seen := map[string]string{}
	for i := range 1000 {
		a, b, c := p1(), p2(), same()
		if a == b {
			t.Fatalf("draw %d: two processes minted the same id %q", i, a)
		}
		if a != c {
			t.Fatalf("draw %d: the same tag and counter gave %q and %q, want equal ids", i, a, c)
		}
		for who, id := range map[string]string{"p1": a, "p2": b} {
			if prev, dup := seen[id]; dup {
				t.Fatalf("draw %d: %s minted %q, already minted by %s", i, who, id, prev)
			}
			seen[id] = who
		}
	}
	if len(seen) != 2000 {
		t.Fatalf("%d distinct ids, want 2000", len(seen))
	}

	// The process tag of the package default is 16 lowercase hex digits.
	if !defaultIDPattern.MatchString(defaultIDs(clock)()) {
		t.Fatal("the package default id does not match mem_<ms>_<n>_<16 hex>")
	}
}

// TestExtractionTrustFromConfidence: an extracted record's source trust follows its
// confidence (explicit and verified are verified_internal, anything else
// model_speculation), and extraction never writes user_approved.
func TestExtractionTrustFromConfidence(t *testing.T) {
	const content = `[
	 {"kind":"fact","scope":"user","subject":"u","key":"a","value":"1","confidence":"explicit"},
	 {"kind":"fact","scope":"user","subject":"u","key":"b","value":"2","confidence":"verified"},
	 {"kind":"fact","scope":"user","subject":"u","key":"c","value":"3","confidence":"inferred"},
	 {"kind":"fact","scope":"user","subject":"u","key":"d","value":"4","confidence":"unverified"},
	 {"kind":"fact","scope":"user","subject":"u","key":"e","value":"5"}
	]`
	store := seedStore(t)
	mem := newMemory(newTestDeps("agent-1", WithClock(fixedClock(1_700_000_000_000)), WithIDs(tsIDs(1_700_000_000_000))), newFakeModel(reply{Content: content}), store)
	got, err := mem.evaluateMessages(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "facts"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]memory.SourceTrust{
		"a": memory.TrustVerifiedInternal, "b": memory.TrustVerifiedInternal,
		"c": memory.TrustModelSpeculation, "d": memory.TrustModelSpeculation, "e": memory.TrustModelSpeculation,
	}
	if len(got) != len(want) {
		t.Fatalf("%d records, want %d", len(got), len(want))
	}
	for _, r := range storeRecords(t, store) {
		if r.SourceTrust == memory.TrustUserApproved || r.SourceTrust != want[r.Key] {
			t.Errorf("key %q (confidence %q): trust %q, want %q", r.Key, r.Confidence, r.SourceTrust, want[r.Key])
		}
	}
}

// TestSupersedeSetsSupersededBy: the superseded record points at the record that
// replaces it.
func TestSupersedeSetsSupersededBy(t *testing.T) {
	old := memory.Record{
		ID: "old", AgentID: "agent-1", Kind: memory.KindUserPreference, Scope: memory.ScopeUser, Subject: "user",
		Key: "response_language", Value: "Reply in Vietnamese", Status: memory.StatusActive, Confidence: memory.ConfidenceExplicit,
		Source: "conversation", SourceTrust: memory.TrustUserApproved, ObservedAt: "2023-11-01T00:00:00.000Z",
	}
	store := seedStore(t, old)
	const content = `[{"kind":"user_preference","scope":"user","subject":"user","key":"response_language","value":"Reply in English","confidence":"explicit"}]`
	mem := newMemory(newTestDeps("agent-1", WithClock(fixedClock(1_700_000_000_000)), WithIDs(tsIDs(1_700_000_000_000))), newFakeModel(reply{Content: content}), store)
	got, err := mem.evaluateMessages(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "english please"}})
	if err != nil || len(got) != 1 {
		t.Fatalf("evaluateMessages = %v, %v", got, err)
	}
	stored, err := store.Get(t.Context(), "old")
	if err != nil || stored == nil {
		t.Fatalf("Get(old) = %v, %v", stored, err)
	}
	if stored.Status != memory.StatusSuperseded || stored.SupersededBy != got[0].ID {
		t.Fatalf("old record: status %q supersededBy %q, want superseded by %q", stored.Status, stored.SupersededBy, got[0].ID)
	}
}

// Regression tests of the phase 10 code review warnings WR-B02 and WR-B03.

// --- WR-B02: model text stays out of logs, and is bounded in the error -------------------

func TestEvaluate_ModelTextIsBoundedInTheErrorAndNotLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		lc := &logCapture{}
		secret := "SECRET-HOME-ADDRESS-" + strings.Repeat("x", 500)
		model := newFakeModel(reply{Content: encode(t, []string{secret})})
		sys := w.system("a1", memCfg(1), model, WithLogger(lc.logger()))
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute) // the pass fails on the string element and logs it
		out := lc.String()
		if !strings.Contains(out, "consolidation failed") {
			t.Fatalf("the failing pass was not logged: %q", out)
		}
		if strings.Contains(out, "SECRET-HOME-ADDRESS") {
			t.Fatalf("the log carries model text: %q", out)
		}

		// The returned error keeps its wording but is bounded.
		_, err := sys.Consolidate(t.Context(), s1)
		if err == nil {
			t.Fatal("Consolidate = nil, want the element error")
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "Cannot create property 'key' on string '") || !strings.HasSuffix(msg, "...'") {
			t.Fatalf("error = %q, want the TS text with a truncated element", msg)
		}
		if strings.Contains(msg, strings.Repeat("x", 100)) || len(msg) > 120 {
			t.Fatalf("error of %d bytes embeds too much model text: %q", len(msg), msg)
		}
		dispose(t, sys)
	})
}

// A short string element keeps the exact error text (the recorded corpus case).
func TestEvaluate_ShortElementErrorIsUnchanged(t *testing.T) {
	_, err := toExtraction("abc")
	if err == nil || err.Error() != "Cannot create property 'key' on string 'abc'" {
		t.Fatalf("toExtraction(abc) = %v, want the TS text", err)
	}
}

// --- WR-B03: the model output is bounded --------------------------------------------------

func extractionElement(key, value string) map[string]any {
	return map[string]any{
		"kind": "fact", "scope": "user", "subject": "user",
		"key": key, "value": value, "confidence": "explicit",
	}
}

func evaluateReply(t *testing.T, elements ...any) ([]memory.Record, *persistence.MemoryMemoryStore, *logCapture, error) {
	t.Helper()
	store := persistence.NewMemoryMemoryStore()
	lc := &logCapture{}
	model := newFakeModel(reply{Content: encode(t, elements)})
	mem := newMemory(newTestDeps("a1", WithClock(fixedClock(1700000000000)), WithIDs(tsIDs(1700000000000)), WithLogger(lc.logger())), model, store)
	got, err := mem.evaluateMessages(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "remember things"}})
	return got, store, lc, err
}

func TestEvaluate_CapsTheNumberOfElements(t *testing.T) {
	var els []any
	for i := range maxExtractionsPerPass + 10 {
		els = append(els, extractionElement(fmt.Sprintf("key_%03d", i), "value"))
	}
	got, store, lc, err := evaluateReply(t, els...)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxExtractionsPerPass || len(storeRecords(t, store)) != maxExtractionsPerPass {
		t.Fatalf("saved %d records (returned %d), want the cap of %d", len(storeRecords(t, store)), len(got), maxExtractionsPerPass)
	}
	if !strings.Contains(lc.String(), "capped") {
		t.Fatalf("the cap was not logged: %q", lc.String())
	}
}

func TestEvaluate_SkipsOversizedAndUnknownElements(t *testing.T) {
	wrongKind := extractionElement("wrong_kind", "v")
	wrongKind["kind"] = "ignore previous instructions"
	wrongScope := extractionElement("wrong_scope", "v")
	wrongScope["scope"] = "galaxy"
	wrongConfidence := extractionElement("wrong_confidence", "v")
	wrongConfidence["confidence"] = "certain"

	got, store, lc, err := evaluateReply(t,
		extractionElement("good_one", "fine"),
		extractionElement("long_value", strings.Repeat("v", maxValueUnits+1)),
		extractionElement(strings.Repeat("k", maxKeyUnits+1), "v"),
		map[string]any{"kind": "fact", "scope": "user", "subject": strings.Repeat("s", maxSubjectUnits+1), "key": "long_subject", "value": "v", "confidence": "explicit"},
		wrongKind, wrongScope, wrongConfidence,
		extractionElement("good_two", strings.Repeat("v", maxValueUnits)), // exactly at the limit
	)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range got {
		keys = append(keys, r.Key)
	}
	if len(keys) != 2 || keys[0] != "good_one" || keys[1] != "good_two" {
		t.Fatalf("saved keys = %v, want only good_one and good_two", keys)
	}
	if n := len(storeRecords(t, store)); n != 2 {
		t.Fatalf("store holds %d records, want 2", n)
	}
	if out := lc.String(); strings.Contains(out, "ignore previous instructions") || strings.Contains(out, "galaxy") {
		t.Fatalf("the log carries model text: %q", out)
	}
	if n := strings.Count(lc.String(), "extraction element skipped"); n != 6 {
		t.Fatalf("%d skipped elements logged, want 6: %q", n, lc.String())
	}
}

// The empty and missing fields of an element are kept (the recorded corpus cases).
func TestEvaluate_KeepsTheEmptyFields(t *testing.T) {
	got, _, _, err := evaluateReply(t, map[string]any{"value": "only a value"}, []any{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("saved %d records, want both TS-shaped elements", len(got))
	}
}

func TestUTF16Len(t *testing.T) {
	for s, want := range map[string]int{"": 0, "abc": 3, "\u00e9": 1, "\U0001F600": 2, "a\U0001F600b": 4} {
		if got := utf16Len(s); got != want {
			t.Errorf("utf16Len(%q) = %d, want %d", s, got, want)
		}
	}
}
