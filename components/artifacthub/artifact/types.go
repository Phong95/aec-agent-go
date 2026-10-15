// Package artifact holds the type-only contracts of the artifact hub (A-62).
//
// An artifact is a metadata tracker over one file in the owning agent's
// sandbox: a report, a spec section, a CSV takeoff, a clash report.
//
// An artifact stores no content of its own. It tracks a file that lives in the
// agent's sandboxhub.View - the file IS the deliverable, with an id, a type,
// versions and provenance layered on top so later turns, other agents and the UI
// can refer to it by name instead of re-pasting its content into the prompt.
// Reading an artifact reads that file live: current when its sha256 still
// matches the last tracked one, changed when it does not (a notice, not an
// error), missing when the file (or the sandbox itself) is gone.
//
// Phase 5 delivers the data types, the File union codec and the View contract
// only. The hub and the store arrive in Phase 6.
package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// Origin says where one version came from - agent, session, tool, and the
// sandbox file it tracked.
type Origin struct {
	AgentID string `json:"agentId"`
	// SessionID is the session the version was written in, when it was written
	// during a chat turn.
	SessionID string `json:"sessionId,omitempty"`
	// Tool is the tool that produced it, e.g. "save_artifact". Empty when written
	// through the SDK directly.
	Tool string `json:"tool,omitempty"`
	// SandboxPath is the logical sandbox path of the file this version tracked.
	SandboxPath string `json:"sandboxPath"`
	// SandboxID is the id of the sandbox the file lived in, e.g. "docker:shared".
	SandboxID string `json:"sandboxId"`
}

// Artifact is an artifact's head record - metadata plus the tracked file's
// last-known size and hash. The content lives only in the sandbox
// file at SandboxPath; nothing here, or in any version, is a copy of it.
type Artifact struct {
	ID string `json:"id"`
	// AgentID is the owning agent (AgentProfile.id). Every read path is filtered by it.
	AgentID string `json:"agentId"`
	// Name is stable, unique per agent - what the agent refers to the artifact by.
	Name string `json:"name"`
	// Type is a MIME type ("text/markdown") or a domain type ("aec/takeoff").
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitzero"`
	// SessionID is the session the artifact was first created in (provenance only -
	// artifacts outlive sessions).
	SessionID string `json:"sessionId,omitempty"`
	// Version is the head version number. Versions are 1-based and contiguous.
	Version int `json:"version"`
	// SandboxPath is the canonical logical path of the tracked file.
	SandboxPath string `json:"sandboxPath"`
	// SandboxID is the id of the sandbox the tracked file lives in, e.g. "docker:shared".
	SandboxID string `json:"sandboxId"`
	// Bytes of the tracked file as of the last save/update/re-track.
	Bytes int `json:"bytes"`
	// SHA256 (hex) of the tracked file as of the last save/update/re-track.
	SHA256    string `json:"sha256"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// Version is one entry of an artifact's content-less history log.
type Version struct {
	ArtifactID string `json:"artifactId"`
	// Version is 1-based, contiguous.
	Version   int    `json:"version"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
	CreatedAt string `json:"createdAt"`
	// Note is the agent's own note on why this version exists ("added cost column").
	Note   string `json:"note,omitempty"`
	Origin Origin `json:"origin"`
}

// FileStatus is the state of the tracked file.
type FileStatus string

// The three file statuses.
const (
	FileCurrent FileStatus = "current"
	FileChanged FileStatus = "changed"
	FileMissing FileStatus = "missing"
)

