package memorysystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Memory - LLM quality gate before durable storage (the memory extraction engine)
//
// Given recent conversation messages, the model decides what facts, preferences or
// decisions are worth remembering and extracts clean typed records. The extraction
// prompt, the key catalog, the key normalisation and the reply parsing are pinned
// byte for byte by the corpora in tests/testdata/golden/memorysystem-*.json.
// ---------------------------------------------------------------------------

// The extraction system prompt (TestPromptsMatchGolden proves it against the recorded
// value). The two dashes are written as escapes so that this source stays ASCII.
const (
	emDash = "\u2014"
	enDash = "\u2013"

	extractionSystemPrompt = `You are a memory extraction engine for an AI agent.
You receive a batch of recent conversation messages between a user and an assistant.

Your job:
1. Identify facts, preferences, decisions, or procedures worth storing as long-term memories.
2. Extract each as a clean, structured memory record.

Rules:
- Tool telemetry ("Tool X completed", "Tool X called") is NOT worth remembering unless it reveals a reusable fact about the environment.
- User preferences, corrections, decisions, and recurring facts ARE worth remembering.
- Write each memory value as a clean, concise statement ` + emDash + ` not the raw message text.
- Detect the language of the input. Write memory values in the SAME language as the user's input.
- For technical terms, keep them in English regardless of the surrounding language.
- If no messages are worth remembering, return an empty array.

Keys:
- You may receive a list of EXISTING KEYS grouped by scope. If a memory is about the same topic as an existing key, reuse that exact key ` + emDash + ` even if the value changed.
- Only create a new key when no existing key covers the topic.
- A key names the topic, not the value: lowercase snake_case, 1` + enDash + `4 words (e.g. unit_system, not uses_metric).

Respond with ONLY a JSON array (no markdown fences):
[
  {
    "kind": "user_preference" | "fact" | "decision" | "procedure" | "task_state" | "source_reference",
    "scope": "user" | "project" | "profile" | "organization" | "task",
    "subject": "who or what this is about",
    "key": "short unique identifier for dedup (e.g. response_language, deploy_process)",
    "value": "clean memory statement",
    "confidence": "explicit" | "verified" | "inferred" | "unverified"
  }
]`
)

// maxExistingKeys caps the existing keys sent to the extraction model: it keeps the
// prompt small as memories accumulate (keys only, no values).
const maxExistingKeys = 200

// The limits on what one reply of the model may write to the durable store. The model
// output is untrusted, so a runaway or prompt-injected reply must not flood the store
// or hold the write mutex for long. Lengths are in UTF-16 code units, as the JS string
// length is. A real memory is one clean, concise statement, so none of these is reached
// by honest output, and none by a recorded scenario.
const (
	// maxExtractionsPerPass is the number of elements of one reply that are processed;
	// the rest is dropped.
	maxExtractionsPerPass = 50
	// maxKeyUnits, maxSubjectUnits and maxValueUnits are the longest key (as the model
	// wrote it, before normalisation), subject and value; an element that exceeds one
	// is skipped.
	maxKeyUnits     = 256
	maxSubjectUnits = 256
	maxValueUnits   = 4096
	// maxErrorElementUnits bounds the model text that an error message embeds.
	maxErrorElementUnits = 64
)

// memoryComponent is the extraction component of one agent.
type memoryComponent struct {
	deps
	model Model
	store persistence.MemoryStore
}

// newMemory returns the extraction component of one agent.
func newMemory(d deps, model Model, store persistence.MemoryStore) *memoryComponent {
	return &memoryComponent{deps: d, model: model, store: store}
}

// jsTypeError carries the text of a V8 TypeError. The text starts with a capital
// letter, which staticcheck ST1005 forbids in errors.New, hence a type.
type jsTypeError struct {
	msg string
	// logMsg, when set, is what a log line carries instead of msg: the text that the
	// model produced stays out of the application log (the package logging contract:
	// ids only, never a memory value or message text). The returned error keeps msg.
	logMsg string
}

func (e *jsTypeError) Error() string { return e.msg }

// LogValue implements slog.LogValuer.
func (e *jsTypeError) LogValue() slog.Value {
	if e.logMsg != "" {
		return slog.StringValue(e.logMsg)
	}
	return slog.StringValue(e.msg)
}

