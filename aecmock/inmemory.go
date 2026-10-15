package aecmock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// In-memory sandbox - a complete sandboxhub.View with no Docker, for tests and for
// developing tools against the contract (A-59).
//
// Files live in an insertion-ordered store keyed by logical path; symlinks are
// supported so escape checks can be tested. Exec understands a few POSIX
// commands: echo, pwd, cat, printenv, true, false, sleep, yes - enough to
// exercise timeouts, output caps, cancellation and env isolation. Anything else
// exits 127.
//
// The golden fixtures inmemory-sandbox-files.json and inmemory-sandbox-exec.json
// pin its behaviour: the results, error codes, error messages and (with the same
// injected clock) every timestamp must match the recorded values.
// ---------------------------------------------------------------------------

// Compile-time proofs of the sandbox contracts.
var (
	_ sandboxhub.Provider            = (*InMemoryProvider)(nil)
	_ sandboxhub.ProviderDataRemover = (*InMemoryProvider)(nil)
	_ sandboxhub.Sandbox             = (*InMemorySandbox)(nil)
	_ sandboxhub.AgentDataRemover    = (*InMemorySandbox)(nil)
	_ sandboxhub.View                = (*inMemoryView)(nil)
)

// inMemoryDefaultLimits are the default limits of an in-memory sandbox.
var inMemoryDefaultLimits = sandboxhub.Limits{
	MaxReadBytes:       1_000_000,
	MaxWriteBytes:      1_000_000,
	MaxListEntries:     1_000,
	MaxSearchMatches:   200,
	MaxExecTimeoutMs:   30_000,
	MaxExecOutputBytes: 256_000,
}

// allInMemoryCapabilities are the default capabilities, in declared order. There is
// no exec.shell, which is what the conformance suite's last test relies on.
func allInMemoryCapabilities() []sandboxhub.Capability {
	return []sandboxhub.Capability{
		sandboxhub.CapabilityFSRead, sandboxhub.CapabilityFSWrite, sandboxhub.CapabilityFSSearch, sandboxhub.CapabilityExec,
	}
}

// inMemoryConfig is what the options of NewInMemoryProvider set (the provider
// options plus the clock).
type inMemoryConfig struct {
	kind         sandboxhub.Kind
	limits       sandboxhub.Limits
	capabilities []sandboxhub.Capability // nil means all four
	failCreates  int
	now          func() int64
}

// InMemoryOption changes how an InMemoryProvider behaves. Options are applied in
// order.
type InMemoryOption func(*inMemoryConfig)

// WithKind sets the kind the provider registers as. The default is docker, so it
// can stand in for the Docker provider.
func WithKind(k sandboxhub.Kind) InMemoryOption {
	return func(c *inMemoryConfig) { c.kind = k }
}

// WithLimits overlays the given limits on the defaults: a field the patch sets
// replaces the default, the others keep it.
func WithLimits(p sandboxhub.LimitsPatch) InMemoryOption {
	return func(c *inMemoryConfig) { c.limits = c.limits.Apply(p) }
}

// WithCapabilities sets the capabilities of every sandbox the provider creates.
// With no argument the sandboxes have none; without the
// option they have the four defaults.
func WithCapabilities(caps ...sandboxhub.Capability) InMemoryOption {
	return func(c *inMemoryConfig) { c.capabilities = append([]sandboxhub.Capability{}, caps...) }
}

// WithFailCreates fails the first n Create calls (simulates Docker still
// starting).
func WithFailCreates(n int) InMemoryOption {
	return func(c *inMemoryConfig) { c.failCreates = n }
}

// WithClock replaces the clock (epoch milliseconds) that stamps file modification
// times and exec durations. Tests inject wiretest.Clock to get the same timestamps
// as the recorded fixtures. The provider serialises every call to
// it, so the function itself need not be safe for concurrent use.
func WithClock(now func() int64) InMemoryOption {
	return func(c *inMemoryConfig) {
		if now != nil {
			c.now = now
		}
	}
}

