package bedrock

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	smithydoc "github.com/aws/smithy-go/document"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// toDocument turns the JSON text of an object into the smithy document the Bedrock
// API takes for a tool call's input and a tool's input schema.
//
// The text is decoded with UseNumber and every json.Number becomes a
// smithy document.Number, so the spelling of a number (1.50, 1e21, a 20-digit
// integer) is written back exactly. The raw bytes are NEVER handed to the lazy
// document: it would encode a []byte as an array of integers
// ("input":[123,34,...]), which Bedrock rejects.
//
// The AWS encoder writes the keys of a map in sorted order, and there is no way to
// supply an ordered type (the document interface is sealed), so the request carries
// tool inputs and schemas with sorted keys rather than in insertion order. The
// model sees the same JSON; only the key order differs.
func toDocument(raw []byte) (document.Interface, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("bedrock: trailing data after the JSON value")
	}
	return document.NewLazyDocument(numbersToDocument(v)), nil
}

// numbersToDocument replaces every json.Number in v with a document.Number,
// through maps and slices.
func numbersToDocument(v any) any {
	switch x := v.(type) {
	case json.Number:
		return smithydoc.Number(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = numbersToDocument(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = numbersToDocument(e)
		}
		return out
	default:
		return v
	}
}

// fromDocument returns the JSON text of a document the API sent back (the input of
// a tool call in a Converse response). The AWS decoder reads every number as a
// float64 and every object as a map, so the text is written with jsonx: JS number
// formatting, no HTML escaping, keys sorted. The numbers are the doubles the
// response carried; the keys are sorted, because a Go map has no order.
func fromDocument(doc document.Interface) ([]byte, error) {
	var v any
	if err := doc.UnmarshalSmithyDocument(&v); err != nil {
		return nil, err
	}
	return jsonx.Marshal(v)
}
