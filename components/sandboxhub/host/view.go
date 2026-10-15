package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/viewkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

const (
	// searchMaxFileBytes: search skips files above this size.
	searchMaxFileBytes = 8 * 1024 * 1024
	// searchTimeBudget is how long one search may run before it reports truncated.
	searchTimeBudget = 30 * time.Second
	// maxFileBytes: whole-file reads above this fail - ReadFile hashes the whole file.
	maxFileBytes = 64 * 1024 * 1024
)

// searchSkipDirs are the folders search never descends into - huge and never what
// the model is looking for.
var searchSkipDirs = map[string]bool{".git": true, "node_modules": true}

// view is one agent's view of a host Sandbox.
type view struct {
	sb         *Sandbox
	workingDir string
	roots      []string
}

// ID returns the sandbox id.
func (v *view) ID() string { return v.sb.id }

// Traits returns the sandbox traits.
func (v *view) Traits() sandboxhub.Traits { return v.sb.traits }

// Capabilities returns a fresh slice in the declared order.
func (v *view) Capabilities() []sandboxhub.Capability {
	return append(make([]sandboxhub.Capability, 0, len(v.sb.caps)), v.sb.caps...)
}

// Limits returns the sandbox limits.
func (v *view) Limits() sandboxhub.Limits { return v.sb.limits }

// WorkingDir returns the logical directory relative paths resolve against.
func (v *view) WorkingDir() string { return v.workingDir }

// Roots returns a fresh copy of the roots.
func (v *view) Roots() []string { return slices.Clone(v.roots) }

// -- Guards --

func (v *view) fail(message string, code kernel.SandboxErrorCode) error {
	return kernel.NewSandboxError(message, code, v.sb.id)
}

// begin is the guard every file operation runs first: the sandbox must not be
// disposed, it must have the capability, and the operation must not be cancelled.
func (v *view) begin(ctx context.Context, capability sandboxhub.Capability) error {
	if v.sb.isDisposed() {
		return v.sb.disposedError()
	}
	if !slices.Contains(v.sb.caps, capability) {
		return v.fail(fmt.Sprintf("This sandbox does not support %s", capability), kernel.SandboxCodeCapabilityUnsupported)
	}
	return v.cancelled(ctx)
}

// cancelled fails with the cancelled code when ctx is done.
func (v *view) cancelled(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
	}
	return nil
}

// resolvedPath is the outcome of resolve.
type resolvedPath struct {
	// logical is the canonical logical path.
	logical string
	// real is the real host path, after every symlink and junction.
	realPath string
	// realRoot is the real host folder of the root that contains real.
	realRoot string
}

// resolve turns a path the caller supplied into a real host path. It is checked
// twice: the logical path against the roots (sandboxhub.ResolveLogicalPath,
// then, on Windows, the file-name policy of invalidWindowsName), then the REAL host
// location (after symlinks and junctions, through missing tails) against the real
// host folders of those roots. Every operation then uses the real path through an
// os.Root at the matched real root.
func (v *view) resolve(input string) (resolvedPath, error) {
	logical, err := sandboxhub.ResolveLogicalPath(input, v.workingDir, v.roots, v.sb.id)
	if err != nil {
		return resolvedPath{}, err
	}
	if err := checkWindowsNames(logical, input, v.sb.id); err != nil {
		return resolvedPath{}, err
	}
	realPath, err := realPathThroughMissing(v.sb.toHost(logical))
	if err != nil {
		return resolvedPath{}, mapFSError(err, input, v.sb.id)
	}
	for _, root := range v.roots {
		realRoot, err := realPathThroughMissing(v.sb.toHost(root))
		if err != nil {
			return resolvedPath{}, mapFSError(err, input, v.sb.id)
		}
		if hostWithin(realPath, realRoot) {
			return resolvedPath{logical: logical, realPath: realPath, realRoot: realRoot}, nil
		}
	}
	return resolvedPath{}, v.fail(
		fmt.Sprintf("\"%s\" resolves through a symlink to a location outside the workspace", input), kernel.SandboxCodePathEscape)
}