// redactForLog is the value to log for an error: an error that embeds model output is
// replaced by its withheld form.
func redactForLog(err error) any {
	var te *jsTypeError
	if errors.As(err, &te) && te.logMsg != "" {
		return te.logMsg
	}
	return err
}

// extraction is one element of the model's reply. A field that the element does not
// carry as a string is empty here (see the exceptions documented on toExtraction).
type extraction struct {
	kind, scope, subject, key, value, confidence string
}

// evaluateMessages asks the model which memories the messages hold and persists the
// worthy ones. It returns the created and reinforced records in reply order. An error
// from the model, a truncated or unparsable reply, or an element that fails stops the
// pass: the elements before it stay saved and the caller (Dreaming) does not advance
// its checkpoint, so the next pass retries.
//
// The write section runs under the agent's write mutex, taken once per element so a
// recall or an expiry pass can interleave between two elements; the model call does not.
//
// The reply is bounded (WR-B03): only the first maxExtractionsPerPass elements are
// processed, and an element whose key, subject or value is too long, or whose kind,
// scope or confidence is set to something that is not a member of the memory enums, is
// skipped and logged by its position. The empty and missing fields are still stored
// (see toExtraction).
func (m *memoryComponent) evaluateMessages(ctx context.Context, msgs []kernel.Message) ([]memory.Record, error) {
	results := []memory.Record{}
	if len(msgs) == 0 {
		return results, nil
	}

	extractions, err := m.extractFromMessages(ctx, msgs)
	if err != nil {
		return nil, err
	}
	if len(extractions) == 0 {
		return results, nil
	}

	now := isoNow(m.opts.now)

	if len(extractions) > maxExtractionsPerPass {
		m.opts.logger.Warn("memory: extraction reply capped", "agent", m.agentID, "elements", len(extractions), "kept", maxExtractionsPerPass)
		extractions = extractions[:maxExtractionsPerPass]
	}
	for i, el := range extractions {
		ex, err := toExtraction(el)
		if err != nil {
			return nil, err
		}
		if reason := validateExtraction(ex); reason != "" {
			m.opts.logger.Warn("memory: extraction element skipped", "agent", m.agentID, "index", i, "reason", reason)
			continue
		}
		ex.key = normalizeKey(ex.key)
		rec, err := m.persistExtraction(ctx, ex, now)
		if err != nil {
			return nil, err
		}
		results = append(results, rec)
	}
	return results, nil
}

// persistExtraction stores one extracted memory under the write mutex: it reinforces
// or supersedes the agent's record of the same key and scope, or creates a new one.
func (m *memoryComponent) persistExtraction(ctx context.Context, ex extraction, now string) (memory.Record, error) {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	// Check for an existing memory with the same key and scope, scoped to this
	// agent: never reinforce or supersede another agent's memory.
	existing, err := m.store.FindByKey(ctx, m.agentID, memory.Scope(ex.scope), ex.key)
	if err != nil {
		return memory.Record{}, err
	}
	if existing != nil {
		return m.handleDuplicate(ctx, *existing, ex, now)
	}
	return m.createRecord(ctx, ex, now)
}

// validateExtraction is the bound on one extracted element (see evaluateMessages). It
// returns why the element is refused, "" when it is fine. The reason never contains
// model text.
func validateExtraction(ex extraction) string {
	switch {
	case utf16Len(ex.key) > maxKeyUnits:
		return "key too long"
	case utf16Len(ex.subject) > maxSubjectUnits:
		return "subject too long"
	case utf16Len(ex.value) > maxValueUnits:
		return "value too long"
	case ex.kind != "" && !isMemoryKind(memory.Kind(ex.kind)):
		return "unknown kind"
	case ex.scope != "" && !isMemoryScope(memory.Scope(ex.scope)):
		return "unknown scope"
	case ex.confidence != "" && !isMemoryConfidence(memory.Confidence(ex.confidence)):
		return "unknown confidence"
	}
	return ""
}

// utf16Len is the JS length of s: the number of UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}

func isMemoryKind(k memory.Kind) bool {
	switch k {
	case memory.KindUserPreference, memory.KindFact, memory.KindDecision, memory.KindProcedure,
		memory.KindTaskState, memory.KindSourceReference:
		return true
	}
	return false
}

