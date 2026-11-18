package agent

import (
	"context"
	"encoding/json"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// InstructionsFileName (A-227) is the one instruction file name the agent reads: the
// AGENTS.md convention (agents.md). CLAUDE.md, GEMINI.md and override files are not read.
const InstructionsFileName = "AGENTS.md"

// instructionsRecheck is how long a session's AGENTS.md files are trusted before they are
// checked again (one Stat each). Within it every model call gets the same block, so the
// prompt cache keeps hitting; a file the agent writes itself is re-read at once.
const instructionsRecheck = 10 * time.Second

// maxInstructionSessions bounds the sessions whose instructions are kept in memory; the
// least recently used one is dropped past it (it is read again when it comes back).
const maxInstructionSessions = 512

// instructionTools are the file tools whose path argument brings in the AGENTS.md of the
// sub-folders it reaches (A-229).
var instructionTools = map[string]bool{
	"read_file": true, "write_file": true, "edit_file": true, "list_files": true, "search_files": true,
}

// The status of one InstructionFile.
const (
	// InstructionLoaded: the whole file is in the agent's context.
	InstructionLoaded = "loaded"
	// InstructionCut: the file was cut at the byte limit.
	InstructionCut = "cut"
	// InstructionLeftOut: the file did not fit under the byte limit.
	InstructionLeftOut = "left_out"
	// InstructionUnreadable: the file could not be read (an offline runner).
	InstructionUnreadable = "unreadable"
)

// InstructionFile (A-230) is one AGENTS.md file of a session, for /instructions and the
// hosts' session API.
type InstructionFile struct {
	// Path is the file's logical path (e.g. /workspace/packages/api/AGENTS.md).
	Path string `json:"path"`
	// Size is the file's size in bytes (0 when unreadable).
	Size int `json:"size"`
	// SubFolder is true for a file loaded while the agent worked in a sub-folder (A-229),
	// false for the AGENTS.md of a folder the session works in.
	SubFolder bool `json:"subFolder"`
	// Status is InstructionLoaded, InstructionCut, InstructionLeftOut or
	// InstructionUnreadable.
	Status string `json:"status"`
}

// InstructionsState (A-230) is what a session's agent follows: whether project
// instructions are on, their byte limit, and the files, the folders' own first.
type InstructionsState struct {
	Enabled  bool `json:"enabled"`
	MaxBytes int  `json:"maxBytes"`
	// Folders are the folders the session works in (none = the agent's whole sandbox,
	// which has no project folder).
	Folders []string          `json:"folders"`
	Files   []InstructionFile `json:"files"`
}

// instructionsWorkspace is the sandbox side the project instructions read through: the
// SandboxManager.
type instructionsWorkspace interface {
	WorkspaceFolders(sessionID string) ([]string, bool)
	GetSessionSandbox(ctx context.Context, sessionID string) (sandboxhub.View, error)
}

// instructionFile is one AGENTS.md read from a session's workspace.
type instructionFile struct {
	path string
	// dir is the folder the file belongs to.
	dir string
	// size and modifiedAt are the Stat the content was read at.
	size       int
	modifiedAt int64
	// content is the text read, at most the byte limit at the time; truncated is true
	// when the file holds more.
	content   string
	truncated bool
	// checkedAt is when the file was last checked (a sub-folder file's own recheck).
	checkedAt time.Time
}

// instructionsSession is one session's AGENTS.md state. mu is held across the reads, so
// two calls of one session never read the same files at once.
type instructionsSession struct {
	mu       sync.Mutex
	lastUsed time.Time
	// folders are the folders the root files were read for; checkedAt is when; stale
	// forces the next call to check again (a file the agent wrote, a setting changed).
	folders   []string
	checkedAt time.Time
	stale     bool
	maxBytes  int
	// root maps a folder to its AGENTS.md (absent = the folder has none); unreadable is the
	// folders whose file could not be checked and that have nothing cached.
	root       map[string]*instructionFile
	unreadable []string
	// sub is the sub-folder files loaded (A-229), in load order; subByDir indexes them,
	// and noFile is the sub-folders checked that have none.
	sub      []*instructionFile
	subByDir map[string]*instructionFile
	noFile   map[string]bool
	// announced is the paths of the last SessionInstructionsChanged, so it is emitted
	// only on a change.
	announced []string
}

// ProjectInstructions (A-227) loads the AGENTS.md files of the folders a session works
// in, so the agent follows the project's own rules the way Claude Code and Codex do:
//
//   - The AGENTS.md of each folder the session works in (its runner's folders, or its
//     sandbox folders) goes into the system message as <project_instructions>, under a
//     byte limit (loopConfig.projectInstructionsMaxBytes, default 32 KiB).
//   - A sub-folder's AGENTS.md is added to the result of the first file tool call that
//     reaches it (AfterTool, A-229), once per session; one that later dropped out of the
//     conversation (compaction, cleared tool results) goes into the system block instead.
//
// The files are read through the session's sandbox view, so a runner (the user's
// computer) and a sandbox work the same. They are cached per session and checked again
// every instructionsRecheck, so the block stays byte-identical between changes.
// Sessions with neither a runner nor sandbox folders work in the agent's whole sandbox
// and get nothing.
type ProjectInstructions struct {
	agentID   string
	workspace instructionsWorkspace
	config    func() profile.ResolvedLoopConfig
	bus       events.Bus
	now       func() time.Time

	mu       sync.Mutex
	sessions map[string]*instructionsSession
}

// newProjectInstructions returns the project instructions of the agent agentID. config
// is read on every call, so a settings change applies to the next model call; bus may be
// nil (no change events).
func newProjectInstructions(agentID string, workspace instructionsWorkspace, config func() profile.ResolvedLoopConfig, bus events.Bus) *ProjectInstructions {
	return &ProjectInstructions{
		agentID: agentID, workspace: workspace, config: config, bus: bus,
		now: time.Now, sessions: map[string]*instructionsSession{},
	}
}

// session returns the session's state, created on first use; it drops the least
// recently used one past maxInstructionSessions.
func (p *ProjectInstructions) session(sessionID string) *instructionsSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[sessionID]
	if !ok {
		if len(p.sessions) >= maxInstructionSessions {
			var oldest string
			var at time.Time
			for id, other := range p.sessions {
				if oldest == "" || other.lastUsed.Before(at) {
					oldest, at = id, other.lastUsed
				}
			}
			delete(p.sessions, oldest)
		}
		s = &instructionsSession{root: map[string]*instructionFile{}, subByDir: map[string]*instructionFile{}, noFile: map[string]bool{}}
		p.sessions[sessionID] = s
	}
	s.lastUsed = p.now()
	return s
}

