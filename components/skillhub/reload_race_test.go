package skillhub_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestReadsNeverMissACustomSkillDuringReload: a Reload (what Kernel.UsePersistence
// triggers) that lands between a reader's load check and its read of the cache
// must not make an existing skill look missing, nor the list look empty: a swap
// must never make a skill vanish, so the readers must read the cache and the
// loaded flag under one lock.
func TestReadsNeverMissACustomSkillDuringReload(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemorySkillStore()
	now := kernel.NowISO()
	if err := store.Save(ctx, persistence.SkillRecord{Name: "custom-one", Content: md("custom-one"), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	hub := newHub(store)
	mustRegister(t, hub, md("builtin-one"), skill.SourceBuiltIn)

	const readers, rounds = 4, 400
	var stop atomic.Bool
	var reloader sync.WaitGroup
	reloader.Add(1)
	go func() {
		defer reloader.Done()
		for !stop.Load() {
			hub.Reload()
			runtime.Gosched() // a Reload per scheduling slice, not a busy loop that starves the readers
		}
	}()

	var getMisses, listShort, byNamesShort, errs atomic.Int64
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				if _, ok, err := hub.Get(ctx, "custom-one"); err != nil {
					errs.Add(1)
				} else if !ok {
					getMisses.Add(1)
				}
				if all, err := hub.List(ctx); err != nil {
					errs.Add(1)
				} else if len(all) != 2 {
					listShort.Add(1)
				}
				if some, err := hub.GetByNames(ctx, []string{"builtin-one", "custom-one"}); err != nil {
					errs.Add(1)
				} else if len(some) != 2 {
					byNamesShort.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	stop.Store(true)
	reloader.Wait()

	if n := errs.Load(); n != 0 {
		t.Errorf("%d reads failed", n)
	}
	if n := getMisses.Load(); n != 0 {
		t.Errorf("Get reported an existing skill as missing %d times", n)
	}
	if n := listShort.Load(); n != 0 {
		t.Errorf("List dropped a skill %d times", n)
	}
	if n := byNamesShort.Load(); n != 0 {
		t.Errorf("GetByNames dropped a skill %d times", n)
	}
}

// reloadingStore is a skill store whose List calls Reload on the hub that reads
// it: every load is invalidated before it can be installed, the worst case of a
// reload storm.
type reloadingStore struct {
	persistence.SkillStore
	hub atomic.Pointer[skillhub.SkillHub]
}

func (s *reloadingStore) List(ctx context.Context) ([]persistence.SkillRecord, error) {
	records, err := s.SkillStore.List(ctx)
	if h := s.hub.Load(); h != nil {
		h.Reload()
	}
	return records, err
}

// TestReloadStormCannotHoldACancelledReader: when every load is invalidated by a
// Reload, a reader retries for as long as the storm lasts, but it must leave with
// the context error once its context ends, for the readers and for the lazy load
// that Create runs.
func TestReloadStormCannotHoldACancelledReader(t *testing.T) {
	store := &reloadingStore{SkillStore: persistence.NewMemorySkillStore()}
	hub := newHub(store)
	store.hub.Store(hub)

	calls := map[string]func(ctx context.Context) error{
		"Get": func(ctx context.Context) error {
			_, _, err := hub.Get(ctx, "custom-one")
			return err
		},
		"GetByNames": func(ctx context.Context) error {
			_, err := hub.GetByNames(ctx, []string{"custom-one"})
			return err
		},
		"List": func(ctx context.Context) error {
			_, err := hub.List(ctx)
			return err
		},
		"Create": func(ctx context.Context) error {
			_, err := hub.Create(ctx, skill.Package{Content: md("created-one")}, skillhub.CreateOptions{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- call(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("error = %v, want the context deadline", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the reader is still retrying 5s after its context ended")
			}
		})
	}
}
