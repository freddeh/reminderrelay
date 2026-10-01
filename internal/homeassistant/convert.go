package homeassistant

import (
	"time"

	"github.com/njoerd114/reminderrelay/internal/model"
)

// HA todo service constants.
const (
	domainTodo        = "todo"
	serviceGetItems   = "get_items"
	serviceAddItem    = "add_item"
	serviceUpdateItem = "update_item"
	serviceRemoveItem = "remove_item"

	statusNeedsAction = "needs_action"
	statusCompleted   = "completed"

	dateLayout     = "2006-01-02"
	dateTimeLayout = "2006-01-02 15:04:05"
)

// featureSetDueDateOnItem and featureSetDueDatetimeOnItem are bits from HA's
// todo.TodoListEntityFeature enum (homeassistant/components/todo/const.py).
// Sending due_datetime to an entity that doesn't declare
// SET_DUE_DATETIME_ON_ITEM is rejected, so callers must check supported
// features before choosing which field to send.
const (
	featureSetDueDateOnItem     = 16
	featureSetDueDatetimeOnItem = 32
)

// haTodoItem is the JSON structure for a single item returned by the HA
// todo.get_items service.
type haTodoItem struct {
	UID         string `json:"uid"`
	Summary     string `json:"summary"`
	Status      string `json:"status"` // "needs_action" or "completed"
	Description string `json:"description,omitempty"`
	Due         string `json:"due,omitempty"` // "YYYY-MM-DD" or RFC 3339
}

// haItemsResponse wraps the items array inside the service response for a
// single entity.
type haItemsResponse struct {
	Items []haTodoItem `json:"items"`
}

// haItemToModelItem converts an HA todo item to a [model.Item]. The priority
// prefix (e.g. "[High] ") is stripped from the description and decoded into
// the Priority field.
func haItemToModelItem(h haTodoItem) model.Item {
	priority, description := model.DecodePriorityPrefix(h.Description)

	item := model.Item{
		UID:         h.UID,
		Title:       h.Summary,
		Description: description,
		Priority:    priority,
		Completed:   h.Status == statusCompleted,
	}

	if h.Due != "" {
		if t, err := parseDue(h.Due); err == nil {
			item.DueDate = &t
		}
	}

	return item
}

// buildAddItemData returns the service-call payload for todo.add_item.
// features is the target entity's supported_features bitmask (see
// [featureSetDueDatetimeOnItem]); pass 0 if unknown, which falls back to the
// date-only due_date field.
func buildAddItemData(entityID string, item *model.Item, features int) map[string]interface{} {
	data := map[string]interface{}{
		"entity_id": entityID,
		"item":      item.Title,
	}

	desc := model.EncodePriorityPrefix(item.Priority, item.Description)
	if desc != "" {
		data["description"] = desc
	}

	setDueFields(data, item.DueDate, features)

	return data
}

// buildUpdateItemData returns the service-call payload for todo.update_item.
// identifier is the value sent in the "item" field to select the target
// item — either its HA UID or its current title (see [Adapter.UpdateItem]).
// currentTitle is always the item's actual current title, used only to
// decide whether a "rename" is needed, independent of which identifier was
// used to target it.
func buildUpdateItemData(entityID, identifier, currentTitle string, item *model.Item, features int) map[string]interface{} {
	data := map[string]interface{}{
		"entity_id": entityID,
		"item":      identifier,
	}

	if item.Title != currentTitle {
		data["rename"] = item.Title
	}

	data["description"] = model.EncodePriorityPrefix(item.Priority, item.Description)

	setDueFields(data, item.DueDate, features)

	if item.Completed {
		data["status"] = statusCompleted
	} else {
		data["status"] = statusNeedsAction
	}

	return data
}

// setDueFields sets either due_datetime or due_date on data, based on
// whether dueDate carries a time-of-day and whether the target entity
// declares SET_DUE_DATETIME_ON_ITEM support. A due date at exactly midnight
// is treated as date-only — EventKit and HA both represent "no specific
// time" as midnight, so an exact-midnight due time can't be distinguished
// from "no time set" and is handled the same way as a date-only due date.
func setDueFields(data map[string]interface{}, dueDate *time.Time, features int) {
	if dueDate == nil {
		return
	}
	if hasTimeComponent(*dueDate) && features&featureSetDueDatetimeOnItem != 0 {
		// due_datetime is a naive "YYYY-MM-DD HH:MM:SS" with no timezone,
		// interpreted by HA in its own configured local timezone — so this
		// must be the wall-clock time in *this machine's* local zone, not
		// whatever zone dueDate happens to carry (see hasTimeComponent).
		data["due_datetime"] = dueDate.Local().Format(dateTimeLayout)
		return
	}
	data["due_date"] = formatDue(dueDate)
}

// hasTimeComponent reports whether t carries a non-midnight time-of-day.
//
// t must be evaluated in local time here, not whatever Location it happens
// to carry: EventKit's bridge round-trips a date-only due date as the true
// UTC instant of *local* midnight (via NSCalendar's dateFromComponents,
// which uses the current/local calendar — see bridge_darwin.m), so a
// genuinely date-only reminder often arrives with Location == UTC and a
// non-zero UTC hour (e.g. 22:00 UTC for local midnight in UTC+2). Checking
// t.Hour() directly against that UTC-located value would misidentify it as
// carrying a time-of-day, and — via setDueFields above and
// calendarEventPayload — send the wrong (or wrongly-shifted) due date/day
// to HA.
func hasTimeComponent(t time.Time) bool {
	local := t.Local()
	return local.Hour() != 0 || local.Minute() != 0 || local.Second() != 0
}

// buildRemoveItemData returns the service-call payload for todo.remove_item.
func buildRemoveItemData(entityID, title string) map[string]interface{} {
	return map[string]interface{}{
		"entity_id": entityID,
		"item":      title,
	}
}

// buildGetItemsData returns the service-call payload for todo.get_items.
func buildGetItemsData(entityID string) map[string]interface{} {
	return map[string]interface{}{
		"entity_id": entityID,
	}
}

// parseDue parses an HA due-date string. It tries date-only format first
// ("2006-01-02"), then falls back to RFC 3339.
//
// A date-only string is parsed as local midnight, not UTC midnight: EventKit
// represents a date-only due date as the true UTC instant of local midnight
// (see [hasTimeComponent]), so constructing the equivalent local-midnight
// instant here is what makes a round trip through Reminders land back on the
// same calendar day instead of the previous or next one depending on the
// machine's UTC offset sign.
func parseDue(s string) (time.Time, error) {
	if t, err := time.ParseInLocation(dateLayout, s, time.Local); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// formatDue formats a time value as a date-only string for HA, using its
// local calendar date — see [hasTimeComponent] for why the value's own
// Location can't be trusted to already reflect that.
func formatDue(t *time.Time) string {
	return t.Local().Format(dateLayout)
}
