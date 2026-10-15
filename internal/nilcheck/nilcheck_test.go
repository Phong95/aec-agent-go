package nilcheck_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/internal/nilcheck"
)

type thing struct{}

type iface interface{ M() }

func (*thing) M() {}

// IsNil is true for a nil interface and for a typed nil pointer, map, slice, func
// and channel, and false for every non-nil value, including zero values.
func TestIsNil(t *testing.T) {
	var (
		nilPtr   *thing
		nilMap   map[string]int
		nilSlice []int
		nilFunc  func()
		nilChan  chan int
		nilIface iface
		typedNil iface = (*thing)(nil)
	)
	for name, v := range map[string]any{
		"nil":                nil,
		"nil pointer":        nilPtr,
		"nil map":            nilMap,
		"nil slice":          nilSlice,
		"nil func":           nilFunc,
		"nil chan":           nilChan,
		"nil interface":      nilIface,
		"typed nil in iface": typedNil,
	} {
		if !nilcheck.IsNil(v) {
			t.Errorf("IsNil(%s) = false, want true", name)
		}
	}
	for name, v := range map[string]any{
		"pointer":       &thing{},
		"empty map":     map[string]int{},
		"empty slice":   []int{},
		"func":          func() {},
		"chan":          make(chan int),
		"zero int":      0,
		"empty string":  "",
		"false":         false,
		"zero struct":   thing{},
		"iface pointer": iface(&thing{}),
	} {
		if nilcheck.IsNil(v) {
			t.Errorf("IsNil(%s) = true, want false", name)
		}
	}
}
