// Package subagenthub is the global library of subagent types (A-105, design revision)
// over persistence.SubagentTypeStore, plus the built-in types every server has
// (builtin.go) - the SubagentHub of core/ts.
//
// Built-in and app types are registered in memory; custom types live in the
// persistence.SubagentTypeStore returned by the getStore function on every load and
// write, so AgentCore.UsePersistence can swap the store (and calls Reload). The package
// lives apart from subagenttype because it imports persistence, and persistence imports
// subagenttype.
package subagenthub

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// errNoStore is returned when getStore yields no store.
var errNoStore = errors.New("subagenthub: the persistence subagent type store is not configured")

// SubagentHub is the global library of subagent types - like the SkillHub. Two kinds of
// entries:
//   - registered (built-in + app types): in memory, read-only at runtime;
//   - custom (created by users): persisted in the SubagentTypeStore, loaded on first use
//     and cached - write custom types through this hub so the cache stays current.
//
// Registered names win: a stored custom type with the same name is ignored (logged).
// Agents enable types by name (AgentProfile.Subagents.Types); deleting a custom type
// publishes subagent-type:deleted (WithEvents), and AgentCore drops it from every agent.
//
// Concurrency: Create, Update, Delete and the lazy load of the custom types are
// serialised by one mutex, so a check-then-save cannot interleave; Reload only
// invalidates the cache and never waits. Every Type the hub returns is a deep copy. It
// is safe for concurrent use.
type SubagentHub struct {
	getStore func() persistence.SubagentTypeStore
	now      func() int64
	logger   *slog.Logger
	events   events.Bus // set by WithEvents; nil means Delete publishes nothing

	// opMu serialises Create, Update, Delete and the lazy load. Lock order: opMu, then mu.
	opMu sync.Mutex

	// mu guards everything below. Store I/O never runs while it is held.
	mu         sync.RWMutex
	registered map[string]subagenttype.Type
	custom     map[string]subagenttype.Type
	loaded     bool
	generation uint64
}

// NewSubagentHub returns an empty hub. getStore is resolved on every load and write, so
// AgentCore.UsePersistence can swap the store.
func NewSubagentHub(getStore func() persistence.SubagentTypeStore, opts ...Option) *SubagentHub {
	h := &SubagentHub{
		getStore:   getStore,
		now:        kernel.NowMillis,
		logger:     slog.Default(),
		registered: map[string]subagenttype.Type{},
		custom:     map[string]subagenttype.Type{},
	}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	return h
}

