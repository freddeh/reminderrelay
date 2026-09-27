package sync

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njoerd114/reminderrelay/internal/model"
	"github.com/njoerd114/reminderrelay/internal/state"
)

func TestBootstrap_SkipsNonEmptyDB(t *testing.T) {
	rem := newMockReminders()
	ha := newMockHA()
	store := newMockStore()

	// Seed one item to make IsEmpty return false.
	store.seed(stateItemHelper("rem-1", "ha-1", "Shopping", "Existing"))

	var buf bytes.Buffer
	b := NewBootstrap(rem, ha, store, testLogger, strings.NewReader(""), &buf)
	ran, err := b.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ran {
		t.Error("bootstrap should not run when state DB is non-empty")
	}
}

func TestBootstrap_MatchesByTitle(t *testing.T) {
	now := time.Now().UTC()

	rem := newMockReminders(
		newItem("rem-1", "Buy milk", "Shopping", model.PriorityHigh, false, now),
		newItem("rem-2", "Only in Reminders", "Shopping", model.PriorityNone, false, now),
	)

	ha := newMockHA()
	ha.addItems("todo.shopping",
		model.Item{UID: "ha-1", Title: "Buy milk", ModifiedAt: now},
		model.Item{UID: "ha-3", Title: "Only in HA", ModifiedAt: now},
	)

	store := newMockStore()
	var output bytes.Buffer
	input := strings.NewReader("y\n")

	b := NewBootstrap(rem, ha, store, slog.Default(), input, &output)
	ran, err := b.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ran {
		t.Fatal("bootstrap should have executed")
	}

	// Verify summary output.
	summary := output.String()
	if !strings.Contains(summary, "Buy milk") {
		t.Error("summary should mention matched item 'Buy milk'")
	}
	if !strings.Contains(summary, "Only in Reminders") {
		t.Error("summary should mention Reminders-only item")
	}
	if !strings.Contains(summary, "Only in HA") {
		t.Error("summary should mention HA-only item")
	}

	// State DB should have 3 entries: 1 matched + 1 pushed to HA + 1 pushed to Rem.
	if store.count() != 3 {
		t.Errorf("state items = %d, want 3", store.count())
	}

	// HA should have 3 items (original 2 + 1 from Reminders).
	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 3 {
		t.Errorf("HA items = %d, want 3", len(haItems))
	}

	// Reminders should have 3 items (original 2 + 1 from HA).
	if rem.count() != 3 {
		t.Errorf("Reminders items = %d, want 3", rem.count())
	}
}

func TestBootstrap_CancelledByUser(t *testing.T) {
	now := time.Now().UTC()
	rem := newMockReminders(
		newItem("rem-1", "Task", "Shopping", model.PriorityNone, false, now),
	)
	ha := newMockHA()
	store := newMockStore()

	var output bytes.Buffer
	input := strings.NewReader("n\n") // User says no.

	b := NewBootstrap(rem, ha, store, slog.Default(), input, &output)
	ran, err := b.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ran {
		t.Error("bootstrap should not execute when user says no")
	}

	// State DB should remain empty.
	if store.count() != 0 {
		t.Error("state DB should be empty after cancellation")
	}
}

func TestBootstrap_CaseInsensitiveMatch(t *testing.T) {
	now := time.Now().UTC()

	rem := newMockReminders(
		newItem("rem-1", "Buy Milk", "Shopping", model.PriorityNone, false, now),
	)
	ha := newMockHA()
	ha.addItems("todo.shopping",
		model.Item{UID: "ha-1", Title: "buy milk", ModifiedAt: now},
	)

	store := newMockStore()
	var output bytes.Buffer
	input := strings.NewReader("y\n")

	b := NewBootstrap(rem, ha, store, slog.Default(), input, &output)
	ran, err := b.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ran {
		t.Fatal("bootstrap should have executed")
	}

	// Should match as 1 pair, not create duplicates.
	if store.count() != 1 {
		t.Errorf("state items = %d, want 1 (case-insensitive match)", store.count())
	}
}

