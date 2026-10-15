package artifacts

import (
	"context"
	"errors"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The non-ASCII characters of the tool texts are built from code points, so this source
// stays ASCII-only.
var (
	emDash   = string(rune(0x2014)) // U+2014 EM DASH
	midDot   = string(rune(0x00b7)) // U+00B7 MIDDLE DOT
	ellipsis = string(rune(0x2026)) // U+2026 HORIZONTAL ELLIPSIS
)

// hints are the advice appended to an ArtifactError, by code. A code that is
// not listed has no hint.
var hints = map[kernel.ArtifactErrorCode]string{
	kernel.ArtifactCodeNameConflict:  "Use update_artifact to add a new version, or save under a different name.",
	kernel.ArtifactCodeNotFound:      "Check the exact name or id with list_artifacts.",
	kernel.ArtifactCodeConflict:      "Read the artifact again to get the current version, then update it.",
	kernel.ArtifactCodeNotConfigured: "No artifact store is configured on this server; tell the user.",
	kernel.ArtifactCodeFileMissing:   "write the file first (write_file) or pass content, then try again",
}

// withArtifacts runs fn against the agent's artifacts. Every failure
// becomes an output the model can act on, so the result is never an error:
//
//   - no artifact store for the agent: "<tool>: this agent has no artifact store", an em
//     dash and "lasting outputs are not available", not retryable;
//   - a *kernel.ArtifactError: "<tool> failed (<code>): <message>", then the hint of the
//     code on a new line when there is one, retryable as the error says, with the code in
//     the metadata. Deterministic failures (a name conflict, not found, a missing tracked
//     file and the like) are not retryable, so the harness does not repeat them;
//   - a *kernel.SandboxError (every artifact read or write touches the agent's sandbox):
//     "<tool> failed: sandbox error (<code>): <message>", and for the code disabled one
//     more line saying that the agent has no sandbox, retryable as the error says, with
//     the code in the metadata;
//   - any other error: "<tool> failed: <message>", an error output with no retryable
//     flag, so the harness may retry it.
//
// fn returns the error of a view or sandbox call as it is; withArtifacts does the
// mapping. The view comes from tc.GetArtifacts, which is already scoped to the agent and
// the session: the tools never name an agent themselves (T-06-24).
func withArtifacts(tc toolhub.Context, name string, fn func(view artifact.View) (toolhub.Output, error)) toolhub.Output {
	if tc.GetArtifacts == nil {
		return noStore(name)
	}
	view := tc.GetArtifacts()
	if nilcheck.IsNil(view) {
		return noStore(name)
	}
	out, err := fn(view)
	if err == nil {
		return out
	}

	var ae *kernel.ArtifactError
	if errors.As(err, &ae) {
		content := name + " failed (" + string(ae.Code) + "): " + ae.Message
		if hint, ok := hints[ae.Code]; ok {
			content += "\n" + hint
		}
		meta := ordered.NewObject()
		meta.Set("code", string(ae.Code))
		return toolkit.Failure(content, ae.Retryable, meta)
	}
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		content := name + " failed: sandbox error (" + string(se.Code) + "): " + se.Message
		if se.Code == kernel.SandboxCodeDisabled {
			content += "\nthis agent has no sandbox, so it cannot have artifacts"
		}
		meta := ordered.NewObject()
		meta.Set("code", string(se.Code))
		return toolkit.Failure(content, se.Retryable, meta)
	}
	return toolhub.Error(name + " failed: " + err.Error())
}

// noStore is the output of a tool call on an agent with no artifact store.
func noStore(name string) toolhub.Output {
	return toolkit.Fail(name + ": this agent has no artifact store " + emDash + " lasting outputs are not available")
}

// argError is a validation problem the model can fix: "<tool>: <message>", not
// retryable.
func argError(name, message string) toolhub.Output {
	return toolkit.Fail(name + ": " + message)
}

