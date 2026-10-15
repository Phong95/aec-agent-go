package kernel

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// causeCase is one error class with the generic WithCause bound to it.
type causeCase struct {
	err error
	// set calls WithCause on err and reports whether it returned err itself.
	set func(cause error) (error, bool)
}

func causeCaseOf[E ErrorClass](e E) causeCase {
	return causeCase{err: e, set: func(cause error) (error, bool) {
		got := WithCause(e, cause)
		return got, got.sdkError() == e.sdkError()
	}}
}

// everyClass builds one error of each class, all with the message "m".
func everyClass() []causeCase {
	return []causeCase{
		causeCaseOf(NewSDKError("m", false)),
		causeCaseOf(NewModelError("m", ModelCodeTimeout, true)),
		causeCaseOf(NewToolError("m", ToolCodeTimeout, "grep", false)),
		causeCaseOf(NewPolicyError("m", PolicyCodeInvalidArgs, "edit")),
		causeCaseOf(NewSessionError("m", SessionCodeCorrupted, false)),
		causeCaseOf(NewSkillError("m", SkillCodeInvalid, "review", false)),
		causeCaseOf(NewSandboxError("m", SandboxCodeTimeout, "sb")),
		causeCaseOf(NewArtifactError("m", ArtifactCodeConflict, "art")),
	}
}

func TestCauseIsReachableThroughEveryClass(t *testing.T) {
	urlErr := &url.Error{Op: "Post", URL: "https://example.test/?key=SECRET", Err: context.DeadlineExceeded}
	for _, c := range everyClass() {
		t.Run(fmt.Sprintf("%T", c.err), func(t *testing.T) {
			err, same := c.set(urlErr)
			if !same {
				t.Fatal("WithCause must return the error it was given")
			}
			// The cause never enters the text.
			if err.Error() != "m" {
				t.Errorf("Error() = %q, want %q", err.Error(), "m")
			}
			// Directly and through %w, the cause is reached by Is and As.
			for _, e := range []error{err, fmt.Errorf("turn 2: %w", err)} {
				if !errors.Is(e, context.DeadlineExceeded) {
					t.Errorf("errors.Is(%v, DeadlineExceeded) = false", e)
				}
				var got *url.Error
				if !errors.As(e, &got) || got != urlErr {
					t.Errorf("errors.As *url.Error failed for %v", e)
				}
				// The SDK matches still work: the class first, then its SDKError.
				var sdk *SDKError
				if !errors.As(e, &sdk) || sdk.Message != "m" {
					t.Errorf("errors.As *SDKError failed: %v", sdk)
				}
				if !errors.Is(e, &SDKError{}) {
					t.Error("the SDKError class-level wildcard no longer matches")
				}
			}
			// A nil cause clears it.
			if cleared, _ := c.set(nil); errors.Is(cleared, context.DeadlineExceeded) {
				t.Error("a nil cause must clear the cause")
			}
		})
	}
}

func TestWithCauseOnNilIsANoOp(t *testing.T) {
	var me *ModelError
	if got := WithCause(me, errors.New("x")); got != nil {
		t.Errorf("WithCause(nil) = %v, want nil", got)
	}
	var sdk *SDKError
	if got := WithCause(sdk, errors.New("x")); got != nil {
		t.Errorf("WithCause(nil *SDKError) = %v, want nil", got)
	}
}

func TestCauseDoesNotConfuseTheClassMatches(t *testing.T) {
	// A ToolError caused by a ModelError: the outer class wins errors.As, both
	// classes match errors.Is and HasCode, and retryability is the outer one's.
	inner := NewModelError("rate limited", ModelCodeRateLimit, true)
	outer := WithCause(NewToolError("tool failed", ToolCodeExecutionFailed, "web", false), inner)

	var sdk *SDKError
	if !errors.As(outer, &sdk) || sdk.Message != "tool failed" {
		t.Errorf("errors.As *SDKError found %q, want the outer error", sdk.Message)
	}
	if IsRetryable(outer) {
		t.Error("IsRetryable must read the outer error, which is not retryable")
	}
	var me *ModelError
	if !errors.As(outer, &me) || me != inner {
		t.Error("errors.As *ModelError must reach the cause")
	}
	if !HasCode(outer, ToolCodeExecutionFailed) || !HasCode(outer, ModelCodeRateLimit) {
		t.Error("HasCode must match the outer code and the code of the cause")
	}
	if HasCode(outer, ModelCodeAuth) || HasCode(outer, ToolCodeTimeout) {
		t.Error("HasCode matched a code that is not in the chain")
	}
}

