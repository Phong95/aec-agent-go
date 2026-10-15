package workspace

// Options bound what the workspace tools show the model. The sandbox itself enforces the
// hard limits; these only shape the output. A field that is zero or negative means the
// default; only a value greater than zero is used.
type Options struct {
	// MaxReadBytes is the most bytes of a file read_file returns to the model. Default
	// 100 000.
	MaxReadBytes int
	// MaxDisplayOutputBytes is the most bytes of command output shown to the model
	// (head and tail are kept). Default 30 000.
	MaxDisplayOutputBytes int
	// DefaultTimeoutMs is the command timeout when the model gives none. Default 120 000
	// (120 s).
	DefaultTimeoutMs int
	// MaxTimeoutMs is the ceiling for timeout_seconds (the sandbox clamps further).
	// Default 600 000 (10 min).
	MaxTimeoutMs int
	// MaxListEntries is the most entries list_files returns. Default 500.
	MaxListEntries int
	// MaxSearchMatches is the most matches search_files returns. Default 100.
	MaxSearchMatches int
}
