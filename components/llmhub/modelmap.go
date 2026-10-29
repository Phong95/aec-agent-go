package llmhub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/viewkit"
)

// Pure, side-effect-free mappers from provider-specific raw list-models
// payloads to the SDK's provider-agnostic ModelInfo contract (A-54).
// Every field is read with type checks rather than trusting the static
// type, because vendor payloads vary and this file must never fail on a
// malformed or unexpected shape: unknown fields simply map to absent.
//
// Each mapper takes the raw JSON bytes of ONE catalog object, so a provider's
// ListModels does only transport and paging and never decodes a catalog itself.
// Numbers are read as JS doubles (a JSON number outside the double range is an
// infinity, as in JS), so a value that is not finite is absent.

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// jsObject is a decoded JSON object.
type jsObject = map[string]any

// decodeObject decodes raw into an object, keeping numbers as json.Number so
// that none is lost to a float64 overflow. Anything that is not one JSON object
// (including invalid JSON) gives nil, which reads as an object with no keys.
func decodeObject(raw json.RawMessage) jsObject {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// number reads a JSON number as a JS double; a number outside the double range
// is an infinity, as JSON.parse reads it.
func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, _ := strconv.ParseFloat(string(n), 64) // a range error still returns the infinity JS gives
	return f, true
}

// intLimit is the first float64 that does not fit an int.
var intLimit = math.Ldexp(1, bits.UintSize-1)

// positiveInt returns a finite integer > 0, otherwise absent. A value that does
// not fit an int is absent too.
func positiveInt(v any) *int {
	f, ok := number(v)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil
	}
	n := math.Trunc(f)
	if n <= 0 || n >= intLimit {
		return nil
	}
	i := int(n)
	return &i
}

// getPath reads a dotted path (e.g. "top_provider.context_length") off an
// object. Only objects are walked into: an array, a scalar or null on the way
// gives absent.
func getPath(obj jsObject, path string) any {
	var cur any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// firstPositiveInt returns the first path (dotted paths allowed) that yields a
// positive int.
func firstPositiveInt(obj jsObject, paths ...string) *int {
	for _, path := range paths {
		if v := positiveInt(getPath(obj, path)); v != nil {
			return v
		}
	}
	return nil
}

// unixSecondsToIso turns unix seconds into ISO 8601 text, absent when n is not
// a number > 0. A value outside the JS Date range is absent.
func unixSecondsToIso(v any) string {
	n, ok := number(v)
	if !ok || math.IsInf(n, 0) || math.IsNaN(n) || n <= 0 {
		return ""
	}
	ms := math.Trunc(n * 1000) // new Date(n * 1000) truncates a fractional millisecond
	if ms > maxDateMs {
		return ""
	}
	return isoString(int64(ms))
}

// isoOrUndefined parses a date and returns it as ISO 8601 text; the date must be
// after the epoch, otherwise absent. That drops the epoch placeholder.
func isoOrUndefined(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	ms, ok := parseDate(s)
	if !ok || ms <= 0 {
		return ""
	}
	return isoString(ms)
}

// lowerStringArray is absent unless the value is a non-empty array of strings,
// which come back lowercased.
func lowerStringArray(v any) []string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]string, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out[i] = jsstr.ToLower(s)
	}
	return out
}