// Register adds a built-in or app type. An empty source means app; SourceCustom (or any
// unknown source) is an error, because custom types are made with Create. It returns a
// *subagent.Error when the definition is invalid ("invalid_argument") or the name is
// taken ("already_exists").
func (h *SubagentHub) Register(def subagenttype.Definition, source subagenttype.Source) (subagenttype.Type, error) {
	if source == "" {
		source = subagenttype.SourceApp
	}
	if source != subagenttype.SourceBuiltIn && source != subagenttype.SourceApp {
		return subagenttype.Type{}, subagent.NewError(`cannot register a "`+string(source)+`" subagent type, only built-in and app (use Create for custom types)`, subagent.CodeInvalidArgument)
	}
	t, err := ToType(def, source)
	if err != nil {
		return subagenttype.Type{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, taken := h.registered[t.Name]; taken {
		return subagenttype.Type{}, subagent.NewError(`Subagent type "`+t.Name+`" already registered`, subagent.CodeAlreadyExists)
	}
	h.registered[t.Name] = t
	return t.Clone(), nil
}

// Get returns a registered or custom type by name. The bool is false when the library
// has no such type.
func (h *SubagentHub) Get(ctx context.Context, name string) (subagenttype.Type, bool, error) {
	h.mu.RLock()
	t, ok := h.registered[name]
	h.mu.RUnlock()
	if ok {
		return t.Clone(), true, nil
	}
	var found subagenttype.Type
	var hit bool
	err := h.readLoaded(ctx, func() {
		if c, has := h.custom[name]; has {
			found, hit = c.Clone(), true
		}
	})
	if err != nil {
		return subagenttype.Type{}, false, err
	}
	return found, hit, nil
}

// List returns every type, registered first, each group sorted by name.
func (h *SubagentHub) List(ctx context.Context) ([]subagenttype.Type, error) {
	var out []subagenttype.Type
	err := h.readLoaded(ctx, func() {
		out = make([]subagenttype.Type, 0, len(h.registered)+len(h.custom))
		out = appendSorted(out, h.registered)
		out = appendSorted(out, h.custom)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Create adds a custom type. It returns a *subagent.Error ("invalid_argument",
// "already_exists").
func (h *SubagentHub) Create(ctx context.Context, def subagenttype.Definition) (subagenttype.Type, error) {
	t, err := ToType(def, subagenttype.SourceCustom)
	if err != nil {
		return subagenttype.Type{}, err
	}
	now := kernel.MillisToISO(h.now())
	t.CreatedAt, t.UpdatedAt = now, now

	h.opMu.Lock()
	defer h.opMu.Unlock()
	if err := h.ensureLoadedLocked(ctx); err != nil {
		return subagenttype.Type{}, err
	}
	if h.exists(t.Name) {
		return subagenttype.Type{}, subagent.NewError(`A subagent type named "`+t.Name+`" already exists`, subagent.CodeAlreadyExists)
	}
	if err := h.save(ctx, t); err != nil {
		return subagenttype.Type{}, err
	}
	h.cache(t)
	return t.Clone(), nil
}

// Update replaces a custom type's definition; its name stays (def.Name is ignored).
// Built-in and app types can't be changed ("already_exists"); an unknown name is
// "not_found".
func (h *SubagentHub) Update(ctx context.Context, name string, def subagenttype.Definition) (subagenttype.Type, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	existing, err := h.requireCustom(ctx, name)
	if err != nil {
		return subagenttype.Type{}, err
	}
	def.Name = name
	t, err := ToType(def, subagenttype.SourceCustom)
	if err != nil {
		return subagenttype.Type{}, err
	}
	t.CreatedAt = existing.CreatedAt
	t.UpdatedAt = kernel.MillisToISO(h.now())
	if err := h.save(ctx, t); err != nil {
		return subagenttype.Type{}, err
	}
	h.cache(t)
	return t.Clone(), nil
}

// Delete removes a custom type, then publishes events.SubagentTypeDeleted (WithEvents)
// with ctx and returns once its subscribers returned - so an agent that had it enabled
// has already lost it.
func (h *SubagentHub) Delete(ctx context.Context, name string) error {
	if err := h.deleteLocked(ctx, name); err != nil {
		return err
	}
	if h.events != nil {
		events.Publish(ctx, h.events, events.SubagentTypeDeleted, "", events.SubagentTypeDeletedPayload{Name: name})
	}
	return nil
}

// deleteLocked removes the type under the mutation mutex.
func (h *SubagentHub) deleteLocked(ctx context.Context, name string) error {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	if _, err := h.requireCustom(ctx, name); err != nil {
		return err
	}
	store, err := h.store()
	if err != nil {
		return err
	}
	if err := store.Delete(ctx, name); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.custom, name)
	return nil
}

// Reload forgets the cached custom types (the store was swapped); the next read loads
// them again. A load that was running when Reload was called discards its result.
func (h *SubagentHub) Reload() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.generation++
	h.loaded = false
	h.custom = map[string]subagenttype.Type{}
}

// exists reports whether the name is taken by a registered or a cached custom type.
// The caller holds opMu and has loaded the custom types.
func (h *SubagentHub) exists(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, reg := h.registered[name]
	_, cus := h.custom[name]
	return reg || cus
}

// cache puts a saved custom type in the cache. When a Reload dropped the cache in the
// meantime the next load reads the saved record from the store instead.
func (h *SubagentHub) cache(t subagenttype.Type) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loaded {
		h.custom[t.Name] = t
	}
}

// save writes a custom type to the store resolved right now.
func (h *SubagentHub) save(ctx context.Context, t subagenttype.Type) error {
	store, err := h.store()
	if err != nil {
		return err
	}
	return store.Save(ctx, t.Clone())
}

// store resolves the persistence subagent type store.
func (h *SubagentHub) store() (persistence.SubagentTypeStore, error) {
	if h.getStore == nil {
		return nil, errNoStore
	}
	store := h.getStore()
	if nilcheck.IsNil(store) {
		return nil, errNoStore
	}
	return store, nil
}

// requireCustom returns the cached custom type, or the already_exists (registered) /
// not_found error. The caller holds opMu.
func (h *SubagentHub) requireCustom(ctx context.Context, name string) (subagenttype.Type, error) {
	h.mu.RLock()
	reg, isRegistered := h.registered[name]
	h.mu.RUnlock()
	if isRegistered {
		return subagenttype.Type{}, subagent.NewError(`"`+name+`" is a `+string(reg.Source)+` subagent type and can't be changed`, subagent.CodeAlreadyExists)
	}
	if err := h.ensureLoadedLocked(ctx); err != nil {
		return subagenttype.Type{}, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	t, ok := h.custom[name]
	if !ok {
		return subagenttype.Type{}, subagent.NewError(`No subagent type named "`+name+`"`, subagent.CodeNotFound)
	}
	return t, nil
}

// readLoaded runs read with the read lock held and the custom types loaded. When a
// Reload slips in between the load and the lock, the types are loaded again; every
// repeat first looks at ctx. read runs exactly once.
func (h *SubagentHub) readLoaded(ctx context.Context, read func()) error {
	for {
		if err := h.ensureLoaded(ctx); err != nil {
			return err
		}
		h.mu.RLock()
		if !h.loaded {
			h.mu.RUnlock()
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		read()
		h.mu.RUnlock()
		return nil
	}
}

// ensureLoaded loads the custom types from the store when they are not cached.
func (h *SubagentHub) ensureLoaded(ctx context.Context) error {
	h.mu.RLock()
	loaded := h.loaded
	h.mu.RUnlock()
	if loaded {
		return nil
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	return h.ensureLoadedLocked(ctx)
}

// ensureLoadedLocked is ensureLoaded for a caller that holds opMu. A failed load is not
// cached - the next read retries. A Reload during the read makes it read again.
func (h *SubagentHub) ensureLoadedLocked(ctx context.Context) error {
	for {
		h.mu.RLock()
		loaded, generation := h.loaded, h.generation
		h.mu.RUnlock()
		if loaded {
			return nil
		}
		custom, err := h.readStore(ctx)
		if err != nil {
			return err
		}
		h.mu.Lock()
		if h.generation == generation {
			h.custom = custom
			h.loaded = true
			h.mu.Unlock()
			return nil
		}
		h.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// readStore reads every stored type. One shadowed by a registered type is skipped with
// a warning.
func (h *SubagentHub) readStore(ctx context.Context) (map[string]subagenttype.Type, error) {
	store, err := h.store()
	if err != nil {
		return nil, err
	}
	records, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]subagenttype.Type, len(records))
	for _, t := range records {
		h.mu.RLock()
		reg, shadowed := h.registered[t.Name]
		h.mu.RUnlock()
		if shadowed {
			h.logger.Warn(`[subagents] stored type "` + t.Name + `" is shadowed by a ` + string(reg.Source) + ` type and ignored`)
			continue
		}
		t = t.Clone()
		t.Source = subagenttype.SourceCustom
		out[t.Name] = t
	}
	return out, nil
}

// appendSorted appends copies of the types of m, sorted by name.
func appendSorted(out []subagenttype.Type, m map[string]subagenttype.Type) []subagenttype.Type {
	start := len(out)
	for _, t := range m {
		out = append(out, t.Clone())
	}
	slices.SortFunc(out[start:], func(a, b subagenttype.Type) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// namePattern is the shape of a type name: lowercase letters, digits and dashes.
var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ToType returns a definition checked and normalised (toType of core/ts), or a
// *subagent.Error with CodeInvalidArgument. Strings are trimmed; the tools are trimmed
// and de-duplicated (nil stays nil: all of the parent's tools); an empty access is
// read-only; MaxIterations is clamped to 1..subagenttype.MaxIterations.
func ToType(def subagenttype.Definition, source subagenttype.Source) (subagenttype.Type, error) {
	fail := func(message string) (subagenttype.Type, error) {
		return subagenttype.Type{}, subagent.NewError(message, subagent.CodeInvalidArgument)
	}
	name := jsstr.Trim(def.Name)
	if !namePattern.MatchString(name) || kernel.UTF16Len(name) > subagenttype.MaxNameChars {
		return fail(`subagent type name "` + name + `" must be lowercase letters, digits and dashes (max ` + strconv.Itoa(subagenttype.MaxNameChars) + `)`)
	}
	description := jsstr.Trim(def.Description)
	if description == "" {
		return fail("a subagent type needs a description (when to use it)")
	}
	if kernel.UTF16Len(description) > subagenttype.MaxDescriptionChars {
		return fail("description is longer than " + strconv.Itoa(subagenttype.MaxDescriptionChars) + " characters")
	}
	systemPrompt := jsstr.Trim(def.SystemPrompt)
	if systemPrompt == "" {
		return fail("a subagent type needs a system prompt")
	}
	if kernel.UTF16Len(systemPrompt) > subagenttype.MaxSystemPromptChars {
		return fail("system prompt is longer than " + strconv.Itoa(subagenttype.MaxSystemPromptChars) + " characters")
	}
	var tools []string
	if def.Tools != nil {
		tools = make([]string, 0, len(def.Tools))
		for _, t := range def.Tools {
			t = jsstr.Trim(t)
			if t == "" {
				return fail("tools must be a list of tool names")
			}
			if !slices.Contains(tools, t) {
				tools = append(tools, t)
			}
		}
		if len(tools) > subagenttype.MaxTools {
			return fail("at most " + strconv.Itoa(subagenttype.MaxTools) + " tools")
		}
	}
	if def.Access != "" && def.Access != subagent.AccessReadOnly && def.Access != subagent.AccessWrite {
		return fail(`access must be "read-only" or "write"`)
	}
	access := subagent.AccessReadOnly
	if def.Access == subagent.AccessWrite {
		access = subagent.AccessWrite
	}
	var maxIterations *int
	if def.MaxIterations != nil {
		v := min(max(*def.MaxIterations, 1), subagenttype.MaxIterations)
		maxIterations = &v
	}
	return subagenttype.Type{
		Definition: subagenttype.Definition{
			Name:          name,
			Description:   description,
			SystemPrompt:  systemPrompt,
			Tools:         tools,
			Access:        access,
			Provider:      jsstr.Trim(def.Provider),
			Model:         jsstr.Trim(def.Model),
			MaxIterations: maxIterations,
		},
		Source: source,
	}, nil
}
