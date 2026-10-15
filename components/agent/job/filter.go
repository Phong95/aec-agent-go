package job

import (
	"reflect"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/internal/jobkit"
)

// FilterMismatch says why event e does not match filter f, or returns "" when it does.
// It is the filterMismatch of core/ts, with the same reasons.
func FilterMismatch(f Filter, e Event) string {
	if len(f.Types) > 0 && !matchesAny(f.Types, e.Type) {
		return `type "` + e.Type + `" is not one of ` + strings.Join(f.Types, ", ")
	}
	if len(f.Subjects) > 0 && (e.Subject == "" || !contains(f.Subjects, e.Subject)) {
		subject := "(none)"
		if e.Subject != "" {
			subject = `"` + e.Subject + `"`
		}
		return "subject " + subject + " is not one of the job's subjects"
	}
	if len(f.Where) == 0 {
		return ""
	}
	data, err := jobkit.Decode(e.Data)
	if err != nil {
		data = nil
	}
	for _, c := range f.Where {
		if !conditionHolds(c, e, data) {
			value := ""
			if c.Value != nil {
				if raw, err := jobkit.Marshal(c.Value); err == nil {
					value = " " + string(raw)
				}
			}
			return "condition " + c.Path + " " + string(c.Op) + value + " does not hold"
		}
	}
	return ""
}

// TypeMatches says whether pattern matches an event type: "a.*" matches "a.b";
// anything else must be equal.
func TypeMatches(pattern, typ string) bool {
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		return strings.HasPrefix(typ, prefix+".")
	}
	return pattern == typ
}

func matchesAny(patterns []string, typ string) bool {
	for _, p := range patterns {
		if TypeMatches(p, typ) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// conditionHolds evaluates one condition; data is the event's decoded data.
func conditionHolds(c Condition, e Event, data any) bool {
	actual, found := jobkit.ReadPath(e.Type, e.Subject, e.Source, data, c.Path)
	switch c.Op {
	case OpEq:
		return found && jobkit.SameValue(actual, c.Value)
	case OpNeq:
		return !found || !jobkit.SameValue(actual, c.Value)
	case OpIn:
		for _, v := range listOf(c.Value) {
			if found && jobkit.SameValue(actual, v) {
				return true
			}
		}
		return false
	case OpExists:
		present := found && actual != nil
		want := true
		if b, ok := c.Value.(bool); ok {
			want = b
		}
		return present == want
	case OpContains:
		if !found {
			return false
		}
		if s, ok := actual.(string); ok {
			sub, ok := c.Value.(string)
			return ok && strings.Contains(s, sub)
		}
		if list, ok := actual.([]any); ok {
			for _, v := range list {
				if jobkit.SameValue(v, c.Value) {
					return true
				}
			}
		}
		return false
	default:
		return false
	}
}

// listOf returns the items of a slice value ([]any from JSON, or any Go slice), or nil
// for anything else.
func listOf(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return nil
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out
}