// sandboxFor resolves the agent's sandbox for an artifact tool call.
// Every artifact tracks a file, so a tool with no sandbox has nothing to track or read:
// the refusal is a clear, non-retryable argument error rather than a deeper failure once
// the call proceeds. The error of the accessor itself (a *kernel.SandboxError such as
// disabled) is returned unchanged for withArtifacts to map. Exactly one of the three
// results is set.
func sandboxFor(ctx context.Context, tc toolhub.Context, name string) (sandboxhub.View, *toolhub.Output, error) {
	env, err := toolkit.SandboxFor(ctx, tc)
	if errors.Is(err, toolkit.ErrNoSandbox) {
		refusal := argError(name, "this agent has no sandbox, so it cannot have artifacts "+emDash+" artifacts track files in the agent's workspace")
		return nil, &refusal, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return env, nil, nil
}

// stringList returns the string elements of the list, each trimmed with
// the JS trim, empty ones dropped; an empty result is absent (nil), like a key that was
// not given.
func stringList(a toolkit.Args, key string) []string {
	var list []string
	for _, s := range a.Strings(key) {
		if t := jsstr.Trim(s); t != "" {
			list = append(list, t)
		}
	}
	return list
}

// trimmedString is the trimmed string value of key: "" for an absent or non-string
// value (an empty string and an absent key are treated alike by every caller).
func trimmedString(a toolkit.Args, key string) string {
	s, _ := a.String(key)
	return jsstr.Trim(s)
}

// refArg returns the id and name from the tool arguments, trimmed, each left
// out when empty. The model may use either to point at an artifact.
func refArg(a toolkit.Args) artifact.Ref {
	return artifact.Ref{ID: trimmedString(a, "id"), Name: trimmedString(a, "name")}
}

// source is the content or path of a save or update: inline Content (nil
// when there is none), or a sandbox file Path to track, and the from_path that was
// dropped because inline content won.
type source struct {
	Content         *string
	Path            string
	IgnoredFromPath string
}

// sourceFrom reads the content or the path of a save or update: either inline content
// (written to the sandbox and then tracked), or a sandbox file named by from_path
// (tracked as it is). Models routinely fill every property a schema offers, so passing
// both is handled rather than refused: the inline content wins and the ignored from_path
// is reported back in the result. A blank content next to a from_path means the model
// meant to track that file. It reads and writes nothing: the view does.
func sourceFrom(a toolkit.Args) source {
	inline, hasInline := a.String("content")
	fromPath := trimmedString(a, "from_path")
	// An empty or whitespace content next to a from_path means the model meant to track
	// the file (some providers send "" for the property they did not fill).
	if hasInline && (fromPath == "" || jsstr.Trim(inline) != "") {
		return source{Content: &inline, IgnoredFromPath: fromPath}
	}
	if fromPath != "" {
		return source{Path: fromPath}
	}
	return source{}
}

// ignoredFromPathNote is told to the model when it passed both content and from_path: the
// call went through on the inline content, and this says which argument was dropped so
// the next call is unambiguous (and so a track it actually wanted is not silent).
func ignoredFromPathNote(ignored string) string {
	if ignored == "" {
		return ""
	}
	return "\nNote: you passed both \"content\" and \"from_path\" " + emDash + " the inline content was saved and \"" + ignored + "\" was ignored. " +
		"Pass only one. To track that file instead, call update_artifact with just \"from_path\"."
}

// artifactLink (A-132) is the link save/update put on their Output, so the chat can
// open the artifact.
func artifactLink(a artifact.Artifact) *kernel.ToolResultLink {
	return &kernel.ToolResultLink{URI: "artifact://" + a.ID, Name: a.Name, Type: a.Type, Version: a.Version}
}

// emitWritten publishes artifact:saved or artifact:updated for the version just written
// by the two tools. tc.Emit may be nil when a tool runs outside a
// hub; the call is then skipped. The byte count is converted to the int64 of the event
// payload.
func emitWritten(tc toolhub.Context, kind events.Kind[events.ArtifactWrittenPayload], a artifact.Artifact) {
	if tc.Emit == nil {
		return
	}
	tc.Emit(kind.Name, events.ArtifactWrittenPayload{
		AgentID:     a.AgentID,
		ArtifactID:  a.ID,
		Name:        a.Name,
		Type:        a.Type,
		Version:     a.Version,
		Bytes:       int64(a.Bytes),
		SandboxPath: a.SandboxPath,
		SandboxID:   a.SandboxID,
	})
}