// stringArray is the strings of an array, in order, and false when v is not an
// array. Elements that are not strings are dropped.
func stringArray(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := []string{}
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

// compactCapabilities drops absent flags and is absent when nothing is left.
func compactCapabilities(c ModelCapabilities) *ModelCapabilities {
	if c.Vision == nil && c.PDFInput == nil && c.ToolCalling == nil && c.Reasoning == nil &&
		c.StructuredOutputs == nil && c.Streaming == nil {
		return nil
	}
	return &c
}

// supported reads obj[key].supported when it is a boolean.
func supported(obj jsObject, key string) *bool {
	entry, ok := obj[key].(map[string]any)
	if !ok {
		return nil
	}
	if b, ok := entry["supported"].(bool); ok {
		return &b
	}
	return nil
}

// firstNonEmptyString is the first non-empty string among the given values.
func firstNonEmptyString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// stringOf is v when it is a string. An empty string is the same as absent in a
// ModelInfo.
func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// nonNegativeNumber is a finite number >= 0, otherwise absent. It accepts
// numeric strings, read as JS Number() reads them (so the empty string is 0).
func nonNegativeNumber(v any) *float64 {
	var n float64
	switch x := v.(type) {
	case string:
		n = viewkit.JSNumber(x)
	case json.Number:
		n, _ = number(x)
	default:
		return nil
	}
	if math.IsInf(n, 0) || math.IsNaN(n) || n < 0 {
		return nil
	}
	return &n
}

// boolPtr returns &b.
func boolPtr(b bool) *bool { return &b }

// includes reports whether list holds s.
func includes(list []string, s string) bool { return slices.Contains(list, s) }

// rawOrNil is the untouched input bytes when they are valid JSON, so that the
// info always encodes; invalid JSON has no payload to keep.
func rawOrNil(raw json.RawMessage) json.RawMessage {
	if !json.Valid(raw) {
		return nil
	}
	return slices.Clone(raw)
}

// ---------------------------------------------------------------------------
// JS Date: the parts the mappers need
// ---------------------------------------------------------------------------

// maxDateMs is the largest time value of a JS Date (8.64e15 ms either way).
const maxDateMs = 8.64e15

// isoString formats a time value like Date.prototype.toISOString: a year outside
// 0..9999 is a sign and six digits.
func isoString(ms int64) string {
	t := time.UnixMilli(ms).UTC()
	y := t.Year()
	year := fmt.Sprintf("%04d", y)
	if y < 0 || y > 9999 {
		sign := "+"
		if y < 0 {
			sign, y = "-", -y
		}
		year = fmt.Sprintf("%s%06d", sign, y)
	}
	return fmt.Sprintf("%s-%02d-%02dT%02d:%02d:%02d.%03dZ", year, int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1_000_000)
}

// fixedDigits reads exactly n ASCII digits of s at i.
func fixedDigits(s string, i, n int) (int64, bool) {
	if i < 0 || i+n > len(s) {
		return 0, false
	}
	var v int64
	for k := range n {
		c := s[i+k]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int64(c-'0')
	}
	return v, true
}

// floorDiv divides rounding toward negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// daysFromCivil is the number of days from 1970-01-01 to the given date. A day
// past the end of the month rolls into the next month, as in a JS Date.
func daysFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := floorDiv(y, 400)
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// parseDate reads the date formats Date.parse takes in ISO 8601 form
// (YYYY, YYYY-MM, YYYY-MM-DD, with an optional time HH:mm[:ss[.fraction]] and a
// Z or a +-HH:mm offset, or the six-digit extended year) as a JS time value in
// milliseconds. A time with no offset is read as UTC (JS reads it as local
// time), and the legacy free-form formats Date.parse also takes are not read.
func parseDate(s string) (int64, bool) {
	var year int64
	i := 0
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		y, ok := fixedDigits(s, 1, 6)
		if !ok || (s[0] == '-' && y == 0) {
			return 0, false
		}
		if s[0] == '-' {
			y = -y
		}
		year, i = y, 7
	} else {
		y, ok := fixedDigits(s, 0, 4)
		if !ok {
			return 0, false
		}
		year, i = y, 4
	}

	month, day := int64(1), int64(1)
	if i < len(s) && s[i] == '-' {
		m, ok := fixedDigits(s, i+1, 2)
		if !ok || m < 1 || m > 12 {
			return 0, false
		}
		month, i = m, i+3
		if i < len(s) && s[i] == '-' {
			d, ok := fixedDigits(s, i+1, 2)
			if !ok || d < 1 || d > 31 {
				return 0, false
			}
			day, i = d, i+3
		}
	}

	var hour, minute, second, milli int64
	hasTime := false
	if i < len(s) && (s[i] == 'T' || s[i] == 't' || s[i] == ' ') {
		hasTime = true
		h, ok1 := fixedDigits(s, i+1, 2)
		mi, ok2 := fixedDigits(s, i+4, 2)
		if !ok1 || !ok2 || i+3 >= len(s) || s[i+3] != ':' {
			return 0, false
		}
		hour, minute, i = h, mi, i+6
		if i < len(s) && s[i] == ':' {
			sec, ok := fixedDigits(s, i+1, 2)
			if !ok {
				return 0, false
			}
			second, i = sec, i+3
			if i < len(s) && s[i] == '.' {
				start := i + 1
				j := start
				for j < len(s) && s[j] >= '0' && s[j] <= '9' {
					j++
				}
				if j == start {
					return 0, false
				}
				frac := s[start:j]
				if len(frac) > 3 {
					frac = frac[:3]
				}
				for len(frac) < 3 {
					frac += "0"
				}
				milli, _ = fixedDigits(frac, 0, 3)
				i = j
			}
		}
		if hour > 24 || minute > 59 || second > 59 || (hour == 24 && (minute != 0 || second != 0 || milli != 0)) {
			return 0, false
		}
	}

	var offsetMs int64
	if i < len(s) {
		switch s[i] {
		case 'Z', 'z':
			i++
		case '+', '-':
			if !hasTime {
				return 0, false
			}
			sign := int64(1)
			if s[i] == '-' {
				sign = -1
			}
			hh, ok := fixedDigits(s, i+1, 2)
			if !ok {
				return 0, false
			}
			next := i + 3
			if next < len(s) && s[next] == ':' {
				next++
			}
			mm, ok := fixedDigits(s, next, 2)
			if !ok || hh > 23 || mm > 59 {
				return 0, false
			}
			offsetMs = sign * (hh*3600000 + mm*60000)
			i = next + 2
		}
	}
	if i != len(s) {
		return 0, false
	}

	days := daysFromCivil(year, month, day)
	ms := (((days*24+hour)*60+minute)*60+second)*1000 + milli - offsetMs
	if ms > maxDateMs || ms < -maxDateMs {
		return 0, false
	}
	return ms, true
}