// Forget drops a session's state (the session was deleted).
func (p *ProjectInstructions) Forget(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, sessionID)
}

// Index is the <project_instructions> block of the session's system message; false = the
// session has no AGENTS.md (or none could be read and nothing is cached), or project
// instructions are off. msgs is the conversation the model is about to see: a sub-folder
// file whose note is no longer in it is put in the block.
func (p *ProjectInstructions) Index(ctx context.Context, sessionID string, msgs []kernel.Message) (string, bool) {
	cfg := p.config()
	if !cfg.ProjectInstructions || nilcheck.IsNil(p.workspace) {
		return "", false
	}
	folders, ok := p.workspace.WorkspaceFolders(sessionID)
	if !ok {
		return "", false
	}
	s := p.session(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	p.refresh(ctx, sessionID, s, folders, cfg.ProjectInstructionsMaxBytes)
	files := s.rootFiles(folders)
	for _, f := range s.sub {
		if !mentioned(msgs, f.path) {
			files = append(files, f)
		}
	}
	return renderInstructions(files, s.unreadable, cfg.ProjectInstructionsMaxBytes)
}

// State (A-230) is what the session's agent follows now, for /instructions and the hosts:
// the folders' files (checked again when they are due), then the sub-folder files
// loaded in this session.
func (p *ProjectInstructions) State(ctx context.Context, sessionID string) InstructionsState {
	cfg := p.config()
	state := InstructionsState{Enabled: cfg.ProjectInstructions, MaxBytes: cfg.ProjectInstructionsMaxBytes, Folders: []string{}, Files: []InstructionFile{}}
	if nilcheck.IsNil(p.workspace) {
		return state
	}
	folders, ok := p.workspace.WorkspaceFolders(sessionID)
	if !ok {
		return state
	}
	state.Folders = slices.Clone(folders)
	if !cfg.ProjectInstructions {
		return state
	}
	s := p.session(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	p.refresh(ctx, sessionID, s, folders, cfg.ProjectInstructionsMaxBytes)
	budget := cfg.ProjectInstructionsMaxBytes
	for _, f := range s.rootFiles(folders) {
		status := InstructionLoaded
		switch {
		case budget <= 0:
			status = InstructionLeftOut
		case len(strings.TrimRight(f.content, "\n")) > budget || f.truncated:
			status = InstructionCut
		}
		budget -= len(f.content)
		state.Files = append(state.Files, InstructionFile{Path: f.path, Size: f.size, Status: status})
	}
	for _, folder := range s.unreadable {
		state.Files = append(state.Files, InstructionFile{Path: path.Join(folder, InstructionsFileName), Status: InstructionUnreadable})
	}
	for _, f := range s.sub {
		status := InstructionLoaded
		if f.truncated {
			status = InstructionCut
		}
		state.Files = append(state.Files, InstructionFile{Path: f.path, Size: f.size, SubFolder: true, Status: status})
	}
	return state
}

// AfterTool (A-229, agentloop.ToolNotes) returns the AGENTS.md files a file tool call
// brought in: those of the sub-folders between the folder the session works in and the
// path the call touched, not loaded yet in this session (or changed since), as notes to
// add to the call's result. "" = none. A call that wrote an AGENTS.md makes it count as
// loaded without a note (the agent wrote it), and the folder's own is checked again at the
// next model call.
func (p *ProjectInstructions) AfterTool(ctx context.Context, sessionID, toolName string, args kernel.Arguments) string {
	if !instructionTools[toolName] {
		return ""
	}
	cfg := p.config()
	if !cfg.ProjectInstructions || nilcheck.IsNil(p.workspace) {
		return ""
	}
	folders, ok := p.workspace.WorkspaceFolders(sessionID)
	if !ok {
		return ""
	}
	view, err := p.workspace.GetSessionSandbox(ctx, sessionID)
	if err != nil || nilcheck.IsNil(view) {
		return ""
	}
	var call struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &call)
	raw := call.Path
	touched, err := sandboxhub.ResolveLogicalPath(raw, view.WorkingDir(), view.Roots(), view.ID())
	if err != nil {
		return ""
	}
	wrote := toolName == "write_file" || toolName == "edit_file"
	dir := path.Dir(touched)
	switch toolName {
	case "list_files":
		dir = touched
	case "search_files":
		if st, err := view.Stat(ctx, touched); err == nil && st != nil && st.Type == sandboxhub.EntryDirectory {
			dir = touched
		}
	}
	folder, ok := containingFolder(folders, dir)
	if !ok {
		return ""
	}
	s := p.session(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if wrote && path.Base(touched) == InstructionsFileName {
		if dir == folder {
			s.stale = true
			return ""
		}
		delete(s.noFile, dir)
		if f, ok := p.readFile(ctx, view, dir, cfg.ProjectInstructionsMaxBytes); ok && f != nil {
			s.putSub(f)
			p.announce(sessionID, s, folders)
		}
		return ""
	}
	var notes []string
	for _, d := range dirsBetween(folder, dir) {
		if s.noFile[d] {
			continue
		}
		known := s.subByDir[d]
		if known != nil && p.now().Sub(known.checkedAt) < instructionsRecheck {
			continue
		}
		st, err := view.Stat(ctx, path.Join(d, InstructionsFileName))
		if err != nil {
			continue
		}
		if st == nil || st.Type != sandboxhub.EntryFile {
			if known != nil {
				s.dropSub(d)
			}
			s.noFile[d] = true
			continue
		}
		if known != nil && known.size == st.Size && known.modifiedAt == st.ModifiedAt {
			known.checkedAt = p.now()
			continue
		}
		f, ok := p.readFile(ctx, view, d, cfg.ProjectInstructionsMaxBytes)
		if !ok || f == nil {
			continue
		}
		s.putSub(f)
		notes = append(notes, subNote(f, known != nil))
	}
	if len(notes) == 0 {
		return ""
	}
	p.announce(sessionID, s, folders)
	return strings.Join(notes, "\n\n")
}

// Invalidate makes the next call of every session check its files again (a setting
// changed).
func (p *ProjectInstructions) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		s.mu.Lock()
		s.stale = true
		s.mu.Unlock()
	}
}

