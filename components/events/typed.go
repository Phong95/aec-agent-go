package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Emit emits an event of kind k with payload p on b and returns the stored event.
func Emit[P any](b Bus, k Kind[P], sessionID string, p P) kernel.Event {
	return b.Emit(kernel.Event{Type: k.Name, SessionID: sessionID, Payload: p})
}

// Publish publishes an event of kind k with payload p on b with ctx (see
// Bus.Publish) and returns the stored event.
func Publish[P any](ctx context.Context, b Bus, k Kind[P], sessionID string, p P) kernel.Event {
	return b.Publish(ctx, kernel.Event{Type: k.Name, SessionID: sessionID, Payload: p})
}

// SubscribeCtx is Subscribe for a handler that needs the delivery's context (see
// CtxHandler). An event whose payload cannot be converted is logged and skipped.
func SubscribeCtx[P any](b Bus, k Kind[P], h func(context.Context, kernel.Event, P)) Unsubscribe {
	return b.OnCtx(k.Name, func(ctx context.Context, e kernel.Event) {
		p, err := PayloadAs[P](e)
		if err != nil {
			busLogger(b).Error("event payload not decodable", "type", e.Type, "error", err)
			return
		}
		h(ctx, e, p)
	})
}

// Subscribe registers h for events of kind k. The handler receives the payload
// already converted to P. An event whose payload cannot be converted is
// logged (type and error only, never the payload) and skipped. Like every
// Handler, h runs synchronously on the goroutine of the Emit and must not block.
func Subscribe[P any](b Bus, k Kind[P], h func(kernel.Event, P)) Unsubscribe {
	return b.On(k.Name, func(e kernel.Event) {
		p, err := PayloadAs[P](e)
		if err != nil {
			busLogger(b).Error("event payload not decodable", "type", e.Type, "error", err)
			return
		}
		h(e, p)
	})
}

// busLogger returns the logger the bus was configured with (Dispatcher.Logger),
// or slog.Default() for a Bus that does not expose one.
func busLogger(b Bus) *slog.Logger {
	if l, ok := b.(interface{ Logger() *slog.Logger }); ok {
		if lg := l.Logger(); lg != nil {
			return lg
		}
	}
	return slog.Default()
}

// PayloadAs converts the payload of e to P. It accepts a live P, a non-nil *P,
// json.RawMessage, []byte and map[string]any (the shape of an event decoded
// from a durable store). The last three are decoded strictly: unknown fields,
// wrong types, malformed JSON, a JSON null and trailing data are errors. A nil
// payload, a nil *P and any other dynamic type are errors too; PayloadAs never
// panics.
//
// A map[string]any is re-encoded before decoding, so numbers pass through
// float64 (an integer above 2^53 loses precision) and object keys come back
// sorted, which also alters the exact bytes of a kernel.Arguments field. A
// durable bus that must keep those bytes should hand out events whose payload
// is a json.RawMessage; DecodeEvent builds such an event from stored JSON.
func PayloadAs[P any](e kernel.Event) (P, error) {
	var zero P
	switch v := e.Payload.(type) {
	case nil:
		return zero, errors.New("events: payload is nil")
	case P:
		return v, nil
	case *P:
		if v == nil {
			return zero, errors.New("events: payload is a nil pointer")
		}
		return *v, nil
	case json.RawMessage:
		return decodePayload[P](v)
	case []byte:
		return decodePayload[P](v)
	case map[string]any:
		raw, err := jsonx.Marshal(v)
		if err != nil {
			return zero, fmt.Errorf("events: re-encode map payload: %w", err)
		}
		return decodePayload[P](raw)
	default:
		return zero, fmt.Errorf("events: payload of type %T is not a %T", e.Payload, zero)
	}
}

func decodePayload[P any](raw []byte) (P, error) {
	var p P
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return p, errors.New("events: payload is JSON null")
	}
	if err := jsonx.UnmarshalStrict(raw, &p); err != nil {
		var zero P
		return zero, fmt.Errorf("events: decode payload as %T: %w", p, err)
	}
	return p, nil
}

// DecodeEvent decodes one stored event (the JSON of a kernel.Event) and keeps
// the payload as a json.RawMessage instead of decoding it into map[string]any.
// PayloadAs then decodes the payload straight from the original bytes, so key
// order, number spellings and the exact bytes of a kernel.Arguments field
// survive (a map[string]any payload loses them; see PayloadAs). Durable Bus
// implementations should use it when loading events from storage. The decode
// is strict: unknown fields and trailing data are errors.
func DecodeEvent(data []byte) (kernel.Event, error) {
	var env struct {
		Type      string          `json:"type"`
		SessionID string          `json:"sessionId"`
		Payload   json.RawMessage `json:"payload"`
		ID        string          `json:"id"`
		Timestamp int64           `json:"timestamp"`
	}
	if err := jsonx.UnmarshalStrict(data, &env); err != nil {
		return kernel.Event{}, fmt.Errorf("events: decode event: %w", err)
	}
	e := kernel.Event{Type: env.Type, SessionID: env.SessionID, ID: env.ID, Timestamp: env.Timestamp}
	if len(env.Payload) > 0 {
		e.Payload = env.Payload
	}
	return e, nil
}
