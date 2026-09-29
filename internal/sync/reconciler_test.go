package sync

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/njoerd114/reminderrelay/internal/model"
	"github.com/njoerd114/reminderrelay/internal/state"
)

var (
	testLogger   = slog.Default()
	testMappings = map[string]model.ListMapping{"Shopping": {HAEntity: "todo.shopping"}}
)

func newItem(uid, title, listName string, priority model.Priority, completed bool, modifiedAt time.Time) *model.Item {
	return &model.Item{
		UID:        uid,
		Title:      title,
		ListName:   listName,
		Priority:   priority,
		Completed:  completed,
		ModifiedAt: modifiedAt,
	}
}

// ---------------------------------------------------------------------------
// Scenario 1: Item exists only in Reminders → created in HA
// ---------------------------------------------------------------------------

func TestReconcile_NewReminderItem_CreatedInHA(t *testing.T) {
	now := time.Now().UTC()
	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityHigh, false, now)

	rem := newMockReminders(remItem)
	ha := newMockHA()
	store := newMockStore()

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Created != 1 {
		t.Errorf("Created = %d, want 1", stats.Created)
	}

	// HA should have the item.
	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 1 {
		t.Fatalf("HA items = %d, want 1", len(haItems))
	}
	if haItems[0].Title != "Buy milk" {
		t.Errorf("HA item title = %q, want %q", haItems[0].Title, "Buy milk")
	}

	// State DB should have a mapping.
	if store.count() != 1 {
		t.Errorf("state items = %d, want 1", store.count())
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: Item exists only in HA → created in Reminders
// ---------------------------------------------------------------------------

func TestReconcile_NewHAItem_CreatedInReminders(t *testing.T) {
	now := time.Now().UTC()

	rem := newMockReminders()
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:        "ha-1",
		Title:      "Buy eggs",
		Priority:   model.PriorityNone,
		ModifiedAt: now,
	})
	store := newMockStore()

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Created != 1 {
		t.Errorf("Created = %d, want 1", stats.Created)
	}

	// Reminders should have the item.
	if rem.count() != 1 {
		t.Errorf("Reminders items = %d, want 1", rem.count())
	}

	// State DB should have a mapping.
	if store.count() != 1 {
		t.Errorf("state items = %d, want 1", store.count())
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: Both sides updated, Reminders newer → Reminders wins
// ---------------------------------------------------------------------------

func TestReconcile_Conflict_RemindersWins(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	remTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	haObservedAt := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC) // before remTime → Reminders is newer

	// State DB: item was synced with some hash at older time. HAModified
	// simulates "the HA-side change was first observed at 11:00" (there's
	// no real HA-reported timestamp — see state.Item.HAModified).
	origItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, older)
	origHash := origItem.ContentHash()

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID:      "rem-1",
		HAUID:             "ha-1",
		ListName:          "Shopping",
		Title:             "Buy milk",
		LastSyncHash:      origHash,
		RemindersModified: older,
		HAModified:        haObservedAt,
		LastSyncedAt:      older,
	})

	// Reminders: title changed to "Buy whole milk" (newer than haObservedAt).
	remItem := newItem("rem-1", "Buy whole milk", "Shopping", model.PriorityNone, false, remTime)
	rem := newMockReminders(remItem)

	// HA: title changed to "Buy skim milk".
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:      "ha-1",
		Title:    "Buy skim milk",
		Priority: model.PriorityNone,
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}
	if stats.Conflicts != 1 {
		t.Errorf("Conflicts = %d, want 1", stats.Conflicts)
	}

	// HA should have Reminders' version.
	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 1 || haItems[0].Title != "Buy whole milk" {
		t.Errorf("HA item title = %q, want %q", haItems[0].Title, "Buy whole milk")
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: Both sides updated, HA newer → HA wins
// ---------------------------------------------------------------------------

func TestReconcile_Conflict_HAWins(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	remTime := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	haObservedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) // after remTime → HA is newer

	origItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, older)
	origHash := origItem.ContentHash()

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID:      "rem-1",
		HAUID:             "ha-1",
		ListName:          "Shopping",
		Title:             "Buy milk",
		LastSyncHash:      origHash,
		RemindersModified: older,
		HAModified:        haObservedAt,
		LastSyncedAt:      older,
	})

	remItem := newItem("rem-1", "Buy skim milk", "Shopping", model.PriorityNone, false, remTime)
	rem := newMockReminders(remItem)

	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:      "ha-1",
		Title:    "Buy whole milk",
		Priority: model.PriorityNone,
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}

	// Reminders should have HA's version.
	got := rem.get("rem-1")
	if got == nil || got.Title != "Buy whole milk" {
		title := ""
		if got != nil {
			title = got.Title
		}
		t.Errorf("Reminders item title = %q, want %q", title, "Buy whole milk")
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: Deleted from Reminders → removed from HA + state DB
// ---------------------------------------------------------------------------

func TestReconcile_DeletedFromReminders_RemovedFromHA(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID: "rem-1",
		HAUID:        "ha-1",
		ListName:     "Shopping",
		Title:        "Buy milk",
		LastSyncHash: "old-hash",
		LastSyncedAt: older,
	})

	// Reminders: item gone.
	rem := newMockReminders()

	// HA: item still exists.
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:   "ha-1",
		Title: "Buy milk",
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", stats.Deleted)
	}

	// HA should be empty.
	if len(ha.getItems("todo.shopping")) != 0 {
		t.Error("HA item should have been deleted")
	}

	// State DB should be empty.
	if store.count() != 0 {
		t.Error("state DB should be empty")
	}
}