// refresh checks the folders' AGENTS.md files again when they are due: the folders or
// the byte limit changed, the session was marked stale, or instructionsRecheck passed.
// A file that cannot be checked keeps what was read before; with nothing cached its
// folder is unreadable. Called with s.mu held.
func (p *ProjectInstructions) refresh(ctx context.Context, sessionID string, s *instructionsSession, folders []string, maxBytes int) {
	now := p.now()
	if !s.stale && slices.Equal(s.folders, folders) && s.maxBytes == maxBytes && !s.checkedAt.IsZero() && now.Sub(s.checkedAt) < instructionsRecheck {
		return
	}
	limitChanged := s.maxBytes != maxBytes
	s.folders, s.checkedAt, s.stale, s.maxBytes = slices.Clone(folders), now, false, maxBytes
	s.unreadable = nil
	for folder := range s.root {
		if !slices.Contains(folders, folder) {
			delete(s.root, folder)
		}
	}
	view, err := p.workspace.GetSessionSandbox(ctx, sessionID)
	if err != nil || nilcheck.IsNil(view) {
		for _, folder := range folders {
			if s.root[folder] == nil {
				s.unreadable = append(s.unreadable, folder)
			}
		}
		p.announce(sessionID, s, folders)
		return
	}
	for _, folder := range folders {
		known := s.root[folder]
		st, err := view.Stat(ctx, path.Join(folder, InstructionsFileName))
		switch {
		case err != nil:
			if known == nil {
				s.unreadable = append(s.unreadable, folder)
			}
			continue
		case st == nil || st.Type != sandboxhub.EntryFile:
			delete(s.root, folder)
			continue
		case known != nil && !limitChanged && known.size == st.Size && known.modifiedAt == st.ModifiedAt:
			continue
		}
		f, ok := p.readFile(ctx, view, folder, maxBytes)
		switch {
		case !ok && known == nil:
			s.unreadable = append(s.unreadable, folder)
		case ok && f == nil:
			delete(s.root, folder)
		case ok:
			s.root[folder] = f
		}
	}
	p.announce(sessionID, s, folders)
}

