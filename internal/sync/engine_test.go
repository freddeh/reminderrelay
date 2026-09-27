package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/njoerd114/reminderrelay/internal/model"
)

// countingReminders wraps a RemindersSource and counts FetchAll calls, so
// tests can tell how many reconcile passes actually ran.
type countingReminders struct {
	RemindersSource
	mu    sync.Mutex
	calls int
}

func (c *countingReminders) FetchAll(ctx context.Context, listNames []string) ([]*model.Item, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.RemindersSource.FetchAll(ctx, listNames)
}

func (c *countingReminders) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestEngine_DebounceReconcile_CoalescesBurstOfEvents guards against HA's
// habit of firing several state_changed events for a single user action:
// without debouncing, each one would trigger its own reconcile pass.
func TestEngine_DebounceReconcile_CoalescesBurstOfEvents(t *testing.T) {
	now := time.Now().UTC()
	rem := &countingReminders{RemindersSource: newMockReminders()}
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{UID: "ha-1", Title: "Buy milk", ModifiedAt: now})
	store := newMockStore()

	r := NewReconciler(rem, ha, store, testLogger)
	e := NewEngine(r, nil, testMappings, time.Hour, testLogger)
	e.debounceWindow = 20 * time.Millisecond

	ctx := context.Background()
	for range 5 {
		e.debounceReconcile(ctx, "Shopping", "todo.shopping")
		time.Sleep(5 * time.Millisecond) // shorter than the debounce window: keeps resetting the timer
	}

	// Give the final debounce timer time to fire.
	time.Sleep(100 * time.Millisecond)

	if got := rem.count(); got != 1 {
		t.Errorf("reconcile ran %d times, want 1 (burst of WS events should coalesce)", got)
	}
}

// TestEngine_DebounceReconcile_SeparateEntitiesDoNotCoalesce ensures the
// debounce is scoped per entity — a burst on one entity must not suppress or
// delay a reconcile for a different entity.
func TestEngine_DebounceReconcile_SeparateEntitiesDoNotCoalesce(t *testing.T) {
	now := time.Now().UTC()
	rem := &countingReminders{RemindersSource: newMockReminders()}
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{UID: "ha-1", Title: "Buy milk", ModifiedAt: now})
	ha.addItems("todo.groceries", model.Item{UID: "ha-2", Title: "Buy bread", ModifiedAt: now})
	store := newMockStore()

	mappings := map[string]string{"Shopping": "todo.shopping", "Groceries": "todo.groceries"}
	r := NewReconciler(rem, ha, store, testLogger)
	e := NewEngine(r, nil, mappings, time.Hour, testLogger)
	e.debounceWindow = 20 * time.Millisecond

	ctx := context.Background()
	e.debounceReconcile(ctx, "Shopping", "todo.shopping")
	e.debounceReconcile(ctx, "Groceries", "todo.groceries")

	time.Sleep(100 * time.Millisecond)

	if got := rem.count(); got != 2 {
		t.Errorf("reconcile ran %d times, want 2 (one per distinct entity)", got)
	}
}