// toLogical maps a real host path (inside the base) back to a logical path, for a
// search of a single file. It is computed against the real base, so a base that is
// itself reached through a link (a macOS /var folder) still maps back.
func (v *view) toLogical(realPath, fallback string) string {
	realBase, err := realPathThroughMissing(v.sb.base)
	if err != nil {
		return fallback
	}
	rel, err := filepath.Rel(realBase, realPath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fallback
	}
	logical := sandboxhub.WorkspaceRoot
	if rel != "." {
		logical = path.Join(sandboxhub.WorkspaceRoot, filepath.ToSlash(rel))
	}
	if sandboxhub.IsWithinRoots(logical, v.roots) {
		return logical
	}
	return fallback
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// entryTypeOf is the entry type of a file mode.
func entryTypeOf(m fs.FileMode) sandboxhub.EntryType {
	switch {
	case m&fs.ModeSymlink != 0:
		return sandboxhub.EntrySymlink
	case m.IsDir():
		return sandboxhub.EntryDirectory
	case m.IsRegular():
		return sandboxhub.EntryFile
	default:
		return sandboxhub.EntryOther
	}
}

// direntType is the entry type of a directory entry. A Windows junction is a
// ModeIrregular entry that os.Root can read as a link: it is listed as a symlink,
// as Node does.
func direntType(root *os.Root, d fs.DirEntry, rel string) sandboxhub.EntryType {
	m := d.Type()
	if onWindows && m&fs.ModeIrregular != 0 {
		if _, err := root.Readlink(rel); err == nil {
			return sandboxhub.EntrySymlink
		}
	}
	return entryTypeOf(m)
}

// readDirSorted lists a directory through the root, sorted by name with
// jsstr.LocaleCompare (a stable sort, like the JS one). A directory that cannot be
// read is an empty list.
func readDirSorted(root *os.Root, rel string) []fs.DirEntry {
	f, err := root.Open(rel)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil
	}
	slices.SortStableFunc(entries, func(a, b fs.DirEntry) int { return jsstr.LocaleCompare(a.Name(), b.Name()) })
	return entries
}

// -- Files --

// ReadFile returns the file: the content cut to the line range and to MaxBytes
// (clamped to the sandbox limit), the logical path, and the size and sha256 of the
// whole file. A directory is invalid_path; a file above 64 MiB is too_large.
func (v *view) ReadFile(ctx context.Context, input string, opts sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return sandboxhub.FileContent{}, err
	}
	res, err := v.resolve(input)
	if err != nil {
		return sandboxhub.FileContent{}, err
	}
	var raw []byte
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		st, err := root.Stat(rel)
		if err != nil {
			return mapFSError(err, input, v.sb.id)
		}
		if !st.Mode().IsRegular() {
			return v.fail(fmt.Sprintf("\"%s\" is a %s, not a file", input, entryTypeOf(st.Mode())), kernel.SandboxCodeInvalidPath)
		}
		if st.Size() > maxFileBytes {
			return v.fail(fmt.Sprintf("\"%s\" is %d bytes \U00002014 too large to read", input, st.Size()), kernel.SandboxCodeTooLarge)
		}
		// The Stat above is only a fast refusal: the file can grow (or be swapped) before
		// it is read, so the read itself is bounded to the limit plus one byte, and more
		// than the limit is too_large too. The handle is checked again to be a regular file.
		f, err := root.Open(rel)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if fst, err := f.Stat(); err != nil {
			return err
		} else if !fst.Mode().IsRegular() {
			return v.fail(fmt.Sprintf("\"%s\" is a %s, not a file", input, entryTypeOf(fst.Mode())), kernel.SandboxCodeInvalidPath)
		}
		var over bool
		raw, over, err = readAtMost(f, maxFileBytes)
		if err != nil {
			return err
		}
		if over {
			return v.fail(fmt.Sprintf("\"%s\" is more than %d bytes \U00002014 too large to read", input, maxFileBytes), kernel.SandboxCodeTooLarge)
		}
		return nil
	})
	if err != nil {
		return sandboxhub.FileContent{}, mapFSError(err, input, v.sb.id)
	}

	content, truncated := viewkit.LineRange(viewkit.LossyUTF8(raw), opts.StartLine, opts.EndLine)
	maxBytes := viewkit.ClampOption(opts.MaxBytes, v.sb.limits.MaxReadBytes, v.sb.limits.MaxReadBytes)
	if len(content) > maxBytes {
		content = viewkit.SliceBytes(content, maxBytes)
		truncated = true
	}
	return sandboxhub.FileContent{Path: res.logical, Content: content, Size: len(raw), Truncated: truncated, SHA256: sha256Hex(raw)}, nil
}

// readAtMost reads r through io.LimitReader(limit+1): at most limit bytes are kept, and
// over is true (with no content) when r holds more than limit bytes.
func readAtMost(r io.Reader, limit int64) (raw []byte, over bool, err error) {
	raw, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > limit {
		return nil, true, nil
	}
	return raw, false, nil
}