// readFile reads dir's AGENTS.md, at most maxBytes of it. ok is false when it could not be
// read; a nil file with ok true means there is none.
func (p *ProjectInstructions) readFile(ctx context.Context, view sandboxhub.View, dir string, maxBytes int) (*instructionFile, bool) {
	file := path.Join(dir, InstructionsFileName)
	st, err := view.Stat(ctx, file)
	if err != nil {
		return nil, false
	}
	if st == nil || st.Type != sandboxhub.EntryFile {
		return nil, true
	}
	limit := maxBytes
	read, err := view.ReadFile(ctx, file, sandboxhub.ReadOptions{MaxBytes: &limit})
	if err != nil {
		return nil, false
	}
	content := strings.ToValidUTF8(read.Content, "")
	if strings.ContainsRune(content, 0) {
		return nil, true
	}
	return &instructionFile{
		path: file, dir: dir, size: st.Size, modifiedAt: st.ModifiedAt,
		content: content, truncated: read.Truncated || read.Size > len(read.Content),
		checkedAt: p.now(),
	}, true
}

// announce emits SessionInstructionsChanged when the session's files differ from the last
// ones announced. Called with s.mu held.
func (p *ProjectInstructions) announce(sessionID string, s *instructionsSession, folders []string) {
	var paths []string
	for _, f := range s.rootFiles(folders) {
		paths = append(paths, f.path)
	}
	for _, f := range s.sub {
		paths = append(paths, f.path)
	}
	if slices.Equal(paths, s.announced) {
		return
	}
	s.announced = paths
	if nilcheck.IsNil(p.bus) {
		return
	}
	events.Emit(p.bus, events.SessionInstructionsChanged, sessionID, events.SessionInstructionsChangedPayload{
		AgentID: p.agentID, SessionID: sessionID, Paths: append([]string{}, paths...),
	})
}

// rootFiles are the folders' files in the folders' order.
func (s *instructionsSession) rootFiles(folders []string) []*instructionFile {
	var files []*instructionFile
	for _, folder := range folders {
		if f := s.root[folder]; f != nil {
			files = append(files, f)
		}
	}
	return files
}

// putSub records a loaded sub-folder file, replacing the one of its folder in place.
func (s *instructionsSession) putSub(f *instructionFile) {
	delete(s.noFile, f.dir)
	if old := s.subByDir[f.dir]; old != nil {
		for i, other := range s.sub {
			if other == old {
				s.sub[i] = f
			}
		}
	} else {
		s.sub = append(s.sub, f)
	}
	s.subByDir[f.dir] = f
}

