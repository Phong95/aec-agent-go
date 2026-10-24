package workspace

// The defaults of Options.
const (
	// A-129 item 4: was 100 000 (~25k tokens); now within the loop's 50 000-character
	// tool-result limit with the line-number prefixes, so read_file's own paging applies.
	defaultMaxReadBytes          = 32_000
	defaultMaxDisplayOutputBytes = 30_000
	defaultTimeoutMs             = 120_000
	defaultMaxTimeoutMs          = 10 * 60_000
	defaultMaxListEntries        = 500
	defaultMaxSearchMatches      = 100
)

// resolved is Options with every default applied.
type resolved struct {
	maxReadBytes          int
	maxDisplayOutputBytes int
	defaultTimeoutMs      int
	maxTimeoutMs          int
	maxListEntries        int
	maxSearchMatches      int
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
		maxReadBytes:          positive(o.MaxReadBytes, defaultMaxReadBytes),
		maxDisplayOutputBytes: positive(o.MaxDisplayOutputBytes, defaultMaxDisplayOutputBytes),
		defaultTimeoutMs:      positive(o.DefaultTimeoutMs, defaultTimeoutMs),
		maxTimeoutMs:          positive(o.MaxTimeoutMs, defaultMaxTimeoutMs),
		maxListEntries:        positive(o.MaxListEntries, defaultMaxListEntries),
		maxSearchMatches:      positive(o.MaxSearchMatches, defaultMaxSearchMatches),
	}
}
