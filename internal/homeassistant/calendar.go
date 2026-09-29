package homeassistant

import (
	"context"
	"fmt"
	"time"

	haclient "github.com/mkelcik/go-ha-client/v2"

	"github.com/njoerd114/reminderrelay/internal/model"
)

// Calendar entity feature bits, from HA's calendar.CalendarEntityFeature enum
// (homeassistant/components/calendar/const.py). Mirroring due dates requires
// all three: the entity must be able to create, update, and delete events
// under program control.
const (
	calendarFeatureCreateEvent = 1
	calendarFeatureDeleteEvent = 2
	calendarFeatureUpdateEvent = 4

	calendarFeaturesRequired = calendarFeatureCreateEvent | calendarFeatureDeleteEvent | calendarFeatureUpdateEvent
)

// calendarEventDescription is the fixed, minimal description written on
// every mirrored calendar event — a marker, not a copy of the reminder's
// notes.
const calendarEventDescription = "Synced from Reminders by ReminderRelay."

// defaultCalendarEventDuration is used for timed due dates, which need an
// end time. HA's minimum event duration is effectively unconstrained
// (1 second), so this is purely a display choice.
const defaultCalendarEventDuration = 30 * time.Minute

// calendarLookupPastWindow and calendarLookupFutureWindow bound how far
// back/forward [Adapter.ListCalendarEvents] searches around "now". Wide
// enough for realistic personal due dates while keeping the underlying HA
// query bounded; a due date pushed out further than the future window won't
// be detected as having changed on the calendar side until it falls back
// inside it — an accepted limitation for a personal task list.
const (
	calendarLookupPastWindow   = 365 * 24 * time.Hour
	calendarLookupFutureWindow = 2 * 365 * 24 * time.Hour
)

// CalendarSupportsMutation reports whether entityID declares create, update,
// and delete support for events. Calendar mirroring for a list should be
// disabled if this returns false — see the "stick with local_calendar"
// design note in README.md.
func (a *Adapter) CalendarSupportsMutation(ctx context.Context, entityID string) (bool, error) {
	features, err := a.CalendarSupportedFeatures(ctx, entityID)
	if err != nil {
		return false, err
	}
	return features&calendarFeaturesRequired == calendarFeaturesRequired, nil
}