// File is the tracked file's live state, read straight from the sandbox - never
// stored.
//
// It is one struct with a status-keyed codec (a discriminated union on the wire).
// The wire shapes are exactly two:
//
//	current, changed: status, path, sandboxId, content, size, sha256, truncated
//	missing:          status, path, sandboxId, reason
//
// The decoder accepts those shapes strictly and returns an error for an unknown
// status, a missing or null required key, an unknown key and a mixed shape (the
// same discipline as sandboxhub.Selection). The encoder returns an error for an
// empty or unknown status and for a value that carries fields of the other shape.
// A current or changed file always writes "content", even when it is empty.
type File struct {
	Status FileStatus `json:"status"`
	// Path is the canonical logical path the content was read from.
	Path      string `json:"path"`
	SandboxID string `json:"sandboxId"`
	// Content is set for current and changed files only.
	Content string `json:"content"`
	// Size of the whole file in bytes (before any maxBytes truncation); current and
	// changed files only.
	Size int `json:"size"`
	// SHA256 (hex) of the whole file; current and changed files only.
	SHA256 string `json:"sha256"`
	// Truncated is true when maxBytes cut the content returned here - the file
	// itself is untouched; current and changed files only.
	Truncated bool `json:"truncated"`
	// Reason is why the file could not be read (not found, wrong sandbox, no
	// sandbox at all); missing files only.
	Reason string `json:"reason"`
}

// fileContentWire is the wire shape of a current or changed file.
type fileContentWire struct {
	Status    FileStatus `json:"status"`
	Path      string     `json:"path"`
	SandboxID string     `json:"sandboxId"`
	Content   string     `json:"content"`
	Size      int        `json:"size"`
	SHA256    string     `json:"sha256"`
	Truncated bool       `json:"truncated"`
}

// fileMissingWire is the wire shape of a missing file.
type fileMissingWire struct {
	Status    FileStatus `json:"status"`
	Path      string     `json:"path"`
	SandboxID string     `json:"sandboxId"`
	Reason    string     `json:"reason"`
}

// fileContentIn decodes a current or changed file; a nil pointer after decoding
// means the key was absent or null.
type fileContentIn struct {
	Status    *FileStatus `json:"status"`
	Path      *string     `json:"path"`
	SandboxID *string     `json:"sandboxId"`
	Content   *string     `json:"content"`
	Size      *int        `json:"size"`
	SHA256    *string     `json:"sha256"`
	Truncated *bool       `json:"truncated"`
}

// fileMissingIn decodes a missing file.
type fileMissingIn struct {
	Status    *FileStatus `json:"status"`
	Path      *string     `json:"path"`
	SandboxID *string     `json:"sandboxId"`
	Reason    *string     `json:"reason"`
}

// MarshalJSON writes the shape that belongs to f.Status. An empty or unknown
// status, and a value that mixes the two shapes, return an error.
func (f File) MarshalJSON() ([]byte, error) {
	switch f.Status {
	case FileCurrent, FileChanged:
		if f.Reason != "" {
			return nil, fmt.Errorf("artifact: a %s file must not carry a reason", f.Status)
		}
		return jsonx.Marshal(fileContentWire{
			Status: f.Status, Path: f.Path, SandboxID: f.SandboxID,
			Content: f.Content, Size: f.Size, SHA256: f.SHA256, Truncated: f.Truncated,
		})
	case FileMissing:
		if f.Content != "" || f.Size != 0 || f.SHA256 != "" || f.Truncated {
			return nil, errors.New("artifact: a missing file must not carry content, size, sha256 or truncated")
		}
		return jsonx.Marshal(fileMissingWire{
			Status: f.Status, Path: f.Path, SandboxID: f.SandboxID, Reason: f.Reason,
		})
	default:
		return nil, fmt.Errorf("artifact: invalid file status %q", f.Status)
	}
}

