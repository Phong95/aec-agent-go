package kernel

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// errorRow is one row of tests/testdata/golden/errors.json, the recorded fixture of
// every error class (one instance per class and code).
type errorRow struct {
	Class     string `json:"class"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	Retryable bool   `json:"retryable"`
}

func loadErrorRows(t *testing.T) []errorRow {
	t.Helper()
	var rows []errorRow
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "errors.json"), &rows); err != nil {
		t.Fatalf("decode errors.json: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("errors.json is empty")
	}
	return rows
}

// asName finds a T in err's chain through errors.As and returns its Name().
func asName[T interface {
	error
	Name() string
}](err error) (string, bool) {
	var target T
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Name(), true
}

// classSpec describes how to build and match one Go error class.
type classSpec struct {
	// build constructs the error with the given code; retryable is honoured only
	// when callerRetry is true (the other classes derive or fix it).
	build func(code string, retryable bool) error
	// target builds an errors.Is target; an empty code means class level.
	target func(code string) error
	// as reports whether errors.As to the concrete class succeeds and its Name().
	as func(err error) (string, bool)
	// callerRetry is true when the constructor takes retryable as an argument.
	callerRetry bool
}

// specFor is exhaustive over the eight class names and fails on an unknown
// one, so a class added to the fixture breaks this test until it is covered.
func specFor(t *testing.T, class string) classSpec {
	t.Helper()
	switch class {
	case "SDKError":
		return classSpec{
			build:       func(_ string, r bool) error { return NewSDKError("m", r) },
			target:      func(string) error { return &SDKError{} },
			as:          asName[*SDKError],
			callerRetry: true,
		}
	case "ModelError":
		return classSpec{
			build:       func(c string, r bool) error { return NewModelError("m", ModelErrorCode(c), r) },
			target:      func(c string) error { return &ModelError{Code: ModelErrorCode(c)} },
			as:          asName[*ModelError],
			callerRetry: true,
		}
	case "ToolError":
		return classSpec{
			build:       func(c string, r bool) error { return NewToolError("m", ToolErrorCode(c), "tool", r) },
			target:      func(c string) error { return &ToolError{Code: ToolErrorCode(c)} },
			as:          asName[*ToolError],
			callerRetry: true,
		}
	case "PolicyError":
		return classSpec{
			build:  func(c string, _ bool) error { return NewPolicyError("m", PolicyErrorCode(c), "tool") },
			target: func(c string) error { return &PolicyError{Code: PolicyErrorCode(c)} },
			as:     asName[*PolicyError],
		}
	case "SessionError":
		return classSpec{
			build:       func(c string, r bool) error { return NewSessionError("m", SessionErrorCode(c), r) },
			target:      func(c string) error { return &SessionError{Code: SessionErrorCode(c)} },
			as:          asName[*SessionError],
			callerRetry: true,
		}
	case "SkillError":
		return classSpec{
			build:       func(c string, r bool) error { return NewSkillError("m", SkillErrorCode(c), "skill", r) },
			target:      func(c string) error { return &SkillError{Code: SkillErrorCode(c)} },
			as:          asName[*SkillError],
			callerRetry: true,
		}
	case "SandboxError":
		return classSpec{
			build:  func(c string, _ bool) error { return NewSandboxError("m", SandboxErrorCode(c), "") },
			target: func(c string) error { return &SandboxError{Code: SandboxErrorCode(c)} },
			as:     asName[*SandboxError],
		}
	case "ArtifactError":
		return classSpec{
			build:  func(c string, _ bool) error { return NewArtifactError("m", ArtifactErrorCode(c), "") },
			target: func(c string) error { return &ArtifactError{Code: ArtifactErrorCode(c)} },
			as:     asName[*ArtifactError],
		}
	default:
		t.Fatalf("errors.json has class %q that this test does not know: port it to Go and add it here", class)
		return classSpec{}
	}
}

func TestErrorsMatchFixture(t *testing.T) {
	rows := loadErrorRows(t)

	// A different code of the same class, for the negative Is assertion.
	codesByClass := map[string][]string{}
	for _, r := range rows {
		codesByClass[r.Class] = append(codesByClass[r.Class], r.Code)
	}

	for _, row := range rows {
		t.Run(row.Class+"/"+row.Code, func(t *testing.T) {
			spec := specFor(t, row.Class)
			err := spec.build(row.Code, false)

			name, ok := spec.as(err)
			if !ok {
				t.Fatalf("errors.As to the concrete class failed for %T", err)
			}
			if name != row.Name {
				t.Errorf("Name() = %q, want %q (TS)", name, row.Name)
			}

			var sdk *SDKError
			if !errors.As(err, &sdk) {
				t.Fatalf("errors.As to *SDKError failed for %T", err)
			}
			if sdk.Retryable != row.Retryable {
				t.Errorf("Retryable = %v, want %v (TS)", sdk.Retryable, row.Retryable)
			}
			if IsRetryable(err) != row.Retryable {
				t.Errorf("IsRetryable = %v, want %v (TS)", IsRetryable(err), row.Retryable)
			}
			if sdk.Error() != "m" || err.Error() != "m" {
				t.Errorf("Error() = %q, want %q", err.Error(), "m")
			}

			// Class level matches; every error is also an SDKError at class level.
			if !errors.Is(err, spec.target("")) {
				t.Errorf("errors.Is class level failed")
			}
			if !errors.Is(err, &SDKError{}) {
				t.Errorf("errors.Is to the SDKError class failed")
			}
			if row.Class == "SDKError" {
				return
			}
			if !errors.Is(err, spec.target(row.Code)) {
				t.Errorf("errors.Is exact code %q failed", row.Code)
			}
			for _, other := range codesByClass[row.Class] {
				if other != row.Code && errors.Is(err, spec.target(other)) {
					t.Errorf("errors.Is matched a different code %q", other)
				}
			}
			// Another class never matches, even with the same code.
			for _, cls := range []string{"ModelError", "ToolError", "PolicyError", "SessionError", "SkillError", "SandboxError", "ArtifactError"} {
				if cls != row.Class && errors.Is(err, specFor(t, cls).target(row.Code)) {
					t.Errorf("errors.Is matched class %s", cls)
				}
			}

			// Caller-provided retryability is honoured.
			if spec.callerRetry {
				if got := spec.build(row.Code, true); !IsRetryable(got) {
					t.Errorf("constructing with retryable=true must yield a retryable error")
				}
			}
		})
	}
}

// goCodeConstants parses errors.go and errors_derived.go and returns, per code
// type name (for example "ModelErrorCode"), the string values of its constants.
func goCodeConstants(t *testing.T) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, file := range []string{"errors.go", "errors_derived.go"} {
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil || len(vs.Values) != 1 {
					continue
				}
				typeIdent, ok := vs.Type.(*ast.Ident)
				if !ok || !strings.HasSuffix(typeIdent.Name, "ErrorCode") {
					continue
				}
				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: constant of %s is not a string literal", file, typeIdent.Name)
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				out[typeIdent.Name] = append(out[typeIdent.Name], val)
			}
		}
	}
	return out
}

// TestErrorCodeSets requires the Go constants of every class to equal the
// codes of the fixture, so drift fails in both directions.
func TestErrorCodeSets(t *testing.T) {
	fixture := map[string][]string{}
	for _, r := range loadErrorRows(t) {
		if r.Class == "SDKError" {
			continue
		}
		fixture[r.Class+"Code"] = append(fixture[r.Class+"Code"], r.Code)
	}
	goSets := goCodeConstants(t)

	for typeName, want := range fixture {
		got := goSets[typeName]
		sort.Strings(got)
		w := append([]string(nil), want...)
		sort.Strings(w)
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s: Go constants %v differ from TS codes %v", typeName, got, w)
		}
	}
	for typeName := range goSets {
		if _, ok := fixture[typeName]; !ok {
			t.Errorf("Go declares code type %s that has no class in errors.json", typeName)
		}
	}
	if len(fixture) != 7 {
		t.Errorf("expected 7 code-carrying classes, fixture has %d", len(fixture))
	}
}

func TestSandboxRetryableSet(t *testing.T) {
	retryable := map[SandboxErrorCode]bool{
		SandboxCodeTimeout:                true,
		SandboxCodeCancelled:              true,
		SandboxCodeEnvironmentUnavailable: true,
	}
	for code := range nonRetryableSandboxCodes {
		if retryable[code] {
			t.Errorf("%s is in the non-retryable set", code)
		}
	}
	if len(nonRetryableSandboxCodes) != 11 {
		t.Errorf("non-retryable sandbox set has %d codes, want 11", len(nonRetryableSandboxCodes))
	}
	for _, c := range goCodeConstants(t)["SandboxErrorCode"] {
		got := NewSandboxError("m", SandboxErrorCode(c), "").Retryable
		if got != retryable[SandboxErrorCode(c)] {
			t.Errorf("sandbox code %s: Retryable = %v", c, got)
		}
	}
	for _, c := range goCodeConstants(t)["ArtifactErrorCode"] {
		if NewArtifactError("m", ArtifactErrorCode(c), "").Retryable {
			t.Errorf("artifact code %s must not be retryable", c)
		}
	}
	// Unknown (future) codes are retryable by default.
	if !NewArtifactError("m", "future_transient", "").Retryable || !NewSandboxError("m", "future_transient", "").Retryable {
		t.Error("an unknown code must be retryable by default")
	}
}

func TestExtraFieldsCarried(t *testing.T) {
	var te *ToolError
	if !errors.As(NewToolError("m", ToolCodeTimeout, "grep", false), &te) || te.ToolName != "grep" {
		t.Errorf("ToolError.ToolName lost: %+v", te)
	}
	var pe *PolicyError
	if !errors.As(NewPolicyError("m", PolicyCodeInvalidArgs, "edit"), &pe) || pe.ToolName != "edit" || pe.Retryable {
		t.Errorf("PolicyError fields wrong: %+v", pe)
	}
	var ke *SkillError
	if !errors.As(NewSkillError("m", SkillCodeReadOnly, "review", false), &ke) || ke.SkillName != "review" {
		t.Errorf("SkillError.SkillName lost: %+v", ke)
	}
	var se *SandboxError
	if !errors.As(NewSandboxError("m", SandboxCodeTimeout, "sb"), &se) || se.SandboxID != "sb" {
		t.Errorf("SandboxError.SandboxID lost: %+v", se)
	}
	var ae *ArtifactError
	if !errors.As(NewArtifactError("m", ArtifactCodeConflict, "art"), &ae) || ae.ArtifactID != "art" {
		t.Errorf("ArtifactError.ArtifactID lost: %+v", ae)
	}
}

// asSandboxRow is one row of errors-as-sandbox.json.
type asSandboxRow struct {
	Input        string `json:"input"`
	SandboxID    string `json:"sandboxId"`
	Message      string `json:"message"`
	Code         string `json:"code"`
	Retryable    bool   `json:"retryable"`
	SameInstance bool   `json:"sameInstance"`
}

func TestAsSandboxErrorMatchesGolden(t *testing.T) {
	var rows []asSandboxRow
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "errors-as-sandbox.json"), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	existing := NewSandboxError("x", SandboxCodeNotFound, "sb3")
	for _, row := range rows {
		t.Run(row.Input, func(t *testing.T) {
			var in error
			switch row.Input {
			case "error":
				in = errors.New("boom")
			case "string":
				// A string input of the fixture becomes an error with that text.
				in = errors.New("plain")
			case "sandbox-error":
				in = existing
			default:
				t.Fatalf("unknown input kind %q", row.Input)
			}
			got := AsSandboxError(in, row.SandboxID)
			if got.Message != row.Message {
				t.Errorf("message = %q, want %q", got.Message, row.Message)
			}
			if string(got.Code) != row.Code {
				t.Errorf("code = %q, want %q", got.Code, row.Code)
			}
			if got.Retryable != row.Retryable {
				t.Errorf("retryable = %v, want %v", got.Retryable, row.Retryable)
			}
			if (got == existing) != row.SameInstance {
				t.Errorf("same instance = %v, want %v", got == existing, row.SameInstance)
			}
			if !row.SameInstance && got.SandboxID != row.SandboxID {
				t.Errorf("SandboxID = %q, want %q", got.SandboxID, row.SandboxID)
			}
		})
	}
}

func TestAsSandboxErrorNilAndWrapped(t *testing.T) {
	// A nil error prints <nil>.
	got := AsSandboxError(nil, "sb")
	if got.Message != `Sandbox "sb" is unavailable: <nil>` || got.Code != SandboxCodeEnvironmentUnavailable {
		t.Errorf("nil input: %+v", got)
	}
	// A wrapped SandboxError is found through errors.As and returned unchanged.
	inner := NewSandboxError("x", SandboxCodeCancelled, "sb9")
	if AsSandboxError(fmt.Errorf("ctx: %w", inner), "other") != inner {
		t.Error("a wrapped SandboxError must be returned unchanged")
	}
	// Quotes in the id are written literally, not %q-escaped.
	q := AsSandboxError(errors.New("e"), `a"b`)
	if q.Message != `Sandbox "a"b" is unavailable: e` {
		t.Errorf("quote handling: %q", q.Message)
	}
}

