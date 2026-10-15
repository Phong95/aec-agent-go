package kernel

import "github.com/Phong95/aec-agent-go/internal/jsonx"

// EncodeJSON encodes v as compact JSON byte-identical to what JSON.stringify
// produces: no HTML escaping, raw U+2028/U+2029, no trailing
// newline.
//
// It is the wire-exact entry point for code outside this module: server/go is
// a separate module and cannot import internal/jsonx. Plain json.Marshal of a
// kernel type is semantically equal but not byte-equal (it escapes '<').
func EncodeJSON(v any) ([]byte, error) {
	return jsonx.Marshal(v)
}