// ---------------------------------------------------------------------------
// Anthropic
// ---------------------------------------------------------------------------

// MapAnthropicModel maps one raw Anthropic model-list entry to a ModelInfo
// whose Provider is provider. Raw is the input bytes, untouched.
func MapAnthropicModel(raw json.RawMessage, provider string) ModelInfo {
	obj := decodeObject(raw)

	var capabilitiesObj jsObject
	hasCapabilities := false
	switch c := obj["capabilities"].(type) {
	case map[string]any:
		hasCapabilities, capabilitiesObj = true, c
	case []any:
		hasCapabilities = true // an array is an object to a typeof check
	}

	var inputModalities []string
	var capabilities *ModelCapabilities
	if hasCapabilities {
		inputModalities = []string{"text"}
		if v := supported(capabilitiesObj, "image_input"); v != nil && *v {
			inputModalities = append(inputModalities, "image")
		}
		if v := supported(capabilitiesObj, "pdf_input"); v != nil && *v {
			inputModalities = append(inputModalities, "pdf")
		}
		capabilities = compactCapabilities(ModelCapabilities{
			Vision:   supported(capabilitiesObj, "image_input"),
			PDFInput: supported(capabilitiesObj, "pdf_input"),
			// Anthropic does not report tool-calling support in this payload.
			ToolCalling:       nil,
			Reasoning:         supported(capabilitiesObj, "thinking"),
			StructuredOutputs: supported(capabilitiesObj, "structured_outputs"),
		})
	}

	return ModelInfo{
		ModelRecord: ModelRecord{
			ID:                  stringOf(obj["id"]),
			Provider:            provider,
			DisplayName:         stringOf(obj["display_name"]),
			OwnedBy:             "anthropic",
			CreatedAt:           isoOrUndefined(obj["created_at"]),
			ContextWindowTokens: positiveInt(obj["max_input_tokens"]),
			MaxOutputTokens:     positiveInt(obj["max_tokens"]),
			InputModalities:     inputModalities,
			ReasoningEfforts:    anthropicReasoningEfforts(capabilitiesObj),
			Capabilities:        capabilities,
		},
		Raw: rawOrNil(raw),
	}
}

// anthropicEfforts (A-164) are the Anthropic effort levels, lowest first (the order the
// Models API capabilities use).
var anthropicEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// anthropicReasoningEfforts (A-164) is each level of capabilities.effort whose supported
// is true, in anthropicEfforts order; nil when the model does not report effort support
// or accepts none.
func anthropicReasoningEfforts(capabilities jsObject) []string {
	if v := supported(capabilities, "effort"); v == nil || !*v {
		return nil
	}
	effort, _ := capabilities["effort"].(map[string]any)
	var efforts []string
	for _, level := range anthropicEfforts {
		if v := supported(effort, level); v != nil && *v {
			efforts = append(efforts, level)
		}
	}
	return efforts
}

// ---------------------------------------------------------------------------
// OpenAI-compatible (OpenAI, Novita, OpenRouter, Groq, vLLM, ...)
// ---------------------------------------------------------------------------