func isMemoryScope(sc memory.Scope) bool {
	switch sc {
	case memory.ScopeUser, memory.ScopeProject, memory.ScopeProfile, memory.ScopeOrganization, memory.ScopeTask:
		return true
	}
	return false
}

func isMemoryConfidence(c memory.Confidence) bool {
	switch c {
	case memory.ConfidenceExplicit, memory.ConfidenceVerified, memory.ConfidenceInferred, memory.ConfidenceUnverified:
		return true
	}
	return false
}

// extractFromMessages batches the conversation into one model call and parses the reply
// into the raw elements of the JSON array.
func (m *memoryComponent) extractFromMessages(ctx context.Context, msgs []kernel.Message) ([]any, error) {
	// Only user and assistant messages carry useful context.
	var lines []string
	for _, msg := range msgs {
		if msg.Role == kernel.RoleUser || msg.Role == kernel.RoleAssistant {
			lines = append(lines, "["+string(msg.Role)+"]: "+msg.Content)
		}
	}
	conversationLines := strings.Join(lines, "\n")
	if jsstr.Trim(conversationLines) == "" {
		return nil, nil
	}

	// Existing keys let the model reuse a key instead of inventing a near-duplicate.
	existingKeys, err := m.existingKeyCatalog(ctx)
	if err != nil {
		return nil, err
	}
	userContent := conversationLines
	if existingKeys != "" {
		userContent = "EXISTING KEYS:\n" + existingKeys + "\n\nCONVERSATION:\n" + conversationLines
	}

	// Errors propagate on purpose: returning nothing would look like "nothing worth
	// remembering" and Dreaming would advance the session checkpoint past these
	// messages for good. Failing keeps the checkpoint, so the next tick retries.
	resp, err := m.model.Chat(ctx, []kernel.Message{
		{Role: kernel.RoleSystem, Content: extractionSystemPrompt},
		{Role: kernel.RoleUser, Content: userContent},
	}, llmhub.ModelOptions{})
	if err != nil {
		return nil, err
	}
	if resp.StopReason == kernel.StopMaxTokens {
		return nil, fmt.Errorf("memory extraction truncated (output tokens: %d)", resp.Usage.OutputTokens)
	}
	return parseExtractions(resp.Message.Content)
}

// existingKeyCatalog is the distinct active keys of the agent grouped by scope, one
// line each, scopes in first-seen order and keys sorted (JS default sort, UTF-16 code
// unit order), e.g. "user: response_language, unit_system".
func (m *memoryComponent) existingKeyCatalog(ctx context.Context) (string, error) {
	active, err := m.store.Query(ctx, persistence.MemoryFilter{
		AgentID:  m.agentID,
		Statuses: []memory.Status{memory.StatusActive},
		Limit:    maxExistingKeys,
	})
	if err != nil {
		return "", err
	}

	var scopes []memory.Scope
	byScope := map[memory.Scope]map[string]struct{}{}
	for _, rec := range active {
		keys, ok := byScope[rec.Scope]
		if !ok {
			keys = map[string]struct{}{}
			byScope[rec.Scope] = keys
			scopes = append(scopes, rec.Scope)
		}
		keys[rec.Key] = struct{}{}
	}

	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		keys := make([]string, 0, len(byScope[scope]))
		for k := range byScope[scope] {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareUTF16)
		out = append(out, string(scope)+": "+strings.Join(keys, ", "))
	}
	return strings.Join(out, "\n"), nil
}

// compareUTF16 orders strings by UTF-16 code units, which is what the JS default
// sort does. It differs from the byte order of UTF-8 for a code point at or above
// U+10000 against one between U+E000 and U+FFFF.
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