// UnmarshalJSON decodes the two wire shapes. Everything else (null, arrays,
// numbers, an unknown or missing status, a missing or null required key, an
// unknown key such as content on a missing file or reason on a current file)
// returns an error and leaves f unchanged.
func (f *File) UnmarshalJSON(data []byte) error {
	trimmed := bytes.Trim(data, " \t\r\n") // JSON whitespace only, not Unicode spaces
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("artifact: file must be a JSON object")
	}
	var probe struct {
		Status *string `json:"status"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return fmt.Errorf("artifact: invalid file: %w", err)
	}
	if probe.Status == nil {
		return errors.New("artifact: file requires a status")
	}
	switch FileStatus(*probe.Status) {
	case FileCurrent, FileChanged:
		var in fileContentIn
		if err := jsonx.UnmarshalStrict(trimmed, &in); err != nil {
			return fmt.Errorf("artifact: invalid %s file: %w", *probe.Status, err)
		}
		if in.Status == nil || in.Path == nil || in.SandboxID == nil || in.Content == nil ||
			in.Size == nil || in.SHA256 == nil || in.Truncated == nil {
			return fmt.Errorf("artifact: a %s file requires status, path, sandboxId, content, size, sha256 and truncated", *probe.Status)
		}
		*f = File{
			Status: *in.Status, Path: *in.Path, SandboxID: *in.SandboxID,
			Content: *in.Content, Size: *in.Size, SHA256: *in.SHA256, Truncated: *in.Truncated,
		}
		return nil
	case FileMissing:
		var in fileMissingIn
		if err := jsonx.UnmarshalStrict(trimmed, &in); err != nil {
			return fmt.Errorf("artifact: invalid missing file: %w", err)
		}
		if in.Status == nil || in.Path == nil || in.SandboxID == nil || in.Reason == nil {
			return errors.New("artifact: a missing file requires status, path, sandboxId and reason")
		}
		*f = File{Status: *in.Status, Path: *in.Path, SandboxID: *in.SandboxID, Reason: *in.Reason}
		return nil
	default:
		return fmt.Errorf("artifact: file status %q must be \"current\", \"changed\" or \"missing\"", *probe.Status)
	}
}

// Read is an artifact plus its tracked file's live state - what View.Read
// returns.
type Read struct {
	Artifact Artifact `json:"artifact"`
	File     File     `json:"file"`
}

// Write is an artifact plus the version just written - what View.Save and
// View.Update return.
type Write struct {
	Artifact Artifact `json:"artifact"`
	Version  Version  `json:"version"`
}

// ReadOptions are the options of View.Read.
type ReadOptions struct {
	// MaxBytes is the most bytes of the file's content to return. nil = the
	// sandbox's own Limits.MaxReadBytes.
	MaxBytes *int `json:"maxBytes,omitempty"`
}

// Ref is how a caller points at an artifact: by id or by name. Old versions
// cannot be opened (D-02). Ref has no MarshalJSON method: UpdateInput embeds it
// and a promoted method would replace the encoding of UpdateInput.
type Ref struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// SaveInput is the input of View.Save.
type SaveInput struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Path is the sandbox path to write (when Content is given) or to track (when
	// it is not). Default when writing: "artifacts/<name>".
	Path string `json:"path,omitempty"`
	// Content is inline text: written to the sandbox first, then tracked. nil
	// tracks the existing file at Path instead. An empty string is a valid empty file.
	Content     *string  `json:"content,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitzero"`
	Note        string   `json:"note,omitempty"`
	// Tool is the tool that is saving, for Origin.Tool.
	Tool string `json:"tool,omitempty"`
}

// UpdateInput is the input of View.Update. It embeds Ref.
type UpdateInput struct {
	Ref
	Path    string  `json:"path,omitempty"`
	Content *string `json:"content,omitempty"`
	// Description replaces the description when given. It is a pointer because
	// absent and empty differ: nil keeps the stored description and a pointer to
	// "" clears it. (A JSON null decodes to nil, which keeps it.)
	Description *string `json:"description,omitempty"`
	// Tags replaces the tag list when given (an empty non-nil list clears it); nil
	// leaves it as it is.
	Tags []string `json:"tags,omitzero"`
	Note string   `json:"note,omitempty"`
	// ExpectedVersion is optimistic concurrency: fail with "conflict" unless the
	// head is still this version.
	ExpectedVersion *int   `json:"expectedVersion,omitempty"`
	Tool            string `json:"tool,omitempty"`
}

// Query filters an agent's artifacts. The agent scope is applied by the view, not
// the caller.
type Query struct {
	// SessionID selects only artifacts first created in this session.
	SessionID string   `json:"sessionId,omitempty"`
	Types     []string `json:"types,omitzero"`
	// Name is an exact name match.
	Name string `json:"name,omitempty"`
	// Tags matches an artifact carrying any of these tags.
	Tags []string `json:"tags,omitzero"`
	// Since and Before are ISO timestamps on updatedAt.
	Since  string `json:"since,omitempty"`
	Before string `json:"before,omitempty"`
	Limit  *int   `json:"limit,omitempty"`
}