// ListCalendarEvents returns every event HA currently reports for entityID
// within a bounded window around now (see [calendarLookupPastWindow]).
// The reconciler uses this once per list per pass to detect edits made
// directly on the calendar — the read half of the 3-way due-date merge
// described in README.md's "Calendar Mirroring" section. Events HA reports
// with neither a date nor a dateTime start (which shouldn't normally
// happen) are skipped rather than failing the whole listing.
func (a *Adapter) ListCalendarEvents(ctx context.Context, entityID string) ([]model.CalendarEvent, error) {
	now := time.Now()
	from := now.Add(-calendarLookupPastWindow)
	to := now.Add(calendarLookupFutureWindow)

	var raw haclient.CalendarEvents
	err := Retry(ctx, defaultMaxAttempts, func() error {
		var callErr error
		raw, callErr = a.rest.GetCalendarEvents(ctx, entityID, from, to)
		return callErr
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("listing calendar events for %s: %w", entityID, err)
	}

	events := make([]model.CalendarEvent, 0, len(raw))
	for _, e := range raw {
		if e.UID == "" {
			continue
		}
		due, err := parseCalendarEventTime(e.Start)
		if err != nil {
			a.logger.Debug("skipping calendar event with unparsable start time", "uid", e.UID, "error", err)
			continue
		}
		events = append(events, model.CalendarEvent{UID: e.UID, DueDate: due})
	}
	return events, nil
}

// parseCalendarEventTime converts HA's CalendarEventTime (exactly one of
// Date or DateTime is set) into a *time.Time.
func parseCalendarEventTime(t haclient.CalendarEventTime) (*time.Time, error) {
	if t.DateTime != "" {
		parsed, err := time.Parse(time.RFC3339, t.DateTime)
		if err != nil {
			return nil, err
		}
		return &parsed, nil
	}
	if t.Date != "" {
		parsed, err := time.Parse(dateLayout, t.Date)
		if err != nil {
			return nil, err
		}
		return &parsed, nil
	}
	return nil, fmt.Errorf("event start has neither date nor dateTime")
}

// CreateCalendarEvent creates a mirrored due-date event on entityID and
// returns its assigned UID.
//
// Home Assistant's calendar/event/create WebSocket command (the only way to
// create an event — see [Adapter.UpdateCalendarEvent]) does not return the
// newly created event's UID, and callers cannot supply one of their own, so
// this looks the event back up by summary within a narrow time window
// immediately after creating it.
//
// Requires [Adapter.Connect] to have been called.
func (a *Adapter) CreateCalendarEvent(ctx context.Context, entityID string, item *model.Item) (string, error) {
	if a.ws == nil {
		return "", fmt.Errorf("create calendar event: WebSocket client not configured")
	}
	if item.DueDate == nil {
		return "", fmt.Errorf("create calendar event %q: item has no due date", item.Title)
	}

	req := map[string]interface{}{
		"type":      "calendar/event/create",
		"entity_id": entityID,
		"event":     calendarEventPayload(item),
	}
	err := Retry(ctx, defaultMaxAttempts, func() error {
		return a.ws.Do(ctx, req, nil)
	}, nil)
	if err != nil {
		return "", fmt.Errorf("creating calendar event %q on %s: %w", item.Title, entityID, err)
	}

	uid, err := a.findCalendarEventUID(ctx, entityID, item)
	if err != nil {
		return "", fmt.Errorf("created calendar event %q on %s but could not locate its UID afterward: %w", item.Title, entityID, err)
	}
	return uid, nil
}

// findCalendarEventUID looks up a just-created event by summary within a
// window bracketing its due date, and returns its UID.
func (a *Adapter) findCalendarEventUID(ctx context.Context, entityID string, item *model.Item) (string, error) {
	from := item.DueDate.Add(-time.Hour)
	to := item.DueDate.Add(48 * time.Hour) // generous enough to cover all-day events regardless of timezone
	events, err := a.rest.GetCalendarEvents(ctx, entityID, from, to)
	if err != nil {
		return "", err
	}

	want := eventSummary(item)
	for _, e := range events {
		if e.Summary == want && e.UID != "" {
			return e.UID, nil
		}
	}
	return "", fmt.Errorf("no matching event found")
}

// UpdateCalendarEvent updates an existing mirrored event in place.
//
// Unlike todo.add_item/update_item/remove_item, calendar event mutation is
// not exposed as a REST service in Home Assistant as of core 2026.x — only
// calendar.create_event and calendar.get_events are. update and delete are
// WebSocket-only commands (calendar/event/update, calendar/event/delete),
// so this rides the same authenticated WebSocket connection SubscribeChanges
// uses, via the vendored client's generic WSClient.Do.
//
// Requires [Adapter.Connect] to have been called.
func (a *Adapter) UpdateCalendarEvent(ctx context.Context, entityID, uid string, item *model.Item) error {
	if a.ws == nil {
		return fmt.Errorf("update calendar event: WebSocket client not configured")
	}
	if item.DueDate == nil {
		return fmt.Errorf("update calendar event %q: item has no due date", item.Title)
	}

	req := map[string]interface{}{
		"type":      "calendar/event/update",
		"entity_id": entityID,
		"uid":       uid,
		"event":     calendarEventPayload(item),
	}
	err := Retry(ctx, defaultMaxAttempts, func() error {
		return a.ws.Do(ctx, req, nil)
	}, nil)
	if err != nil {
		return fmt.Errorf("updating calendar event %s on %s: %w", uid, entityID, err)
	}
	return nil
}

// DeleteCalendarEvent removes a mirrored event. See [Adapter.UpdateCalendarEvent]
// for why this is a WebSocket command rather than a REST service call.
//
// Requires [Adapter.Connect] to have been called.
func (a *Adapter) DeleteCalendarEvent(ctx context.Context, entityID, uid string) error {
	if a.ws == nil {
		return fmt.Errorf("delete calendar event: WebSocket client not configured")
	}

	req := map[string]interface{}{
		"type":      "calendar/event/delete",
		"entity_id": entityID,
		"uid":       uid,
	}
	err := Retry(ctx, defaultMaxAttempts, func() error {
		return a.ws.Do(ctx, req, nil)
	}, nil)
	if err != nil {
		return fmt.Errorf("deleting calendar event %s on %s: %w", uid, entityID, err)
	}
	return nil
}

// calendarEventPayload builds the WebSocket "event" object for create/update
// commands: dtstart/dtend as an all-day date pair when the due date has no
// time-of-day (see [hasTimeComponent]), or as timed RFC3339 datetimes with
// [defaultCalendarEventDuration] otherwise.
func calendarEventPayload(item *model.Item) map[string]interface{} {
	due := *item.DueDate
	event := map[string]interface{}{
		"summary":     eventSummary(item),
		"description": calendarEventDescription,
	}

	if hasTimeComponent(due) {
		event["dtstart"] = due.Format(time.RFC3339)
		event["dtend"] = due.Add(defaultCalendarEventDuration).Format(time.RFC3339)
	} else {
		event["dtstart"] = due.Format(dateLayout)
		// RFC5545 all-day events have an exclusive end date.
		event["dtend"] = due.AddDate(0, 0, 1).Format(dateLayout)
	}
	return event
}

// eventSummary is the mirrored event's title: the reminder's title, tagged
// with a "[Done] " prefix once completed (the event is kept, not deleted,
// on completion — see README.md).
func eventSummary(item *model.Item) string {
	if item.Completed {
		return "[Done] " + item.Title
	}
	return item.Title
}
