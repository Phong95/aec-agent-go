package secrethub

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// ---------------------------------------------------------------------------
// SecretHub providers (A-101) - who holds the master keys. A master key never encrypts a value
// directly: it wraps (encrypts) the per-value data key. A KMS provider sends the data key
// to the KMS and keeps only the wrapped form, so the master key never leaves the KMS.
// ---------------------------------------------------------------------------

// WrappedKey is a wrapped data key and the master key that wrapped it.
type WrappedKey struct {
	KeyID string
	// Wrapped is opaque bytes only the provider can unwrap.
	Wrapped []byte
}

// SecretHubProvider is the port a host implements for its key management: the SDK ships
// LocalSecretHubProvider; hosts plug in AWS KMS, GCP KMS, Azure Key Vault or Vault Transit
// through AgentCore.UseSecretHubProvider. An implementation must be safe for concurrent use.
type SecretHubProvider interface {
	// ID names the provider in status reports ("local", "aws-kms"...).
	ID() string
	// CurrentKeyID is the master key new data keys are wrapped with.
	CurrentKeyID() string
	// WrapKey wraps a data key with the current master key.
	WrapKey(ctx context.Context, dataKey []byte) (WrappedKey, error)
	// UnwrapKey unwraps a data key wrapped by keyID - the current key or an older one the
	// provider still holds (rotation). It fails when it doesn't hold that key or the bytes
	// are bad.
	UnwrapKey(ctx context.Context, wrapped []byte, keyID string) ([]byte, error)
}

// LegacySecretHubProvider is the optional part of a SecretHubProvider that opens the A-100 single-key
// format (enc:v1: AES-256-GCM, key = SHA-256(secret)), so values written before A-101 stay
// readable (the optional legacyKeys of core/ts).
type LegacySecretHubProvider interface {
	// LegacyKeys returns the AES keys that may have written enc:v1 values.
	LegacyKeys() [][]byte
}

const (
	ivBytes  = 12
	tagBytes = 16
)

// GCMSeal is AES-256-GCM with a random iv: iv[12] | ciphertext | tag[16] - the layout every
// format here uses. An empty aad binds nothing.
func GCMSeal(key, plain, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, ivBytes)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	return gcm.Seal(iv, iv, plain, aad), nil
}

// GCMOpen opens GCMSeal's output; it fails when the key, the AAD or the bytes don't
// match.
func GCMOpen(key, sealed, aad []byte) ([]byte, error) {
	if len(sealed) < ivBytes+tagBytes {
		return nil, errors.New("sealed data is too short")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, sealed[:ivBytes], sealed[ivBytes:], aad)
}

// DeriveKey is SHA-256 of a master secret - the AES key LocalSecretHubProvider (and the enc:v1
// format) uses.
func DeriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// LocalKeyID is the id of a local master secret: "local:" + 12 hex characters of a hash
// that reveals nothing about it.
func LocalKeyID(secret string) string {
	sum := sha256.Sum256([]byte("aec-key-id:" + secret))
	return "local:" + hex.EncodeToString(sum[:])[:12]
}

// LocalSecretHubProviderOptions configure a LocalSecretHubProvider.
type LocalSecretHubProviderOptions struct {
	// Current is the master secret new data keys are wrapped with (e.g. AEC_SECRET_KEY).
	Current string
	// Previous are older master secrets, still accepted for reading until a rotation
	// re-seals their values.
	Previous []string
}

// LocalSecretHubProvider holds master keys in the host process - derived from secrets it is
// given (environment variables). Each secret's id is derived from it, so rotating is: make
// the new secret Current, move the old one to Previous, run the rotation, then drop the old
// one. It is immutable and safe for concurrent use.
type LocalSecretHubProvider struct {
	currentID string
	keys      map[string][]byte
	order     []string // key ids, current first, for LegacyKeys
}

var (
	_ SecretHubProvider       = (*LocalSecretHubProvider)(nil)
	_ LegacySecretHubProvider = (*LocalSecretHubProvider)(nil)
)

// NewLocalSecretHubProvider returns a provider over the given master secrets. A blank Current is
// an error.
func NewLocalSecretHubProvider(o LocalSecretHubProviderOptions) (*LocalSecretHubProvider, error) {
	current := jsstr.Trim(o.Current)
	if current == "" {
		return nil, errors.New("LocalSecretHubProvider needs a master secret")
	}
	p := &LocalSecretHubProvider{currentID: LocalKeyID(current), keys: map[string][]byte{}}
	p.add(current)
	for _, s := range o.Previous {
		if s = jsstr.Trim(s); s != "" {
			p.add(s)
		}
	}
	return p, nil
}

func (p *LocalSecretHubProvider) add(secret string) {
	id := LocalKeyID(secret)
	if _, ok := p.keys[id]; ok {
		return
	}
	p.keys[id] = DeriveKey(secret)
	p.order = append(p.order, id)
}

// NewEphemeralSecretHubProvider returns a provider with a random master key that lives as long
// as the process (tests, in-memory stores) - LocalSecretHubProvider.ephemeral() of core/ts.
func NewEphemeralSecretHubProvider() *LocalSecretHubProvider {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error (Go 1.24+)
	p, _ := NewLocalSecretHubProvider(LocalSecretHubProviderOptions{Current: base64.StdEncoding.EncodeToString(raw)})
	return p
}

// ID returns "local".
func (*LocalSecretHubProvider) ID() string { return "local" }

// CurrentKeyID returns the id of the current master secret.
func (p *LocalSecretHubProvider) CurrentKeyID() string { return p.currentID }

// WrapKey wraps a data key with the current master key (AES-256-GCM, no AAD).
func (p *LocalSecretHubProvider) WrapKey(_ context.Context, dataKey []byte) (WrappedKey, error) {
	wrapped, err := GCMSeal(p.keys[p.currentID], dataKey, nil)
	if err != nil {
		return WrappedKey{}, err
	}
	return WrappedKey{KeyID: p.currentID, Wrapped: wrapped}, nil
}

// UnwrapKey unwraps a data key wrapped by keyID, the current key or a previous one.
func (p *LocalSecretHubProvider) UnwrapKey(_ context.Context, wrapped []byte, keyID string) ([]byte, error) {
	key, ok := p.keys[keyID]
	if !ok {
		return nil, errors.New(`master key "` + keyID + `" is not configured`)
	}
	return GCMOpen(key, wrapped, nil)
}

// LegacyKeys returns the derived keys of every configured master secret, current first:
// an enc:v1 value was sealed with SHA-256(AEC_SECRET_KEY).
func (p *LocalSecretHubProvider) LegacyKeys() [][]byte {
	out := make([][]byte, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, append([]byte(nil), p.keys[id]...))
	}
	return out
}