// ---------------------------------------------------------------------------
// Scenario 6: Deleted from HA → removed from Reminders + state DB
// ---------------------------------------------------------------------------

func TestReconcile_DeletedFromHA_RemovedFromReminders(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, older)

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID: "rem-1",
		HAUID:        "ha-1",
		ListName:     "Shopping",
		Title:        "Buy milk",
		LastSyncHash: "old-hash",
		LastSyncedAt: older,
	})

	rem := newMockReminders(remItem)
	ha := newMockHA() // HA: item gone

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", stats.Deleted)
	}

	// Reminders should be empty.
	if rem.count() != 0 {
		t.Error("Reminders item should have been deleted")
	}

	// State DB should be empty.
	if store.count() != 0 {
		t.Error("state DB should be empty")
	}
}

// ---------------------------------------------------------------------------
// Scenario 7: No changes → no mutations (idempotent pass)
// ---------------------------------------------------------------------------

func TestReconcile_NoChanges_Idempotent(t *testing.T) {
	now := time.Now().UTC()
	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, now)
	hash := remItem.ContentHash()

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID: "rem-1",
		HAUID:        "ha-1",
		ListName:     "Shopping",
		Title:        "Buy milk",
		LastSyncHash: hash,
		LastSyncedAt: now,
	})

	rem := newMockReminders(remItem)
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:        "ha-1",
		Title:      "Buy milk",
		Priority:   model.PriorityNone,
		Completed:  false,
		ModifiedAt: now,
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Created != 0 || stats.Updated != 0 || stats.Deleted != 0 {
		t.Errorf("expected no mutations, got Created=%d Updated=%d Deleted=%d",
			stats.Created, stats.Updated, stats.Deleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Only Reminders changed → propagate to HA (no conflict)
// ---------------------------------------------------------------------------

func TestReconcile_OnlyRemindersChanged_UpdatesHA(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	origItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, older)
	origHash := origItem.ContentHash()

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID: "rem-1",
		HAUID:        "ha-1",
		ListName:     "Shopping",
		Title:        "Buy milk",
		LastSyncHash: origHash,
		LastSyncedAt: older,
	})

	// Reminders: updated title.
	remItem := newItem("rem-1", "Buy whole milk", "Shopping", model.PriorityNone, false, newer)
	rem := newMockReminders(remItem)

	// HA: unchanged (same hash as original).
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:        "ha-1",
		Title:      "Buy milk",
		Priority:   model.PriorityNone,
		ModifiedAt: older,
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}
	if stats.Conflicts != 0 {
		t.Errorf("Conflicts = %d, want 0 (only one side changed)", stats.Conflicts)
	}

	// HA should have updated title.
	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 1 || haItems[0].Title != "Buy whole milk" {
		t.Errorf("HA item title = %q, want %q", haItems[0].Title, "Buy whole milk")
	}
}

// ---------------------------------------------------------------------------
// Scenario: Only HA changed → propagate to Reminders (no conflict)
// ---------------------------------------------------------------------------