// WriteFile writes the file, creating missing parent directories unless CreateDirs
// is false, and returns the logical path, bytes, sha256 and whether the file is
// new. The checks run in this order: the byte limit, the path, the
// existing entry, the expected hash (an absent file counts as a mismatch), the
// parent directory.
func (v *view) WriteFile(ctx context.Context, input, content string, opts sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSWrite); err != nil {
		return sandboxhub.WriteResult{}, err
	}
	bytesLen := len(content)
	if bytesLen > v.sb.limits.MaxWriteBytes {
		return sandboxhub.WriteResult{}, v.fail(
			fmt.Sprintf("Content is %d bytes (limit %d)", bytesLen, v.sb.limits.MaxWriteBytes), kernel.SandboxCodeTooLarge)
	}
	res, err := v.resolve(input)
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	var created bool
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		existing, err := root.Lstat(rel)
		if err != nil && isPathEscape(err) {
			return err
		}
		exists := err == nil
		if exists && !existing.Mode().IsRegular() {
			return v.fail(fmt.Sprintf("\"%s\" is a %s", input, entryTypeOf(existing.Mode())), kernel.SandboxCodeInvalidPath)
		}
		if opts.ExpectedSHA256 != "" {
			current := ""
			if exists {
				data, err := root.ReadFile(rel)
				if err != nil {
					return err
				}
				current = sha256Hex(data)
			}
			if !exists || current != opts.ExpectedSHA256 {
				return v.fail(fmt.Sprintf("\"%s\" changed since it was read (hash mismatch)", input), kernel.SandboxCodeConflict)
			}
		}

		parent := filepath.Dir(rel)
		parentStat, err := root.Stat(parent)
		switch {
		case err != nil && isPathEscape(err):
			return err
		case err != nil:
			if opts.CreateDirs != nil && !*opts.CreateDirs {
				return v.fail(fmt.Sprintf("Directory \"%s\" does not exist", path.Dir(res.logical)), kernel.SandboxCodeNotFound)
			}
			if err := root.MkdirAll(parent, dirPerm); err != nil {
				return err
			}
		case !parentStat.IsDir():
			return v.fail(fmt.Sprintf("\"%s\" is not a directory", path.Dir(res.logical)), kernel.SandboxCodeInvalidPath)
		}

		created = !exists
		return root.WriteFile(rel, []byte(content), filePerm)
	})
	if err != nil {
		return sandboxhub.WriteResult{}, mapFSError(err, input, v.sb.id)
	}
	return sandboxhub.WriteResult{Path: res.logical, Bytes: bytesLen, SHA256: sha256Hex([]byte(content)), Created: created}, nil
}

// Stat returns nil and no error when the path does not exist or cannot be read.
// Stat follows links; Size is 0 for anything but a file; ModifiedAt is whole
// milliseconds.
func (v *view) Stat(ctx context.Context, input string) (*sandboxhub.FileStat, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return nil, err
	}
	res, err := v.resolve(input)
	if err != nil {
		return nil, err
	}
	var st fs.FileInfo
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		var err error
		st, err = root.Stat(rel)
		return err
	})
	if err != nil {
		if isPathEscape(err) {
			return nil, mapFSError(err, input, v.sb.id)
		}
		return nil, nil //nolint:nilerr,nilnil // the View contract: nil and no error for an absent path
	}
	size := 0
	if st.Mode().IsRegular() {
		size = int(st.Size())
	}
	return &sandboxhub.FileStat{Path: res.logical, Type: entryTypeOf(st.Mode()), Size: size, ModifiedAt: st.ModTime().UnixMilli()}, nil
}

