package toolhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/kernel"
)

// DefaultPolicy is the built-in Policy: the tool must be in the agent's
// allowlist, the arguments must match the shallow parameter schema, and the
// optional authorization hook must agree. The messages are fixed (they are
// pinned by recorded fixtures). The zero value is a usable policy without a hook.
type DefaultPolicy struct {
	hook AuthorizationHook
}

var _ Policy = (*DefaultPolicy)(nil)

// NewDefaultPolicy returns a DefaultPolicy.
func NewDefaultPolicy(opts DefaultPolicyOptions) *DefaultPolicy {
	return &DefaultPolicy{hook: opts.AuthorizationHook}
}

// Name returns policy:default.
func (*DefaultPolicy) Name() string { return "policy:default" }

// Version returns 1.0.0.
func (*DefaultPolicy) Version() string { return "1.0.0" }

// Initialize does nothing.
func (*DefaultPolicy) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*DefaultPolicy) Dispose(context.Context) error { return nil }

// errArgsNotObject is the denial for arguments that are not a JSON object
// (such as null or a string): the policy fails closed instead of crashing.
const errArgsNotObject = "Arguments must be a JSON object"

// Check runs the three stages in order: allowlist, schema, hook. It never
// modifies args: the schema stage decodes a copy.
func (p *DefaultPolicy) Check(ctx context.Context, toolName string, args kernel.Arguments, tc Context, def Definition, allowedTools []string) Decision {
	// 1. Allowlist - tool must be in the agent's allowed set
	if d := checkAllowlist(toolName, allowedTools); !d.Allowed {
		return d
	}

	// 2. Schema validation - args must match the tool's parameter schema
	if d := validateSchema(args, def.Parameters); !d.Allowed {
		return d
	}

	// 3. Authorization hook - consumer-provided business rules
	if p.hook != nil {
		if d := p.runHook(ctx, toolName, args, tc); !d.Allowed {
			return d
		}
	}

	return Allow()
}

// runHook calls the hook with its own copy of the arguments, so a hook that
// writes into the slice cannot change the bytes that are executed and
// persisted. A hook error or panic is a denial (fail closed).
func (p *DefaultPolicy) runHook(ctx context.Context, toolName string, args kernel.Arguments, tc Context) (d Decision) {
	defer func() {
		if r := recover(); r != nil {
			d = Deny("Authorization hook failed: " + fmt.Sprint(r))
		}
	}()
	res, err := p.hook(ctx, toolName, bytes.Clone(args), tc)
	if err != nil {
		return Deny("Authorization hook failed: " + err.Error())
	}
	if !res.Allowed {
		return res
	}
	return Allow()
}

func checkAllowlist(toolName string, allowedTools []string) Decision {
	if len(allowedTools) == 0 {
		return Allow()
	}
	for _, name := range allowedTools {
		if name == toolName {
			return Allow()
		}
	}
	return Deny(fmt.Sprintf(`Tool "%s" is not in the allowed list`, toolName))
}

// decodeObject decodes a copy of args into a generic map, with numbers kept as
// json.Number so no precision is lost. It reads args and never writes to it.
// Empty arguments count as {}.
func decodeObject(args kernel.Arguments) (map[string]any, error) {
	if len(bytes.Trim(args, " \t\r\n")) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON value")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}

func validateSchema(args kernel.Arguments, schema JSONSchema) Decision {
	obj, err := decodeObject(args)
	if err != nil {
		return Deny(errArgsNotObject)
	}

	// Check required fields. Presence is key presence, so an explicit JSON null
	// passes this check (and fails the type check).
	for _, field := range schema.Required {
		if _, ok := obj[field]; !ok {
			return Deny(fmt.Sprintf(`Missing required argument: "%s"`, field))
		}
	}

	// Check property types and constraints, in property order.
	for key, propSchema := range schema.Properties.All() {
		value, ok := obj[key]
		if !ok {
			continue
		}
		if d := checkType(key, value, propSchema); !d.Allowed {
			return d
		}
	}

	return Allow()
}

func checkType(key string, value any, schema JSONSchema) Decision {
	actualType := jsType(value)

	switch schema.Type {
	case "array":
		if _, ok := value.([]any); !ok {
			return Deny(fmt.Sprintf(`Argument "%s" must be an array, got %s`, key, actualType))
		}
	case "object":
		// Only a JSON object is an object: null and an array are denied, and the reason
		// names them (got null, got array).
		switch value.(type) {
		case map[string]any:
		case nil:
			return Deny(fmt.Sprintf(`Argument "%s" must be object, got null`, key))
		case []any:
			return Deny(fmt.Sprintf(`Argument "%s" must be object, got array`, key))
		default:
			return Deny(fmt.Sprintf(`Argument "%s" must be object, got %s`, key, actualType))
		}
	case "integer":
		// A JSON number with no fractional part (1, 1.0, 1e3; not 1.5 or 1e-3).
		if n, ok := value.(json.Number); !ok || !isIntegerNumber(string(n)) {
			return Deny(fmt.Sprintf(`Argument "%s" must be integer, got %s`, key, actualType))
		}
	default:
		if expectedJS(schema.Type) != actualType {
			return Deny(fmt.Sprintf(`Argument "%s" must be %s, got %s`, key, schema.Type, actualType))
		}
	}

	// Enum constraint. The check is "present", not "non-empty": an empty enum
	// denies every string.
	if s, ok := value.(string); ok && schema.Enum != nil {
		if !contains(schema.Enum, s) {
			return Deny(fmt.Sprintf(`Argument "%s" must be one of [%s], got "%s"`, key, strings.Join(schema.Enum, ", "), s))
		}
	}

	return Allow()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// jsType maps a decoded JSON value to its JavaScript typeof name: null, arrays
// and objects are all "object".
func jsType(v any) string {
	switch v.(type) {
	case nil, []any, map[string]any:
		return "object"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	default:
		return "object"
	}
}

// isIntegerNumber reports whether the JSON number text n has no fractional part. It works
// on the decimal text exactly, so a huge exponent (1e400) or a long fraction never loses
// precision and never allocates in proportion to the exponent: the value is
// digits x 10^(exp - fractionDigits), an integer when every digit below the units place,
// after dropping trailing zeros, is gone.
func isIntegerNumber(n string) bool {
	n = strings.TrimPrefix(n, "-")
	mant, expText := n, ""
	if i := strings.IndexAny(n, "eE"); i >= 0 {
		mant, expText = n[:i], n[i+1:]
	}
	intPart, frac := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, frac = mant[:i], mant[i+1:]
	}
	digits := strings.TrimRight(intPart+frac, "0")
	if strings.Trim(digits, "0") == "" {
		return true // zero, in any spelling
	}
	// scale is the number of digits below the units place before the exponent applies,
	// less the trailing zeros just dropped.
	scale := int64(len(frac)) - int64(len(intPart+frac)-len(digits))
	var exp int64
	if expText != "" {
		e, err := strconv.ParseInt(expText, 10, 64)
		if err != nil {
			// An exponent beyond int64: positive is an integer, negative is a tiny fraction.
			return !strings.HasPrefix(expText, "-")
		}
		exp = e
	}
	return exp >= scale
}

// expectedJS maps a schema type to the typeof name a value of that type has.
// An unknown schema type maps to "", which never equals a typeof result (the JS
// undefined), so no value ever satisfies it.
func expectedJS(schemaType string) string {
	switch schemaType {
	case "string", "number", "boolean":
		return schemaType
	case "array", "object":
		return "object"
	default:
		return ""
	}
}