func TestReconcile_OnlyHAChanged_UpdatesReminders(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	origItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, older)
	origHash := origItem.ContentHash()

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID: "rem-1",
		HAUID:        "ha-1",
		ListName:     "Shopping",
		Title:        "Buy milk",
		LastSyncHash: origHash,
		LastSyncedAt: older,
	})

	// Reminders: unchanged.
	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, older)
	rem := newMockReminders(remItem)

	// HA: updated title.
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:        "ha-1",
		Title:      "Buy whole milk",
		Priority:   model.PriorityNone,
		ModifiedAt: newer,
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}

	got := rem.get("rem-1")
	if got == nil || got.Title != "Buy whole milk" {
		title := ""
		if got != nil {
			title = got.Title
		}
		t.Errorf("Reminders item title = %q, want %q", title, "Buy whole milk")
	}
}

// ---------------------------------------------------------------------------
// Scenario: Multiple items across lists
// ---------------------------------------------------------------------------

func TestReconcile_MultipleItems(t *testing.T) {
	now := time.Now().UTC()

	rem := newMockReminders(
		newItem("rem-1", "Existing", "Shopping", model.PriorityNone, false, now),
		newItem("rem-2", "New from Rem", "Shopping", model.PriorityNone, false, now),
	)

	ha := newMockHA()
	ha.addItems("todo.shopping",
		model.Item{UID: "ha-1", Title: "Existing", ModifiedAt: now},
		model.Item{UID: "ha-3", Title: "New from HA", ModifiedAt: now},
	)

	// Only "Existing" is tracked.
	existingItem := newItem("rem-1", "Existing", "Shopping", model.PriorityNone, false, now)
	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID: "rem-1",
		HAUID:        "ha-1",
		ListName:     "Shopping",
		Title:        "Existing",
		LastSyncHash: existingItem.ContentHash(),
		LastSyncedAt: now,
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// "New from Rem" → created in HA, "New from HA" → created in Reminders.
	if stats.Created != 2 {
		t.Errorf("Created = %d, want 2", stats.Created)
	}
	if stats.Updated != 0 {
		t.Errorf("Updated = %d, want 0", stats.Updated)
	}

	// State should have 3 entries total.
	if store.count() != 3 {
		t.Errorf("state items = %d, want 3", store.count())
	}
}

// ---------------------------------------------------------------------------
// Scenario: Completed status changed in Reminders → propagate to HA
// ---------------------------------------------------------------------------

func TestReconcile_CompletedStatusChange(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	origItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, older)
	origHash := origItem.ContentHash()

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID: "rem-1",
		HAUID:        "ha-1",
		ListName:     "Shopping",
		Title:        "Buy milk",
		LastSyncHash: origHash,
		LastSyncedAt: older,
	})

	// Reminders: completed.
	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, true, newer)
	rem := newMockReminders(remItem)

	// HA: unchanged.
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:        "ha-1",
		Title:      "Buy milk",
		Priority:   model.PriorityNone,
		Completed:  false,
		ModifiedAt: older,
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}

	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 1 || !haItems[0].Completed {
		t.Error("HA item should be completed after sync")
	}
}

// ---------------------------------------------------------------------------
// mergeField / mergeDueDate unit tests
// ---------------------------------------------------------------------------

func TestMergeField_NeitherChanged(t *testing.T) {
	resolved, remDiff, haDiff, conflict := mergeField("synced", "synced", "synced", time.Time{}, time.Time{})
	if resolved != "synced" || remDiff || haDiff || conflict {
		t.Errorf("got (%q, %v, %v, %v), want (\"synced\", false, false, false)", resolved, remDiff, haDiff, conflict)
	}
}

func TestMergeField_OnlyRemindersChanged(t *testing.T) {
	resolved, remDiff, haDiff, conflict := mergeField("synced", "new", "synced", time.Time{}, time.Time{})
	if resolved != "new" || remDiff || !haDiff || conflict {
		t.Errorf("got (%q, remDiff=%v, haDiff=%v, conflict=%v), want (\"new\", false, true, false)", resolved, remDiff, haDiff, conflict)
	}
}

func TestMergeField_OnlyHAChanged(t *testing.T) {
	resolved, remDiff, haDiff, conflict := mergeField("synced", "synced", "new", time.Time{}, time.Time{})
	if resolved != "new" || !remDiff || haDiff || conflict {
		t.Errorf("got (%q, remDiff=%v, haDiff=%v, conflict=%v), want (\"new\", true, false, false)", resolved, remDiff, haDiff, conflict)
	}
}