// RemovedData is one RemoveData call a provider recorded: the sandbox id and, for
// a shared sandbox, the agent whose folder was removed.
type RemovedData struct {
	ID      string
	AgentID string
}

// InMemoryProvider creates InMemorySandbox values.
// It is a sandboxhub.Provider, a sandboxhub.ProviderDataRemover and a kernel.Plugin
// named sandbox:in-memory, and it is safe for concurrent use.
type InMemoryProvider struct {
	cfg inMemoryConfig

	// clockMu serialises the injected clock. It is held only around the call.
	clockMu sync.Mutex

	mu           sync.Mutex
	sandboxes    map[string]*InMemorySandbox
	createCount  int
	failuresLeft int
	removed      []RemovedData
}

// NewInMemoryProvider returns a provider with the given options. With none it
// registers as docker, uses the default limits and the four default
// capabilities, and reads the wall clock.
func NewInMemoryProvider(opts ...InMemoryOption) *InMemoryProvider {
	cfg := inMemoryConfig{kind: sandboxhub.KindDocker, limits: inMemoryDefaultLimits, now: kernel.NowMillis}
	for _, o := range opts {
		o(&cfg)
	}
	return &InMemoryProvider{
		cfg:          cfg,
		sandboxes:    make(map[string]*InMemorySandbox),
		failuresLeft: cfg.failCreates,
	}
}

// Name returns the plugin name, sandbox:in-memory.
func (*InMemoryProvider) Name() string { return "sandbox:in-memory" }

// Version returns the plugin version, 1.0.0.
func (*InMemoryProvider) Version() string { return "1.0.0" }

// Kind returns the kind the provider registers as.
func (p *InMemoryProvider) Kind() sandboxhub.Kind { return p.cfg.kind }

// tick reads the injected clock. Calls are serialised; the lock is never held
// while anything else runs.
func (p *InMemoryProvider) tick() int64 {
	p.clockMu.Lock()
	defer p.clockMu.Unlock()
	return p.cfg.now()
}

// Create builds an in-memory sandbox and remembers it by id. The first
// WithFailCreates calls fail with a plain error, simulated provider failure.
func (p *InMemoryProvider) Create(_ context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (sandboxhub.Sandbox, error) {
	p.mu.Lock()
	p.createCount++
	if p.failuresLeft > 0 {
		p.failuresLeft--
		p.mu.Unlock()
		return nil, errors.New("simulated provider failure")
	}
	p.mu.Unlock()

	network := spec.Network
	if network == "" {
		network = sandboxhub.NetworkNone
	}
	traits := sandboxhub.Traits{
		Kind:       p.cfg.kind,
		Isolated:   p.cfg.kind != sandboxhub.KindHost,
		Network:    network,
		Persistent: false,
		Tenancy:    cc.Tenancy,
		OS:         sandboxhub.OSLinux,
	}
	caps := p.cfg.capabilities
	if caps == nil {
		caps = allInMemoryCapabilities()
	}
	env := newInMemorySandbox(cc.ID, traits, p.cfg.limits, caps, p.tick)

	p.mu.Lock()
	p.sandboxes[cc.ID] = env
	p.mu.Unlock()
	return env, nil
}

// NewInMemoryView returns an agent view of a fresh shared in-memory sandbox with
// the given sandbox id: a provider with the default clock, one Create with a docker
// spec and shared tenancy, and ForAgent for agentID. The working directory is
// /workspace/agents/<agentID> and it is the only root; relative paths resolve
// against it and a path that resolves outside it fails with path_escape. The view
// is safe for concurrent use (every operation takes the sandbox mutex). It replaces
// the Phase 6 FileView stop-gap, so a test that needs a view of one agent but
// neither the provider nor the sandbox calls this; a test that must plant files,
// inject a clock or inspect removals builds an InMemoryProvider itself. It takes no
// *testing.T because aecmock non-test code may not import testing.
func NewInMemoryView(id, agentID string) (sandboxhub.View, error) {
	env, err := NewInMemoryProvider().Create(context.Background(),
		sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: id, Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		return nil, err
	}
	return env.ForAgent(context.Background(), agentID)
}

// Sandbox returns the sandbox this provider created under id, if it still holds
// it - lets tests inspect or plant files.
func (p *InMemoryProvider) Sandbox(id string) (*InMemorySandbox, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	env, ok := p.sandboxes[id]
	return env, ok
}

// CreateCount is the number of Create calls so far, failed ones included.
func (p *InMemoryProvider) CreateCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.createCount
}

