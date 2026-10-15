package sandboxhub

// LimitsPatch is a partial Limits: every field is optional, and a set
// field (even zero) replaces the value, so each is a pointer. Providers take one
// as an option that adjusts their default Limits.
type LimitsPatch struct {
	MaxReadBytes       *int `json:"maxReadBytes,omitempty"`
	MaxWriteBytes      *int `json:"maxWriteBytes,omitempty"`
	MaxListEntries     *int `json:"maxListEntries,omitempty"`
	MaxSearchMatches   *int `json:"maxSearchMatches,omitempty"`
	MaxExecTimeoutMs   *int `json:"maxExecTimeoutMs,omitempty"`
	MaxExecOutputBytes *int `json:"maxExecOutputBytes,omitempty"`
}

// Apply returns l with every field set in p replaced (the patch is laid over the
// defaults). l is not changed.
func (l Limits) Apply(p LimitsPatch) Limits {
	if p.MaxReadBytes != nil {
		l.MaxReadBytes = *p.MaxReadBytes
	}
	if p.MaxWriteBytes != nil {
		l.MaxWriteBytes = *p.MaxWriteBytes
	}
	if p.MaxListEntries != nil {
		l.MaxListEntries = *p.MaxListEntries
	}
	if p.MaxSearchMatches != nil {
		l.MaxSearchMatches = *p.MaxSearchMatches
	}
	if p.MaxExecTimeoutMs != nil {
		l.MaxExecTimeoutMs = *p.MaxExecTimeoutMs
	}
	if p.MaxExecOutputBytes != nil {
		l.MaxExecOutputBytes = *p.MaxExecOutputBytes
	}
	return l
}
