// Package aecmock holds public test helpers for code that builds on the SDK.
// It imports only the standard library and the
// SDK's own contracts, so tests outside this module (server/go, third-party
// tool packages) can import it, the same way they import the conformance
// suites.
//
// The mock tools in this file are deterministic stand-ins for real tools: they
// return canned data and never touch a network, a file or a clock.
package aecmock

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"strconv"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that every mock tool is a toolhub.Handler.
var (
	_ toolhub.Handler = (*ScheduleLookupTool)(nil)
	_ toolhub.Handler = (*BudgetQueryTool)(nil)
	_ toolhub.Handler = (*DocumentSearchTool)(nil)
	_ toolhub.Handler = (*SendRfiTool)(nil)
)

const mockVersion = "1.0.0"

// emDash is U+2014, built from bytes so this source stays ASCII-only.
var emDash = string([]byte{0xe2, 0x80, 0x94})

// stringProp is a string property schema with a description.
func stringProp(name, description string) toolhub.Property {
	return toolhub.Property{Name: name, Schema: toolhub.JSONSchema{Type: "string", Description: description}}
}

// rawArgs decodes the argument object into raw values, so that a key that is
// absent stays distinct from one that is JSON null.
func rawArgs(args kernel.Arguments) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := args.Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// reencode round-trips one raw argument value through a generic decode and the
// SDK encoder: the tools put the parsed value into an object and encode it
// again, so key order and number spelling follow a JSON decode then encode.
func reencode(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return kernel.EncodeJSON(v)
}

// argument returns the re-encoded value of key, or nil when the key is absent
// (the property is then omitted from the output).
func argument(raw map[string]json.RawMessage, key string) (json.RawMessage, error) {
	v, ok := raw[key]
	if !ok {
		return nil, nil
	}
	return reencode(v)
}

// nullish reports whether a re-encoded argument is absent or JSON null, the
// two cases that fall back to a default.
func nullish(v json.RawMessage) bool {
	return v == nil || string(v) == "null"
}

// result encodes out as the content of a successful tool output.
func result(out any) (toolhub.Output, error) {
	b, err := kernel.EncodeJSON(out)
	if err != nil {
		return toolhub.Output{}, err
	}
	return toolhub.Output{Content: string(b)}, nil
}

// ---------------------------------------------------------------------------
// schedule_lookup
// ---------------------------------------------------------------------------

// ScheduleLookupTool is the mock schedule_lookup tool.
// Its zero value is ready to use.
type ScheduleLookupTool struct{}

// Name returns the plugin name.
func (*ScheduleLookupTool) Name() string { return "tool:schedule-lookup" }

// Version returns the plugin version.
func (*ScheduleLookupTool) Version() string { return mockVersion }

// Initialize does nothing.
func (*ScheduleLookupTool) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*ScheduleLookupTool) Dispose(context.Context) error { return nil }

// Definition returns the tool definition.
func (*ScheduleLookupTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "schedule_lookup",
		Risk:        toolhub.RiskReadOnly,
		Description: "Query project schedule data from Primavera P6",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				stringProp("projectId", "Project identifier"),
				stringProp("scope", "Filter by structure or zone"),
			),
			Required: []string{"projectId"},
		},
	}
}

type scheduleTask struct {
	Name    string `json:"name"`
	Planned string `json:"planned"`
	Actual  string `json:"actual"`
	Status  string `json:"status"`
}

type scheduleResult struct {
	Project json.RawMessage `json:"project,omitzero"`
	Scope   json.RawMessage `json:"scope"`
	Tasks   []scheduleTask  `json:"tasks"`
}

// Execute returns the canned schedule. The scope defaults to "all" when it is
// absent or null.
func (*ScheduleLookupTool) Execute(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	raw, err := rawArgs(args)
	if err != nil {
		return toolhub.Output{}, err
	}
	project, err := argument(raw, "projectId")
	if err != nil {
		return toolhub.Output{}, err
	}
	scope, err := argument(raw, "scope")
	if err != nil {
		return toolhub.Output{}, err
	}
	if nullish(scope) {
		scope = json.RawMessage(`"all"`)
	}
	return result(scheduleResult{
		Project: project,
		Scope:   scope,
		Tasks: []scheduleTask{
			{Name: "Foundation - East Wing", Planned: "2026-08-01", Actual: "2026-08-04", Status: "behind"},
			{Name: "Foundation - West Wing", Planned: "2026-08-15", Actual: "2026-08-14", Status: "ahead"},
		},
	})
}

// ---------------------------------------------------------------------------
// budget_query
// ---------------------------------------------------------------------------

// BudgetQueryTool is the mock budget_query tool. Its zero
// value is ready to use.
type BudgetQueryTool struct{}

// Name returns the plugin name.
func (*BudgetQueryTool) Name() string { return "tool:budget-query" }

// Version returns the plugin version.
func (*BudgetQueryTool) Version() string { return mockVersion }

// Initialize does nothing.
func (*BudgetQueryTool) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*BudgetQueryTool) Dispose(context.Context) error { return nil }

// Definition returns the tool definition.
func (*BudgetQueryTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "budget_query",
		Risk:        toolhub.RiskReadOnly,
		Description: "Query project budget and cost data",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				stringProp("projectId", "Project identifier"),
				stringProp("category", "Budget category filter"),
			),
			Required: []string{"projectId"},
		},
	}
}

