package aecmock

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/viewkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxSymlinkHops is how many symlinks a path may pass through before it fails
// with invalid_path.
const maxSymlinkHops = 40

// inMemoryView is one agent's view of an InMemorySandbox.
// Every method takes the sandbox mutex for its whole body, except Exec, which
// holds it only while it validates the request.
type inMemoryView struct {
	sb         *InMemorySandbox
	workingDir string
	roots      []string
}

// ID returns the sandbox id.
func (v *inMemoryView) ID() string { return v.sb.id }

// Traits returns the sandbox traits.
func (v *inMemoryView) Traits() sandboxhub.Traits { return v.sb.traits }

// Capabilities returns a fresh slice in the provider's declared order.
func (v *inMemoryView) Capabilities() []sandboxhub.Capability {
	// Never nil: a sandbox without capabilities has an empty list.
	return append(make([]sandboxhub.Capability, 0, len(v.sb.caps)), v.sb.caps...)
}

// Limits returns the sandbox limits.
func (v *inMemoryView) Limits() sandboxhub.Limits { return v.sb.limits }

// WorkingDir returns the logical directory relative paths resolve against.
func (v *inMemoryView) WorkingDir() string { return v.workingDir }

// Roots returns a fresh copy of the roots.
func (v *inMemoryView) Roots() []string { return slices.Clone(v.roots) }

// -- Guards --

func (v *inMemoryView) fail(message string, code kernel.SandboxErrorCode) error {
	return kernel.NewSandboxError(message, code, v.sb.id)
}

// begin is the guard every operation but exec runs first: the sandbox must not be
// disposed, it must have the capability, and the operation must not be cancelled.
// The caller holds the sandbox mutex.
func (v *inMemoryView) begin(ctx context.Context, capability sandboxhub.Capability) error {
	if v.sb.disposed {
		return v.fail(fmt.Sprintf("Sandbox \"%s\" was disposed", v.sb.id), kernel.SandboxCodeEnvironmentUnavailable)
	}
	if !v.sb.hasCapability(capability) {
		return v.fail(fmt.Sprintf("This sandbox does not support %s", capability), kernel.SandboxCodeCapabilityUnsupported)
	}
	if ctx != nil && ctx.Err() != nil {
		return v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
	}
	return nil
}

// realPath follows symlinks component by component and returns the real path
// (which may not exist). The caller holds the sandbox mutex.
func (v *inMemoryView) realPath(logical string) (string, error) {
	resolved := "/"
	pending := splitNonEmpty(logical)
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		next := path.Join(resolved, part)
		node, ok := v.sb.store.get(next)
		if ok && node.kind == sandboxhub.EntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return "", v.fail(fmt.Sprintf("Too many symlinks resolving \"%s\"", logical), kernel.SandboxCodeInvalidPath)
			}
			target := node.target
			if !strings.HasPrefix(target, "/") {
				target = path.Join(resolved, node.target)
			}
			resolved = "/"
			pending = append(splitNonEmpty(target), pending...)
		} else {
			resolved = next
		}
	}
	return path.Clean(resolved), nil
}

// resolve turns a logical path into a real path, both checked against the agent's
// roots: sandboxhub.ResolveLogicalPath, then the symlink walk, then the roots check.
// The caller holds the sandbox mutex.
func (v *inMemoryView) resolve(p string) (string, error) {
	logical, err := sandboxhub.ResolveLogicalPath(p, v.workingDir, v.roots, v.sb.id)
	if err != nil {
		return "", err
	}
	rp, err := v.realPath(logical)
	if err != nil {
		return "", err
	}
	if !sandboxhub.IsWithinRoots(rp, v.roots) {
		return "", v.fail(
			fmt.Sprintf("\"%s\" resolves through a symlink to a location outside the workspace", p), kernel.SandboxCodePathEscape)
	}
	return rp, nil
}

// -- Files --