func TestMergeField_BothChangedSameValue_NotAConflict(t *testing.T) {
	resolved, remDiff, haDiff, conflict := mergeField("synced", "new", "new", time.Time{}, time.Time{})
	if resolved != "new" || remDiff || haDiff || conflict {
		t.Errorf("got (%q, remDiff=%v, haDiff=%v, conflict=%v), want (\"new\", false, false, false)", resolved, remDiff, haDiff, conflict)
	}
}

func TestMergeField_BothChangedDifferently_ConflictTieBreak(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)

	// Reminders newer → Reminders wins.
	resolved, remDiff, haDiff, conflict := mergeField("synced", "rem-value", "ha-value", newer, older)
	if resolved != "rem-value" || remDiff || !haDiff || !conflict {
		t.Errorf("Reminders-newer: got (%q, remDiff=%v, haDiff=%v, conflict=%v), want (\"rem-value\", false, true, true)", resolved, remDiff, haDiff, conflict)
	}

	// HA newer → HA wins.
	resolved, remDiff, haDiff, conflict = mergeField("synced", "rem-value", "ha-value", older, newer)
	if resolved != "ha-value" || !remDiff || haDiff || !conflict {
		t.Errorf("HA-newer: got (%q, remDiff=%v, haDiff=%v, conflict=%v), want (\"ha-value\", true, false, true)", resolved, remDiff, haDiff, conflict)
	}

	// Equal clocks → Reminders wins (the documented tie-break default).
	resolved, _, _, conflict = mergeField("synced", "rem-value", "ha-value", newer, newer)
	if resolved != "rem-value" || !conflict {
		t.Errorf("equal clocks: got (%q, conflict=%v), want (\"rem-value\", true)", resolved, conflict)
	}
}

func TestMergeDueDate_NilOnBothSides(t *testing.T) {
	resolved, remDiff, haDiff, conflict := mergeDueDate("", nil, nil, time.Time{}, time.Time{})
	if resolved != nil || remDiff || haDiff || conflict {
		t.Errorf("got (%v, remDiff=%v, haDiff=%v, conflict=%v), want (nil, false, false, false)", resolved, remDiff, haDiff, conflict)
	}
}

func TestMergeDueDate_OnlyRemindersSetIt(t *testing.T) {
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	resolved, remDiff, haDiff, conflict := mergeDueDate("", &due, nil, time.Time{}, time.Time{})
	if resolved == nil || !resolved.Equal(due) || remDiff || !haDiff || conflict {
		t.Errorf("got (%v, remDiff=%v, haDiff=%v, conflict=%v), want (%v, false, true, false)", resolved, remDiff, haDiff, conflict, due)
	}
}

func TestMergeDueDate_BothChangedDifferently_ConflictTieBreak(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	remDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	haDue := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	resolved, _, _, conflict := mergeDueDate("", &remDue, &haDue, newer, older)
	if resolved == nil || !resolved.Equal(remDue) || !conflict {
		t.Errorf("Reminders-newer: got (%v, conflict=%v), want (%v, true)", resolved, conflict, remDue)
	}
}

// ---------------------------------------------------------------------------
// mergeDueDate3 unit tests
// ---------------------------------------------------------------------------

func TestMergeDueDate3_NoneChanged(t *testing.T) {
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	key := dueDateKey(&due)
	resolved, remDiff, haDiff, calDiff, conflict := mergeDueDate3(key, &due, &due, &due, time.Time{}, time.Time{}, time.Time{})
	if resolved == nil || !resolved.Equal(due) || remDiff || haDiff || calDiff || conflict {
		t.Errorf("got (%v, remDiff=%v, haDiff=%v, calDiff=%v, conflict=%v), want (%v, false, false, false, false)", resolved, remDiff, haDiff, calDiff, conflict, due)
	}
}

func TestMergeDueDate3_OnlyCalendarChanged(t *testing.T) {
	oldDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newDue := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	syncedKey := dueDateKey(&oldDue)

	resolved, remDiff, haDiff, calDiff, conflict := mergeDueDate3(syncedKey, &oldDue, &oldDue, &newDue, time.Time{}, time.Time{}, time.Time{})
	if resolved == nil || !resolved.Equal(newDue) || conflict {
		t.Fatalf("got (%v, conflict=%v), want (%v, false)", resolved, conflict, newDue)
	}
	if !remDiff || !haDiff || calDiff {
		t.Errorf("remDiff=%v haDiff=%v calDiff=%v, want (true, true, false) — Reminders and HA both need the calendar's new value", remDiff, haDiff, calDiff)
	}
}

