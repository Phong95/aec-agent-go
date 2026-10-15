package secrethub

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
)

// ---------------------------------------------------------------------------
// Envelope encryption (A-101). A sealed value is
//
//   "enc:v2:" + base64url(keyId) + ":" + base64(wrapped data key) + ":" + base64(iv | ciphertext | tag)
//
// The value is encrypted with a fresh 32-byte data key (AES-256-GCM, the caller's AAD
// bound in, so a value can't be moved to another secret); the data key is wrapped by the
// provider's current master key. core/ts and both servers read and write the same format
// (base64url without padding for the key id, standard base64 with padding for the rest).
//
// Also read: "enc:v1:" + base64(iv | ciphertext | tag) - the A-100 single-key format
// (key = SHA-256(AEC_SECRET_KEY), no AAD) - and plain text (no "enc:" prefix), so values
// written before A-101 open until a rotation re-seals them.
// ---------------------------------------------------------------------------

const (
	prefixV2     = "enc:v2:"
	prefixV1     = "enc:v1:"
	dataKeyBytes = 32
)

// Format is how a stored value is sealed.
type Format string

// The formats of a stored value.
const (
	FormatV2    Format = "v2"
	FormatV1    Format = "v1"
	FormatPlain Format = "plain"
)

// SealedFormat is what a sealed value's prefix says about it. KeyID is set for FormatV2.
type SealedFormat struct {
	Format Format
	KeyID  string
}

// SealedFormatOf reads the format (and, for enc:v2, the master key id) of a stored value.
func SealedFormatOf(stored string) SealedFormat {
	if rest, ok := strings.CutPrefix(stored, prefixV2); ok {
		keyPart, _, _ := strings.Cut(rest, ":")
		return SealedFormat{Format: FormatV2, KeyID: string(decodeBase64URL(keyPart))}
	}
	if strings.HasPrefix(stored, prefixV1) {
		return SealedFormat{Format: FormatV1}
	}
	return SealedFormat{Format: FormatPlain}
}

// SealerOptions configure a Sealer.
type SealerOptions struct {
	// LegacyKeys returns extra keys for the enc:v1 format, beyond the provider's
	// LegacyKeys. Nil means none.
	LegacyKeys func() [][]byte
	// CacheTTL is how long an unwrapped data key is kept in memory. 0 means the default
	// (5 minutes); a negative value turns the cache off.
	CacheTTL time.Duration
	// CacheMax is the most data keys kept at once. 0 means the default (1000).
	CacheMax int
	// Logger receives the one warning about values stored without encryption. Nil means
	// slog.Default().
	Logger *slog.Logger
}

type cachedKey struct {
	key     []byte
	expires time.Time
}

// Sealer seals and opens values with the current SecretHubProvider (resolved on every call, so
// AgentCore.UseSecretHubProvider applies at once) - the SecretSealer of core/ts. Without a
// provider values are stored as plain text and a warning is logged once; hosts that must
// not do that refuse to start without one (see the servers' AEC_REQUIRE_ENCRYPTION). It is
// safe for concurrent use.
type Sealer struct {
	getProvider func() SecretHubProvider
	legacyKeys  func() [][]byte
	ttl         time.Duration
	max         int
	logger      *slog.Logger

	mu     sync.Mutex
	cache  map[string]cachedKey
	order  []string // cache keys in insertion order, the oldest first
	warned bool
}

// NewSealer returns a sealer over the provider getProvider returns on every call (nil
// means none).
func NewSealer(getProvider func() SecretHubProvider, o SealerOptions) *Sealer {
	s := &Sealer{getProvider: getProvider, legacyKeys: o.LegacyKeys, ttl: o.CacheTTL, max: o.CacheMax, logger: o.Logger, cache: map[string]cachedKey{}}
	if s.ttl == 0 {
		s.ttl = 5 * time.Minute
	}
	if s.max <= 0 {
		s.max = 1000
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

// Provider returns the provider in use, or nil.
func (s *Sealer) Provider() SecretHubProvider {
	if s.getProvider == nil {
		return nil
	}
	p := s.getProvider()
	if nilcheck.IsNil(p) {
		return nil
	}
	return p
}

// Seal seals plain under the current master key; aad binds the value to where it is
// stored. Without a provider it returns plain (warned once). A provider failure is an
// *secret.Error ("unavailable").
func (s *Sealer) Seal(ctx context.Context, plain, aad string) (string, error) {
	provider := s.Provider()
	if provider == nil {
		s.mu.Lock()
		warn := !s.warned
		s.warned = true
		s.mu.Unlock()
		if warn {
			s.logger.Warn("secrets: no SecretHub provider is set " + emDash + " secrets are stored unencrypted")
		}
		return plain, nil
	}
	dataKey := make([]byte, dataKeyBytes)
	if _, err := rand.Read(dataKey); err != nil {
		return "", secret.NewError("could not make a data key: "+err.Error(), secret.CodeUnavailable)
	}
	wrapped, err := provider.WrapKey(ctx, dataKey)
	if err != nil {
		return "", secret.NewError(`the SecretHub provider "`+provider.ID()+`" could not wrap a key: `+err.Error(), secret.CodeUnavailable)
	}
	body, err := GCMSeal(dataKey, []byte(plain), aadBytes(aad))
	if err != nil {
		return "", secret.NewError("could not seal a secret: "+err.Error(), secret.CodeUnavailable)
	}
	return prefixV2 +
		base64.RawURLEncoding.EncodeToString([]byte(wrapped.KeyID)) + ":" +
		base64.StdEncoding.EncodeToString(wrapped.Wrapped) + ":" +
		base64.StdEncoding.EncodeToString(body), nil
}

// Open opens a value written by Seal (or the enc:v1 format, or plain text). It returns an
// *secret.Error ("decrypt_failed").
func (s *Sealer) Open(ctx context.Context, stored, aad string) (string, error) {
	if strings.HasPrefix(stored, prefixV2) {
		return s.openV2(ctx, stored, aad)
	}
	if strings.HasPrefix(stored, prefixV1) {
		return s.openV1(stored)
	}
	return stored, nil
}

// NeedsReseal reports whether the value is not sealed with the current master key (a
// rotation re-seals it). Without a provider nothing needs re-sealing.
func (s *Sealer) NeedsReseal(stored string) bool {
	provider := s.Provider()
	if provider == nil {
		return false
	}
	f := SealedFormatOf(stored)
	return f.Format != FormatV2 || f.KeyID != provider.CurrentKeyID()
}

// ClearCache forgets the unwrapped data keys (after a rotation, or to drop them from
// memory).
func (s *Sealer) ClearCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.cache)
	s.order = nil
}