// ReadFile returns the file: the content cut to the line range and to MaxBytes
// (clamped to the sandbox limit; a zero MaxBytes means the default limit), the real
// path, and the size and sha256 of the whole file - a cut never changes the
// reported size or hash. A cut inside a multi-byte character decodes to one
// replacement character. A missing path is
// not_found with the input path (not the resolved one) in the message; a directory
// is invalid_path.
func (v *inMemoryView) ReadFile(ctx context.Context, p string, opts sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return sandboxhub.FileContent{}, err
	}
	rp, err := v.resolve(p)
	if err != nil {
		return sandboxhub.FileContent{}, err
	}
	node, ok := v.sb.store.get(rp)
	if !ok {
		return sandboxhub.FileContent{}, v.fail(fmt.Sprintf("\"%s\" does not exist", p), kernel.SandboxCodeNotFound)
	}
	if node.kind != sandboxhub.EntryFile {
		return sandboxhub.FileContent{}, v.fail(fmt.Sprintf("\"%s\" is a %s, not a file", p, node.kind), kernel.SandboxCodeInvalidPath)
	}

	content, truncated := viewkit.LineRange(node.content, opts.StartLine, opts.EndLine)
	maxBytes := viewkit.ClampOption(opts.MaxBytes, v.sb.limits.MaxReadBytes, v.sb.limits.MaxReadBytes)
	if len(content) > maxBytes {
		content = viewkit.SliceBytes(content, maxBytes)
		truncated = true
	}
	return sandboxhub.FileContent{
		Path: rp, Content: content, Size: len(node.content), Truncated: truncated, SHA256: sha256Hex(node.content),
	}, nil
}

// WriteFile writes the file, creating missing parent directories unless
// CreateDirs is false, and returns the path, bytes, sha256 and whether the file is
// new. The file is stamped after its parents, so the clock reads are one per
// missing directory and then one for the file.
func (v *inMemoryView) WriteFile(ctx context.Context, p, content string, opts sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	if err := v.begin(ctx, sandboxhub.CapabilityFSWrite); err != nil {
		return sandboxhub.WriteResult{}, err
	}
	bytes := len(content)
	if bytes > v.sb.limits.MaxWriteBytes {
		return sandboxhub.WriteResult{}, v.fail(
			fmt.Sprintf("Content is %d bytes (limit %d)", bytes, v.sb.limits.MaxWriteBytes), kernel.SandboxCodeTooLarge)
	}
	rp, err := v.resolve(p)
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	existing, exists := v.sb.store.get(rp)
	if exists && existing.kind != sandboxhub.EntryFile {
		return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("\"%s\" is a %s", p, existing.kind), kernel.SandboxCodeInvalidPath)
	}
	if opts.ExpectedSHA256 != "" && (!exists || sha256Hex(existing.content) != opts.ExpectedSHA256) {
		return sandboxhub.WriteResult{}, v.fail(
			fmt.Sprintf("\"%s\" changed since it was read (hash mismatch)", p), kernel.SandboxCodeConflict)
	}

	parent := path.Dir(rp)
	parentNode, parentExists := v.sb.store.get(parent)
	switch {
	case !parentExists:
		if opts.CreateDirs != nil && !*opts.CreateDirs {
			return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("Directory \"%s\" does not exist", parent), kernel.SandboxCodeNotFound)
		}
		v.sb.mkdirp(parent)
	case parentNode.kind != sandboxhub.EntryDirectory:
		return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("\"%s\" is not a directory", parent), kernel.SandboxCodeInvalidPath)
	}

	v.sb.store.set(rp, memNode{kind: sandboxhub.EntryFile, content: content, mtime: v.sb.tick()})
	return sandboxhub.WriteResult{Path: rp, Bytes: bytes, SHA256: sha256Hex(content), Created: !exists}, nil
}

// Stat returns nil and no error when the path does not exist. It reads no clock.
func (v *inMemoryView) Stat(ctx context.Context, p string) (*sandboxhub.FileStat, error) {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return nil, err
	}
	rp, err := v.resolve(p)
	if err != nil {
		return nil, err
	}
	node, ok := v.sb.store.get(rp)
	if !ok {
		return nil, nil //nolint:nilnil // the View contract: nil and no error for an absent path
	}
	size := 0
	if node.kind == sandboxhub.EntryFile {
		size = len(node.content)
	}
	return &sandboxhub.FileStat{Path: rp, Type: node.kind, Size: size, ModifiedAt: node.mtime}, nil
}