// dropSub forgets the sub-folder file of dir (it was deleted).
func (s *instructionsSession) dropSub(dir string) {
	old := s.subByDir[dir]
	delete(s.subByDir, dir)
	s.sub = slices.DeleteFunc(s.sub, func(f *instructionFile) bool { return f == old })
}

// containingFolder is the folder of folders that dir is in (or is).
func containingFolder(folders []string, dir string) (string, bool) {
	best := ""
	for _, f := range folders {
		if (dir == f || strings.HasPrefix(dir, strings.TrimSuffix(f, "/")+"/")) && len(f) > len(best) {
			best = f
		}
	}
	return best, best != ""
}

// dirsBetween is the folders below folder down to dir, outermost first: folder itself is
// left out (its file is in the system message), dir is included.
func dirsBetween(folder, dir string) []string {
	var dirs []string
	for d := dir; d != folder && len(d) > len(folder); d = path.Dir(d) {
		dirs = append(dirs, d)
	}
	slices.Reverse(dirs)
	return dirs
}

// noteMarker starts a sub-folder file's note in a tool result; the system block looks for
// it to tell whether the note is still in the conversation.
func noteMarker(filePath string) string {
	return `<project_instructions path="` + filePath + `">`
}

// mentioned reports whether a note for filePath is still in msgs.
func mentioned(msgs []kernel.Message, filePath string) bool {
	marker := noteMarker(filePath)
	for _, m := range msgs {
		if strings.Contains(m.Content, marker) {
			return true
		}
		for _, r := range m.ToolResults {
			if strings.Contains(r.Content, marker) {
				return true
			}
		}
	}
	return false
}

// subNote is the note a tool result carries for a sub-folder file.
func subNote(f *instructionFile, changed bool) string {
	intro := "The AGENTS.md of " + f.dir + ": follow it for the files under that folder. It adds to the instructions of the folders above it and wins where they disagree."
	if changed {
		intro = "The AGENTS.md of " + f.dir + " changed; this replaces what it said before. Follow it for the files under that folder."
	}
	body := strings.TrimRight(f.content, "\n")
	if f.truncated {
		body += "\n[cut: the file is longer - read the rest with read_file]"
	}
	return noteMarker(f.path) + "\n" + intro + "\n\n" + body + "\n</project_instructions>"
}

// renderInstructions is the <project_instructions> block: the files in order under the
// byte limit (the one that crosses it is cut, the rest are named as left out), then the
// folders whose file could not be read. False = nothing to say.
func renderInstructions(files []*instructionFile, unreadable []string, maxBytes int) (string, bool) {
	if len(files) == 0 && len(unreadable) == 0 {
		return "", false
	}
	lines := []string{"<project_instructions>"}
	if len(files) > 0 {
		lines = append(lines,
			"These are the AGENTS.md files of the folders you work in: follow them. A file in a deeper folder adds to the ones above it and wins where they disagree, for the files under its folder. What the user asks in the chat wins over all of them.",
			"An AGENTS.md in a sub-folder is shown to you the first time a file tool reaches that folder.")
	}
	budget := maxBytes
	var leftOut []string
	for _, f := range files {
		if budget <= 0 {
			leftOut = append(leftOut, f.path)
			continue
		}
		body := strings.TrimRight(f.content, "\n")
		cut := f.truncated
		if len(body) > budget {
			body, cut = cutUTF8(body, budget), true
		}
		budget -= len(f.content)
		if cut {
			body += "\n[cut at the instructions limit: read the rest with read_file]"
		}
		lines = append(lines, "", `<file path="`+f.path+`">`, body, "</file>")
	}
	if len(leftOut) > 0 {
		lines = append(lines, "", "Left out over the "+strconv.Itoa(maxBytes)+"-byte instructions limit (read them with read_file when you work there): "+strings.Join(leftOut, ", "))
	}
	if len(unreadable) > 0 {
		names := make([]string, len(unreadable))
		for i, folder := range unreadable {
			names[i] = path.Join(folder, InstructionsFileName)
		}
		lines = append(lines, "", "Could not be read (the folder is offline or unreachable): "+strings.Join(names, ", ")+". Read it with read_file once it is back.")
	}
	lines = append(lines, "</project_instructions>")
	return strings.Join(lines, "\n"), true
}

// cutUTF8 cuts s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
