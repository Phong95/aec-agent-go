package toolhub

import (
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// CompactContentMinChars (A-129): below this many characters (UTF-16 code units, as
// core/ts counts them) an inline "content" argument is kept as it is - a reference
// would save next to nothing.
const CompactContentMinChars = 1_024

// ContentWritten (A-129) says where a tool wrote a call's content, for
// WithContentReference.
type ContentWritten struct {
	// Size is the written size as the tool reports it (e.g. "8.2 KB").
	Size string `json:"size"`
	// Path is where the content was written.
	Path string `json:"path"`
	// ReadWith says how the model reads it back (e.g. "read_file").
	ReadWith string `json:"readWith"`
}

// WithContentReference (A-129) returns the call's arguments with a large inline
// "content" replaced by a short reference to where it was written, for
// Output.CompactArguments. A tool that wrote its content to a file (write_file,
// save_artifact, update_artifact) returns this, so later model requests carry the
// reference instead of the whole text again. The other keys keep their order. Nil
// when there is nothing worth replacing.
func WithContentReference(args kernel.Arguments, written ContentWritten) kernel.Arguments {
	var obj ordered.Object
	if err := args.Decode(&obj); err != nil {
		return nil
	}
	v, _ := obj.Get("content")
	content, ok := v.(string)
	if !ok || kernel.UTF16Len(content) < CompactContentMinChars {
		return nil
	}
	obj.Set("content", "["+written.Size+" written to "+written.Path+" — read it with "+written.ReadWith+" if needed]")
	raw, err := jsonx.Marshal(obj)
	if err != nil {
		return nil
	}
	out, err := kernel.NewArguments(raw)
	if err != nil {
		return nil
	}
	return out
}