func TestHasCode(t *testing.T) {
	type check struct {
		got, want bool
		what      string
	}
	wrapped := fmt.Errorf("ctx: %w", NewSessionError("m", SessionCodeNotFound, false))
	checks := []check{
		{HasCode(NewModelError("m", ModelCodeAuth, false), ModelCodeAuth), true, "model exact"},
		{HasCode(NewModelError("m", ModelCodeAuth, false), ModelCodeTimeout), false, "model other code"},
		{HasCode(NewModelError("m", ModelCodeAuth, false), ModelErrorCode("")), true, "model empty code is class level"},
		{HasCode(NewToolError("m", ToolCodeTimeout, "t", false), ToolCodeTimeout), true, "tool exact"},
		// The same string in two classes: the code type picks the class.
		{HasCode(NewToolError("m", ToolCodeTimeout, "t", false), ModelCodeTimeout), false, "tool timeout is not a model timeout"},
		{HasCode(NewToolError("m", ToolCodeNotFound, "t", false), SessionCodeNotFound), false, "tool not_found is not a session not_found"},
		{HasCode(NewPolicyError("m", PolicyCodeNotInAllowlist, "t"), PolicyCodeNotInAllowlist), true, "policy"},
		{HasCode(wrapped, SessionCodeNotFound), true, "session through %w"},
		{HasCode(NewSkillError("m", SkillCodeReadOnly, "s", false), SkillCodeReadOnly), true, "skill"},
		{HasCode(NewSandboxError("m", SandboxCodeConflict, ""), SandboxCodeConflict), true, "sandbox"},
		{HasCode(NewArtifactError("m", ArtifactCodeNotFound, ""), ArtifactCodeNotFound), true, "artifact"},
		{HasCode(NewSDKError("m", false), ModelCodeAuth), false, "base error has no code"},
		{HasCode(errors.New("plain"), ModelCodeAuth), false, "plain error"},
		{HasCode(nil, ModelCodeAuth), false, "nil"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: HasCode = %v, want %v", c.what, c.got, c.want)
		}
	}
}

func TestModelSentinels(t *testing.T) {
	sentinels := map[ModelErrorCode]error{
		ModelCodeAuth:           ErrModelAuth,
		ModelCodeRateLimit:      ErrModelRateLimit,
		ModelCodeTimeout:        ErrModelTimeout,
		ModelCodeInvalidRequest: ErrModelInvalidRequest,
		ModelCodeProvider:       ErrModelProvider,
	}
	for code, sentinel := range sentinels {
		err := fmt.Errorf("call: %w", NewModelError("m", code, false))
		for other, s := range sentinels {
			if got := errors.Is(err, s); got != (other == code) {
				t.Errorf("errors.Is(%s error, sentinel %s) = %v", code, other, got)
			}
		}
		if errors.Is(NewToolError("m", ToolCodeTimeout, "t", false), sentinel) {
			t.Errorf("a ToolError matched the model sentinel %s", code)
		}
		if sentinel.Error() == "" {
			t.Errorf("sentinel %s has an empty text", code)
		}
	}
}

func TestAsSandboxErrorKeepsTheCause(t *testing.T) {
	cause := fmt.Errorf("dial: %w", context.DeadlineExceeded)
	got := AsSandboxError(cause, "sb")
	if got.Message != `Sandbox "sb" is unavailable: dial: context deadline exceeded` {
		t.Errorf("message = %q", got.Message)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Error("AsSandboxError must keep the original failure as the cause")
	}
}

func TestModelErrorExtraFieldsAreNeverEncoded(t *testing.T) {
	me := WithCause(NewModelError("Rate limited", ModelCodeRateLimit, true), errors.New("SECRET cause"))
	me.StatusCode = 429
	me.RetryAfter = 7 * time.Second
	if me.Error() != "Rate limited" {
		t.Errorf("Error() = %q", me.Error())
	}
	b, err := jsonx.Marshal(me)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"Message":"Rate limited","Retryable":true,"Code":"rate_limit"}`; got != want {
		t.Errorf("encoding = %s, want %s", got, want)
	}
	if strings.Contains(string(b), "SECRET") {
		t.Error("the cause leaked into the encoding")
	}
}
