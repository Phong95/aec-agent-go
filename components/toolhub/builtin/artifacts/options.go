package artifacts

// The defaults of Options.
const (
	defaultMaxReadBytes   = 100_000
	defaultMaxListEntries = 50
)

// resolved is Options with every default applied.
type resolved struct {
	maxReadBytes   int
	maxListEntries int
}

// resolve applies the defaults: a value that is not greater than zero is replaced.
func resolve(o Options) resolved {
	positive := func(v, fallback int) int {
		if v > 0 {
			return v
		}
		return fallback
	}
	return resolved{
		maxReadBytes:   positive(o.MaxReadBytes, defaultMaxReadBytes),
		maxListEntries: positive(o.MaxListEntries, defaultMaxListEntries),
	}
}
