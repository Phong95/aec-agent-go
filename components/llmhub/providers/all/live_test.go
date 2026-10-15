package all_test

// Live provider tests (CONTEXT D3). They cover the "Anthropic live" and "Novita live"
// cases and add the same two checks for Bedrock and the ChatGPT subscription.
//
// Live tests make real (billed) API calls, so a key in the shell is not enough: they
// also need an explicit opt-in, which keeps go test offline:
//
//	RUN_LIVE_TESTS=1 go test ./llm/all/ -run TestLive -v
//
// They are the only tests of the SDK that use a provider's default HTTP client and so
// may reach a real endpoint, and only when opted in. CI never sets RUN_LIVE_TESTS.
// No test logs a variable value, a header or a full error text, because an error
// from a provider can echo a credential; a failure reports the ModelError code only.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	_ "github.com/Phong95/aec-agent-go/components/llmhub/providers/all"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel"
)

const (
	// liveTimeout bounds one live call; a live provider that hangs must not hang go test.
	liveTimeout = 2 * time.Minute

	// liveDefaultAnthropicModel and liveDefaultNovitaModel are the default models.
	// Providers retire model ids over time: override with ANTHROPIC_MODEL and
	// NOVITA_MODEL.
	liveDefaultAnthropicModel = "claude-opus-5-5"
	liveDefaultNovitaModel    = "deepseek/deepseek-v4-flash"
)

// liveEnv returns the value of name, or the fallback when it is unset or empty.
func liveEnv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// requireLive skips the test unless RUN_LIVE_TESTS=1 and every named variable is
// set. The skip message names the missing switch or variable and never a value.
func requireLive(t *testing.T, vars ...string) {
	t.Helper()
	if os.Getenv("RUN_LIVE_TESTS") != "1" {
		t.Skip("live test skipped: set RUN_LIVE_TESTS=1 to make real (billed) provider calls")
	}
	for _, v := range vars {
		if os.Getenv(v) == "" {
			t.Skipf("live test skipped: %s is not set", v)
		}
	}
}

// resolveLive builds the named provider through the registry with the default HTTP
// client of its leaf package.
func resolveLive(t *testing.T, name string, cfg llmhub.ProviderConfig) llmhub.Provider {
	t.Helper()
	p, err := llmhub.ResolveProvider(name, cfg)
	if err != nil {
		t.Fatalf("ResolveProvider(%q) failed", name)
	}
	return p
}

// failureCode names a provider failure by its ModelError code only, so a failing live
// test cannot write a credential that an upstream error text echoed.
func failureCode(err error) string {
	var me *kernel.ModelError
	if errors.As(err, &me) {
		return string(me.Code)
	}
	return "not a ModelError"
}

// liveMessages is the one-word prompt of the live tests.
func liveMessages() []kernel.Message {
	return []kernel.Message{{Role: kernel.RoleUser, Content: "Say hello in one word."}}
}

// assertLiveChat checks the shape of a Chat answer: an assistant message with content.
func assertLiveChat(t *testing.T, p llmhub.Provider, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), liveTimeout)
	defer cancel()
	res, err := p.Chat(ctx, liveMessages(), llmhub.ModelOptions{Model: model})
	if err != nil {
		t.Fatalf("Chat failed: %s", failureCode(err))
	}
	if res.Message.Role != kernel.RoleAssistant {
		t.Errorf("Chat role = %q, want assistant", res.Message.Role)
	}
	if len(res.Message.Content) == 0 {
		t.Error("Chat returned an assistant message with no content")
	}
}

// assertLiveStream checks the shape of a Stream: some chunks, no error chunk, the
// last chunk done.
func assertLiveStream(t *testing.T, p llmhub.Provider, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), liveTimeout)
	defer cancel()
	var chunks []kernel.StreamChunk
	for chunk, err := range p.Stream(ctx, liveMessages(), llmhub.ModelOptions{Model: model}) {
		if err != nil {
			t.Fatalf("Stream failed: %s", failureCode(err))
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) == 0 {
		t.Fatal("Stream yielded no chunks")
	}
	// The stream reports provider failures as an "error" chunk followed by "done".
	for _, c := range chunks {
		if c.Type == kernel.ChunkError {
			t.Fatal("Stream yielded an error chunk")
		}
	}
	if last := chunks[len(chunks)-1]; last.Type != kernel.ChunkDone {
		t.Errorf("last chunk type = %q, want done", last.Type)
	}
}

