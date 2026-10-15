// Package skillhub is the skill library over persistence.SkillStore, plus the
// essential skills the SDK ships (essential.go).
//
// Built-in and app skills are registered in memory; custom skills live in the
// persistence.SkillStore returned by the getStore function on every load and
// write, so Kernel.UsePersistence can swap the store. The package lives apart
// from skill because it imports persistence, and persistence imports skill.
package skillhub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// errNoStore is returned when getStore yields no store.
var errNoStore = errors.New("skillhub: the persistence skill store is not configured")

// deletedListener is one OnDeleted subscription. active is cleared by the
// unsubscribe function so a Delete that already took its snapshot skips a
// listener removed before its turn.
type deletedListener struct {
	fn     func(name string)
	active atomic.Bool
}

// SkillHub is the skill library. Two kinds of entries:
//   - registered (built-in essentials + app skills): in memory, read-only at runtime;
//   - custom (created by agents through skill_manage, or by users): persisted in the
//     SkillStore, loaded on first use and cached - write custom skills through this hub
//     so the cache stays current.
//
// Registered names win: a stored custom skill with the same name is ignored (logged).
//
// Global hub for skills, for example this hub has 50 skills but user want Devops Agent
// so user only enable 10 devop skills from 50 skills.
//
// Concurrency: Create, Update, Delete and the lazy load of the
// custom skills are serialised by one mutex, so a check-then-save cannot interleave;
// Reload only invalidates the cache and never waits. Every Skill the hub returns is a
// deep copy.
type SkillHub struct {
	getStore func() persistence.SkillStore
	now      func() int64
	logger   *slog.Logger
	events   events.Bus // set by WithEvents; nil means Delete emits nothing

	// opMu serialises Create, Update, Delete and the lazy load. Lock order: opMu, then mu.
	opMu sync.Mutex

	// mu guards everything below. Store I/O never runs while it is held.
	mu         sync.RWMutex
	registered ordered.Map[skill.Skill]
	custom     ordered.Map[skill.Skill]
	loaded     bool
	generation uint64
	listeners  []*deletedListener
}

// NewSkillHub returns an empty hub. getStore is resolved on every load and write, so
// Kernel.UsePersistence can swap the store.
func NewSkillHub(getStore func() persistence.SkillStore, opts ...Option) *SkillHub {
	h := &SkillHub{
		getStore: getStore,
		now:      kernel.NowMillis,
		logger:   slog.Default(),
	}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	return h
}