func TestMergeDueDate3_CalendarEventDeleted_ClearsAllThree(t *testing.T) {
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	syncedKey := dueDateKey(&due)

	// calDue nil represents either a genuinely cleared due date on the
	// calendar, or the mirrored event having been deleted — both look the
	// same to the merge.
	resolved, remDiff, haDiff, calDiff, conflict := mergeDueDate3(syncedKey, &due, &due, nil, time.Time{}, time.Time{}, time.Time{})
	if resolved != nil || conflict {
		t.Fatalf("got (%v, conflict=%v), want (nil, false)", resolved, conflict)
	}
	if !remDiff || !haDiff || calDiff {
		t.Errorf("remDiff=%v haDiff=%v calDiff=%v, want (true, true, false)", remDiff, haDiff, calDiff)
	}
}

func TestMergeDueDate3_AllThreeChangedToSameValue_NotAConflict(t *testing.T) {
	oldDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newDue := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	syncedKey := dueDateKey(&oldDue)

	resolved, remDiff, haDiff, calDiff, conflict := mergeDueDate3(syncedKey, &newDue, &newDue, &newDue, time.Time{}, time.Time{}, time.Time{})
	if resolved == nil || !resolved.Equal(newDue) || conflict {
		t.Errorf("got (%v, conflict=%v), want (%v, false)", resolved, conflict, newDue)
	}
	if remDiff || haDiff || calDiff {
		t.Errorf("remDiff=%v haDiff=%v calDiff=%v, want all false — every side already agrees", remDiff, haDiff, calDiff)
	}
}

func TestMergeDueDate3_ThreeWayConflict_ClockTieBreak(t *testing.T) {
	oldDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	remDue := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	haDue := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	calDue := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	syncedKey := dueDateKey(&oldDue)

	t1 := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)

	// Calendar has the latest clock → calendar wins.
	resolved, remDiff, haDiff, calDiff, conflict := mergeDueDate3(syncedKey, &remDue, &haDue, &calDue, t1, t2, t3)
	if resolved == nil || !resolved.Equal(calDue) || !conflict {
		t.Fatalf("calendar-newest: got (%v, conflict=%v), want (%v, true)", resolved, conflict, calDue)
	}
	if !remDiff || !haDiff || calDiff {
		t.Errorf("calendar-newest: remDiff=%v haDiff=%v calDiff=%v, want (true, true, false)", remDiff, haDiff, calDiff)
	}

	// Reminders has the latest clock → Reminders wins.
	resolved, _, _, _, conflict = mergeDueDate3(syncedKey, &remDue, &haDue, &calDue, t3, t2, t1)
	if resolved == nil || !resolved.Equal(remDue) || !conflict {
		t.Errorf("reminders-newest: got (%v, conflict=%v), want (%v, true)", resolved, conflict, remDue)
	}

	// All equal clocks → Reminders wins (first in precedence order).
	resolved, _, _, _, _ = mergeDueDate3(syncedKey, &remDue, &haDue, &calDue, t1, t1, t1)
	if resolved == nil || !resolved.Equal(remDue) {
		t.Errorf("equal clocks: got %v, want %v (Reminders precedence)", resolved, remDue)
	}
}

func TestMergeDueDate3_HAAndCalendarTie_HAWinsOverCalendar(t *testing.T) {
	oldDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	haDue := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	calDue := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	syncedKey := dueDateKey(&oldDue)

	sameClock := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	// Reminders unchanged; HA and Calendar both changed with equal clocks —
	// documented precedence is HA over Calendar in that case.
	resolved, remDiff, haDiff, calDiff, conflict := mergeDueDate3(syncedKey, &oldDue, &haDue, &calDue, time.Time{}, sameClock, sameClock)
	if resolved == nil || !resolved.Equal(haDue) || !conflict {
		t.Fatalf("got (%v, conflict=%v), want (%v, true)", resolved, conflict, haDue)
	}
	if !remDiff || haDiff || !calDiff {
		t.Errorf("remDiff=%v haDiff=%v calDiff=%v, want (true, false, true)", remDiff, haDiff, calDiff)
	}
}