func TestLiveAnthropic(t *testing.T) {
	requireLive(t, "ANTHROPIC_API_KEY")
	model := liveEnv("ANTHROPIC_MODEL", liveDefaultAnthropicModel)
	newProvider := func(t *testing.T) llmhub.Provider {
		return resolveLive(t, "anthropic", llmhub.ProviderConfig{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: model})
	}

	t.Run("chat() returns a valid response", func(t *testing.T) {
		assertLiveChat(t, newProvider(t), model)
	})
	t.Run("chatStream() yields chunks", func(t *testing.T) {
		assertLiveStream(t, newProvider(t), model)
	})
	// The conformance suite runs on the live provider. The failing and blocking
	// factories stay nil, so the subtests that need them skip with their message: a live upstream cannot be made to fail or hang on demand.
	t.Run("conformance", func(t *testing.T) {
		conformance.Provider(t, newProvider, conformance.ProviderOptions{})
	})
}

func TestLiveNovita(t *testing.T) {
	requireLive(t, "NOVITA_API_KEY")
	model := liveEnv("NOVITA_MODEL", liveDefaultNovitaModel)
	newProvider := func(t *testing.T) llmhub.Provider {
		return resolveLive(t, "novita", llmhub.ProviderConfig{APIKey: os.Getenv("NOVITA_API_KEY"), Model: model})
	}

	t.Run("chat() returns a valid response", func(t *testing.T) {
		assertLiveChat(t, newProvider(t), model)
	})
	t.Run("chatStream() yields chunks", func(t *testing.T) {
		assertLiveStream(t, newProvider(t), model)
	})
}

// TestLiveBedrock needs the static AWS credentials (the provider never reads the AWS
// credential chain). AWS_REGION and BEDROCK_MODEL are optional: the provider's own
// defaults apply when they are unset.
func TestLiveBedrock(t *testing.T) {
	requireLive(t, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
	cfg := llmhub.ProviderConfig{
		APIKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		Region:    os.Getenv("AWS_REGION"),
		Model:     os.Getenv("BEDROCK_MODEL"),
	}
	newProvider := func(t *testing.T) llmhub.Provider { return resolveLive(t, "bedrock", cfg) }

	t.Run("chat() returns a valid response", func(t *testing.T) {
		assertLiveChat(t, newProvider(t), cfg.Model)
	})
	t.Run("chatStream() yields chunks", func(t *testing.T) {
		assertLiveStream(t, newProvider(t), cfg.Model)
	})
}

// TestLiveSubscription needs a ChatGPT access token and account id. The endpoint
// behaviour the adapter assumes is not verified against the live service yet
// (WINDOWS.md row 5 stays open until this test has run with real credentials).
func TestLiveSubscription(t *testing.T) {
	requireLive(t, "AEC_CHATGPT_TOKEN", "AEC_CHATGPT_ACCOUNT_ID")
	cfg := llmhub.ProviderConfig{
		AuthType:         llmhub.AuthOAuth,
		OAuthAccessToken: os.Getenv("AEC_CHATGPT_TOKEN"),
		ChatgptAccountID: os.Getenv("AEC_CHATGPT_ACCOUNT_ID"),
	}
	newProvider := func(t *testing.T) llmhub.Provider { return resolveLive(t, "openai-subscription", cfg) }

	t.Run("chat() returns a valid response", func(t *testing.T) {
		assertLiveChat(t, newProvider(t), cfg.Model)
	})
	t.Run("chatStream() yields chunks", func(t *testing.T) {
		assertLiveStream(t, newProvider(t), cfg.Model)
	})
}