// List lists a directory. Children are stable-sorted by name with
// jsstr.LocaleCompare, hidden entries are skipped before the truncation check, and
// symlinks (a Windows junction too) are listed and never followed.
func (v *view) List(ctx context.Context, input string, opts sandboxhub.ListOptions) (sandboxhub.ListResult, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return sandboxhub.ListResult{}, err
	}
	res, err := v.resolve(input)
	if err != nil {
		return sandboxhub.ListResult{}, err
	}
	limit := viewkit.ClampOption(opts.MaxEntries, v.sb.limits.MaxListEntries, v.sb.limits.MaxListEntries)
	includeHidden := opts.IncludeHidden != nil && *opts.IncludeHidden
	recursive := opts.Recursive != nil && *opts.Recursive
	entries := []sandboxhub.DirEntry{}
	truncated := false

	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		st, err := root.Stat(rel)
		if err != nil {
			return mapFSError(err, input, v.sb.id)
		}
		if !st.IsDir() {
			return v.fail(fmt.Sprintf("\"%s\" is not a directory", input), kernel.SandboxCodeInvalidPath)
		}
		var walk func(dirRel, dirLogical string)
		walk = func(dirRel, dirLogical string) {
			for _, child := range readDirSorted(root, dirRel) {
				name := child.Name()
				if !includeHidden && strings.HasPrefix(name, ".") {
					continue
				}
				if len(entries) >= limit {
					truncated = true
					return
				}
				childRel := filepath.Join(dirRel, name)
				childLogical := path.Join(dirLogical, name)
				typ := direntType(root, child, childRel)
				size := 0
				if typ == sandboxhub.EntryFile {
					if info, err := child.Info(); err == nil {
						size = int(info.Size())
					}
				}
				entries = append(entries, sandboxhub.DirEntry{Path: childLogical, Name: name, Type: typ, Size: size})
				// Symlinks are listed, never followed.
				if recursive && typ == sandboxhub.EntryDirectory {
					walk(childRel, childLogical)
				}
				if truncated {
					return
				}
			}
		}
		walk(rel, res.logical)
		return nil
	})
	if err != nil {
		return sandboxhub.ListResult{}, mapFSError(err, input, v.sb.id)
	}
	return sandboxhub.ListResult{Entries: entries, Truncated: truncated}, nil
}

// Search looks for the pattern line by line in the files under the path, in name
// order. It skips .git and node_modules, files above 8 MiB and binary files (a NUL
// byte), never follows links, checks ctx for every entry and stops with truncated
// after 30 seconds. A regular expression is compiled by viewkit.CompileSearchRegex
// (RE2, with the divergent constructs rejected), so a pattern JavaScript accepts and
// RE2 does not is invalid_argument.
func (v *view) Search(ctx context.Context, q sandboxhub.SearchQuery) (sandboxhub.SearchResult, error) {
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
	res, err := v.resolve(searchPath)
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
	deadline := time.Now().Add(searchTimeBudget)

	matches := []sandboxhub.SearchMatch{}
	// scanFile reports true when the match limit was hit.
	scanFile := func(root *os.Root, rel, logical, name string) bool {
		if q.Glob != "" && !viewkit.GlobMatch(q.Glob, name, onWindows) {
			return false
		}
		st, err := root.Stat(rel)
		if err != nil || !st.Mode().IsRegular() || st.Size() > searchMaxFileBytes {
			return false
		}
		raw, err := root.ReadFile(rel)
		if err != nil || bytes.IndexByte(raw, 0) >= 0 { // unreadable or binary
			return false
		}
		for i, line := range strings.Split(viewkit.LossyUTF8(raw), "\n") {
			if !matcher(line) {
				continue
			}
			if len(matches) >= limit {
				return true
			}
			matches = append(matches, sandboxhub.SearchMatch{Path: logical, Line: i + 1, Text: line})
		}
		return false
	}
	// walk reports true when the search must stop (match limit or time budget).
	var walk func(root *os.Root, dirRel, dirLogical string) (bool, error)
	walk = func(root *os.Root, dirRel, dirLogical string) (bool, error) {
		for _, child := range readDirSorted(root, dirRel) {
			if err := v.cancelled(ctx); err != nil {
				return false, err
			}
			if time.Now().After(deadline) {
				return true, nil
			}
			name := child.Name()
			childRel := filepath.Join(dirRel, name)
			childLogical := path.Join(dirLogical, name)
			// Links are never followed (they could lead outside the roots): a symlink
			// or junction is neither a directory nor a regular file here.
			switch {
			case child.IsDir():
				if searchSkipDirs[name] {
					continue
				}
				stop, err := walk(root, childRel, childLogical)
				if err != nil || stop {
					return stop, err
				}
			case child.Type().IsRegular():
				if scanFile(root, childRel, childLogical, name) {
					return true, nil
				}
			}
		}
		return false, nil
	}

	truncated := false
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		st, err := root.Stat(rel)
		if err != nil {
			label := q.Path
			if label == "" {
				label = "."
			}
			return mapFSError(err, label, v.sb.id)
		}
		if st.IsDir() {
			var err error
			truncated, err = walk(root, rel, res.logical)
			return err
		}
		truncated = scanFile(root, rel, v.toLogical(res.realPath, res.logical), filepath.Base(res.realPath))
		return nil
	})
	if err != nil {
		return sandboxhub.SearchResult{}, mapFSError(err, searchPath, v.sb.id)
	}
	return sandboxhub.SearchResult{Matches: matches, Truncated: truncated}, nil
}

// -- Commands --

// Exec, the host command runner with its process-tree kill, is in exec.go. The view
// advertises exec and exec.shell after the file capabilities.