type budgetResult struct {
	Project     json.RawMessage `json:"project,omitzero"`
	TotalBudget int             `json:"totalBudget"`
	Spent       int             `json:"spent"`
	Committed   int             `json:"committed"`
	Remaining   int             `json:"remaining"`
}

// Execute returns the canned budget figures.
func (*BudgetQueryTool) Execute(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	raw, err := rawArgs(args)
	if err != nil {
		return toolhub.Output{}, err
	}
	project, err := argument(raw, "projectId")
	if err != nil {
		return toolhub.Output{}, err
	}
	return result(budgetResult{
		Project:     project,
		TotalBudget: 5_000_000,
		Spent:       2_150_000,
		Committed:   1_200_000,
		Remaining:   1_650_000,
	})
}

// ---------------------------------------------------------------------------
// document_search
// ---------------------------------------------------------------------------

// DocumentSearchTool is the mock document_search tool.
// Its zero value is ready to use.
type DocumentSearchTool struct{}

// Name returns the plugin name.
func (*DocumentSearchTool) Name() string { return "tool:document-search" }

// Version returns the plugin version.
func (*DocumentSearchTool) Version() string { return mockVersion }

// Initialize does nothing.
func (*DocumentSearchTool) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*DocumentSearchTool) Dispose(context.Context) error { return nil }

// Definition returns the tool definition. The docType property encodes with
// enum before description (the JSONSchema field order); the recorded fixture has
// them the other way round, so the two are equal as JSON but not byte for byte.
func (*DocumentSearchTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "document_search",
		Risk:        toolhub.RiskReadOnly,
		Description: "Search project documents, RFIs, and submittals",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				stringProp("query", "Search query"),
				toolhub.Property{Name: "docType", Schema: toolhub.JSONSchema{
					Type:        "string",
					Description: "Document type filter",
					Enum:        []string{"rfi", "submittal", "drawing", "specification"},
				}},
			),
			Required: []string{"query"},
		},
	}
}

type documentHit struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type documentResult struct {
	Query   json.RawMessage `json:"query,omitzero"`
	Results []documentHit   `json:"results"`
}

// Execute returns the canned search hits.
func (*DocumentSearchTool) Execute(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	raw, err := rawArgs(args)
	if err != nil {
		return toolhub.Output{}, err
	}
	query, err := argument(raw, "query")
	if err != nil {
		return toolhub.Output{}, err
	}
	return result(documentResult{
		Query: query,
		Results: []documentHit{
			{ID: "RFI-042", Title: "Facade material clarification", Status: "open"},
			{ID: "RFI-038", Title: "Foundation rebar spacing", Status: "closed"},
		},
	})
}

// ---------------------------------------------------------------------------
// send_rfi
// ---------------------------------------------------------------------------

// SendRfiTool is the consequential mock send_rfi tool. It
// exercises the approval gate: the harness asks the user before every call and
// nothing is actually sent. Its zero value is ready to use.
type SendRfiTool struct {
	// RandIntn returns a number in [0, n) for the RFI id. Nil uses crypto/rand;
	// set it in a test to make the id deterministic.
	RandIntn func(n int) int
}

// Name returns the plugin name.
func (*SendRfiTool) Name() string { return "tool:send-rfi" }

// Version returns the plugin version.
func (*SendRfiTool) Version() string { return mockVersion }

// Initialize does nothing.
func (*SendRfiTool) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*SendRfiTool) Dispose(context.Context) error { return nil }

// Definition returns the tool definition; its risk is consequential.
func (*SendRfiTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "send_rfi",
		Description: "Send a Request for Information (RFI) to an external consultant. This is an external action " +
			"that cannot be recalled, so the user is asked to approve it first.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				stringProp("projectId", "Project identifier"),
				stringProp("recipient", "Consultant email address"),
				stringProp("subject", "RFI subject line"),
				stringProp("question", "The question to send"),
			),
			Required: []string{"projectId", "recipient", "subject", "question"},
		},
		Risk: toolhub.RiskConsequential,
	}
}

// randIntn draws from RandIntn, or from crypto/rand when it is nil.
func (s *SendRfiTool) randIntn(n int) int {
	if s.RandIntn != nil {
		return s.RandIntn(n)
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

type rfiResult struct {
	Status    string          `json:"status"`
	RfiID     string          `json:"rfiId"`
	Project   json.RawMessage `json:"project,omitzero"`
	Recipient json.RawMessage `json:"recipient,omitzero"`
	Subject   json.RawMessage `json:"subject,omitzero"`
}

// Execute returns a mock "sent" status and an RFI id between RFI-100 and
// RFI-999. Nothing is delivered.
func (s *SendRfiTool) Execute(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	raw, err := rawArgs(args)
	if err != nil {
		return toolhub.Output{}, err
	}
	var out rfiResult
	for _, f := range []struct {
		key string
		dst *json.RawMessage
	}{
		{"projectId", &out.Project},
		{"recipient", &out.Recipient},
		{"subject", &out.Subject},
	} {
		v, err := argument(raw, f.key)
		if err != nil {
			return toolhub.Output{}, err
		}
		*f.dst = v
	}
	out.Status = "sent (mock " + emDash + " no email was delivered)"
	out.RfiID = "RFI-" + strconv.Itoa(100+s.randIntn(900))
	return result(out)
}
