package artifacts

// Options bound what the artifact tools show the model. They are tool-level ceilings
// only: the sandbox clamps a read further to its own limits. A field that is zero or
// negative means the default; only a value greater than zero is used.
type Options struct {
	// MaxReadBytes is the most bytes of a tracked file's content read_artifact returns to
	// the model. Default 100 000.
	// Update (A-209): unused - there is no read action any more (the model reads the file
	// with read_file); kept so hosts that set it still compile.
	MaxReadBytes int
	// MaxListEntries is the most artifacts list_artifacts returns. Default 50. (A-209: the
	// artifact tool's "list" action.)
	MaxListEntries int
}