// MapOpenAICompatibleModel maps one raw entry of an OpenAI-compatible
// /models list to a ModelInfo whose Provider is provider. Raw is the input
// bytes, untouched.
func MapOpenAICompatibleModel(raw json.RawMessage, provider string) ModelInfo {
	obj := decodeObject(raw)

	displayName := firstNonEmptyString(obj["display_name"], obj["name"], obj["title"])

	contextWindowTokens := firstPositiveInt(obj,
		"context_window",
		"context_length",
		"context_size",
		"max_context_length",
		"max_model_len",
		"top_provider.context_length",
		"max_input_tokens",
	)

	maxOutputTokens := firstPositiveInt(obj,
		"max_output_tokens",
		"max_completion_tokens",
		"top_provider.max_completion_tokens",
		"max_tokens",
	)

	inputModalities := lowerStringArray(obj["input_modalities"])
	if inputModalities == nil {
		inputModalities = lowerStringArray(getPath(obj, "architecture.input_modalities"))
	}
	outputModalities := lowerStringArray(obj["output_modalities"])
	if outputModalities == nil {
		outputModalities = lowerStringArray(getPath(obj, "architecture.output_modalities"))
	}

	supportedParameters, hasSupportedParameters := stringArray(obj["supported_parameters"])
	features, hasFeatures := stringArray(obj["features"])

	// vision is derived whenever inputModalities is known (true or false).
	// toolCalling/reasoning/structuredOutputs are inclusion tests against the
	// vendor's supported_parameters/features arrays: true when matched,
	// absent otherwise (never guessed false) - the arrays are an open set
	// and a vendor's silence about a feature is not evidence of its absence.
	var capabilities *ModelCapabilities
	if inputModalities != nil || hasSupportedParameters || hasFeatures {
		var c ModelCapabilities
		if inputModalities != nil {
			c.Vision = boolPtr(includes(inputModalities, "image"))
		}
		if includes(supportedParameters, "tools") || includes(features, "function-calling") || includes(features, "tools") {
			c.ToolCalling = boolPtr(true)
		}
		if includes(supportedParameters, "reasoning") || includes(features, "reasoning") {
			c.Reasoning = boolPtr(true)
		}
		if includes(supportedParameters, "structured_outputs") || includes(supportedParameters, "response_format") ||
			includes(features, "structured-outputs") {
			c.StructuredOutputs = boolPtr(true)
		}
		capabilities = compactCapabilities(c)
	}

	// Pricing is only normalized for the OpenRouter shape (pricing.prompt /
	// pricing.completion, USD per token, as numeric strings). Other vendors'
	// price fields have undocumented units (e.g. Novita's integer
	// input_token_price_per_m) and so are left in raw rather than guessed.
	var pricing *ModelPricing
	if pricingObj, ok := obj["pricing"].(map[string]any); ok {
		promptPrice := nonNegativeNumber(pricingObj["prompt"])
		completionPrice := nonNegativeNumber(pricingObj["completion"])
		if promptPrice != nil || completionPrice != nil {
			pricing = &ModelPricing{}
			if promptPrice != nil {
				v := *promptPrice * 1_000_000
				pricing.InputUSDPerMillionTokens = &v
			}
			if completionPrice != nil {
				v := *completionPrice * 1_000_000
				pricing.OutputUSDPerMillionTokens = &v
			}
		}
	}

	return ModelInfo{
		ModelRecord: ModelRecord{
			ID:                  stringOf(obj["id"]),
			Provider:            provider,
			DisplayName:         displayName,
			Description:         stringOf(obj["description"]),
			OwnedBy:             stringOf(obj["owned_by"]),
			CreatedAt:           unixSecondsToIso(obj["created"]),
			ContextWindowTokens: contextWindowTokens,
			MaxOutputTokens:     maxOutputTokens,
			InputModalities:     inputModalities,
			OutputModalities:    outputModalities,
			Capabilities:        capabilities,
			Pricing:             pricing,
		},
		Raw: rawOrNil(raw),
	}
}

// ---------------------------------------------------------------------------
// Bedrock (ListFoundationModels)
// ---------------------------------------------------------------------------