// RemovedData returns a copy of the data removals the provider recorded - data of
// sandboxes that are not running, recorded so tests can assert cleanup.
func (p *InMemoryProvider) RemovedData() []RemovedData {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.removed)
}

// RemoveData records the call, then removes the data: one agent's folder from a
// shared sandbox when agentID is set, otherwise the whole sandbox.
func (p *InMemoryProvider) RemoveData(ctx context.Context, _ sandboxhub.Spec, cc sandboxhub.CreateContext, agentID string) error {
	p.mu.Lock()
	p.removed = append(p.removed, RemovedData{ID: cc.ID, AgentID: agentID})
	env, ok := p.sandboxes[cc.ID]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	if agentID != "" && cc.Tenancy == sandboxhub.TenancyShared {
		return env.RemoveAgentData(ctx, agentID)
	}
	p.mu.Lock()
	delete(p.sandboxes, cc.ID)
	p.mu.Unlock()
	return nil
}

// memNode is one entry of the store: a file, a directory or a symlink with its
// modification time.
type memNode struct {
	kind    sandboxhub.EntryType
	content string // file
	target  string // symlink
	mtime   int64
}

// memStore is a map of nodes that remembers insertion order. Setting a key that
// exists keeps its place and deleting a key then
// setting it again appends it. List and search stable-sort a walk of the keys in
// this order, so the order is behaviour.
type memStore struct {
	nodes map[string]memNode
	keys  []string
}

func newMemStore() memStore { return memStore{nodes: make(map[string]memNode)} }

func (s *memStore) get(key string) (memNode, bool) {
	n, ok := s.nodes[key]
	return n, ok
}

func (s *memStore) set(key string, n memNode) {
	if _, ok := s.nodes[key]; !ok {
		s.keys = append(s.keys, key)
	}
	s.nodes[key] = n
}

func (s *memStore) clear() {
	s.nodes = make(map[string]memNode)
	s.keys = nil
}

// deleteWhere removes every key for which drop reports true, keeping the order of
// the rest.
func (s *memStore) deleteWhere(drop func(key string) bool) {
	kept := s.keys[:0]
	for _, k := range s.keys {
		if drop(k) {
			delete(s.nodes, k)
			continue
		}
		kept = append(kept, k)
	}
	s.keys = kept
}

// InMemorySandbox is one in-memory sandbox: a shared or own workspace whose agent
// views read, write, stat, list, search and exec over an in-memory file tree. All
// of its state sits under one mutex, which is never
// held while a mini-exec command runs, so cat and rm can call back into the view.
type InMemorySandbox struct {
	id     string
	traits sandboxhub.Traits
	limits sandboxhub.Limits
	caps   []sandboxhub.Capability
	tick   func() int64

	mu       sync.Mutex
	store    memStore
	disposed bool
}

func newInMemorySandbox(id string, traits sandboxhub.Traits, limits sandboxhub.Limits, caps []sandboxhub.Capability, tick func() int64) *InMemorySandbox {
	// A set keeps the first occurrence of a capability.
	var unique []sandboxhub.Capability
	for _, c := range caps {
		if !slices.Contains(unique, c) {
			unique = append(unique, c)
		}
	}
	s := &InMemorySandbox{id: id, traits: traits, limits: limits, caps: unique, tick: tick, store: newMemStore()}
	s.reset()
	return s
}

// reset empties the store and stamps the two root directories (two clock reads).
// The caller owns s or holds mu.
func (s *InMemorySandbox) reset() {
	s.store.clear()
	for _, dir := range []string{sandboxhub.WorkspaceRoot, sandboxhub.AgentsDir} {
		s.store.set(dir, memNode{kind: sandboxhub.EntryDirectory, mtime: s.tick()})
	}
}