// TestBootstrap_DuplicateTitles_LinksDistinctPairs reproduces a real-world
// crash: several Reminders items and several HA items share the exact same
// title (e.g. a recurring reminder created many times with an identical
// name). The buggy matchByTitle matched every one of them to the single last
// HA item with that title, so Bootstrap.execute tried to write multiple
// state rows with the same ha_uid and hit sync_items' unique constraint. This
// uses the real SQLite-backed Store (not the in-memory mock, which doesn't
// enforce the constraint) so a regression here fails the same way it did in
// production.
func TestBootstrap_DuplicateTitles_LinksDistinctPairs(t *testing.T) {
	now := time.Now().UTC()
	const title = "Klara Essensanmeldung MENSA"

	rem := newMockReminders(
		newItem("rem-1", title, "Shopping", model.PriorityNone, false, now),
		newItem("rem-2", title, "Shopping", model.PriorityNone, false, now),
		newItem("rem-3", title, "Shopping", model.PriorityNone, false, now),
	)
	ha := newMockHA()
	ha.addItems("todo.shopping",
		model.Item{UID: "ha-1", Title: title, ModifiedAt: now},
		model.Item{UID: "ha-2", Title: title, ModifiedAt: now},
		model.Item{UID: "ha-3", Title: title, ModifiedAt: now},
	)

	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var output bytes.Buffer
	input := strings.NewReader("y\n")

	b := NewBootstrap(rem, ha, store, slog.Default(), input, &output)
	ran, err := b.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("bootstrap failed (ha_uid unique-constraint regression): %v", err)
	}
	if !ran {
		t.Fatal("bootstrap should have executed")
	}

	items, err := store.GetAllItemsForList(context.Background(), "Shopping")
	if err != nil {
		t.Fatalf("GetAllItemsForList: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("state items = %d, want 3", len(items))
	}

	seenHA := make(map[string]bool, len(items))
	seenRem := make(map[string]bool, len(items))
	for _, it := range items {
		if seenHA[it.HAUID] {
			t.Errorf("HA UID %s linked more than once", it.HAUID)
		}
		seenHA[it.HAUID] = true
		if seenRem[it.RemindersUID] {
			t.Errorf("Reminders UID %s linked more than once", it.RemindersUID)
		}
		seenRem[it.RemindersUID] = true
	}
}

func TestMatchByTitle_EmptyLists(t *testing.T) {
	result := matchByTitle("Shopping", "todo.shopping", nil, nil)

	if len(result.matched) != 0 {
		t.Errorf("matched = %d, want 0", len(result.matched))
	}
	if len(result.remOnly) != 0 {
		t.Errorf("remOnly = %d, want 0", len(result.remOnly))
	}
	if len(result.haOnly) != 0 {
		t.Errorf("haOnly = %d, want 0", len(result.haOnly))
	}
}

func TestMatchByTitle_AllMatched(t *testing.T) {
	now := time.Now().UTC()
	remItems := []*model.Item{
		newItem("rem-1", "A", "Shopping", model.PriorityNone, false, now),
		newItem("rem-2", "B", "Shopping", model.PriorityNone, false, now),
	}
	haItems := []model.Item{
		{UID: "ha-1", Title: "A", ModifiedAt: now},
		{UID: "ha-2", Title: "B", ModifiedAt: now},
	}

	result := matchByTitle("Shopping", "todo.shopping", remItems, haItems)

	if len(result.matched) != 2 {
		t.Errorf("matched = %d, want 2", len(result.matched))
	}
	if len(result.remOnly) != 0 || len(result.haOnly) != 0 {
		t.Errorf("expected no unmatched, got remOnly=%d haOnly=%d", len(result.remOnly), len(result.haOnly))
	}
}