func TestErrorsThroughWrap(t *testing.T) {
	base := NewModelError("rate limited", ModelCodeRateLimit, true)
	wrapped := fmt.Errorf("turn 3: %w", fmt.Errorf("provider: %w", base))

	var me *ModelError
	if !errors.As(wrapped, &me) || me != base {
		t.Fatalf("errors.As through %%w failed: %v", me)
	}
	var sdk *SDKError
	if !errors.As(wrapped, &sdk) || !sdk.Retryable {
		t.Fatalf("errors.As to *SDKError through %%w failed")
	}
	if !IsRetryable(wrapped) {
		t.Error("IsRetryable through %w must be true")
	}
	if !errors.Is(wrapped, &ModelError{Code: ModelCodeRateLimit}) {
		t.Error("errors.Is exact code through %w failed")
	}
	if IsRetryable(nil) || IsRetryable(errors.New("plain")) {
		t.Error("IsRetryable must be false for nil and for non-SDK errors")
	}
	if IsRetryable(NewPolicyError("m", PolicyCodeNotInAllowlist, "t")) {
		t.Error("policy errors are never retryable")
	}
}

// failingModelCall returns a nil *ModelError through the error interface: the
// returned error is NOT nil. This is the Go typed-nil trap; it is intentionally
// wrong code kept here to document the pitfall.
func failingModelCall() error {
	var e *ModelError
	return e
}

// okModelCall is the correct form: return a literal nil.
func okModelCall() error {
	return nil
}

func TestTypedNilTrap(t *testing.T) {
	if failingModelCall() == nil {
		t.Fatal("a nil *ModelError in an error interface must be non-nil (typed nil); the trap changed")
	}
	if okModelCall() != nil {
		t.Fatal("a literal nil must stay nil")
	}
	// Constructors never produce the trap: they return non-nil concrete pointers.
	if NewModelError("m", ModelCodeAuth, false) == nil || NewSandboxError("m", SandboxCodeTimeout, "") == nil {
		t.Fatal("constructors must return non-nil pointers")
	}
	// Helpers tolerate the trap without panicking.
	if IsRetryable(failingModelCall()) {
		t.Error("IsRetryable of a typed nil must be false")
	}
}
