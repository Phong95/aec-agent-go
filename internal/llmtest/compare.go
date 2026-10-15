package llmtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// CanonicalJSON returns b re-encoded with object keys sorted and numbers kept
// exactly as written (no float64 rounding), so two JSON texts that differ only in
// key order or whitespace compare equal. It fails on invalid JSON and on trailing
// data after the first value.
func CanonicalJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("llmtest: trailing data after the JSON value")
	}
	return jsonx.Marshal(v)
}

// AssertJSONEqual fails the test, with both texts, unless got and want are the
// same JSON value (key order and whitespace aside).
func AssertJSONEqual(t testing.TB, got, want []byte) {
	t.Helper()
	g, err := CanonicalJSON(got)
	if err != nil {
		t.Fatalf("llmtest: got is not JSON: %v\n got: %s", err, got)
	}
	w, err := CanonicalJSON(want)
	if err != nil {
		t.Fatalf("llmtest: want is not JSON: %v\nwant: %s", err, want)
	}
	if !bytes.Equal(g, w) {
		t.Fatalf("llmtest: JSON differs\n got: %s\nwant: %s", g, w)
	}
}

// ChunkTexts encodes every chunk with kernel.EncodeJSON, the wire encoding, so a
// test compares a stream with the recorded fixture chunk list as plain strings.
func ChunkTexts(t testing.TB, chunks []kernel.StreamChunk) []string {
	t.Helper()
	out := make([]string, len(chunks))
	for i, c := range chunks {
		b, err := kernel.EncodeJSON(c)
		if err != nil {
			t.Fatalf("llmtest: encode chunk %d: %v", i, err)
		}
		out[i] = string(b)
	}
	return out
}
