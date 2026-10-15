package docker

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// fingerprintResources and fingerprintInput are the object hashed as compact JSON, in a
// fixed key order. The order is the interop contract: one different byte gives another
// fingerprint, and a container created earlier with the same settings would be
// recreated (its volume survives, its running state does not).
type fingerprintResources struct {
	Cpus     float64 `json:"cpus"`
	MemoryMb int     `json:"memoryMb"`
	Pids     int     `json:"pids"`
}

type fingerprintInput struct {
	V         int                  `json:"v"`
	Image     string               `json:"image"`
	Network   string               `json:"network"`
	Resources fingerprintResources `json:"resources"`
	Tmpfs     string               `json:"tmpfs"`
	User      string               `json:"user"`
}

// fingerprintText is the JSON text that gets hashed: jsonx.Marshal writes numbers the
// ES6 way JSON.stringify does (1e+21, 1e-7, 0.5).
func fingerprintText(in fingerprintInput) (string, error) {
	b, err := jsonx.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// fingerprintOf is the first 16 hex characters of the sha256 of the text.
func fingerprintOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// jsNumberText is String(n) for a finite float64: the text jsonx.Marshal writes,
// which is the ES6 Number::toString form (used for --cpus).
func jsNumberText(n float64) (string, error) {
	b, err := jsonx.Marshal(n)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