func TestDueDateKey_NilAndRoundTrip(t *testing.T) {
	if got := dueDateKey(nil); got != "" {
		t.Errorf("dueDateKey(nil) = %q, want empty", got)
	}
	due := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	if got := dueDateKey(&due); got != due.Format(time.RFC3339) {
		t.Errorf("dueDateKey = %q, want %q", got, due.Format(time.RFC3339))
	}
}

// ---------------------------------------------------------------------------
// Field-level merge: an unrelated change on each side should NOT be treated
// as a conflict, and both changes should survive — this is the behaviour
// the field-by-field merge replaces whole-item last-write-wins with.
// ---------------------------------------------------------------------------

func TestReconcile_IndependentFieldChanges_BothSurvive_NoConflict(t *testing.T) {
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	origItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, older)
	origHash := origItem.ContentHash()

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID: "rem-1",
		HAUID:        "ha-1",
		ListName:     "Shopping",
		Title:        "Buy milk",
		LastSyncHash: origHash,
		LastSyncedAt: older,
	})

	// Reminders: due date added (title/completed unchanged).
	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, newer)
	remItem.DueDate = &due
	rem := newMockReminders(remItem)

	// HA: marked completed (title/due unchanged) — a different field.
	ha := newMockHA()
	ha.addItems("todo.shopping", model.Item{
		UID:       "ha-1",
		Title:     "Buy milk",
		Priority:  model.PriorityNone,
		Completed: true,
	})

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Conflicts != 0 {
		t.Errorf("Conflicts = %d, want 0 (different fields changed on each side)", stats.Conflicts)
	}
	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}

	// HA should have picked up the due date from Reminders.
	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 1 || haItems[0].DueDate == nil {
		t.Fatal("HA item should have picked up the due date from Reminders")
	}

	// Reminders should have picked up the completed flag from HA.
	got := rem.get("rem-1")
	if got == nil || !got.Completed {
		t.Error("Reminders item should have picked up Completed=true from HA")
	}
}

// ---------------------------------------------------------------------------
// Calendar mirroring
// ---------------------------------------------------------------------------

func calendarTestMappings() map[string]model.ListMapping {
	return map[string]model.ListMapping{
		"Shopping": {HAEntity: "todo.shopping", HACalendarEntity: "calendar.shopping_due"},
	}
}

func TestReconcile_CalendarMirror_CreatesEventForNewDueDate(t *testing.T) {
	now := time.Now().UTC()
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, now)
	remItem.DueDate = &due
	rem := newMockReminders(remItem)

	ha := newMockHACalendar()
	store := newMockStore()

	r := NewReconciler(rem, ha, store, testLogger)
	_, err := r.Run(context.Background(), calendarTestMappings())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ha.eventCount("calendar.shopping_due") != 1 {
		t.Fatalf("calendar events = %d, want 1", ha.eventCount("calendar.shopping_due"))
	}

	si, err := store.GetItemByRemindersUID(context.Background(), "rem-1")
	if err != nil || si == nil {
		t.Fatalf("expected state row for rem-1, err=%v", err)
	}
	if si.CalendarEventUID == "" {
		t.Fatal("state row should record the mirrored event's UID")
	}
	if got := ha.getEvent("calendar.shopping_due", si.CalendarEventUID); got == nil || got.Title != "Buy milk" {
		t.Errorf("mirrored event = %v, want title 'Buy milk'", got)
	}
}

func TestReconcile_CalendarMirror_CompletionMarksDoneWithoutDeleting(t *testing.T) {
	now := time.Now().UTC()
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID:     "rem-1",
		HAUID:            "ha-1",
		ListName:         "Shopping",
		Title:            "Buy milk",
		SyncedDueDate:    dueDateKey(&due),
		CalendarEventUID: "cal-1",
	})

	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, true, now) // now completed
	remItem.DueDate = &due
	rem := newMockReminders(remItem)

	ha := newMockHACalendar()
	ha.addItems("todo.shopping", model.Item{UID: "ha-1", Title: "Buy milk", DueDate: &due, Completed: false})
	ha.events["calendar.shopping_due"] = map[string]*model.Item{
		"cal-1": {Title: "Buy milk", DueDate: &due, Completed: false},
	}

	r := NewReconciler(rem, ha, store, testLogger)
	_, err := r.Run(context.Background(), calendarTestMappings())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ha.eventCount("calendar.shopping_due") != 1 {
		t.Error("completing the item should keep the mirrored event, not delete it")
	}
	got := ha.getEvent("calendar.shopping_due", "cal-1")
	if got == nil || !got.Completed {
		t.Errorf("mirrored event = %v, want Completed=true", got)
	}
}

