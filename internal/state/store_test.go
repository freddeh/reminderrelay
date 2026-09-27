package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleItem() *Item {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &Item{
		RemindersUID:      "rem-uid-001",
		HAUID:             "ha-uid-001",
		ListName:          "Shopping",
		Title:             "Buy milk",
		LastSyncHash:      "abc123",
		RemindersModified: now,
		HAModified:        now,
		LastSyncedAt:      now,
	}
}

func TestOpen_CreatesSchema(t *testing.T) {
	s := openTestStore(t)
	// IsEmpty queries sync_items — if schema is wrong this panics.
	empty, err := s.IsEmpty(context.Background())
	if err != nil {
		t.Fatalf("IsEmpty after open: %v", err)
	}
	if !empty {
		t.Error("expected empty store after open")
	}
}

func TestOpen_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("s1.Close: %v", err)
	}

	// Re-opening the same file must not fail or wipe data.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("s2.Close: %v", err)
	}
}

func TestUpsertAndGetByRemindersUID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	item := sampleItem()

	if err := s.UpsertItem(ctx, item); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if item.ID == 0 {
		t.Error("UpsertItem did not set ID")
	}

	got, err := s.GetItemByRemindersUID(ctx, "rem-uid-001")
	if err != nil {
		t.Fatalf("GetItemByRemindersUID: %v", err)
	}
	if got == nil {
		t.Fatal("GetItemByRemindersUID returned nil, want item")
	}
	if got.Title != "Buy milk" {
		t.Errorf("Title = %q, want %q", got.Title, "Buy milk")
	}
	if got.HAUID != "ha-uid-001" {
		t.Errorf("HAUID = %q, want %q", got.HAUID, "ha-uid-001")
	}
}

func TestUpsertAndGetByHAUID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertItem(ctx, sampleItem()); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	got, err := s.GetItemByHAUID(ctx, "ha-uid-001")
	if err != nil {
		t.Fatalf("GetItemByHAUID: %v", err)
	}
	if got == nil {
		t.Fatal("GetItemByHAUID returned nil, want item")
	}
	if got.RemindersUID != "rem-uid-001" {
		t.Errorf("RemindersUID = %q, want %q", got.RemindersUID, "rem-uid-001")
	}
}

func TestGetByRemindersUID_NotFound(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetItemByRemindersUID(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing item, got %+v", got)
	}
}

func TestUpsert_UpdatePath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	item := sampleItem()

	if err := s.UpsertItem(ctx, item); err != nil {
		t.Fatalf("initial UpsertItem: %v", err)
	}

	// Update title and hash via a second upsert on the same RemindersUID.
	item.Title = "Buy oat milk"
	item.LastSyncHash = "newHash"
	if err := s.UpsertItem(ctx, item); err != nil {
		t.Fatalf("update UpsertItem: %v", err)
	}

	got, err := s.GetItemByRemindersUID(ctx, "rem-uid-001")
	if err != nil {
		t.Fatalf("GetItemByRemindersUID: %v", err)
	}
	if got.Title != "Buy oat milk" {
		t.Errorf("Title = %q, want %q", got.Title, "Buy oat milk")
	}
	if got.LastSyncHash != "newHash" {
		t.Errorf("LastSyncHash = %q, want %q", got.LastSyncHash, "newHash")
	}

	// Must still be exactly one row.
	all, err := s.GetAllItemsForList(ctx, "Shopping")
	if err != nil {
		t.Fatalf("GetAllItemsForList: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 item after update, got %d", len(all))
	}
}

// TestUpsertItem_DuplicateHAUID_Rejected guards the idx_ha_uid unique index:
// a second row for a different Reminders item must not be allowed to claim
// an HA UID that's already tracked by another row, even though UpsertItem's
// ON CONFLICT only targets reminders_uid.
func TestUpsertItem_DuplicateHAUID_Rejected(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first := sampleItem()
	if err := s.UpsertItem(ctx, first); err != nil {
		t.Fatalf("first UpsertItem: %v", err)
	}

	second := sampleItem()
	second.RemindersUID = "rem-uid-002" // different Reminders item...
	second.HAUID = first.HAUID          // ...but claims the same HA UID.

	if err := s.UpsertItem(ctx, second); err == nil {
		t.Fatal("expected UpsertItem to fail on duplicate ha_uid, got nil error")
	}

	all, err := s.GetAllItemsForList(ctx, "Shopping")
	if err != nil {
		t.Fatalf("GetAllItemsForList: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 item after rejected duplicate, got %d", len(all))
	}
}