func (s *InMemorySandbox) hasCapability(c sandboxhub.Capability) bool {
	return slices.Contains(s.caps, c)
}

// ID returns the sandbox id the hub assigned.
func (s *InMemorySandbox) ID() string { return s.id }

// Traits returns the sandbox traits.
func (s *InMemorySandbox) Traits() sandboxhub.Traits { return s.traits }

// ForAgent returns the view one agent works through: the working directory and
// roots of sandboxhub.AgentLayout, with the working directory created.
func (s *InMemorySandbox) ForAgent(_ context.Context, agentID string) (sandboxhub.View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disposed {
		return nil, kernel.NewSandboxError(
			fmt.Sprintf("Sandbox \"%s\" was disposed", s.id), kernel.SandboxCodeEnvironmentUnavailable, s.id)
	}
	layout, err := sandboxhub.AgentLayout(s.traits.Tenancy, agentID)
	if err != nil {
		return nil, err
	}
	s.mkdirp(layout.WorkingDir)
	return &inMemoryView{sb: s, workingDir: layout.WorkingDir, roots: layout.Roots}, nil
}

// Health is ready until the sandbox is disposed.
func (s *InMemorySandbox) Health(context.Context) sandboxhub.Health {
	if s.Disposed() {
		return sandboxhub.HealthUnavailable
	}
	return sandboxhub.HealthReady
}

// Disposed reports whether Dispose was called.
func (s *InMemorySandbox) Disposed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disposed
}

// Has reports whether the store holds an entry at the absolute logical path. It is
// a test helper that reads the store directly, so it also
// works after Dispose, when every view operation fails.
func (s *InMemorySandbox) Has(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.store.get(p)
	return ok
}

// RemoveAgentData deletes one agent's folder and everything below it.
func (s *InMemorySandbox) RemoveAgentData(_ context.Context, agentID string) error {
	dir := sandboxhub.AgentsDir + "/" + agentID
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.deleteWhere(func(key string) bool { return key == dir || strings.HasPrefix(key, dir+"/") })
	return nil
}

// Dispose marks the sandbox disposed; every later operation fails with
// environment_unavailable. With removeData the store is reset (two clock reads).
func (s *InMemorySandbox) Dispose(_ context.Context, removeData bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disposed = true
	if removeData {
		s.reset()
	}
	return nil
}

// Symlink is a test helper: it creates a symlink (the target may point anywhere -
// that is the point). The link path must be an absolute logical path; any other
// path returns an error instead of storing a path no view can reach.
func (s *InMemorySandbox) Symlink(link, target string) error {
	if !strings.HasPrefix(link, "/") {
		return fmt.Errorf("aecmock: symlink %q must be an absolute logical path", link)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirp(path.Dir(link))
	s.store.set(link, memNode{kind: sandboxhub.EntrySymlink, target: target, mtime: s.tick()})
	return nil
}

// PlantFile is a test helper: it plants a file at any logical path (outside the
// agent's roots too). The path must be absolute.
func (s *InMemorySandbox) PlantFile(p, content string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("aecmock: planted file %q must be an absolute logical path", p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirp(path.Dir(p))
	s.store.set(p, memNode{kind: sandboxhub.EntryFile, content: content, mtime: s.tick()})
	return nil
}

// mkdirp creates every missing directory on the way to dir, reading the clock once
// per missing segment. A segment that already exists is left as it is, whatever it
// is. The caller holds mu (or owns s).
func (s *InMemorySandbox) mkdirp(dir string) {
	current := ""
	for _, part := range splitNonEmpty(dir) {
		current += "/" + part
		if _, ok := s.store.get(current); !ok {
			s.store.set(current, memNode{kind: sandboxhub.EntryDirectory, mtime: s.tick()})
		}
	}
}

// splitNonEmpty is s.split("/").filter(Boolean).
func splitNonEmpty(s string) []string {
	var parts []string
	for _, p := range strings.Split(s, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
