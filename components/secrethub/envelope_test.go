package secrethub_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
)

const apiKey = "sk-live-0123456789abcdef"

func localProvider(t *testing.T, current string, previous ...string) *secrethub.LocalSecretHubProvider {
	t.Helper()
	p, err := secrethub.NewLocalSecretHubProvider(secrethub.LocalSecretHubProviderOptions{Current: current, Previous: previous})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sealerOf(p secrethub.SecretHubProvider) *secrethub.Sealer {
	return secrethub.NewSealer(func() secrethub.SecretHubProvider { return p }, secrethub.SealerOptions{})
}

func isCode(err error, code secret.ErrorCode) bool {
	return errors.Is(err, &secret.Error{Code: code})
}

func TestSealerSealsWithEnvelopeEncryption(t *testing.T) {
	ctx := t.Context()
	p := localProvider(t, "master-one")
	s := sealerOf(p)
	sealed, err := s.Seal(ctx, apiKey, "secret:global:K")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "enc:v2:") || strings.Contains(sealed, apiKey) {
		t.Fatalf("sealed = %q", sealed)
	}
	if f := secrethub.SealedFormatOf(sealed); f.Format != secrethub.FormatV2 || f.KeyID != p.CurrentKeyID() {
		t.Fatalf("format = %+v", f)
	}
	if !regexp.MustCompile(`^local:[0-9a-f]{12}$`).MatchString(p.CurrentKeyID()) {
		t.Fatalf("key id = %q", p.CurrentKeyID())
	}
	if got, err := s.Open(ctx, sealed, "secret:global:K"); err != nil || got != apiKey {
		t.Fatalf("Open = %q, %v", got, err)
	}
	// Two seals of one value differ (fresh data key and iv).
	a, _ := s.Seal(ctx, apiKey, "x")
	b, _ := s.Seal(ctx, apiKey, "x")
	if a == b {
		t.Fatal("two seals must differ")
	}
}

func TestSealerRefusesAMovedValueOrAnUnknownKey(t *testing.T) {
	ctx := t.Context()
	s := sealerOf(localProvider(t, "master-one"))
	sealed, _ := s.Seal(ctx, apiKey, "secret:global:A")
	if _, err := s.Open(ctx, sealed, "secret:global:B"); !isCode(err, secret.CodeDecryptFailed) {
		t.Fatalf("moved value: %v", err)
	}
	if _, err := sealerOf(localProvider(t, "master-two")).Open(ctx, sealed, "secret:global:A"); !isCode(err, secret.CodeDecryptFailed) {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestSealerOpensAPreviousKeyAndFlagsItForResealing(t *testing.T) {
	ctx := t.Context()
	sealed, _ := sealerOf(localProvider(t, "old-master")).Seal(ctx, "value-1", "aad")
	rotated := sealerOf(localProvider(t, "new-master", "old-master"))
	if got, err := rotated.Open(ctx, sealed, "aad"); err != nil || got != "value-1" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if !rotated.NeedsReseal(sealed) {
		t.Fatal("a value under the old key needs re-sealing")
	}
	fresh, _ := rotated.Seal(ctx, "value-1", "aad")
	if rotated.NeedsReseal(fresh) {
		t.Fatal("a value under the current key needs no re-sealing")
	}
}

func TestSealerReadsEncV1AndPlainText(t *testing.T) {
	ctx := t.Context()
	// enc:v1 = base64(iv | ciphertext | tag), key = SHA-256(AEC_SECRET_KEY), no AAD.
	key := sha256.Sum256([]byte("legacy-secret"))
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	v1 := "enc:v1:" + base64.StdEncoding.EncodeToString(gcm.Seal(iv, iv, []byte("bot-token"), nil))
	s := sealerOf(localProvider(t, "legacy-secret"))
	if got, err := s.Open(ctx, v1, ""); err != nil || got != "bot-token" {
		t.Fatalf("enc:v1 = %q, %v", got, err)
	}
	if got, _ := s.Open(ctx, "plain-token", ""); got != "plain-token" {
		t.Fatalf("plain = %q", got)
	}
	if _, err := sealerOf(nil).Open(ctx, v1, ""); !isCode(err, secret.CodeDecryptFailed) {
		t.Fatalf("enc:v1 without a key: %v", err)
	}
}

func TestSealerStoresPlainTextWithoutAProviderWarningOnce(t *testing.T) {
	var buf bytes.Buffer
	s := secrethub.NewSealer(func() secrethub.SecretHubProvider { return nil }, secrethub.SealerOptions{Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	for _, v := range []string{"v", "w"} {
		if got, err := s.Seal(t.Context(), v, ""); err != nil || got != v {
			t.Fatalf("Seal = %q, %v", got, err)
		}
	}
	if n := strings.Count(buf.String(), "stored unencrypted"); n != 1 {
		t.Fatalf("warned %d times, want 1:\n%s", n, buf.String())
	}
}

// vector is one entry of tests/testdata/secrets/vector.json.
type vector struct {
	WrittenBy string `json:"writtenBy"`
	Master    string `json:"master"`
	KeyID     string `json:"keyId"`
	AAD       string `json:"aad"`
	Plain     string `json:"plain"`
	Sealed    string `json:"sealed"`
}

// TestSharedTestVectors opens the vector core/ts sealed and the one this SDK sealed: both
// SDKs and both servers decrypt the same strings (tests/unit/secrets.test.ts of core/ts
// holds the same vectors).
func TestSharedTestVectors(t *testing.T) {
	raw, err := os.ReadFile("../../tests/testdata/secrets/vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors map[string]vector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 {
		t.Fatalf("want the ts and go vectors, got %d", len(vectors))
	}
	for name, v := range vectors {
		t.Run(name, func(t *testing.T) {
			p := localProvider(t, v.Master)
			if p.CurrentKeyID() != v.KeyID {
				t.Fatalf("key id = %q, want %q", p.CurrentKeyID(), v.KeyID)
			}
			got, err := sealerOf(p).Open(t.Context(), v.Sealed, v.AAD)
			if err != nil || got != v.Plain {
				t.Fatalf("Open = %q, %v; want %q", got, err, v.Plain)
			}
			if f := secrethub.SealedFormatOf(v.Sealed); f.Format != secrethub.FormatV2 || f.KeyID != v.KeyID {
				t.Fatalf("format = %+v", f)
			}
		})
	}
}
