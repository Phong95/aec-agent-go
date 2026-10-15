package artifacthub

import (
	"context"
	"errors"
	"fmt"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// view is one agent's window on the artifact store. A
// sandbox backs every artifact: save/update write or track a file there, and read
// always goes back to it live rather than to a stored copy.
//
// Every read and every write is filtered by agentID: an artifact belonging to
// another agent behaves exactly like one that does not exist ("not_found"), so
// artifact ids cannot be used to probe or overwrite another agent's outputs.
// ---------------------------------------------------------------------------

// Compile-time proof that view is the artifact view contract.
var _ artifact.View = (*view)(nil)

// tracked is the file an artifact points at after a write or a re-track.
type tracked struct {
	path   string
	bytes  int
	sha256 string
}

type view struct {
	hub       *ArtifactHub
	agentID   string
	sessionID string
}

// AgentID returns the agent the view belongs to.
func (v *view) AgentID() string { return v.agentID }

// SessionID returns the session stamped on versions written through the view.
func (v *view) SessionID() string { return v.sessionID }

// Save creates the artifact at version 1: it writes the inline content to the
// sandbox, or tracks the existing file at the path, and stores the head record
// with its first version.
func (v *view) Save(ctx context.Context, in artifact.SaveInput, sb sandboxhub.View) (artifact.Write, error) {
	// Fail fast, before the argument checks and any lock wait, when no store is
	// configured. The store the operation uses is resolved once the lock is held.
	if _, err := v.hub.store(); err != nil {
		return artifact.Write{}, err
	}
	if err := requireSandbox(sb); err != nil {
		return artifact.Write{}, err
	}
	name, err := checkName(in.Name)
	if err != nil {
		return artifact.Write{}, err
	}
	typ, err := checkType(in.Type)
	if err != nil {
		return artifact.Write{}, err
	}

	// Check-then-create must not interleave with another save of the same name,
	// nor with a Delete or ReleaseAgentData of this agent.
	unlock, err := v.hub.locks.lockArtifact(ctx, v.agentID, name)
	if err != nil {
		return artifact.Write{}, err
	}
	defer unlock()

	// Resolved after the wait: an operation queued across a UsePersistence swap
	// must use the current store, one store for the whole operation.
	store, err := v.hub.store()
	if err != nil {
		return artifact.Write{}, err
	}
	existing, err := store.FindByName(ctx, v.agentID, name)
	if err != nil {
		return artifact.Write{}, err
	}
	if existing != nil {
		return artifact.Write{}, kernel.NewArtifactError(
			fmt.Sprintf("This agent already has an artifact named \"%s\" (v%d) \u2014 use update_artifact to change it", name, existing.Version),
			kernel.ArtifactCodeNameConflict, existing.ID)
	}

	t, err := v.resolveNewFile(ctx, in, sb, name)
	if err != nil {
		return artifact.Write{}, err
	}

	now := v.hub.timestamp()
	a := artifact.Artifact{
		ID:          v.hub.nextID(),
		AgentID:     v.agentID,
		Name:        name,
		Type:        typ,
		Description: checkDescription(in.Description),
		Tags:        checkTags(in.Tags),
		SessionID:   v.sessionID,
		Version:     1,
		SandboxPath: t.path,
		SandboxID:   sb.ID(),
		Bytes:       t.bytes,
		SHA256:      t.sha256,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	ver := v.buildVersion(a.ID, 1, t, now, in.Note, in.Tool, sb)

	if err := store.Create(ctx, a, ver); err != nil {
		return artifact.Write{}, err
	}
	return artifact.Write{Artifact: a, Version: ver}, nil
}

// Update appends a version: it rewrites the tracked file when content is given,
// otherwise it re-tracks the file at the target path, and moves the head record
// to version + 1.
func (v *view) Update(ctx context.Context, in artifact.UpdateInput, sb sandboxhub.View) (artifact.Write, error) {
	early, err := v.hub.store()
	if err != nil {
		return artifact.Write{}, err
	}
	if err := requireSandbox(sb); err != nil {
		return artifact.Write{}, err
	}

	// The name of the artifact is only known once the reference is resolved, so
	// resolve it to find the lock (with the store as it is now, which only serves
	// to learn the name), then resolve it again under the lock with the store that
	// is current then: a Delete that ran in between must make this Update fail with
	// not_found instead of being resurrected by AddVersion, and the version below
	// must be the latest.
	first, err := v.resolveArtifact(ctx, early, in.Ref)
	if err != nil {
		return artifact.Write{}, err
	}
	unlock, err := v.hub.locks.lockArtifact(ctx, v.agentID, first.Name)
	if err != nil {
		return artifact.Write{}, err
	}
	defer unlock()

	store, err := v.hub.store()
	if err != nil {
		return artifact.Write{}, err
	}
	current, err := v.resolveArtifact(ctx, store, in.Ref)
	if err != nil {
		return artifact.Write{}, err
	}

	if in.ExpectedVersion != nil && *in.ExpectedVersion != current.Version {
		return artifact.Write{}, kernel.NewArtifactError(
			fmt.Sprintf("\"%s\" is at v%d, not v%d \u2014 read it again before updating", current.Name, current.Version, *in.ExpectedVersion),
			kernel.ArtifactCodeConflict, current.ID)
	}

	target, err := resolveUpdateTarget(in, current, sb)
	if err != nil {
		return artifact.Write{}, err
	}
	var t tracked
	if in.Content != nil {
		t, err = writeAndTrack(ctx, target, *in.Content, sb)
	} else {
		t, err = trackExisting(ctx, target, sb)
	}
	if err != nil {
		return artifact.Write{}, err
	}

	// nil keeps the stored value; a given value replaces it, and an empty result
	// clears the field (omitted on the wire).
	next := current.Clone()
	if in.Description != nil {
		next.Description = checkDescription(*in.Description)
	}
	if in.Tags != nil {
		next.Tags = checkTags(in.Tags)
	}
	now := v.hub.timestamp()
	next.Version = current.Version + 1
	next.SandboxPath = t.path
	next.SandboxID = sb.ID()
	next.Bytes = t.bytes
	next.SHA256 = t.sha256
	next.UpdatedAt = now

	ver := v.buildVersion(next.ID, next.Version, t, now, in.Note, in.Tool, sb)
	if err := store.AddVersion(ctx, next, ver); err != nil {
		return artifact.Write{}, err
	}
	return artifact.Write{Artifact: next, Version: ver}, nil
}

// Get returns the metadata of the agent's own artifact; no sandbox is needed.
func (v *view) Get(ctx context.Context, ref artifact.Ref) (artifact.Artifact, error) {
	store, err := v.hub.store()
	if err != nil {
		return artifact.Artifact{}, err
	}
	a, err := v.resolveArtifact(ctx, store, ref)
	if err != nil {
		return artifact.Artifact{}, err
	}
	return a, nil
}

// Read reads the tracked file live through sb. A file whose sha256 still matches
// is current, one that does not is changed, and one that cannot be found - or
// that lives in another sandbox - is a missing file with a reason, not an error.
func (v *view) Read(ctx context.Context, ref artifact.Ref, sb sandboxhub.View, opts artifact.ReadOptions) (artifact.Read, error) {
	store, err := v.hub.store()
	if err != nil {
		return artifact.Read{}, err
	}
	a, err := v.resolveArtifact(ctx, store, ref)
	if err != nil {
		return artifact.Read{}, err
	}
	if err := requireSandbox(sb); err != nil {
		return artifact.Read{}, err
	}

	if sb.ID() != a.SandboxID {
		return artifact.Read{Artifact: a, File: missingFile(
			a.SandboxPath, a.SandboxID,
			fmt.Sprintf("\"%s\" was tracked in sandbox \"%s\", not \"%s\"", a.Name, a.SandboxID, sb.ID()))}, nil
	}

	file, err := sb.ReadFile(ctx, a.SandboxPath, sandboxhub.ReadOptions{MaxBytes: opts.MaxBytes})
	if err != nil {
		if se, ok := missingFileError(err); ok {
			return artifact.Read{Artifact: a, File: missingFile(a.SandboxPath, sb.ID(), se.Message)}, nil
		}
		return artifact.Read{}, err
	}
	status := artifact.FileChanged
	if file.SHA256 == a.SHA256 {
		status = artifact.FileCurrent
	}
	return artifact.Read{Artifact: a, File: artifact.File{
		Status: status, Path: file.Path, SandboxID: sb.ID(), Content: file.Content,
		Size: file.Size, SHA256: file.SHA256, Truncated: file.Truncated,
	}}, nil
}

// List returns the agent's artifacts, newest updated first. Filter fields that
// are empty (and a nil or zero limit) are dropped; the
// agent scope is always applied.
func (v *view) List(ctx context.Context, q artifact.Query) ([]artifact.Artifact, error) {
	store, err := v.hub.store()
	if err != nil {
		return nil, err
	}
	f := persistence.ArtifactFilter{
		AgentID:   v.agentID,
		SessionID: q.SessionID,
		Types:     q.Types,
		Name:      q.Name,
		Tags:      q.Tags,
		Since:     q.Since,
		Before:    q.Before,
	}
	if q.Limit != nil {
		f.Limit = *q.Limit
	}
	return store.Query(ctx, f)
}

// Versions returns every version of the agent's own artifact, newest first.
func (v *view) Versions(ctx context.Context, ref artifact.Ref) ([]artifact.Version, error) {
	store, err := v.hub.store()
	if err != nil {
		return nil, err
	}
	a, err := v.resolveArtifact(ctx, store, ref)
	if err != nil {
		return nil, err
	}
	return store.ListVersions(ctx, a.ID)
}

// Delete removes the agent's own artifact and its history; the sandbox file stays.
func (v *view) Delete(ctx context.Context, ref artifact.Ref) error {
	early, err := v.hub.store()
	if err != nil {
		return err
	}
	// Resolve once to learn the name for the lock (the store as it is now only
	// serves for that), then again under the lock with the store that is current
	// then, like Update.
	first, err := v.resolveArtifact(ctx, early, ref)
	if err != nil {
		return err
	}
	// Wait for a Save or Update of this artifact that is writing right now: it
	// holds the same lock from its check to its store call, so the removal is
	// ordered either before it (the Update then fails with not_found) or after it.
	unlock, err := v.hub.locks.lockArtifact(ctx, v.agentID, first.Name)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := v.hub.store()
	if err != nil {
		return err
	}
	a, err := v.resolveArtifact(ctx, store, ref)
	if err != nil {
		return err
	}
	return store.Delete(ctx, a.ID)
}

// --- internals ---------------------------------------------------------------

// requireSandbox rejects a nil sandbox view. A
// typed nil (an interface holding a nil pointer) counts as absent, like a nil
// store in ArtifactHub.store: it would otherwise panic on the first call.
func requireSandbox(sb sandboxhub.View) error {
	if nilcheck.IsNil(sb) {
		return errors.New("artifacthub: a sandbox view is required")
	}
	return nil
}

// resolveArtifact maps an id or a name to the agent's own artifact. Another
// agent's artifact is "not_found", exactly like a missing one. The caller passes the
// store it resolved, so one operation uses one store throughout.
func (v *view) resolveArtifact(ctx context.Context, store persistence.ArtifactStore, ref artifact.Ref) (artifact.Artifact, error) {
	var err error
	id := jsstr.Trim(ref.ID)
	name := jsstr.Trim(ref.Name)
	if id == "" && name == "" {
		return artifact.Artifact{}, kernel.NewArtifactError(`Give either "id" or "name" to identify the artifact`, kernel.ArtifactCodeInvalidArgument, "")
	}

	var found *artifact.Artifact
	if id != "" {
		found, err = store.Get(ctx, id)
	} else {
		found, err = store.FindByName(ctx, v.agentID, name)
	}
	if err != nil {
		return artifact.Artifact{}, err
	}
	if found == nil || found.AgentID != v.agentID {
		what := fmt.Sprintf("named \"%s\"", name)
		if id != "" {
			what = fmt.Sprintf("with id \"%s\"", id)
		}
		return artifact.Artifact{}, kernel.NewArtifactError(
			fmt.Sprintf("No artifact %s belongs to this agent", what), kernel.ArtifactCodeNotFound, id)
	}
	return *found, nil
}

// resolveNewFile writes inline content and tracks it, or tracks an existing file
// - the only two ways to create version 1.
func (v *view) resolveNewFile(ctx context.Context, in artifact.SaveInput, sb sandboxhub.View, name string) (tracked, error) {
	path := jsstr.Trim(in.Path)
	if in.Content != nil {
		if path == "" {
			path = "artifacts/" + name
		}
		return writeAndTrack(ctx, path, *in.Content, sb)
	}
	if path != "" {
		return trackExisting(ctx, path, sb)
	}
	return tracked{}, kernel.NewArtifactError(
		`Give either "content" (text to write and track) or "path" (an existing sandbox file to track)`,
		kernel.ArtifactCodeInvalidArgument, "")
}

// resolveUpdateTarget is where an update writes or re-tracks: the explicit path
// wins; otherwise the artifact's own path when the caller is in the sandbox it was
// tracked in; otherwise, with inline content, a fresh artifacts/<name>. Re-tracking
// (no content, no path) from a different sandbox has nothing to point at, so it
// fails clearly instead of guessing.
func resolveUpdateTarget(in artifact.UpdateInput, current artifact.Artifact, sb sandboxhub.View) (string, error) {
	if explicit := jsstr.Trim(in.Path); explicit != "" {
		return explicit, nil
	}
	if sb.ID() == current.SandboxID {
		return current.SandboxPath, nil
	}
	if in.Content != nil {
		return "artifacts/" + current.Name, nil
	}
	return "", kernel.NewArtifactError(
		fmt.Sprintf("\"%s\" was tracked in sandbox \"%s\", not \"%s\" \u2014 pass \"from_path\" or \"content\" to track it here", current.Name, current.SandboxID, sb.ID()),
		kernel.ArtifactCodeFileMissing, current.ID)
}

func writeAndTrack(ctx context.Context, path, content string, sb sandboxhub.View) (tracked, error) {
	written, err := sb.WriteFile(ctx, path, content, sandboxhub.WriteOptions{})
	if err != nil {
		return tracked{}, err
	}
	return tracked{path: written.Path, bytes: written.Bytes, sha256: written.SHA256}, nil
}

// trackExisting reads the file with a one byte limit, which still reports its
// whole size and hash.
func trackExisting(ctx context.Context, path string, sb sandboxhub.View) (tracked, error) {
	one := 1
	file, err := sb.ReadFile(ctx, path, sandboxhub.ReadOptions{MaxBytes: &one})
	if err != nil {
		if se, ok := missingFileError(err); ok {
			return tracked{}, kernel.NewArtifactError(
				fmt.Sprintf("\"%s\" is not in sandbox \"%s\" (%s) \u2014 write it first, or pass content", path, sb.ID(), se.Code),
				kernel.ArtifactCodeFileMissing, "")
		}
		return tracked{}, err
	}
	return tracked{path: file.Path, bytes: file.Size, sha256: file.SHA256}, nil
}

func (v *view) buildVersion(artifactID string, version int, t tracked, createdAt, note, tool string, sb sandboxhub.View) artifact.Version {
	return artifact.Version{
		ArtifactID: artifactID,
		Version:    version,
		Bytes:      t.bytes,
		SHA256:     t.sha256,
		CreatedAt:  createdAt,
		Note:       cleanNote(note),
		Origin: artifact.Origin{
			AgentID:     v.agentID,
			SessionID:   v.sessionID,
			Tool:        tool,
			SandboxPath: t.path,
			SandboxID:   sb.ID(),
		},
	}
}

// missingFileError reports whether err is a *kernel.SandboxError whose code means
// "the file is not there". Any other error, a SandboxError with another code
// included, is not mapped: the caller returns it unchanged so errors.Is and
// errors.As still work.
func missingFileError(err error) (*kernel.SandboxError, bool) {
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se == nil {
		return nil, false
	}
	_, missing := missingFileCodes[se.Code]
	return se, missing
}

func missingFile(path, sandboxID, reason string) artifact.File {
	return artifact.File{Status: artifact.FileMissing, Path: path, SandboxID: sandboxID, Reason: reason}
}