func TestMatchByTitle_DuplicateTitles_PairOneToOne(t *testing.T) {
	now := time.Now().UTC()
	remItems := []*model.Item{
		newItem("rem-1", "Recurring", "Shopping", model.PriorityNone, false, now),
		newItem("rem-2", "Recurring", "Shopping", model.PriorityNone, false, now),
		newItem("rem-3", "Recurring", "Shopping", model.PriorityNone, false, now),
	}
	haItems := []model.Item{
		{UID: "ha-1", Title: "Recurring", ModifiedAt: now},
		{UID: "ha-2", Title: "Recurring", ModifiedAt: now},
		{UID: "ha-3", Title: "Recurring", ModifiedAt: now},
	}

	result := matchByTitle("Shopping", "todo.shopping", remItems, haItems)

	if len(result.matched) != 3 {
		t.Fatalf("matched = %d, want 3", len(result.matched))
	}
	if len(result.remOnly) != 0 || len(result.haOnly) != 0 {
		t.Errorf("expected no unmatched, got remOnly=%d haOnly=%d", len(result.remOnly), len(result.haOnly))
	}

	// Each matched pair must reference a distinct HA item — reusing the same
	// HA item for multiple Reminders items is what caused the ha_uid unique
	// constraint violation in production.
	seen := make(map[string]bool, 3)
	for _, m := range result.matched {
		if seen[m.ha.UID] {
			t.Errorf("HA item %s matched more than once", m.ha.UID)
		}
		seen[m.ha.UID] = true
	}
	if len(seen) != 3 {
		t.Errorf("distinct HA UIDs matched = %d, want 3", len(seen))
	}
}

func TestMatchByTitle_MoreRemindersThanHA_ExcessGoesRemOnly(t *testing.T) {
	now := time.Now().UTC()
	remItems := []*model.Item{
		newItem("rem-1", "Recurring", "Shopping", model.PriorityNone, false, now),
		newItem("rem-2", "Recurring", "Shopping", model.PriorityNone, false, now),
	}
	haItems := []model.Item{
		{UID: "ha-1", Title: "Recurring", ModifiedAt: now},
	}

	result := matchByTitle("Shopping", "todo.shopping", remItems, haItems)

	if len(result.matched) != 1 {
		t.Errorf("matched = %d, want 1", len(result.matched))
	}
	if len(result.remOnly) != 1 {
		t.Errorf("remOnly = %d, want 1", len(result.remOnly))
	}
	if len(result.haOnly) != 0 {
		t.Errorf("haOnly = %d, want 0", len(result.haOnly))
	}
}

func TestMatchByTitle_MoreHAThanReminders_ExcessGoesHAOnly(t *testing.T) {
	now := time.Now().UTC()
	remItems := []*model.Item{
		newItem("rem-1", "Recurring", "Shopping", model.PriorityNone, false, now),
	}
	haItems := []model.Item{
		{UID: "ha-1", Title: "Recurring", ModifiedAt: now},
		{UID: "ha-2", Title: "Recurring", ModifiedAt: now},
	}

	result := matchByTitle("Shopping", "todo.shopping", remItems, haItems)

	if len(result.matched) != 1 {
		t.Errorf("matched = %d, want 1", len(result.matched))
	}
	if len(result.remOnly) != 0 {
		t.Errorf("remOnly = %d, want 0", len(result.remOnly))
	}
	if len(result.haOnly) != 1 {
		t.Fatalf("haOnly = %d, want 1", len(result.haOnly))
	}
	if result.haOnly[0].UID != "ha-2" {
		t.Errorf("haOnly[0].UID = %q, want %q (FIFO: the first HA item is claimed by the match)", result.haOnly[0].UID, "ha-2")
	}
}

// stateItemHelper creates a minimal state.Item for test seeding.
func stateItemHelper(remUID, haUID, listName, title string) *stateItem {
	return &stateItem{
		RemindersUID: remUID,
		HAUID:        haUID,
		ListName:     listName,
		Title:        title,
	}
}

// stateItem is imported from the state package via the type used in store.
type stateItem = state.Item