// Register adds a built-in or app skill. An empty source means app; skill.SourceCustom
// (or any unknown source) is an error, because custom skills are made with Create.
// It returns a *kernel.SkillError when the package is invalid or the name is taken.
func (h *SkillHub) Register(pkg skill.Package, source skill.Source) (skill.Skill, error) {
	if source == "" {
		source = skill.SourceApp
	}
	if source != skill.SourceBuiltIn && source != skill.SourceApp {
		return skill.Skill{}, fmt.Errorf("skillhub: cannot register a %q skill, only built-in and app (use Create for custom skills)", source)
	}
	sk, err := toSkill(pkg, meta{source: source, version: 1})
	if err != nil {
		return skill.Skill{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.registered.Has(sk.Name) {
		return skill.Skill{}, kernel.NewSkillError(fmt.Sprintf("Skill \"%s\" already registered", sk.Name), kernel.SkillCodeAlreadyExists, sk.Name, false)
	}
	h.registered.Set(sk.Name, sk)
	return sk.Clone(), nil
}

// Get returns the skill with this name, registered skills first. The bool is false
// when the library has no such skill.
func (h *SkillHub) Get(ctx context.Context, name string) (skill.Skill, bool, error) {
	h.mu.RLock()
	sk, ok := h.registered.Get(name)
	h.mu.RUnlock()
	if ok {
		return sk.Clone(), true, nil
	}
	var found skill.Skill
	var hit bool
	err := h.readLoaded(ctx, func() {
		if c, has := h.custom.Get(name); has {
			found, hit = c.Clone(), true
		}
	})
	if err != nil {
		return skill.Skill{}, false, err
	}
	return found, hit, nil
}

// GetByNames returns the skills with these names in the given order; unknown names
// are dropped.
func (h *SkillHub) GetByNames(ctx context.Context, names []string) ([]skill.Skill, error) {
	out := make([]skill.Skill, 0, len(names))
	err := h.readLoaded(ctx, func() {
		for _, n := range names {
			if sk, ok := h.registered.Get(n); ok {
				out = append(out, sk.Clone())
			} else if sk, ok := h.custom.Get(n); ok {
				out = append(out, sk.Clone())
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// List returns every skill in the library, registered first, each group sorted by
// name. Byte order equals JS localeCompare for valid skill names (a-z, 0-9, hyphen).
func (h *SkillHub) List(ctx context.Context) ([]skill.Skill, error) {
	var out []skill.Skill
	err := h.readLoaded(ctx, func() {
		out = make([]skill.Skill, 0, h.registered.Len()+h.custom.Len())
		out = appendSorted(out, &h.registered)
		out = appendSorted(out, &h.custom)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Create adds a custom skill. It returns a *kernel.SkillError ("invalid",
// "already_exists").
func (h *SkillHub) Create(ctx context.Context, pkg skill.Package, opts CreateOptions) (skill.Skill, error) {
	now := kernel.MillisToISO(h.now())
	sk, err := toSkill(pkg, meta{
		source:    skill.SourceCustom,
		version:   1,
		createdAt: now,
		updatedAt: now,
		createdBy: opts.CreatedBy,
	})
	if err != nil {
		return skill.Skill{}, err
	}

	h.opMu.Lock()
	defer h.opMu.Unlock()
	if err := h.ensureLoadedLocked(ctx); err != nil {
		return skill.Skill{}, err
	}
	if h.exists(sk.Name) {
		return skill.Skill{}, kernel.NewSkillError(
			fmt.Sprintf("A skill named \"%s\" already exists \u2014 pick another name, or update the existing skill", sk.Name),
			kernel.SkillCodeAlreadyExists,
			sk.Name,
			false,
		)
	}
	if err := h.save(ctx, sk); err != nil {
		return skill.Skill{}, err
	}
	h.cache(sk)
	return sk.Clone(), nil
}

// Update replaces a custom skill's SKILL.md and files. The name cannot change.
func (h *SkillHub) Update(ctx context.Context, name string, pkg skill.Package) (skill.Skill, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	existing, err := h.requireCustom(ctx, name)
	if err != nil {
		return skill.Skill{}, err
	}
	sk, err := toSkill(pkg, meta{
		source:    skill.SourceCustom,
		version:   existing.Version + 1,
		createdAt: existing.CreatedAt,
		updatedAt: kernel.MillisToISO(h.now()),
		createdBy: existing.CreatedBy,
	})
	if err != nil {
		return skill.Skill{}, err
	}
	if sk.Name != name {
		return skill.Skill{}, kernel.NewSkillError(
			fmt.Sprintf("The frontmatter name \"%s\" does not match \"%s\" \u2014 skills cannot be renamed; create a new skill and delete this one", sk.Name, name),
			kernel.SkillCodeInvalid,
			name,
			false,
		)
	}
	if err := h.save(ctx, sk); err != nil {
		return skill.Skill{}, err
	}
	h.cache(sk)
	return sk.Clone(), nil
}

// Delete removes a custom skill. Agents that had it enabled simply stop seeing it.
// The OnDeleted listeners run after the skill is removed and before Delete returns,
// with no lock held, so a listener may call back into the hub.
// With WithEvents, events.SkillDeleted is emitted after the listeners.
func (h *SkillHub) Delete(ctx context.Context, name string) error {
	listeners, err := h.deleteLocked(ctx, name)
	if err != nil {
		return err
	}
	for _, l := range listeners {
		if l.active.Load() {
			l.fn(name)
		}
	}
	if h.events != nil {
		events.Emit(h.events, events.SkillDeleted, "", events.SkillDeletedPayload{Name: name})
	}
	return nil
}

// deleteLocked removes the skill under the mutation mutex and returns the listener
// snapshot for Delete to call once the mutex is released.
func (h *SkillHub) deleteLocked(ctx context.Context, name string) ([]*deletedListener, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	if _, err := h.requireCustom(ctx, name); err != nil {
		return nil, err
	}
	store, err := h.store()
	if err != nil {
		return nil, err
	}
	if err := store.Delete(ctx, name); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.custom.Delete(name)
	return slices.Clone(h.listeners), nil
}

// OnDeleted registers a listener called after a custom skill is deleted, before
// Delete returns - e.g. to disable it for every agent that had it enabled. It
// returns an idempotent unsubscribe function.
func (h *SkillHub) OnDeleted(listener func(name string)) (unsubscribe func()) {
	if listener == nil {
		return func() {}
	}
	l := &deletedListener{fn: listener}
	l.active.Store(true)
	h.mu.Lock()
	h.listeners = append(h.listeners, l)
	h.mu.Unlock()
	return func() {
		l.active.Store(false)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.listeners = slices.DeleteFunc(h.listeners, func(x *deletedListener) bool { return x == l })
	}
}

// Reload forgets the cached custom skills; the next read loads them from the store
// again. A load that was running when Reload was called discards its result.
func (h *SkillHub) Reload() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.generation++
	h.loaded = false
	h.custom = ordered.Map[skill.Skill]{}
}

// exists reports whether the name is taken by a registered or a cached custom
// skill. The caller holds opMu and has loaded the custom skills.
func (h *SkillHub) exists(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.registered.Has(name) || h.custom.Has(name)
}

// cache puts a saved custom skill in the cache. When a Reload dropped the cache in
// the meantime the next load reads the saved record from the store instead.
func (h *SkillHub) cache(sk skill.Skill) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loaded {
		h.custom.Set(sk.Name, sk)
	}
}

// save writes the record of a custom skill to the store resolved right now.
func (h *SkillHub) save(ctx context.Context, sk skill.Skill) error {
	store, err := h.store()
	if err != nil {
		return err
	}
	return store.Save(ctx, h.toRecord(sk))
}

// store resolves the persistence skill store.
func (h *SkillHub) store() (persistence.SkillStore, error) {
	if h.getStore == nil {
		return nil, errNoStore
	}
	store := h.getStore()
	if nilcheck.IsNil(store) {
		return nil, errNoStore
	}
	return store, nil
}

// requireCustom returns the cached custom skill, or the read_only / not_found
// error. The caller holds opMu.
func (h *SkillHub) requireCustom(ctx context.Context, name string) (skill.Skill, error) {
	h.mu.RLock()
	reg, isRegistered := h.registered.Get(name)
	h.mu.RUnlock()
	if isRegistered {
		return skill.Skill{}, kernel.NewSkillError(
			fmt.Sprintf("\"%s\" is a %s skill and cannot be changed \u2014 create a new skill with a different name instead", name, reg.Source),
			kernel.SkillCodeReadOnly,
			name,
			false,
		)
	}
	if err := h.ensureLoadedLocked(ctx); err != nil {
		return skill.Skill{}, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	sk, ok := h.custom.Get(name)
	if !ok {
		return skill.Skill{}, kernel.NewSkillError(fmt.Sprintf("Skill \"%s\" not found", name), kernel.SkillCodeNotFound, name, false)
	}
	return sk, nil
}

// readLoaded runs read with the read lock held and the custom skills loaded. The
// loaded check and the read share one lock acquisition: a Reload that lands
// between an ensureLoaded and a separate RLock would otherwise leave read looking
// at the empty cache of the new generation, and an existing skill would be
// reported missing; a swap must never make a skill vanish. When a Reload slips in
// before the lock is taken, the custom skills are loaded again and the check
// repeats; every repeat first looks at ctx, so a caller whose context ends leaves
// even while Reload calls keep invalidating the cache. read runs exactly once, after the check passed.
func (h *SkillHub) readLoaded(ctx context.Context, read func()) error {
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

// ensureLoaded loads the custom skills from the store when they are not cached.
func (h *SkillHub) ensureLoaded(ctx context.Context) error {
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

// ensureLoadedLocked is ensureLoaded for a caller that holds opMu. A failed load is
// not cached - the next read retries. A Reload that happens while the store is read
// makes the loop read again, so a stale result is never cached; every repeat first
// looks at ctx, so a reload storm cannot hold a caller whose context has ended.
func (h *SkillHub) ensureLoadedLocked(ctx context.Context) error {
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

// readStore reads every stored skill. A record that is shadowed by a registered
// skill, or that no longer validates, is skipped with one warning; the warnings
// carry the skill name and the parse error text, never the content.
func (h *SkillHub) readStore(ctx context.Context) (ordered.Map[skill.Skill], error) {
	var out ordered.Map[skill.Skill]
	store, err := h.store()
	if err != nil {
		return out, err
	}
	records, err := store.List(ctx)
	if err != nil {
		return out, err
	}
	for _, record := range records {
		h.mu.RLock()
		shadowed := h.registered.Has(record.Name)
		h.mu.RUnlock()
		if shadowed {
			h.logger.Warn(fmt.Sprintf("[skills] stored skill \"%s\" is shadowed by a registered skill with the same name \u2014 ignored", record.Name))
			continue
		}
		sk, err := fromRecord(record)
		if err != nil {
			// A record that no longer validates must not hide the rest of the library.
			h.logger.Warn(fmt.Sprintf("[skills] stored skill \"%s\" is invalid \u2014 ignored: %s", record.Name, err.Error()))
			continue
		}
		out.Set(record.Name, sk)
	}
	return out, nil
}

// meta is the provenance part of a Skill (source, version, ...).
type meta struct {
	source    skill.Source
	version   int
	createdAt string
	updatedAt string
	createdBy string
}

// toSkill parses a package and adds the provenance.
func toSkill(pkg skill.Package, m meta) (skill.Skill, error) {
	parsed, err := skill.ParsePackage(pkg)
	if err != nil {
		return skill.Skill{}, err
	}
	fm := parsed.Frontmatter
	return skill.Skill{
		Name:          fm.Name,
		Description:   fm.Description,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		AllowedTools:  fm.AllowedTools,
		Metadata:      fm.Metadata,
		Body:          parsed.Body,
		Content:       strings.ReplaceAll(pkg.Content, "\r\n", "\n"),
		Files:         parsed.Files,
		RequiresTools: parsed.RequiresTools,
		Source:        m.source,
		Version:       m.version,
		CreatedAt:     m.createdAt,
		UpdatedAt:     m.updatedAt,
		CreatedBy:     m.createdBy,
	}, nil
}

// toRecord is the stored form of a custom skill. A skill without timestamps (never
// the case for a custom skill) is stamped now.
func (h *SkillHub) toRecord(sk skill.Skill) persistence.SkillRecord {
	createdAt, updatedAt := sk.CreatedAt, sk.UpdatedAt
	if createdAt == "" {
		createdAt = kernel.MillisToISO(h.now())
	}
	if updatedAt == "" {
		updatedAt = kernel.MillisToISO(h.now())
	}
	return persistence.SkillRecord{
		Name:      sk.Name,
		Content:   sk.Content,
		Files:     sk.Files,
		CreatedBy: sk.CreatedBy,
		Version:   sk.Version,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

// fromRecord parses a stored record back into a custom skill. It fails when the
// frontmatter name differs from the stored name.
func fromRecord(record persistence.SkillRecord) (skill.Skill, error) {
	sk, err := toSkill(skill.Package{Content: record.Content, Files: record.Files}, meta{
		source:    skill.SourceCustom,
		version:   record.Version,
		createdAt: record.CreatedAt,
		updatedAt: record.UpdatedAt,
		createdBy: record.CreatedBy,
	})
	if err != nil {
		return skill.Skill{}, err
	}
	if sk.Name != record.Name {
		return skill.Skill{}, fmt.Errorf("frontmatter name \"%s\" does not match the stored name", sk.Name)
	}
	return sk, nil
}

// appendSorted appends clones of the map's skills sorted by name.
func appendSorted(out []skill.Skill, m *ordered.Map[skill.Skill]) []skill.Skill {
	group := m.Values()
	slices.SortFunc(group, func(a, b skill.Skill) int { return strings.Compare(a.Name, b.Name) })
	for _, sk := range group {
		out = append(out, sk.Clone())
	}
	return out
}