func TestReconcile_CalendarMirror_DueDateClearedDeletesEvent(t *testing.T) {
	now := time.Now().UTC()
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	store := newMockStore()
	store.seed(&state.Item{
		RemindersUID:     "rem-1",
		HAUID:            "ha-1",
		ListName:         "Shopping",
		Title:            "Buy milk",
		SyncedDueDate:    dueDateKey(&due),
		CalendarEventUID: "cal-1",
	})

	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, now) // DueDate now nil
	rem := newMockReminders(remItem)

	ha := newMockHACalendar()
	ha.addItems("todo.shopping", model.Item{UID: "ha-1", Title: "Buy milk"})
	ha.events["calendar.shopping_due"] = map[string]*model.Item{
		"cal-1": {Title: "Buy milk", DueDate: &due},
	}

	r := NewReconciler(rem, ha, store, testLogger)
	_, err := r.Run(context.Background(), calendarTestMappings())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ha.eventCount("calendar.shopping_due") != 0 {
		t.Error("clearing the due date should delete the mirrored event")
	}
}

func TestReconcile_CalendarMirror_SkippedWhenEntityLacksMutationSupport(t *testing.T) {
	now := time.Now().UTC()
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, now)
	remItem.DueDate = &due
	rem := newMockReminders(remItem)

	ha := newMockHACalendar()
	ha.mutable = false // e.g. a calendar backend without DELETE_EVENT/UPDATE_EVENT
	store := newMockStore()

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), calendarTestMappings())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ha.eventCount("calendar.shopping_due") != 0 {
		t.Error("no event should be mirrored when the calendar entity lacks mutation support")
	}
	if stats.Errors == 0 {
		t.Error("expected a logged error for the unsupported calendar entity")
	}
	// The underlying todo item should still have synced normally.
	if len(ha.getItems("todo.shopping")) != 1 {
		t.Error("todo sync itself should not be blocked by calendar mirror failure")
	}
}

func TestReconcile_CalendarMirror_NotConfigured_NoOp(t *testing.T) {
	now := time.Now().UTC()
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, now)
	remItem.DueDate = &due
	rem := newMockReminders(remItem)

	ha := newMockHACalendar() // implements CalendarSource, but no calendar entity configured
	store := newMockStore()

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), testMappings) // testMappings has no HACalendarEntity
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}
	if ha.eventCount("calendar.shopping_due") != 0 {
		t.Error("no calendar entity configured — nothing should be mirrored")
	}
}

// ---------------------------------------------------------------------------
// Calendar as a genuine 3-way due-date participant (once a mirrored event
// already exists — see calParticipates in reconcileList).
// ---------------------------------------------------------------------------

// seedThreeWayState seeds a state row whose mirrored calendar event already
// exists, with Reminders/HA/Calendar all agreeing on oldDue — the baseline
// every 3-way test in this section starts from.
func seedThreeWayState(t *testing.T, store *mockStore, oldDue time.Time, haModified, calModified time.Time) {
	t.Helper()
	origItem := &model.Item{Title: "Buy milk", DueDate: &oldDue}
	origHash := origItem.ContentHash()
	mirrorItem := &model.Item{Title: "Buy milk", DueDate: &oldDue, Completed: false}

	store.seed(&state.Item{
		RemindersUID:        "rem-1",
		HAUID:               "ha-1",
		ListName:            "Shopping",
		Title:               "Buy milk",
		SyncedDueDate:       dueDateKey(&oldDue),
		LastSyncHash:        origHash,
		HALastSeenHash:      origHash,
		HAModified:          haModified,
		CalendarEventUID:    "cal-1",
		CalendarSyncHash:    mirrorItem.CalendarHash(),
		CalendarLastSeenDue: dueDateKey(&oldDue),
		CalendarModified:    calModified,
	})
}

