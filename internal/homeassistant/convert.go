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
		data["due_datetime"] = dueDate.Format(dateTimeLayout)
		return
	}
	data["due_date"] = formatDue(dueDate)
}

// hasTimeComponent reports whether t carries a non-midnight time-of-day.
func hasTimeComponent(t time.Time) bool {
	return t.Hour() != 0 || t.Minute() != 0 || t.Second() != 0
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
func parseDue(s string) (time.Time, error) {
	if t, err := time.Parse(dateLayout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// formatDue formats a time value as a date-only string for HA.
func formatDue(t *time.Time) string {
	return t.Format(dateLayout)
}