// handleDuplicate merges or supersedes an existing record of the same key.
//
// When it supersedes, it builds the new record first and stores its id in the old
// record's SupersededBy, so the chain can be followed. The id is minted before the
// old record is updated; the store writes update the old one, then save the new one.
func (m *memoryComponent) handleDuplicate(ctx context.Context, existing memory.Record, ex extraction, timestamp string) (memory.Record, error) {
	// Same value: reinforce.
	if existing.Value == ex.value {
		existing.UpdatedAt = timestamp
		if err := m.store.Update(ctx, existing); err != nil {
			return memory.Record{}, err
		}
		return existing, nil
	}

	// Different value for the same key: supersede.
	rec := m.newRecord(ex, timestamp)
	existing.Status = memory.StatusSuperseded
	existing.SupersededBy = rec.ID
	if err := m.store.Update(ctx, existing); err != nil {
		return memory.Record{}, err
	}
	if err := m.store.Save(ctx, rec); err != nil {
		return memory.Record{}, err
	}
	return rec, nil
}

// trustFromConfidence is the source trust of an extracted memory. The extraction input
// includes assistant text, so a model could write memories that recall would treat as
// trusted if every extracted memory were user_approved, the highest trust level. The
// trust is therefore derived from the confidence the extraction reported: a fact the
// user stated (explicit) or that was checked (verified) is verified_internal; an
// inferred, unverified, empty or unknown confidence is model_speculation. Extraction
// never yields user_approved: that level is for a memory a human approved.
func trustFromConfidence(c memory.Confidence) memory.SourceTrust {
	switch c {
	case memory.ConfidenceExplicit, memory.ConfidenceVerified:
		return memory.TrustVerifiedInternal
	default:
		return memory.TrustModelSpeculation
	}
}

// createRecord stores a new durable record.
//
// The source trust is derived from the extracted confidence (see
// trustFromConfidence). The recall block still tells the model that memories can be
// outdated.
func (m *memoryComponent) createRecord(ctx context.Context, ex extraction, timestamp string) (memory.Record, error) {
	rec := m.newRecord(ex, timestamp)
	if err := m.store.Save(ctx, rec); err != nil {
		return memory.Record{}, err
	}
	return rec, nil
}

// newRecord builds the record of an extraction with a fresh id; nothing is stored.
func (m *memoryComponent) newRecord(ex extraction, timestamp string) memory.Record {
	return memory.Record{
		ID:          m.opts.nextID(),
		AgentID:     m.agentID,
		Kind:        memory.Kind(ex.kind),
		Scope:       memory.Scope(ex.scope),
		Subject:     ex.subject,
		Key:         ex.key,
		Value:       ex.value,
		Status:      memory.StatusActive,
		Confidence:  memory.Confidence(ex.confidence),
		Source:      "conversation",
		SourceTrust: trustFromConfidence(memory.Confidence(ex.confidence)),
		ObservedAt:  timestamp,
		UpdatedAt:   timestamp,
	}
}

// ---------------------------------------------------------------------------
// Key normalisation - "Response Language" / "response-language" -> "response_language"
// ---------------------------------------------------------------------------

// normalizeKey normalises a key that is already a string:
// trim().toLowerCase(), every run of whitespace and dashes becomes one underscore,
// runs of underscores collapse, and one leading and one trailing underscore are
// dropped. A run of whitespace, dashes and underscores is one underscore either way,
// so a single pass does what the three replaces do.
func normalizeKey(key string) string {
	s := jsstr.ToLower(jsstr.Trim(key))
	var b strings.Builder
	b.Grow(len(s))
	inRun := false
	for _, r := range s {
		if jsstr.IsSpace(r) || r == '-' || r == '_' {
			if !inRun {
				b.WriteByte('_')
				inRun = true
			}
			continue
		}
		inRun = false
		b.WriteRune(r)
	}
	out := strings.TrimPrefix(b.String(), "_")
	return strings.TrimSuffix(out, "_")
}

// ---------------------------------------------------------------------------
// Parse the model's JSON array - tolerant of markdown fences, strict otherwise
// ---------------------------------------------------------------------------