// TestUpsertItem_DuplicateRemindersUID_ViaDirectInsert_Rejected guards the
// idx_reminders_uid unique index directly at the database level, independent
// of UpsertItem's own ON CONFLICT merge logic.
func TestUpsertItem_DuplicateRemindersUID_ViaDirectInsert_Rejected(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertItem(ctx, sampleItem()); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	const insert = `
		INSERT INTO sync_items
		    (reminders_uid, ha_uid, list_name, title, last_sync_hash,
		     reminders_modified, ha_modified, last_synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, insert,
		"rem-uid-001", "ha-uid-002", "Shopping", "Duplicate", "", "", "", "")
	if err == nil {
		t.Fatal("expected raw INSERT with duplicate reminders_uid to violate the unique index, got nil error")
	}
}

// TestUpsertItem_UnlinkedItemsOnOppositeSides_DoNotConflict ensures the
// partial unique indexes (WHERE ... != '') don't block the legitimate case
// of multiple items that are each tracked on only one side and not yet
// linked to a counterpart.
func TestUpsertItem_UnlinkedItemsOnOppositeSides_DoNotConflict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	remOnly := &Item{RemindersUID: "rem-only", HAUID: "", ListName: "Shopping", Title: "Rem only"}
	haOnly := &Item{RemindersUID: "", HAUID: "ha-only", ListName: "Shopping", Title: "HA only"}

	if err := s.UpsertItem(ctx, remOnly); err != nil {
		t.Fatalf("UpsertItem(remOnly): %v", err)
	}
	if err := s.UpsertItem(ctx, haOnly); err != nil {
		t.Fatalf("UpsertItem(haOnly): %v", err)
	}

	all, err := s.GetAllItemsForList(ctx, "Shopping")
	if err != nil {
		t.Fatalf("GetAllItemsForList: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 items, got %d", len(all))
	}
}

func TestGetAllItemsForList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	items := []*Item{
		{RemindersUID: "r1", HAUID: "h1", ListName: "Shopping", Title: "Milk"},
		{RemindersUID: "r2", HAUID: "h2", ListName: "Shopping", Title: "Eggs"},
		{RemindersUID: "r3", HAUID: "h3", ListName: "Work", Title: "Email"},
	}
	for _, it := range items {
		if err := s.UpsertItem(ctx, it); err != nil {
			t.Fatalf("UpsertItem %q: %v", it.Title, err)
		}
	}

	shopping, err := s.GetAllItemsForList(ctx, "Shopping")
	if err != nil {
		t.Fatalf("GetAllItemsForList(Shopping): %v", err)
	}
	if len(shopping) != 2 {
		t.Errorf("Shopping list: got %d items, want 2", len(shopping))
	}

	work, err := s.GetAllItemsForList(ctx, "Work")
	if err != nil {
		t.Fatalf("GetAllItemsForList(Work): %v", err)
	}
	if len(work) != 1 {
		t.Errorf("Work list: got %d items, want 1", len(work))
	}

	none, err := s.GetAllItemsForList(ctx, "Nonexistent")
	if err != nil {
		t.Fatalf("GetAllItemsForList(Nonexistent): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("Nonexistent list: got %d items, want 0", len(none))
	}
}

func TestDeleteItem(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	item := sampleItem()

	if err := s.UpsertItem(ctx, item); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	if err := s.DeleteItem(ctx, item.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	got, err := s.GetItemByRemindersUID(ctx, "rem-uid-001")
	if err != nil {
		t.Fatalf("GetItemByRemindersUID after delete: %v", err)
	}
	if got != nil {
		t.Error("expected nil after delete, got item")
	}

	empty, err := s.IsEmpty(ctx)
	if err != nil {
		t.Fatalf("IsEmpty: %v", err)
	}
	if !empty {
		t.Error("expected store to be empty after deleting only item")
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Use a time with sub-millisecond precision to exercise RFC3339Nano.
	ts := time.Date(2026, 2, 17, 14, 30, 0, 123456789, time.UTC)
	item := &Item{
		RemindersUID:      "ts-test",
		HAUID:             "ts-ha",
		ListName:          "Test",
		Title:             "Timestamp test",
		RemindersModified: ts,
		HAModified:        ts,
		LastSyncedAt:      ts,
	}
	if err := s.UpsertItem(ctx, item); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	got, err := s.GetItemByRemindersUID(ctx, "ts-test")
	if err != nil {
		t.Fatalf("GetItemByRemindersUID: %v", err)
	}
	if !got.RemindersModified.Equal(ts) {
		t.Errorf("RemindersModified = %v, want %v", got.RemindersModified, ts)
	}
	if !got.HAModified.Equal(ts) {
		t.Errorf("HAModified = %v, want %v", got.HAModified, ts)
	}
	if !got.LastSyncedAt.Equal(ts) {
		t.Errorf("LastSyncedAt = %v, want %v", got.LastSyncedAt, ts)
	}
}

func TestZeroTimestampsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	item := &Item{
		RemindersUID: "zero-ts",
		HAUID:        "zero-ha",
		ListName:     "Test",
		Title:        "Zero timestamps",
		// RemindersModified, HAModified, LastSyncedAt all zero
	}
	if err := s.UpsertItem(ctx, item); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	got, err := s.GetItemByRemindersUID(ctx, "zero-ts")
	if err != nil {
		t.Fatalf("GetItemByRemindersUID: %v", err)
	}
	if !got.RemindersModified.IsZero() {
		t.Errorf("expected zero RemindersModified, got %v", got.RemindersModified)
	}
}

func TestDefaultDBPath(t *testing.T) {
	path, err := DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	if path == "" {
		t.Error("DefaultDBPath returned empty string")
	}
}