// List lists a directory. Children are stable-sorted by their full path with
// jsstr.LocaleCompare, hidden entries are skipped before the truncation check, and
// symlinks are listed and never followed.
func (v *inMemoryView) List(ctx context.Context, p string, opts sandboxhub.ListOptions) (sandboxhub.ListResult, error) {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return sandboxhub.ListResult{}, err
	}
	rp, err := v.resolve(p)
	if err != nil {
		return sandboxhub.ListResult{}, err
	}
	node, ok := v.sb.store.get(rp)
	if !ok {
		return sandboxhub.ListResult{}, v.fail(fmt.Sprintf("\"%s\" does not exist", p), kernel.SandboxCodeNotFound)
	}
	if node.kind != sandboxhub.EntryDirectory {
		return sandboxhub.ListResult{}, v.fail(fmt.Sprintf("\"%s\" is not a directory", p), kernel.SandboxCodeInvalidPath)
	}

	limit := viewkit.ClampOption(opts.MaxEntries, v.sb.limits.MaxListEntries, v.sb.limits.MaxListEntries)
	includeHidden := opts.IncludeHidden != nil && *opts.IncludeHidden
	recursive := opts.Recursive != nil && *opts.Recursive
	entries := []sandboxhub.DirEntry{}
	truncated := false

	type child struct {
		key  string
		node memNode
	}
	var walk func(dir string)
	walk = func(dir string) {
		var children []child
		for _, key := range v.sb.store.keys {
			if path.Dir(key) == dir && key != dir {
				children = append(children, child{key: key, node: v.sb.store.nodes[key]})
			}
		}
		slices.SortStableFunc(children, func(a, b child) int { return jsstr.LocaleCompare(a.key, b.key) })
		for _, c := range children {
			name := path.Base(c.key)
			if !includeHidden && strings.HasPrefix(name, ".") {
				continue
			}
			if len(entries) >= limit {
				truncated = true
				return
			}
			size := 0
			if c.node.kind == sandboxhub.EntryFile {
				size = len(c.node.content)
			}
			entries = append(entries, sandboxhub.DirEntry{Path: c.key, Name: name, Type: c.node.kind, Size: size})
			// Symlinks are listed, never followed.
			if recursive && c.node.kind == sandboxhub.EntryDirectory {
				walk(c.key)
			}
			if truncated {
				return
			}
		}
	}
	walk(rp)
	return sandboxhub.ListResult{Entries: entries, Truncated: truncated}, nil
}

// Search looks for the pattern line by line in the files under the path. There is
// no binary, size or directory filtering (the host and Docker sandboxes have it).
// It is truncated only when one more match exists beyond the limit.
func (v *inMemoryView) Search(ctx context.Context, q sandboxhub.SearchQuery) (sandboxhub.SearchResult, error) {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	if err := v.begin(ctx, sandboxhub.CapabilityFSSearch); err != nil {
		return sandboxhub.SearchResult{}, err
	}
	if q.Pattern == "" {
		return sandboxhub.SearchResult{}, v.fail("Search pattern must be a non-empty string", kernel.SandboxCodeInvalidArgument)
	}
	searchPath := q.Path
	if searchPath == "" {
		searchPath = v.workingDir
	}
	base, err := v.resolve(searchPath)
	if err != nil {
		return sandboxhub.SearchResult{}, err
	}
	limit := viewkit.ClampOption(q.MaxMatches, v.sb.limits.MaxSearchMatches, v.sb.limits.MaxSearchMatches)

	ignoreCase := q.CaseSensitive != nil && !*q.CaseSensitive
	var matcher func(line string) bool
	if q.Literal != nil && !*q.Literal {
		re, err := viewkit.CompileSearchRegex(q.Pattern, ignoreCase)
		if err != nil {
			return sandboxhub.SearchResult{}, v.fail(fmt.Sprintf("Invalid regular expression: %s", err.Error()), kernel.SandboxCodeInvalidArgument)
		}
		matcher = re.MatchString
	} else {
		needle := q.Pattern
		if ignoreCase {
			needle = jsstr.ToLower(needle)
		}
		matcher = func(line string) bool {
			if ignoreCase {
				line = jsstr.ToLower(line)
			}
			return strings.Contains(line, needle)
		}
	}

	var files []string
	for _, key := range v.sb.store.keys {
		node := v.sb.store.nodes[key]
		if node.kind == sandboxhub.EntryFile && (key == base || strings.HasPrefix(key, base+"/")) {
			files = append(files, key)
		}
	}
	slices.SortStableFunc(files, jsstr.LocaleCompare)

	matches := []sandboxhub.SearchMatch{}
	for _, key := range files {
		if q.Glob != "" && !viewkit.GlobMatch(q.Glob, path.Base(key), false) {
			continue
		}
		for i, line := range strings.Split(v.sb.store.nodes[key].content, "\n") {
			if !matcher(line) {
				continue
			}
			if len(matches) >= limit {
				return sandboxhub.SearchResult{Matches: matches, Truncated: true}, nil
			}
			matches = append(matches, sandboxhub.SearchMatch{Path: key, Line: i + 1, Text: line})
		}
	}
	return sandboxhub.SearchResult{Matches: matches, Truncated: false}, nil
}