func (s *Sealer) openV2(ctx context.Context, stored, aad string) (string, error) {
	parts := strings.Split(stored[len(prefixV2):], ":")
	if len(parts) != 3 {
		return "", secret.NewError("a stored secret is malformed", secret.CodeDecryptFailed)
	}
	keyID := string(decodeBase64URL(parts[0]))
	dataKey, err := s.unwrap(ctx, keyID, parts[1])
	if err != nil {
		return "", err
	}
	plain, err := GCMOpen(dataKey, decodeBase64(parts[2]), aadBytes(aad))
	if err != nil {
		return "", secret.NewError("a stored secret could not be decrypted (it was moved, or the data is damaged)", secret.CodeDecryptFailed)
	}
	return string(plain), nil
}

func (s *Sealer) unwrap(ctx context.Context, keyID, wrappedPart string) ([]byte, error) {
	cacheKey := keyID + ":" + wrappedPart
	s.mu.Lock()
	hit, ok := s.cache[cacheKey]
	s.mu.Unlock()
	if ok && hit.expires.After(time.Now()) {
		return hit.key, nil
	}
	provider := s.Provider()
	if provider == nil {
		return nil, secret.NewError(`a SecretHub provider holding master key "`+keyID+`" is needed to read this secret`, secret.CodeDecryptFailed)
	}
	key, err := provider.UnwrapKey(ctx, decodeBase64(wrappedPart), keyID)
	if err != nil {
		return nil, secret.NewError(`master key "`+keyID+`" could not unwrap this secret's key: `+err.Error(), secret.CodeDecryptFailed)
	}
	if s.ttl > 0 {
		s.mu.Lock()
		if _, present := s.cache[cacheKey]; !present {
			for len(s.cache) >= s.max && len(s.order) > 0 {
				delete(s.cache, s.order[0])
				s.order = s.order[1:]
			}
			s.order = append(s.order, cacheKey)
		}
		s.cache[cacheKey] = cachedKey{key: key, expires: time.Now().Add(s.ttl)}
		s.mu.Unlock()
	}
	return key, nil
}

func (s *Sealer) openV1(stored string) (string, error) {
	var keys [][]byte
	if lp, ok := s.Provider().(LegacySecretHubProvider); ok {
		keys = append(keys, lp.LegacyKeys()...)
	}
	if s.legacyKeys != nil {
		keys = append(keys, s.legacyKeys()...)
	}
	if len(keys) == 0 {
		return "", secret.NewError("AEC_SECRET_KEY is needed to read a secret stored before A-101 (enc:v1)", secret.CodeDecryptFailed)
	}
	raw := decodeBase64(stored[len(prefixV1):])
	for _, key := range keys {
		if plain, err := GCMOpen(key, raw, nil); err == nil {
			return string(plain), nil
		}
		// try the next key
	}
	return "", secret.NewError("a stored secret (enc:v1) could not be decrypted "+emDash+" AEC_SECRET_KEY differs from the one that wrote it", secret.CodeDecryptFailed)
}

// aadBytes is the AAD of a non-empty aad, nil (no AAD) for "".
func aadBytes(aad string) []byte {
	if aad == "" {
		return nil
	}
	return []byte(aad)
}

// decodeBase64URL decodes base64url as leniently as Node's Buffer.from(s, "base64url"):
// padding is optional and the standard alphabet is accepted too. Undecodable input gives
// nil.
func decodeBase64URL(s string) []byte {
	trimmed := strings.TrimRight(s, "=")
	if b, err := base64.RawURLEncoding.DecodeString(trimmed); err == nil {
		return b
	}
	if b, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil {
		return b
	}
	return nil
}

// decodeBase64 decodes standard base64, padded or not. Undecodable input gives nil (which
// then fails to open).
func decodeBase64(s string) []byte {
	return decodeBase64URL(s)
}

// emDash is U+2014, written as an escape so this source stays ASCII.
const emDash = "—"