func TestReconcile_CalendarOnlyDueDateChange_PropagatesToBothSides(t *testing.T) {
	oldDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newDue := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC()

	store := newMockStore()
	seedThreeWayState(t, store, oldDue, time.Time{}, time.Time{})

	// Reminders and HA are unchanged from the synced state; only the
	// calendar event was moved (e.g. dragged on the HA calendar UI).
	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, now)
	remItem.DueDate = &oldDue
	rem := newMockReminders(remItem)

	ha := newMockHACalendar()
	ha.addItems("todo.shopping", model.Item{UID: "ha-1", Title: "Buy milk", DueDate: &oldDue})
	ha.events["calendar.shopping_due"] = map[string]*model.Item{
		"cal-1": {Title: "Buy milk", DueDate: &newDue},
	}

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), calendarTestMappings())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Conflicts != 0 {
		t.Errorf("Conflicts = %d, want 0 (only the calendar changed)", stats.Conflicts)
	}

	got := rem.get("rem-1")
	if got == nil || got.DueDate == nil || !got.DueDate.Equal(newDue) {
		t.Errorf("Reminders due date = %v, want %v", got, newDue)
	}
	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 1 || haItems[0].DueDate == nil || !haItems[0].DueDate.Equal(newDue) {
		t.Errorf("HA due date = %v, want %v", haItems, newDue)
	}
}

func TestReconcile_CalendarEventDeletedDirectly_ClearsDueDateOnBothSides(t *testing.T) {
	oldDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC()

	store := newMockStore()
	seedThreeWayState(t, store, oldDue, time.Time{}, time.Time{})

	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, now)
	remItem.DueDate = &oldDue
	rem := newMockReminders(remItem)

	ha := newMockHACalendar()
	ha.addItems("todo.shopping", model.Item{UID: "ha-1", Title: "Buy milk", DueDate: &oldDue})
	// No "cal-1" entry — the user deleted the event directly on the calendar.

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), calendarTestMappings())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0 — deleting the already-gone event should not be attempted", stats.Errors)
	}

	got := rem.get("rem-1")
	if got == nil || got.DueDate != nil {
		t.Errorf("Reminders due date = %v, want nil (cleared)", got)
	}
	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 1 || haItems[0].DueDate != nil {
		t.Errorf("HA due date = %v, want nil (cleared)", haItems)
	}

	si, err := store.GetItemByRemindersUID(context.Background(), "rem-1")
	if err != nil || si == nil {
		t.Fatalf("expected state row, err=%v", err)
	}
	if si.CalendarEventUID != "" {
		t.Errorf("CalendarEventUID = %q, want empty after the mirror was cleared", si.CalendarEventUID)
	}
}

func TestReconcile_ThreeWayDueDateConflict_LatestClockWins(t *testing.T) {
	oldDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	remDue := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	haDue := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	calDue := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)

	remModified := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) // latest — Reminders should win
	haObserved := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	calObserved := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	store := newMockStore()
	seedThreeWayState(t, store, oldDue, haObserved, calObserved)

	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, remModified)
	remItem.DueDate = &remDue
	rem := newMockReminders(remItem)

	ha := newMockHACalendar()
	ha.addItems("todo.shopping", model.Item{UID: "ha-1", Title: "Buy milk", DueDate: &haDue})
	ha.events["calendar.shopping_due"] = map[string]*model.Item{
		"cal-1": {Title: "Buy milk", DueDate: &calDue},
	}

	r := NewReconciler(rem, ha, store, testLogger)
	stats, err := r.Run(context.Background(), calendarTestMappings())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Conflicts != 1 {
		t.Errorf("Conflicts = %d, want 1", stats.Conflicts)
	}

	got := rem.get("rem-1")
	if got == nil || got.DueDate == nil || !got.DueDate.Equal(remDue) {
		t.Errorf("Reminders due date = %v, want %v (Reminders' own value, unchanged)", got, remDue)
	}
	haItems := ha.getItems("todo.shopping")
	if len(haItems) != 1 || haItems[0].DueDate == nil || !haItems[0].DueDate.Equal(remDue) {
		t.Errorf("HA due date = %v, want %v (Reminders won the conflict)", haItems, remDue)
	}

	mirrored := ha.getEvent("calendar.shopping_due", "cal-1")
	if mirrored == nil || mirrored.DueDate == nil || !mirrored.DueDate.Equal(remDue) {
		t.Errorf("mirrored calendar event = %v, want due date %v (Reminders won the conflict)", mirrored, remDue)
	}
}