// parseExtractions strips one leading code fence (with an optional json tag, any
// case) and one trailing fence, and decodes a JSON array. Empty text and anything
// that is not an array are errors, so the pass fails and the checkpoint stays. The
// elements are returned undecoded: each one is handled as it is reached, so an
// element that cannot be handled fails the pass only after the earlier ones were
// saved.
//
// Exception J1 (json-syntax-error-text): a malformed reply is reported with an
// error that starts with "memory extraction returned invalid JSON" and carries the
// Go decoder's message. It fails the pass at that point, so the checkpoint and the
// store agree.
func parseExtractions(content string) ([]any, error) {
	text := stripFences(content)
	if text == "" {
		return nil, errors.New("memory extraction returned empty content")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("memory extraction returned invalid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("memory extraction returned invalid JSON: unexpected data after the value")
	}
	arr, ok := parsed.([]any)
	if !ok {
		return nil, errors.New("memory extraction did not return a JSON array")
	}
	return arr, nil
}

// stripFences is content.trim().replace(/^```(?:json)?\s*/i, "").replace(/\s*```$/,
// "").trim(). The case folding of "json" is ASCII only, as the JS /i flag does not
// map a non-ASCII letter such as U+017F to an ASCII one.
func stripFences(content string) string {
	s := jsstr.Trim(content)
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		if len(rest) >= 4 && asciiEqualFold(rest[:4], "json") {
			rest = rest[4:]
		}
		s = jsstr.TrimStart(rest)
	}
	if rest, ok := strings.CutSuffix(s, "```"); ok {
		s = jsstr.TrimEnd(rest)
	}
	return jsstr.Trim(s)
}

// asciiEqualFold compares a with an all lower case ASCII b, ignoring the case of the
// ASCII letters of a only.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != b[i] {
			return false
		}
	}
	return true
}

// toExtraction reads one element of the reply. An element that is not an object
// fails with a JavaScript-style TypeError text: one for a number, a string or a
// boolean and a different one for null. An array counts as an object with no fields.
//
// The typed Record holds a string in every field, so three shapes cannot be held and
// are documented exceptions, asserted by TestEvaluateMatchesGolden:
//
//	field-not-a-string     a kind, scope, subject, value or confidence that is a number,
//	                       boolean, object or array. The pass fails at that element
//	                       with an error. key is not in this list because it is always
//	                       turned into a string (String(key ?? "")).
//	null-or-missing-field  a kind, scope, subject, value or confidence that is null or
//	                       absent. The empty string is stored, which encodes as ""
//	                       instead of null or nothing.
//	                       An array element is the same case for every field.
func toExtraction(v any) (extraction, error) {
	switch el := v.(type) {
	case nil:
		return extraction{}, &jsTypeError{msg: "Cannot read properties of null (reading 'key')"}
	case json.Number:
		return extraction{}, &jsTypeError{msg: "Cannot create property 'key' on number '" + jsToString(el) + "'"}
	case string:
		// The text of the element is model output: bounded in the error, withheld from logs.
		shown := jsstr.TruncateUTF16(el, maxErrorElementUnits)
		if len(shown) < len(el) {
			shown += "..."
		}
		return extraction{}, &jsTypeError{
			msg:    "Cannot create property 'key' on string '" + shown + "'",
			logMsg: "Cannot create property 'key' on string (text withheld from the log)",
		}
	case bool:
		return extraction{}, &jsTypeError{msg: "Cannot create property 'key' on boolean '" + jsToString(el) + "'"}
	case []any:
		return extraction{}, nil
	case map[string]any:
		var ex extraction
		var err error
		for _, f := range []struct {
			name string
			dst  *string
		}{{"kind", &ex.kind}, {"scope", &ex.scope}, {"subject", &ex.subject}, {"value", &ex.value}, {"confidence", &ex.confidence}} {
			if *f.dst, err = stringField(el, f.name); err != nil {
				return extraction{}, err
			}
		}
		// String(key ?? ""): a missing or null key is the empty string.
		if k, ok := el["key"]; ok && k != nil {
			ex.key = jsToString(k)
		}
		return ex, nil
	}
	return extraction{}, errors.New("memory extraction element has an unexpected type")
}

// stringField reads a string field of a reply element; null or absent is "".
func stringField(m map[string]any, name string) (string, error) {
	v, ok := m[name]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("memory extraction field %q is not a string", name)
	}
	return s, nil
}

// jsToString is String(v) for a value decoded from JSON: a null (or undefined) is
// "" here because the only callers are the key coercion, where `?? ""` runs first,
// and Array.prototype.join, which writes null as an empty string.
func jsToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		f, _ := strconv.ParseFloat(string(x), 64) // an out of range value is +-Inf, as in JS
		return jsNumberString(f)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = jsToString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	return ""
}
