// Package sync implements the bidirectional reconciliation engine for
// ReminderRelay. It compares Apple Reminders items and Home Assistant todo
// items against the state database, detects creates, updates, deletes,
// and conflicts, and dispatches mutations to the appropriate adapter.
//
// The package contains two main components:
//
//   - [Engine] runs the polling loop and optional WebSocket listener.
//   - [Bootstrap] handles first-run title-matching to link existing
//     items on both sides.
package sync

import (
	"context"

	"github.com/njoerd114/reminderrelay/internal/model"
	"github.com/njoerd114/reminderrelay/internal/state"
)

// RemindersSource provides read/write access to Apple Reminders items.
// Implemented by [reminders.Adapter].
type RemindersSource interface {
	FetchAll(ctx context.Context, listNames []string) ([]*model.Item, error)
	Create(ctx context.Context, item *model.Item) (uid string, err error)
	Update(ctx context.Context, uid string, item *model.Item) error
	Delete(ctx context.Context, uid string) error
}

// HASource provides read/write access to Home Assistant todo items.
// Implemented by [homeassistant.Adapter].
type HASource interface {
	GetItems(ctx context.Context, entityID string) ([]model.Item, error)
	// AddItem returns the newly created item's HA UID, diffed from
	// todo.get_items before/after the add rather than re-fetched and matched
	// by title afterwards — title-based matching could pick the wrong item
	// when titles collide, or silently link to an unrelated one.
	AddItem(ctx context.Context, entityID string, item *model.Item) (uid string, err error)
	// UpdateItem and RemoveItem target the item by haUID when known (falling
	// back to currentTitle/title internally if the HA version doesn't
	// resolve UIDs); haUID may be empty for items not yet tracked with one.
	UpdateItem(ctx context.Context, entityID, haUID, currentTitle string, item *model.Item) error
	RemoveItem(ctx context.Context, entityID, haUID, title string) error
}

// CalendarSource provides mirroring of due-dated items onto a Home Assistant
// calendar entity, giving them a real due-date UI and letting HA automations
// trigger off the calendar's event-start/end triggers — neither of which the
// todo domain supports. Only the due date is genuinely bidirectional: an
// edit made directly on the HA calendar (dragging an event, deleting it)
// flows back into Reminders and HA-todo via the 3-way merge in
// [Reconciler.mergeAndSync]. Title, notes, priority, and completed status
// still flow one-way into the mirrored event, never back — see README.md's
// "Calendar Mirroring" section for the reasoning. Implemented by
// [homeassistant.Adapter].
type CalendarSource interface {
	// CalendarSupportsMutation reports whether entityID can be mirrored to:
	// it must support creating, updating, and deleting events under program
	// control (HA core 2026.x's local_calendar does; other calendar
	// backends may not — see README.md).
	CalendarSupportsMutation(ctx context.Context, entityID string) (bool, error)
	// ListCalendarEvents returns every event currently on entityID (within
	// an adapter-defined lookback/lookahead window), used to detect
	// calendar-side due-date edits. An event with no corresponding state row
	// (one the user created directly on the calendar) is ignored — see
	// README.md for why creating new items this way isn't supported.
	ListCalendarEvents(ctx context.Context, entityID string) ([]model.CalendarEvent, error)
	CreateCalendarEvent(ctx context.Context, entityID string, item *model.Item) (uid string, err error)
	UpdateCalendarEvent(ctx context.Context, entityID, uid string, item *model.Item) error
	DeleteCalendarEvent(ctx context.Context, entityID, uid string) error
}

// StateStore provides access to the sync state database.
// Implemented by [state.Store].
type StateStore interface {
	GetItemByRemindersUID(ctx context.Context, uid string) (*state.Item, error)
	GetItemByHAUID(ctx context.Context, uid string) (*state.Item, error)
	GetAllItemsForList(ctx context.Context, listName string) ([]*state.Item, error)
	UpsertItem(ctx context.Context, item *state.Item) error
	DeleteItem(ctx context.Context, id int64) error
	IsEmpty(ctx context.Context) (bool, error)
}
