package host

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// ---------------------------------------------------------------------------
// Host path confinement, the part below the logical path (CONTEXT A1).
//
//  1. realPathThroughMissing resolves the host path of a logical path through
//     every symlink AND Windows junction, with a manual walk. It is not
//     filepath.EvalSymlinks: since Go 1.23 that does not resolve junctions (a
//     path through one fails), so a port built on it would treat a junction as a
//     plain directory and pass the escape check (RESEARCH F-W2).
//  2. hostWithin compares the result with the real host folders of the agent's
//     roots, ignoring case on Windows.
//  3. inRoot then opens the file through an os.Root at the matched real root with
//     the root-relative remainder, so a link that was swapped in after the check is
//     refused by the operating system layer (the TOCTOU backstop, T-07-15).
// ---------------------------------------------------------------------------

// maxLinkHops is how many links one real-path walk follows (a limit of 40).
const maxLinkHops = 40

// realPathThroughMissing is realpath() that also works for paths that do not exist
// yet: it resolves the deepest existing ancestor through every symlink and Windows
// junction and appends the missing tail untouched, so a write through a linked
// parent is checked where it would really land. A dangling link is followed to
// where a write would really land. The walk starts at the volume root and
// inspects each component with os.Lstat; a ModeSymlink entry, or on Windows a
// ModeIrregular entry that os.Readlink can read (a junction), is replaced by its
// target (a relative target is relative to the directory that holds the link). "."
// and ".." inside a link target are applied to the already resolved, link-free
// prefix, which is the POSIX meaning. After maxLinkHops links the walk stops
// following and returns the path reached so far with the rest appended (the
// operation then fails in the os.Root layer with a loop error). The
// result is cleaned.
func realPathThroughMissing(target string) (string, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	vol := filepath.VolumeName(abs)
	sep := string(filepath.Separator)
	resolved := vol + sep
	pending := splitComponents(abs[len(vol):])
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		info, err := os.Lstat(next)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
				// The first missing component: the rest cannot exist either.
				return joinRest(next, pending), nil
			}
			return "", err
		}
		linkTo, isLink, err := linkTarget(next, info)
		if err != nil {
			return "", err
		}
		if !isLink {
			resolved = next
			continue
		}
		hops++
		if hops > maxLinkHops {
			return joinRest(next, pending), nil
		}
		switch {
		case filepath.VolumeName(linkTo) != "" || (len(linkTo) > 0 && os.IsPathSeparator(linkTo[0])):
			// An absolute target. A Windows target that is rooted but has no drive
			// ("\x") is on the volume of the link.
			tv := filepath.VolumeName(linkTo)
			if tv == "" {
				tv = vol
			}
			vol = tv
			resolved = vol + sep
			pending = append(splitComponents(linkTo[len(filepath.VolumeName(linkTo)):]), pending...)
		default:
			pending = append(splitComponents(linkTo), pending...)
		}
	}
	return filepath.Clean(resolved), nil
}

// joinRest is filepath.Join(base, rest...).
func joinRest(base string, rest []string) string {
	return filepath.Join(append([]string{base}, rest...)...)
}

// splitComponents splits a path on its separators and drops empty components.
func splitComponents(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == os.PathSeparator })
}

// linkTarget returns where the entry at p links to when it is a symlink or, on
// Windows, a junction (a ModeIrregular entry that os.Readlink can read; an
// irregular entry that is not a link, such as a cloud placeholder, is an ordinary
// component). info is the os.Lstat result for p.
func linkTarget(p string, info fs.FileInfo) (string, bool, error) {
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		to, err := os.Readlink(p)
		if err != nil {
			return "", false, err
		}
		return to, true, nil
	case runtime.GOOS == "windows" && mode&fs.ModeIrregular != 0:
		to, err := os.Readlink(p)
		if err != nil {
			return "", false, nil //nolint:nilerr // an irregular entry that is not a link is an ordinary component
		}
		return to, true, nil
	}
	return "", false, nil
}

// hostWithin reports whether child is parent or below it, comparing host paths
// case-insensitively on Windows.
func hostWithin(child, parent string) bool {
	return within(child, parent, string(filepath.Separator), onWindows)
}

// within is hostWithin with the separator and the case rule as parameters, so the
// table test runs the Windows rule on every operating system.
func within(child, parent, sep string, foldCase bool) bool {
	if foldCase {
		child, parent = jsstr.ToLower(child), jsstr.ToLower(parent)
	}
	if child == parent {
		return true
	}
	if !strings.HasSuffix(parent, sep) {
		parent += sep
	}
	return strings.HasPrefix(child, parent)
}

// inRoot opens an os.Root at realRoot, computes the path of realPath relative to it
// ("." for the root itself), runs fn and closes the root. A root is opened per
// operation and never kept, so there is no lifetime to manage. fn must use only
// the root and the relative path it is given: os.Root refuses every path that
// leaves realRoot, also through a link that was swapped in after the real-path
// check.
func inRoot(realRoot, realPath string, fn func(root *os.Root, rel string) error) error {
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(realRoot)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fn(root, rel)
}