// MapBedrockFoundationModel maps one raw Bedrock model summary (the JSON of the
// ListFoundationModels summary) to a ModelInfo whose Provider is provider. The
// second result is false, and the info empty, when the summary has no modelId:
// such an entry is dropped. Raw is the input bytes, untouched.
func MapBedrockFoundationModel(raw json.RawMessage, provider string) (ModelInfo, bool) {
	obj := decodeObject(raw)

	id := stringOf(obj["modelId"])
	if id == "" {
		return ModelInfo{}, false
	}

	inputModalities := lowerStringArray(obj["inputModalities"])
	outputModalities := lowerStringArray(obj["outputModalities"])
	var c ModelCapabilities
	if inputModalities != nil {
		c.Vision = boolPtr(includes(inputModalities, "image"))
	}
	if b, ok := obj["responseStreamingSupported"].(bool); ok {
		c.Streaming = &b
	}

	return ModelInfo{
		ModelRecord: ModelRecord{
			ID:               id,
			Provider:         provider,
			DisplayName:      stringOf(obj["modelName"]),
			OwnedBy:          stringOf(obj["providerName"]),
			InputModalities:  inputModalities,
			OutputModalities: outputModalities,
			Capabilities:     compactCapabilities(c),
		},
		Raw: rawOrNil(raw),
	}, true
}

// ---------------------------------------------------------------------------
// ChatGPT subscription (Codex models endpoint)
// ---------------------------------------------------------------------------

// MapCodexModel maps one raw entry of the ChatGPT Codex models list to a
// ModelInfo whose Provider is provider. Raw is the input bytes, untouched.
func MapCodexModel(raw json.RawMessage, provider string) ModelInfo {
	obj := decodeObject(raw)

	inputModalities := lowerStringArray(obj["input_modalities"])
	var c ModelCapabilities
	if inputModalities != nil {
		c.Vision = boolPtr(includes(inputModalities, "image"))
	}
	// A model that reports parallel tool-call support (either value) calls tools.
	if _, ok := obj["supports_parallel_tool_calls"].(bool); ok {
		c.ToolCalling = boolPtr(true)
	}
	if levels, ok := obj["supported_reasoning_levels"].([]any); ok {
		c.Reasoning = boolPtr(len(levels) > 0)
	}

	return ModelInfo{
		ModelRecord: ModelRecord{
			ID:                  stringOf(obj["slug"]),
			Provider:            provider,
			DisplayName:         stringOf(obj["display_name"]),
			Description:         stringOf(obj["description"]),
			OwnedBy:             "openai",
			ContextWindowTokens: firstPositiveInt(obj, "context_window", "max_context_window"),
			// How far a client may raise the window above the default (Codex: model_context_window).
			MaxContextWindowTokens: firstPositiveInt(obj, "max_context_window"),
			InputModalities:        inputModalities,
			ReasoningEfforts:       codexReasoningEfforts(obj["supported_reasoning_levels"]),
			DefaultReasoningEffort: strings.ToLower(stringOf(obj["default_reasoning_level"])),
			Capabilities:           compactCapabilities(c),
		},
		Raw: rawOrNil(raw),
	}
}

// codexReasoningEfforts is the effort of each Codex supported_reasoning_levels
// entry, lowercased, in order and without repeats; absent when no entry has one.
func codexReasoningEfforts(v any) []string {
	levels, ok := v.([]any)
	if !ok {
		return nil
	}
	var efforts []string
	for _, level := range levels {
		entry, ok := level.(jsObject)
		if !ok {
			continue
		}
		effort := strings.ToLower(stringOf(entry["effort"]))
		if effort != "" && !slices.Contains(efforts, effort) {
			efforts = append(efforts, effort)
		}
	}
	return efforts
}

// ---------------------------------------------------------------------------
// Google Gemini (models.list)
// ---------------------------------------------------------------------------

// MapGeminiModel maps one entry of the Gemini API models list (GET /v1beta/models)
// to a ModelInfo whose Provider is provider. The ID is the entry's name without its
// "models/" prefix, the form ModelOptions.Model takes. Raw is the input bytes,
// untouched.
func MapGeminiModel(raw json.RawMessage, provider string) ModelInfo {
	obj := decodeObject(raw)

	var c ModelCapabilities
	if b, ok := obj["thinking"].(bool); ok {
		c.Reasoning = &b
	}
	if methods, ok := stringArray(obj["supportedGenerationMethods"]); ok {
		c.Streaming = boolPtr(includes(methods, "streamGenerateContent"))
	}

	return ModelInfo{
		ModelRecord: ModelRecord{
			ID:                  strings.TrimPrefix(stringOf(obj["name"]), "models/"),
			Provider:            provider,
			DisplayName:         stringOf(obj["displayName"]),
			Description:         stringOf(obj["description"]),
			OwnedBy:             "google",
			ContextWindowTokens: positiveInt(obj["inputTokenLimit"]),
			MaxOutputTokens:     positiveInt(obj["outputTokenLimit"]),
			Capabilities:        compactCapabilities(c),
		},
		Raw: rawOrNil(raw),
	}
}
